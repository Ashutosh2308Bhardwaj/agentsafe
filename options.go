package agentsafe

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// An Option configures a Runner built by New.
type Option func(*Runner)

// New builds a Runner and checks its configuration, so mistakes surface at startup rather than in the middle
// of a run: a gated tool that isn't idempotent, approvals with no policy, a log with no lease, duplicate tool
// names. Every problem is reported at once, wrapped in ErrConfig.
//
//	r, err := agentsafe.New(model, &agentsafe.FileLog{Path: "run.jsonl"},
//		agentsafe.WithTools(payout),
//		agentsafe.WithAuthorizer(agentsafe.AllowList("ops@example.com")),
//		agentsafe.WithStartedBy("scheduler"))
//
// A Runner can also be written as a struct literal; call Validate before using it.
func New(model Model, log Log, opts ...Option) (*Runner, error) {
	r := &Runner{Model: model, Log: log}
	for _, o := range opts {
		o(r)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// WithTools adds tools the model may call.
func WithTools(tools ...Tool) Option { return func(r *Runner) { r.Tools = append(r.Tools, tools...) } }

// WithMaxSteps sets the step budget for new runs (default 8). Resumed runs keep the budget in their log.
func WithMaxSteps(n int) Option { return func(r *Runner) { r.MaxSteps = n } }

// WithScope sets the idempotency scope: runs with the same scope share operation keys (e.g. a hash of the
// input files, so re-running the same batch can't repeat its payouts).
func WithScope(scope string) Option { return func(r *Runner) { r.Scope = scope } }

// WithAuthorizer sets who may approve or reject gated calls.
func WithAuthorizer(a Authorizer) Option { return func(r *Runner) { r.Authorizer = a } }

// WithAnyApprover lets anyone decide gated calls: an explicit, visible choice.
func WithAnyApprover() Option { return func(r *Runner) { r.AnyApprover = true } }

// WithStartedBy records who starts runs (an identity your system verified), for NotRequester.
func WithStartedBy(identity string) Option { return func(r *Runner) { r.StartedBy = identity } }

// WithToolTimeout bounds every tool call (a TimeoutTool sets its own).
func WithToolTimeout(d time.Duration) Option { return func(r *Runner) { r.ToolTimeout = d } }

// WithToolRetries sets how an IdempotentTool whose outcome is unknown is retried with the same key: up to
// attempts tries, waiting backoff (doubling) between them.
func WithToolRetries(attempts int, backoff time.Duration) Option {
	return func(r *Runner) { r.ToolAttempts, r.ToolBackoff = attempts, backoff }
}

// WithLogf sets progress output (nil = silent).
func WithLogf(f func(format string, args ...any)) Option { return func(r *Runner) { r.Logf = f } }

// WithRedactor masks arguments, results and summaries in progress output.
func WithRedactor(rd Redactor) Option { return func(r *Runner) { r.Redact = rd } }

// WithoutLease runs on a Log that has no lease (Locker): a visible choice to rely on fencing (ErrConflict)
// alone, accepting that two runners may both call the model before one of them is stopped.
func WithoutLease() Option { return func(r *Runner) { r.Unlocked = true } }

// WithHook sets a chaos/test hook, called at named points of a run (e.g. to kill the process there).
func WithHook(h func(point string)) Option { return func(r *Runner) { r.Hook = h } }

// toolName is what model APIs accept as a function name (OpenAI, Anthropic and Gemini agree on this set).
var toolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Validate reports every configuration problem at once, wrapped in ErrConfig. New calls it.
func (r *Runner) Validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if r.Model == nil {
		bad("no Model")
	}
	if r.Log == nil {
		bad("no Log")
	} else if _, ok := r.Log.(Locker); !ok && !r.Unlocked {
		bad("the Log has no lease (Locker); use a Log that has one, or WithoutLease to rely on fencing alone")
	}
	if r.MaxSteps < 0 || r.ToolTimeout < 0 || r.ToolAttempts < 0 || r.ToolBackoff < 0 {
		bad("MaxSteps, ToolTimeout, ToolAttempts and ToolBackoff can't be negative")
	}
	if r.Authorizer != nil && r.AnyApprover {
		bad("both an Authorizer and AnyApprover: choose one")
	}
	seen := map[string]bool{}
	gated := false
	for i, t := range r.Tools {
		if t == nil {
			bad("tool %d is nil", i)
			continue
		}
		name := t.Spec().Name
		switch {
		case !toolName.MatchString(name):
			bad("tool name %q: model APIs accept 1-64 of a-z A-Z 0-9 _ -", name)
		case seen[name]:
			bad("two tools are named %q", name)
		}
		seen[name] = true
		if c, ok := t.(interface{ configErr() error }); ok && c.configErr() != nil {
			errs = append(errs, c.configErr())
		}
		if _, ok := t.(Gated); ok {
			gated = true
			if _, ok := t.(IdempotentTool); !ok {
				bad("tool %q needs approval but isn't an IdempotentTool: approvals are addressed by operation key", name)
			}
		}
	}
	if gated && r.Authorizer == nil && !r.AnyApprover {
		bad("tools need approval but nobody may give it: set WithAuthorizer, or WithAnyApprover (%w)", ErrNoAuthorizer)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrConfig, errors.Join(errs...))
	}
	return nil
}
