package agentsafe

import (
	"strings"
	"testing"
)

func call(id, name string) ToolCall {
	return ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: name, Arguments: "{}"}}
}

func decided(step int, calls ...ToolCall) Event {
	return Event{Type: EvModelDecided, Step: step, Message: &Message{Role: RoleAssistant, ToolCalls: calls}, FinishReason: "tool_calls"}
}

func answered(step int, text string) Event {
	return Event{Type: EvModelDecided, Step: step, Message: &Message{Role: RoleAssistant, Content: Str(text)}, FinishReason: "stop"}
}

var started = Event{Type: EvRunStarted, System: "sys", Task: "task", MaxSteps: 8}

func TestHappyPath(t *testing.T) {
	s, err := Rebuild([]Event{
		started,
		decided(1, call("a", "read")),
		{Type: EvToolStarted, CallID: "a"},
		{Type: EvToolResult, CallID: "a", Result: `{"ok":true}`},
		answered(2, "done"),
		{Type: EvRunFinished, Stop: "stop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusFinished || s.Step != 2 || s.Text != "done" || len(s.Messages) != 4 {
		t.Fatalf("got %+v", s)
	}
}

func TestPendingAndMaybeExecuted(t *testing.T) {
	// Crash after tool_started for "a", before its result; "b" never started.
	s, err := Rebuild([]Event{started, decided(1, call("a", "write"), call("b", "write")), {Type: EvToolStarted, CallID: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusExecuting || len(s.Pending) != 2 || !s.Started["a"] || s.Started["b"] {
		t.Fatalf("want a=maybe executed, b=never ran; got %+v", s)
	}
}

func TestIllegalTransitions(t *testing.T) {
	cases := map[string]struct {
		events []Event
		want   string
	}{
		"finish with a pending call (F14)": {
			[]Event{started, decided(1, call("a", "w")), {Type: EvRunFinished, Stop: "stop"}}, "run_finished in status executing"},
		"result without start": {
			[]Event{started, decided(1, call("a", "w")), {Type: EvToolResult, CallID: "a"}}, "no tool_started"},
		"result for a call nobody proposed": {
			[]Event{started, decided(1, call("a", "w")), {Type: EvToolStarted, CallID: "zzz"}}, "not a pending call"},
		"second result for the same call (F8)": {
			[]Event{started, decided(1, call("a", "w"), call("b", "w")), {Type: EvToolStarted, CallID: "a"},
				{Type: EvToolResult, CallID: "a"}, {Type: EvToolResult, CallID: "a"}}, "not a pending call"},
		"new decision while calls are pending": {
			[]Event{started, decided(1, call("a", "w")), decided(2, call("b", "w"))}, "model_decided in status executing"},
		"step out of order": {
			[]Event{started, decided(2, call("a", "w"))}, "expected 1"},
		"repeated call id in one decision": {
			[]Event{started, decided(1, call("a", "w"), call("a", "w"))}, "repeated id"},
		"event before run_started": {
			[]Event{decided(1, call("a", "w"))}, "status new"},
		"run without a budget": {
			[]Event{{Type: EvRunStarted, System: "s", Task: "t"}}, "without a budget"},
		"decision beyond the budget": {
			[]Event{{Type: EvRunStarted, System: "s", Task: "t", MaxSteps: 1}, decided(1, call("a", "w")),
				{Type: EvToolStarted, CallID: "a"}, {Type: EvToolResult, CallID: "a"}, decided(2, call("b", "w"))}, "beyond budget"},
		"anything after run_finished": {
			[]Event{started, answered(1, "x"), {Type: EvRunFinished, Stop: "stop"}, {Type: EvBudgetExtended, ExtraSteps: 3, By: "ops"}}, "only a paused run"},
		"extend without saying who": {
			[]Event{{Type: EvRunStarted, System: "s", Task: "t", MaxSteps: 1}, decided(1, call("a", "w")),
				{Type: EvToolStarted, CallID: "a"}, {Type: EvToolResult, CallID: "a"},
				{Type: EvRunPaused, Reason: "budget_exhausted"}, {Type: EvBudgetExtended, ExtraSteps: 2}}, "who decided"},
		"executed twice under one key": {
			[]Event{started, decided(1, call("a", "w"), call("b", "w")),
				{Type: EvToolStarted, CallID: "a", Key: "k1"}, {Type: EvToolResult, CallID: "a", Key: "k1"},
				{Type: EvToolStarted, CallID: "b", Key: "k1"}, {Type: EvToolResult, CallID: "b", Key: "k1"}}, "second executed result"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Rebuild(c.events)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestRestartedCallIsAllowed(t *testing.T) {
	// After a crash the runner logs tool_started again for the same call: that's the retry, and legal.
	_, err := Rebuild([]Event{started, decided(1, call("a", "w")),
		{Type: EvToolStarted, CallID: "a"}, {Type: EvToolStarted, CallID: "a"}, {Type: EvToolResult, CallID: "a"}})
	if err != nil {
		t.Fatal(err)
	}
}
