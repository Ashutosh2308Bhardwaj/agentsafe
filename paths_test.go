package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Rules the code claims on its error paths, one test each.

// A payment in the system of record that the run never made (made by hand, by another system, by a bug)
// is a CRITICAL finding: money that moved outside the agent's approvals.
func TestReconcileFlagsWhatShouldntExist(t *testing.T) {
	rep := Reconcile(nil, []Effect{
		{ID: "payment:T9", Kind: "payment", Gated: true, Key: "manual-1"},
		{ID: "discrepancy:T3:extra", Kind: "discrepancy", Key: "k3"},
	}, nil, nil)
	var crit, errs int
	for _, f := range rep.Findings {
		if f.Check == "no-extras" {
			switch f.Severity {
			case Critical:
				crit++
			case Error:
				errs++
			case Warn:
				t.Errorf("an extra effect is never just a warning: %v", f)
			}
		}
	}
	if crit != 1 || errs != 1 {
		t.Fatalf("an extra payment is CRITICAL, an extra record an ERROR: %v", rep)
	}
}

// A codec that fails (a KMS outage) stops the write: nothing is stored, in plaintext or otherwise.
type brokenCodec struct{ sealErr, openGarbage bool }

func (b brokenCodec) Seal(_ context.Context, p, _ []byte) ([]byte, error) {
	if b.sealErr {
		return nil, errors.New("kms unavailable")
	}
	return p, nil
}
func (b brokenCodec) Open(_ context.Context, c, _ []byte) ([]byte, error) {
	if b.openGarbage {
		return []byte("not json"), nil
	}
	return c, nil
}

func TestCodecFailures(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "r.jsonl")
	l := &FileLog{Path: path, Codec: brokenCodec{sealErr: true}}
	if err := l.Append(ctx, Event{Type: EvRunStarted, Task: "secret task", MaxSteps: 1}); err == nil || !strings.Contains(err.Error(), "kms unavailable") {
		t.Fatalf("a failing codec must fail the write: %v", err)
	}
	if b, _ := os.ReadFile(path); len(b) != 0 {
		t.Fatalf("nothing may be written when sealing fails, got %q", b)
	}

	ok := &FileLog{Path: path, Codec: brokenCodec{}}
	if err := ok.Append(ctx, Event{Type: EvRunStarted, Task: "t", MaxSteps: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&FileLog{Path: path, Codec: brokenCodec{openGarbage: true}}).Read(ctx); !errors.Is(err, ErrCannotOpen) {
		t.Fatalf("garbage from a codec must be refused, not read as an event: %v", err)
	}
	for name, ct := range map[string][]byte{"no nonce": {2, 'k', '1', 1, 2}, "unknown key id": {2, 'z', 'z', 1}} {
		if _, err := k1.Open(ctx, ct, nil); err == nil {
			t.Errorf("%s: must not open", name)
		}
	}
	if _, err := Open(ctx, Event{Sealed: "!!not base64!!"}, k1); !errors.Is(err, ErrCannotOpen) {
		t.Errorf("a sealed field that isn't base64 must be ErrCannotOpen: %v", err)
	}
}

// The state machine's remaining refusals.
func TestMoreInvalidTransitions(t *testing.T) {
	waiting := []Event{started, decided(1, call("a", "pay")), {Type: EvApprovalRequested, CallID: "a", Key: "k"}}
	for name, events := range map[string][]Event{
		"model_decided without a message":    {started, {Type: EvModelDecided, Step: 1}},
		"tool_refused for an unknown call":   {started, decided(1, call("a", "pay")), {Type: EvToolRefused, CallID: "zz"}},
		"approval for a call already asked":  append(append([]Event{}, waiting[:2]...), Event{Type: EvApprovalRequested, CallID: "a", Key: "k"}, Event{Type: EvApprovalDecided, CallID: "a", Key: "k", Decision: "approved", By: "o"}, Event{Type: EvApprovalRequested, CallID: "a", Key: "k"}),
		"a decision for another call":        append(append([]Event{}, waiting...), Event{Type: EvApprovalDecided, CallID: "b", Key: "k", Decision: "approved", By: "o"}),
		"a decision that isn't one":          append(append([]Event{}, waiting...), Event{Type: EvApprovalDecided, CallID: "a", Key: "k", Decision: "maybe", By: "o"}),
		"approval_requested without a key":   {started, decided(1, call("a", "pay")), {Type: EvApprovalRequested, CallID: "a"}},
		"an unknown event type":              {started, {Type: "teleport"}},
		"budget_extended with nobody behind": {{Type: EvRunStarted, MaxSteps: 1}, {Type: EvRunPaused}, {Type: EvBudgetExtended, ExtraSteps: 1}},
	} {
		if _, err := Rebuild(events); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s must be refused, got %v", name, err)
		}
	}
}

