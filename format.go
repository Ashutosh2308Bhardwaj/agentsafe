package agentsafe

import (
	"errors"
	"fmt"
)

// FormatVersion is the event format this library writes. Logs outlive the code that wrote them (a run
// paused for approval may be resumed weeks later by a newer version), so every event records the format
// it was written in, and readers upgrade older events before using them.
//
// Compatibility rules (see FORMAT.md):
//   - fields are only ever added, never renamed, removed, or given a new meaning;
//   - an optional field whose absence is harmless may be added without a version bump;
//   - any change that alters how an existing event must be read bumps FormatVersion and adds an upgrade
//     step below, plus a golden log in testdata/ that must keep rebuilding to the same state.
const FormatVersion = 1

// ErrNewerLogFormat is returned for an event written by a newer version of this library. It is refused, not
// guessed at: Go's JSON decoder silently drops unknown fields, and an old reader that skipped a field which
// mattered would act on a history that never happened.
var ErrNewerLogFormat = errors.New("agentsafe: event was written in a newer log format than this library understands")

// upgrades[v] converts an event from format v to v+1. Version 0 is every event written before versions
// existed (no "v" field); its shape is identical to version 1.
var upgrades = map[int]func(Event) (Event, error){
	0: func(e Event) (Event, error) { return e, nil },
}

// UpgradeAll brings every event to FormatVersion, or refuses the log at the first event it can't read.
func UpgradeAll(events []Event) ([]Event, error) {
	out := make([]Event, len(events))
	for i, e := range events {
		u, err := Upgrade(e)
		if err != nil {
			return nil, err
		}
		out[i] = u
	}
	return out, nil
}

// Upgrade brings one event to FormatVersion, or refuses it.
func Upgrade(e Event) (Event, error) {
	if e.V > FormatVersion {
		return e, fmt.Errorf("%w: event %d is v%d, this library reads up to v%d", ErrNewerLogFormat, e.Seq, e.V, FormatVersion)
	}
	for e.V < FormatVersion {
		up, ok := upgrades[e.V]
		if !ok {
			return e, fmt.Errorf("agentsafe: no upgrade from log format v%d (event %d)", e.V, e.Seq)
		}
		from := e.V
		var err error
		if e, err = up(e); err != nil {
			return e, fmt.Errorf("upgrading event %d from v%d: %w", e.Seq, from, err)
		}
		e.V = from + 1
	}
	return e, nil
}
