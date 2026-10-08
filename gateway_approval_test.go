package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// gatedGateway is a gateway whose payout needs a human: finance may decide, and nobody may decide their own run.
func gatedGateway(t *testing.T, path string, paid map[string]int) *Gateway {
	t.Helper()
	type payout struct {
		Ref    string `json:"ref"`
		Amount string `json:"amount"`
	}
	pay := Func("pay", "pay", func(_ context.Context, in payout) (string, error) {
		paid[in.Ref]++
		return "paid " + in.Ref, nil
	}, Idempotent("ref"), NeedsApproval(func(in payout) any { return in }))
	read := Func("get_balance", "read", func(context.Context, struct{}) (string, error) { return "1000.00", nil })
	g, err := OpenGateway(context.Background(), &FileLog{Path: path}, WithTools(pay, read), WithStartedBy("support-agent"),
		WithAuthorizer(All(AllowList("finance", "support-agent"), NotRequester())))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

const t77 = `{"ref":"T-77","amount":"1200.00"}`

func TestGatewayApprovalsDontStopOtherCalls(t *testing.T) {
	path, paid := filepath.Join(t.TempDir(), "proxy.jsonl"), map[string]int{}
	agent := gatedGateway(t, path, paid)
	key := agentAsks(t, agent, paid)
	humanDecides(t, gatedGateway(t, path, paid), key) // the human, in another process
	agentAsksAgain(t, agent, paid)
}

// agentAsks: a gated call is answered pending, with its key; nothing is paid, and nothing else is blocked.
func agentAsks(t *testing.T, agent *Gateway, paid map[string]int) string {
	t.Helper()
	first := gwCall(t, agent, "pay", t77)
	var p struct{ Status, Key string }
	_ = json.Unmarshal([]byte(first.Result), &p)
	if !first.Pending || p.Status != "pending_approval" || len(p.Key) != 32 || paid["T-77"] != 0 {
		t.Fatalf("a gated call is answered pending, with its key, and nothing is paid: %+v", first)
	}
	if r := gwCall(t, agent, "get_balance", `{}`); r.Result != `"1000.00"` {
		t.Fatalf("other calls go on while it waits: %+v", r)
	}
	if again := gwCall(t, agent, "pay", t77); !again.Pending || paid["T-77"] != 0 {
		t.Fatalf("asking again while it waits: still pending: %+v", again)
	}
	return p.Key
}

// humanDecides: the approver sees what waits; the wrong people are refused; finance approves.
func humanDecides(t *testing.T, approver *Gateway, key string) {
	t.Helper()
	ctx := context.Background()
	waiting, err := approver.Pending(ctx)
	if err != nil || len(waiting) != 1 || waiting[0].Key != key || !strings.Contains(waiting[0].Summary, "1200.00") {
		t.Fatalf("the approver sees the one operation waiting, with its checked values: %+v %v", waiting, err)
	}
	for who, why := range map[string]string{"intern": "not on the approver list", "support-agent": "started this run"} {
		if err := approver.Approve(ctx, key, who); !errors.Is(err, ErrNotAuthorized) || !strings.Contains(err.Error(), why) {
			t.Fatalf("%s must be refused (%s): %v", who, why, err)
		}
	}
	if err := approver.Approve(ctx, key, "finance"); err != nil {
		t.Fatal(err)
	}
	if err := approver.Approve(ctx, key, "finance"); err != nil {
		t.Fatalf("the same decision twice changes nothing: %v", err)
	}
	if err := approver.Reject(ctx, key, "finance", "late"); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("a decision can't be reversed: %v", err)
	}
}

// agentAsksAgain: the approved operation runs once, then is answered from the log; new values are a conflict.
func agentAsksAgain(t *testing.T, agent *Gateway, paid map[string]int) {
	t.Helper()
	if r := gwCall(t, agent, "pay", t77); r.Pending || r.Refused || paid["T-77"] != 1 {
		t.Fatalf("approved: the next call for it runs, once: %+v paid=%v", r, paid)
	}
	if r := gwCall(t, agent, "pay", t77); !r.Replayed || paid["T-77"] != 1 {
		t.Fatalf("and after that it's answered from the log: %+v paid=%v", r, paid)
	}
	if r := gwCall(t, agent, "pay", `{"ref":"T-77","amount":"9999.00"}`); !strings.Contains(r.Result, "conflict") || paid["T-77"] != 1 {
		t.Fatalf("the approved operation is the one that was approved: new values are a conflict: %+v", r)
	}
}

func TestGatewayRejectionSticksToTheOperation(t *testing.T) {
	ctx := context.Background()
	path, paid := filepath.Join(t.TempDir(), "proxy.jsonl"), map[string]int{}
	g := gatedGateway(t, path, paid)
	first := gwCall(t, g, "pay", `{"ref":"T-78","amount":"50.00"}`)
	var p struct{ Key string }
	_ = json.Unmarshal([]byte(first.Result), &p)
	if err := g.Reject(ctx, p.Key, "finance", "duplicate ticket"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if r := gwCall(t, g, "pay", `{"ref":"T-78","amount":"50.00"}`); !r.Refused || r.Pending || !strings.Contains(r.Result, "rejected") || paid["T-78"] != 0 {
			t.Fatalf("a rejected operation is refused every time it's asked for, never re-asked: %+v", r)
		}
	}
	if err := g.Approve(ctx, "no-such-key", "finance"); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("deciding something nobody asked for: %v", err)
	}
	if w, _ := g.Pending(ctx); len(w) != 0 {
		t.Fatalf("nothing is waiting: %+v", w)
	}
}
