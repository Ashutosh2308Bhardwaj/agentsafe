package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/storetest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AGENTSAFE_POSTGRES_URL points at a database the tests may write to. Without it they skip, unless
// AGENTSAFE_REQUIRE_POSTGRES is set (CI): a backend whose tests silently skipped hasn't been tested.
func open(t *testing.T) *DB {
	t.Helper()
	url := os.Getenv("AGENTSAFE_POSTGRES_URL")
	if url == "" {
		if os.Getenv("AGENTSAFE_REQUIRE_POSTGRES") != "" {
			t.Fatal("AGENTSAFE_POSTGRES_URL is not set, and AGENTSAFE_REQUIRE_POSTGRES says these tests must run")
		}
		t.Skip("set AGENTSAFE_POSTGRES_URL to run the Postgres tests")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	d, err := New(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func runID(t *testing.T) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return t.Name() + "-" + hex.EncodeToString(b) // tests share the database; runs never collide
}

func TestConformance(t *testing.T) {
	open(t) // skip early, before the suite's subtests
	storetest.Run(t, storetest.Backend{
		NewRun: func(t *testing.T) agentsafe.LineStore { return open(t).Run(runID(t)) },
		Reopen: func(t *testing.T, s agentsafe.LineStore) agentsafe.LineStore {
			return open(t).Run(s.(*Run).id) // a separate pool: another machine
		},
	})
}

func TestManySimultaneousMigrations(t *testing.T) {
	admin := open(t)
	// A fresh, empty schema, so the ten workers really race to create the tables.
	schemaName := "agentsafe_mig_" + hex.EncodeToString([]byte(runID(t)[len(t.Name())+1:]))
	ctx := context.Background()
	if _, err := admin.pool.Exec(ctx, "CREATE SCHEMA "+schemaName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.pool.Exec(ctx, "DROP SCHEMA "+schemaName+" CASCADE") })
	url := os.Getenv("AGENTSAFE_POSTGRES_URL") + "&search_path=" + schemaName
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool, err := pgxpool.New(context.Background(), url)
			if err != nil {
				errs <- err
				return
			}
			defer pool.Close()
			_, err = New(context.Background(), pool)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ten workers starting at once must all succeed: %v", err)
		}
	}
}

// Under READ COMMITTED, writers racing for DIFFERENT line numbers must still leave no gap and no duplicate.
func TestRacingForDifferentLinesLeavesNoGaps(t *testing.T) {
	id := runID(t)
	handles := make([]*Run, 16)
	for i := range handles {
		handles[i] = open(t).Run(id)
	}
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		lines, err := handles[0].ReadLines(ctx)
		if err != nil {
			t.Fatal(err)
		}
		base := len(lines)
		var wg sync.WaitGroup
		for i, h := range handles {
			wg.Add(1)
			go func() {
				defer wg.Done()
				seq := base + 1 + i%4 // four writers each for lines base+1 .. base+4
				err := h.AppendLine(ctx, seq, []byte(fmt.Sprintf(`{"w":%d,"seq":%d}`, i, seq)))
				if err != nil && !errors.Is(err, agentsafe.ErrConflict) {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
	}
	lines, err := handles[0].ReadLines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range lines {
		var v struct{ Seq int }
		if err := json.Unmarshal(l, &v); err != nil || v.Seq != i+1 {
			t.Fatalf("line %d holds seq %d: a gap or a duplicate", i+1, v.Seq)
		}
	}
	if len(lines) < 20 {
		t.Fatalf("every round must append at least one line, got %d lines", len(lines))
	}
}

// expire makes the run's lease run out now (on the database clock): "time passes" as an explicit step, not a
// sleep that a slow CI machine can overshoot.
func expire(t *testing.T, d *DB, run string) {
	t.Helper()
	if _, err := d.pool.Exec(context.Background(),
		`UPDATE agentsafe_leases SET expires_at = now() - interval '1 millisecond' WHERE run_id = $1`, run); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseExpiresWhenItsHolderDies(t *testing.T) {
	a, b := open(t), open(t)
	id := runID(t)
	l, err := a.Run(id).acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	l.stopRenewing() // kill -9: no renewal, no release
	if _, err := b.Run(id).Lock(context.Background()); !errors.Is(err, agentsafe.ErrRunLocked) {
		t.Fatalf("right after the crash the lease is still held, got %v", err)
	}
	expire(t, a, id) // the TTL passes with no renewal
	unlock, err := b.Run(id).Lock(context.Background())
	if err != nil {
		t.Fatalf("after the TTL a dead holder's lease must be takeable: %v", err)
	}
	_ = unlock()
}

func TestRenewalKeepsALiveHolderSLease(t *testing.T) {
	a, b := open(t), open(t)
	a.LeaseTTL = time.Second // renewed every 333ms; generous margins for slow CI machines
	id := runID(t)
	unlock, err := a.Run(id).Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3500 * time.Millisecond) // > 3 TTLs: only renewal keeps it
	if _, err := b.Run(id).Lock(context.Background()); !errors.Is(err, agentsafe.ErrRunLocked) {
		t.Fatalf("a live, renewing holder must keep its lease, got %v", err)
	}
	_ = unlock()
	u, err := b.Run(id).Lock(context.Background())
	if err != nil {
		t.Fatalf("released: %v", err)
	}
	_ = u()
}

func TestHolderThatLostItsLeaseIsFenced(t *testing.T) {
	a, b := open(t), open(t)
	id := runID(t)
	ja := &agentsafe.Journal{Store: a.Run(id)}
	if err := ja.Append(context.Background(), agentsafe.Event{Type: agentsafe.EvRunStarted, Task: "t", MaxSteps: 1}); err != nil {
		t.Fatal(err)
	}
	la, err := a.Run(id).acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	la.stopRenewing() // A "pauses"
	expire(t, a, id)  // ... for longer than the TTL
	unlockB, err := b.Run(id).Lock(context.Background())
	if err != nil {
		t.Fatalf("B must get the expired lease: %v", err)
	}
	defer func() { _ = unlockB() }()
	if err := (&agentsafe.Journal{Store: b.Run(id)}).Append(context.Background(), agentsafe.Event{Type: agentsafe.EvRunPaused, Reason: "budget_exhausted"}); err != nil {
		t.Fatal(err)
	}
	if err := ja.Append(context.Background(), agentsafe.Event{Type: agentsafe.EvRunPaused, Reason: "stale"}); !errors.Is(err, agentsafe.ErrConflict) {
		t.Fatalf("A must be fenced, got %v", err)
	}
	if err := la.release(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(id).Lock(context.Background()); !errors.Is(err, agentsafe.ErrRunLocked) {
		t.Fatalf("A's late release must not free B's lease, got %v", err)
	}
}
