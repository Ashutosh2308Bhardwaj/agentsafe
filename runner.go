package agentsafe

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Tool is something the model can ask the runner to do.
type Tool interface {
	Spec() ToolSpec
	// Call executes the tool. A returned error becomes an {"error": ...} result for the model, not a run
	// failure: the model decides what to do about a failed tool.
	Call(ctx context.Context, args json.RawMessage) (any, error)
}

// Runner drives a run from its log. It holds no run state of its own: everything it knows about the run
// comes from Rebuild(log), so a fresh Runner on the same log continues the same run.
type Runner struct {
	Model    Model
	Tools    []Tool
	Log      Log
	MaxSteps int                  // budget for a NEW run (logged in run_started); 0 = 8. Resumes use the logged budget.
	Scope    string               // idempotency scope, e.g. a hash of the input files; "" = per log
	Logf     func(string, ...any) // progress output; nil = silent
	Redact   Redactor             // masks arguments, results and summaries in Logf output (redact.go); nil = shown as is
	Hook     func(point string)   // test/chaos hook, called at named points; nil = none

	// Tool calls (exec.go). ToolTimeout bounds each call (a TimeoutTool sets its own); 0 = no limit. When an
	// IdempotentTool's outcome is unknown (timeout, ErrOutcomeUnknown), it's retried with the same key up to
	// ToolAttempts times (0 = 3), waiting ToolBackoff (0 = 200ms) and doubling between tries.
	ToolTimeout  time.Duration
	ToolAttempts int
	ToolBackoff  time.Duration

	// Authorizer decides who may approve or reject gated calls. Without one, decisions are refused
	// (ErrNoAuthorizer) unless AnyApprover is set: a gate anyone can open must be a visible choice.
	Authorizer  Authorizer
	AnyApprover bool
	// StartedBy records who started the run (an identity your system verified); NotRequester uses it.
	StartedBy string

	// Unlocked runs without a single-driver guarantee when the Log doesn't implement Locker. Off by default:
	// running a run from two processes at once must be a visible choice, not a silent gap.
	Unlocked bool
}

// Start begins a new run. It refuses a log that already has events.
func (r *Runner) Start(ctx context.Context, system, task string) (State, error) {
	release, err := r.lock(ctx)
	if err != nil {
		return State{}, err
	}
	defer release()
	events, err := r.Log.Read(ctx)
	if err != nil {
		return State{}, err
	}
	if len(events) > 0 {
		return State{}, fmt.Errorf("log already has %d events; use Continue", len(events))
	}
	max := r.MaxSteps
	if max == 0 {
		max = 8
	}
	st := NewState()
	start := Event{Type: EvRunStarted, System: system, Task: task, MaxSteps: max, By: r.StartedBy}
	if d, ok := r.Model.(Describer); ok {
		start.Provider, start.Model = d.Describe()
	}
	if err := r.emit(ctx, &st, start); err != nil {
		return st, err
	}
	return r.loop(ctx, st)
}

// Extend gives a PAUSED run more model decisions and continues it. The decision is logged with who made it:
// budget is never raised silently (week 3 S1 review, Q3).
func (r *Runner) Extend(ctx context.Context, extraSteps int, by string) (State, error) {
	release, err := r.lock(ctx)
	if err != nil {
		return State{}, err
	}
	defer release()
	st, err := r.rebuild(ctx)
	if err != nil {
		return st, err
	}
	if err := r.emit(ctx, &st, Event{Type: EvBudgetExtended, ExtraSteps: extraSteps, By: by}); err != nil {
		return st, err
	}
	return r.loop(ctx, st)
}

// Continue rebuilds the run from its log and drives it until it finishes, pauses, or errors.
func (r *Runner) Continue(ctx context.Context) (State, error) {
	release, err := r.lock(ctx)
	if err != nil {
		return State{}, err
	}
	defer release()
	st, err := r.rebuild(ctx)
	if err != nil {
		return st, err
	}
	return r.loop(ctx, st)
}

func (r *Runner) rebuild(ctx context.Context) (State, error) {
	events, err := r.Log.Read(ctx)
	if err != nil {
		return State{}, err
	}
	st, err := Rebuild(events)
	if err != nil {
		return st, err
	}
	if st.Status == StatusNew {
		return st, fmt.Errorf("empty log; use Start")
	}
	return st, nil
}

func (r *Runner) loop(ctx context.Context, st State) (State, error) {
	for {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		switch st.Status {
		case StatusFinished, StatusPaused, StatusAwaitingApproval:
			// paused waits for Extend, awaiting_approval for Approve/Reject: a human or policy decides, not
			// the loop. Nothing is held in memory: any process can pick the run up later from the log.
			return st, nil

		case StatusAnswered:
			if err := r.emit(ctx, &st, Event{Type: EvRunFinished, Stop: st.Stop, Text: st.Text}); err != nil {
				return st, err
			}

		case StatusAwaitingModel:
			if st.Step >= st.Budget {
				// Out of budget: PAUSE, don't finish (week 1 F12, week 3 W3-1). The log already holds everything
				// needed to continue; whether to continue is a separate, logged decision (Extend).
				if err := r.emit(ctx, &st, Event{Type: EvRunPaused, Reason: "budget_exhausted"}); err != nil {
					return st, err
				}
				r.logf("[paused: budget of %d steps exhausted; Extend to continue]", st.Budget)
				continue
			}
			d, err := r.Model.Decide(ctx, st.Messages, r.specs())
			if err != nil {
				return st, fmt.Errorf("step %d: model: %w", st.Step+1, err)
			}
			r.hook("after_model_call")
			msg := d.Message
			msg.Role = RoleAssistant
			if err := r.emit(ctx, &st, Event{Type: EvModelDecided, Step: st.Step + 1, Message: &msg,
				FinishReason: d.FinishReason, Usage: &d.Usage}); err != nil {
				return st, err
			}
			r.logf("[step %d/%d] %d in / %d out | finish=%s | calls=%d",
				st.Step, st.Budget, d.Usage.PromptTokens, d.Usage.CompletionTokens, d.FinishReason, len(msg.ToolCalls))
			r.hook("after_model_logged")

		case StatusExecuting:
			// One call at a time, in the model's order: after a crash, at most ONE call can be in the
			// "may have executed" state (week 3 S1 review, Q4).
			if err := r.step(ctx, &st, st.Pending[0]); err != nil {
				return st, err
			}

		default:
			return st, fmt.Errorf("unexpected status %s", st.Status)
		}
	}
}

