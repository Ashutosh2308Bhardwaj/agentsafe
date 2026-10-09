// Package mcptest checks an MCP server against what agentsafe-mcp relies on. OneEffect checks any tool that
// changes something: one call makes exactly one effect. A server that does it twice inside one call (two
// tickets, two charges) can't be protected by any proxy: the proxy sees one call. A policy with key "meta" or
// "argument" is the operator's claim that the server deduplicates on that key: after a crash or a timeout, the
// proxy retries with it. SameKey checks the claim. Run it against a sandbox of the real server (a test account,
// a scratch database), not a mock: the point is to catch the real server's races.
//
//	func TestChargeHonoursItsKey(t *testing.T) {
//		policy := mcp.Policy{Identity: []string{"ticket_id"}, Key: mcp.KeyArgument, KeyArgument: "idempotency_key"}
//		mcptest.SameKey(t, sandbox, "charge", policy, json.RawMessage(`{"ticket_id":"T-1","amount":"1.00"}`),
//			func() int { return sandboxChargeCount() })
//	}
package mcptest

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/tooltest"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// SameKey fails t unless the upstream's tool, called the way agentsafe-mcp calls it under policy, performs its
// effect at most once per key: one call after another (a retry after a crash) or many at the same moment (a
// retry after a timeout while the first is still running), every caller getting the same answer. effects counts
// the real effects (charges made, rows written); args are the arguments of one operation.
func SameKey(t testing.TB, upstream *sdk.ClientSession, tool string, policy mcp.Policy, args json.RawMessage, effects func() int) {
	t.Helper()
	if err := CheckSameKey(context.Background(), upstream, tool, policy, args, effects); err != nil {
		t.Fatal(err)
	}
}

// CheckSameKey is SameKey returning an error instead of failing a test.
func CheckSameKey(ctx context.Context, upstream *sdk.ClientSession, tool string, policy mcp.Policy, args json.RawMessage, effects func() int) error {
	keyed, err := mcp.KeyedTool(ctx, upstream, tool, policy)
	if err != nil {
		return err
	}
	return tooltest.CheckSameKey(ctx, keyed, args, effects)
}

// OneEffect fails t unless one call to the upstream's tool, made as agentsafe-mcp makes it under policy (with a
// fresh key where the policy sends one), makes exactly one effect. It's the check for every tool that changes
// something, keyed or not: none (the call did nothing, or effects counts the wrong thing) or two (the server
// repeats the effect inside one call) are both caught. effects counts the real effects; args are the arguments of
// one operation, as the agent sends them (the proxy adds the key).
func OneEffect(t testing.TB, upstream *sdk.ClientSession, tool string, policy mcp.Policy, args json.RawMessage, effects func() int) {
	t.Helper()
	if err := CheckOneEffect(context.Background(), upstream, tool, policy, args, effects); err != nil {
		t.Fatal(err)
	}
}

// CheckOneEffect is OneEffect returning an error instead of failing a test.
func CheckOneEffect(ctx context.Context, upstream *sdk.ClientSession, tool string, policy mcp.Policy, args json.RawMessage, effects func() int) error {
	proxied, err := mcp.ProxiedTool(ctx, upstream, tool, policy)
	if err != nil {
		return err
	}
	// The proxy refuses a call it can't identify before anything runs, so the check does too: no real effect.
	if v, ok := proxied.(interface {
		Validate(context.Context, json.RawMessage) error
	}); ok {
		if err := v.Validate(ctx, args); err != nil {
			return fmt.Errorf("%s: the proxy would refuse these arguments: %w", tool, err)
		}
	}
	before := effects()
	var out any
	if keyed, ok := proxied.(agentsafe.IdempotentTool); ok {
		out, err = keyed.CallWithKey(ctx, newKey(), args)
	} else {
		out, err = proxied.Call(ctx, args)
	}
	res, _ := out.(*sdk.CallToolResult)
	switch {
	case err != nil:
		return fmt.Errorf("%s: the call failed, so nothing can be checked: %w", tool, err)
	case res != nil && res.IsError:
		return fmt.Errorf("%s: the call returned an error, so nothing can be checked: %s", tool, text(res))
	}
	switch n := effects() - before; {
	case n == 0:
		return fmt.Errorf("%s: one call made no effect: is effects() counting the right thing?", tool)
	case n > 1:
		return fmt.Errorf("%s: one call made %d effects (want 1): the server repeats the effect inside a single "+
			"call. No proxy can stop that, it sees one call: report it to the server's maintainers", tool, n)
	}
	return nil
}

// newKey is a key no other call has used, as the Gateway gives a new operation.
func newKey() string { return rand.Text() }

func text(res *sdk.CallToolResult) string {
	for _, c := range res.Content {
		if t, ok := c.(*sdk.TextContent); ok {
			return t.Text
		}
	}
	return "(no text)"
}
