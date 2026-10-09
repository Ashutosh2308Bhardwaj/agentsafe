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
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp/mcptest"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/tooltest"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// verify checks a tool against a sandbox of its server, called exactly as the proxy calls it under its policy
// (the same code builds both, with a fresh key where the policy sends one). Every tool: one call makes exactly
// one effect (mcptest.CheckOneEffect); two means the server repeats it inside one call, which no proxy can stop.
// A policy that sends a key: the server really deduplicates on it, one call after another and 20 at once
// (mcptest.CheckSameKey). Without a key the tool isn't retry-safe, which verify reports: after a crash agentsafe
// records the outcome as unknown and never retries it. Effects are counted by --count, a shell command that
// prints a number. It makes real calls with real effects, so it refuses to run without --sandbox. --timeout bounds
// the whole run, the server's calls and the counts.
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
	ctx, cancel := context.WithTimeout(context.Background(), v.timeout)
	defer cancel()
	upstream, err := startUpstream(ctx, v.upstream)
	if err != nil {
		return err
	}
	defer func() { _ = upstream.Close() }()

	var countErr error
	effects := func() int {
		time.Sleep(v.settle) // a count that lags the effect (an eventually consistent API) would hide one
		n, err := runCount(ctx, v.count)
		if err != nil && countErr == nil {
			countErr = err
		}
		return n
	}
	// The policy is checked against the tool before any call: a wrong one mustn't cost a real effect.
	if _, err := mcp.ProxiedTool(ctx, upstream, v.tool, policy); err != nil {
		return err
	}
	r := check(ctx, upstream, v, policy, effects)
	if ctx.Err() != nil {
		return fmt.Errorf("verify gave up after %s (--timeout): the server or --count didn't answer: %w", v.timeout, ctx.Err())
	}
	if countErr != nil {
		return fmt.Errorf("counting effects: %w", countErr)
	}
	return report(r, v.json)
}

// verdict is what verify found, also printed as JSON (--json) for collecting across servers.
type verdict struct {
	Tool      string `json:"tool"`
	Key       string `json:"key"`        // the policy's key mode: none, meta, argument (pass: "pass")
	OneEffect string `json:"one_effect"` // pass, fail
	SameKey   string `json:"same_key"`   // pass, fail, skipped (no key, or one effect failed)
	RetrySafe bool   `json:"retry_safe"` // an unknown outcome can be retried: one effect and the key holds
	Detail    string `json:"detail,omitempty"`
}

func check(ctx context.Context, upstream *sdk.ClientSession, v verifyArgs, policy mcp.Policy, effects func() int) verdict {
	r := verdict{Tool: v.tool, Key: string(policy.Key), OneEffect: "pass", SameKey: "skipped"}
	if policy.Pass {
		r.Key = "pass"
	}
	if err := mcptest.CheckOneEffect(ctx, upstream, v.tool, policy, json.RawMessage(v.args), effects); err != nil {
		r.OneEffect, r.Detail = "fail", err.Error()
		return r
	}
	if policy.Pass || policy.Key == mcp.KeyNone {
		r.Detail = "no key: after a crash or timeout agentsafe records the outcome as unknown and never retries it"
		return r
	}
	if err := mcptest.CheckSameKey(ctx, upstream, v.tool, policy, json.RawMessage(v.args), effects); err != nil {
		r.SameKey, r.Detail = "fail", err.Error()
		return r
	}
	r.SameKey, r.RetrySafe = "pass", true
	return r
}

// report prints the verdict; an error (a non-zero exit) when a check failed.
func report(r verdict, asJSON bool) error {
	failed := r.OneEffect == "fail" || r.SameKey == "fail"
	if asJSON {
		out, err := json.Marshal(r)
		if err != nil {
			return err
		}
		fmt.Println(string(out))
	} else {
		switch {
		case failed:
			fmt.Println("FAIL:", r.Detail)
		case r.RetrySafe:
			fmt.Printf("PASS: %s makes one effect per call and deduplicates on its key (key: %s): %d simultaneous "+
				"calls with one key made one, with one answer. Safe to retry.\n", r.Tool, r.Key, tooltest.Concurrency)
		default:
			fmt.Printf("PASS: %s makes one effect per call. Not retry-safe (key: %s): %s.\n", r.Tool, r.Key, r.Detail)
		}
	}
	if failed {
		return errors.New("verify failed")
	}
	return nil
}

// verifyArgs are verify's flags, checked.
type verifyArgs struct {
	policy, tool, args, count string
	json                      bool
	settle, timeout           time.Duration
	upstream                  []string
}

func parseVerify(args []string) (verifyArgs, error) {
	var v verifyArgs
	fs := flag.NewFlagSet("agentsafe-mcp verify", flag.ContinueOnError)
	fs.StringVar(&v.policy, "policy", "", "policy file (required): the tool's policy is what's checked")
	fs.StringVar(&v.tool, "tool", "", "the tool to check (required)")
	fs.StringVar(&v.args, "args", "", "arguments of one operation, as JSON (required)")
	fs.StringVar(&v.count, "count", "", "shell command printing how many effects exist so far, e.g. a SELECT count(*) (required)")
	fs.BoolVar(&v.json, "json", false, "print the verdict as JSON")
	fs.DurationVar(&v.settle, "settle", 0, "wait this long before each count, for an API whose lists lag its writes")
	fs.DurationVar(&v.timeout, "timeout", 5*time.Minute, "give up on the whole run after this long (a server or --count that hangs)")
	sandbox := fs.Bool("sandbox", false, "confirm the server is a sandbox: verify makes real calls, with real effects")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: agentsafe-mcp verify --policy FILE --tool NAME --args JSON --count CMD --sandbox [--json] [--settle D] [--timeout D] -- COMMAND [ARGS...]")
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
		return v, errors.New("verify calls the tool for real: run it against a sandbox of the server, and say so with --sandbox")
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
	cmd := exec.CommandContext(ctx, shell, flagC, command) //nolint:gosec // the operator's own command
	cmd.WaitDelay = time.Second                            // a child the shell started can hold the output open after the shell is killed
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("%q: %w", command, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("%q printed %q, not a number", command, strings.TrimSpace(string(out)))
	}
	return n, nil
}
