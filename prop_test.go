package agentsafe

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// Property tests: random but meaningful run histories, and the rules the state machine promises for all of
// them. Every case is reproducible from its seed (printed on failure). AGENTSAFE_PROP_N raises the count.

func propN(def int) int {
	if n, err := strconv.Atoi(os.Getenv("AGENTSAFE_PROP_N")); err == nil && n > 0 {
		return n
	}
	return def
}

// genHistory is a random walk over legal transitions: at each step it lists the events the state machine
// allows and picks one. It mirrors what a runner, a crash, and a human can produce.
func genHistory(r *rand.Rand) ([]Event, error) {
	var events []Event
	s := NewState()
	nextCall, keys := 0, []string{}
	for len(events) < 200 {
		var options []Event
		switch s.Status {
		case StatusNew:
			options = append(options, Event{Type: EvRunStarted, Task: "t", MaxSteps: 1 + r.Intn(4), By: "scheduler"})
		case StatusAwaitingModel:
			if s.Step < s.Budget {
				var calls []ToolCall
				for range r.Intn(4) {
					nextCall++
					calls = append(calls, ToolCall{ID: fmt.Sprintf("c%d", nextCall), Type: "function",
						Function: FunctionCall{Name: "pay", Arguments: "{}"}})
				}
				msg := &Message{Role: RoleAssistant, ToolCalls: calls}
				if len(calls) == 0 {
					msg.Content = Str("done")
				}
				options = append(options, Event{Type: EvModelDecided, Step: s.Step + 1, Message: msg})
			} else {
				options = append(options, Event{Type: EvRunPaused, Reason: "budget_exhausted"})
			}
			options = append(options, Event{Type: EvRunFinished, Stop: "operator"})
		case StatusExecuting:
			for _, c := range s.Pending {
				d := s.Approvals[c.ID]
				if d == "" || d == "approved" {
					options = append(options, Event{Type: EvToolStarted, CallID: c.ID})
				}
				if s.Started[c.ID] {
					// Reuse a key now and then: the second time it must be a replay.
					key := fmt.Sprintf("k%d", len(keys))
					if len(keys) > 0 && r.Intn(3) == 0 {
						key = keys[r.Intn(len(keys))]
					}
					_, seen := s.Effects[key]
					options = append(options, Event{Type: EvToolResult, CallID: c.ID, Key: key, Result: `"ok"`, Replayed: seen})
				} else {
					options = append(options, Event{Type: EvToolRefused, CallID: c.ID, Result: `{"error":"refused"}`})
					if d == "" {
						options = append(options, Event{Type: EvApprovalRequested, CallID: c.ID, Key: "op-" + c.ID, Tool: "pay"})
					}
				}
			}
		case StatusAwaitingApproval:
			w := s.Waiting
			options = append(options,
				Event{Type: EvApprovalDecided, CallID: w.CallID, Key: w.Key, Decision: "approved", By: "ops"},
				Event{Type: EvApprovalDecided, CallID: w.CallID, Key: w.Key, Decision: "rejected", By: "ops"},
				Event{Type: EvApprovalDenied, CallID: w.CallID, Key: w.Key, Decision: "approved", By: "intern", Reason: "not allowed"})
		case StatusAnswered:
			options = append(options, Event{Type: EvRunFinished, Stop: "stop"})
		case StatusPaused:
			options = append(options, Event{Type: EvBudgetExtended, ExtraSteps: 1 + r.Intn(2), By: "ops"})
		case StatusFinished:
			return events, nil
		}
		e := options[r.Intn(len(options))]
		e.Seq, e.V = len(events)+1, FormatVersion
		if err := s.Apply(e); err != nil {
			return events, fmt.Errorf("a legal-looking %s was refused after %d events: %w", e.Type, len(events), err)
		}
		if e.Type == EvToolResult && !e.Replayed {
			keys = append(keys, e.Key)
		}
		events = append(events, e)
		if err := invariants(s, events); err != nil {
			return events, err
		}
	}
	return events, nil
}

