package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/storetest"
)

func open(t *testing.T, path string) *DB {
	t.Helper()
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestConformance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")
	n := 0
	storetest.Run(t, storetest.Backend{
		NewRun: func(t *testing.T) agentsafe.LineStore {
			n++
			return open(t, path).Run(fmt.Sprintf("run-%d", n)) // many runs, one database
		},
		Reopen: func(t *testing.T, s agentsafe.LineStore) agentsafe.LineStore {
			return open(t, path).Run(s.(*Run).id) // a separate connection pool: another process
		},
	})
}

func TestLeaseExpiresWhenItsHolderDies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")
	a, b := open(t, path), open(t, path)
	a.LeaseTTL, b.LeaseTTL = 150*time.Millisecond, 150*time.Millisecond
	l, err := a.Run("r").acquire()
	if err != nil {
		t.Fatal(err)
	}
	l.stopRenewing() // the holder is kill -9'd: no renewal, no release
	if _, err := b.Run("r").Lock(); !errors.Is(err, agentsafe.ErrRunLocked) {
		t.Fatalf("right after the crash the lease is still held, got %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	unlock, err := b.Run("r").Lock()
	if err != nil {
		t.Fatalf("after the TTL a dead holder's lease must be takeable: %v", err)
	}
	_ = unlock()
}

func TestRenewalKeepsALiveHolderSLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")
	a, b := open(t, path), open(t, path)
	a.LeaseTTL, b.LeaseTTL = 150*time.Millisecond, 150*time.Millisecond
	unlock, err := a.Run("r").Lock()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // > 3 TTLs: only renewal keeps it
	if _, err := b.Run("r").Lock(); !errors.Is(err, agentsafe.ErrRunLocked) {
		t.Fatalf("a live, renewing holder must keep its lease, got %v", err)
	}
	_ = unlock()
	if u, err := b.Run("r").Lock(); err != nil {
		t.Fatalf("released: %v", err)
	} else {
		_ = u()
	}
}

// The case leases can't handle alone: A's lease expires while A is paused, B takes the run and writes, A
// wakes up. A's renewal must not steal the lease back, and A's next write must be fenced.
func TestHolderThatLostItsLeaseIsFenced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")
	a, b := open(t, path), open(t, path)
	a.LeaseTTL, b.LeaseTTL = 150*time.Millisecond, 150*time.Millisecond
	ja := &agentsafe.Journal{Store: a.Run("r")}
	if err := ja.Append(agentsafe.Event{Type: agentsafe.EvRunStarted, Task: "t", MaxSteps: 1}); err != nil {
		t.Fatal(err)
	}
	la, err := a.Run("r").acquire()
	if err != nil {
		t.Fatal(err)
	}
	la.stopRenewing() // A "pauses"
	time.Sleep(250 * time.Millisecond)
	unlockB, err := b.Run("r").Lock()
	if err != nil {
		t.Fatalf("B must get the expired lease: %v", err)
	}
	defer func() { _ = unlockB() }()
	jb := &agentsafe.Journal{Store: b.Run("r")}
	if err := jb.Append(agentsafe.Event{Type: agentsafe.EvRunPaused, Reason: "budget_exhausted"}); err != nil {
		t.Fatal(err)
	}
	// A wakes up and writes what it was about to write.
	if err := ja.Append(agentsafe.Event{Type: agentsafe.EvRunPaused, Reason: "stale"}); !errors.Is(err, agentsafe.ErrConflict) {
		t.Fatalf("A must be fenced, got %v", err)
	}
	// A's late "release" must not free B's lease.
	if err := la.release(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run("r").Lock(); !errors.Is(err, agentsafe.ErrRunLocked) {
		t.Fatalf("B still holds the run, got %v", err)
	}
}

// A whole run on SQLite: a crash after the tool ran, resumed by a new runner on a new connection.
type counter struct{ n int }

func (c *counter) Spec() agentsafe.ToolSpec {
	return agentsafe.ToolSpec{Name: "count", Description: "c", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (c *counter) Call(context.Context, json.RawMessage) (any, error) { c.n++; return c.n, nil }

type crash struct{}

func TestRunCrashesAndResumesOnSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")
	tool := &counter{}
	model := &agentsafe.ScriptedModel{Plan: []agentsafe.FunctionCall{{Name: "count", Arguments: `{}`}}, Final: "done"}
	r := &agentsafe.Runner{Model: model, Tools: []agentsafe.Tool{tool}, MaxSteps: 4,
		Log: &agentsafe.Journal{Store: open(t, path).Run("job-1"), Codec: agentsafe.AESGCM{
			Keys: map[string][]byte{"k": make([]byte, 32)}, Current: "k"}}}
	r.Hook = func(p string) {
		if p == "after_result_logged" {
			panic(crash{})
		}
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = r.Start(context.Background(), "sys", "task")
	}()
	// A panic unwinds, so Start's deferred unlock released the lease; a kill -9 wouldn't, and the next runner
	// would wait out the TTL (TestLeaseExpiresWhenItsHolderDies). A new runner, on a new connection:
	r2 := &agentsafe.Runner{Model: model, Tools: []agentsafe.Tool{tool},
		Log: &agentsafe.Journal{Store: open(t, path).Run("job-1"), Codec: r.Log.(*agentsafe.Journal).Codec}}
	st, err := r2.Continue(context.Background())
	if err != nil || st.Status != agentsafe.StatusFinished || tool.n != 1 {
		t.Fatalf("resume on SQLite: err=%v status=%s tool ran %d times", err, st.Status, tool.n)
	}
}
