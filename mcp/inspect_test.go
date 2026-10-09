package mcp_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A server like the ones the scan meets: some tools annotated, most not; one key argument among them.
func shopServer() *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "shop-mcp", Version: "2.1.0"}, nil)
	no := false
	obj := func(required []string, props ...string) map[string]any {
		p := map[string]any{}
		for _, n := range props {
			p[n] = map[string]any{"type": "string"}
		}
		return map[string]any{"type": "object", "properties": p, "required": required}
	}
	ok := func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		panic("inspect must not call any tool")
	}
	for _, t := range []*sdk.Tool{
		{Name: "get_order", InputSchema: obj([]string{"order_id"}, "order_id"), Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}},
		{Name: "create_refund", InputSchema: obj([]string{"order_id", "amount", "Idempotency-Key"}, "order_id", "amount", "reason", "Idempotency-Key")},
		{Name: "send_message", InputSchema: obj([]string{"channel", "text"}, "channel", "text"),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: &no}},
		{Name: "update_address", InputSchema: obj([]string{"order_id"}, "order_id", "address", "request_id"),
			Annotations: &sdk.ToolAnnotations{DestructiveHint: &no, IdempotentHint: true}},
		{Name: "delete_order", InputSchema: obj([]string{"order_id"}, "order_id"), Annotations: &sdk.ToolAnnotations{}},
		{Name: "clear_cache", InputSchema: obj(nil)},
	} {
		s.AddTool(t, ok)
	}
	return s
}

func shopSession(t *testing.T) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	if _, err := shopServer().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// Each tool, as it describes itself, and the policy that follows: reads pass; writes are identified by all their
// arguments, keyed on a key argument if they have one, and gated unless the server says they're not destructive.
func TestInspectSuggestsAPolicyPerTool(t *testing.T) {
	in, err := mcp.Inspect(context.Background(), shopSession(t))
	if err != nil {
		t.Fatal(err)
	}
	if in.Server != "shop-mcp" || in.Version != "2.1.0" || len(in.Tools) != 6 {
		t.Fatalf("server and tools: %+v", in)
	}
	got := map[string]mcp.ToolSummary{}
	for _, s := range in.Tools {
		got[s.Name] = s
	}
	all := []string{mcp.AllArguments}
	want := map[string]*mcp.Policy{
		"get_order":      {Pass: true},
		"create_refund":  {Identity: all, Key: mcp.KeyArgument, KeyArgument: "Idempotency-Key", Approval: "always"},
		"send_message":   {Identity: all, Key: mcp.KeyNone},
		"update_address": {Identity: all, Key: mcp.KeyArgument, KeyArgument: "request_id"},
		"delete_order":   {Identity: all, Key: mcp.KeyNone, Approval: "always"},
		"clear_cache":    {Identity: all, Key: mcp.KeyNone, Approval: "always"},
	}
	for name, p := range want {
		if !reflect.DeepEqual(got[name].Policy, p) {
			t.Errorf("%s: policy %+v, want %+v", name, got[name].Policy, p)
		}
	}
	checkHints(t, got)
}

// checkHints: the hints as the server sent them, MCP's defaults where it sent none, and the required arguments.
func checkHints(t *testing.T, got map[string]mcp.ToolSummary) {
	t.Helper()
	if s := got["create_refund"]; !reflect.DeepEqual(s.Required, []string{"amount", "order_id"}) {
		t.Errorf("required arguments, the key argument aside, are reported: %v", s.Required)
	}
	if s := got["create_refund"]; !s.Write || s.Annotated || !s.Destructive {
		t.Errorf("no annotations: MCP's defaults, a destructive write: %+v", s)
	}
	if s := got["update_address"]; !s.Idempotent || s.Destructive {
		t.Errorf("the hints as sent: %+v", s)
	}
	if s := got["get_order"]; s.Write || s.Destructive {
		t.Errorf("read-only: %+v", s)
	}
}

// The starter policy file is usable as it is: it loads, and a proxy opens on it, exposing every tool.
func TestTheStarterPolicyOpensAProxy(t *testing.T) {
	ctx := context.Background()
	up := shopSession(t)
	in, err := mcp.Inspect(ctx, up)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(in.Starter("user:alice"), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := mcp.LoadConfig(path)
	if err != nil {
		t.Fatalf("the starter must load: %v\n%s", err, raw)
	}
	p, err := mcp.Open(ctx, up, &agentsafe.FileLog{Path: filepath.Join(t.TempDir(), "p.jsonl")}, cfg.Tools,
		agentsafe.WithAuthorizer(agentsafe.AllowList(cfg.Approvers...))) // as agentsafe-mcp does
	if err != nil {
		t.Fatalf("the starter must open a proxy: %v\n%s", err, raw)
	}
	if h := p.Hidden(); len(h) != 0 {
		t.Fatalf("every tool has a suggestion: %v", h)
	}
}

func TestATimeoutIsWrittenAsItsRead(t *testing.T) {
	raw, err := json.Marshal(mcp.Policy{Identity: []string{"id"}, Timeout: mcp.Duration(1500_000_000)})
	if err != nil {
		t.Fatal(err)
	}
	var back mcp.Policy
	if err := json.Unmarshal(raw, &back); err != nil || back.Timeout != mcp.Duration(1500_000_000) {
		t.Fatalf("%s: %v %v", raw, back.Timeout, err)
	}
}
