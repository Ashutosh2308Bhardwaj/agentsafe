package agentsafe

import "errors"

// Errors a caller can act on, matched with errors.Is. Errors that belong to one feature live next to it:
// ErrConflict (journal.go), ErrRunLocked and ErrNoLocker (lock.go), ErrNotAuthorized and ErrNoAuthorizer
// (authz.go), ErrOutcomeUnknown and ErrInDoubt (exec.go), ErrSealed and ErrCannotOpen (seal.go),
// ErrTampered (chain.go), ErrCorruptLog (eventlog.go), ErrNewerLogFormat (format.go).
var (
	// ErrInvalidTransition: an event isn't allowed in the run's current state. From Rebuild, the log holds an
	// impossible history (a result for a call never started, an approval skipped, ...). From a Runner method,
	// the request doesn't fit the run's state (Extend on a run that isn't paused); nothing was written.
	ErrInvalidTransition = errors.New("agentsafe: event not allowed in the run's current state")

	// ErrRunExists: Start on a log that already has events. Use Continue.
	ErrRunExists = errors.New("agentsafe: the log already holds a run; use Continue")

	// ErrNoRun: Continue on an empty log. Use Start.
	ErrNoRun = errors.New("agentsafe: the log is empty; use Start")

	// ErrAlreadyDecided: Approve or Reject of an operation that was already decided the other way. Deciding the
	// same way twice is a no-op, not an error.
	ErrAlreadyDecided = errors.New("agentsafe: operation already decided; a decision can't be reversed")

	// ErrNotWaiting: Approve or Reject of a key the run isn't waiting on.
	ErrNotWaiting = errors.New("agentsafe: the run isn't waiting for approval of this operation")

	// ErrConfig: the Runner is set up in a way that can't work (see New).
	ErrConfig = errors.New("agentsafe: invalid configuration")
)
