package reconcile

import (
	"strings"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

func eff(id, kind, key string, gated bool, fields map[string]any) Effect {
	return Effect{ID: id, Kind: kind, Key: key, Gated: gated, Fields: fields}
}

var (
	wantRow = eff("discrepancy:T1004:amount_mismatch", "discrepancy", "", false, map[string]any{"ledger": 4200, "bank": 4020})
	wantPay = eff("payment:reissue:T1007", "payment", "", true, map[string]any{"payee": "Imran Khan", "amount": 11000})
	gotRow  = eff("discrepancy:T1004:amount_mismatch", "discrepancy", "k1", false, map[string]any{"ledger": 4200, "bank": 4020})
	gotPay  = eff("payment:reissue:T1007", "payment", "kp", true, map[string]any{"payee": "Imran Khan", "amount": 11000})
	okLog   = []agentsafe.Event{
		{Type: agentsafe.EvToolResult, Key: "k1"},
		{Type: agentsafe.EvApprovalDecided, Key: "kp", Decision: "approved", By: "ops"},
		{Type: agentsafe.EvToolResult, Key: "kp"},
	}
)

func findings(r Report) string {
	var s []string
	for _, f := range r.Findings {
		s = append(s, string(f.Severity)+" "+f.Check+": "+f.Detail)
	}
	return strings.Join(s, "\n")
}

func TestCleanRunPasses(t *testing.T) {
	r := Audit([]Effect{wantRow, wantPay}, []Effect{gotRow, gotPay}, okLog, []Claim{{Name: "total", Claimed: 1, Actual: 1}})
	if !r.Pass() {
		t.Fatalf("want pass:\n%s", findings(r))
	}
}

func TestEachFailureIsCaught(t *testing.T) {
	wrongAmount := gotRow
	wrongAmount.Fields = map[string]any{"ledger": 7300, "bank": 4020}
	cases := map[string]struct {
		actual []Effect
		log    []agentsafe.Event
		claims []Claim
		want   string
	}{
		"missing write (F14)":             {[]Effect{gotPay}, okLog, nil, "ERROR complete"},
		"duplicate write (F13)":           {[]Effect{gotRow, gotRow, gotPay}, okLog, nil, "ERROR exactly-once"},
		"wrong value (F11)":               {[]Effect{wrongAmount, gotPay}, okLog, nil, "ERROR exact"},
		"double payment":                  {[]Effect{gotRow, gotPay, gotPay}, okLog, nil, "CRITICAL"},
		"payment with no approval":        {[]Effect{gotRow, gotPay}, okLog[:1], nil, "CRITICAL authorised"},
		"payment despite rejection":       {[]Effect{gotRow, gotPay}, []agentsafe.Event{okLog[0], {Type: agentsafe.EvApprovalDecided, Key: "kp", Decision: "rejected", By: "ops"}}, nil, "REJECTED"},
		"approved but never paid":         {[]Effect{gotRow}, okLog, nil, "once-per-approval"},
		"effect not traceable to the run": {[]Effect{gotRow, gotPay}, okLog[1:], nil, "WARN provenance"},
		"claim disagrees with records":    {[]Effect{gotRow, gotPay}, okLog, []Claim{{Name: "total", Claimed: 2, Actual: 1}}, "WARN claims"},
		"no claim at all":                 {[]Effect{gotRow, gotPay}, okLog, []Claim{{Name: "total", Required: true, Actual: 1}}, "made no total claim"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := Audit([]Effect{wantRow, wantPay}, c.actual, c.log, c.claims)
			if r.Pass() || !strings.Contains(findings(r), c.want) {
				t.Fatalf("want a finding containing %q, got:\n%s", c.want, findings(r))
			}
		})
	}
}
