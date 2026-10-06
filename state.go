package agentsafe

import "fmt"

// Status is where a run is. Every status is derived from the log; none is stored.
type Status string

// The statuses a run can be in. Each is derived from the log by Rebuild.
const (
	StatusNew              Status = "new"               // no events yet
	StatusAwaitingModel    Status = "awaiting_model"    // next thing to do: ask the model
	StatusExecuting        Status = "executing"         // the model proposed calls; some have no result yet
	StatusAwaitingApproval Status = "awaiting_approval" // a gated call waits for a human; nothing is held in memory
	StatusAnswered         Status = "answered"          // the model gave a final answer; run_finished not yet logged
	StatusPaused           Status = "paused"            // out of budget: resumable only via an explicit budget_extended
	StatusFinished         Status = "finished"          // done. Terminal: downstream may rely on "finished" meaning finished
)

// State is the run as reconstructed from its events.
type State struct {
	Status   Status
	Step     int        // model decisions so far
	Messages []Message  // the conversation to send on the next model call
	Pending  []ToolCall // proposed by the model, no tool_result yet, in the model's order
	// Started holds pending calls that have a tool_started event: they MAY have executed before a crash.
	// A pending call NOT in Started definitely never ran. That distinction is why tool_started exists.
	Started map[string]bool
	Stop    string
	Text    string // final answer
	Events  int
	Budget  int // max model decisions, from run_started + every budget_extended

	// Approvals: decision per call id ("requested", "approved", "rejected"); ByKey: per operation key, so an
	// approval can be addressed by the payout's key and a repeated identical decision is a no-op.
	Approvals map[string]string
	ByKey     map[string]string
	Waiting   *Waiting // set while awaiting_approval
	Denials   int      // refused approval attempts (approval_denied), for audit
	StartedBy string   // who started the run (Runner.StartedBy), for separation of duties

	// Effects indexes every completed idempotent call by key: the log IS the idempotency store for
	// outcomes it has seen. (Outcomes it never saw, after a crash mid-call, are the tool's job: it gets the key.)
	Effects map[string]Outcome
}

// Waiting is the gated call a run is paused on.
type Waiting struct {
	CallID, Key, Tool, Summary string
}

// Outcome is the recorded result of a completed idempotent call.
type Outcome struct {
	PayloadHash string
	Result      string
}

// NewState is the state of a run with no events.
func NewState() State {
	return State{Status: StatusNew, Started: map[string]bool{}, Effects: map[string]Outcome{},
		Approvals: map[string]string{}, ByKey: map[string]string{}}
}

// Rebuild folds a log into a State, checking every transition. A log that breaks the rules is an error,
// not something to guess around.
func Rebuild(events []Event) (State, error) {
	s := NewState()
	for _, e := range events {
		e, err := Upgrade(e) // older formats are upgraded; newer ones are refused, never guessed at
		if err != nil {
			return s, err
		}
		if err := s.Apply(e); err != nil {
			return s, fmt.Errorf("event %d (%s): %w", e.Seq, e.Type, err)
		}
	}
	return s, nil
}

// Apply is the transition function. It is the ONLY code that changes a State.
//
//	new            --run_started-->                 awaiting_model
//	awaiting_model --model_decided (with calls)-->  executing
//	awaiting_model --model_decided (no calls)-->    answered
//	executing      --tool_started-->                executing       (call marked "maybe executed")
//	executing      --tool_result-->                 executing | awaiting_model (when the last call resolves)
//	awaiting_model --run_paused-->                  paused          (budget exhausted: resumable)
//	paused         --budget_extended-->             awaiting_model  (an explicit, logged decision)
//	answered       --run_finished-->                finished        (terminal)
//	awaiting_model --run_finished-->                finished        (e.g. stopped by an operator)
//	executing      --tool_refused-->                executing | awaiting_model (call resolved, never attempted)
//	executing      --approval_requested-->          awaiting_approval
//	awaiting_appr. --approval_decided-->            executing       (approved: may now start; rejected: must be refused)
//
// Not allowed, by design:
//   - run_finished while executing: every proposed call must have an outcome first (week 1 F14)
//   - tool_result without tool_started: an outcome for something that was never attempted
//   - tool_started / tool_result for a call the model didn't propose, or that already has a result (F8)
//   - model_decided while calls are pending, with a step out of order, or beyond the budget
//   - anything at all after run_finished
//   - tool_started for a call whose approval is pending or was rejected (the gate can't be skipped)
//   - tool_refused for a call that was already started (it may have run: that needs a tool_result)
//   - approval_requested for an operation already rejected in this run (a rejection can't be undone by asking again)
func (s *State) Apply(e Event) error {
	if e.Sealed != "" {
		return fmt.Errorf("%w (event %d)", ErrSealed, e.Seq)
	}
	if err := s.apply(e); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidTransition, err)
	}
	return nil
}

