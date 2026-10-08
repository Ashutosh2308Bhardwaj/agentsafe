package reconcile_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/reconcile"
)

// A proxy run's log reconciles like an agent run's: a gated payment needs an approval for its key in the log.
func TestReconcilingAProxyRun(t *testing.T) {
	ctx := context.Background()
	type payout struct {
		Ref string `json:"ref"`
	}
	pay := agentsafe.Func("pay", "pay", func(context.Context, payout) (string, error) { return "paid", nil },
		agentsafe.Idempotent("ref"), agentsafe.NeedsApproval(func(in payout) any { return in }))
	free := agentsafe.Func("refund_fee", "a write with no approval", func(context.Context, payout) (string, error) { return "ok", nil },
		agentsafe.Idempotent("ref"))
	log := &agentsafe.FileLog{Path: filepath.Join(t.TempDir(), "proxy.jsonl")}
	g, err := agentsafe.OpenGateway(ctx, log, agentsafe.WithTools(pay, free), agentsafe.WithStartedBy("agent"),
		agentsafe.WithAuthorizer(agentsafe.AllowList("finance")))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := g.Call(ctx, "c", "pay", json.RawMessage(`{"ref":"T-77"}`))
	var p struct{ Key string }
	_ = json.Unmarshal([]byte(r.Result), &p)
	if err := g.Approve(ctx, p.Key, "finance"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Call(ctx, "c", "pay", json.RawMessage(`{"ref":"T-77"}`)); err != nil {
		t.Fatal(err)
	}
	fee, _ := g.Call(ctx, "c", "refund_fee", json.RawMessage(`{"ref":"T-78"}`))
	_ = fee
	events, err := log.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	keyOf := func(tool string) string {
		for _, e := range events {
			if e.Type == agentsafe.EvToolResult && e.Tool == tool && !e.Replayed {
				return e.Key
			}
		}
		return ""
	}
	payment := reconcile.Effect{ID: "payment:T-77", Kind: "payment", Gated: true, Key: keyOf("pay"), Fields: map[string]any{"amount": "1"}}
	if rep := reconcile.Audit([]reconcile.Effect{payment}, []reconcile.Effect{payment}, events, nil); !rep.Pass() {
		t.Fatalf("an approved, logged payment through a proxy passes:\n%s", rep.String())
	}
	// A payment the gateway never approved (here: the ungated tool's effect, presented as a gated payment).
	unapproved := reconcile.Effect{ID: "payment:T-78", Kind: "payment", Gated: true, Key: keyOf("refund_fee"), Fields: map[string]any{"amount": "1"}}
	rep := reconcile.Audit([]reconcile.Effect{unapproved}, []reconcile.Effect{unapproved}, events, nil)
	if rep.Pass() || rep.Findings[0].Severity != reconcile.Critical {
		t.Fatalf("money with no approval in the proxy's log is CRITICAL:\n%s", rep.String())
	}
}
