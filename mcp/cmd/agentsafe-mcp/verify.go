package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp/mcptest"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/tooltest"
)

// verify checks that the upstream really deduplicates on the key its policy sends: one call with a new key makes
// one effect, and 20 at once with one key still make one, with one answer for everyone (mcptest.CheckSameKey).
// The effects are counted by --count, a shell command that prints a number. It makes real calls with real
// effects, so it refuses to run without --sandbox.
func verify(args []string) error {
	v, err := parseVerify(args)
	if err != nil {
		return err
	}
	cfg, err := mcp.LoadConfig(v.policy)
	if err != nil {
		return err
	}
	policy, ok := cfg.Tools[v.tool]
	if !ok {
		return fmt.Errorf("%s has no policy in %s", v.tool, v.policy)
	}
	ctx := context.Background()
	upstream, err := startUpstream(ctx, v.upstream)
	if err != nil {
		return err
	}
	defer func() { _ = upstream.Close() }()

	var countErr error
	effects := func() int {
		n, err := runCount(ctx, v.count)
		if err != nil && countErr == nil {
			countErr = err
		}
		return n
	}
	err = mcptest.CheckSameKey(ctx, upstream, v.tool, policy, json.RawMessage(v.args), effects)
	if countErr != nil {
		return fmt.Errorf("counting effects: %w", countErr)
	}
	if err != nil {
		return fmt.Errorf("FAIL: %w", err)
	}
	fmt.Printf("PASS: %s deduplicates on its key (key: %s): one call with a new key made one effect, and %d "+
		"simultaneous calls with one key made one, with one answer.\n", v.tool, policy.Key, tooltest.Concurrency)
	return nil
}

// verifyArgs are verify's flags, checked.
type verifyArgs struct {
	policy, tool, args, count string
	upstream                  []string
}

func parseVerify(args []string) (verifyArgs, error) {
	var v verifyArgs
	fs := flag.NewFlagSet("agentsafe-mcp verify", flag.ContinueOnError)
	fs.StringVar(&v.policy, "policy", "", "policy file (required): the tool's policy is what's checked")
	fs.StringVar(&v.tool, "tool", "", "the tool to check (required)")
	fs.StringVar(&v.args, "args", "", "arguments of one operation, as JSON (required)")
	fs.StringVar(&v.count, "count", "", "shell command printing how many effects exist so far, e.g. a SELECT count(*) (required)")
	sandbox := fs.Bool("sandbox", false, "confirm the server is a sandbox: verify makes real calls, with real effects")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: agentsafe-mcp verify --policy FILE --tool NAME --args JSON --count CMD --sandbox -- COMMAND [ARGS...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return v, err
	}
	v.upstream = fs.Args()
	switch {
	case v.policy == "" || v.tool == "" || v.args == "" || v.count == "" || len(v.upstream) == 0:
		fs.Usage()
		return v, errors.New("--policy, --tool, --args, --count and the upstream command are required")
	case !*sandbox:
		return v, errors.New("verify calls the tool at least twice, for real: run it against a sandbox of the server, and say so with --sandbox")
	case !json.Valid([]byte(v.args)):
		return v, errors.New("--args isn't JSON")
	}
	return v, nil
}

// runCount runs the --count command through the system shell and reads the number it prints.
func runCount(ctx context.Context, command string) (int, error) {
	shell, flagC := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flagC = "cmd", "/C"
	}
	out, err := exec.CommandContext(ctx, shell, flagC, command).Output() //nolint:gosec // the operator's own command
	if err != nil {
		return 0, fmt.Errorf("%q: %w", command, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("%q printed %q, not a number", command, strings.TrimSpace(string(out)))
	}
	return n, nil
}
