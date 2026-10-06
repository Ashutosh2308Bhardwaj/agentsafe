package reconcile

import (
	"strings"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

type flag bool

// A payment in the system of record that the run never made (made by hand, by another system, by a bug)
// is a CRITICAL finding: money that moved outside the agent's approvals.
func TestReconcileFlagsWhatShouldntExist(t *testing.T) {
	rep := Audit(nil, []Effect{
		{ID: "payment:T9", Kind: "payment", Gated: true, Key: "manual-1"},
		{ID: "discrepancy:T3:extra", Kind: "discrepancy", Key: "k3"},
	}, nil, nil)
	var crit, errs int
	for _, f := range rep.Findings {
		if f.Check == "no-extras" {
			switch f.Severity {
			case Critical:
				crit++
			case Error:
				errs++
			case Warn:
				t.Errorf("an extra effect is never just a warning: %v", f)
			}
		}
	}
	if crit != 1 || errs != 1 {
		t.Fatalf("an extra payment is CRITICAL, an extra record an ERROR: %v", rep)
	}
}

// A gated effect with no key can't be tied to any approval: CRITICAL.
func TestReconcileGatedEffectWithoutAKey(t *testing.T) {
	rep := Audit([]Effect{{ID: "payment:T1", Gated: true}}, []Effect{{ID: "payment:T1", Gated: true}}, nil, nil)
	for _, f := range rep.Findings {
		if f.Severity == Critical && strings.Contains(f.Detail, "no idempotency key") {
			return
		}
	}
	t.Fatalf("want a CRITICAL 'no idempotency key' finding: %v", rep)
}

func TestSameOnBooleans(t *testing.T) {
	if eq, _ := Same(true, flag(true)); !eq {
		t.Error("a named bool compares by value")
	}
	if eq, why := Same(true, false); eq || !strings.Contains(why, "false, should be true") {
		t.Errorf("got %v %q", eq, why)
	}
}

// A log from a newer library can't be read, so nothing in it can be trusted: a CRITICAL finding.
func TestUnreadableLogIsAFinding(t *testing.T) {
	rep := Audit(nil, nil, []agentsafe.Event{{V: agentsafe.FormatVersion + 1}}, nil)
	if rep.Pass() || !strings.Contains(rep.String(), "can't be read") {
		t.Fatalf("the checker must flag an unreadable log, got:\n%s", rep)
	}
}
