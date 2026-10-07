package agentsafe

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// failingLog is a FileLog whose failAt-th Append fails (disk full, a stale-writer conflict) and writes nothing.
type failingLog struct {
	*FileLog
	failAt, n int
	fired     bool
}

var errDiskFull = errors.New("disk full")

func (l *failingLog) Append(ctx context.Context, e Event) error {
	l.n++
	if l.n == l.failAt {
		l.fired = true
		return errDiskFull
	}
	return l.FileLog.Append(ctx, e)
}

// A run that touches every place the runner writes: a gated payout (approval requested, decided), a plain
// tool, the same payout proposed again (replayed from the log), a budget pause and its extension, the final
// answer. For every N, the N-th write fails. Whatever N is:
//   - a tool runs only if its tool_started was written (the tool checks the log itself, as it runs);
//   - the failure surfaces as an error, never as a run that carries on;
//   - a new process resumes from what was written and the run finishes, with the payout made once.
func TestEveryLogWriteFailureStopsTheRunBeforeItActs(t *testing.T) {
	total := writesInARun(t, 0)
	if total < 15 {
		t.Fatalf("setup: the run should write at least 15 events, wrote %d", total)
	}
	for n := 1; n <= total; n++ {
		t.Run(fmt.Sprint("write-", n), func(t *testing.T) { writesInARun(t, n) })
	}
}

func writesInARun(t *testing.T, failAt int) int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.jsonl")
	var mu sync.Mutex
	paid := map[string]int{}
	actedWithoutRecord := ""
	recorded := func(ctx context.Context, tool string) {
		if !startRecorded(ctx, path, tool) {
			actedWithoutRecord = tool
		}
	}
	type ref struct {
		Ref string `json:"ref"`
	}
	pay := Func("pay", "pay", func(ctx context.Context, in ref) (string, error) {
		recorded(ctx, "pay")
		mu.Lock()
		defer mu.Unlock()
		paid[KeyFrom(ctx)] = 1 // the gateway dedupes by key: >1 distinct keys for one ref would be the bug
		return "paid " + in.Ref, nil
	}, Idempotent("ref"), NeedsApproval(func(in ref) any { return in }))
	export := Func("export", "export", func(ctx context.Context, _ struct{}) (string, error) {
		recorded(ctx, "export")
		return "exported", nil
	})
	model := &ScriptedModel{Final: "done", Plan: []FunctionCall{
		{Name: "pay", Arguments: `{"ref":"R1"}`}, {Name: "export", Arguments: `{}`}, {Name: "pay", Arguments: `{"ref":"R1"}`}}}
	flaky := &failingLog{FileLog: &FileLog{Path: path}, failAt: failAt}

	runner := func(log Log) *Runner {
		r, err := New(model, log, WithTools(pay, export), WithMaxSteps(2), WithAnyApprover())
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	ctx := context.Background()
	r := runner(flaky)
	var st State
	for i := 0; ; i++ {
		if i > 30 {
			t.Fatal("the run never finished")
		}
		var err error
		st, err = advance(ctx, t, r)
		if err != nil {
			if !errors.Is(err, errDiskFull) {
				t.Fatalf("write %d: unexpected error %v", failAt, err)
			}
			r = runner(&FileLog{Path: path}) // the failure surfaced: a new process takes over, with a healthy disk
			continue
		}
		if st.Status == StatusFinished {
			break
		}
	}
	if failAt > 0 && !flaky.fired {
		t.Fatalf("write %d was never reached", failAt)
	}
	if actedWithoutRecord != "" {
		t.Fatalf("write %d failed and %s ran anyway, with no tool_started on record", failAt, actedWithoutRecord)
	}
	if len(paid) != 1 || st.Text != "done" {
		t.Fatalf("write %d: want one payout key and a finished run, got %d keys, %q", failAt, len(paid), st.Text)
	}
	return flaky.n
}

// startRecorded reports whether the log's last event is tool_started for tool: write-ahead, checked from
// inside the tool as it acts.
func startRecorded(ctx context.Context, path, tool string) bool {
	events, err := (&FileLog{Path: path}).Read(ctx)
	if err != nil || len(events) == 0 {
		return false
	}
	last := events[len(events)-1]
	return last.Type == EvToolStarted && last.Tool == tool
}

// advance does what a process would do next with the run: start, approve, extend or continue it.
func advance(ctx context.Context, t *testing.T, r *Runner) (State, error) {
	events, err := r.Log.Read(ctx)
	if err != nil {
		return State{}, err
	}
	st, err := Rebuild(events)
	if err != nil {
		t.Fatalf("the log must always rebuild, even after a failed write: %v", err)
	}
	switch st.Status {
	case StatusNew:
		return r.Start(ctx, "sys", "task")
	case StatusAwaitingApproval:
		return r.Approve(ctx, st.Waiting.Key, "ops")
	case StatusPaused:
		return r.Extend(ctx, 2, "ops")
	case StatusFinished:
		return st, nil
	default:
		return r.Continue(ctx)
	}
}

// The other ways a run can be refused before anything happens.
func TestRunnerRefusalsBeforeActing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "r.jsonl")
	if err := writeFile(path, "{\"seq\":1,\"type\":\"run_started\"\n{\"seq\":2}\n"); err != nil { // damaged
		t.Fatal(err)
	}
	r, _ := New(&ScriptedModel{Final: "x"}, &FileLog{Path: path})
	for name, call := range map[string]func() (State, error){
		"Start":    func() (State, error) { return r.Start(ctx, "s", "t") },
		"Continue": func() (State, error) { return r.Continue(ctx) },
		"Extend":   func() (State, error) { return r.Extend(ctx, 1, "ops") },
		"Approve":  func() (State, error) { return r.Approve(ctx, "k", "ops") },
	} {
		if _, err := call(); !errors.Is(err, ErrCorruptLog) {
			t.Errorf("%s on a damaged log must refuse with ErrCorruptLog, got %v", name, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	ok, _ := New(&ScriptedModel{Final: "x"}, &FileLog{Path: filepath.Join(t.TempDir(), "c.jsonl")})
	if _, err := ok.Start(cancelled, "s", "t"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context must stop the run, got %v", err)
	}
	locked := &FileLog{Path: filepath.Join(t.TempDir(), "l.jsonl")}
	unlock, err := locked.Lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()
	lr, _ := New(&ScriptedModel{Final: "x"}, &FileLog{Path: locked.Path})
	for name, call := range map[string]func() (State, error){
		"Start": func() (State, error) { return lr.Start(ctx, "s", "t") }, "Continue": func() (State, error) { return lr.Continue(ctx) },
		"Extend": func() (State, error) { return lr.Extend(ctx, 1, "ops") }, "Reject": func() (State, error) { return lr.Reject(ctx, "k", "ops", "no") },
	} {
		if _, err := call(); !errors.Is(err, ErrRunLocked) {
			t.Errorf("%s while another runner holds the run must refuse with ErrRunLocked, got %v", name, err)
		}
	}
	if !strings.Contains(fmt.Sprint(ErrRunLocked), "locked") {
		t.Fatal("sentinel text")
	}
}
