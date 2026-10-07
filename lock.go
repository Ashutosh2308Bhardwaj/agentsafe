package agentsafe

import (
	"context"
	"errors"
	"fmt"
)

// ErrRunLocked is returned when another runner is driving the same run. Nothing was done; it is safe to
// retry later.
var ErrRunLocked = errors.New("agentsafe: run is locked by another runner")

// ErrNoLocker is returned when the Log can't guarantee a single driver per run and the Runner wasn't
// explicitly told to run without one (Runner.Unlocked).
var ErrNoLocker = errors.New("agentsafe: log does not implement Locker; set Runner.Unlocked to run without a single-driver guarantee")

// Locker is implemented by a Log that can guarantee only one runner drives a run at a time.
//
// Lock must not block: if the run is held, it returns an error wrapping ErrRunLocked. The returned function
// releases the lock. A lock MUST be released automatically if its holder dies (crash, kill -9), or a dead
// process would hold the run forever.
//
// Why it matters: without it, two processes resuming the same run (a scheduler retry, two workers) both ask
// the model and both act. Idempotency keys stop most duplicate effects, but the two runners interleave
// events in one log and the run's history stops making sense.
type Locker interface {
	Lock(ctx context.Context) (unlock func() error, err error)
}

// canLock reports whether a Log can take a lease. A Log whose Lock depends on what it wraps (a Journal over a
// store with or without one) says so with CanLock, so New refuses it up front instead of the first Start.
func canLock(l Log) bool {
	if _, ok := l.(Locker); !ok {
		return false
	}
	if c, ok := l.(interface{ CanLock() bool }); ok {
		return c.CanLock()
	}
	return true
}

// lock takes the run's lock for the duration of one Start / Continue / Extend / Approve / Reject call.
func (r *Runner) lock(ctx context.Context) (func(), error) {
	// Every entry point comes through here first: a Runner written as a struct literal gets the same checks
	// as one built with New, before it reads or writes anything.
	if err := r.Validate(); err != nil {
		return nil, err
	}
	lk, ok := r.Log.(Locker)
	if !ok {
		if r.Unlocked {
			return func() {}, nil
		}
		return nil, ErrNoLocker
	}
	unlock, err := lk.Lock(ctx)
	if err != nil {
		return nil, err
	}
	return func() {
		if err := unlock(); err != nil {
			r.logf("warning: releasing run lock: %v", err)
		}
	}, nil
}

func lockedErr(path string) error {
	return fmt.Errorf("%w (%s)", ErrRunLocked, path)
}
