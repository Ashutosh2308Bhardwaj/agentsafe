// Package postgres stores agentsafe runs in PostgreSQL (pgx), for runs shared between machines.
//
//	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
//	db, err := postgres.New(ctx, pool)
//	r := &agentsafe.Runner{Log: &agentsafe.Journal{Store: db.Run("payout-2026-10-06")}, ...}
//
// Each run's lines are rows keyed by (run_id, seq). Appending line N inserts it only if the run's last line is
// N-1: the conditional append that fences stale writers (agentsafe.ErrConflict). Each run has a lease row
// (agentsafe.Locker), renewed in the background and expiring on its own if the holder dies. Every lease
// time is the DATABASE's clock (now()), never a machine's: with runners on many machines, clock skew would
// otherwise let two of them each believe the other's lease had expired.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/internal/heartbeat"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS agentsafe_lines (
	run_id text   NOT NULL,
	seq    bigint NOT NULL,
	line   bytea  NOT NULL,
	PRIMARY KEY (run_id, seq)
);
CREATE TABLE IF NOT EXISTS agentsafe_leases (
	run_id     text        PRIMARY KEY,
	holder     text        NOT NULL,
	expires_at timestamptz NOT NULL
);`

// migrationLock serialises table creation: CREATE TABLE IF NOT EXISTS isn't safe against itself when many
// workers start at once (both pass the check, one fails on the catalog's unique index).
const migrationLock = 0x6167656e74736166 // "agentsaf"

// DefaultLeaseTTL is how long a lease outlives its holder's last renewal.
const DefaultLeaseTTL = 30 * time.Second

// DB is a PostgreSQL database holding many runs.
type DB struct {
	pool *pgxpool.Pool
	// LeaseTTL is how long a lease survives without renewal, i.e. how long a crashed runner blocks its run.
	// Renewal happens every LeaseTTL/3. 0 = DefaultLeaseTTL.
	LeaseTTL time.Duration
}

// New uses pool, creating the tables if needed.
func New(ctx context.Context, pool *pgxpool.Pool) (*DB, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrationLock)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("agentsafe/postgres: creating tables: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &DB{pool: pool}, nil
}

// Run returns the store for one run. It implements agentsafe.LineStore and agentsafe.Locker.
func (d *DB) Run(id string) *Run { return &Run{d: d, id: id} }

// Run is one run's lines and lease.
type Run struct {
	d  *DB
	id string
}

// ReadLines implements agentsafe.LineStore.
func (r *Run) ReadLines(ctx context.Context) ([][]byte, error) {
	rows, err := r.d.pool.Query(ctx, `SELECT line FROM agentsafe_lines WHERE run_id = $1 ORDER BY seq`, r.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var l []byte
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// AppendLine implements agentsafe.LineStore: line seq is stored only if the run has exactly seq-1 lines.
//
// Under READ COMMITTED two writers can both see "last line is N-1". The primary key decides: the second
// insert of (run, N) waits for the first to commit, then ON CONFLICT DO NOTHING stores nothing. The max check
// rules out gaps; the key rules out duplicates. No lock, no SERIALIZABLE retry loop.
func (r *Run) AppendLine(ctx context.Context, seq int, line []byte) error {
	if seq < 1 {
		return fmt.Errorf("%w: line %d", agentsafe.ErrConflict, seq)
	}
	tag, err := r.d.pool.Exec(ctx, `
		INSERT INTO agentsafe_lines (run_id, seq, line)
		SELECT $1::text, $2::bigint, $3::bytea
		WHERE (SELECT COALESCE(MAX(seq), 0) FROM agentsafe_lines WHERE run_id = $1::text) = $2::bigint - 1
		ON CONFLICT DO NOTHING`, r.id, int64(seq), line)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: run %s already has a line %d, or not line %d", agentsafe.ErrConflict, r.id, seq, seq-1)
	}
	return nil
}

// Lock implements agentsafe.Locker: it takes the run's lease if it's free or expired, and renews it in the
// background until unlock. If this process dies, renewal stops and the lease expires after LeaseTTL. A lease
// can also be lost while held (this process paused longer than the TTL): another runner may then take the
// run, and this one is stopped by the conditional append (ErrConflict) at its next write.
func (r *Run) Lock(ctx context.Context) (func() error, error) {
	l, err := r.acquire(ctx)
	if err != nil {
		return nil, err
	}
	return l.release, nil
}

type lease struct {
	r      *Run
	holder string
	hb     *heartbeat.Heartbeat
}

func (r *Run) ttl() time.Duration {
	if r.d.LeaseTTL > 0 {
		return r.d.LeaseTTL
	}
	return DefaultLeaseTTL
}

func (r *Run) acquire(ctx context.Context) (*lease, error) {
	holder, err := heartbeat.Holder()
	if err != nil {
		return nil, err
	}
	l := &lease{r: r, holder: holder}
	tag, err := r.d.pool.Exec(ctx, `
		INSERT INTO agentsafe_leases (run_id, holder, expires_at) VALUES ($1, $2, now() + $3 * interval '1 millisecond')
		ON CONFLICT (run_id) DO UPDATE SET holder = excluded.holder, expires_at = excluded.expires_at
		WHERE agentsafe_leases.expires_at <= now()`, r.id, l.holder, r.ttl().Milliseconds())
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("%w (run %s)", agentsafe.ErrRunLocked, r.id)
	}
	// Only extends a lease we still hold: if it was lost (expired and taken), this updates nothing, and our next
	// append is fenced.
	l.hb = heartbeat.Start(r.ttl(), func(ctx context.Context) {
		_, _ = l.r.d.pool.Exec(ctx, `UPDATE agentsafe_leases SET expires_at = now() + $3 * interval '1 millisecond'
			WHERE run_id = $1 AND holder = $2`, l.r.id, l.holder, l.r.ttl().Milliseconds())
	})
	return l, nil
}

// stopRenewing stops the heartbeat without giving the lease up: what a crash does.
func (l *lease) stopRenewing() { l.hb.Stop() }

func (l *lease) release() error {
	l.stopRenewing()
	_, err := l.r.d.pool.Exec(context.Background(), `DELETE FROM agentsafe_leases WHERE run_id = $1 AND holder = $2`,
		l.r.id, l.holder)
	return err
}

var _ agentsafe.LineStore = (*Run)(nil)
var _ agentsafe.Locker = (*Run)(nil)