// transitions is the whole vocabulary of a run: one handler per event type, each enforcing what that event
// may follow. An event type missing here is refused.
var transitions = map[EventType]func(*State, Event) error{
	EvRunStarted:        (*State).runStarted,
	EvModelDecided:      (*State).modelDecided,
	EvToolStarted:       (*State).toolStarted,
	EvToolResult:        (*State).toolResult,
	EvToolRefused:       (*State).toolRefused,
	EvApprovalRequested: (*State).approvalRequested,
	EvApprovalDecided:   (*State).approvalDecided,
	EvApprovalDenied:    (*State).approvalDenied,
	EvRunPaused:         (*State).runPaused,
	EvBudgetExtended:    (*State).budgetExtended,
	EvRunFinished:       (*State).runFinished,
}

func (s *State) apply(e Event) error {
	handle, ok := transitions[e.Type]
	if !ok {
		return fmt.Errorf("unknown event type %q", e.Type)
	}
	if err := handle(s, e); err != nil {
		return err
	}
	s.Events++
	return nil
}

// runStarted applies run_started: the run begins, with its inputs and budget.
func (s *State) runStarted(e Event) error {
	if s.Status != StatusNew {
		return fmt.Errorf("run_started in status %s", s.Status)
	}
	if e.MaxSteps <= 0 {
		return fmt.Errorf("run_started without a budget (max_steps)")
	}
	s.Messages = []Message{{Role: RoleSystem, Content: Str(e.System)}, {Role: RoleUser, Content: Str(e.Task)}}
	s.Budget, s.StartedBy = e.MaxSteps, e.By
	s.Status = StatusAwaitingModel
	return nil
}

// modelDecided applies model_decided: a final answer, or calls to resolve (ids unique, within the budget).
func (s *State) modelDecided(e Event) error {
	if s.Status != StatusAwaitingModel {
		return fmt.Errorf("model_decided in status %s", s.Status)
	}
	if e.Step != s.Step+1 {
		return fmt.Errorf("model_decided step %d, expected %d", e.Step, s.Step+1)
	}
	if e.Message == nil {
		return fmt.Errorf("model_decided without a message")
	}
	if e.Step > s.Budget {
		return fmt.Errorf("model_decided step %d beyond budget %d", e.Step, s.Budget)
	}
	s.Step = e.Step
	if len(e.Message.ToolCalls) == 0 {
		if e.Message.Content != nil {
			s.Text = *e.Message.Content
		}
		s.Stop = e.FinishReason
		s.Status = StatusAnswered
		return nil
	}
	seen := map[string]bool{}
	for _, c := range e.Message.ToolCalls {
		if c.ID == "" || seen[c.ID] {
			return fmt.Errorf("tool call with empty or repeated id %q", c.ID)
		}
		seen[c.ID] = true
	}
	s.Messages = append(s.Messages, *e.Message)
	s.Pending = append([]ToolCall(nil), e.Message.ToolCalls...)
	s.Status = StatusExecuting
	return nil
}

// toolStarted applies tool_started: a pending, not-refused call is about to run; from here it may have executed.
func (s *State) toolStarted(e Event) error {
	if s.Status != StatusExecuting {
		return fmt.Errorf("tool_started in status %s", s.Status)
	}
	if !s.isPending(e.CallID) {
		return fmt.Errorf("tool_started for %q, which is not a pending call", e.CallID)
	}
	if d := s.Approvals[e.CallID]; d != "" && d != "approved" {
		return fmt.Errorf("tool_started for %q, whose approval is %s", e.CallID, d)
	}
	s.Started[e.CallID] = true // re-starting after a crash is allowed: it's the retry
	return nil
}

// toolResult applies tool_result: a started call's outcome; an executed result per idempotency key at most once.
func (s *State) toolResult(e Event) error {
	if s.Status != StatusExecuting {
		return fmt.Errorf("tool_result in status %s", s.Status)
	}
	if !s.isPending(e.CallID) {
		return fmt.Errorf("tool_result for %q, which is not a pending call", e.CallID)
	}
	if !s.Started[e.CallID] {
		return fmt.Errorf("tool_result for %q with no tool_started", e.CallID)
	}
	s.Messages = append(s.Messages, Message{Role: RoleTool, ToolCallID: e.CallID, Content: Str(e.Result)})
	if e.Key != "" && !e.Replayed {
		if _, dup := s.Effects[e.Key]; dup {
			return fmt.Errorf("second executed result for idempotency key %s", e.Key)
		}
		s.Effects[e.Key] = Outcome{PayloadHash: e.PayloadHash, Result: e.Result}
	}
	s.removePending(e.CallID)
	delete(s.Started, e.CallID)
	if len(s.Pending) == 0 {
		s.Status = StatusAwaitingModel
	}
	return nil
}

