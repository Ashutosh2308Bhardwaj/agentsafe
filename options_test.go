package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewReportsEveryConfigurationMistakeAtOnce(t *testing.T) {
	pay := &payTool{gateway: map[string]bool{}, valid: 1} // gated + idempotent
	notIdem := gatedOnly{}
	_, err := New(nil, &memLog{}, WithTools(pay, notIdem, &plain{name: "export"}, &plain{name: "export"},
		&plain{name: "has space"}, nil), WithMaxSteps(-1))
	if !errors.Is(err, ErrConfig) || !errors.Is(err, ErrNoAuthorizer) {
		t.Fatalf("want ErrConfig (and ErrNoAuthorizer for the missing policy), got %v", err)
	}
	for _, want := range []string{"no Model", "has no lease", "can't be negative", `"gated" needs approval but isn't an IdempotentTool`,
		`two tools are named "export"`, `tool name "has space"`, "tool 5 is nil", "nobody may give it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	if _, err := New(&ScriptedModel{}, &memLog{}, WithoutLease(), WithAuthorizer(AllowList("a")), WithAnyApprover()); !errors.Is(err, ErrConfig) ||
		!strings.Contains(err.Error(), "choose one") {
		t.Fatalf("an Authorizer and AnyApprover together are ambiguous, got %v", err)
	}
}

func TestNewBuildsAWorkingRunner(t *testing.T) {
	pay := &payTool{gateway: map[string]bool{}, valid: 11000}
	r, err := New(&ScriptedModel{Plan: []FunctionCall{{Name: "pay", Arguments: `{"ref":"T1","amount":11000}`}}, Final: "done"},
		&FileLog{Path: filepath.Join(t.TempDir(), "run.jsonl")},
		WithTools(pay), WithAuthorizer(AllowList("ops")), WithStartedBy("scheduler"), WithMaxSteps(4),
		WithToolTimeout(time.Second), WithToolRetries(2, time.Millisecond), WithScope("batch-1"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil || st.Status != StatusAwaitingApproval {
		t.Fatalf("err=%v status=%s", err, st.Status)
	}
	if st, err = r.Approve(context.Background(), st.Waiting.Key, "ops"); err != nil || pay.paid != 1 || st.Status != StatusFinished {
		t.Fatalf("err=%v paid=%d status=%s", err, pay.paid, st.Status)
	}
}

// Every error a caller may act on is matchable with errors.Is.
func TestSentinelErrors(t *testing.T) {
	ctx := context.Background()
	r, _ := payRun(t, `{"ref":"T1007","amount":11000}`)
	st, _ := r.Start(ctx, "sys", "task")
	if _, err := r.Start(ctx, "sys", "task"); !errors.Is(err, ErrRunExists) {
		t.Errorf("Start twice: %v", err)
	}
	if _, err := r.Extend(ctx, 3, "ops"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("Extend a run that isn't paused: %v", err)
	}
	if _, err := r.Approve(ctx, "no-such-key", "ops@test"); !errors.Is(err, ErrNotWaiting) {
		t.Errorf("Approve an unknown key: %v", err)
	}
	if _, err := r.Approve(ctx, st.Waiting.Key, "ops@test"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reject(ctx, st.Waiting.Key, "ops@test", "no"); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("Reject after Approve: %v", err)
	}
	empty := &Runner{Model: r.Model, Log: &FileLog{Path: filepath.Join(t.TempDir(), "x.jsonl")}}
	if _, err := empty.Continue(ctx); !errors.Is(err, ErrNoRun) {
		t.Errorf("Continue an empty log: %v", err)
	}
	if _, err := Rebuild([]Event{{Type: EvToolResult, CallID: "x"}}); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("Rebuild an impossible history: %v", err)
	}
}

type gatedOnly struct{}

func (gatedOnly) Spec() ToolSpec {
	return ToolSpec{Name: "gated", Description: "g", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (gatedOnly) Call(context.Context, json.RawMessage) (any, error) { return nil, nil }
func (gatedOnly) NeedsApproval(json.RawMessage) bool                 { return true }
func (gatedOnly) Summary(json.RawMessage) (any, error)               { return nil, nil }
