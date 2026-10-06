package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

func TestTraceIsAPureFunctionOfTheLog(t *testing.T) {
	r, _, _ := newRun(t)
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	events, _ := r.Log.Read(context.Background())
	a, err := Build(events, "test")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Build(events, "test")
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
	type rec struct {
		Txn string `json:"txn"`
	}
	tool := agentsafe.Func("record", "record", func(context.Context, rec) (string, error) { return "ok", nil },
		agentsafe.Idempotent("txn"))
	model := &agentsafe.ScriptedModel{Plan: []agentsafe.FunctionCall{{Name: "record", Arguments: `{"txn":"T1"}`}}, Final: "done"}
	log := &agentsafe.FileLog{Path: filepath.Join(t.TempDir(), "run.jsonl")}
	crashed := false
	crashing, err := agentsafe.New(model, log, agentsafe.WithTools(tool), agentsafe.WithHook(func(p string) {
		if p == "after_tool_executed" && !crashed {
			crashed = true
			panic("kill -9: the effect happened, its result was never logged")
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = crashing.Start(context.Background(), "sys", "task")
	}()
	r, _ := agentsafe.New(model, &agentsafe.FileLog{Path: log.Path}, agentsafe.WithTools(tool))
	if _, err := r.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := r.Log.Read(context.Background())
	tr, err := Build(events, "test")
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
	_, err := Build([]agentsafe.Event{started, decided(1, call("a", "w")), {Type: agentsafe.EvRunFinished}}, "x")
	if err == nil {
		t.Fatal("a log that breaks the state machine must not produce a trace")
	}
}

func TestTreeShowsStepDetailsEvenWithoutAModelName(t *testing.T) {
	// Logs from before run_started recorded the model have spans named just "chat".
	ev := []agentsafe.Event{{Type: agentsafe.EvRunStarted, System: "s", Task: "t", MaxSteps: 4}, answered(1, "done"), {Type: agentsafe.EvRunFinished, Stop: "stop"}}
	tr, err := Build(ev, "x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tr.Tree(), "step 1  chat") {
		t.Fatalf("chat line lost its details:\n%s", tr.Tree())
	}
}

func TestDenialShowsUpInTheTrace(t *testing.T) {
	r, _, key := waitingRun(t, agentsafe.AllowList("ops@test"))
	r.Approve(context.Background(), key, "mallory")
	events, _ := r.Log.Read(context.Background())
	tr, err := Build(events, "t")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range tr.Spans {
		for _, ev := range s.Events {
			if ev.Name == "denied: approved by mallory" && len(ev.Attrs) == 1 &&
				strings.Contains(fmt.Sprint(ev.Attrs[0].Value), "not on the approver list") {
				return
			}
		}
	}
	t.Fatal("want a 'denied' span event with the reason on the approval span")
}

func TestTraceRedactMasksContentKeepsStructure(t *testing.T) {
	r, _ := payRun(t, `{"ref":"T1007","amount":11000}`)
	// A policy whose refusal quotes content: the denial reason must be masked; the identity stays (audit).
	r.Authorizer = agentsafe.AuthorizerFunc(func(_ context.Context, a agentsafe.Approval) error {
		if a.By != "ops@test" {
			return fmt.Errorf("payout %s needs ops", a.Summary)
		}
		return nil
	})
	st, _ := r.Start(context.Background(), "sys", "task")
	_, _ = r.Approve(context.Background(), st.Waiting.Key, "mallory")
	_, _ = r.Approve(context.Background(), st.Waiting.Key, "ops@test")
	events, _ := r.Log.Read(context.Background())
	tr, err := Build(events, "t")
	if err != nil {
		t.Fatal(err)
	}
	tr.Redact(agentsafe.Redactors(agentsafe.RedactFields("ref"), agentsafe.RedactPattern(regexp.MustCompile(`T\d{4}`))))
	otlp, err := tr.OTLPJSON("svc")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(otlp), "T1007") || strings.Contains(tr.Tree(), "T1007") {
		t.Fatalf("T1007 survived trace redaction:\n%s", tr.Tree())
	}
	for _, kept := range []string{"execute_tool", "approval", st.Waiting.Key, "ops@test", "denied: approved by mallory", agentsafe.Masked} {
		if !strings.Contains(string(otlp), kept) {
			t.Fatalf("%q should still be in the exported trace", kept)
		}
	}
}

func TestTraceRedactMasksToolErrorMessages(t *testing.T) {
	events := []agentsafe.Event{started, decided(1, call("a", "pay")), {Type: agentsafe.EvToolStarted, CallID: "a", Tool: "pay"},
		{Type: agentsafe.EvToolResult, CallID: "a", Tool: "pay", Result: `{"error":"account 9876543210 is frozen"}`}}
	tr, err := Build(events, "t")
	if err != nil {
		t.Fatal(err)
	}
	tr.Redact(agentsafe.RedactPattern(regexp.MustCompile(`\b\d{9,18}\b`)))
	for _, sp := range tr.Spans {
		if strings.Contains(sp.StatusMsg, "9876543210") {
			t.Fatalf("a tool's error message is content: %q", sp.StatusMsg)
		}
		if strings.Contains(sp.StatusMsg, "is frozen") {
			return
		}
	}
	t.Fatal("the masked error message should still be on the span")
}

// Trace outcomes for the idempotency paths: replayed from the log, a conflict, deduplicated by the tool.
func TestTraceOutcomes(t *testing.T) {
	at := time.Unix(0, 0).UTC()
	ev := func(e agentsafe.Event, s int) agentsafe.Event {
		e.Time = at.Add(time.Duration(s) * time.Millisecond)
		return e
	}
	events := []agentsafe.Event{
		ev(agentsafe.Event{Type: agentsafe.EvRunStarted, Task: "t", MaxSteps: 4, Model: "m"}, 0),
		ev(agentsafe.Event{Type: agentsafe.EvModelDecided, Step: 1, Message: &agentsafe.Message{Role: agentsafe.RoleAssistant, ToolCalls: []agentsafe.ToolCall{
			{ID: "a", Function: agentsafe.FunctionCall{Name: "pay"}}, {ID: "b", Function: agentsafe.FunctionCall{Name: "pay"}},
			{ID: "c", Function: agentsafe.FunctionCall{Name: "pay"}}}}}, 1),
		ev(agentsafe.Event{Type: agentsafe.EvToolStarted, CallID: "a", Tool: "pay", Key: "k"}, 2),
		ev(agentsafe.Event{Type: agentsafe.EvToolResult, CallID: "a", Tool: "pay", Key: "k", Replayed: true, Result: `{"replayed":true}`}, 3),
		ev(agentsafe.Event{Type: agentsafe.EvToolStarted, CallID: "b", Tool: "pay", Key: "k"}, 4),
		ev(agentsafe.Event{Type: agentsafe.EvToolResult, CallID: "b", Tool: "pay", Key: "k", Replayed: true, Result: `{"error":"conflict: different values"}`}, 5),
		ev(agentsafe.Event{Type: agentsafe.EvToolStarted, CallID: "c", Tool: "pay", Key: "k2"}, 6),
		ev(agentsafe.Event{Type: agentsafe.EvToolResult, CallID: "c", Tool: "pay", Key: "k2", Result: `{"already_recorded":true}`}, 7),
	}
	tr, err := Build(events, "agent")
	if err != nil {
		t.Fatal(err)
	}
	var outcomes []string
	for _, s := range tr.Spans {
		if o, ok := get(s, "agentsafe.outcome").(string); ok && o != "" {
			outcomes = append(outcomes, o)
		}
	}
	if strings.Join(outcomes, ",") != "replayed,conflict,deduplicated_by_tool" {
		t.Fatalf("got %v", outcomes)
	}
	if tree := tr.Tree(); !strings.Contains(tree, "replayed") || !strings.Contains(tree, "[awaiting_model") || strings.Contains(tree, " in /  out") {
		t.Fatalf("the tree shows outcomes and the unfinished run:\n%s", tree)
	}
}

// The trace shows what happened to a run: retries, pauses, extensions, rejections, and what's still open.
func TestTraceRendersEveryKindOfEvent(t *testing.T) {
	events := []agentsafe.Event{
		{Seq: 1, Type: agentsafe.EvRunStarted, MaxSteps: 1, Model: "m"},
		{Seq: 2, Type: agentsafe.EvModelDecided, Step: 1, Message: &agentsafe.Message{Role: agentsafe.RoleAssistant, ToolCalls: []agentsafe.ToolCall{
			{ID: "a", Function: agentsafe.FunctionCall{Name: "pay"}}, {ID: "b", Function: agentsafe.FunctionCall{Name: "pay"}}}}, Usage: &agentsafe.Usage{PromptTokens: 3, CompletionTokens: 2}},
		{Seq: 3, Type: agentsafe.EvToolStarted, CallID: "a", Tool: "pay", Key: "k"},
		{Seq: 4, Type: agentsafe.EvToolStarted, CallID: "a", Tool: "pay", Key: "k"}, // a retry after a crash
		{Seq: 5, Type: agentsafe.EvToolResult, CallID: "a", Tool: "pay", Key: "k", Result: `{"error":"` + strings.Repeat("x", 200) + `"}`},
		{Seq: 6, Type: agentsafe.EvApprovalRequested, CallID: "b", Tool: "pay", Key: "k2"},
		{Seq: 7, Type: agentsafe.EvApprovalDecided, CallID: "b", Key: "k2", Decision: "rejected", By: "ops", Reason: "payee disputes it"},
		{Seq: 8, Type: agentsafe.EvToolRefused, CallID: "b", Tool: "pay", Key: "k2", Result: `{"error":"rejected"}`},
		{Seq: 9, Type: agentsafe.EvRunPaused, Reason: "budget_exhausted"},
		{Seq: 10, Type: agentsafe.EvBudgetExtended, ExtraSteps: 2, By: "ops"},
		{Seq: 11, Type: agentsafe.EvModelDecided, Step: 2, Message: &agentsafe.Message{Role: agentsafe.RoleAssistant, ToolCalls: []agentsafe.ToolCall{{ID: "c", Function: agentsafe.FunctionCall{Name: "pay"}}}}},
		{Seq: 12, Type: agentsafe.EvToolStarted, CallID: "c", Tool: "pay", Key: "k3"}, // crashed here: no result
	}
	tr, err := Build(events, "agent")
	if err != nil {
		t.Fatal(err)
	}
	tree := tr.Tree()
	for _, want := range []string{"⚠ 2 attempts (crash mid-call)", "…", "rejected by ops", "OPEN", "3 in / 2 out"} {
		if !strings.Contains(tree, want) {
			t.Errorf("the tree must show %q:\n%s", want, tree)
		}
	}
	otlp, err := tr.OTLPJSON("svc")
	if err != nil || !strings.Contains(string(otlp), "paused: budget_exhausted") || !strings.Contains(string(otlp), "budget_extended") ||
		!strings.Contains(string(otlp), "payee disputes it") {
		t.Errorf("pause, extension and the rejection reason must be in the export: %v", err)
	}
	if _, err := Build([]agentsafe.Event{{Type: agentsafe.EvModelDecided}}, "a"); err == nil {
		t.Error("a log that doesn't start with run_started isn't a run")
	}
	if _, err := Build([]agentsafe.Event{{V: agentsafe.FormatVersion + 1, Type: agentsafe.EvRunStarted}}, "a"); !errors.Is(err, agentsafe.ErrNewerLogFormat) {
		t.Errorf("a newer format must be refused, not guessed: %v", err)
	}
	if _, err := agentsafe.Upgrade(agentsafe.Event{V: -1}); !errors.Is(err, agentsafe.ErrCorruptLog) {
		t.Errorf("a version that never existed is corruption: %v", err)
	}
}
