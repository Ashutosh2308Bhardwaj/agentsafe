package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// ErrNotAuthorized is returned when the Authorizer refuses a decision. The attempt is recorded in the log
// (approval_denied) and the run keeps waiting.
var ErrNotAuthorized = errors.New("agentsafe: not authorized to decide this approval")

// ErrNoAuthorizer is returned when a decision is made on a run with no Authorizer and without an explicit
// Runner.AnyApprover opt-out: by default, a gate that anyone can open is refused.
var ErrNoAuthorizer = errors.New("agentsafe: no Authorizer configured; set Runner.Authorizer, or Runner.AnyApprover to let anyone decide")

// Approval is what an Authorizer decides on.
//
// Authentication is your system's job: By must be an identity you have already verified (SSO, API key,
// mTLS). agentsafe only authorizes: is this identity allowed to make this decision about this operation?
type Approval struct {
	By           string          // the verified identity making the decision
	Decision     string          // "approved" or "rejected"
	Reason       string          // the decider's reason (rejections)
	Tool         string          // the gated tool
	Key          string          // the operation's idempotency key
	Summary      json.RawMessage // what the approver was shown: the VALIDATED arguments (payee, amount, ...)
	RunStartedBy string          // who started the run (Runner.StartedBy), for separation of duties
}

// An Authorizer allows (nil) or refuses (an error saying why) one approval decision.
type Authorizer interface {
	Authorize(ctx context.Context, a Approval) error
}

// AuthorizerFunc adapts a function to an Authorizer, for custom policies such as amount thresholds.
type AuthorizerFunc func(ctx context.Context, a Approval) error

// Authorize implements Authorizer.
func (f AuthorizerFunc) Authorize(ctx context.Context, a Approval) error { return f(ctx, a) }

// AllowList allows decisions only by the listed identities.
func AllowList(identities ...string) Authorizer {
	return AuthorizerFunc(func(_ context.Context, a Approval) error {
		if !slices.Contains(identities, a.By) {
			return fmt.Errorf("%q is not on the approver list", a.By)
		}
		return nil
	})
}

// NotRequester enforces separation of duties (maker-checker): whoever started the run can't decide its
// approvals. A run started without Runner.StartedBy has no recorded requester, and is refused: the rule
// can't be checked, so it isn't silently skipped.
func NotRequester() Authorizer {
	return AuthorizerFunc(func(_ context.Context, a Approval) error {
		switch a.RunStartedBy {
		case "":
			return errors.New("the run has no recorded requester (Runner.StartedBy), so separation of duties can't be checked")
		case a.By:
			return fmt.Errorf("%q started this run and can't approve its own operations", a.By)
		}
		return nil
	})
}

// All allows a decision only if every Authorizer allows it; the first refusal is returned.
func All(authorizers ...Authorizer) Authorizer {
	return AuthorizerFunc(func(ctx context.Context, a Approval) error {
		for _, z := range authorizers {
			if err := z.Authorize(ctx, a); err != nil {
				return err
			}
		}
		return nil
	})
}

// authorize checks one decision. A refusal is recorded as approval_denied (an audit fact) before returning.
func (r *Runner) authorize(ctx context.Context, st *State, decision, by, reason string) error {
	if r.Authorizer == nil {
		if r.AnyApprover {
			return nil
		}
		return ErrNoAuthorizer
	}
	a := Approval{By: by, Decision: decision, Reason: reason, Tool: st.Waiting.Tool, Key: st.Waiting.Key,
		Summary: json.RawMessage(st.Waiting.Summary), RunStartedBy: st.StartedBy}
	err := r.Authorizer.Authorize(ctx, a)
	if err == nil {
		return nil
	}
	if lerr := r.emit(st, Event{Type: EvApprovalDenied, CallID: st.Waiting.CallID, Key: st.Waiting.Key,
		Decision: decision, By: by, Reason: err.Error()}); lerr != nil {
		return fmt.Errorf("recording a refused approval: %w (refusal: %w)", lerr, err)
	}
	r.logf("[%s: %s by %s DENIED: %v]", st.Waiting.Key, decision, by, err)
	return fmt.Errorf("%w: %w", ErrNotAuthorized, err)
}
