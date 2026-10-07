package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"time"
)

// A tool call ends in one of two ways: a KNOWN outcome (a result, or an error saying it didn't happen), or an
// UNKNOWN one: it timed out, panicked, or said so itself. A timed-out payout may still have been charged, so
// an unknown outcome is never recorded as a failure:
//
//	IdempotentTool   nothing is logged; retried with the SAME key (the tool dedupes) up to ToolAttempts;
//	                 still unknown → ErrInDoubt, and the run is left as after a crash: Continue retries it.
//	                 Logging "timed out" would be worse than nothing: the log replays a key's result, so every
//	                 later attempt would be told "failed" about a payment that went through.
//	other tools      can't be retried safely; the model gets an error that says the outcome is unknown.
//	panics           a bug: reported, never retried automatically (it would most likely panic again).

// ErrOutcomeUnknown means a call may or may not have taken effect. Tools return it (wrapped) when they can't
// tell, e.g. a gateway answered 504 after the request was sent. Timeouts and panics are reported with it.
var ErrOutcomeUnknown = errors.New("agentsafe: outcome unknown: the call may or may not have taken effect")

// ErrInDoubt is returned when an IdempotentTool's outcome is still unknown after its attempts. Nothing was
// logged for it: the run stays resumable, and Continue retries it with the same key.
var ErrInDoubt = errors.New("agentsafe: operation in doubt; left unresolved, Continue retries it with the same key")

// PanicError is a tool panic, recovered. It matches ErrOutcomeUnknown: the tool may have acted before panicking.
type PanicError struct {
	Tool  string
	Value any
	Stack []byte
}

func (p *PanicError) Error() string { return fmt.Sprintf("tool %s panicked: %v", p.Tool, p.Value) }

// Is makes errors.Is(p, ErrOutcomeUnknown) true.
func (p *PanicError) Is(target error) bool { return target == ErrOutcomeUnknown }

// TimeoutTool lets a tool set its own per-call timeout, overriding Runner.ToolTimeout. 0 = the Runner's.
type TimeoutTool interface {
	Timeout() time.Duration
}

func (r *Runner) timeoutFor(tool Tool) time.Duration {
	if t, ok := tool.(TimeoutTool); ok && t.Timeout() > 0 {
		return t.Timeout()
	}
	return r.ToolTimeout
}

// invoke calls the tool once, bounded by its timeout, with panics recovered. A non-nil error matching
// ErrOutcomeUnknown means the outcome is unknown; any other error is the tool's known failure.
//
// Go can't stop a goroutine: a tool that ignores ctx keeps running after its timeout, and may still act. The
// runner stops waiting for it, treats the outcome as unknown, and (for an IdempotentTool) may call again with
// the same key while it's still running: tools must honour ctx, and dedupe concurrent calls with one key.
func (r *Runner) invoke(ctx context.Context, tool Tool, c ToolCall, key string) (any, error) {
	timeout := r.timeoutFor(tool)
	cctx, cancel := ctx, context.CancelFunc(func() {})
	if timeout > 0 {
		cctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	type outcome struct {
		out any
		err error
	}
	done := make(chan outcome, 1) // buffered: an abandoned call can still finish without blocking forever
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- outcome{err: &PanicError{Tool: c.Function.Name, Value: p, Stack: debug.Stack()}}
			}
		}()
		args := json.RawMessage(c.Function.Arguments)
		var o outcome
		if it, ok := tool.(IdempotentTool); ok && key != "" {
			o.out, o.err = it.CallWithKey(cctx, key, args)
		} else {
			o.out, o.err = tool.Call(cctx, args)
		}
		done <- o
	}()

	var o outcome
	select {
	case o = <-done:
	case <-cctx.Done():
		select {
		case o = <-done: // finished at the same moment: a known outcome beats a guess (an optimisation, not
			// a safety rule: without it, the call is just retried with the same key)
		default:
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%w: %s was still running when the run was cancelled (%w)", ErrOutcomeUnknown, c.Function.Name, ctx.Err())
			}
			return nil, fmt.Errorf("%w: %s did not return within %s", ErrOutcomeUnknown, c.Function.Name, timeout)
		}
	}
	if o.err != nil && (errors.Is(o.err, context.DeadlineExceeded) || errors.Is(o.err, context.Canceled)) &&
		!errors.Is(o.err, ErrOutcomeUnknown) {
		// The tool gave up on its context: whatever it had sent may still land.
		return nil, fmt.Errorf("%w: %s stopped on its context: %w", ErrOutcomeUnknown, c.Function.Name, o.err)
	}
	return o.out, o.err
}

// execute runs a call to a known outcome, which it returns as the result JSON for the model. An error means
// the outcome of an IdempotentTool call is still unknown (ErrInDoubt): the caller must log nothing.
func (r *Runner) execute(ctx context.Context, tool Tool, c ToolCall, key string) (string, error) {
	if tool == nil {
		return errorJSON(fmt.Errorf("no tool named %q", c.Function.Name)), nil
	}
	if !json.Valid([]byte(c.Function.Arguments)) {
		return errorJSON(fmt.Errorf("arguments are not valid JSON: %.80s", c.Function.Arguments)), nil
	}
	attempts, backoff, idempotent := r.retryPolicy(tool, key)

	var err error
	for attempt := 1; ; attempt++ {
		var out any
		out, err = r.invoke(ctx, tool, c, key)
		if !errors.Is(err, ErrOutcomeUnknown) {
			return resultJSON(out, err), nil
		}
		var p *PanicError
		if errors.As(err, &p) {
			r.logf("    ‼ %v\n%s", p, p.Stack)
		}
		if !idempotent {
			return errorJSON(fmt.Errorf("%w. It may have taken effect: check before trying again", err)), nil
		}
		if p != nil || attempt >= attempts || ctx.Err() != nil {
			break
		}
		r.logf("    ? %s: %s; retrying with the same key (attempt %d/%d)", c.Function.Name, r.show(err.Error()), attempt+1, attempts)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
		}
		backoff *= 2
	}
	return "", fmt.Errorf("%w: %s key %s: %w", ErrInDoubt, c.Function.Name, key, err)
}

// retryPolicy says how often a call may be tried. Only an idempotent tool with a key is retried: the key makes
// a second attempt safe. Anything else gets one attempt.
func (r *Runner) retryPolicy(tool Tool, key string) (attempts int, backoff time.Duration, idempotent bool) {
	if _, ok := tool.(IdempotentTool); !ok || key == "" {
		return 1, r.ToolBackoff, false
	}
	attempts, backoff = r.ToolAttempts, r.ToolBackoff
	if attempts <= 0 {
		attempts = 3
	}
	if backoff <= 0 {
		backoff = 200 * time.Millisecond
	}
	return attempts, backoff, true
}

// resultJSON is what the model is told about a known outcome: the result, or the error (it didn't happen).
func resultJSON(out any, err error) string {
	if err != nil {
		return errorJSON(err)
	}
	b, merr := json.Marshal(out)
	if merr != nil {
		return errorJSON(fmt.Errorf("result not serialisable: %w", merr))
	}
	return string(b)
}
