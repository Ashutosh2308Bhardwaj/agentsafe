package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
)

// inspect lists a server's tools, what each says about itself, and a starting policy, without calling any of them
// (mcp.Inspect). --policy-out writes the starting policy file, with the person running it as the approver. The
// suggestion doesn't trust the server's hints unless --trust-annotations says to, and never uses a candidate key.
func inspect(args []string) error {
	fs := flag.NewFlagSet("agentsafe-mcp inspect", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the inspection as JSON")
	out := fs.String("policy-out", "", "write a starting policy file here (it must not exist yet)")
	timeout := fs.Duration("timeout", time.Minute, "give up on a server that hasn't listed its tools by then")
	trust := fs.Bool("trust-annotations", false, "let the server's hints relax the policy: read-only tools pass, "+
		"writes marked not destructive need no approval (MCP calls the hints untrusted)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: agentsafe-mcp inspect [--json] [--policy-out FILE] [--trust-annotations] -- COMMAND [ARGS...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("the upstream command is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	upstream, err := startUpstream(ctx, fs.Args())
	if err != nil {
		return err
	}
	defer func() { _ = upstream.Close() }()
	var opts []mcp.InspectOption
	if *trust {
		opts = append(opts, mcp.TrustAnnotations())
	}
	in, err := mcp.Inspect(ctx, upstream, opts...)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := writeStarter(*out, in); err != nil {
			return err
		}
	}
	if *asJSON {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	}
	printInspection(in)
	if *out != "" {
		fmt.Printf("\nWrote %s. Read it before you use it: identity [\"*\"] (all arguments) never refuses a call but sees no "+
			"conflicts: name the fields that identify an operation where you know them. No tool gets a key: for a "+
			"candidate key, run agentsafe-mcp verify with key: argument, and switch to it if it passes. Tools left "+
			"out stay hidden until you add a policy.\n", *out)
	}
	return nil
}

func writeStarter(path string, in mcp.Inspection) error {
	who, err := osIdentity()
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(in.Starter(who), "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator's own path
	if err != nil {
		return fmt.Errorf("writing the starting policy: %w", err)
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func printInspection(in mcp.Inspection) {
	var writes, keyed, annotated int
	for _, t := range in.Tools {
		if t.Annotated {
			annotated++
		}
		if t.Write {
			writes++
			if t.KeyArgument != "" {
				keyed++
			}
		}
	}
	fmt.Printf("%s %s: %d tools, %d read-only, %d that change something (%d with a candidate key); %d annotated.\n\n",
		in.Server, in.Version, len(in.Tools), len(in.Tools)-writes, writes, keyed, annotated)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TOOL\tKIND\tCANDIDATE KEY\tSUGGESTED POLICY")
	for _, t := range in.Tools {
		kind := "read"
		if t.Write {
			kind = "write"
			if t.Destructive {
				kind = "write, destructive"
			}
			if !t.Annotated {
				kind += " (assumed)"
			}
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.Name, kind, orDash(t.KeyArgument), describe(t.Policy))
	}
	_ = w.Flush()
}

// describe is a policy in a few words.
func describe(p *mcp.Policy) string {
	if p == nil {
		return "hidden until reviewed"
	}
	if p.Pass {
		return "pass"
	}
	parts := []string{"identity " + strings.Join(p.Identity, ",")}
	if len(p.Identity) == 1 && p.Identity[0] == mcp.AllArguments {
		parts[0] = "all arguments"
	}
	if p.Key == mcp.KeyArgument {
		parts = append(parts, "key → "+p.KeyArgument)
	} else {
		parts = append(parts, "no key (not retry-safe)")
	}
	if p.Approval == "always" {
		parts = append(parts, "approval")
	}
	return strings.Join(parts, "; ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