func TestStoreAndJournalRefusals(t *testing.T) {
	ctx := context.Background()
	s := NewFileStore(filepath.Join(t.TempDir(), "r.jsonl"))
	if err := s.AppendLine(ctx, 1, []byte("{\"a\":1}\n{\"b\":2}")); err == nil {
		t.Fatal("a line containing a line break would become two events: it must be refused")
	}
	j := &Journal{Store: memStore{lines: [][]byte{[]byte(`{"seq":1,"type":"run_started"`)}}}
	if _, err := j.Read(ctx); !errors.Is(err, ErrCorruptLog) {
		t.Fatalf("a store that returns an unreadable line is corrupt: %v", err)
	}
	if _, err := (&Journal{Store: memStore{}}).Lock(ctx); !errors.Is(err, ErrNoLocker) {
		t.Fatalf("a store without a lease must say so: %v", err)
	}
}

type memStore struct{ lines [][]byte }

func (m memStore) ReadLines(context.Context) ([][]byte, error)   { return m.lines, nil }
func (m memStore) AppendLine(context.Context, int, []byte) error { return nil }

// A file the process can't write: the error surfaces; nothing pretends to be stored.
func TestUnwritableLog(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("POSIX permissions (and not as root)")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	l := &FileLog{Path: filepath.Join(dir, "r.jsonl")}
	if err := l.Append(context.Background(), Event{Type: EvRunStarted, MaxSteps: 1}); err == nil {
		t.Fatal("appending in a read-only directory must fail")
	}
	if _, err := l.Lock(context.Background()); err == nil {
		t.Fatal("taking the lease in a read-only directory must fail")
	}
}

// A tool result that can't be encoded, or arguments that aren't JSON, are told to the model as errors.
func TestUnencodableResultAndInvalidArguments(t *testing.T) {
	ch := Func("weird", "", func(context.Context, struct{}) (chan int, error) { return make(chan int), nil })
	r, _ := New(&ScriptedModel{Plan: []FunctionCall{{Name: "weird", Arguments: `{}`}, {Name: "weird", Arguments: `{not json`}}, Final: "done"},
		&FileLog{Path: filepath.Join(t.TempDir(), "r.jsonl")}, WithTools(ch))
	if _, err := r.Start(context.Background(), "s", "t"); err != nil {
		t.Fatal(err)
	}
	events, _ := r.Log.Read(context.Background())
	var results []string
	for _, e := range events {
		if e.Type == EvToolResult || e.Type == EvToolRefused {
			results = append(results, e.Result)
		}
	}
	if len(results) != 2 || !strings.Contains(results[0], "not serialisable") || !strings.Contains(results[1], "error") {
		t.Fatalf("got %v", results)
	}
}

// A gated tool written as a struct literal skips New's checks; the runner still refuses to gate a call it
// can't address by key, and a summary that fails stops the run rather than asking a human about nothing.
func TestGateRefusesWhatItCantAddress(t *testing.T) {
	ctx := context.Background()
	r := &Runner{Model: &ScriptedModel{Plan: []FunctionCall{{Name: "gated", Arguments: `{}`}}, Final: "x"},
		Tools: []Tool{gatedOnly{}}, Log: &FileLog{Path: filepath.Join(t.TempDir(), "r.jsonl")}, AnyApprover: true}
	if _, err := r.Start(ctx, "s", "t"); !errors.Is(err, ErrConfig) {
		t.Fatalf("a gated tool without a key must stop the run with ErrConfig: %v", err)
	}
	pay := &payTool{gateway: map[string]bool{}, valid: 1}
	bad := &summaryFails{pay}
	r2 := &Runner{Model: &ScriptedModel{Plan: []FunctionCall{{Name: "pay", Arguments: `{"ref":"x","amount":1}`}}, Final: "x"},
		Tools: []Tool{bad}, Log: &FileLog{Path: filepath.Join(t.TempDir(), "r2.jsonl")}, AnyApprover: true}
	if _, err := r2.Start(ctx, "s", "t"); err == nil || !strings.Contains(err.Error(), "summary unavailable") {
		t.Fatalf("a failing summary must stop the run: %v", err)
	}
}

type summaryFails struct{ *payTool }

func (s *summaryFails) Summary(json.RawMessage) (any, error) {
	return nil, errors.New("summary unavailable")
}

// A gated effect with no key can't be tied to any approval: CRITICAL.
func TestReconcileGatedEffectWithoutAKey(t *testing.T) {
	rep := Reconcile([]Effect{{ID: "payment:T1", Gated: true}}, []Effect{{ID: "payment:T1", Gated: true}}, nil, nil)
	for _, f := range rep.Findings {
		if f.Severity == Critical && strings.Contains(f.Detail, "no idempotency key") {
			return
		}
	}
	t.Fatalf("want a CRITICAL 'no idempotency key' finding: %v", rep)
}

