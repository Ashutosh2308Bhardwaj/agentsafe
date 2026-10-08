// Command agentsafe-mcp runs an MCP server behind agentsafe: put it in front of the server's command in your
// agent's MCP configuration, and every tool call goes through agentsafe's log first.
//
//	agentsafe-mcp --log calls.jsonl --policy policy.json --started-by support-agent -- npx -y @acme/billing-mcp
//
// It speaks MCP on stdin/stdout to the agent, and starts the upstream server as a child process. Progress and
// errors go to stderr: stdout is the protocol. The policy file (mcp.Config) says which tools are protected by
// an idempotency key, how the key reaches the upstream, which need a human's approval, and who may approve.
//
// Approvals are decided from the command line, by the OS account running it:
//
//	agentsafe-mcp pending --log calls.jsonl --policy policy.json
//	agentsafe-mcp approve --log calls.jsonl --policy policy.json KEY
//	agentsafe-mcp reject  --log calls.jsonl --policy policy.json --reason "duplicate ticket" KEY
//
// AGENTSAFE_KILL_AT=<point> kills the proxy at a named point (e.g. after_tool_executed): for crash tests.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"syscall"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.1.0"

func main() {
	var err error
	if len(os.Args) > 1 && (os.Args[1] == "approve" || os.Args[1] == "reject" || os.Args[1] == "pending") {
		err = decide(os.Args[1], os.Args[2:])
	} else {
		err = serve(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentsafe-mcp:", err)
		os.Exit(1)
	}
}

// common are the flags every command takes.
type common struct {
	log, policy string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.log, "log", "", "the log every call is written to (required)")
	fs.StringVar(&c.policy, "policy", "", "policy file: keys, approvals, approvers (mcp.Config)")
}

func (c *common) config() (mcp.Config, error) {
	if c.log == "" {
		return mcp.Config{}, errors.New("--log is required")
	}
	if c.policy == "" {
		return mcp.Config{}, nil
	}
	return mcp.LoadConfig(c.policy)
}

// authorizer allows the configured approvers, never on a run they started.
func authorizer(cfg mcp.Config) agentsafe.Option {
	if len(cfg.Approvers) == 0 {
		return func(*agentsafe.Runner) {}
	}
	return agentsafe.WithAuthorizer(agentsafe.All(agentsafe.AllowList(cfg.Approvers...), agentsafe.NotRequester()))
}

func serve(args []string) error {
	fs := flag.NewFlagSet("agentsafe-mcp", flag.ContinueOnError)
	var c common
	c.register(fs)
	scope := fs.String("scope", "", "idempotency scope: keys are unique within it (default: the log)")
	startedBy := fs.String("started-by", "", "who runs this agent: recorded, and never allowed to approve its calls")
	quiet := fs.Bool("quiet", false, "don't report each call on stderr")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: agentsafe-mcp --log FILE [--policy FILE] [--started-by WHO] [--scope S] -- COMMAND [ARGS...]")
		fmt.Fprintln(os.Stderr, "       agentsafe-mcp pending|approve|reject --log FILE --policy FILE [KEY]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := c.config()
	if err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("the upstream command is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := exec.CommandContext(ctx, fs.Arg(0), fs.Args()[1:]...) //nolint:gosec // running the configured upstream is the point
	cmd.Stderr = os.Stderr
	upstream, err := sdk.NewClient(&sdk.Implementation{Name: "agentsafe-mcp", Version: version}, nil).
		Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("starting the upstream %q: %w", fs.Arg(0), err)
	}
	defer func() { _ = upstream.Close() }()

	opts := []agentsafe.Option{agentsafe.WithScope(*scope), agentsafe.WithStartedBy(*startedBy), authorizer(cfg)}
	if !*quiet {
		opts = append(opts, agentsafe.WithLogf(func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }))
	}
	if at := os.Getenv("AGENTSAFE_KILL_AT"); at != "" {
		opts = append(opts, agentsafe.WithHook(func(point string) {
			if point == at {
				fmt.Fprintln(os.Stderr, "agentsafe-mcp: killed at", point)
				kill()
			}
		}))
	}
	proxy, err := mcp.Open(ctx, upstream, &agentsafe.FileLog{Path: c.log}, cfg.Tools, opts...)
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

// decide is pending, approve or reject: a person, at a terminal, identified by their OS account.
func decide(command string, args []string) error {
	fs := flag.NewFlagSet("agentsafe-mcp "+command, flag.ContinueOnError)
	var c common
	c.register(fs)
	reason := fs.String("reason", "", "why (reject)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := c.config()
	if err != nil {
		return err
	}
	ctx := context.Background()
	g, err := agentsafe.OpenGateway(ctx, &agentsafe.FileLog{Path: c.log}, authorizer(cfg))
	if err != nil {
		return err
	}
	defer func() { _ = g.Close() }()
	if command == "pending" {
		return printPending(ctx, g)
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("%s takes one operation key (see: agentsafe-mcp pending)", command)
	}
	who, err := osIdentity()
	if err != nil {
		return err
	}
	key := fs.Arg(0)
	if command == "approve" {
		err = g.Approve(ctx, key, who)
	} else {
		err = g.Reject(ctx, key, who, *reason)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s %sd by %s. The agent's next call for it gets the outcome.\n", key, command, who)
	return nil
}

func printPending(ctx context.Context, g *agentsafe.Gateway) error {
	waiting, err := g.Pending(ctx)
	if err != nil {
		return err
	}
	if len(waiting) == 0 {
		fmt.Println("nothing is waiting for a decision")
	}
	for _, w := range waiting {
		fmt.Printf("%s  %s %s\n", w.Key, w.Tool, w.Summary)
	}
	return nil
}

// osIdentity is who is deciding: the OS account running this command. It comes from the process's user id,
// not from $USER, which anyone can set.
func osIdentity() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("who is deciding: %w", err)
	}
	return "user:" + u.Username, nil
}
