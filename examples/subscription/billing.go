package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// billing is a fake subscription API, served over real HTTP on localhost, shaped like Stripe's: a request may
// carry an Idempotency-Key, and a repeated key gets the first response back instead of a second change. Its
// records live in a file, so they survive this process being killed: it's "someone else's system".
type billing struct {
	mu   sync.Mutex
	path string
	slow bool // answer the next change only after the client has given up (once)
	srv  *http.Server
	url  string
}

// records are the API's books: every customer's seats, every invoice it issued, and the response it sent
// for each idempotency key.
type records struct {
	Seats     map[string]int             `json:"seats"`
	Invoices  []Invoice                  `json:"invoices"`
	Responses map[string]json.RawMessage `json:"responses"`
}

// Invoice is the charge for added seats. Adding seats is relative: applied twice, it's twice the seats and
// twice the charge.
type Invoice struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	Seats    int    `json:"seats"`
	Amount   string `json:"amount"`
}

const seatPrice = 12 // per seat, per month

func startBilling(path string, slow bool) (*billing, error) {
	b := &billing{path: path, slow: slow}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := b.save(records{Seats: map[string]int{"cus_42": 10}, Responses: map[string]json.RawMessage{}}); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/customers/{id}/seats", b.addSeats)
	b.srv, b.url = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}, "http://"+ln.Addr().String()
	go func() { _ = b.srv.Serve(ln) }()
	return b, nil
}

func (b *billing) Close() { _ = b.srv.Close() }

// addSeats adds seats and issues their invoice, unless the idempotency key was seen before, in which case
// it returns the first response and changes nothing.
func (b *billing) addSeats(w http.ResponseWriter, r *http.Request) {
	var req struct{ Add int }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Add <= 0 {
		http.Error(w, `{"error":"add must be a positive number of seats"}`, http.StatusBadRequest)
		return
	}
	resp, replayed, err := b.apply(r.PathValue("id"), req.Add, r.Header.Get("Idempotency-Key"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	} else if b.takeSlow() {
		time.Sleep(time.Second) // the change is made; the response arrives after the client gave up
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// apply is the check and the change under one lock: two requests with one key can't both add seats.
func (b *billing) apply(customer string, add int, key string) (json.RawMessage, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.load()
	if err != nil {
		return nil, false, err
	}
	if prev, ok := rec.Responses[key]; ok && key != "" {
		return prev, true, nil
	}
	rec.Seats[customer] += add
	inv := Invoice{ID: fmt.Sprintf("in_%03d", len(rec.Invoices)+1), Customer: customer, Seats: add,
		Amount: fmt.Sprintf("%d.00", add*seatPrice)}
	rec.Invoices = append(rec.Invoices, inv)
	resp, err := json.Marshal(map[string]any{"customer": customer, "seats": rec.Seats[customer], "invoice": inv})
	if err != nil {
		return nil, false, err
	}
	if key != "" {
		rec.Responses[key] = resp
	}
	return resp, false, b.save(rec)
}

func (b *billing) takeSlow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	slow := b.slow
	b.slow = false
	return slow
}

func (b *billing) load() (records, error) {
	var rec records
	raw, err := os.ReadFile(b.path)
	if err != nil {
		return rec, err
	}
	return rec, json.Unmarshal(raw, &rec)
}

func (b *billing) save(rec records) error {
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}

// report prints what the API's books say: the systems of record, not the agent's account.
func (b *billing) report() error {
	rec, err := b.load()
	if err != nil {
		return err
	}
	fmt.Printf("billing API: cus_42 has %d seats; %d invoice(s)\n", rec.Seats["cus_42"], len(rec.Invoices))
	for _, inv := range rec.Invoices {
		fmt.Printf("  %s  %s +%d seats  %s\n", inv.ID, inv.Customer, inv.Seats, inv.Amount)
	}
	return nil
}

// addSeatsRequest is the client side: what your tool does. key goes in the Idempotency-Key header.
func addSeatsRequest(ctx context.Context, baseURL, customer string, add int, key string) (map[string]any, error) {
	body, err := json.Marshal(map[string]int{"add": add})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/customers/"+customer+"/seats", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err // a timeout: the change may or may not have been made
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("billing API: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	out["replayed"] = resp.Header.Get("Idempotent-Replayed") == "true"
	return out, nil
}
