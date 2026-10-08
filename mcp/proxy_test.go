package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeUpstream is an MCP server like a billing integration: a read, a billed write, and a tool that fails.
type fakeUpstream struct {
	mu      sync.Mutex
	seats   int
	charges int               // effects of charge: what the customer was actually billed for
	keys    []string          // the idempotency key each charge request carried, and how ("arg:"/"meta:")
	done    map[string]string // what it answered per key: it deduplicates, like a payment API
}

func (f *fakeUpstream) server() *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "billing-mcp", Version: "1.0.0"}, nil)
	object := map[string]any{"type": "object"}
	s.AddTool(&sdk.Tool{Name: "get_seats", Description: "How many seats cus_42 has", InputSchema: object,
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return text(false, "%d seats", f.seats), nil
		})
	s.AddTool(&sdk.Tool{Name: "add_seats", Description: "Add seats; each is billed", InputSchema: map[string]any{
		"type": "object", "properties": map[string]any{"add": map[string]any{"type": "integer"}}}},
		func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var in struct{ Add int }
			if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
				return nil, err
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.seats += in.Add
			return text(false, "now %d seats", f.seats), nil
		})
	s.AddTool(&sdk.Tool{Name: "charge", Description: "Bill the customer", InputSchema: map[string]any{
		"type": "object", "properties": map[string]any{"ticket_id": map[string]any{"type": "string"},
			"amount": map[string]any{"type": "string"}, "idempotency_key": map[string]any{"type": "string"}}}},
		f.charge)
	s.AddTool(&sdk.Tool{Name: "cancel_plan", Description: "Always refuses", InputSchema: object},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return text(true, "cancellations need a human"), nil
		})
	return s
}

func text(isError bool, format string, a ...any) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: isError, Content: []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf(format, a...)}}}
}

// rig is an agent's MCP client → the proxy → the fake upstream, all in memory, with the proxy's log on disk.
type rig struct {
	agent    *sdk.ClientSession
	proxy    *mcp.Proxy
	upstream *fakeUpstream
	log      string
}

func newRig(t *testing.T, log string, up *fakeUpstream) *rig {
	t.Helper()
	return newPolicyRig(t, log, up, nil)
}