// invariants are what every reachable state must satisfy.
func invariants(s State, events []Event) error {
	switch {
	case s.Status == StatusFinished && len(s.Pending) > 0:
		return errors.New("finished with calls pending")
	case (s.Waiting != nil) != (s.Status == StatusAwaitingApproval):
		return errors.New("Waiting is set iff the run awaits approval")
	case s.Step > s.Budget:
		return fmt.Errorf("step %d beyond budget %d", s.Step, s.Budget)
	case s.Status == StatusExecuting && len(s.Pending) == 0:
		return errors.New("executing with nothing pending")
	case s.Status == StatusAwaitingModel && len(s.Pending) > 0:
		return errors.New("asking the model with calls pending")
	}
	for id := range s.Started {
		if !s.isPending(id) {
			return fmt.Errorf("started call %s isn't pending", id)
		}
	}
	executed := map[string]int{}
	for _, e := range events {
		if e.Type == EvToolResult && e.Key != "" && !e.Replayed {
			executed[e.Key]++
		}
	}
	for k, n := range executed {
		if n != 1 {
			return fmt.Errorf("key %s has %d executed results", k, n)
		}
		if _, ok := s.Effects[k]; !ok {
			return fmt.Errorf("key %s executed but not indexed", k)
		}
	}
	return nil
}

func TestPropertyLegalHistoriesAndEveryPrefixRebuild(t *testing.T) {
	for i := range propN(400) {
		seed := int64(i)
		events, err := genHistory(rand.New(rand.NewSource(seed)))
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		for n := 0; n <= len(events); n++ { // a crash leaves a prefix: every prefix is a valid run
			if _, err := Rebuild(events[:n]); err != nil {
				t.Fatalf("seed %d: prefix of %d events refused: %v", seed, n, err)
			}
		}
	}
}

