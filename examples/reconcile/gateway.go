package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Gateway is a fake payout gateway, file-backed so it survives our process being killed: it is "someone
// else's system". It supports the two things the outbox pattern needs from a real one:
//   - Pay is idempotent on an Idempotency-Key: the same key returns the original payment, never a second one,
//     however the calls arrive (one after another, after a crash, or at the same moment); the same key with a
//     different amount is a 409 conflict;
//   - Status(key) answers "did this payment happen?" when a response was lost.
//
// LoseResponses makes the next n Pay calls succeed but return ErrTimeout: the gateway charged, the caller
// never heard. That is week 1 F13, at the money layer.
type Gateway struct {
	// mu makes "seen this key? / record the payment" one step. Without it, simultaneous calls with one key all
	// pass the check before any of them records (check-then-act). A real gateway gets the same guarantee from
	// a unique constraint on the key (INSERT ... ON CONFLICT); this one lives in-process, so a mutex does.
	mu sync.Mutex

	Path          string
	LoseResponses int
	OnCharged     func() // test/chaos hook: called after a payment is durably recorded, before returning

	// Silent faults (week 4 S3): the gateway misbehaves AND reports success.
	WrongAmount bool // records 90% of the amount, returns the requested amount
	IgnoreKey   bool // a buggy gateway: every Pay is a new charge, idempotency key or not
}

// Payment is the gateway's record.
type Payment struct {
	PaymentID string  `json:"payment_id"`
	Key       string  `json:"idempotency_key"`
	Payee     string  `json:"payee"`
	Amount    float64 `json:"amount_inr"`
	Ref       string  `json:"ref"`
}

var ErrTimeout = errors.New("gateway timeout: no response (the payment may or may not have happened)")

// load returns payments by payment id (not by key: a buggy gateway can hold two payments for one key).
func (g *Gateway) load() (map[string]Payment, error) {
	m := map[string]Payment{}
	b, err := os.ReadFile(g.Path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	return m, json.Unmarshal(b, &m)
}

func (g *Gateway) save(m map[string]Payment) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := g.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, g.Path) // atomic replace: the gateway's own durability is not our problem to fake badly
}

// Pay charges once per key.
func (g *Gateway) Pay(key, payee string, amount float64, ref string) (Payment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m, err := g.load()
	if err != nil {
		return Payment{}, err
	}
	if prev := byKey(m, key); prev != nil && !g.IgnoreKey {
		if prev.Amount != amount && !g.WrongAmount || prev.Payee != payee {
			return Payment{}, fmt.Errorf("409 conflict: idempotency key %s was used for %s ₹%v", key, prev.Payee, prev.Amount)
		}
		return *prev, nil // the same request again: the ORIGINAL payment, no second charge
	}
	p := Payment{PaymentID: fmt.Sprintf("pay_%03d", len(m)+1), Key: key, Payee: payee, Amount: amount, Ref: ref}
	if g.WrongAmount {
		p.Amount = amount * 0.9 // the money that actually moves
	}
	m[p.PaymentID] = p
	if err := g.save(m); err != nil {
		return Payment{}, err
	}
	if g.OnCharged != nil {
		g.OnCharged()
	}
	if g.LoseResponses > 0 {
		g.LoseResponses--
		return Payment{}, ErrTimeout
	}
	if g.WrongAmount {
		p.Amount = amount // ...and the confirmation the caller receives says what it asked for
	}
	return p, nil
}

func byKey(m map[string]Payment, key string) *Payment {
	for _, p := range m {
		if key != "" && p.Key == key {
			return &p
		}
	}
	return nil
}

// Status looks a payment up by key.
func (g *Gateway) Status(key string) (*Payment, error) {
	m, err := g.load()
	if err != nil {
		return nil, err
	}
	return byKey(m, key), nil
}

// Payments returns every payment the gateway made: the system of record for money.
func (g *Gateway) Payments() ([]Payment, error) {
	m, err := g.load()
	var out []Payment
	for _, p := range m {
		out = append(out, p)
	}
	return out, err
}

func gatewayAt(out, run string) *Gateway {
	return &Gateway{Path: filepath.Join(out, run+"-gateway.json")}
}
