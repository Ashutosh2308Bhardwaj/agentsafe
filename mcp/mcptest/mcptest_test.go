package mcptest_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp/mcptest"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// billing is an MCP server with three charge tools: one that deduplicates on its key properly, one that accepts
// the key and ignores it, and one that checks the key and then charges without holding a lock (check-then-act).
type billing struct {
	mu      sync.Mutex
	charges int
	done    map[string]bool
}

func (b *billing) server() *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "billing", Version: "1"}, nil)
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"ticket_id": map[string]any{"type": "string"}, "idempotency_key": map[string]any{"type": "string"}}}
	key := func(req *sdk.CallToolRequest) string {
		var in struct {
			Key string `json:"idempotency_key"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return in.Key
	}
	// A fresh result per call: the SDK writes to the result it sends, so a shared one is a data race.
	ok := func() *sdk.CallToolResult {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "charged"}}}
	}
	s.AddTool(&sdk.Tool{Name: "charge", InputSchema: schema}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.done[key(req)] {
			b.done[key(req)], b.charges = true, b.charges+1
		}
		return ok(), nil
	})
	s.AddTool(&sdk.Tool{Name: "charge_ignores_key", InputSchema: schema}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.charges++
		return ok(), nil
	})
	s.AddTool(&sdk.Tool{Name: "charge_racy", InputSchema: schema}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		b.mu.Lock()
		seen := b.done[key(req)]
		b.mu.Unlock()
		if !seen { // the check and the effect aren't atomic: a real server calls its payment API in between
			time.Sleep(5 * time.Millisecond)
			b.mu.Lock()
			b.done[key(req)], b.charges = true, b.charges+1
			b.mu.Unlock()
		}
		return ok(), nil
	})
	s.AddTool(&sdk.Tool{Name: "charge_twice", InputSchema: schema}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.charges += 2 // a bug inside the server: one call, two effects
		return ok(), nil
	})
	s.AddTool(&sdk.Tool{Name: "charge_nothing", InputSchema: schema}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return ok(), nil
	})
	s.AddTool(&sdk.Tool{Name: "charge_fails", InputSchema: schema}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "card declined"}}}, nil
	})
	return s
}

func session(t *testing.T, b *billing) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	if _, err := b.server().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

var policy = mcp.Policy{Identity: []string{"ticket_id"}, Key: mcp.KeyArgument, KeyArgument: "idempotency_key"}

func TestAServerThatHonoursTheKeyPasses(t *testing.T) {
	b := &billing{done: map[string]bool{}}
	mcptest.SameKey(t, session(t, b), "charge", policy, json.RawMessage(`{"ticket_id":"T-1"}`), func() int {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.charges
	})
}

// The claim "key: argument" is the operator's; these servers would break it, and the check says so.
func TestAServerThatDoesntIsCaught(t *testing.T) {
	for tool, want := range map[string]string{"charge_ignores_key": "effects", "charge_racy": "effects"} {
		b := &billing{done: map[string]bool{}}
		err := mcptest.CheckSameKey(context.Background(), session(t, b), tool, policy, json.RawMessage(`{"ticket_id":"T-1"}`),
			func() int {
				b.mu.Lock()
				defer b.mu.Unlock()
				return b.charges
			})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: a server that doesn't deduplicate must be caught: %v", tool, err)
		}
	}
}

func TestOnlyAKeyTheServerReceivesCanBeChecked(t *testing.T) {
	for name, p := range map[string]mcp.Policy{
		"key: none": {Identity: []string{"ticket_id"}},
		"pass":      {Pass: true},
	} {
		_, err := mcp.KeyedTool(context.Background(), session(t, &billing{done: map[string]bool{}}), "charge", p)
		if !errors.Is(err, agentsafe.ErrConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// One call, one effect: for any tool, keyed or not. Two effects (a duplicate inside the server), none, or an
// error are each caught, and said apart.
func TestOneCallOneEffect(t *testing.T) {
	for tool, want := range map[string]string{
		"charge":             "",
		"charge_ignores_key": "", // a key isn't involved: one call, one charge
		"charge_twice":       "one call made 2 effects",
		"charge_nothing":     "made no effect",
		"charge_fails":       "card declined",
	} {
		b := &billing{done: map[string]bool{}}
		err := mcptest.CheckOneEffect(context.Background(), session(t, b), tool, json.RawMessage(`{"ticket_id":"T-1"}`),
			func() int {
				b.mu.Lock()
				defer b.mu.Unlock()
				return b.charges
			})
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: got %v, want %q", tool, err, want)
		}
	}
}
