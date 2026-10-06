package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Storage is split in two, so a backend can't get the hard parts wrong:
//
//	LineStore (a backend)   stores one run's lines in order, and appends line N only if the run has exactly
//	                        N-1 lines. That's all: a file, a SQLite table, a Postgres table.
//	Journal   (this file)   everything else, identical for every backend: event encoding, sequence numbers,
//	                        the hash chain, sealing, verification on every read.
//
// The conditional append is the run's fencing token. Two runners on one run (a lease that expired under a
// paused process, a scheduler retry) both try to write event N; the store accepts one, the other gets
// ErrConflict and stops. Because the runner logs every action BEFORE taking it (tool_started before the tool
// call), a stale runner is stopped before it can act, not just before it can record. Leases (Locker) only
// save wasted work; this is what keeps a run's history single-writer.
//
// storetest.Run checks a LineStore against this contract.

// LineStore is the storage contract a backend implements for ONE run.
type LineStore interface {
	// ReadLines returns every acknowledged line, in order, without line endings. It must never return a line
	// whose AppendLine didn't succeed (a torn write), and must return every line whose AppendLine did.
	ReadLines(ctx context.Context) ([][]byte, error)
	// AppendLine durably stores line as number seq (1-based), only if exactly seq-1 lines exist. Otherwise it
	// returns an error wrapping ErrConflict and stores nothing. The check and the write must be atomic against
	// every other writer of the run, in any process: a unique (run, seq) constraint, or a lock around both.
	// line is one line of JSON, without a line ending.
	AppendLine(ctx context.Context, seq int, line []byte) error
}

// ErrConflict is returned by Append when another writer appended to the run first: this writer's view of the
// run is stale. Nothing was written. Re-read the run (Continue) rather than retrying the append.
var ErrConflict = errors.New("agentsafe: another writer appended to this run first")

// Journal is a Log over any LineStore. Key makes the hash chain an HMAC chain (chain.go); Codec seals
// content at rest (seal.go).
type Journal struct {
	Store LineStore
	Key   []byte
	Codec Codec

	next int    // next sequence number; 0 = unknown (re-read before the next append)
	prev string // chain link for the next line
}

// Append writes e as the next event. Seq, Time and Prev are set here.
func (j *Journal) Append(e Event) error {
	ctx := context.Background()
	if j.next == 0 {
		events, lines, err := j.load(ctx)
		if err != nil {
			return err
		}
		j.next, j.prev = len(events)+1, j.link(last(lines))
	}
	e.Seq, e.Time, e.Prev = j.next, time.Now().UTC(), j.prev
	if j.Codec != nil {
		var err error
		if e, err = Seal(e, j.Codec); err != nil {
			return err
		}
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := j.Store.AppendLine(ctx, e.Seq, line); err != nil {
		j.next = 0 // stale, or unknown whether it was stored: re-read before the next append
		return err
	}
	j.next++
	j.prev = j.hash(line)
	return nil
}

// Read returns every acknowledged event in order, chain-verified, with sealed events opened.
func (j *Journal) Read() ([]Event, error) {
	events, _, err := j.load(context.Background())
	if err != nil {
		return nil, err
	}
	for i := range events {
		if events[i], err = Open(events[i], j.Codec); err != nil {
			return nil, err
		}
	}
	return events, nil
}

// Head returns the chain link after the last event: anchor it outside the log (chain.go).
func (j *Journal) Head() (string, error) {
	_, lines, err := j.load(context.Background())
	if err != nil {
		return "", err
	}
	return j.link(last(lines)), nil
}

// Lock takes the store's lease when it has one (Locker). Afterwards the next append re-reads the run:
// another runner may have written while this one didn't hold the lease.
func (j *Journal) Lock() (func() error, error) {
	lk, ok := j.Store.(Locker)
	if !ok {
		return nil, ErrNoLocker
	}
	unlock, err := lk.Lock()
	if err != nil {
		return nil, err
	}
	j.next = 0
	return unlock, nil
}

// load reads and verifies the run. Events are returned still sealed.
func (j *Journal) load(ctx context.Context) ([]Event, [][]byte, error) {
	lines, err := j.Store.ReadLines(ctx)
	if err != nil {
		return nil, nil, err
	}
	events := make([]Event, len(lines))
	for i, l := range lines {
		if err := json.Unmarshal(l, &events[i]); err != nil {
			// The store only returns acknowledged lines, so an unreadable one is damage, not a torn write.
			return nil, nil, fmt.Errorf("%w: line %d can't be read: %w", ErrCorruptLog, i+1, err)
		}
	}
	if err := j.verify(events, lines); err != nil {
		return nil, nil, err
	}
	return events, lines, nil
}

func last(lines [][]byte) []byte {
	if len(lines) == 0 {
		return nil
	}
	return lines[len(lines)-1]
}
