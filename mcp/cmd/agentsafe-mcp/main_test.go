package main

import (
	"context"
	"encoding/json"
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
	log, state := filepath.Join(dir, "calls.jsonl"), filepath.Join(dir, "seats.txt")

	ctx := context.Background()
	cmd := exec.Command(proxyBin, "--log", log, "--started-by", "support-agent", "--", upstreamBin, "--state", state)
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

	if b, _ := os.ReadFile(state); string(b) != "15" {
		t.Fatalf("the upstream did it once: %q", b)
	}
	events, err := (&agentsafe.FileLog{Path: log}).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err := agentsafe.Rebuild(events)
	if err != nil || st.Kind != agentsafe.KindProxy || st.Events != 4 || st.StartedBy != "support-agent" {
		t.Fatalf("run_started, call_received, tool_started, tool_result: err=%v %+v", err, st)
	}
	if !strings.Contains(stderr.String(), "add_seats") {
		t.Fatalf("progress goes to stderr (stdout is the protocol): %q", stderr.String())
	}
}
