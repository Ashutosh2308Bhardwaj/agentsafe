package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Error paths and branches the scenario tests don't reach, each checked for the behaviour that matters.

// The exact-text path of the Canonical fix: fractions float64 can't hold keep every digit, minimally.
func TestCanonicalExactFractions(t *testing.T) {
	for in, want := range map[string]string{
		`{"x":0.10000000000000000001}`:   `{"x":0.10000000000000000001}`,
		`{"x":123456789012345678.50}`:    `{"x":123456789012345678.5}`,
		`{"x":-9007199254740993}`:        `{"x":-9007199254740993}`,
		`{"x":1.00000000000000000000e0}`: `{"x":1}`, // round-trips: the old float form
	} {
		got, err := Canonical(json.RawMessage(in))
		if err != nil || got != want {
			t.Errorf("Canonical(%s) = %s (%v), want %s", in, got, err, want)
		}
	}
	a, _ := Canonical(json.RawMessage(`{"x":0.10000000000000000001}`))
	b, _ := Canonical(json.RawMessage(`{"x":0.10000000000000000002}`))
	if a == b {
		t.Fatal("two different fractions must not share a key")
	}
}

type schemaAll struct {
	Ratio   float64            `json:"ratio"`
	Count   uint32             `json:"count"`
	Num     json.Number        `json:"num"`
	Raw     json.RawMessage    `json:"raw,omitempty"`
	Labels  map[string]string  `json:"labels"`
	Nested  struct{ X bool }   `json:"nested"`
	Matrix  [][]int            `json:"matrix"`
	Skipped string             `json:"-"`
	ByName  map[string]Decimal `json:"by_name,omitempty"`
}

func TestSchemaForEveryInputKind(t *testing.T) {
	s, err := schemaOf(reflectTypeOf[schemaAll]())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(s, &got)
	b, _ := json.Marshal(got["properties"])
	for _, want := range []string{
		`"ratio":{"type":"number"}`, `"count":{"type":"integer"}`, `"num":{"type":"number"}`, `"raw":{}`,
		`"labels":{"additionalProperties":{"type":"string"},"type":"object"}`,
		`"nested":{"additionalProperties":false,"properties":{"X":{"type":"boolean"}},"required":["X"],"type":"object"}`,
		`"matrix":{"items":{"items":{"type":"integer"},"type":"array"},"type":"array"}`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
	if strings.Contains(string(b), "Skipped") {
		t.Error(`a json:"-" field must not reach the model`)
	}
	for name, typ := range map[string]any{"int keys": struct{ M map[int]string }{}, "a channel": struct{ C chan int }{}} {
		if _, err := schemaOf(reflectTypeOfValue(typ)); err == nil {
			t.Errorf("%s can't be a tool input and must be refused at startup", name)
		}
	}
}

// A model error stops the run with the error; nothing is logged for that step, so Continue retries it.
type failingModel struct{ calls int }

func (m *failingModel) Decide(context.Context, []Message, []ToolSpec) (Decision, error) {
	m.calls++
	if m.calls == 1 {
		return Decision{}, errors.New("provider down")
	}
	return Decision{Message: Message{Role: RoleAssistant, Content: Str("ok")}, FinishReason: "stop"}, nil
}

func TestModelErrorStopsTheRunAndContinueRetries(t *testing.T) {
	m := &failingModel{}
	r, err := New(m, &FileLog{Path: filepath.Join(t.TempDir(), "r.jsonl")}, WithLogf(func(string, ...any) {}),
		WithRedactor(RedactFields("x")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(context.Background(), "sys", "task"); err == nil || !strings.Contains(err.Error(), "provider down") {
		t.Fatalf("the model's error must surface, got %v", err)
	}
	st, err := r.Continue(context.Background())
	if err != nil || st.Status != StatusFinished || st.Text != "ok" {
		t.Fatalf("Continue must retry the model step: %v %s", err, st.Status)
	}
}

// A call to a tool that doesn't exist is answered with an error the model can read; the run goes on.
func TestUnknownToolIsAnErrorResultNotACrash(t *testing.T) {
	r, err := New(&ScriptedModel{Plan: []FunctionCall{{Name: "nope", Arguments: `{}`}}, Final: "done"},
		&FileLog{Path: filepath.Join(t.TempDir(), "r.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil || st.Status != StatusFinished {
		t.Fatalf("err=%v status=%s", err, st.Status)
	}
	events, _ := r.Log.Read(context.Background())
	var res string
	for _, e := range events {
		if e.Type == EvToolResult {
			res = e.Result
		}
	}
	if !strings.Contains(res, `no tool named \"nope\"`) {
		t.Fatalf("got %s", res)
	}
}

// Trace outcomes for the idempotency paths: replayed from the log, a conflict, deduplicated by the tool.
func TestTraceOutcomes(t *testing.T) {
	at := time.Unix(0, 0).UTC()
	ev := func(e Event, s int) Event { e.Time = at.Add(time.Duration(s) * time.Millisecond); return e }
	events := []Event{
		ev(Event{Type: EvRunStarted, Task: "t", MaxSteps: 4, Model: "m"}, 0),
		ev(Event{Type: EvModelDecided, Step: 1, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "a", Function: FunctionCall{Name: "pay"}}, {ID: "b", Function: FunctionCall{Name: "pay"}},
			{ID: "c", Function: FunctionCall{Name: "pay"}}}}}, 1),
		ev(Event{Type: EvToolStarted, CallID: "a", Tool: "pay", Key: "k"}, 2),
		ev(Event{Type: EvToolResult, CallID: "a", Tool: "pay", Key: "k", Replayed: true, Result: `{"replayed":true}`}, 3),
		ev(Event{Type: EvToolStarted, CallID: "b", Tool: "pay", Key: "k"}, 4),
		ev(Event{Type: EvToolResult, CallID: "b", Tool: "pay", Key: "k", Replayed: true, Result: `{"error":"conflict: different values"}`}, 5),
		ev(Event{Type: EvToolStarted, CallID: "c", Tool: "pay", Key: "k2"}, 6),
		ev(Event{Type: EvToolResult, CallID: "c", Tool: "pay", Key: "k2", Result: `{"already_recorded":true}`}, 7),
	}
	tr, err := BuildTrace(events, "agent")
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

func TestHeadOfALogThatCantBeRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl")
	l := &FileLog{Path: path}
	if h, err := l.Head(context.Background()); err != nil || h != "genesis" {
		t.Fatalf("an empty log's head is genesis: %q %v", h, err)
	}
	if err := writeFile(path, "not json\n{\"seq\":1}\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Head(context.Background()); !errors.Is(err, ErrCorruptLog) {
		t.Fatalf("Head of a damaged log must say so: %v", err)
	}
}

func reflectTypeOf[T any]() reflect.Type    { return reflect.TypeFor[T]() }
func reflectTypeOfValue(v any) reflect.Type { return reflect.TypeOf(v) }
func writeFile(path, content string) error  { return os.WriteFile(path, []byte(content), 0o600) }
