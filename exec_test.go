package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gateway is a payment gateway with idempotency keys. Its behaviour per call is scripted by `then`:
// "ok", "hang" (charge, then never answer), "hang-before" (never answer, nothing charged), "504" (charge, then
// say the outcome is unknown), "decline", "panic", "ctx" (charge, then the client's own timeout fires).
type gateway struct {
	mu      sync.Mutex
	charged map[string]bool
	paid    int
	calls   int
	then    []string
	stuck   chan struct{} // closed at cleanup, releasing hung calls
	entered chan struct{} // one signal per call, on entry
}

// counts reads the counters under the lock: abandoned (timed-out) calls are still running in their goroutines.
func (g *gateway) counts() (paid, calls int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paid, g.calls
}

func newGateway(t *testing.T, then ...string) *gateway {
	g := &gateway{charged: map[string]bool{}, then: then, stuck: make(chan struct{}), entered: make(chan struct{}, 16)}
	t.Cleanup(func() { close(g.stuck) })
	return g
}

func (g *gateway) Spec() ToolSpec {
	return ToolSpec{Name: "charge", Description: "charge", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (g *gateway) Call(context.Context, json.RawMessage) (any, error) { panic("must use CallWithKey") }
func (g *gateway) Identity(a json.RawMessage) (any, any, error) {
	return map[string]any{"a": string(a)}, nil, nil
}
func (g *gateway) charge(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.charged[key] {
		g.charged[key] = true
		g.paid++
	}
}
func (g *gateway) CallWithKey(_ context.Context, key string, _ json.RawMessage) (any, error) {
	g.entered <- struct{}{}
	g.mu.Lock()
	g.calls++
	mode := "ok"
	if g.calls <= len(g.then) {
		mode = g.then[g.calls-1]
	}
	g.mu.Unlock()
	switch mode {
	case "hang":
		g.charge(key)
		<-g.stuck // ignores ctx: the worst case
		return nil, errors.New("too late")
	case "hang-before":
		<-g.stuck
		return nil, errors.New("too late")
	case "504":
		g.charge(key)
		return nil, fmt.Errorf("gateway 504: %w", ErrOutcomeUnknown)
	case "decline":
		return nil, errors.New("card declined")
	case "panic":
		g.charge(key)
		panic("nil map in the gateway client")
	case "ctx":
		g.charge(key) // the request went out; then the client's OWN deadline expired (the runner's hasn't)
		return nil, fmt.Errorf("post /charges: %w", context.DeadlineExceeded)
	}
	g.charge(key)
	return map[string]any{"paid": true}, nil
}

func chargeRun(t *testing.T, tools ...Tool) *Runner {
	var plan []FunctionCall
	for _, tl := range tools {
		plan = append(plan, FunctionCall{Name: tl.Spec().Name, Arguments: `{"ref":"A"}`})
	}
	return &Runner{Model: &ScriptedModel{Plan: plan, Final: "done"}, Tools: tools, MaxSteps: 8,
		Log: &FileLog{Path: filepath.Join(t.TempDir(), "run.jsonl")}, ToolTimeout: 100 * time.Millisecond,
		ToolBackoff: time.Millisecond}
}

func results(t *testing.T, r *Runner) []string {
	t.Helper()
	events, err := r.Log.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if e.Type == EvToolResult {
			out = append(out, e.Result)
		}
	}
	return out
}

func TestChargedThenHungIsRetriedWithTheSameKeyAndPaysOnce(t *testing.T) {
	g := newGateway(t, "hang") // charges, then the response never comes
	r := chargeRun(t, g)
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil || st.Status != StatusFinished {
		t.Fatalf("err=%v status=%s", err, st.Status)
	}
	if paid, calls := g.counts(); paid != 1 || calls != 2 {
		t.Fatalf("want one payment over two calls with the same key, got paid=%d calls=%d", paid, calls)
	}
	if res := results(t, r); len(res) != 1 || res[0] != `{"paid":true}` {
		t.Fatalf("the log must hold the real outcome, never 'timed out': %v", res)
	}
}

func TestStillUnknownAfterAllAttemptsIsLeftInDoubtNotFailed(t *testing.T) {
	g := newGateway(t, "hang", "hang", "hang")
	r := chargeRun(t, g)
	_, err := r.Start(context.Background(), "sys", "task")
	if !errors.Is(err, ErrInDoubt) || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("want ErrInDoubt, got %v", err)
	}
	if paid, calls := g.counts(); calls != 3 || paid != 1 {
		t.Fatalf("want 3 attempts and 1 payment, got calls=%d paid=%d", calls, paid)
	}
	if res := results(t, r); len(res) != 0 {
		t.Fatalf("nothing may be logged for an operation in doubt (it would be replayed as its outcome): %v", res)
	}
	// The gateway recovers; a later Continue (any process) retries the same key and learns the truth.
	st, err := (&Runner{Model: r.Model, Tools: r.Tools, Log: r.Log, ToolTimeout: time.Second}).Continue(context.Background())
	if paid, _ := g.counts(); err != nil || st.Status != StatusFinished || paid != 1 {
		t.Fatalf("resume must resolve it without paying again: err=%v status=%s paid=%d", err, st.Status, paid)
	}
	if res := results(t, r); len(res) != 1 || res[0] != `{"paid":true}` {
		t.Fatalf("got %v", res)
	}
}

func TestToolSaysOutcomeUnknownIsRetried(t *testing.T) {
	g := newGateway(t, "504")
	r := chargeRun(t, g)
	st, err := r.Start(context.Background(), "sys", "task")
	if paid, calls := g.counts(); err != nil || st.Status != StatusFinished || paid != 1 || calls != 2 {
		t.Fatalf("err=%v paid=%d calls=%d", err, paid, calls)
	}
}

