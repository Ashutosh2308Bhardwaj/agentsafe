package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe/reconcile"
)

// The checker's "what should exist" and the planted ground truth are derived independently. They must agree,
// compared the way the checker compares (typed: a Decimal from the CSV against the truth's numbers).
func TestExpectedMatchesGroundTruth(t *testing.T) {
	exp, err := expectedFromFiles("data")
	if err != nil {
		t.Fatal(err)
	}
	var truth struct{ Discrepancies []Row }
	b, err := os.ReadFile("data/ground_truth.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &truth); err != nil {
		t.Fatal(err)
	}
	got := map[string]reconcile.Effect{}
	pays := 0
	for _, e := range exp {
		if e.Kind == "payment" {
			pays++
			continue
		}
		got[e.ID] = e
	}
	if len(got) != len(truth.Discrepancies) || pays != 1 {
		t.Fatalf("want %d discrepancies + 1 payment, got %d + %d", len(truth.Discrepancies), len(got), pays)
	}
	for _, d := range truth.Discrepancies {
		id := "discrepancy:" + d.TxnID + ":" + d.Kind
		e, ok := got[id]
		if !ok {
			t.Fatalf("%s is in the ground truth but not expected", id)
		}
		for field, want := range map[string]*float64{"ledger_amount": d.LedgerAmount, "bank_amount": d.BankAmount} {
			if eq, why := reconcile.Same(want, e.Fields[field]); !eq {
				t.Fatalf("%s %s: %s", id, field, why)
			}
		}
	}
}
