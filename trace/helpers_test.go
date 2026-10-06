package trace

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

var started = agentsafe.Event{Type: agentsafe.EvRunStarted, System: "sys", Task: "task", MaxSteps: 8}

func call(id, name string) agentsafe.ToolCall {
	return agentsafe.ToolCall{ID: id, Type: "function", Function: agentsafe.FunctionCall{Name: name, Arguments: "{}"}}
}

func decided(step int, calls ...agentsafe.ToolCall) agentsafe.Event {
	return agentsafe.Event{Type: agentsafe.EvModelDecided, Step: step, FinishReason: "tool_calls",
		Message: &agentsafe.Message{Role: agentsafe.RoleAssistant, ToolCalls: calls}}
}

func answered(step int, text string) agentsafe.Event {
	return agentsafe.Event{Type: agentsafe.EvModelDecided, Step: step, FinishReason: "stop",
		Message: &agentsafe.Message{Role: agentsafe.RoleAssistant, Content: agentsafe.Str(text)}}
}

// Runs built with the public API only: the traces in these tests come from real runs.

type payIn struct {
	Ref    string  `json:"ref"`
	Amount float64 `json:"amount"`
}

func payTool() agentsafe.Tool {
	return agentsafe.Func("pay", "pay", func(context.Context, payIn) (string, error) { return "paid", nil },
		agentsafe.Idempotent("ref"), agentsafe.NeedsApproval(func(p payIn) any { return p }))
}

// newRun: two calls to a plain tool, then an answer.
func newRun(t *testing.T) (*agentsafe.Runner, *agentsafe.ScriptedModel, agentsafe.Tool) {
	t.Helper()
	work := agentsafe.Func("work", "work", func(context.Context, struct{}) (string, error) { return "ok", nil })
	model := &agentsafe.ScriptedModel{Final: "done",
		Plan: []agentsafe.FunctionCall{{Name: "work", Arguments: "{}"}, {Name: "work", Arguments: "{}"}}}
	return &agentsafe.Runner{Model: model, Tools: []agentsafe.Tool{work},
		Log: &agentsafe.FileLog{Path: filepath.Join(t.TempDir(), "run.jsonl")}}, model, work
}

// payRun: one gated payout; set the Runner's Authorizer before starting it.
func payRun(t *testing.T, args string) (*agentsafe.Runner, agentsafe.Tool) {
	t.Helper()
	tool := payTool()
	return &agentsafe.Runner{Tools: []agentsafe.Tool{tool},
		Model: &agentsafe.ScriptedModel{Plan: []agentsafe.FunctionCall{{Name: "pay", Arguments: args}}, Final: "done"},
		Log:   &agentsafe.FileLog{Path: filepath.Join(t.TempDir(), "run.jsonl")}}, tool
}

// waitingRun: a payout run paused at its approval gate.
func waitingRun(t *testing.T, z agentsafe.Authorizer) (*agentsafe.Runner, agentsafe.Tool, string) {
	t.Helper()
	r, tool := payRun(t, `{"ref":"T1007","amount":11000}`)
	r.Authorizer = z
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil || st.Waiting == nil {
		t.Fatalf("setup: want a run awaiting approval: %v", err)
	}
	return r, tool, st.Waiting.Key
}
