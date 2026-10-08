// Package mcptest checks an MCP server against what agentsafe-mcp relies on. A policy with key "meta" or
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
	"encoding/json"
	"testing"

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
