// Package sqlite stores agentsafe runs in SQLite (pure Go: modernc.org/sqlite, no cgo).
//
//	db, err := sqlite.Open("runs.db")
//	r := &agentsafe.Runner{Log: &agentsafe.Journal{Store: db.Run("payout-2026-10-06")}, ...}
//
// Many runs share one database. Each run's lines are rows keyed by (run_id, seq); appending line N is a single
// statement, inside a write transaction, that inserts it only if the run's last line is N-1: the conditional
// append that fences stale writers (agentsafe.ErrConflict). Each run also has a lease row (agentsafe.Locker):
// held by one runner, renewed in the background, and expiring on its own if the holder dies.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const schema = `
CREATE TABLE IF NOT EXISTS agentsafe_lines (
	run_id TEXT    NOT NULL,
	seq    INTEGER NOT NULL,
	line   BLOB    NOT NULL,
	PRIMARY KEY (run_id, seq)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS agentsafe_leases (
	run_id     TEXT    PRIMARY KEY,
	holder     TEXT    NOT NULL,
	expires_at INTEGER NOT NULL -- unix milliseconds
) WITHOUT ROWID;`

// DefaultLeaseTTL is how long a lease outlives its holder's last renewal.
const DefaultLeaseTTL = 30 * time.Second

// DB is a SQLite database holding many runs.
type DB struct {
	db *sql.DB
	// LeaseTTL is how long a lease survives without renewal, i.e. how long a crashed runner blocks its run.
	// Renewal happens every LeaseTTL/3. 0 = DefaultLeaseTTL.
	LeaseTTL time.Duration
}

// Open opens (creating if needed) a database file with the settings agentsafe relies on: WAL, every commit
// fsync'd (synchronous=FULL), and waiting up to 10s for another writer instead of failing.
func Open(path string) (*DB, error) {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "synchronous(FULL)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	d, err := New(db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// New uses a database you opened yourself with the "sqlite" driver, and creates the tables if needed. Set
// busy_timeout, or concurrent writers fail with "database is locked" instead of waiting their turn.
func New(db *sql.DB) (*DB, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("agentsafe/sqlite: creating tables: %w", err)
	}
	return &DB{db: db}, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// Run returns the store for one run. It implements agentsafe.LineStore and agentsafe.Locker.
func (d *DB) Run(id string) *Run { return &Run{d: d, id: id} }

// Run is one run's lines and lease.
type Run struct {
	d  *DB
	id string
}

// ReadLines implements agentsafe.LineStore.
func (r *Run) ReadLines(ctx context.Context) ([][]byte, error) {
	rows, err := r.d.db.QueryContext(ctx, `SELECT line FROM agentsafe_lines WHERE run_id = ? ORDER BY seq`, r.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err below reports failures
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
func (r *Run) AppendLine(ctx context.Context, seq int, line []byte) error {
	if seq < 1 {
		return fmt.Errorf("%w: line %d", agentsafe.ErrConflict, seq)
	}
	// The check and the insert are ONE statement, so SQLite takes the write lock (waiting up to busy_timeout)
	// before evaluating the check: two writers can't both see "last line is N-1". BEGIN IMMEDIATE is a guard
	// for later edits: if a read were ever added before the insert, a plain BEGIN would make that read a stale
	// snapshot, and the upgrade to writer would fail with SQLITE_BUSY_SNAPSHOT instead of waiting.
	conn, err := r.d.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // returns the connection to the pool
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	res, err := conn.ExecContext(ctx, `
		INSERT INTO agentsafe_lines (run_id, seq, line)
		SELECT ?1, ?2, ?3
		WHERE (SELECT COALESCE(MAX(seq), 0) FROM agentsafe_lines WHERE run_id = ?1) = ?2 - 1
		ON CONFLICT DO NOTHING`, r.id, seq, line)
	if err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("%w: run %s already has a line %d, or not line %d", agentsafe.ErrConflict, r.id, seq, seq-1)
	}
	return nil
}

// Lock implements agentsafe.Locker: it takes the run's lease if it's free or expired, and renews it in the
// background until unlock. If this process dies, renewal stops and the lease expires after LeaseTTL. A lease
// can also be lost while held (this process paused longer than the TTL): another runner may then take the
// run, and this one is stopped by the conditional append (ErrConflict) at its next write.
func (r *Run) Lock() (func() error, error) {
	l, err := r.acquire()
	if err != nil {
		return nil, err
	}
	return l.release, nil
}

type lease struct {
	r      *Run
	holder string
	stop   chan struct{}
	done   sync.WaitGroup
	once   sync.Once
}

func (r *Run) ttl() time.Duration {
	if r.d.LeaseTTL > 0 {
		return r.d.LeaseTTL
	}
	return DefaultLeaseTTL
}

func (r *Run) acquire() (*lease, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	l := &lease{r: r, holder: hex.EncodeToString(b), stop: make(chan struct{})}
	now := time.Now()
	res, err := r.d.db.Exec(`
		INSERT INTO agentsafe_leases (run_id, holder, expires_at) VALUES (?1, ?2, ?3)
		ON CONFLICT (run_id) DO UPDATE SET holder = excluded.holder, expires_at = excluded.expires_at
		WHERE agentsafe_leases.expires_at <= ?4`,
		r.id, l.holder, now.Add(r.ttl()).UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, fmt.Errorf("%w (run %s)", agentsafe.ErrRunLocked, r.id)
	}
	l.done.Add(1)
	go l.renew()
	return l, nil
}

func (l *lease) renew() {
	defer l.done.Done()
	t := time.NewTicker(l.r.ttl() / 3)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			// Only extends a lease we still hold. If it was lost (expired and taken), this updates nothing,
			// and our next append is fenced.
			_, _ = l.r.d.db.Exec(`UPDATE agentsafe_leases SET expires_at = ? WHERE run_id = ? AND holder = ?`,
				time.Now().Add(l.r.ttl()).UnixMilli(), l.r.id, l.holder)
		}
	}
}

// stopRenewing stops the heartbeat without giving the lease up: what a crash does.
func (l *lease) stopRenewing() {
	l.once.Do(func() { close(l.stop) })
	l.done.Wait()
}

func (l *lease) release() error {
	l.stopRenewing()
	_, err := l.r.d.db.Exec(`DELETE FROM agentsafe_leases WHERE run_id = ? AND holder = ?`, l.r.id, l.holder)
	return err
}

var _ agentsafe.LineStore = (*Run)(nil)
var _ agentsafe.Locker = (*Run)(nil)
