package mcp_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The client's own metadata travels to the upstream (a trace context, say); what belongs to its connection to
// the proxy doesn't, and nobody but the proxy writes agentsafe's namespace.
func TestTheClientsMetadataReachesTheUpstream(t *testing.T) {
	for _, mode := range []mcp.KeyMode{mcp.KeyArgument, mcp.KeyMeta} {
		up := &fakeUpstream{}
		r := newPolicyRig(t, filepath.Join(t.TempDir(), "p.jsonl"), up, chargePolicy(mode))
		_, err := r.agent.CallTool(context.Background(), &sdk.CallToolParams{Name: "charge",
			Arguments: json.RawMessage(`{"ticket_id":"T-1","amount":"10.00"}`),
			Meta: sdk.Meta{
				"traceparent":                "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
				"dev.mcp/hop":                "reserved for MCP: describes this connection",
				mcp.MetaKeyIdempotency:       "forged-by-the-client",
				mcp.MetaPrefix + "something": "only the proxy writes here",
			}})
		if err != nil {
			t.Fatal(err)
		}
		got := up.metas[0]
		if got["traceparent"] != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
			t.Errorf("%s: the trace context must reach the upstream: %v", mode, got)
		}
		if _, ok := got["dev.mcp/hop"]; ok {
			t.Errorf("%s: a reserved MCP key must not be forwarded: %v", mode, got)
		}
		if _, ok := got[mcp.MetaPrefix+"something"]; ok {
			t.Errorf("%s: a client can't write agentsafe's namespace: %v", mode, got)
		}
		key, _ := got[mcp.MetaKeyIdempotency].(string)
		switch {
		case mode == mcp.KeyMeta && len(key) != 32:
			t.Errorf("meta mode: the proxy's key, not the client's forgery: %q", key)
		case mode == mcp.KeyArgument && key != "":
			t.Errorf("argument mode: no key in _meta at all, least of all the forgery: %q", key)
		}
	}
}
