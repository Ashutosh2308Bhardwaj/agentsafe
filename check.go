package agentsafe

import (
	"fmt"
	"sort"
	"strings"
)

// Reconciliation: prove the agent did what it claimed, against the systems it acted on.
//
// A trace is the agent's account of a run (W4-1: it can't see the ledger or the gateway). The checker
// compares three INDEPENDENT sources:
//
//	Expected  what SHOULD exist, computed from the source data by plain code: no model involved
//	Log       what the agent's run says happened: tool results, approvals, its final claims
//	Actual    what DOES exist in the systems of record (ledger rows, gateway payments)
//
// Everything is counted from durable records, never from a process's memory (week 2 W2-5).

// Effect is one thing in a system of record (actual) or one thing that should be there (expected).
// ID is the BUSINESS identity, e.g. "discrepancy:T1004:amount_mismatch" or "payment:reissue:T1007":
// both sides can compute it without knowing the agent's internals. Key is the idempotency key the
// effect carries (actual effects only).
type Effect struct {
	ID     string
	Kind   string         // e.g. "discrepancy", "payment"
	Fields map[string]any // compared field by field (week 1 F11)
	Key    string
	Gated  bool // an effect that needs an approval (money)
}

// Claim is something the agent asserted, next to what the systems of record say.
type Claim struct {
	Name     string
	Claimed  any
	Actual   any
	Required bool // a finished run must make this claim
}

// Severity of a finding.
type Severity string

// Severities, most serious first.
const (
	Critical Severity = "CRITICAL" // money moved without authorisation, or moved twice
	Error    Severity = "ERROR"    // a wrong, missing or extra effect
	Warn     Severity = "WARN"     // the agent's account and the records disagree
)

// Finding is one discrepancy between the sources.
type Finding struct {
	Severity Severity
	Check    string
	Detail   string
}

// Report is the checker's verdict on one run.
type Report struct {
	Findings                      []Finding
	Expected, Actual, Matched     int
	ApprovedKeys, AuthorizedMoney int
}

// Pass is true only if nothing at all was found.
func (r Report) Pass() bool { return len(r.Findings) == 0 }

// Reconcile runs the four checks.
//
//  1. complete + exact: every expected effect exists exactly once with every field equal; nothing extra
//  2. authorised: every gated effect (money) has an APPROVED approval in the log for its key
//  3. once per approval: every approved key has exactly one actual effect
//  4. provenance + claims: every actual effect traces to a logged tool result with its key, and every
//     claim the agent made matches the records
func Reconcile(expected, actual []Effect, events []Event, claims []Claim) Report {
	rep := Report{Expected: len(expected), Actual: len(actual)}
	add := func(s Severity, check, f string, a ...any) {
		rep.Findings = append(rep.Findings, Finding{s, check, fmt.Sprintf(f, a...)})
	}
	if up, err := UpgradeAll(events); err != nil {
		add(Critical, "log", "the run's log can't be read, so nothing in it can be trusted: %v", err)
		events = nil
	} else {
		events = up
	}

	byID := map[string][]Effect{}
	for _, e := range actual {
		byID[e.ID] = append(byID[e.ID], e)
	}

	// 1. complete + exact
	want := map[string]bool{}
	for _, x := range expected {
		want[x.ID] = true
		got := byID[x.ID]
		switch {
		case len(got) == 0:
			add(Error, "complete", "%s should exist and doesn't", x.ID)
		case len(got) > 1:
			sev := Error
			if x.Gated {
				sev = Critical
			}
			add(sev, "exactly-once", "%s exists %d times", x.ID, len(got))
		default:
			if diff := diffFields(x.Fields, got[0].Fields); diff != "" {
				add(Error, "exact", "%s has wrong values: %s", x.ID, diff)
			} else {
				rep.Matched++
			}
		}
	}
	for _, e := range actual {
		if !want[e.ID] {
			sev := Error
			if e.Gated {
				sev = Critical
			}
			add(sev, "no-extras", "%s exists but shouldn't", e.ID)
		}
	}

	// From the log: approvals, and every key that has a logged executed result.
	approved, rejected, resulted := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, ev := range events {
		switch {
		case ev.Type == EvApprovalDecided && ev.Decision == "approved":
			approved[ev.Key] = true
		case ev.Type == EvApprovalDecided && ev.Decision == "rejected":
			rejected[ev.Key] = true
		case ev.Type == EvToolResult && ev.Key != "":
			resulted[ev.Key] = true
		}
	}
	rep.ApprovedKeys = len(approved)

	// 2. authorised, and 3. once per approval
	perKey := map[string]int{}
	for _, e := range actual {
		if !e.Gated {
			continue
		}
		perKey[e.Key]++
		switch {
		case e.Key == "":
			add(Critical, "authorised", "%s moved money with no idempotency key: it can't be tied to any approval", e.ID)
		case rejected[e.Key]:
			add(Critical, "authorised", "%s exists although its approval (%s) was REJECTED", e.ID, e.Key)
		case !approved[e.Key]:
			add(Critical, "authorised", "%s exists with no approval in the log for key %s", e.ID, e.Key)
		default:
			rep.AuthorizedMoney++
		}
	}
	for k := range approved {
		switch n := perKey[k]; {
		case n == 0:
			add(Error, "once-per-approval", "approved operation %s has no effect in the system of record", k)
		case n > 1:
			add(Critical, "once-per-approval", "approved operation %s took effect %d times", k, n)
		}
	}

	// 4. provenance + claims
	for _, e := range actual {
		if e.Key == "" || !resulted[e.Key] {
			add(Warn, "provenance", "%s has no logged tool result (key %q): written outside the run, or its result was never confirmed", e.ID, e.Key)
		}
	}
	for _, c := range claims {
		switch {
		case c.Claimed == nil && c.Required:
			add(Warn, "claims", "the agent made no %s claim", c.Name)
		case c.Claimed != nil:
			if eq, why := Same(c.Actual, c.Claimed); !eq {
				add(Warn, "claims", "the agent claimed %s = %v; the records say %v (%s)", c.Name, c.Claimed, c.Actual, why)
			}
		}
	}

	sort.SliceStable(rep.Findings, func(i, j int) bool { return rank(rep.Findings[i].Severity) < rank(rep.Findings[j].Severity) })
	return rep
}

// String renders the report for a terminal.
func (r Report) String() string {
	var b strings.Builder
	verdict := "PASS"
	if !r.Pass() {
		verdict = "FAIL"
	}
	fmt.Fprintf(&b, "reconciliation: %s  (expected %d, actual %d, exact matches %d, approved operations %d, authorised payments %d)\n",
		verdict, r.Expected, r.Actual, r.Matched, r.ApprovedKeys, r.AuthorizedMoney)
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "  %-8s [%s] %s\n", f.Severity, f.Check, f.Detail)
	}
	return b.String()
}

func rank(s Severity) int {
	switch s {
	case Critical:
		return 0
	case Error:
		return 1
	default: // Warn
		return 2
	}
}

// diffFields compares the expected fields with the actual ones (Same). Only expected fields are compared: a
// system of record may hold more columns than the check cares about. A field that's absent is "missing",
// never equal to null.
func diffFields(want, got map[string]any) string {
	var diffs []string
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s is missing (should be %v)", k, norm(w).show))
			continue
		}
		if eq, why := Same(w, g); !eq {
			diffs = append(diffs, k+": "+why)
		}
	}
	sort.Strings(diffs)
	return strings.Join(diffs, "; ")
}
