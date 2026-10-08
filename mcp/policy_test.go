package mcp_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func chargePolicy(mode mcp.KeyMode) map[string]mcp.Policy {
	p := mcp.Policy{Identity: []string{"ticket_id"}, Key: mode}
	if mode == mcp.KeyArgument {
		p.KeyArgument = "idempotency_key"
	}
	return map[string]mcp.Policy{"charge": p}
}

func TestARepeatedOperationIsAnsweredFromTheLog(t *testing.T) {
	up := &fakeUpstream{}
	r := newPolicyRig(t, filepath.Join(t.TempDir(), "p.jsonl"), up, chargePolicy(mcp.KeyArgument))
	first := r.call(t, "charge", `{"ticket_id":"T-1","amount":"10.00"}`)
	again := r.call(t, "charge", `{"ticket_id":"T-1","amount":"10.00"}`)
	if up.charges != 1 || len(up.keys) != 1 || resultText(again) != resultText(first) {
		t.Fatalf("the second ask must be answered from the log, without reaching the upstream: charges=%d requests=%d %q vs %q",
			up.charges, len(up.keys), resultText(first), resultText(again))
	}
	if again.Meta[mcp.MetaPrefix+"replayed"] != true || first.Meta[mcp.MetaPrefix+"replayed"] != nil {
		t.Fatalf("a replay is marked as one, and only a replay: %v / %v", first.Meta, again.Meta)
	}
}

func TestTheSameOperationWithNewValuesIsAConflict(t *testing.T) {
	up := &fakeUpstream{}
	r := newPolicyRig(t, filepath.Join(t.TempDir(), "p.jsonl"), up, chargePolicy(mcp.KeyArgument))
	r.call(t, "charge", `{"ticket_id":"T-1","amount":"10.00"}`)
	res := r.call(t, "charge", `{"ticket_id":"T-1","amount":"12.00"}`)
	if !res.IsError || !strings.Contains(resultText(res), "conflict") || up.charges != 1 {
		t.Fatalf("a changed amount for the same ticket is a conflict, never a second charge: %q charges=%d", resultText(res), up.charges)
	}
}

func TestTheKeyReachesTheUpstreamAsThePolicySays(t *testing.T) {
	for _, c := range []struct {
		mode   mcp.KeyMode
		prefix string
	}{{mcp.KeyArgument, "arg:"}, {mcp.KeyMeta, "meta:"}, {mcp.KeyNone, ""}} {
		up := &fakeUpstream{}
		r := newPolicyRig(t, filepath.Join(t.TempDir(), "p.jsonl"), up, chargePolicy(c.mode))
		r.call(t, "charge", `{"ticket_id":"T-1","amount":"10.00"}`)
		got := up.keys[0]
		if !strings.HasPrefix(got, c.prefix) || (c.mode != mcp.KeyNone && len(strings.TrimPrefix(got, c.prefix)) != 32) || (c.mode == mcp.KeyNone && got != "") {
			t.Errorf("%s: the upstream received %q", c.mode, got)
		}
	}
}

// A model that invents its own idempotency key on every try must not turn a retry into a conflict, or get its
// key used: the proxy's key replaces it, and it's not part of the payload.
func TestTheModelsOwnKeyIsIgnored(t *testing.T) {
	up := &fakeUpstream{}
	r := newPolicyRig(t, filepath.Join(t.TempDir(), "p.jsonl"), up, chargePolicy(mcp.KeyArgument))
	r.call(t, "charge", `{"ticket_id":"T-1","amount":"10.00","idempotency_key":"model-made-1"}`)
	res := r.call(t, "charge", `{"ticket_id":"T-1","amount":"10.00","idempotency_key":"model-made-2"}`)
	if res.IsError || up.charges != 1 || strings.Contains(up.keys[0], "model-made") {
		t.Fatalf("one charge under the proxy's key: %q charges=%d keys=%v", resultText(res), up.charges, up.keys)
	}
}

func TestACallThatCantBeIdentifiedIsRefused(t *testing.T) {
	up := &fakeUpstream{}
	r := newPolicyRig(t, filepath.Join(t.TempDir(), "p.jsonl"), up, chargePolicy(mcp.KeyArgument))
	res := r.call(t, "charge", `{"amount":"10.00"}`)
	if !res.IsError || !strings.Contains(resultText(res), "missing ticket_id") || len(up.keys) != 0 {
		t.Fatalf("no identity, no key: refused before it reaches the upstream: %q requests=%d", resultText(res), len(up.keys))
	}
}

func TestPoliciesAreCheckedAgainstTheUpstream(t *testing.T) {
	for name, policies := range map[string]map[string]mcp.Policy{
		"an unknown approval rule":    {"charge": {Identity: []string{"ticket_id"}, Approval: "sometimes"}},
		"a misspelt tool":             {"chrage": {Identity: []string{"ticket_id"}}},
		"an argument it doesn't have": {"charge": {Identity: []string{"invoice_id"}}},
		"argument mode, no argument":  {"charge": {Identity: []string{"ticket_id"}, Key: mcp.KeyArgument}},
		"a key with no identity":      {"charge": {Key: mcp.KeyMeta}},
		"an unknown key mode":         {"charge": {Identity: []string{"ticket_id"}, Key: "header"}},
	} {
		_, err := openWith(t, policies)
		if !errors.Is(err, agentsafe.ErrConfig) {
			t.Errorf("%s: must be refused at startup, got %v", name, err)
		}
	}
}

// openWith opens a proxy on the fake upstream with these policies.
func openWith(t *testing.T, policies map[string]mcp.Policy) (*mcp.Proxy, error) {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	if _, err := (&fakeUpstream{}).server().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	upstream, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	return mcp.Open(ctx, upstream, &agentsafe.FileLog{Path: filepath.Join(t.TempDir(), "p.jsonl")}, policies)
}

// A policy without identity would make every call the same operation: the tool would work once, then refuse every
// other call as a conflict. It's refused at startup, whatever else it says.
func TestAPolicyThatProtectsAToolNeedsAnIdentity(t *testing.T) {
	for name, p := range map[string]mcp.Policy{
		"empty":          {},
		"key: none":      {Key: mcp.KeyNone},
		"approval never": {Approval: "never"},
		"a timeout":      {Timeout: mcp.Duration(time.Second)},
		"a meta key":     {Key: mcp.KeyMeta},
	} {
		_, err := openWith(t, passRest(map[string]mcp.Policy{"charge": p}))
		if !errors.Is(err, agentsafe.ErrConfig) || !strings.Contains(err.Error(), "identity is required") {
			t.Errorf("%s: must be refused at startup: %v", name, err)
		}
	}
}
