// Command agentsafe-mcp runs an MCP server behind agentsafe: put it in front of the server's command in your
// agent's MCP configuration, and every tool call goes through agentsafe's log first.
//
//	agentsafe-mcp --log calls.jsonl --policy policy.json -- npx -y @acme/billing-mcp
//
// It speaks MCP on stdin/stdout to the agent, and starts the upstream server as a child process. Progress and
// errors go to stderr: stdout is the protocol. The policy file says which tools are protected by an
// idempotency key, and how the key reaches the upstream (mcp.LoadPolicies); without one, every call is
// passed through and logged.
//
// AGENTSAFE_KILL_AT=<point> kills the process at a named point (e.g. after_tool_executed): for crash tests.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "agentsafe-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	logPath := flag.String("log", "", "the log every call is written to (required)")
	scope := flag.String("scope", "", "idempotency scope: keys are unique within it (default: the log)")
	startedBy := flag.String("started-by", "", "who runs this agent, recorded in the log")
	quiet := flag.Bool("quiet", false, "don't report each call on stderr")
	policyPath := flag.String("policy", "", "policy file: which tools get idempotency keys, and how")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: agentsafe-mcp --log FILE [--policy FILE] [--scope S] [--started-by WHO] -- COMMAND [ARGS...]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *logPath == "" || flag.NArg() == 0 {
		flag.Usage()
		return errors.New("--log and the upstream command are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := exec.CommandContext(ctx, flag.Arg(0), flag.Args()[1:]...) //nolint:gosec // running the configured upstream is the point
	cmd.Stderr = os.Stderr
	upstream, err := sdk.NewClient(&sdk.Implementation{Name: "agentsafe-mcp", Version: version}, nil).
		Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("starting the upstream %q: %w", flag.Arg(0), err)
	}
	defer func() { _ = upstream.Close() }()

	var policies map[string]mcp.Policy
	if *policyPath != "" {
		if policies, err = mcp.LoadPolicies(*policyPath); err != nil {
			return err
		}
	}
	opts := []agentsafe.Option{agentsafe.WithScope(*scope), agentsafe.WithStartedBy(*startedBy)}
	if at := os.Getenv("AGENTSAFE_KILL_AT"); at != "" {
		opts = append(opts, agentsafe.WithHook(func(point string) {
			if point == at {
				fmt.Fprintln(os.Stderr, "agentsafe-mcp: killed at", point)
				kill()
			}
		}))
	}
	if !*quiet {
		opts = append(opts, agentsafe.WithLogf(func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }))
	}
	proxy, err := mcp.Open(ctx, upstream, &agentsafe.FileLog{Path: *logPath}, policies, opts...)
	if err != nil {
		return err
	}
	defer func() { _ = proxy.Close() }()

	err = proxy.Server(&sdk.Implementation{Name: "agentsafe-mcp", Version: version}).Run(ctx, &sdk.StdioTransport{})
	if errors.Is(err, context.Canceled) {
		return nil // stopped by a signal
	}
	return err
}
