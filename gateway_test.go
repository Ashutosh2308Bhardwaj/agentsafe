package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// upstream is what a gateway forwards to: a read, and a payment that dedupes by idempotency key (like a
// provider that honours one) and counts what it actually did.
type upstream struct {
	mu    sync.Mutex
	reads int
	paid  map[string]int // effects per key
	plain int            // effects of the keyless write
}

func (u *upstream) tools() []Tool {
	type ref struct {
		Ref string `json:"ref"`
	}
	return []Tool{
		Func("get_balance", "read", func(context.Context, struct{}) (string, error) {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.reads++
			return "1000.00", nil
		}),
		Func("pay", "pay once per ref", func(ctx context.Context, in ref) (string, error) {
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.paid[KeyFrom(ctx)] == 0 {
				u.paid[KeyFrom(ctx)] = 1
			}
			return "paid " + in.Ref, nil
		}, Idempotent("ref")),
		Func("send_note", "a write with no key", func(context.Context, ref) (string, error) {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.plain++
			return "sent", nil
		}),
	}
}

func openTestGateway(t *testing.T, path string, u *upstream, hook func(string)) *Gateway {
	t.Helper()
	g, err := OpenGateway(context.Background(), &FileLog{Path: path}, WithTools(u.tools()...), WithHook(hook),
		WithStartedBy("support-agent"))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func gwCall(t *testing.T, g *Gateway, tool, args string) GatewayResult {
	t.Helper()
	res, err := g.Call(context.Background(), "test-client", tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

func TestGatewayLogsEveryCallAndReplaysRepeats(t *testing.T) {
	u := &upstream{paid: map[string]int{}}
	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	g := openTestGateway(t, path, u, nil)
	defer func() { _ = g.Close() }()

	if r := gwCall(t, g, "get_balance", `{}`); r.Result != `"1000.00"` || r.Replayed {
		t.Fatalf("a read passes through: %+v", r)
	}
	first := gwCall(t, g, "pay", `{"ref":"T-77"}`)
	again := gwCall(t, g, "pay", `{"ref":"T-77"}`)
	if first.Replayed || !again.Replayed || again.Result != first.Result || len(u.paid) != 1 {
		t.Fatalf("a repeated operation is answered from the log and nothing runs: %+v %+v paid=%v", first, again, u.paid)
	}
	if r := gwCall(t, g, "pay", `{"ref":"T-77","extra":1}`); !strings.Contains(r.Result, "error") {
		t.Fatalf("unknown fields are refused: %+v", r)
	}
	if r := gwCall(t, g, "nope", `{}`); !strings.Contains(r.Result, `no tool named`) {
		t.Fatalf("an unknown tool is an error, not a crash: %+v", r)
	}

	assertOpenProxyLog(t, path, 1)
}

// assertOpenProxyLog checks a gateway's log: an open proxy run with the given number of effects, its calls
// logged as received with who sent them.
func assertOpenProxyLog(t *testing.T, path string, effects int) {
	t.Helper()
	events, err := (&FileLog{Path: path}).Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st, err := Rebuild(events)
	if err != nil || st.Kind != KindProxy || st.Status != StatusOpen || st.StartedBy != "support-agent" || len(st.Effects) != effects {
		t.Fatalf("an open proxy run with %d effect(s): err=%v %+v", effects, err, st)
	}
	if events[1].Type != EvCallReceived || events[1].Client != "test-client" {
		t.Fatalf("each call is logged as received, with who sent it: %+v", events[1])
	}
}

// A crash at each point of a call, then a new process opens the log. Whatever happened, nothing runs twice.
func TestGatewaySettlesACallACrashLeftUnfinished(t *testing.T) {
	for _, c := range []struct {
		name, tool, at string
		ran            func(*upstream) int // effects after the restart
		want           string              // what the log says became of the call
	}{
		{"keyed write, killed after it ran", "pay", "after_tool_executed", func(u *upstream) int { return len(u.paid) }, `"paid T-77"`},
		{"keyed write, killed before it was forwarded", "pay", "call_received", func(u *upstream) int { return len(u.paid) }, "nothing was done"},
		{"keyless write, killed after it ran", "send_note", "after_tool_executed", func(u *upstream) int { return u.plain }, "outcome unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := &upstream{paid: map[string]int{}}
			path := filepath.Join(t.TempDir(), "proxy.jsonl")
			g := openTestGateway(t, path, u, func(p string) {
				if p == c.at {
					panic(crash{})
				}
			})
			func() {
				defer func() { _ = recover() }()
				_, _ = g.Call(context.Background(), "test-client", c.tool, json.RawMessage(`{"ref":"T-77"}`))
			}()
			_ = g.Close() // the process died: its lease goes with it

			g2 := openTestGateway(t, path, u, nil) // a new process
			defer func() { _ = g2.Close() }()
			events, _ := (&FileLog{Path: path}).Read(context.Background())
			last := events[len(events)-1]
			if !strings.Contains(last.Result, c.want) {
				t.Fatalf("the unfinished call must be settled with %q, got %s %q", c.want, last.Type, last.Result)
			}
			before := c.ran(u)
			if c.tool == "pay" { // and the same operation, asked again, never runs twice
				_ = gwCall(t, g2, "pay", `{"ref":"T-77"}`)
			}
			if after := c.ran(u); after > 1 || (c.at != "call_received" && before != 1) {
				t.Fatalf("effects: %d after settling, %d after asking again; want exactly one (none if never forwarded)", before, after)
			}
		})
	}
}

func TestGatewayHoldsTheLeaseUntilClosed(t *testing.T) {
	u := &upstream{paid: map[string]int{}}
	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	g := openTestGateway(t, path, u, nil)
	if _, err := OpenGateway(context.Background(), &FileLog{Path: path}, WithTools(u.tools()...)); !errors.Is(err, ErrRunLocked) {
		t.Fatalf("a second gateway on the same log must be refused: %v", err)
	}
	_ = g.Close()
	if _, err := g.Call(context.Background(), "c", "get_balance", json.RawMessage(`{}`)); !errors.Is(err, ErrGatewayClosed) {
		t.Fatalf("a closed gateway takes no calls: %v", err)
	}
	g2 := openTestGateway(t, path, u, nil)
	_ = g2.Close()
}

func TestGatewayRefusesWhatItCantDo(t *testing.T) {
	ctx := context.Background()
	agentLog := &FileLog{Path: filepath.Join(t.TempDir(), "agent.jsonl")}
	r, _ := New(&ScriptedModel{Final: "x"}, agentLog)
	if _, err := r.Start(ctx, "s", "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenGateway(ctx, agentLog); !errors.Is(err, ErrConfig) {
		t.Fatalf("a log holding an agent run isn't a gateway's: %v", err)
	}
	gated := Func("pay", "pay", func(context.Context, struct {
		Ref string `json:"ref"`
	}) (string, error) {
		return "", nil
	}, Idempotent("ref"), NeedsApproval(func(in struct {
		Ref string `json:"ref"`
	}) any {
		return in
	}))
	if _, err := OpenGateway(ctx, &FileLog{Path: filepath.Join(t.TempDir(), "p.jsonl")}, WithTools(gated)); !errors.Is(err, ErrConfig) {
		t.Fatalf("approvals through a gateway aren't supported yet, and must say so: %v", err)
	}
	if _, err := OpenGateway(ctx, nil); !errors.Is(err, ErrConfig) {
		t.Fatalf("no log: %v", err)
	}
}
