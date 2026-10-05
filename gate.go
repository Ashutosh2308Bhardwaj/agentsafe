package agentsafe

import (
	"context"
	"encoding/json"
	"fmt"
)

// Validator is a tool that can check a call against the source of truth BEFORE anything runs or anyone is
// asked. A failed validation resolves the call as tool_refused: never attempted, never shown to an approver.
// This is where grounding lives (week 1 F7, F11; week 2 W2-3, W2-6; week 3 W3-5): every consequential value
// must match the source, because once an idempotent write is recorded, a wrong value is locked in.
type Validator interface {
	Validate(ctx context.Context, args json.RawMessage) error
}

// Gated is a tool whose calls need a human decision before they run: the irreversible ones.
type Gated interface {
	// NeedsApproval reports whether this particular call must be approved.
	NeedsApproval(args json.RawMessage) bool
	// Summary is what the approver is shown. It is built from VALIDATED arguments, so the human signs off on
	// checked values, never on the model's raw proposal.
	Summary(args json.RawMessage) (any, error)
}

// Approve records an approval for the operation `key` (the payout's idempotency key, not the model's call
// id) and continues the run. Approving an already-approved operation is a no-op.
func (r *Runner) Approve(ctx context.Context, key, by string) (State, error) {
	return r.decide(ctx, key, "approved", by, "")
}

// Reject records a rejection; the call is resolved as tool_refused and the model is told why.
func (r *Runner) Reject(ctx context.Context, key, by, reason string) (State, error) {
	return r.decide(ctx, key, "rejected", by, reason)
}

func (r *Runner) decide(ctx context.Context, key, decision, by, reason string) (State, error) {
	release, err := r.lock()
	if err != nil {
		return State{}, err
	}
	defer release()
	st, err := r.rebuild()
	if err != nil {
		return st, err
	}
	if prev := st.ByKey[key]; prev == decision {
		r.logf("[%s already %s: nothing to do]", key, decision)
		return r.loop(ctx, st) // idempotent: the same decision twice changes nothing
	} else if prev != "" && prev != "requested" {
		return st, fmt.Errorf("%s was already %s; a decision can't be reversed by re-deciding", key, prev)
	}
	if st.Status != StatusAwaitingApproval || st.Waiting == nil || st.Waiting.Key != key {
		return st, fmt.Errorf("run is not waiting for approval of %s (status %s)", key, st.Status)
	}
	if err := r.emit(&st, Event{Type: EvApprovalDecided, CallID: st.Waiting.CallID, Key: key,
		Decision: decision, By: by, Reason: reason}); err != nil {
		return st, err
	}
	r.logf("[%s %s by %s]", key, decision, by)
	r.hook("approval_decided")
	return r.loop(ctx, st)
}

// gate runs validation and the approval gate for a call. It returns proceed=true when the call may execute.
// Otherwise it has already logged what happened (tool_refused or approval_requested).
func (r *Runner) gate(ctx context.Context, st *State, tool Tool, c ToolCall, key string) (proceed bool, err error) {
	args := json.RawMessage(c.Function.Arguments)
	if v, ok := tool.(Validator); ok && !st.Started[c.ID] {
		if verr := v.Validate(ctx, args); verr != nil {
			res := errorJSON(fmt.Errorf("refused before execution: %w", verr))
			r.logf("    ✗ %s(%.60s) refused: %v", c.Function.Name, c.Function.Arguments, verr)
			return false, r.emit(st, Event{Type: EvToolRefused, CallID: c.ID, Tool: c.Function.Name, Result: res, Key: key})
		}
	}
	g, gated := tool.(Gated)
	if !gated || !g.NeedsApproval(args) {
		return true, nil
	}
	switch st.Approvals[c.ID] {
	case "approved":
		return true, nil
	case "rejected":
		res := errorJSON(fmt.Errorf("rejected by a human approver; nothing was done"))
		return false, r.emit(st, Event{Type: EvToolRefused, CallID: c.ID, Tool: c.Function.Name, Result: res, Key: key})
	}
	if key == "" {
		return false, fmt.Errorf("gated tool %s must be an IdempotentTool: approvals are addressed by operation key", c.Function.Name)
	}
	sum, err := g.Summary(args)
	if err != nil {
		return false, err
	}
	b, _ := json.Marshal(sum)
	if err := r.emit(st, Event{Type: EvApprovalRequested, CallID: c.ID, Tool: c.Function.Name, Key: key, Summary: string(b)}); err != nil {
		return false, err
	}
	r.logf("[awaiting approval] %s %s\n    key=%s  →  Approve or Reject by key", c.Function.Name, b, key)
	r.hook("approval_requested")
	return false, nil
}