// Each corruption breaks a rule the runner relies on, so Rebuild must refuse it, whatever history it's in.
func TestPropertyCorruptionsAreRefused(t *testing.T) {
	type mutation struct {
		name  string
		apply func(r *rand.Rand, ev []Event) ([]Event, bool) // false: doesn't apply to this history
	}
	pick := func(r *rand.Rand, ev []Event, ok func(Event) bool) int {
		var idx []int
		for i, e := range ev {
			if ok(e) {
				idx = append(idx, i)
			}
		}
		if len(idx) == 0 {
			return -1
		}
		return idx[r.Intn(len(idx))]
	}
	insert := func(ev []Event, at int, e Event) []Event {
		return append(append(append([]Event{}, ev[:at]...), e), ev[at:]...)
	}
	mutations := []mutation{
		{"a second result for one call", func(r *rand.Rand, ev []Event) ([]Event, bool) {
			i := pick(r, ev, func(e Event) bool { return e.Type == EvToolResult })
			if i < 0 {
				return nil, false
			}
			return insert(ev, i+1, ev[i]), true
		}},
		{"a result whose call never started", func(r *rand.Rand, ev []Event) ([]Event, bool) {
			i := pick(r, ev, func(e Event) bool { return e.Type == EvToolResult })
			if i < 0 {
				return nil, false
			}
			var out []Event
			for _, e := range ev {
				if e.Type != EvToolStarted || e.CallID != ev[i].CallID {
					out = append(out, e)
				}
			}
			return out, true
		}},
		{"an approval skipped", func(r *rand.Rand, ev []Event) ([]Event, bool) {
			i := pick(r, ev, func(e Event) bool { return e.Type == EvApprovalDecided && e.Decision == "approved" })
			if i < 0 || i == len(ev)-1 {
				return nil, false
			}
			return append(append([]Event{}, ev[:i]...), ev[i+1:]...), true
		}},
		{"anything after run_finished", func(r *rand.Rand, ev []Event) ([]Event, bool) {
			if len(ev) == 0 || ev[len(ev)-1].Type != EvRunFinished {
				return nil, false
			}
			return append(append([]Event{}, ev...), ev[r.Intn(len(ev))]), true
		}},
		{"a model step out of order", func(r *rand.Rand, ev []Event) ([]Event, bool) {
			i := pick(r, ev, func(e Event) bool { return e.Type == EvModelDecided })
			if i < 0 {
				return nil, false
			}
			out := append([]Event{}, ev...)
			out[i].Step += 1 + r.Intn(3)
			return out, true
		}},
		{"run_finished with calls pending", func(r *rand.Rand, ev []Event) ([]Event, bool) {
			i := pick(r, ev, func(e Event) bool { return e.Type == EvModelDecided && len(e.Message.ToolCalls) > 0 })
			if i < 0 {
				return nil, false
			}
			return insert(ev[:i+1], i+1, Event{Type: EvRunFinished, Stop: "stop"}), true
		}},
		{"asking again about a rejected operation", func(r *rand.Rand, ev []Event) ([]Event, bool) {
			i := pick(r, ev, func(e Event) bool { return e.Type == EvApprovalDecided && e.Decision == "rejected" })
			if i < 0 {
				return nil, false
			}
			// After the rejection is resolved (refused), the same operation comes back under another call id.
			j := i + 1
			for j < len(ev) && (ev[j].Type != EvToolRefused || ev[j].CallID != ev[i].CallID) {
				j++
			}
			if j >= len(ev) {
				return nil, false
			}
			out := append([]Event{}, ev[:j+1]...)
			st, err := Rebuild(out)
			if err != nil {
				return nil, false
			}
			again := ""
			switch {
			case st.Status == StatusExecuting: // another call of the same turn is still pending
				for _, c := range st.Pending {
					if !st.Started[c.ID] && st.Approvals[c.ID] == "" {
						again = c.ID
					}
				}
			case st.Status == StatusAwaitingModel && st.Step < st.Budget: // the model proposes it again
				again = "again-" + ev[i].CallID
				out = append(out, Event{Type: EvModelDecided, Step: st.Step + 1, Message: &Message{Role: RoleAssistant,
					ToolCalls: []ToolCall{{ID: again, Type: "function", Function: FunctionCall{Name: "pay", Arguments: "{}"}}}}})
			}
			if again == "" {
				return nil, false
			}
			return append(out, Event{Type: EvApprovalRequested, CallID: again, Key: ev[i].Key, Tool: "pay"}), true
		}},
		{"a second run_started", func(r *rand.Rand, ev []Event) ([]Event, bool) {
			if len(ev) < 2 {
				return nil, false
			}
			return insert(ev, 1+r.Intn(len(ev)-1), ev[0]), true
		}},
	}
	applied := map[string]int{}
	for i := range propN(400) {
		seed := int64(i)
		r := rand.New(rand.NewSource(seed))
		events, err := genHistory(r)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		for _, m := range mutations {
			bad, ok := m.apply(r, events)
			if !ok {
				continue
			}
			applied[m.name]++
			if _, err := Rebuild(bad); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("seed %d: %s must be refused, got %v", seed, m.name, err)
			}
		}
	}
	for _, m := range mutations { // a mutation that never applied would pass vacuously
		if applied[m.name] < 20 {
			t.Errorf("%q applied to only %d histories: the generator doesn't reach it", m.name, applied[m.name])
		}
	}
}

// End to end with the real runner: random plans, random decisions, crashes at random points, every restart
// in a new "process". Whatever happens, no operation pays twice, a rejected payout never pays, and the log
// always rebuilds.
func TestPropertyRunnerUnderRandomCrashesPaysAtMostOnce(t *testing.T) {
	for i := range propN(150) {
		seed := int64(i)
		if err := runOnce(t, seed); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
	}
}

type propGateway struct {
	mu    sync.Mutex
	paid  map[string]int // effects per key: the gateway dedupes, so >1 here is impossible; we count attempts too
	calls int
}

func (g *propGateway) pay(ctx context.Context, p struct {
	Ref string `json:"ref"`
}) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	k := KeyFrom(ctx)
	if g.paid[k] == 0 {
		g.paid[k] = 1
	}
	return "paid " + p.Ref, nil
}

