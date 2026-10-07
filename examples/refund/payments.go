package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// payments is a fake payments API, served over real HTTP on localhost, shaped like Stripe's: charges can be
// looked up, refunds are created with an optional Idempotency-Key, and a repeated key gets the first
// response back. It refuses a refund larger than what's left of the charge. Its books live in a file, so
// they survive this process being killed: it's "someone else's system".
type payments struct {
	mu   sync.Mutex
	path string
	srv  *http.Server
	url  string
}

type ledger struct {
	Charges   map[string]*Charge         `json:"charges"`
	Refunds   []Refund                   `json:"refunds"`
	Responses map[string]json.RawMessage `json:"responses"` // idempotency key -> the response it got
}

// Charge is a payment the customer made.
type Charge struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	Amount   string `json:"amount"`
	Refunded string `json:"refunded"`
}

// Refund is money sent back. A duplicate refund is money lost.
type Refund struct {
	ID     string `json:"id"`
	Charge string `json:"charge"`
	Amount string `json:"amount"`
}

func startPayments(path string) (*payments, error) {
	p := &payments{path: path}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		err := p.save(ledger{Responses: map[string]json.RawMessage{}, Charges: map[string]*Charge{
			"ch_3QA": {ID: "ch_3QA", Customer: "cus_42", Amount: "4200.50", Refunded: "0.00"}}})
		if err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/charges/{id}", p.getCharge)
	mux.HandleFunc("POST /v1/refunds", p.createRefund)
	p.srv, p.url = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}, "http://"+ln.Addr().String()
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

func (p *payments) Close() { _ = p.srv.Close() }

func (p *payments) getCharge(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	l, err := p.load()
	p.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	c, ok := l.Charges[r.PathValue("id")]
	if !ok {
		http.Error(w, `{"error":"no such charge"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, c)
}

func (p *payments) createRefund(w http.ResponseWriter, r *http.Request) {
	var req struct{ Charge, Amount string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	resp, status, replayed := p.refund(req.Charge, req.Amount, r.Header.Get("Idempotency-Key"))
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(resp)
}

// refund is the check and the effect under one lock: a repeated key returns the first response, and a
// refund can never exceed what's left of the charge.
func (p *payments) refund(charge, amount, key string) (json.RawMessage, int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fail := func(status int, msg string) (json.RawMessage, int, bool) {
		b, _ := json.Marshal(map[string]string{"error": msg})
		return b, status, false
	}
	l, err := p.load()
	if err != nil {
		return fail(http.StatusInternalServerError, err.Error())
	}
	if prev, ok := l.Responses[key]; ok && key != "" {
		return prev, http.StatusOK, true
	}
	c, ok := l.Charges[charge]
	if !ok {
		return fail(http.StatusNotFound, "no such charge")
	}
	amt, ok1 := cents(amount)
	paid, ok2 := cents(c.Amount)
	done, ok3 := cents(c.Refunded)
	if !ok1 || !ok2 || !ok3 || amt <= 0 {
		return fail(http.StatusBadRequest, "amount must be a positive decimal")
	}
	if done+amt > paid {
		return fail(http.StatusBadRequest, fmt.Sprintf("refund of %s exceeds the %s left on %s", amount, money(paid-done), charge))
	}
	c.Refunded = money(done + amt)
	ref := Refund{ID: fmt.Sprintf("re_%03d", len(l.Refunds)+1), Charge: charge, Amount: money(amt)}
	l.Refunds = append(l.Refunds, ref)
	resp, err := json.Marshal(ref)
	if err != nil {
		return fail(http.StatusInternalServerError, err.Error())
	}
	if key != "" {
		l.Responses[key] = resp
	}
	if err := p.save(l); err != nil {
		return fail(http.StatusInternalServerError, err.Error())
	}
	return resp, http.StatusOK, false
}

func (p *payments) load() (ledger, error) {
	var l ledger
	raw, err := os.ReadFile(p.path)
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(raw, &l)
}

func (p *payments) save(l ledger) error {
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}

// report prints what the payments API's books say: the system of record, not the agent's account.
func (p *payments) report() error {
	l, err := p.load()
	if err != nil {
		return err
	}
	c := l.Charges["ch_3QA"]
	fmt.Printf("payments API: %s refunded %s of %s; %d refund(s)\n", c.ID, c.Refunded, c.Amount, len(l.Refunds))
	for _, r := range l.Refunds {
		fmt.Printf("  %s  %s  %s\n", r.ID, r.Charge, r.Amount)
	}
	return nil
}

// cents parses a decimal amount ("1200.5", "1200.50") exactly, as an integer number of cents.
func cents(s string) (int64, bool) {
	r, ok := new(big.Rat).SetString(s)
	if !ok || strings.Contains(s, "/") {
		return 0, false
	}
	r.Mul(r, big.NewRat(100, 1))
	if !r.IsInt() || !r.Num().IsInt64() {
		return 0, false
	}
	return r.Num().Int64(), true
}

func money(c int64) string { return fmt.Sprintf("%d.%02d", c/100, c%100) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// The client side: what your tools do.

func fetchCharge(ctx context.Context, baseURL, id string) (*Charge, error) {
	var c Charge
	return &c, call(ctx, http.MethodGet, baseURL+"/v1/charges/"+id, nil, "", &c)
}

// createRefund sends key as the Idempotency-Key header.
func createRefund(ctx context.Context, baseURL, charge, amount, key string) (*Refund, error) {
	var ref Refund
	return &ref, call(ctx, http.MethodPost, baseURL+"/v1/refunds", map[string]string{"charge": charge, "amount": amount}, key, &ref)
}

func call(ctx context.Context, method, url string, body any, key string, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("payments API: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, out)
}
