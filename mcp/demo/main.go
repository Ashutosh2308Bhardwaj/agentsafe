// Command demo shows what agentsafe-mcp does, with no credentials: an agent charges a customer through a billing
// MCP server, the answer is lost, and the agent asks again. Straight to the server the customer is charged twice;
// through agentsafe-mcp, once. Run it with ../demo.sh, which builds the two programs it needs.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	proxy := flag.String("proxy", "", "the agentsafe-mcp binary")
	billing := flag.String("billing", "", "the billing MCP server binary (internal/fakeupstream)")
	work := flag.String("work", "", "a scratch directory")
	flag.Parse()
	d := demo{out: os.Stdout, proxy: *proxy, billing: *billing, work: *work, color: isTerminal(os.Stdout)}
	if _, err := d.run(); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

type demo struct {
	out                  io.Writer
	proxy, billing, work string
	color                bool
}

// charges are what each scene left in the books: what the customer was actually billed.
type charges struct{ direct, proxied, crashNoKey, crashKey int }

const charge = `{"ticket_id":"T-1","amount":"10.00"}`

func (d demo) run() (charges, error) {
	var c charges
	var err error
	d.say("An agent charges a customer $10.00 for ticket T-1. The answer gets lost, so it asks again.\n\n")

	d.say(d.bold("1. The answer is lost on the way back, so the agent retries\n"))
	d.say("   Straight to the billing server:\n")
	if c.direct, err = d.retry("direct", ""); err != nil {
		return c, err
	}
	d.verdict(c.direct)
	d.say("   Through agentsafe-mcp:\n")
	if c.proxied, err = d.retry("proxied", `{"identity":["ticket_id"]}`); err != nil {
		return c, err
	}
	d.verdict(c.proxied)

	d.say(d.bold("\n2. agentsafe-mcp itself crashes after the charge, before the answer reaches the agent\n"))
	d.say("   The billing server can't deduplicate (no key it reads):\n")
	if c.crashNoKey, err = d.crash("crash-nokey", `{"identity":["ticket_id"]}`); err != nil {
		return c, err
	}
	d.verdict(c.crashNoKey)
	d.say("   The billing server deduplicates on an idempotency key, which agentsafe sends (the retry\n" +
		"   carries the same key, so the server returns its first answer):\n")
	if c.crashKey, err = d.crash("crash-key",
		`{"identity":["ticket_id"],"key":"argument","key_argument":"idempotency_key"}`); err != nil {
		return c, err
	}
	d.verdict(c.crashKey)

	d.say("\nagentsafe logs each operation before it runs. A repeat is answered from the log; an outcome it\n" +
		"couldn't see is retried only with a key the server deduplicates on, and otherwise reported as unknown,\n" +
		"never guessed. The log of this run: " + filepath.Join(d.work, "crash-nokey", "calls.jsonl") + "\n")
	return c, nil
}

// retry: one session, the same call twice, as a client does when the first answer times out. policy is the charge
// tool's in agentsafe-mcp; "" is straight to the billing server.
func (d demo) retry(scene, policy string) (int, error) {
	dir, err := d.scene(scene)
	if err != nil {
		return 0, err
	}
	agent, err := d.connect(dir, policy, "")
	if err != nil {
		return 0, err
	}
	defer func() { _ = agent.Close() }()
	for i, label := range []string{"call ", "retry"} {
		text, replayed, err := call(agent)
		if err != nil {
			return 0, err
		}
		note := ""
		if replayed {
			note = d.dim("   answered from agentsafe's log, not charged again")
		}
		d.say(fmt.Sprintf("     %s → %s%s\n", label, text, note))
		if i == 0 {
			d.say(d.dim("     (the answer is lost: the agent times out and asks again)\n"))
		}
	}
	return books(dir)
}

// crash: the proxy is killed right after the server charged; a new one starts and the agent asks again.
func (d demo) crash(scene, policy string) (int, error) {
	dir, err := d.scene(scene)
	if err != nil {
		return 0, err
	}
	agent, err := d.connect(dir, policy, "after_tool_executed")
	if err != nil {
		return 0, err
	}
	if _, _, err := call(agent); err == nil {
		return 0, fmt.Errorf("%s: the proxy should have been killed mid-call", scene)
	}
	_ = agent.Close()
	d.say("     call  → " + d.dim("(agentsafe-mcp killed after the server charged: no answer)") + "\n")
	agent, err = d.connect(dir, policy, "")
	if err != nil {
		return 0, err
	}
	defer func() { _ = agent.Close() }()
	text, _, err := call(agent)
	if err != nil {
		return 0, err
	}
	d.say(d.dim("     (a new agentsafe-mcp starts on the same log; the agent asks again)") + "\n")
	d.say("     retry → " + text + "\n")
	return books(dir)
}

func (d demo) scene(name string) (string, error) {
	dir := filepath.Join(d.work, name)
	return dir, os.MkdirAll(dir, 0o750)
}

// connect starts the agent's MCP session: to the billing server, or ("" policy aside) to agentsafe-mcp in front of
// it, with that policy for charge. killAt kills agentsafe-mcp at that point (AGENTSAFE_KILL_AT).
func (d demo) connect(dir, policy, killAt string) (*sdk.ClientSession, error) {
	billing := []string{d.billing, "--state", filepath.Join(dir, "books.json")}
	argv := billing
	if policy != "" {
		file := filepath.Join(dir, "policy.json")
		if err := os.WriteFile(file, []byte(`{"tools":{"charge":`+policy+`}}`), 0o600); err != nil {
			return nil, err
		}
		argv = append([]string{d.proxy, "--quiet", "--policy", file, "--log", filepath.Join(dir, "calls.jsonl"), "--"}, billing...)
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the demo's own binaries
	cmd.Env = append(os.Environ(), "AGENTSAFE_KILL_AT="+killAt)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return sdk.NewClient(&sdk.Implementation{Name: "demo-agent", Version: "1"}, nil).
		Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
}

// call makes the charge and returns the answer's text and whether agentsafe answered it from its log.
func call(agent *sdk.ClientSession) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := agent.CallTool(ctx, &sdk.CallToolParams{Name: "charge", Arguments: json.RawMessage(charge)})
	if err != nil {
		return "", false, err
	}
	var text strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*sdk.TextContent); ok {
			text.WriteString(t.Text)
		}
	}
	s := text.String()
	if i := strings.Index(s, "outcome unknown"); i >= 0 {
		s = "outcome unknown: not retried, never guessed (check the billing system)"
	}
	return s, res.Meta["io.github.ashutosh2308bhardwaj.agentsafe/replayed"] == true, nil
}

// books is how many times the customer was charged in a scene.
func books(dir string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "books.json")) //nolint:gosec // the demo's own file
	if err != nil {
		return 0, err
	}
	var b struct{ Charges int }
	return b.Charges, json.Unmarshal(raw, &b)
}

func (d demo) verdict(n int) {
	times := "times"
	if n == 1 {
		times = "time"
	}
	mark, paint := "✓", "32"
	if n != 1 {
		mark, paint = "✗", "31"
	}
	d.say(fmt.Sprintf("     customer charged %s\n", d.paint(paint, fmt.Sprintf("%d %s %s", n, times, mark))))
}

func (d demo) say(s string)         { _, _ = io.WriteString(d.out, s) }
func (d demo) bold(s string) string { return d.paint("1", s) }
func (d demo) dim(s string) string  { return d.paint("2", s) }
func (d demo) paint(code, s string) string {
	if !d.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == ""
}