func TestToolHonouringItsContextIsStillUnknown(t *testing.T) {
	g := newGateway(t, "ctx") // e.g. an HTTP client with its own timeout: "deadline exceeded" is not "declined"
	r := chargeRun(t, g)
	st, err := r.Start(context.Background(), "sys", "task")
	if paid, calls := g.counts(); err != nil || st.Status != StatusFinished || paid != 1 || calls != 2 {
		t.Fatalf("a context error is not a 'no': err=%v paid=%d calls=%d", err, paid, calls)
	}
	if res := results(t, r); res[0] != `{"paid":true}` {
		t.Fatalf("got %v", res)
	}
}

func TestKnownFailureIsLoggedAndNotRetried(t *testing.T) {
	g := newGateway(t, "decline")
	r := chargeRun(t, g)
	_, err := r.Start(context.Background(), "sys", "task")
	if _, calls := g.counts(); err != nil || calls != 1 {
		t.Fatalf("a decline is an answer: err=%v calls=%d", err, calls)
	}
	if res := results(t, r); len(res) != 1 || !strings.Contains(res[0], "card declined") {
		t.Fatalf("got %v", res)
	}
}

func TestPanickingIdempotentToolIsInDoubtNotRetriedProcessSurvives(t *testing.T) {
	g := newGateway(t, "panic")
	r := chargeRun(t, g)
	_, err := r.Start(context.Background(), "sys", "task")
	var p *PanicError
	if !errors.Is(err, ErrInDoubt) || !errors.As(err, &p) || !strings.Contains(fmt.Sprint(p.Value), "nil map") || len(p.Stack) == 0 {
		t.Fatalf("want ErrInDoubt carrying the panic and its stack, got %v", err)
	}
	if _, calls := g.counts(); calls != 1 || len(results(t, r)) != 0 {
		t.Fatalf("a panic is a bug: no automatic retry, nothing logged; calls=%d", calls)
	}
	// After the fix is deployed, Continue retries the same key: the gateway dedupes.
	st, err := r.Continue(context.Background())
	if paid, _ := g.counts(); err != nil || st.Status != StatusFinished || paid != 1 {
		t.Fatalf("err=%v paid=%d", err, paid)
	}
}

func TestCancellingTheRunStopsRetryingImmediately(t *testing.T) {
	g := newGateway(t, "hang-before", "hang-before", "hang-before")
	r := chargeRun(t, g)
	r.ToolTimeout, r.ToolBackoff = 0, time.Hour // no timeout, and a backoff we must not wait for
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-g.entered; cancel() }() // cancel once the call is in flight, not on a timer
	t0 := time.Now()
	_, err := r.Start(ctx, "sys", "task")
	took := time.Since(t0)
	time.Sleep(100 * time.Millisecond) // a second call would have arrived by now
	if _, calls := g.counts(); !errors.Is(err, ErrInDoubt) || took > 5*time.Second || calls != 1 {
		t.Fatalf("want a prompt ErrInDoubt with no retry, got %v after %s, calls=%d", err, took, calls)
	}
}

// plain is a non-idempotent tool.
type plain struct {
	name    string
	calls   atomic.Int32
	do      func(ctx context.Context) (any, error)
	timeout time.Duration
}

func (p *plain) Spec() ToolSpec {
	return ToolSpec{Name: p.name, Description: p.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (p *plain) Call(ctx context.Context, _ json.RawMessage) (any, error) {
	p.calls.Add(1)
	return p.do(ctx)
}

type timed struct{ *plain }

func (t timed) Timeout() time.Duration { return t.timeout }

func TestPlainToolUnknownOutcomeIsToldToTheModelNotRetried(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	hang := &plain{name: "notify", do: func(context.Context) (any, error) { <-stuck; return nil, nil }}
	boom := &plain{name: "export", do: func(context.Context) (any, error) { panic("index out of range") }}
	r := chargeRun(t, hang, boom)
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil || st.Status != StatusFinished {
		t.Fatalf("plain tools' failures go to the model, the run goes on: err=%v status=%s", err, st.Status)
	}
	res := results(t, r)
	if hang.calls.Load() != 1 || boom.calls.Load() != 1 || len(res) != 2 {
		t.Fatalf("no retries for tools without a key: %d %d %v", hang.calls.Load(), boom.calls.Load(), res)
	}
	for i, want := range []string{"did not return within 100ms", "panicked: index out of range"} {
		if !strings.Contains(res[i], want) || !strings.Contains(res[i], "may have taken effect") {
			t.Fatalf("the model must be told the outcome is unknown: %s", res[i])
		}
	}
}

func TestTimeoutToolOverridesTheRunnerDefault(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	slow := timed{&plain{name: "slow", timeout: 10 * time.Millisecond, do: func(context.Context) (any, error) { <-stuck; return nil, nil }}}
	r := chargeRun(t, slow)
	r.ToolTimeout = 0 // no default limit; the tool's own applies
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	if res := results(t, r); !strings.Contains(res[0], "did not return within 10ms") {
		t.Fatalf("got %v", res)
	}
}

func TestAResultThatArrivesInTimeIsKept(t *testing.T) {
	fast := &plain{name: "fast", do: func(context.Context) (any, error) { return map[string]int{"n": 1}, nil }}
	r := chargeRun(t, fast)
	r.ToolTimeout = time.Second
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	if res := results(t, r); res[0] != `{"n":1}` {
		t.Fatalf("got %v", res)
	}
}
