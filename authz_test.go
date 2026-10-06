package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// waitingRun starts a gated payout run and returns it paused at the approval gate.
func waitingRun(t *testing.T, z Authorizer) (*Runner, *payTool, string) {
	t.Helper()
	r, tool := payRun(t, `{"ref":"T1007","amount":11000}`)
	r.Authorizer = z
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil || st.Status != StatusAwaitingApproval {
		t.Fatalf("setup: want awaiting_approval, got %s err=%v", st.Status, err)
	}
	return r, tool, st.Waiting.Key
}

// mustStillWait asserts a refused decision changed nothing: no payment, run still waiting on the same key,
// and the refusal is on record.
func mustStillWait(t *testing.T, r *Runner, tool *payTool, key string, denials int) {
	t.Helper()
	events, err := r.Log.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st, err := Rebuild(events)
	if err != nil {
		t.Fatal(err)
	}
	if tool.paid != 0 || st.Status != StatusAwaitingApproval || st.Waiting == nil || st.Waiting.Key != key {
		t.Fatalf("a refused decision must change nothing: paid=%d status=%s", tool.paid, st.Status)
	}
	if st.Denials != denials {
		t.Fatalf("want %d approval_denied on record, got %d", denials, st.Denials)
	}
}

func TestNoAuthorizerRefusesEveryDecision(t *testing.T) {
	// A run whose gated tools nobody may approve doesn't start at all: nothing is written.
	unstartable, _ := payRun(t, `{"ref":"T1007","amount":11000}`)
	unstartable.Authorizer = nil
	if _, err := unstartable.Start(context.Background(), "sys", "task"); !errors.Is(err, ErrConfig) || !errors.Is(err, ErrNoAuthorizer) {
		t.Fatalf("a gate nobody may open must stop the run before it starts, got %v", err)
	}
	if events, _ := unstartable.Log.Read(context.Background()); len(events) != 0 {
		t.Fatalf("nothing may be written for a run that can't be configured, got %d events", len(events))
	}

	// A waiting run whose policy is taken away refuses every decision.
	r, tool, key := waitingRun(t, AllowList("ops@test"))
	r.Authorizer = nil
	if _, err := r.Approve(context.Background(), key, "cfo"); !errors.Is(err, ErrNoAuthorizer) {
		t.Fatalf("an unconfigured gate must refuse, got %v", err)
	}
	if _, err := r.Reject(context.Background(), key, "cfo", "no"); !errors.Is(err, ErrNoAuthorizer) {
		t.Fatalf("rejection is a decision too, got %v", err)
	}
	// A configuration error isn't an attempt by anyone, so nothing is logged.
	mustStillWait(t, r, tool, key, 0)

	r.AnyApprover = true // the explicit opt-out
	if st, err := r.Approve(context.Background(), key, "anyone"); err != nil || tool.paid != 1 || st.Status != StatusFinished {
		t.Fatalf("AnyApprover must allow: err=%v paid=%d", err, tool.paid)
	}
}

func TestOutsiderCannotApproveAndTheAttemptIsRecorded(t *testing.T) {
	r, tool, key := waitingRun(t, AllowList("ops@test"))
	_, err := r.Approve(context.Background(), key, "cfo") // claims a big title; isn't on the list
	if !errors.Is(err, ErrNotAuthorized) || !strings.Contains(err.Error(), `"cfo" is not on the approver list`) {
		t.Fatalf("want ErrNotAuthorized naming the outsider, got %v", err)
	}
	if _, err := r.Reject(context.Background(), key, "cfo", "blocking it"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("an outsider can't block a payout either, got %v", err)
	}
	mustStillWait(t, r, tool, key, 2)

	events, _ := r.Log.Read(context.Background())
	last := events[len(events)-1]
	if last.Type != EvApprovalDenied || last.By != "cfo" || last.Decision != "rejected" || last.Key != key {
		t.Fatalf("the refusal must be logged with who, what and which operation: %+v", last)
	}
	// The run is still decidable by someone who is allowed.
	if st, err := r.Approve(context.Background(), key, "ops@test"); err != nil || tool.paid != 1 || st.Status != StatusFinished {
		t.Fatalf("an allowed approver must still be able to decide: err=%v paid=%d", err, tool.paid)
	}
}

func TestMakerCannotCheckTheirOwnRun(t *testing.T) {
	r, tool, key := waitingRun(t, NotRequester()) // payRun starts runs as "scheduler"
	if _, err := r.Approve(context.Background(), key, "scheduler"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("whoever started the run must not approve it, got %v", err)
	}
	mustStillWait(t, r, tool, key, 1)
	if _, err := r.Approve(context.Background(), key, "ops@test"); err != nil || tool.paid != 1 {
		t.Fatalf("someone else may: err=%v paid=%d", err, tool.paid)
	}
}

