package agentsafe

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestTraceIsAPureFunctionOfTheLog(t *testing.T) {
	r, _, _ := newRun(t)
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	events, _ := r.Log.Read()
	a, err := BuildTrace(events, "test")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := BuildTrace(events, "test")
	ja, _ := a.OTLPJSON("svc")
	jb, _ := b.OTLPJSON("svc")
	if string(ja) != string(jb) {
		t.Fatal("same log must give the identical trace (deterministic IDs)")
	}
	var chats, tools int
	for _, s := range a.Spans[1:] {
		switch {
		case strings.HasPrefix(s.Name, "chat "):
			chats++
		case strings.HasPrefix(s.Name, "execute_tool "):
			tools++
		}
		if s.ParentID != a.Spans[0].SpanID || s.TraceID != a.Spans[0].TraceID {
			t.Fatalf("span %s not under the root", s.Name)
		}
	}
	if chats != 3 || tools != 2 {
		t.Fatalf("want 3 chat spans (2 tool steps + answer) and 2 tool spans, got %d and %d", chats, tools)
	}
	var otlp map[string]any
	if err := json.Unmarshal(ja, &otlp); err != nil || otlp["resourceSpans"] == nil {
		t.Fatalf("OTLP JSON invalid: %v", err)
	}
}

func TestTraceShowsACrashMidCall(t *testing.T) {
	// Crash after the effect, resume: the trace must show 2 attempts, not hide the retry.
	r, _ := setup(t, `{"txn":"T1","amount":100,"note":"a"}`)
	runUntil(t, r, "after_tool_executed", true)
	if _, err := r.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := r.Log.Read()
	tr, err := BuildTrace(events, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range tr.Spans {
		if strings.HasPrefix(s.Name, "execute_tool") && attrInt(s, "agentsafe.attempts") == 2 && len(s.Events) == 1 {
			return
		}
	}
	t.Fatalf("want an execute_tool span with 2 attempts and a retry event:\n%s", tr.Tree())
}

func TestTraceRefusesAnInvalidLog(t *testing.T) {
	_, err := BuildTrace([]Event{started, decided(1, call("a", "w")), {Type: EvRunFinished}}, "x")
	if err == nil {
		t.Fatal("a log that breaks the state machine must not produce a trace")
	}
}

func TestTreeShowsStepDetailsEvenWithoutAModelName(t *testing.T) {
	// Logs from before run_started recorded the model have spans named just "chat".
	ev := []Event{{Type: EvRunStarted, System: "s", Task: "t", MaxSteps: 4}, answered(1, "done"), {Type: EvRunFinished, Stop: "stop"}}
	tr, err := BuildTrace(ev, "x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tr.Tree(), "step 1  chat") {
		t.Fatalf("chat line lost its details:\n%s", tr.Tree())
	}
}
