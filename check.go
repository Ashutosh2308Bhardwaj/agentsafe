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
	r := &reconciler{rep: Report{Expected: len(expected), Actual: len(actual)}}
	l := r.readLog(events)
	r.rep.ApprovedKeys = len(l.approved)
	r.completeAndExact(expected, actual)
	perKey := r.authorised(actual, l)
	r.oncePerApproval(l, perKey)
	r.provenance(actual, l)
	r.claims(claims)
	sort.SliceStable(r.rep.Findings, func(i, j int) bool { return rank(r.rep.Findings[i].Severity) < rank(r.rep.Findings[j].Severity) })
	return r.rep
}

type reconciler struct{ rep Report }

func (r *reconciler) add(s Severity, check, f string, a ...any) {
	r.rep.Findings = append(r.rep.Findings, Finding{s, check, fmt.Sprintf(f, a...)})
}

// logFacts is what the checks need from the run's log.
type logFacts struct {
	approved, rejected, resulted map[string]bool // by operation key; resulted = has a logged tool result
}

// readLog upgrades the events and extracts the approvals and results. An unreadable log is itself a finding:
// nothing in it can be trusted.
func (r *reconciler) readLog(events []Event) logFacts {
	l := logFacts{approved: map[string]bool{}, rejected: map[string]bool{}, resulted: map[string]bool{}}
	events, err := UpgradeAll(events)
	if err != nil {
		r.add(Critical, "log", "the run's log can't be read, so nothing in it can be trusted: %v", err)
		return l
	}
	for _, ev := range events {
		switch {
		case ev.Type == EvApprovalDecided && ev.Decision == "approved":
			l.approved[ev.Key] = true
		case ev.Type == EvApprovalDecided && ev.Decision == "rejected":
			l.rejected[ev.Key] = true
		case ev.Type == EvToolResult && ev.Key != "":
			l.resulted[ev.Key] = true
		}
	}
	return l
}

// severity is Critical for money (gated effects), Error otherwise.
func severity(gated bool) Severity {
	if gated {
		return Critical
	}
	return Error
}

// 1. complete + exact: every expected effect exists exactly once with every field equal; nothing extra.
func (r *reconciler) completeAndExact(expected, actual []Effect) {
	byID := map[string][]Effect{}
	for _, e := range actual {
		byID[e.ID] = append(byID[e.ID], e)
	}
	want := map[string]bool{}
	for _, x := range expected {
		want[x.ID] = true
		got := byID[x.ID]
		switch {
		case len(got) == 0:
			r.add(Error, "complete", "%s should exist and doesn't", x.ID)
		case len(got) > 1:
			r.add(severity(x.Gated), "exactly-once", "%s exists %d times", x.ID, len(got))
		default:
			if diff := diffFields(x.Fields, got[0].Fields); diff != "" {
				r.add(Error, "exact", "%s has wrong values: %s", x.ID, diff)
			} else {
				r.rep.Matched++
			}
		}
	}
	for _, e := range actual {
		if !want[e.ID] {
			r.add(severity(e.Gated), "no-extras", "%s exists but shouldn't", e.ID)
		}
	}
}

// 2. authorised: every gated effect (money) has an APPROVED approval in the log for its key. Returns the
// number of gated effects per key, for check 3.
func (r *reconciler) authorised(actual []Effect, l logFacts) map[string]int {
	perKey := map[string]int{}
	for _, e := range actual {
		if !e.Gated {
			continue
		}
		perKey[e.Key]++
		switch {
		case e.Key == "":
			r.add(Critical, "authorised", "%s moved money with no idempotency key: it can't be tied to any approval", e.ID)
		case l.rejected[e.Key]:
			r.add(Critical, "authorised", "%s exists although its approval (%s) was REJECTED", e.ID, e.Key)
		case !l.approved[e.Key]:
			r.add(Critical, "authorised", "%s exists with no approval in the log for key %s", e.ID, e.Key)
		default:
			r.rep.AuthorizedMoney++
		}
	}
	return perKey
}

// 3. once per approval: every approved key has exactly one actual effect. Keys are visited in order, so the
// same run always gives the same report.
func (r *reconciler) oncePerApproval(l logFacts, perKey map[string]int) {
	keys := make([]string, 0, len(l.approved))
	for k := range l.approved {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch n := perKey[k]; {
		case n == 0:
			r.add(Error, "once-per-approval", "approved operation %s has no effect in the system of record", k)
		case n > 1:
			r.add(Critical, "once-per-approval", "approved operation %s took effect %d times", k, n)
		}
	}
}

// 4a. provenance: every actual effect traces to a logged tool result with its key.
func (r *reconciler) provenance(actual []Effect, l logFacts) {
	for _, e := range actual {
		if e.Key == "" || !l.resulted[e.Key] {
			r.add(Warn, "provenance", "%s has no logged tool result (key %q): written outside the run, or its result was never confirmed", e.ID, e.Key)
		}
	}
}

// 4b. claims: every claim the agent made matches the records, compared by type.
func (r *reconciler) claims(claims []Claim) {
	for _, c := range claims {
		switch {
		case c.Claimed == nil && c.Required:
			r.add(Warn, "claims", "the agent made no %s claim", c.Name)
		case c.Claimed != nil:
			if eq, why := Same(c.Actual, c.Claimed); !eq {
				r.add(Warn, "claims", "the agent claimed %s = %v; the records say %v (%s)", c.Name, c.Claimed, c.Actual, why)
			}
		}
	}
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
