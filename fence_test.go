package agentsafe

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// A runner pauses (GC, VM migration) right after logging the model's decision and before acting on it. Its
// lease expires; a second runner takes the run over and finishes it. When the first wakes up, it must not
// act: its tool_started append is fenced (ErrConflict), so the tool never runs a second time.
func TestPausedRunnerThatLostTheRunCannotAct(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	export := &plain{name: "export", do: func(context.Context) (any, error) { return "done", nil }}
	model := &ScriptedModel{Plan: []FunctionCall{{Name: "export", Arguments: `{}`}}, Final: "ok"}

	// noLease hides FileLog's lock: as if the lease had expired, leaving fencing as the only protection.
	b := &Runner{Model: model, Tools: []Tool{export}, Log: noLease{&FileLog{Path: path}}, Unlocked: true}
	var takeover error
	a := &Runner{Model: model, Tools: []Tool{export}, Log: noLease{&FileLog{Path: path}}, Unlocked: true, MaxSteps: 4,
		Hook: func(p string) {
			if p == "after_model_logged" && takeover == nil {
				// A is "paused" here. B finds a run with a decided, unexecuted call and finishes it.
				_, takeover = b.Continue(context.Background())
				if takeover == nil {
					takeover = errors.New("done") // only once
				}
			}
		}}
	_, err := a.Start(context.Background(), "sys", "task")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("the stale runner must be fenced with ErrConflict, got %v (takeover: %v)", err, takeover)
	}
	if n := export.calls.Load(); n != 1 {
		t.Fatalf("the tool must run once (by the new owner), ran %d times", n)
	}
	events, err := (&FileLog{Path: path}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if st, err := Rebuild(events); err != nil || st.Status != StatusFinished {
		t.Fatalf("the run's history must stay single-writer and valid: %v %v", st.Status, err)
	}
}

type noLease struct{ l *FileLog }

func (n noLease) Append(e Event) error   { return n.l.Append(e) }
func (n noLease) Read() ([]Event, error) { return n.l.Read() }
