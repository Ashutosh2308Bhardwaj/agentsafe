package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func build(t *testing.T, dir, name, pkg string) string {
	t.Helper()
	bin := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, pkg).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
	return bin
}

// The binary as an agent runs it: the agent's MCP client starts agentsafe-mcp, which starts the upstream.
// stdio both ways, real processes.
func TestAgentsafeMCPAsAProcess(t *testing.T) {
	dir := t.TempDir()
	proxyBin := build(t, dir, "agentsafe-mcp", ".")
	upstreamBin := build(t, dir, "fakeupstream", "../../internal/fakeupstream")
	log, state := filepath.Join(dir, "calls.jsonl"), filepath.Join(dir, "books.json")

	ctx := context.Background()
	policy := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"tools":{"add_seats":{"pass":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(proxyBin, "--log", log, "--policy", policy, "--started-by", "support-agent", "--", upstreamBin, "--state", state)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	agent, err := sdk.NewClient(&sdk.Implementation{Name: "test-agent", Version: "1"}, nil).
		Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v\nstderr: %s", err, stderr.String())
	}
	res, err := agent.CallTool(ctx, &sdk.CallToolParams{Name: "add_seats", Arguments: json.RawMessage(`{"add":5}`)})
	if err != nil || res.IsError || res.Content[0].(*sdk.TextContent).Text != "now 15 seats" {
		t.Fatalf("a call through the proxy process: %+v, %v\nstderr: %s", res, err, stderr.String())
	}
	if err := agent.Close(); err != nil {
		t.Logf("close: %v", err)
	}

	if b, _ := os.ReadFile(state); !strings.Contains(string(b), `"seats":15`) {
		t.Fatalf("the upstream did it once: %s", b)
	}
	events, err := (&agentsafe.FileLog{Path: log}).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err := agentsafe.Rebuild(events)
	if err != nil || st.Kind != agentsafe.KindProxy || st.Events != 4 || st.StartedBy != "support-agent" {
		t.Fatalf("run_started, call_received, tool_started, tool_result: err=%v %+v", err, st)
	}
	for _, want := range []string{
		"add_seats",         // progress goes to stderr: stdout is the protocol
		"WARNING", "charge", // charge has no policy: not exposed, and the operator is told
		"forwarded without idempotency", // add_seats is passed through and not read-only
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr must mention %q:\n%s", want, stderr.String())
		}
	}
}

// The proxy killed at the worst instant: the upstream has charged, the proxy hasn't logged it. A new proxy
// process starts on the same log, and the agent asks again, as any agent does after an error. Per key mode,
// the customer must be charged once; with no policy (the control), the retry charges twice.
func TestKilledAfterTheUpstreamChargedThenRestarted(t *testing.T) {
	dir := t.TempDir()
	proxyBin := build(t, dir, "agentsafe-mcp", ".")
	upstreamBin := build(t, dir, "fakeupstream", "../../internal/fakeupstream")
	for _, c := range []struct {
		name, policy string
		charges      int
		retry        string // what the agent's retry gets back
	}{
		{"key as an argument", `{"tools":{"charge":{"identity":["ticket_id"],"key":"argument","key_argument":"idempotency_key"}}}`, 1, "charged 10.00 (charge 1)"},
		{"key in _meta", `{"tools":{"charge":{"identity":["ticket_id"],"key":"meta"}}}`, 1, "charged 10.00 (charge 1)"},
		{"no key the upstream can read", `{"tools":{"charge":{"identity":["ticket_id"],"key":"none"}}}`, 1, "outcome unknown"},
		{"control: passed through, unprotected", `{"tools":{"charge":{"pass":true}}}`, 2, "charged 10.00 (charge 2)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := filepath.Join(t.TempDir(), "run")
			if err := os.MkdirAll(run, 0o750); err != nil {
				t.Fatal(err)
			}
			args := []string{"--log", filepath.Join(run, "calls.jsonl")}
			if c.policy != "" {
				policy := filepath.Join(run, "policy.json")
				if err := os.WriteFile(policy, []byte(c.policy), 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--policy", policy)
			}
			args = append(args, "--", upstreamBin, "--state", filepath.Join(run, "books.json"))
			charge := `{"ticket_id":"T-1","amount":"10.00"}`

			if _, err := session(t, proxyBin, args, "after_tool_executed", charge); err == nil {
				t.Fatal("the first proxy must have been killed mid-call")
			}
			res, err := session(t, proxyBin, args, "", charge) // a new proxy process; the agent asks again
			if err != nil {
				t.Fatalf("after the restart: %v", err)
			}
			raw, _ := os.ReadFile(filepath.Join(run, "books.json"))
			if !strings.Contains(string(raw), fmt.Sprintf(`"charges":%d`, c.charges)) || !strings.Contains(res, c.retry) {
				t.Fatalf("want %d charge(s) and %q for the retry; books %s, retry got %q", c.charges, c.retry, raw, res)
			}
		})
	}
}