// A started call may have run: it can't be resolved as "refused, never attempted".
func TestRefusingAStartedCallIsInvalid(t *testing.T) {
	_, err := Rebuild([]Event{started, decided(1, call("a", "pay")), {Type: EvToolStarted, CallID: "a"}, {Type: EvToolRefused, CallID: "a"}})
	if !errors.Is(err, ErrInvalidTransition) || !strings.Contains(err.Error(), "may have run") {
		t.Fatalf("got %v", err)
	}
}

type idemIn struct {
	InvoiceID string `json:"invoice_id"`
	N         int    `json:"n,omitempty"`
}

// An operation whose identity is blank has no identity: it must not get a key (every blank invoice would
// share it); arguments with trailing data, or a misconfigured tool, are refused.
func TestFuncIdentityAndDecodingEdges(t *testing.T) {
	tool := Func("pay", "", func(context.Context, idemIn) (int, error) { return 1, nil }, Idempotent("invoice_id")).(IdempotentTool)
	for _, args := range []string{`{"invoice_id":""}`, `{"n":3}`, `{"invoice_id":null}`} {
		if _, _, err := tool.Identity(json.RawMessage(args)); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Errorf("%s: a blank identity must not produce a key, got %v", args, err)
		}
	}
	if _, err := tool.Call(context.Background(), json.RawMessage(`{"invoice_id":"A"} {"invoice_id":"B"}`)); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Errorf("two JSON values where one is expected must be refused: %v", err)
	}
	broken := Func("pay", "", func(context.Context, idemIn) (int, error) { return 1, nil }, Idempotent())
	if _, err := broken.Call(context.Background(), json.RawMessage(`{"invoice_id":"A"}`)); err == nil || !strings.Contains(err.Error(), "at least one identity field") {
		t.Errorf("a misconfigured tool must refuse to run, saying why: %v", err)
	}
	if _, err := New(&ScriptedModel{}, nil); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "no Log") {
		t.Errorf("no Log: %v", err)
	}
}

// The trace shows what happened to a run: retries, pauses, extensions, rejections, and what's still open.
func TestTraceRendersEveryKindOfEvent(t *testing.T) {
	events := []Event{
		{Seq: 1, Type: EvRunStarted, MaxSteps: 1, Model: "m"},
		{Seq: 2, Type: EvModelDecided, Step: 1, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "a", Function: FunctionCall{Name: "pay"}}, {ID: "b", Function: FunctionCall{Name: "pay"}}}}, Usage: &Usage{PromptTokens: 3, CompletionTokens: 2}},
		{Seq: 3, Type: EvToolStarted, CallID: "a", Tool: "pay", Key: "k"},
		{Seq: 4, Type: EvToolStarted, CallID: "a", Tool: "pay", Key: "k"}, // a retry after a crash
		{Seq: 5, Type: EvToolResult, CallID: "a", Tool: "pay", Key: "k", Result: `{"error":"` + strings.Repeat("x", 200) + `"}`},
		{Seq: 6, Type: EvApprovalRequested, CallID: "b", Tool: "pay", Key: "k2"},
		{Seq: 7, Type: EvApprovalDecided, CallID: "b", Key: "k2", Decision: "rejected", By: "ops", Reason: "payee disputes it"},
		{Seq: 8, Type: EvToolRefused, CallID: "b", Tool: "pay", Key: "k2", Result: `{"error":"rejected"}`},
		{Seq: 9, Type: EvRunPaused, Reason: "budget_exhausted"},
		{Seq: 10, Type: EvBudgetExtended, ExtraSteps: 2, By: "ops"},
		{Seq: 11, Type: EvModelDecided, Step: 2, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Function: FunctionCall{Name: "pay"}}}}},
		{Seq: 12, Type: EvToolStarted, CallID: "c", Tool: "pay", Key: "k3"}, // crashed here: no result
	}
	tr, err := BuildTrace(events, "agent")
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
	if _, err := BuildTrace([]Event{{Type: EvModelDecided}}, "a"); err == nil {
		t.Error("a log that doesn't start with run_started isn't a run")
	}
	if _, err := BuildTrace([]Event{{V: FormatVersion + 1, Type: EvRunStarted}}, "a"); !errors.Is(err, ErrNewerLogFormat) {
		t.Errorf("a newer format must be refused, not guessed: %v", err)
	}
	if _, err := Upgrade(Event{V: -1}); !errors.Is(err, ErrCorruptLog) {
		t.Errorf("a version that never existed is corruption: %v", err)
	}
}

type flag bool

func TestSameOnBooleans(t *testing.T) {
	if eq, _ := Same(true, flag(true)); !eq {
		t.Error("a named bool compares by value")
	}
	if eq, why := Same(true, false); eq || !strings.Contains(why, "false, should be true") {
		t.Errorf("got %v %q", eq, why)
	}
}
