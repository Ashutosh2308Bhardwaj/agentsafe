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

// notifier is a fake email API on localhost: it accepts a message with an optional Idempotency-Key, and a
// repeated key gets the first response back instead of a second email. Its records live in a file, so they
// survive the worker being killed.
type notifier struct {
	mu   sync.Mutex
	path string
	srv  *http.Server
	url  string
}

type sentMail struct {
	Messages  []json.RawMessage          `json:"messages"`
	Responses map[string]json.RawMessage `json:"responses"`
}

func startNotifier(path string) (*notifier, error) {
	n := &notifier{path: path}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := n.save(sentMail{Responses: map[string]json.RawMessage{}}); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", n.send)
	n.srv, n.url = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}, "http://"+ln.Addr().String()
	go func() { _ = n.srv.Serve(ln) }()
	return n, nil
}

func (n *notifier) Close() { _ = n.srv.Close() }

func (n *notifier) send(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil || !json.Valid(body) {
		http.Error(w, `{"error":"bad message"}`, http.StatusBadRequest)
		return
	}
	resp, err := n.record(body, r.Header.Get("Idempotency-Key"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

func (n *notifier) record(body []byte, key string) (json.RawMessage, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s, err := n.load()
	if err != nil {
		return nil, err
	}
	if prev, ok := s.Responses[key]; ok && key != "" {
		return prev, nil
	}
	s.Messages = append(s.Messages, body)
	resp, err := json.Marshal(map[string]string{"id": fmt.Sprintf("msg_%03d", len(s.Messages)), "status": "queued"})
	if err != nil {
		return nil, err
	}
	if key != "" {
		s.Responses[key] = resp
	}
	return resp, n.save(s)
}

func (n *notifier) load() (sentMail, error) {
	var s sentMail
	raw, err := os.ReadFile(n.path)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(raw, &s)
}

func (n *notifier) save(s sentMail) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := n.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, n.path)
}

func (n *notifier) count() (int, error) {
	s, err := n.load()
	return len(s.Messages), err
}

// sendMessage is the worker's client: key goes in the Idempotency-Key header.
func sendMessage(ctx context.Context, baseURL, payload, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", bytes.NewReader([]byte(payload)))
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
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("notifications API: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