func newPolicyRig(t *testing.T, log string, up *fakeUpstream, policies map[string]mcp.Policy, opts ...agentsafe.Option) *rig {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	if _, err := up.server().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	upstream, err := sdk.NewClient(&sdk.Implementation{Name: "agentsafe-mcp", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := mcp.Open(ctx, upstream, &agentsafe.FileLog{Path: log}, passRest(policies), append([]agentsafe.Option{agentsafe.WithStartedBy("support-agent")}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	st2, ct2 := sdk.NewInMemoryTransports()
	if _, err := p.Server(&sdk.Implementation{Name: "agentsafe-mcp", Version: "test"}).Connect(ctx, st2, nil); err != nil {
		t.Fatal(err)
	}
	agent, err := sdk.NewClient(&sdk.Implementation{Name: "langgraph-agent", Version: "0.1"}, nil).Connect(ctx, ct2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close(); _ = p.Close(); _ = upstream.Close() })
	return &rig{agent: agent, proxy: p, upstream: up, log: log}
}

func (r *rig) call(t *testing.T, tool, args string) *sdk.CallToolResult {
	t.Helper()
	res, err := r.agent.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

func resultText(r *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestTheAgentSeesTheUpstreamsTools(t *testing.T) {
	r := newRig(t, filepath.Join(t.TempDir(), "proxy.jsonl"), &fakeUpstream{seats: 10})
	got := map[string]*sdk.Tool{}
	for tool, err := range r.agent.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		got[tool.Name] = tool
	}
	if len(got) != 4 || got["add_seats"] == nil || got["get_seats"].Annotations == nil || !got["get_seats"].Annotations.ReadOnlyHint {
		t.Fatalf("the agent must see the upstream's tools, as the upstream describes them: %v", got)
	}
}

func TestEveryCallIsForwardedAndLogged(t *testing.T) {
	r := newRig(t, filepath.Join(t.TempDir(), "proxy.jsonl"), &fakeUpstream{seats: 10})
	if res := r.call(t, "add_seats", `{"add":5}`); res.IsError || resultText(res) != "now 15 seats" {
		t.Fatalf("a call goes through and its result comes back as the upstream sent it: %+v", res)
	}
	if res := r.call(t, "get_seats", `{}`); resultText(res) != "15 seats" {
		t.Fatalf("got %q", resultText(res))
	}
	if res := r.call(t, "cancel_plan", `{}`); !res.IsError || resultText(res) != "cancellations need a human" {
		t.Fatalf("the upstream's own tool errors come back as they are: %+v", res)
	}

	events, err := (&agentsafe.FileLog{Path: r.log}).Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st, err := agentsafe.Rebuild(events)
	if err != nil || st.Kind != agentsafe.KindProxy || st.Status != agentsafe.StatusOpen || st.StartedBy != "support-agent" {
		t.Fatalf("an open proxy run: err=%v %+v", err, st)
	}
	received, results := callsAndResults(events)
	if strings.Join(received, ", ") != "add_seats from langgraph-agent, get_seats from langgraph-agent, cancel_plan from langgraph-agent" {
		t.Fatalf("every call is logged as received, with the client's name: %v", received)
	}
	if len(results) != 3 || !strings.Contains(results[0], "now 15 seats") || !strings.Contains(results[2], `"isError":true`) {
		t.Fatalf("every result is logged as the upstream sent it: %v", results)
	}
}

// One log across restarts: a new proxy process continues the same open run.
func TestARestartedProxyContinuesTheSameRun(t *testing.T) {
	log := filepath.Join(t.TempDir(), "proxy.jsonl")
	up := &fakeUpstream{seats: 10}
	r := newRig(t, log, up)
	r.call(t, "add_seats", `{"add":5}`)
	_ = r.agent.Close()
	_ = r.proxy.Close()

	r2 := newRig(t, log, up)
	r2.call(t, "add_seats", `{"add":1}`)
	events, _ := (&agentsafe.FileLog{Path: log}).Read(context.Background())
	starts := 0
	for _, e := range events {
		if e.Type == agentsafe.EvRunStarted {
			starts++
		}
	}
	if starts != 1 || up.seats != 16 {
		t.Fatalf("one run across restarts (run_started %d times), every call forwarded once (seats %d)", starts, up.seats)
	}
}

// callsAndResults is what a proxy log says it was asked (tool and client) and what came back.
func callsAndResults(events []agentsafe.Event) (received, results []string) {
	for _, e := range events {
		if e.Type == agentsafe.EvCallReceived {
			received = append(received, e.Tool+" from "+e.Client)
		}
		if e.Type == agentsafe.EvToolResult {
			results = append(results, e.Result)
		}
	}
	return received, results
}

// charge bills once per idempotency key, from an argument or from _meta, like a payment API.
func (f *fakeUpstream) charge(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	var args map[string]any
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return nil, err
	}
	amount, _ := args["amount"].(string)
	key, how := "", ""
	if k, ok := args["idempotency_key"].(string); ok {
		key, how = k, "arg:"
	}
	if k, ok := req.Params.Meta[mcp.MetaKeyIdempotency].(string); ok {
		key, how = k, "meta:"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, how+key)
	if prev, ok := f.done[key]; ok && key != "" {
		return text(false, "%s", prev), nil
	}
	f.charges++
	answer := fmt.Sprintf("charged %s (charge %d)", amount, f.charges)
	if f.done == nil {
		f.done = map[string]string{}
	}
	f.done[key] = answer
	return text(false, "%s", answer), nil
}

// passRest gives every fake-upstream tool without a policy "pass": the proxy fails closed, and these tests are
// about other things.
func passRest(policies map[string]mcp.Policy) map[string]mcp.Policy {
	out := map[string]mcp.Policy{}
	for _, name := range []string{"get_seats", "add_seats", "charge", "cancel_plan"} {
		out[name] = mcp.Policy{Pass: true}
	}
	for name, p := range policies {
		out[name] = p
	}
	return out
}
