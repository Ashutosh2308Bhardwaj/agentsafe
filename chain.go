package agentsafe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrTampered is returned when a log's history was changed after it was written: a line edited, inserted,
// deleted or reordered. A tampered run is refused, never resumed: resuming it could act on, say, a forged
// approval.
var ErrTampered = errors.New("agentsafe: log history was altered")

// genesis is the chain link of a log's first event.
const genesis = "genesis"

// The hash chain. Every line FileLog writes carries "prev": the hash of the previous line exactly as it is on
// disk (SHA-256, or HMAC-SHA256 with FileLog.Key). Hashing the stored bytes, not a re-encoding of the event,
// means JSON encoding details and future format upgrades can never break the chain.
//
// What it catches, on every Read: an edited, inserted, deleted or reordered line anywhere before the last.
// What it can't catch on its own, and how to close each gap:
//   - an edit to the LAST line (nothing after it links to it): anchor Head() somewhere the attacker can't
//     write (your database, the approval record, a ticket) and compare;
//   - a rewrite of the whole file with every link recomputed: set FileLog.Key and keep the key off the host.
//
// Logs written before the chain existed have no "prev". That is accepted as an unchained prefix; the chain
// starts at the first linked line, and from then on a missing link is tampering.

func (l *FileLog) hash(line []byte) string {
	if l.Key != nil {
		m := hmac.New(sha256.New, l.Key)
		m.Write(line)
		return hex.EncodeToString(m.Sum(nil))
	}
	h := sha256.Sum256(line)
	return hex.EncodeToString(h[:])
}

// link is the "prev" value for the line after `last` (nil = the first line).
func (l *FileLog) link(last []byte) string {
	if last == nil {
		return genesis
	}
	return l.hash(last)
}

func (l *FileLog) verify(events []Event, lines [][]byte) error {
	chained := false
	for i, e := range events {
		if e.Seq != i+1 {
			return fmt.Errorf("%w: line %d has seq %d, want %d (a line was removed, added or moved)", ErrTampered, i+1, e.Seq, i+1)
		}
		if e.Prev == "" {
			if chained {
				return fmt.Errorf("%w: line %d has no chain link, but the chain started earlier", ErrTampered, i+1)
			}
			continue // unchained prefix: written before the chain existed
		}
		want := genesis
		if i > 0 {
			want = l.hash(lines[i-1])
		}
		if e.Prev != want {
			return fmt.Errorf("%w: line %d (seq %d) doesn't link to the line before it: the history before it was changed", ErrTampered, i+1, e.Seq)
		}
		chained = true
	}
	return nil
}

// Head returns the hash of the last acknowledged line: anchor it outside the log to make an edit of the
// last event, or a truncation, detectable. An empty log's head is "genesis".
func (l *FileLog) Head() (string, error) {
	_, last, _, err := l.load()
	if err != nil {
		return "", err
	}
	return l.link(last), nil
}
