package agentsafe

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// EventType is what happened. These eleven types are the whole vocabulary of a run.
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
	EvApprovalDenied    EventType = "approval_denied"    // a decision the Authorizer refused (format v2): who tried, and why not
)

// Event is one line of the log. Fields are used per type; the rest are omitted.
type Event struct {
	V    int       `json:"v,omitempty"` // log format version (FormatVersion); absent = 0, written before versions existed
	Seq  int       `json:"seq"`
	Prev string    `json:"prev,omitempty"` // hash of the previous line as written (FileLog's chain); see chain.go
	Type EventType `json:"type"`
	Time time.Time `json:"time"`

	// run_started
	System   string `json:"system,omitempty"`
	Task     string `json:"task,omitempty"`
	MaxSteps int    `json:"max_steps,omitempty"` // the budget lives in the log, not in the runner's config
	Provider string `json:"provider,omitempty"`  // e.g. "groq": so a trace can be built from the log alone
	Model    string `json:"model,omitempty"`

	// run_paused / budget_extended / approvals
	Reason     string `json:"reason,omitempty"`
	ExtraSteps int    `json:"extra_steps,omitempty"`
	By         string `json:"by,omitempty"` // who extended, decided, or (run_started, v2) started the run

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

	// Sealed (format v3) holds the content fields above, encrypted by a Codec; they are empty while it is
	// set. See seal.go.
	Sealed string `json:"sealed,omitempty"`
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

// FileLog is a JSON-lines Log on disk: a Journal over a file. Every Append is fsync'd before it returns, so
// an event that Append reported as written survives kill -9 and power loss. A torn last line (a crash in the
// middle of a write) is ignored by Read and cut off by the next Append; see filestore.go.
type FileLog struct {
	Path string
	// Key, if set, makes the chain an HMAC-SHA256 chain: without the key, an attacker who rewrites the whole
	// file can't recompute the links. Use the same key for a log's whole life; keep it off the log's host.
	Key []byte
	// Codec, if set, seals each event's content before it is written and opens it on Read (seal.go). The
	// chain covers the sealed line, so tampering is still detected without the Codec's keys.
	Codec Codec

	j *Journal
}

func (l *FileLog) journal() *Journal {
	if l.j == nil {
		l.j = &Journal{Store: newFileStore(l.Path)}
	}
	l.j.Key, l.j.Codec = l.Key, l.Codec
	return l.j
}

// Append writes e as the next line. Seq, Time and Prev are set here.
func (l *FileLog) Append(e Event) error { return l.journal().Append(e) }

// Read returns every acknowledged event in order, ignoring a torn tail. A missing file is an empty log.
// Sealed events are opened with the Codec; without it, a sealed log is refused (ErrSealed).
func (l *FileLog) Read() ([]Event, error) {
	events, err := l.journal().Read()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", l.Path, err)
	}
	return events, nil
}

// Head returns the hash of the last acknowledged line: anchor it outside the log to make an edit of the
// last event, or a truncation, detectable. An empty log's head is "genesis".
func (l *FileLog) Head() (string, error) { return l.journal().Head() }

// Lock takes the run's lease: an exclusive OS lock on "<Path>.lock", released by the OS when the holder
// exits, however it exits (filestore.go).
func (l *FileLog) Lock() (func() error, error) { return l.journal().Lock() }

// load reads and verifies the file; events are returned still sealed.
func (l *FileLog) load() ([]Event, error) {
	events, _, err := l.journal().load(context.Background())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", l.Path, err)
	}
	return events, nil
}
