package main

import (
	"path/filepath"
	"sync"
	"testing"
)

// The same key must charge at most once however the calls arrive, including at the same moment: after a
// timeout the runner retries while the first call may still be in flight.
func TestSimultaneousPaysWithOneKeyChargeOnce(t *testing.T) {
	g := &Gateway{Path: filepath.Join(t.TempDir(), "gw.json")}
	var wg sync.WaitGroup
	ids := make([]string, 20)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := g.Pay("key-1", "Ramesh", 11000, "T1007")
			if err != nil {
				t.Error(err)
			}
			ids[i] = p.PaymentID
		}(i)
	}
	wg.Wait()
	payments, err := g.Payments()
	if err != nil {
		t.Fatal(err)
	}
	if len(payments) != 1 {
		t.Fatalf("20 simultaneous calls with one key made %d payments", len(payments))
	}
	for _, id := range ids {
		if id != payments[0].PaymentID {
			t.Fatalf("every caller must get the ORIGINAL payment, got %s and %s", id, payments[0].PaymentID)
		}
	}
}