func runOnce(t *testing.T, seed int64) error {
	r := rand.New(rand.NewSource(seed))
	gw := &propGateway{paid: map[string]int{}}
	gated := r.Intn(2) == 0
	opts := []FuncOption{Idempotent("ref")}
	if gated {
		opts = append(opts, NeedsApproval(func(p struct {
			Ref string `json:"ref"`
		}) any {
			return p
		}))
	}
	tool := Func("pay", "pay", gw.pay, opts...)
	var plan []FunctionCall
	for range 1 + r.Intn(4) {
		plan = append(plan, FunctionCall{Name: "pay", Arguments: fmt.Sprintf(`{"ref":"R%d"}`, r.Intn(3))}) // repeats on purpose
	}
	path := filepath.Join(t.TempDir(), "run.jsonl")
	points := []string{"after_model_call", "after_model_logged", "before_tool_executed", "after_tool_executed",
		"after_result_logged", "approval_requested"}
	newRunner := func(crash bool) *Runner {
		var hook func(string)
		if crash {
			point, nth, seen := points[r.Intn(len(points))], 1+r.Intn(2), 0
			hook = func(p string) {
				if p == point {
					if seen++; seen == nth {
						panic("kill -9 at " + p)
					}
				}
			}
		}
		run, err := New(&ScriptedModel{Plan: plan, Final: "done"}, &FileLog{Path: path},
			WithTools(tool), WithMaxSteps(20), WithAnyApprover(), WithHook(hook))
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	ctx := context.Background()
	rejected := map[string]bool{}
	step := func(f func(*Runner) (State, error)) (State, error) {
		run := newRunner(r.Intn(3) == 0) // a third of the processes crash somewhere
		var st State
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("crashed: %v", p)
				}
			}()
			st, err = f(run)
		}()
		return st, err
	}
	if _, err := step(func(run *Runner) (State, error) { return run.Start(ctx, "sys", "task") }); err != nil && !isCrash(err) {
		return err
	}
	for attempt := 0; ; attempt++ {
		if attempt > 50 {
			return errors.New("the run never finished")
		}
		events, err := (&FileLog{Path: path}).Read(ctx)
		if err != nil {
			return fmt.Errorf("log unreadable: %w", err)
		}
		if len(events) == 0 { // crashed before run_started was written
			if _, err := step(func(run *Runner) (State, error) { return run.Start(ctx, "sys", "task") }); err != nil && !isCrash(err) {
				return err
			}
			continue
		}
		st, err := Rebuild(events)
		if err != nil {
			return fmt.Errorf("log doesn't rebuild: %w", err)
		}
		if st.Status == StatusFinished {
			break
		}
		if st.Status == StatusAwaitingApproval {
			key := st.Waiting.Key
			if r.Intn(3) == 0 {
				rejected[key] = true
				_, err = step(func(run *Runner) (State, error) { return run.Reject(ctx, key, "ops", "no") })
			} else {
				_, err = step(func(run *Runner) (State, error) { return run.Approve(ctx, key, "ops") })
			}
		} else {
			_, err = step(func(run *Runner) (State, error) { return run.Continue(ctx) })
		}
		if err != nil && !isCrash(err) && !errors.Is(err, ErrAlreadyDecided) {
			return err
		}
	}
	for key, n := range gw.paid {
		if n > 1 {
			return fmt.Errorf("key %s paid %d times", key, n)
		}
		if rejected[key] && n > 0 {
			return fmt.Errorf("rejected key %s was paid", key)
		}
	}
	// Distinct refs paid at most once each, however many times the model proposed them.
	refs := map[string]bool{}
	for _, c := range plan {
		refs[c.Arguments] = true
	}
	if len(gw.paid) > len(refs) {
		return fmt.Errorf("%d keys paid for %d distinct operations", len(gw.paid), len(refs))
	}
	return nil
}

func isCrash(err error) bool {
	return err != nil && len(err.Error()) > 7 && err.Error()[:7] == "crashed"
}
