package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// payTool: idempotent, validated, gated. paid counts REAL payments; the gateway dedupes on key.
type payTool struct {
	paid    int
	gateway map[string]bool
	valid   float64 // the only amount Validate accepts (the "source of truth")
}

func (p *payTool) Spec() ToolSpec {
	return ToolSpec{Name: "pay", Description: "test payout", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (p *payTool) Call(context.Context, json.RawMessage) (any, error) { panic("must use CallWithKey") }
func (p *payTool) args(a json.RawMessage) (ref string, amount float64) {
	var v struct {
		Ref    string  `json:"ref"`
		Amount float64 `json:"amount"`
	}
	json.Unmarshal(a, &v)
	return v.Ref, v.Amount
}
func (p *payTool) Identity(a json.RawMessage) (any, any, error) {
	ref, amt := p.args(a)
	return map[string]any{"ref": ref}, map[string]any{"amount": amt}, nil
}
func (p *payTool) Validate(_ context.Context, a json.RawMessage) error {
	if _, amt := p.args(a); amt != p.valid {
		return errors.New("amount does not match the source")
	}
	return nil
}
func (p *payTool) NeedsApproval(json.RawMessage) bool { return true }
func (p *payTool) Summary(a json.RawMessage) (any, error) {
	ref, amt := p.args(a)
	return map[string]any{"ref": ref, "amount": amt}, nil
}
func (p *payTool) CallWithKey(_ context.Context, key string, _ json.RawMessage) (any, error) {
	if !p.gateway[key] {
		p.gateway[key] = true
		p.paid++
	}
	return map[string]any{"paid": true}, nil
}

func payRun(t *testing.T, args ...string) (*Runner, *payTool) {
	tool := &payTool{gateway: map[string]bool{}, valid: 11000}
	var plan []FunctionCall
	for _, a := range args {
		plan = append(plan, FunctionCall{Name: "pay", Arguments: a})
	}
	return &Runner{Model: &ScriptedModel{Plan: plan, Final: "done"}, Tools: []Tool{tool},
		Log: &FileLog{Path: filepath.Join(t.TempDir(), "run.jsonl")}, MaxSteps: 8,
		Authorizer: AllowList("ops@test"), StartedBy: "scheduler"}, tool
}

func TestGatedCallWaitsDurablyThenRunsOnceOnApproval(t *testing.T) {
	r, tool := payRun(t, `{"ref":"T1007","amount":11000}`)
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusAwaitingApproval || tool.paid != 0 || st.Waiting == nil {
		t.Fatalf("want awaiting_approval and nothing paid, got %s paid=%d", st.Status, tool.paid)
	}
	key := st.Waiting.Key
	// A brand-new Runner (different process, days later) sees the same pending approval.
	r2 := &Runner{Model: r.Model, Tools: r.Tools, Log: r.Log, Authorizer: r.Authorizer}
	if st2, _ := r2.Continue(context.Background()); st2.Status != StatusAwaitingApproval || st2.Waiting.Key != key {
		t.Fatalf("approval wait must survive a restart: %+v", st2.Waiting)
	}
	st, err = r2.Approve(context.Background(), key, "ops@test")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusFinished || tool.paid != 1 {
		t.Fatalf("want finished with exactly one payment, got %s paid=%d", st.Status, tool.paid)
	}
	// Approving again is a no-op, not a second payment.
	if _, err := r2.Approve(context.Background(), key, "ops@test"); err != nil || tool.paid != 1 {
		t.Fatalf("second approval must be a no-op: err=%v paid=%d", err, tool.paid)
	}
	// And a decision can't be flipped afterwards.
	if _, err := r2.Reject(context.Background(), key, "ops@test", "changed my mind"); err == nil {
		t.Fatal("rejecting an approved operation must fail")
	}
}

func TestRejectionRefusesTheCallAndNothingRuns(t *testing.T) {
	r, tool := payRun(t, `{"ref":"T1007","amount":11000}`)
	st, _ := r.Start(context.Background(), "sys", "task")
	st, err := r.Reject(context.Background(), st.Waiting.Key, "ops@test", "payee disputes it")
	if err != nil {
		t.Fatal(err)
	}
	if tool.paid != 0 || st.Status != StatusFinished {
		t.Fatalf("rejected payout must not run: paid=%d status=%s", tool.paid, st.Status)
	}
	events, _ := r.Log.Read(context.Background())
	var refused bool
	for _, e := range events {
		refused = refused || (e.Type == EvToolRefused && strings.Contains(e.Result, "rejected"))
	}
	if !refused {
		t.Fatal("want a tool_refused event telling the model it was rejected")
	}
}

func TestInvalidCallNeverReachesTheApprover(t *testing.T) {
	// The model proposes the wrong amount (W3-5-style). Code catches it; no human is asked.
	r, tool := payRun(t, `{"ref":"T1007","amount":7300}`)
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil {
		t.Fatal(err)
	}
	events, _ := r.Log.Read(context.Background())
	for _, e := range events {
		if e.Type == EvApprovalRequested {
			t.Fatal("an invalid call must be refused before approval is requested")
		}
	}
	if tool.paid != 0 || st.Status != StatusFinished {
		t.Fatalf("got paid=%d status=%s", tool.paid, st.Status)
	}
}

func TestCrashAfterApprovalBeforePaymentPaysOnce(t *testing.T) {
	r, tool := payRun(t, `{"ref":"T1007","amount":11000}`)
	st, _ := r.Start(context.Background(), "sys", "task")
	key := st.Waiting.Key
	r.Hook = func(p string) {
		if p == "after_tool_executed" {
			r.Hook = nil
			panic(crash{})
		}
	}
	func() {
		defer func() { recover() }()
		r.Approve(context.Background(), key, "ops@test")
	}()
	if tool.paid != 1 {
		t.Fatalf("setup: payment should have happened before the crash, paid=%d", tool.paid)
	}
	st, err := r.Continue(context.Background())
	if err != nil || tool.paid != 1 || st.Status != StatusFinished {
		t.Fatalf("resume after approved+paid crash must not pay again: err=%v paid=%d status=%s", err, tool.paid, st.Status)
	}
}

func TestGateCannotBeSkippedInTheLog(t *testing.T) {
	_, err := Rebuild([]Event{started, decided(1, call("a", "pay")),
		{Type: EvApprovalRequested, CallID: "a", Key: "k"}, {Type: EvToolStarted, CallID: "a"}})
	if err == nil || !strings.Contains(err.Error(), "tool_started in status awaiting_approval") {
		t.Fatalf("starting a call that awaits approval must be refused, got %v", err)
	}
	_, err = Rebuild([]Event{started, decided(1, call("a", "pay")),
		{Type: EvApprovalRequested, CallID: "a", Key: "k"},
		{Type: EvApprovalDecided, CallID: "a", Key: "k", Decision: "rejected", By: "ops"},
		{Type: EvToolStarted, CallID: "a"}})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("starting a rejected call must be refused, got %v", err)
	}
}

// The model retries a rejected payout under a new call id (FAILURES.md: "it retries under a new identity").
// It's the same operation, so it's refused without asking the human again, and nothing is paid.
func TestRejectedOperationIsNotAskedAgain(t *testing.T) {
	r, tool := payRun(t, `{"ref":"T1007","amount":11000}`, `{"ref":"T1007","amount":11000}`)
	st, _ := r.Start(context.Background(), "sys", "task")
	st, err := r.Reject(context.Background(), st.Waiting.Key, "ops@test", "payee disputes it")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusFinished || tool.paid != 0 {
		t.Fatalf("the re-proposal must be refused without a second approval request: status=%s paid=%d", st.Status, tool.paid)
	}
	events, _ := r.Log.Read(context.Background())
	requests, refusedAgain := 0, false
	for _, e := range events {
		requests += map[bool]int{true: 1}[e.Type == EvApprovalRequested]
		refusedAgain = refusedAgain || (e.Type == EvToolRefused && strings.Contains(e.Result, "rejected by a human approver earlier in this run"))
	}
	if requests != 1 || !refusedAgain {
		t.Fatalf("one approval request, then an automatic refusal: requests=%d refusedAgain=%v", requests, refusedAgain)
	}
}
