package main

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// The checker's "what should exist" and the planted ground truth are derived independently. They must agree.
func TestExpectedMatchesGroundTruth(t *testing.T) {
	exp, err := expectedFromFiles("data")
	if err != nil {
		t.Fatal(err)
	}
	var truth struct{ Discrepancies []Row }
	b, _ := os.ReadFile("data/ground_truth.json")
	json.Unmarshal(b, &truth)
	var want, got []string
	for _, d := range truth.Discrepancies {
		want = append(want, "discrepancy:"+d.TxnID+":"+d.Kind+" "+show(d.LedgerAmount)+"/"+show(d.BankAmount))
	}
	pays := 0
	for _, e := range exp {
		if e.Kind == "payment" {
			pays++
			continue
		}
		got = append(got, e.ID+" "+e.Fields["ledger_amount"].(string)+"/"+e.Fields["bank_amount"].(string))
	}
	sort.Strings(want)
	sort.Strings(got)
	if len(want) != len(got) || pays != 1 {
		t.Fatalf("want %v + 1 payment, got %v + %d payments", want, got, pays)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("mismatch: %s vs %s", want[i], got[i])
		}
	}
}
