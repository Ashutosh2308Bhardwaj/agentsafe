package agentsafe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSecondRunnerIsLockedOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	r1, _, _ := newRun(t)
	r1.Log = &FileLog{Path: path}
	r1.MaxSteps = 1
	if _, err := r1.Start(context.Background(), "sys", "task"); err != nil { // pauses after 1 step
		t.Fatal(err)
	}

	holder := &FileLog{Path: path}
	unlock, err := holder.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r2 := &Runner{Model: r1.Model, Tools: r1.Tools, Log: &FileLog{Path: path}}
	if _, err := r2.Extend(context.Background(), 5, "test"); !errors.Is(err, ErrRunLocked) {
		t.Fatalf("while another runner holds the run, want ErrRunLocked, got %v", err)
	}
	events, _ := holder.Read(context.Background())
	before := len(events)
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	if after, _ := holder.Read(context.Background()); len(after) != before {
		t.Fatal("a locked-out runner must not have written anything")
	}
	if st, err := r2.Extend(context.Background(), 5, "test"); err != nil || st.Status != StatusFinished {
		t.Fatalf("after the holder releases, the run continues: %v %s", err, st.Status)
	}
}

// slowModel stretches every decision so concurrent runners genuinely overlap.
type slowModel struct{ inner Model }

func (s slowModel) Decide(ctx context.Context, m []Message, t []ToolSpec) (Decision, error) {
	time.Sleep(20 * time.Millisecond)
	return s.inner.Decide(ctx, m, t)
}

func TestConcurrentRunnersOnlyOneDrives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	tool := &countTool{name: "work"}
	plan := []FunctionCall{{Name: "work", Arguments: "{}"}, {Name: "work", Arguments: "{}"}, {Name: "work", Arguments: "{}"}}
	starter := &Runner{Model: &ScriptedModel{Plan: plan, Final: "done"}, Tools: []Tool{tool},
		Log: &FileLog{Path: path}, MaxSteps: 1}
	if _, err := starter.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &Runner{Model: slowModel{&ScriptedModel{Plan: plan, Final: "done"}}, Tools: []Tool{tool},
				Log: &FileLog{Path: path}}
			_, results[i] = r.Extend(context.Background(), 10, fmt.Sprint("worker-", i))
		}()
	}
	wg.Wait()

	won, locked := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrRunLocked):
			locked++
		default:
			// a worker that got the lock AFTER the winner finished finds a finished run: not paused
		}
	}
	if won < 1 || locked < 1 {
		t.Fatalf("want one driver and the rest locked out, got won=%d locked=%d (%v)", won, locked, results)
	}
	events, err := (&FileLog{Path: path}).Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Rebuild(events); err != nil {
		t.Fatalf("the log must stay a single valid history: %v", err)
	}
	if tool.calls != len(plan) {
		t.Fatalf("each planned call must run exactly once, got %d", tool.calls)
	}
}

// TestLockReleasedWhenHolderIsKilled: a process takes the lock and is killed without any cleanup. The OS
// must release the lock, or a crashed runner would hold its run forever.
func TestLockReleasedWhenHolderIsKilled(t *testing.T) {
	if os.Getenv("AGENTSAFE_LOCK_HELPER") != "" {
		return // we are the helper; see TestHelperHoldLock
	}
	path := filepath.Join(t.TempDir(), "run.jsonl")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldLock$")
	cmd.Env = append(os.Environ(), "AGENTSAFE_LOCK_HELPER="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("helper didn't take the lock: %q %v", line, err)
	}
	if _, err := (&FileLog{Path: path}).Lock(context.Background()); !errors.Is(err, ErrRunLocked) {
		t.Fatalf("while the helper is alive the run must be locked, got %v", err)
	}
	if err := cmd.Process.Kill(); err != nil { // SIGKILL on Unix, TerminateProcess on Windows: no cleanup runs
		t.Fatal(err)
	}
	_ = cmd.Wait()
	unlock, err := (&FileLog{Path: path}).Lock(context.Background())
	if err != nil {
		t.Fatalf("after the holder was killed the lock must be free, got %v", err)
	}
	_ = unlock()
}

// TestHelperHoldLock is not a real test: it is the child process for TestLockReleasedWhenHolderIsKilled.
func TestHelperHoldLock(_ *testing.T) {
	path := os.Getenv("AGENTSAFE_LOCK_HELPER")
	if path == "" {
		return
	}
	if _, err := (&FileLog{Path: path}).Lock(context.Background()); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	fmt.Println("locked")
	time.Sleep(time.Minute) // killed long before this
}

// memLog is a Log without a Locker, like a naive custom implementation.
type memLog struct{ events []Event }

func (m *memLog) Append(_ context.Context, e Event) error {
	e.Seq = len(m.events) + 1
	m.events = append(m.events, e)
	return nil
}
func (m *memLog) Read(context.Context) ([]Event, error) { return m.events, nil }

func TestRunnerRequiresALockerUnlessOptedOut(t *testing.T) {
	model := &ScriptedModel{Final: "done"}
	r := &Runner{Model: model, Log: &memLog{}}
	if _, err := r.Start(context.Background(), "sys", "task"); !errors.Is(err, ErrNoLocker) {
		t.Fatalf("a log without a lock must be refused by default, got %v", err)
	}
	r.Unlocked = true
	if st, err := r.Start(context.Background(), "sys", "task"); err != nil || st.Status != StatusFinished {
		t.Fatalf("with an explicit opt-out it runs: %v %s", err, st.Status)
	}
}

func TestSequenceStaysContiguousAcrossProcesses(t *testing.T) {
	// Two FileLogs take turns on one run, like two processes. Each re-reads the sequence when it takes the
	// lock, so no event number is reused or skipped.
	path := filepath.Join(t.TempDir(), "run.jsonl")
	a, b := &FileLog{Path: path}, &FileLog{Path: path}
	for i, l := range []*FileLog{a, b, a, b, a} {
		unlock, err := l.Lock(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Append(context.Background(), Event{Type: EvRunStarted, Task: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
		_ = unlock()
	}
	events, _ := a.Read(context.Background())
	for i, e := range events {
		if e.Seq != i+1 {
			b, _ := json.Marshal(events)
			t.Fatalf("event %d has seq %d; sequence must be contiguous: %s", i, e.Seq, b)
		}
	}
}
