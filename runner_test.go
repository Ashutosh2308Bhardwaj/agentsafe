package agentsafe

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

type countTool struct {
	name  string
	calls int
}

func (c *countTool) Spec() ToolSpec {
	return ToolSpec{Name: c.name, Description: "test", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (c *countTool) Call(context.Context, json.RawMessage) (any, error) {
	c.calls++
	return map[string]int{"n": c.calls}, nil
}

func newRun(t *testing.T) (*Runner, *ScriptedModel, *countTool) {
	tool := &countTool{name: "work"}
	model := &ScriptedModel{Plan: []FunctionCall{{Name: "work", Arguments: "{}"}, {Name: "work", Arguments: "{}"}}, Final: "done"}
	log := &FileLog{Path: filepath.Join(t.TempDir(), "run.jsonl")}
	return &Runner{Model: model, Tools: []Tool{tool}, Log: log}, model, tool
}

func TestRunToCompletion(t *testing.T) {
	r, model, tool := newRun(t)
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusFinished || st.Stop != "stop" || st.Text != "done" || tool.calls != 2 || model.Calls != 3 {
		t.Fatalf("status=%s stop=%s text=%q tool=%d model=%d", st.Status, st.Stop, st.Text, tool.calls, model.Calls)
	}
	// The log alone reproduces the final state.
	events, _ := r.Log.Read(context.Background())
	rebuilt, err := Rebuild(events)
	if err != nil || rebuilt.Status != StatusFinished || len(rebuilt.Messages) != len(st.Messages) {
		t.Fatalf("rebuild mismatch: %v %+v", err, rebuilt)
	}
}

func TestBudgetExhaustedPausesThenExtendContinues(t *testing.T) {
	r, model, tool := newRun(t)
	r.MaxSteps = 1
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusPaused || st.Stop != "budget_exhausted" || st.Step != 1 {
		t.Fatalf("want paused after 1 step, got %+v", st)
	}
	// Continue on a paused run does nothing: more budget is a decision, not a default.
	calls := model.Calls
	if st, _ = r.Continue(context.Background()); st.Status != StatusPaused || model.Calls != calls {
		t.Fatalf("Continue must not resume a paused run: %+v", st)
	}
	// A fresh Runner with a DIFFERENT configured budget must still use the logged one.
	r.MaxSteps = 99
	st, err = r.Extend(context.Background(), 5, "test")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusFinished || st.Budget != 6 || tool.calls != 2 {
		t.Fatalf("want finished with budget 1+5, got %+v (tool calls %d)", st, tool.calls)
	}
}

func TestContinueFinishedRunDoesNothing(t *testing.T) {
	r, model, tool := newRun(t)
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	before := model.Calls
	st, err := r.Continue(context.Background())
	if err != nil || st.Status != StatusFinished || model.Calls != before || tool.calls != 2 {
		t.Fatalf("continue on a finished run must be a no-op: %v model=%d tool=%d", err, model.Calls, tool.calls)
	}
}

func TestStartRefusesExistingLog(t *testing.T) {
	r, _, _ := newRun(t)
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(context.Background(), "sys", "task"); err == nil {
		t.Fatal("second Start on the same log must fail")
	}
}
