package reconcile_test

import (
	"fmt"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/reconcile"
)

// Audit checks what the agent did against the systems of record, independently of what it claimed.
func ExampleAudit() {
	expected := []reconcile.Effect{{ID: "payment:INV-1", Kind: "payment", Gated: true,
		Fields: map[string]any{"amount": agentsafe.Decimal("4200.50")}}}
	actual := []reconcile.Effect{ // what the gateway holds: the same invoice paid twice
		{ID: "payment:INV-1", Kind: "payment", Gated: true, Key: "k1", Fields: map[string]any{"amount": 4200.5}},
		{ID: "payment:INV-1", Kind: "payment", Gated: true, Key: "k1", Fields: map[string]any{"amount": 4200.5}},
	}
	events := []agentsafe.Event{
		{Type: agentsafe.EvApprovalDecided, Key: "k1", Decision: "approved", By: "ops"},
		{Type: agentsafe.EvToolResult, Key: "k1", Result: `"paid"`},
	}
	rep := reconcile.Audit(expected, actual, events, nil)
	for _, f := range rep.Findings {
		fmt.Println(f.Severity, f.Check, "-", f.Detail)
	}
	// Output:
	// CRITICAL exactly-once - payment:INV-1 exists 2 times
	// CRITICAL once-per-approval - approved operation k1 took effect 2 times
}

// Same compares field values by type and exact value, as Reconcile does.
func ExampleSame() {
	for _, pair := range [][2]any{
		{agentsafe.Decimal("4200.50"), 4200.5},
		{4200, "4200"},
		{0.3, 0.1 + float64(len("xx"))/10},
	} {
		if eq, why := reconcile.Same(pair[0], pair[1]); eq {
			fmt.Println("equal")
		} else {
			fmt.Println("different:", why)
		}
	}
	// Output:
	// equal
	// different: "4200" (text) is text, should be number 4200
	// different: 0.30000000000000004, should be 0.3
}
