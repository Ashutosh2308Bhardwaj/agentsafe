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
	release, err := r.lock(ctx)
	if err != nil {
		return State{}, err
	}
	defer release()
	st, err := r.rebuild(ctx)
	if err != nil {
		return st, err
	}
	if prev := st.ByKey[key]; prev == decision {
		r.logf("[%s already %s: nothing to do]", key, decision)
		return r.loop(ctx, st) // idempotent: the same decision twice changes nothing
	} else if prev != "" && prev != "requested" {
		return st, fmt.Errorf("%w: %s was already %s", ErrAlreadyDecided, key, prev)
	}
	if st.Status != StatusAwaitingApproval || st.Waiting == nil || st.Waiting.Key != key {
		return st, fmt.Errorf("%w: %s (status %s)", ErrNotWaiting, key, st.Status)
	}
	if err := r.authorize(ctx, &st, *st.Waiting, decision, by, reason); err != nil {
		return st, err
	}
	if err := r.emit(ctx, &st, Event{Type: EvApprovalDecided, CallID: st.Waiting.CallID, Key: key,
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
			r.logf("    ✗ %s(%s) refused: %s", c.Function.Name, clip(r.show(c.Function.Arguments), 60), r.show(verr.Error()))
			return false, r.emit(ctx, st, Event{Type: EvToolRefused, CallID: c.ID, Tool: c.Function.Name, Result: res, Key: key})
		}
	}
	g, gated := tool.(Gated)
	if !gated || !g.NeedsApproval(args) {
		return true, nil
	}
	if st.Kind == KindProxy {
		return r.proxyGate(ctx, st, g, c, key)
	}
	switch st.Approvals[c.ID] {
	case "approved":
		return true, nil
	case "rejected":
		res := errorJSON(fmt.Errorf("rejected by a human approver; nothing was done"))
		return false, r.emit(ctx, st, Event{Type: EvToolRefused, CallID: c.ID, Tool: c.Function.Name, Result: res, Key: key})
	}
	if key == "" {
		return false, fmt.Errorf("%w: gated tool %s must be an IdempotentTool: approvals are addressed by operation key", ErrConfig, c.Function.Name)
	}
	if st.ByKey[key] == "rejected" {
		// The model re-proposed an operation a human already rejected in this run (a new call id, the same
		// operation: FAILURES.md "it retries under a new identity"). A decision belongs to the operation, so
		// it's refused without asking again: re-asking until someone approves is how a rejection gets undone.
		// Found by TestPropertyRunnerUnderRandomCrashesPaysAtMostOnce.
		res := errorJSON(fmt.Errorf("this operation was rejected by a human approver earlier in this run; nothing was done. Don't propose it again"))
		r.logf("    ✗ %s: operation %s was already rejected; refused without asking again", c.Function.Name, key)
		return false, r.emit(ctx, st, Event{Type: EvToolRefused, CallID: c.ID, Tool: c.Function.Name, Result: res, Key: key})
	}
	sum, err := g.Summary(args)
	if err != nil {
		return false, err
	}
	b, _ := json.Marshal(sum)
	if err := r.emit(ctx, st, Event{Type: EvApprovalRequested, CallID: c.ID, Tool: c.Function.Name, Key: key, Summary: string(b)}); err != nil {
		return false, err
	}
	r.logf("[awaiting approval] %s %s\n    key=%s  →  Approve or Reject by key", c.Function.Name, r.show(string(b)), key)
	r.hook("approval_requested")
	return false, nil
}

// proxyGate is the approval gate in a proxy run, where the run never pauses: the decision belongs to the
// operation's key, and each call for it is answered by where that decision stands.
func (r *Runner) proxyGate(ctx context.Context, st *State, g Gated, c ToolCall, key string) (bool, error) {
	if key == "" {
		return false, fmt.Errorf("%w: gated tool %s must be an IdempotentTool: approvals are addressed by operation key", ErrConfig, c.Function.Name)
	}
	refuse := func(result string) (bool, error) {
		return false, r.emit(ctx, st, Event{Type: EvToolRefused, CallID: c.ID, Tool: c.Function.Name, Result: result, Key: key})
	}
	switch st.ByKey[key] {
	case "approved":
		return true, nil
	case "rejected":
		return refuse(errorJSON(fmt.Errorf("operation %s was rejected by a human approver; nothing was done. Don't propose it again", key)))
	case "requested":
		return refuse(pendingJSON(c.Function.Name, key))
	}
	sum, err := g.Summary(json.RawMessage(c.Function.Arguments))
	if err != nil {
		return false, err
	}
	b, _ := json.Marshal(sum)
	if err := r.emit(ctx, st, Event{Type: EvApprovalRequested, CallID: c.ID, Tool: c.Function.Name, Key: key, Summary: string(b)}); err != nil {
		return false, err
	}
	r.logf("[awaiting approval] %s %s\n    key=%s  →  approve or reject by key; the run carries on", c.Function.Name, r.show(string(b)), key)
	r.hook("approval_requested")
	return false, nil
}

// pendingJSON is what a call is told while its operation waits for a decision.
func pendingJSON(tool, key string) string {
	b, _ := json.Marshal(map[string]string{"status": "pending_approval", "key": key,
		"message": "Waiting for a human to approve " + tool + ". Nothing was done yet. Call it again with the same arguments later to get the outcome."})
	return string(b)
}
