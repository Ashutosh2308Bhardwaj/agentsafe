// Package storetest checks a storage backend against agentsafe's LineStore contract. Every backend runs it;
// if you write your own (DynamoDB, Spanner, ...), run it too:
//
//	func TestConformance(t *testing.T) {
//		storetest.Run(t, storetest.Backend{
//			NewRun: func(t *testing.T) agentsafe.LineStore { return myStore(t, newRunID()) },
//			Reopen: func(t *testing.T, s agentsafe.LineStore) agentsafe.LineStore { return myStore(t, s.(*mine).run) },
//		})
//	}
package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

// Backend opens stores for the suite.
type Backend struct {
	// NewRun returns a store for a new, empty run.
	NewRun func(t *testing.T) agentsafe.LineStore
	// Reopen returns a second, independent handle to the same run, as another process would open it: no
	// state shared with s except the storage itself.
	Reopen func(t *testing.T, s agentsafe.LineStore) agentsafe.LineStore
}

// Run runs every check.
func Run(t *testing.T, b Backend) {
	t.Run("EmptyRunReadsNothing", func(t *testing.T) { emptyRun(t, b) })
	t.Run("LinesReadBackExactlyInOrder", func(t *testing.T) { roundTrip(t, b) })
	t.Run("WrongSequenceIsAConflictAndWritesNothing", func(t *testing.T) { wrongSeq(t, b) })
	t.Run("StaleWriterIsFenced", func(t *testing.T) { staleWriter(t, b) })
	t.Run("ConcurrentWritersExactlyOneWinsEachLine", func(t *testing.T) { concurrent(t, b) })
	t.Run("RunsAreIsolated", func(t *testing.T) { isolated(t, b) })
	t.Run("LeaseIsExclusive", func(t *testing.T) { lease(t, b) })
	t.Run("CancelledContextStoresNothing", func(t *testing.T) { cancelled(t, b) })
	t.Run("JournalOnTop", func(t *testing.T) { journal(t, b) })
}

var ctx = context.Background()

func line(s string) []byte { return []byte(fmt.Sprintf(`{"v":%q}`, s)) }

func mustRead(t *testing.T, s agentsafe.LineStore) [][]byte {
	t.Helper()
	lines, err := s.ReadLines(ctx)
	if err != nil {
		t.Fatalf("ReadLines: %v", err)
	}
	return lines
}

func mustAppend(t *testing.T, s agentsafe.LineStore, seq int, l []byte) {
	t.Helper()
	if err := s.AppendLine(ctx, seq, l); err != nil {
		t.Fatalf("AppendLine(%d): %v", seq, err)
	}
}

func emptyRun(t *testing.T, b Backend) {
	if lines := mustRead(t, b.NewRun(t)); len(lines) != 0 {
		t.Fatalf("a new run must be empty, got %d lines", len(lines))
	}
}

func roundTrip(t *testing.T, b Backend) {
	s := b.NewRun(t)
	want := [][]byte{line("first"), line("ünïcødé ₹11,000  "), line(`quotes " and \ backslashes`),
		line(strings.Repeat("x", 1<<20)), []byte(`{"n":1}`)}
	for i, l := range want {
		mustAppend(t, s, i+1, l)
	}
	for _, h := range []agentsafe.LineStore{s, b.Reopen(t, s)} { // and from another "process"
		got := mustRead(t, h)
		if len(got) != len(want) {
			t.Fatalf("want %d lines, got %d", len(want), len(got))
		}
		for i := range want {
			if !bytes.Equal(got[i], want[i]) {
				t.Fatalf("line %d must read back byte for byte (the hash chain covers these bytes)", i+1)
			}
		}
	}
}

func wrongSeq(t *testing.T, b Backend) {
	s := b.NewRun(t)
	for _, seq := range []int{0, 2, -1} {
		if err := s.AppendLine(ctx, seq, line("x")); !errors.Is(err, agentsafe.ErrConflict) {
			t.Fatalf("seq %d on an empty run: want ErrConflict, got %v", seq, err)
		}
	}
	mustAppend(t, s, 1, line("a"))
	for _, seq := range []int{1, 3} {
		if err := s.AppendLine(ctx, seq, line("dup")); !errors.Is(err, agentsafe.ErrConflict) {
			t.Fatalf("seq %d after 1 line: want ErrConflict, got %v", seq, err)
		}
	}
	if got := mustRead(t, s); len(got) != 1 || !bytes.Equal(got[0], line("a")) {
		t.Fatalf("a refused append must store nothing, got %d lines", len(got))
	}
}

// A runner whose lease expired while it was paused wakes up and writes: it must be refused.
func staleWriter(t *testing.T, b Backend) {
	a := b.NewRun(t)
	mustAppend(t, a, 1, line("a1"))
	other := b.Reopen(t, a)
	mustRead(t, other)
	mustAppend(t, other, 2, line("b2")) // the new owner continues the run
	if err := a.AppendLine(ctx, 2, line("a2")); !errors.Is(err, agentsafe.ErrConflict) {
		t.Fatalf("the stale writer must get ErrConflict, got %v", err)
	}
	got := mustRead(t, b.Reopen(t, a))
	if len(got) != 2 || !bytes.Equal(got[1], line("b2")) {
		t.Fatal("line 2 must be the new owner's")
	}
	mustAppend(t, a, 3, line("a3")) // once it knows the real length, the old handle may append again
}

