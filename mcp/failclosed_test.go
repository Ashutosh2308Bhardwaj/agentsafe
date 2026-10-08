package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A tool without a policy isn't exposed: the agent can't see it or call it. The proxy fails closed, so a tool
// the server adds tomorrow isn't forwarded until someone decides how it's protected.
func TestAToolWithoutAPolicyIsNotExposed(t *testing.T) {
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	if _, err := (&fakeUpstream{}).server().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	upstream, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upstream.Close() }()
	p, err := mcp.Open(ctx, upstream, &agentsafe.FileLog{Path: filepath.Join(t.TempDir(), "p.jsonl")},
		map[string]mcp.Policy{"get_seats": {Pass: true}, "add_seats": {Pass: true},
			"charge": {Identity: []string{"ticket_id"}, Key: mcp.KeyArgument, KeyArgument: "idempotency_key"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if !slices.Equal(p.Hidden(), []string{"cancel_plan"}) || !slices.Equal(p.Unprotected(), []string{"add_seats"}) {
		t.Fatalf("hidden %v (want cancel_plan), unprotected %v (want add_seats: passed through, not read-only)", p.Hidden(), p.Unprotected())
	}

	st2, ct2 := sdk.NewInMemoryTransports()
	if _, err := p.Server(&sdk.Implementation{Name: "agentsafe-mcp", Version: "t"}).Connect(ctx, st2, nil); err != nil {
		t.Fatal(err)
	}
	agent, err := sdk.NewClient(&sdk.Implementation{Name: "agent", Version: "1"}, nil).Connect(ctx, ct2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agent.Close() }()
	for tool, err := range agent.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tool.Name == "cancel_plan" {
			t.Fatal("a tool without a policy must not be listed")
		}
	}
	res, err := agent.CallTool(ctx, &sdk.CallToolParams{Name: "cancel_plan", Arguments: json.RawMessage(`{}`)})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("a tool without a policy must not be callable: %+v", res)
	}
}

func TestPassCantBeCombinedWithProtection(t *testing.T) {
	for name, p := range map[string]mcp.Policy{
		"pass and identity": {Pass: true, Identity: []string{"ticket_id"}},
		"pass and a key":    {Pass: true, Key: mcp.KeyMeta},
		"pass and approval": {Pass: true, Approval: "always"},
	} {
		_, err := openWith(t, map[string]mcp.Policy{"charge": p})
		if !errors.Is(err, agentsafe.ErrConfig) || !strings.Contains(err.Error(), "unprotected") {
			t.Errorf("%s: must be refused: %v", name, err)
		}
	}
}
