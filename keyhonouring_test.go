package agentsafe

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// blindPayout is a payment through a system that can't deduplicate: it never sees the key. It counts what it
// actually did; unknownFirst makes the first call act and then report an unknown outcome (a timeout after the
// money moved).
type blindPayout struct {
	effects      int
	unknownFirst bool
}

func (b *blindPayout) Spec() ToolSpec {
	return ToolSpec{Name: "pay", Description: "pay", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (b *blindPayout) Call(context.Context, json.RawMessage) (any, error) {
	return nil, fmt.Errorf("use CallWithKey")
}
func (b *blindPayout) Identity(args json.RawMessage) (any, any, error) {
	var in struct{ Ref string }
	return in.Ref, string(args), json.Unmarshal(args, &in)
}
func (b *blindPayout) HonoursKey() bool { return false }
func (b *blindPayout) CallWithKey(context.Context, string, json.RawMessage) (any, error) {
	b.effects++
	if b.unknownFirst && b.effects == 1 {
		return nil, fmt.Errorf("%w: gateway timeout", ErrOutcomeUnknown)
	}
	return "paid", nil
}

func blindRun(t *testing.T, tool *blindPayout, hook func(string)) *Runner {
	t.Helper()
	plan := []FunctionCall{{Name: "pay", Arguments: `{"ref":"R1"}`}, {Name: "pay", Arguments: `{"ref":"R1"}`}} // asked twice
	r, err := New(&ScriptedModel{Plan: plan, Final: "done"}, &FileLog{Path: filepath.Join(t.TempDir(), "run.jsonl")},
		WithTools(tool), WithMaxSteps(6), WithToolRetries(3, 1), WithHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// An unknown outcome from a tool that can't deduplicate is recorded, not retried, and the same operation asked
// for again gets that answer from the log.
func TestAnUnknownOutcomeIsNeverRetriedWhenTheKeyCantBeHonoured(t *testing.T) {
	tool := &blindPayout{unknownFirst: true}
	st, err := blindRun(t, tool, nil).Start(context.Background(), "sys", "pay R1")
	if err != nil || st.Status != StatusFinished {
		t.Fatalf("err=%v status=%s", err, st.Status)
	}
	if tool.effects != 1 {
		t.Fatalf("one attempt, however often it's asked: %d", tool.effects)
	}
	results := toolResults(st)
	if len(results) != 2 || !strings.Contains(results[0], "outcome unknown") || !strings.Contains(results[1], "already_recorded") {
		t.Fatalf("the unknown outcome is recorded, then replayed for the repeat: %q", results)
	}
}

// A crash after the effect: the restarted run can't know whether it happened, and won't find out by trying.
func TestAStartedCallIsNotRerunWhenTheKeyCantBeHonoured(t *testing.T) {
	tool := &blindPayout{}
	r := blindRun(t, tool, func(p string) {
		if p == "after_tool_executed" {
			panic(crash{})
		}
	})
	func() {
		defer func() { _ = recover() }()
		_, _ = r.Start(context.Background(), "sys", "pay R1")
	}()
	r.Hook = nil
	st, err := r.Continue(context.Background())
	if err != nil || st.Status != StatusFinished || tool.effects != 1 {
		t.Fatalf("err=%v status=%s effects=%d: the crashed call must not run again", err, st.Status, tool.effects)
	}
	if results := toolResults(st); !strings.Contains(results[0], "outcome unknown") {
		t.Fatalf("recorded as unknown: %q", results)
	}
}

func toolResults(st State) []string {
	var out []string
	for _, m := range st.Messages {
		if m.Role == RoleTool && m.Content != nil {
			out = append(out, *m.Content)
		}
	}
	return out
}
