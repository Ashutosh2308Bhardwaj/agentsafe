package trace_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/trace"
)

// proxyLog runs a real Gateway: a read, a gated payout answered pending, a refused approver, an approval, and
// the payout. It returns the log.
func proxyLog(t *testing.T) []agentsafe.Event {
	t.Helper()
	ctx := context.Background()
	type payout struct {
		Ref    string `json:"ref"`
		Amount string `json:"amount"`
	}
	pay := agentsafe.Func("pay", "pay", func(context.Context, payout) (string, error) { return "paid", nil },
		agentsafe.Idempotent("ref"), agentsafe.NeedsApproval(func(in payout) any { return in }))
	read := agentsafe.Func("get_balance", "read", func(context.Context, struct{}) (string, error) { return "1000.00", nil })
	log := &agentsafe.FileLog{Path: filepath.Join(t.TempDir(), "proxy.jsonl")}
	g, err := agentsafe.OpenGateway(ctx, log, agentsafe.WithTools(pay, read), agentsafe.WithStartedBy("support-agent"),
		agentsafe.WithAuthorizer(agentsafe.All(agentsafe.AllowList("finance"), agentsafe.NotRequester())))
	if err != nil {
		t.Fatal(err)
	}
	call := func(tool, args string) agentsafe.GatewayResult {
		r, err := g.Call(ctx, "langgraph-agent", tool, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	call("get_balance", `{}`)
	var p struct{ Key string }
	_ = json.Unmarshal([]byte(call("pay", `{"ref":"T-77","amount":"1200.00"}`).Result), &p)
	_ = g.Approve(ctx, p.Key, "intern") // refused, and logged
	if err := g.Approve(ctx, p.Key, "finance"); err != nil {
		t.Fatal(err)
	}
	call("pay", `{"ref":"T-77","amount":"1200.00"}`)
	events, err := log.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestATraceOfAProxyRun(t *testing.T) {
	tr, err := trace.Build(proxyLog(t), "billing-proxy")
	if err != nil {
		t.Fatal(err)
	}
	tree := tr.Tree()
	for _, want := range []string{
		"[proxy run, open,",
		"├─ execute_tool get_balance", "(from langgraph-agent)",
		"⏸ approval pay", "approved by finance",
		"├─ execute_tool pay",
	} {
		if !strings.Contains(tree, want) {
			t.Errorf("the tree must show %q:\n%s", want, tree)
		}
	}
	if strings.Contains(tree, "steps") || strings.Contains(tree, "│   └─") {
		t.Errorf("a proxy run has no model steps to nest tools under:\n%s", tree)
	}
	otlp, err := tr.OTLPJSON("billing-proxy")
	if err != nil || !strings.Contains(string(otlp), `"agentsafe.run.kind"`) || !strings.Contains(string(otlp), `"agentsafe.client"`) {
		t.Fatalf("the exported trace says it's a proxy run, and who sent each call: %v", err)
	}
}
