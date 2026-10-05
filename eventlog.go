package agentsafe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// EventType is what happened. These ten types are the whole vocabulary of a run.
type EventType string

// The event types. A run's log is a sequence of these and nothing else.
const (
	EvRunStarted   EventType = "run_started"   // system prompt + task: the run's inputs
	EvModelDecided EventType = "model_decided" // the model's full response, logged BEFORE anything acts on it
	EvToolStarted  EventType = "tool_started"  // the runner is about to execute a call: "maybe executed" from here on
	EvToolResult   EventType = "tool_result"   // the call's outcome, as sent back to the model
	EvRunFinished  EventType = "run_finished"  // stop reason + final text. Terminal: nothing follows it.

	EvRunPaused      EventType = "run_paused"      // budget exhausted: stopped, NOT finished (week 3 S1 review, Q3)
	EvBudgetExtended EventType = "budget_extended" // someone decided to give a paused run more steps: logged, never silent

	EvToolRefused       EventType = "tool_refused"       // a pending call resolved WITHOUT being attempted: failed validation, or rejected
	EvApprovalRequested EventType = "approval_requested" // a gated call is waiting for a human: the run is paused, durably
	EvApprovalDecided   EventType = "approval_decided"   // approved or rejected, by whom, why
)

// Event is one line of the log. Fields are used per type; the rest are omitted.
type Event struct {
	V    int       `json:"v,omitempty"` // log format version (FormatVersion); absent = 0, written before versions existed
	Seq  int       `json:"seq"`
	Type EventType `json:"type"`
	Time time.Time `json:"time"`

	// run_started
	System   string `json:"system,omitempty"`
	Task     string `json:"task,omitempty"`
	MaxSteps int    `json:"max_steps,omitempty"` // the budget lives in the log, not in the runner's config
	Provider string `json:"provider,omitempty"`  // e.g. "groq": so a trace can be built from the log alone
	Model    string `json:"model,omitempty"`

	// run_paused / budget_extended
	Reason     string `json:"reason,omitempty"`
	ExtraSteps int    `json:"extra_steps,omitempty"`
	By         string `json:"by,omitempty"` // who extended it

	// model_decided
	Step         int      `json:"step,omitempty"`
	Message      *Message `json:"message,omitempty"`
	FinishReason string   `json:"finish_reason,omitempty"`
	Usage        *Usage   `json:"usage,omitempty"`

	// tool_started / tool_result
	CallID string `json:"call_id,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"` // JSON, exactly what the model receives

	// tool_started / tool_result for an IdempotentTool
	Key         string `json:"key,omitempty"`          // derived from the operation's identity, never from the call id
	PayloadHash string `json:"payload_hash,omitempty"` // compared on replay: same key + different payload = conflict
	Replayed    bool   `json:"replayed,omitempty"`     // the result came from an earlier tool_result, nothing ran

	// approval_requested / approval_decided / tool_refused
	Summary  string `json:"summary,omitempty"`  // what the approver is shown: VALIDATED values, as JSON
	Decision string `json:"decision,omitempty"` // "approved" | "rejected"

	// run_finished
	Stop string `json:"stop,omitempty"`
	Text string `json:"text,omitempty"`
}

// Log is an append-only record of a run. It is the source of truth: state is derived from it (Rebuild),
// never kept only in memory (week 2 W2-5).
type Log interface {
	Append(e Event) error
	Read() ([]Event, error)
}

// ErrCorruptLog is returned when acknowledged history is damaged: a line that can't be read with valid
// lines after it. That is never the result of a crash (a crash only cuts the end off), so it is refused
// rather than skipped: skipping it would silently delete something that really happened.
var ErrCorruptLog = errors.New("agentsafe: log is corrupt")

// FileLog is a JSON-lines Log on disk. Every Append is fsync'd before it returns, so an event that
// Append reported as written survives kill -9 and power loss.
//
// Torn tails: a crash or power cut in the middle of an Append can leave a partial last line. That event was
// never acknowledged (Append hadn't returned), so nothing acted on it, and dropping it is exactly as if the
// crash had happened just before the write. Read ignores a torn tail; the next Append cuts it off the file
// before writing.
type FileLog struct {
	Path string
	next int // next sequence number; 0 = not yet loaded (re-read, and the tail repaired, on next Append)
}

// Append writes e as the next line. Seq and Time are set here.
func (l *FileLog) Append(e Event) error {
	if l.next == 0 {
		n, err := l.repair()
		if err != nil {
			return err
		}
		l.next = n + 1
	}
	e.Seq, e.Time = l.next, time.Now().UTC()
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	// 0600: the log holds tool arguments and results (payees, amounts, account ids): owner-only.
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close() // the write error is the one that matters
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close() // the sync error is the one that matters
		return err
	}
	l.next++
	return f.Close()
}

// Read returns every acknowledged event in order, ignoring a torn tail. A missing file is an empty log.
func (l *FileLog) Read() ([]Event, error) {
	events, _, err := l.load()
	return events, err
}

// repair cuts a torn tail off the file, so the next line isn't glued onto half a line, and returns the
// number of acknowledged events. Only the run's writer (the lock holder) calls it, via Append.
func (l *FileLog) repair() (int, error) {
	events, good, err := l.load()
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if info.Size() == good {
		return len(events), nil
	}
	f, err := os.OpenFile(l.Path, os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	if err := f.Truncate(good); err != nil {
		_ = f.Close()
		return 0, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return 0, err
	}
	return len(events), f.Close()
}

func (l *FileLog) load() ([]Event, int64, error) {
	data, err := os.ReadFile(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	events, good, err := scanLines(data)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", l.Path, err)
	}
	return events, good, nil
}

// scanLines parses a JSON-lines log. It returns the acknowledged events and the byte length they occupy.
//
//	bytes after the last newline       an unfinished write: torn, dropped
//	a last line that doesn't parse     a torn write (e.g. zero-filled blocks after power loss): dropped
//	a bad line with good lines after   damaged acknowledged history: ErrCorruptLog
func scanLines(data []byte) ([]Event, int64, error) {
	var events []Event
	var good int64
	lines := bytes.SplitAfter(data, []byte{'\n'})
	for i, raw := range lines {
		if len(raw) == 0 {
			continue
		}
		complete := raw[len(raw)-1] == '\n'
		if !complete { // the final fragment: never acknowledged
			break
		}
		var e Event
		if err := json.Unmarshal(bytes.TrimSpace(raw), &e); err != nil {
			if laterData(lines[i+1:]) {
				return nil, 0, fmt.Errorf("%w: line %d can't be read and later lines exist: %v", ErrCorruptLog, i+1, err)
			}
			break // the last line, unreadable: a torn write
		}
		events = append(events, e)
		good += int64(len(raw))
	}
	return events, good, nil
}

func laterData(rest [][]byte) bool {
	for _, r := range rest {
		if len(bytes.TrimSpace(r)) > 0 {
			return true
		}
	}
	return false
}
