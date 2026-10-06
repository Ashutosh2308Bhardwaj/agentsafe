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

// Two different invoice numbers above 2^53 rounded to one float64, so they shared an idempotency key and the
// second payout would have been replayed instead of made. Found by FuzzCanonicalKeepsNumbersExact.
func TestLargeNumbersDontShareAKey(t *testing.T) {
	a, _ := Canonical(json.RawMessage(`{"invoice":12345678901234567}`))
	b, _ := Canonical(json.RawMessage(`{"invoice":12345678901234568}`))
	if a == b {
		t.Fatalf("two invoices, one key: %s", a)
	}
	if a != `{"invoice":12345678901234567}` {
		t.Fatalf("the exact number must be kept: %s", a)
	}
	// 1e-500 underflows to 0 without an error from ParseFloat; computed exactly, a model could make every key
	// cost a huge allocation (math/big itself only refuses exponents near 10^7).
	for _, n := range []string{"1e-500", "1e500", "2E+401", "1e309", "[1,1e500]"} { // 1e309: within the guard, beyond float64
		if _, err := Canonical(json.RawMessage(`{"x":` + n + `}`)); err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Fatalf("%s must be refused as out of range, got %v", n, err)
		}
	}
}

// Keys already in logs must not change: every number float64 holds exactly canonicalizes exactly as before.
func TestCanonicalUnchangedForExactNumbers(t *testing.T) {
	old := func(raw string) string {
		var g any
		_ = json.Unmarshal([]byte(raw), &g)
		b, _ := json.Marshal(g)
		return string(b)
	}
	for _, raw := range []string{`{"amount":4200.5,"n":1}`, `{"a":[1,2.0,-0,1e3,0.1,1e21,123456789012345]}`,
		`{"ref":"T1007","amount":11000}`, `{"x":9007199254740992}`, `[0.30000000000000004,1.5e-7,-2.25]`, `{"z":true,"y":null}`} {
		got, err := Canonical(json.RawMessage(raw))
		if err != nil || got != old(raw) {
			t.Errorf("Canonical(%s) = %s, was %s (err %v)", raw, got, old(raw), err)
		}
	}
}
