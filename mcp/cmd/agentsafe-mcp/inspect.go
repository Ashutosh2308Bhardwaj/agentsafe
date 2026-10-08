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

	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
)

// inspect lists a server's tools, what each says about itself, and a starting policy, without calling any of them
// (mcp.Inspect). --policy-out writes the starting policy file, with the person running it as the approver.
func inspect(args []string) error {
	fs := flag.NewFlagSet("agentsafe-mcp inspect", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the inspection as JSON")
	out := fs.String("policy-out", "", "write a starting policy file here (it must not exist yet)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: agentsafe-mcp inspect [--json] [--policy-out FILE] -- COMMAND [ARGS...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("the upstream command is required")
	}
	ctx := context.Background()
	upstream, err := startUpstream(ctx, fs.Args())
	if err != nil {
		return err
	}
	defer func() { _ = upstream.Close() }()
	in, err := mcp.Inspect(ctx, upstream)
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
		fmt.Printf("\nWrote %s. Read it before you use it: identity is a guess from the required arguments, and a key "+
			"argument is only safe if the server deduplicates on it (agentsafe-mcp verify).\n", *out)
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
	fmt.Printf("%s %s: %d tools, %d read-only, %d that change something (%d with a key argument); %d annotated.\n\n",
		in.Server, in.Version, len(in.Tools), len(in.Tools)-writes, writes, keyed, annotated)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TOOL\tKIND\tKEY ARGUMENT\tSUGGESTED POLICY")
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
		return "none: stays hidden (write identity by hand)"
	}
	if p.Pass {
		return "pass"
	}
	parts := []string{"identity " + strings.Join(p.Identity, ",")}
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