func concurrent(t *testing.T, b Backend) {
	first := b.NewRun(t)
	const writers, rounds = 12, 5
	handles := make([]agentsafe.LineStore, writers)
	for i := range handles {
		handles[i] = b.Reopen(t, first)
	}
	for seq := 1; seq <= rounds; seq++ {
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins, other := 0, []error{}
		for w, h := range handles {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := h.AppendLine(ctx, seq, line(fmt.Sprintf("w%d-s%d", w, seq)))
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					wins++
				case !errors.Is(err, agentsafe.ErrConflict):
					other = append(other, err)
				}
			}()
		}
		wg.Wait()
		if wins != 1 || len(other) > 0 {
			t.Fatalf("line %d: %d writers won (want exactly 1); unexpected errors: %v", seq, wins, other)
		}
	}
	if got := mustRead(t, b.Reopen(t, first)); len(got) != rounds {
		t.Fatalf("want %d lines, got %d", rounds, len(got))
	}
}

func isolated(t *testing.T, b Backend) {
	r1, r2 := b.NewRun(t), b.NewRun(t)
	mustAppend(t, r1, 1, line("run1"))
	mustAppend(t, r2, 1, line("run2")) // seq 1 again: a different run
	if got := mustRead(t, r1); len(got) != 1 || !bytes.Equal(got[0], line("run1")) {
		t.Fatal("runs must not see each other's lines")
	}
}

func lease(t *testing.T, b Backend) {
	a := b.NewRun(t)
	la, ok := a.(agentsafe.Locker)
	if !ok {
		t.Skip("backend has no lease (Locker); runners need Runner.Unlocked, and rely on ErrConflict alone")
	}
	lb := b.Reopen(t, a).(agentsafe.Locker)
	unlock, err := la.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lb.Lock(context.Background()); !errors.Is(err, agentsafe.ErrRunLocked) {
		t.Fatalf("a held lease must refuse a second holder with ErrRunLocked, got %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	unlockB, err := lb.Lock(context.Background())
	if err != nil {
		t.Fatalf("a released lease must be available: %v", err)
	}
	if err := unlockB(); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.NewRun(t).(agentsafe.Locker); ok { // leases are per run
		other, _ := b.NewRun(t).(agentsafe.Locker)
		u1, err1 := la.Lock(context.Background())
		u2, err2 := other.Lock(context.Background())
		if err1 != nil || err2 != nil {
			t.Fatalf("leases on different runs must not block each other: %v %v", err1, err2)
		}
		_ = u1()
		_ = u2()
	}
}

// A caller that gave up (cancelled, timed out) must not have its line stored afterwards.
func cancelled(t *testing.T, b Backend) {
	s := b.NewRun(t)
	c, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.AppendLine(c, 1, line("late")); err == nil {
		t.Fatal("AppendLine with a cancelled context must fail")
	}
	if got := mustRead(t, s); len(got) != 0 {
		t.Fatalf("a cancelled append must store nothing, got %d lines", len(got))
	}
}

// The full stack on this backend: chained events, a stale Journal fenced, a verified read.
func journal(t *testing.T, b Backend) {
	s := b.NewRun(t)
	j1 := &agentsafe.Journal{Store: s, Key: []byte("k")}
	if err := j1.Append(context.Background(), agentsafe.Event{Type: agentsafe.EvRunStarted, Task: "t", MaxSteps: 1}); err != nil {
		t.Fatal(err)
	}
	j2 := &agentsafe.Journal{Store: b.Reopen(t, s), Key: []byte("k")}
	if err := j2.Append(context.Background(), agentsafe.Event{Type: agentsafe.EvRunPaused, Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := j1.Append(context.Background(), agentsafe.Event{Type: agentsafe.EvRunPaused, Reason: "stale"}); !errors.Is(err, agentsafe.ErrConflict) {
		t.Fatalf("a stale Journal must be fenced, got %v", err)
	}
	// Fenced once, it re-reads: its next append lands after j2's, chained onto it.
	if err := j1.Append(context.Background(), agentsafe.Event{Type: agentsafe.EvBudgetExtended, ExtraSteps: 1, By: "ops"}); err != nil {
		t.Fatalf("after a conflict the Journal must re-read and append at the real end: %v", err)
	}
	events, err := (&agentsafe.Journal{Store: b.Reopen(t, s), Key: []byte("k")}).Read(context.Background())
	if err != nil || len(events) != 3 || events[1].Reason != "x" || events[2].Seq != 3 {
		t.Fatalf("want 3 verified events, the second from j2: %v %v", events, err)
	}
	if _, err := (&agentsafe.Journal{Store: b.Reopen(t, s), Key: []byte("wrong")}).Read(context.Background()); !errors.Is(err, agentsafe.ErrTampered) {
		t.Fatalf("the chain must verify on this backend (wrong key = tampered), got %v", err)
	}
}