// step runs one pending call: replay or conflict from the log if it's a repeated idempotent operation,
// otherwise tool_started → execute → tool_result.
func (r *Runner) step(ctx context.Context, st *State, c ToolCall) error {
	tool := r.find(c.Function.Name)
	var key, ph string
	if it, ok := tool.(IdempotentTool); ok && json.Valid([]byte(c.Function.Arguments)) {
		// If the call can't be identified, it runs without a key and fails on its own validation.
		if k, p, err := keyFor(r.scope(), it, json.RawMessage(c.Function.Arguments)); err == nil {
			key, ph = k, p
		}
	}

	if key != "" {
		if prev, seen := st.Effects[key]; seen {
			// The log has already seen this operation complete. Nothing runs.
			result, how := replay(prev.Result), "replayed"
			if prev.PayloadHash != ph {
				result, how = conflict(key), "CONFLICT"
			}
			if err := r.emit(ctx, st, Event{Type: EvToolStarted, CallID: c.ID, Tool: c.Function.Name, Args: c.Function.Arguments, Key: key, PayloadHash: ph}); err != nil {
				return err
			}
			if err := r.emit(ctx, st, Event{Type: EvToolResult, CallID: c.ID, Tool: c.Function.Name, Result: result, Key: key, PayloadHash: ph, Replayed: true}); err != nil {
				return err
			}
			r.logf("    %s(%.60s) -> %s from the log: %.80s", c.Function.Name, r.show(c.Function.Arguments), how, r.show(result))
			r.hook("after_result_logged")
			return nil
		}
	}

	if proceed, err := r.gate(ctx, st, tool, c, key); err != nil || !proceed {
		return err
	}

	if st.Started[c.ID] {
		// Started before a crash, no result logged: it MAY have executed. For an IdempotentTool, the same key
		// goes back into the tool, which returns the original outcome instead of acting twice.
		r.logf("    ↻ %s (%s) was started before a crash and may have executed; retrying with the same key", c.Function.Name, c.ID)
	}
	if err := r.emit(ctx, st, Event{Type: EvToolStarted, CallID: c.ID, Tool: c.Function.Name, Args: c.Function.Arguments, Key: key, PayloadHash: ph}); err != nil {
		return err
	}
	r.hook("before_tool_executed")
	result, err := r.execute(ctx, tool, c, key)
	r.hook("after_tool_executed") // THE point week 2 had to close: effect done, result not yet logged
	if err != nil {
		// In doubt: log nothing. The run is now exactly as after a crash here, and Continue retries the key.
		r.logf("    ? %s left in doubt: %s", c.Function.Name, r.show(err.Error()))
		return err
	}
	if err := r.emit(ctx, st, Event{Type: EvToolResult, CallID: c.ID, Tool: c.Function.Name, Result: result, Key: key, PayloadHash: ph}); err != nil {
		return err
	}
	r.logf("    %s(%.70s) -> %.90s", c.Function.Name, r.show(c.Function.Arguments), r.show(result))
	r.hook("after_result_logged")
	return nil
}

func (r *Runner) scope() string {
	if r.Scope != "" {
		return r.Scope
	}
	return "run" // per log: keys only need to be unique within this run's events
}

func (r *Runner) find(name string) Tool {
	for _, t := range r.Tools {
		if t.Spec().Name == name {
			return t
		}
	}
	return nil
}

// emit validates the event against the state machine, THEN persists it, THEN applies it. An event the
// state machine rejects is never written; an event that fails to write never changes the state.
func (r *Runner) emit(ctx context.Context, st *State, e Event) error {
	e.V = FormatVersion
	check := *st
	check.Started = copyMap(st.Started)
	check.Pending = append([]ToolCall(nil), st.Pending...)
	check.Messages = append([]Message(nil), st.Messages...)
	check.Effects = make(map[string]Outcome, len(st.Effects))
	for k, v := range st.Effects {
		check.Effects[k] = v
	}
	check.Approvals, check.ByKey = copyStrMap(st.Approvals), copyStrMap(st.ByKey)
	if st.Waiting != nil {
		w := *st.Waiting
		check.Waiting = &w
	}
	if err := check.Apply(e); err != nil {
		return fmt.Errorf("refusing to log invalid event: %w", err)
	}
	if err := r.Log.Append(ctx, e); err != nil {
		return fmt.Errorf("log append: %w", err)
	}
	*st = check
	return nil
}

func (r *Runner) specs() []ToolSpec {
	specs := make([]ToolSpec, len(r.Tools))
	for i, t := range r.Tools {
		specs[i] = t.Spec()
	}
	return specs
}

func (r *Runner) logf(f string, a ...any) {
	if r.Logf != nil {
		r.Logf(f, a...)
	}
}

func (r *Runner) hook(p string) {
	if r.Hook != nil {
		r.Hook(p)
	}
}

func errorJSON(err error) string {
	b, _ := json.Marshal(map[string]string{"error": err.Error()})
	return string(b)
}

func copyStrMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyMap(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
