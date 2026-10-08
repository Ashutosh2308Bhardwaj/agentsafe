package mcp_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
)

var financeOnly = agentsafe.WithAuthorizer(agentsafe.All(agentsafe.AllowList("finance"), agentsafe.NotRequester()))

// The whole loop through the proxy: the agent is told "pending" (not an error, nothing done) and can carry
// on; a person approves from another process; the agent's next call is charged once.
func TestAnApprovalThroughTheProxy(t *testing.T) {
	up := &fakeUpstream{}
	log := filepath.Join(t.TempDir(), "p.jsonl")
	policy := chargePolicy(mcp.KeyArgument)
	p := policy["charge"]
	p.Approval = "always"
	policy["charge"] = p
	r := newPolicyRig(t, log, up, policy, financeOnly)
	const ask = `{"ticket_id":"T-77","amount":"1200.00"}`

	first := r.call(t, "charge", ask)
	status, _ := first.StructuredContent.(map[string]any)
	key, _ := status["key"].(string)
	if first.IsError || status["status"] != "pending_approval" || len(key) != 32 || up.charges != 0 {
		t.Fatalf("pending, with its key, not an error, nothing charged: %+v", first)
	}
	if res := r.call(t, "get_seats", `{}`); res.IsError {
		t.Fatalf("other tools go on while it waits: %+v", res)
	}

	approver, err := agentsafe.OpenGateway(context.Background(), &agentsafe.FileLog{Path: log}, financeOnly)
	if err != nil {
		t.Fatal(err)
	}
	if err := approver.Approve(context.Background(), key, "finance"); err != nil {
		t.Fatal(err)
	}
	if res := r.call(t, "charge", ask); res.IsError || resultText(res) != "charged 1200.00 (charge 1)" {
		t.Fatalf("approved: the next call is charged: %+v", res)
	}
	if res := r.call(t, "charge", ask); resultText(res) != "charged 1200.00 (charge 1)" || up.charges != 1 {
		t.Fatalf("once: %q charges=%d", resultText(res), up.charges)
	}
}

func TestApprovalNeedsAnIdentity(t *testing.T) {
	policy := map[string]mcp.Policy{"charge": {Approval: "always"}}
	if _, err := openWith(t, policy); err == nil {
		t.Fatal("approval without identity fields must be refused at startup: a decision is addressed by key")
	}
}
