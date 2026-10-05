package agentsafe

import (
	"bufio"
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

// FileLog is a JSON-lines Log on disk. Every Append is fsync'd before it returns, so an event that
// Append reported as written survives kill -9 and power loss.
type FileLog struct {
	Path string
	next int // next sequence number; 0 = not yet loaded
}

// Append writes e as the next line. Seq and Time are set here.
func (l *FileLog) Append(e Event) error {
	if l.next == 0 {
		events, err := l.Read()
		if err != nil {
			return err
		}
		l.next = len(events) + 1
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

// Read returns every event in order. A missing file is an empty log.
func (l *FileLog) Read() ([]Event, error) {
	f, err := os.Open(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only
	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for n := 1; sc.Scan(); n++ {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			// Append writes whole lines and fsyncs, so a torn line means the file was damaged outside us.
			return nil, fmt.Errorf("%s line %d: %w", l.Path, n, err)
		}
		events = append(events, e)
	}
	return events, sc.Err()
}
