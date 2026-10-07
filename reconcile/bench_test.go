package reconcile_test

import (
	"fmt"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/reconcile"
)

// BenchmarkAudit reconciles a run whose effects all match and all trace to a logged result: every check runs
// in full, field by field.
func BenchmarkAudit(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		expected := make([]reconcile.Effect, n)
		actual := make([]reconcile.Effect, n)
		events := make([]agentsafe.Event, n) // each effect's logged result: provenance traces effects to them
		for i := range expected {
			key := fmt.Sprintf("%032x", i)
			events[i] = agentsafe.Event{Seq: i + 1, Type: agentsafe.EvToolResult, CallID: fmt.Sprintf("call_%d", i), Key: key}
			f := map[string]any{"payee": "Imran Khan", "amount": agentsafe.Decimal("4200.50"), "ref": fmt.Sprintf("T%05d", i)}
			expected[i] = reconcile.Effect{ID: fmt.Sprintf("payment:%d", i), Kind: "payment", Fields: f}
			actual[i] = reconcile.Effect{ID: fmt.Sprintf("payment:%d", i), Kind: "payment", Key: key,
				Fields: map[string]any{"payee": "Imran Khan", "amount": 4200.50, "ref": fmt.Sprintf("T%05d", i)}}
		}
		b.Run(fmt.Sprintf("effects=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if r := reconcile.Audit(expected, actual, events, nil); !r.Pass() {
					b.Fatalf("a matching run must pass: %+v", r.Findings[:1])
				}
			}
		})
	}
}
