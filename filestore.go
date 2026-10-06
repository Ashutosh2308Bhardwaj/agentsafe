package agentsafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// fileStore is the LineStore behind FileLog: one JSON line per event, fsync'd before AppendLine returns.
//
// Torn tails: a crash or power cut in the middle of a write can leave a partial last line. It was never
// acknowledged (AppendLine hadn't returned), so nothing acted on it, and dropping it is exactly as if the crash
// had happened just before the write. ReadLines ignores a torn tail; the next AppendLine cuts it off.
//
// The conditional append: the check "exactly seq-1 lines" and the write happen under an exclusive OS lock on
// "<path>.append" (held for one append), so two writers in any processes can't both write line N.
type fileStore struct {
	path string

	size  int64 // file size after our last read or write; -1 = unknown
	count int   // acknowledged lines at that size
}

func newFileStore(path string) *fileStore { return &fileStore{path: path, size: -1} }

// NewFileStore returns the file LineStore FileLog uses, for building a Journal yourself. It is also a Locker.
func NewFileStore(path string) LineStore { return newFileStore(path) }

func (s *fileStore) ReadLines(context.Context) ([][]byte, error) {
	lines, _, err := s.scan()
	return lines, err
}

func (s *fileStore) AppendLine(ctx context.Context, seq int, line []byte) error {
	if bytes.ContainsAny(line, "\r\n") {
		return errors.New("agentsafe: a log line can't contain a line break")
	}
	release, err := lockPath(s.path + ".append")
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil { // checked after waiting for the lock: the caller may have given up meanwhile
		return err
	}

	info, err := os.Stat(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.size, s.count = 0, 0
	case err != nil:
		return err
	case info.Size() != s.size:
		// First append, someone else appended, or a torn tail: re-read, and cut off anything unacknowledged.
		if err := s.repair(info.Size()); err != nil {
			return err
		}
	}
	if seq != s.count+1 {
		return fmt.Errorf("%w: writing line %d, but the log has %d lines", ErrConflict, seq, s.count)
	}

	// 0600: the log holds tool arguments and results (payees, amounts, account ids): owner-only.
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close() // the write error is the one that matters
		s.size = -1
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close() // the sync error is the one that matters
		s.size = -1
		return err
	}
	s.size += int64(len(line)) + 1
	s.count++
	return f.Close()
}

// repair re-reads the file and truncates a torn tail.
func (s *fileStore) repair(size int64) error {
	lines, good, err := s.scan()
	if err != nil {
		return err
	}
	if size > good {
		f, err := os.OpenFile(s.path, os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if err := f.Truncate(good); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	s.size, s.count = good, len(lines)
	return nil
}

// scan reads the file. It returns the acknowledged lines and the byte length they occupy.
//
//	bytes after the last newline       an unfinished write: torn, dropped
//	a last line that isn't JSON        a torn write (e.g. zero-filled blocks after power loss): dropped
//	a bad line with good lines after   damaged acknowledged history: ErrCorruptLog
func (s *fileStore) scan() ([][]byte, int64, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var kept [][]byte
	var good int64
	raws := bytes.SplitAfter(data, []byte{'\n'})
	for i, raw := range raws {
		if len(raw) == 0 {
			continue
		}
		if raw[len(raw)-1] != '\n' { // the final fragment: never acknowledged
			break
		}
		if !json.Valid(bytes.TrimSpace(raw)) {
			if laterData(raws[i+1:]) {
				return nil, 0, fmt.Errorf("%s: %w: line %d can't be read and later lines exist", s.path, ErrCorruptLog, i+1)
			}
			break // the last line, unreadable: a torn write
		}
		kept = append(kept, lineContent(raw))
		good += int64(len(raw))
	}
	return kept, good, nil
}

// Lock takes an exclusive OS-level lock on "<path>.lock" (flock on Unix, LockFileEx on Windows). The OS
// releases it when the holding process exits, however it exits, so no expiry is needed. It works across
// processes on one machine; runs shared between machines need a database-backed store.
func (s *fileStore) Lock(context.Context) (func() error, error) {
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	s.size = -1 // another process may have appended while we didn't hold the lease
	return func() error {
		uerr := unlockFile(f)
		if cerr := f.Close(); uerr == nil {
			uerr = cerr
		}
		return uerr
	}, nil
}

// lockPath takes a BLOCKING exclusive OS lock on path, for one short critical section.
func lockPath(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: the caller's own log path + suffix
	if err != nil {
		return nil, err
	}
	if err := waitLock(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = unlockFile(f) // closing releases it anyway
		_ = f.Close()
	}, nil
}

func laterData(rest [][]byte) bool {
	for _, r := range rest {
		if len(bytes.TrimSpace(r)) > 0 {
			return true
		}
	}
	return false
}