// session starts agentsafe-mcp (killed at killAt, if set), makes one charge call as an agent, and closes.
func session(t *testing.T, bin string, args []string, killAt, charge string) (string, error) {
	t.Helper()
	ctx := context.Background()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "AGENTSAFE_KILL_AT="+killAt)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	agent, err := sdk.NewClient(&sdk.Implementation{Name: "test-agent", Version: "1"}, nil).
		Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v\nstderr: %s", err, stderr.String())
	}
	defer func() { _ = agent.Close() }()
	res, err := agent.CallTool(ctx, &sdk.CallToolParams{Name: "charge", Arguments: json.RawMessage(charge)})
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return text.String(), nil
}

// The human loop with real processes and a real OS identity: the agent is told "pending", a person runs
// `agentsafe-mcp pending` and `approve` (or `reject`) as themselves, and the agent's next call gets the outcome.
func TestApprovingFromTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	proxyBin := build(t, dir, "agentsafe-mcp", ".")
	upstreamBin := build(t, dir, "fakeupstream", "../../internal/fakeupstream")
	me, err := osIdentity()
	if err != nil {
		t.Fatal(err)
	}
	const charge = `{"ticket_id":"T-77","amount":"1200.00"}`
	for _, c := range []struct {
		name, approvers, startedBy, decision string
		decided                              bool   // the decision is accepted
		why                                  string // why it's refused, if it is
		after                                string // what the agent's next call gets
		charges                              int
	}{
		{"an approver approves", me, "support-agent", "approve", true, "", "charged 1200.00 (charge 1)", 1},
		{"someone not on the list", "user:someone-else", "support-agent", "approve", false, "not on the approver list", "pending_approval", 0},
		{"the agent's own operator", me, me, "approve", false, "started this run", "pending_approval", 0},
		{"an approver rejects", me, "support-agent", "reject", true, "", "rejected", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := t.TempDir()
			log, books, policy := filepath.Join(run, "calls.jsonl"), filepath.Join(run, "books.json"), filepath.Join(run, "policy.json")
			cfg := fmt.Sprintf(`{"approvers":[%q],"tools":{"charge":{"identity":["ticket_id"],"key":"argument","key_argument":"idempotency_key","approval":"always"}}}`, c.approvers)
			if err := os.WriteFile(policy, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			serve := []string{"--log", log, "--policy", policy, "--started-by", c.startedBy, "--", upstreamBin, "--state", books}

			first, err := session(t, proxyBin, serve, "", charge)
			var p struct{ Status, Key string }
			if err != nil || json.Unmarshal([]byte(first), &p) != nil || p.Status != "pending_approval" {
				t.Fatalf("the agent is told pending: %q %v", first, err)
			}
			if out, err := exec.Command(proxyBin, "pending", "--log", log, "--policy", policy).CombinedOutput(); err != nil || !strings.Contains(string(out), p.Key) {
				t.Fatalf("pending lists it: %s %v", out, err)
			}
			out, err := exec.Command(proxyBin, c.decision, "--log", log, "--policy", policy, "--reason", "test", p.Key).CombinedOutput()
			if (err == nil) != c.decided || !strings.Contains(string(out), c.why) {
				t.Fatalf("%s by %s: accepted=%v, want %v (%q): %s", c.decision, me, err == nil, c.decided, c.why, out)
			}
			after, err := session(t, proxyBin, serve, "", charge)
			raw, _ := os.ReadFile(books)
			if err != nil || !strings.Contains(after, c.after) || (c.charges > 0) != strings.Contains(string(raw), fmt.Sprintf(`"charges":%d`, c.charges)) {
				t.Fatalf("the next call: %q (want %q), books %s, err %v", after, c.after, raw, err)
			}
		})
	}
}