func TestNotRequesterRefusesARunWithNoRecordedRequester(t *testing.T) {
	r, tool := payRun(t, `{"ref":"T1007","amount":11000}`)
	r.Authorizer, r.StartedBy = NotRequester(), ""
	st, _ := r.Start(context.Background(), "sys", "task")
	if _, err := r.Approve(context.Background(), st.Waiting.Key, "ops@test"); !errors.Is(err, ErrNotAuthorized) ||
		!strings.Contains(err.Error(), "no recorded requester") {
		t.Fatalf("a rule that can't be checked must refuse, not pass, got %v", err)
	}
	mustStillWait(t, r, tool, st.Waiting.Key, 1)
}

func TestAmountThresholdPolicySeesTheValidatedSummary(t *testing.T) {
	// Custom policy: above 10,000 needs the CFO. It reads Summary: the arguments that passed Validate and that
	// the approver was shown, never anything the caller passes in.
	var seen Approval
	threshold := AuthorizerFunc(func(_ context.Context, a Approval) error {
		seen = a
		var s struct{ Amount float64 }
		if err := json.Unmarshal(a.Summary, &s); err != nil {
			return err
		}
		if s.Amount > 10000 && a.By != "cfo" {
			return fmt.Errorf("%.0f is over 10000 and needs the cfo", s.Amount)
		}
		return nil
	})
	r, tool, key := waitingRun(t, All(AllowList("ops@test", "cfo"), NotRequester(), threshold))
	if _, err := r.Approve(context.Background(), key, "ops@test"); !errors.Is(err, ErrNotAuthorized) ||
		!strings.Contains(err.Error(), "needs the cfo") {
		t.Fatalf("ops can't approve 11000, got %v", err)
	}
	if seen.Tool != "pay" || seen.Key != key || seen.RunStartedBy != "scheduler" || seen.Decision != "approved" {
		t.Fatalf("the Authorizer must see the operation it decides on: %+v", seen)
	}
	mustStillWait(t, r, tool, key, 1)
	if _, err := r.Approve(context.Background(), key, "cfo"); err != nil || tool.paid != 1 {
		t.Fatalf("the cfo can: err=%v paid=%d", err, tool.paid)
	}
}

func TestAllStopsAtTheFirstRefusal(t *testing.T) {
	calls := 0
	count := AuthorizerFunc(func(context.Context, Approval) error { calls++; return nil })
	err := All(count, AllowList("x"), count).Authorize(context.Background(), Approval{By: "y"})
	if err == nil || calls != 1 {
		t.Fatalf("All must refuse at the first refusal: err=%v calls=%d", err, calls)
	}
	if err := All().Authorize(context.Background(), Approval{By: "y"}); err != nil {
		t.Fatalf("All() of nothing allows (use AnyApprover deliberately, not this): %v", err)
	}
}

func TestDenialCantBeForgedInTheLog(t *testing.T) {
	base := []Event{started, decided(1, call("a", "pay")), {Type: EvApprovalRequested, CallID: "a", Key: "k"}}
	for name, bad := range map[string]Event{
		"no by":       {Type: EvApprovalDenied, CallID: "a", Key: "k", Decision: "approved"},
		"wrong key":   {Type: EvApprovalDenied, CallID: "a", Key: "other", Decision: "approved", By: "x"},
		"not waiting": {Type: EvApprovalDenied, CallID: "b", Key: "k", Decision: "approved", By: "x"},
	} {
		if _, err := Rebuild(append(append([]Event{}, base...), bad)); err == nil {
			t.Fatalf("%s: an impossible approval_denied must be refused", name)
		}
	}
	// After the decision, a denial no longer makes sense either.
	done := append(append([]Event{}, base...), Event{Type: EvApprovalDecided, CallID: "a", Key: "k", Decision: "approved", By: "ops"},
		Event{Type: EvApprovalDenied, CallID: "a", Key: "k", Decision: "approved", By: "x"})
	if _, err := Rebuild(done); err == nil {
		t.Fatal("approval_denied after the decision must be refused")
	}
}

func TestV1ApprovalEventsUpgradeUnchanged(t *testing.T) {
	e, err := Upgrade(Event{V: 1, Type: EvApprovalDecided, Decision: "approved", By: "ops"})
	if err != nil || e.V != FormatVersion || e.Decision != "approved" || e.By != "ops" {
		t.Fatalf("v1 events must read unchanged at the current version: %+v err=%v", e, err)
	}
}