// toolRefused applies tool_refused: a pending call resolved without being attempted (never one that started).
func (s *State) toolRefused(e Event) error {
	if s.Status != StatusExecuting {
		return fmt.Errorf("tool_refused in status %s", s.Status)
	}
	if !s.isPending(e.CallID) {
		return fmt.Errorf("tool_refused for %q, which is not a pending call", e.CallID)
	}
	if s.Started[e.CallID] {
		return fmt.Errorf("tool_refused for %q, which was already started and may have run", e.CallID)
	}
	s.Messages = append(s.Messages, Message{Role: RoleTool, ToolCallID: e.CallID, Content: Str(e.Result)})
	s.removePending(e.CallID)
	if len(s.Pending) == 0 {
		s.Status = StatusAwaitingModel
	}
	return nil
}

// approvalRequested applies approval_requested: a pending, unstarted call waits for a human, addressed by its operation key.
func (s *State) approvalRequested(e Event) error {
	if s.Status != StatusExecuting {
		return fmt.Errorf("approval_requested in status %s", s.Status)
	}
	if !s.isPending(e.CallID) || s.Started[e.CallID] || s.Approvals[e.CallID] != "" {
		return fmt.Errorf("approval_requested for %q: not pending, already started, or already requested", e.CallID)
	}
	if e.Key == "" {
		return fmt.Errorf("approval_requested without an operation key")
	}
	if s.ByKey[e.Key] == "rejected" {
		return fmt.Errorf("approval_requested for operation %s, which was already rejected in this run", e.Key)
	}
	s.Approvals[e.CallID], s.ByKey[e.Key] = "requested", "requested"
	s.Waiting = &Waiting{CallID: e.CallID, Key: e.Key, Tool: e.Tool, Summary: e.Summary}
	s.Status = StatusAwaitingApproval
	return nil
}

// approvalDecided applies approval_decided: the decision on the call the run is waiting on.
func (s *State) approvalDecided(e Event) error {
	if s.Status != StatusAwaitingApproval {
		return fmt.Errorf("approval_decided in status %s", s.Status)
	}
	if s.Waiting == nil || e.CallID != s.Waiting.CallID || e.Key != s.Waiting.Key {
		return fmt.Errorf("approval_decided for %q/%q, but the run is waiting on another call", e.CallID, e.Key)
	}
	if (e.Decision != "approved" && e.Decision != "rejected") || e.By == "" {
		return fmt.Errorf("approval_decided needs decision approved|rejected and who decided (by)")
	}
	s.Approvals[e.CallID], s.ByKey[e.Key] = e.Decision, e.Decision
	s.Waiting, s.Status = nil, StatusExecuting
	return nil
}

// approvalDenied applies approval_denied: a refused attempt to decide; the run keeps waiting.
func (s *State) approvalDenied(e Event) error {
	if s.Status != StatusAwaitingApproval || s.Waiting == nil || e.CallID != s.Waiting.CallID || e.Key != s.Waiting.Key {
		return fmt.Errorf("approval_denied for %q/%q, but the run isn't waiting on it", e.CallID, e.Key)
	}
	if e.By == "" {
		return fmt.Errorf("approval_denied needs who tried (by)")
	}
	s.Denials++ // the run keeps waiting: a refused attempt changes nothing but the record
	return nil
}

// runPaused applies run_paused: out of budget, between model calls.
func (s *State) runPaused(e Event) error {
	if s.Status != StatusAwaitingModel {
		return fmt.Errorf("run_paused in status %s", s.Status)
	}
	s.Stop, s.Status = e.Reason, StatusPaused
	return nil
}

// budgetExtended applies budget_extended: someone gave a paused run more steps.
func (s *State) budgetExtended(e Event) error {
	if s.Status != StatusPaused {
		return fmt.Errorf("budget_extended in status %s (only a paused run can be extended)", s.Status)
	}
	if e.ExtraSteps <= 0 || e.By == "" {
		return fmt.Errorf("budget_extended needs extra_steps > 0 and who decided (by)")
	}
	s.Budget += e.ExtraSteps
	s.Stop, s.Status = "", StatusAwaitingModel
	return nil
}

// runFinished applies run_finished: terminal, with nothing pending.
func (s *State) runFinished(e Event) error {
	if s.Status != StatusAwaitingModel && s.Status != StatusAnswered {
		return fmt.Errorf("run_finished in status %s (pending calls: %d)", s.Status, len(s.Pending))
	}
	s.Stop, s.Status = e.Stop, StatusFinished
	if e.Text != "" {
		s.Text = e.Text
	}
	return nil
}

func (s *State) isPending(id string) bool {
	for _, c := range s.Pending {
		if c.ID == id {
			return true
		}
	}
	return false
}

func (s *State) removePending(id string) {
	for i, c := range s.Pending {
		if c.ID == id {
			s.Pending = append(s.Pending[:i], s.Pending[i+1:]...)
			return
		}
	}
}
