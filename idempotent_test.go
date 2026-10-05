package agentsafe

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// ledgerTool is an IdempotentTool whose effect dedupes on the key, like a unique column or a gateway's
// Idempotency-Key. effects counts how many times the side effect REALLY happened.
type ledgerTool struct {
	rows    map[string]string // key -> row id: the downstream system's own memory
	effects int
}

func (l *ledgerTool) Spec() ToolSpec {
	return ToolSpec{Name: "record", Description: "test write", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (l *ledgerTool) Call(_ context.Context, _ json.RawMessage) (any, error) {
	panic("must use CallWithKey")
}
func (l *ledgerTool) Identity(a json.RawMessage) (any, any, error) {
	var v struct {
		Txn    string  `json:"txn"`
		Amount float64 `json:"amount"`
		Note   string  `json:"note"`
	}
	err := json.Unmarshal(a, &v)
	return map[string]any{"txn": v.Txn}, map[string]any{"amount": v.Amount}, err // note: in neither
}
func (l *ledgerTool) CallWithKey(_ context.Context, key string, _ json.RawMessage) (any, error) {
	if id, ok := l.rows[key]; ok {
		return map[string]any{"id": id, "deduped_by_tool": true}, nil
	}
	l.effects++
	l.rows[key] = "row1"
	return map[string]any{"id": "row1"}, nil
}

type crash struct{}

// runUntil runs r and recovers a simulated crash at `point`, so a test can then Continue on the same log.
func runUntil(t *testing.T, r *Runner, point string, start bool) {
	t.Helper()
	r.Hook = func(p string) {
		if p == point {
			r.Hook = nil
			panic(crash{})
		}
	}
	defer func() {
		if rec := recover(); rec != nil {
			if _, ok := rec.(crash); !ok {
				panic(rec)
			}
		}
	}()
	if start {
		r.Start(context.Background(), "sys", "task")
	} else {
		r.Continue(context.Background())
	}
}

func setup(t *testing.T, plan ...string) (*Runner, *ledgerTool) {
	tool := &ledgerTool{rows: map[string]string{}}
	var fc []FunctionCall
	for _, a := range plan {
		fc = append(fc, FunctionCall{Name: "record", Arguments: a})
	}
	return &Runner{Model: &ScriptedModel{Plan: fc, Final: "done"}, Tools: []Tool{tool},
		Log: &FileLog{Path: filepath.Join(t.TempDir(), "run.jsonl")}, MaxSteps: 8}, tool
}

func TestCrashAfterEffectDoesNotDoubleExecute(t *testing.T) {
	// THE week-2 gap: the effect happened, the process died before its result was logged.
	r, tool := setup(t, `{"txn":"T1","amount":100,"note":"a"}`)
	runUntil(t, r, "after_tool_executed", true)
	if tool.effects != 1 {
		t.Fatalf("setup: effect should have happened once before the crash, got %d", tool.effects)
	}
	st, err := r.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tool.effects != 1 || st.Status != StatusFinished {
		t.Fatalf("resume must not repeat the effect: effects=%d status=%s", tool.effects, st.Status)
	}
}

func TestRepeatedOperationIsReplayedFromTheLog(t *testing.T) {
	// The model proposes the same operation again in a LATER step, reworded note, new call id (F9, F13).
	r, tool := setup(t, `{"txn":"T1","amount":100,"note":"a"}`, `{"txn":"T1","amount":100.0,"note":"reworded"}`)
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil {
		t.Fatal(err)
	}
	if tool.effects != 1 {
		t.Fatalf("want 1 effect, got %d", tool.effects)
	}
	last := st.Messages[len(st.Messages)-1]
	var res map[string]any
	json.Unmarshal([]byte(*last.Content), &res)
	if res["already_recorded"] != true {
		t.Fatalf("second call should be a replay, got %s", *last.Content)
	}
}

func TestSameKeyDifferentAmountIsAConflict(t *testing.T) {
	r, tool := setup(t, `{"txn":"T1","amount":100,"note":"a"}`, `{"txn":"T1","amount":7300,"note":"a"}`)
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil {
		t.Fatal(err)
	}
	last := *st.Messages[len(st.Messages)-1].Content
	if tool.effects != 1 || !json.Valid([]byte(last)) || !strings.Contains(last, "conflict") {
		t.Fatalf("want conflict and no second effect: effects=%d result=%s", tool.effects, last)
	}
}

func TestCanonicalNormalises(t *testing.T) {
	a, _ := Canonical(map[string]any{"b": 1, "a": 4200.0})
	b, _ := Canonical(map[string]any{"a": 4200, "b": 1.0})
	if a != b {
		t.Fatalf("%s != %s", a, b)
	}
}
