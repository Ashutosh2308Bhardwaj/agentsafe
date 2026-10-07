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
	g := &historyGen{r: r}
	var events []Event
	s := NewState()
	for len(events) < 200 {
		if s.Status == StatusFinished {
			return events, nil
		}
		options := g.options(s)
		e := options[r.Intn(len(options))]
		e.Seq, e.V = len(events)+1, FormatVersion
		if err := s.Apply(e); err != nil {
			return events, fmt.Errorf("a legal-looking %s was refused after %d events: %w", e.Type, len(events), err)
		}
		if e.Type == EvToolResult && !e.Replayed {
			g.keys = append(g.keys, e.Key)
		}
		events = append(events, e)
		if err := invariants(s, events); err != nil {
			return events, err
		}
	}
	return events, nil
}

// historyGen proposes the legal next events for a state.
type historyGen struct {
	r        *rand.Rand
	nextCall int
	keys     []string // keys with an executed result, for replays
}

func (g *historyGen) options(s State) []Event {
	switch s.Status {
	case StatusNew:
		return []Event{{Type: EvRunStarted, Task: "t", MaxSteps: 1 + g.r.Intn(4), By: "scheduler"}}
	case StatusAwaitingModel:
		return g.modelOptions(s)
	case StatusExecuting:
		var options []Event
		for _, c := range s.Pending {
			options = append(options, g.callOptions(s, c)...)
		}
		return options
	case StatusAwaitingApproval:
		w := s.Waiting
		return []Event{
			{Type: EvApprovalDecided, CallID: w.CallID, Key: w.Key, Decision: "approved", By: "ops"},
			{Type: EvApprovalDecided, CallID: w.CallID, Key: w.Key, Decision: "rejected", By: "ops"},
			{Type: EvApprovalDenied, CallID: w.CallID, Key: w.Key, Decision: "approved", By: "intern", Reason: "not allowed"}}
	case StatusAnswered:
		return []Event{{Type: EvRunFinished, Stop: "stop"}}
	case StatusPaused:
		return []Event{{Type: EvBudgetExtended, ExtraSteps: 1 + g.r.Intn(2), By: "ops"}}
	case StatusFinished: // genHistory stops before asking
	}
	return nil
}

// modelOptions: the model decides (0–3 calls, or an answer) within budget, the run pauses beyond it, and an
// operator can always finish it.
func (g *historyGen) modelOptions(s State) []Event {
	finish := Event{Type: EvRunFinished, Stop: "operator"}
	if s.Step >= s.Budget {
		return []Event{{Type: EvRunPaused, Reason: "budget_exhausted"}, finish}
	}
	var calls []ToolCall
	for range g.r.Intn(4) {
		g.nextCall++
		calls = append(calls, ToolCall{ID: fmt.Sprintf("c%d", g.nextCall), Type: "function",
			Function: FunctionCall{Name: "pay", Arguments: "{}"}})
	}
	msg := &Message{Role: RoleAssistant, ToolCalls: calls}
	if len(calls) == 0 {
		msg.Content = Str("done")
	}
	return []Event{{Type: EvModelDecided, Step: s.Step + 1, Message: msg}, finish}
}

// callOptions: what can happen next to one pending call.
func (g *historyGen) callOptions(s State, c ToolCall) []Event {
	var options []Event
	d := s.Approvals[c.ID]
	if d == "" || d == "approved" {
		options = append(options, Event{Type: EvToolStarted, CallID: c.ID})
	}
	if s.Started[c.ID] {
		// Reuse a key now and then: the second time it must be a replay.
		key := fmt.Sprintf("k%d", len(g.keys))
		if len(g.keys) > 0 && g.r.Intn(3) == 0 {
			key = g.keys[g.r.Intn(len(g.keys))]
		}
		_, seen := s.Effects[key]
		return append(options, Event{Type: EvToolResult, CallID: c.ID, Key: key, Result: `"ok"`, Replayed: seen})
	}
	options = append(options, Event{Type: EvToolRefused, CallID: c.ID, Result: `{"error":"refused"}`})
	if d == "" {
		options = append(options, Event{Type: EvApprovalRequested, CallID: c.ID, Key: "op-" + c.ID, Tool: "pay"})
	}
	return options
}

// invariants are what every reachable state must satisfy.
func invariants(s State, events []Event) error {
	if err := stateInvariants(s); err != nil {
		return err
	}
	return executedOnce(s, events)
}

func stateInvariants(s State) error {
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
	return nil
}

// executedOnce: every key has exactly one executed (not replayed) result, and the state indexes it.
func executedOnce(s State, events []Event) error {
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

// A corruption breaks a rule the runner relies on. apply returns false when it doesn't fit the history.
type corruption struct {
	name  string
	apply func(r *rand.Rand, ev []Event) ([]Event, bool)
}

var corruptions = []corruption{
	{"a second result for one call", duplicateResult},
	{"a result whose call never started", resultWithoutStart},
	{"an approval skipped", skipApproval},
	{"anything after run_finished", eventAfterFinish},
	{"a model step out of order", stepOutOfOrder},
	{"run_finished with calls pending", finishWithPending},
	{"asking again about a rejected operation", reAskRejected},
	{"a second run_started", secondStart},
}

// Each corruption must be refused by Rebuild, whatever history it's in.
func TestPropertyCorruptionsAreRefused(t *testing.T) {
	applied := map[string]int{}
	for i := range propN(400) {
		seed := int64(i)
		r := rand.New(rand.NewSource(seed))
		events, err := genHistory(r)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		for _, c := range corruptions {
			bad, ok := c.apply(r, events)
			if !ok {
				continue
			}
			applied[c.name]++
			if _, err := Rebuild(bad); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("seed %d: %s must be refused, got %v", seed, c.name, err)
			}
		}
	}
	for _, c := range corruptions { // a corruption that never applied would pass vacuously
		if applied[c.name] < 20 {
			t.Errorf("%q applied to only %d histories: the generator doesn't reach it", c.name, applied[c.name])
		}
	}
}

// pick returns the index of a random event matching ok, or -1.
func pick(r *rand.Rand, ev []Event, ok func(Event) bool) int {
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

func insert(ev []Event, at int, e Event) []Event {
	return append(append(append([]Event{}, ev[:at]...), e), ev[at:]...)
}

func isType(t EventType) func(Event) bool { return func(e Event) bool { return e.Type == t } }

func duplicateResult(r *rand.Rand, ev []Event) ([]Event, bool) {
	i := pick(r, ev, isType(EvToolResult))
	if i < 0 {
		return nil, false
	}
	return insert(ev, i+1, ev[i]), true
}

func resultWithoutStart(r *rand.Rand, ev []Event) ([]Event, bool) {
	i := pick(r, ev, isType(EvToolResult))
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
}

func skipApproval(r *rand.Rand, ev []Event) ([]Event, bool) {
	i := pick(r, ev, func(e Event) bool { return e.Type == EvApprovalDecided && e.Decision == "approved" })
	if i < 0 || i == len(ev)-1 {
		return nil, false
	}
	return append(append([]Event{}, ev[:i]...), ev[i+1:]...), true
}

func eventAfterFinish(r *rand.Rand, ev []Event) ([]Event, bool) {
	if len(ev) == 0 || ev[len(ev)-1].Type != EvRunFinished {
		return nil, false
	}
	return append(append([]Event{}, ev...), ev[r.Intn(len(ev))]), true
}

func stepOutOfOrder(r *rand.Rand, ev []Event) ([]Event, bool) {
	i := pick(r, ev, isType(EvModelDecided))
	if i < 0 {
		return nil, false
	}
	out := append([]Event{}, ev...)
	out[i].Step += 1 + r.Intn(3)
	return out, true
}

func finishWithPending(r *rand.Rand, ev []Event) ([]Event, bool) {
	i := pick(r, ev, func(e Event) bool { return e.Type == EvModelDecided && len(e.Message.ToolCalls) > 0 })
	if i < 0 {
		return nil, false
	}
	return insert(ev[:i+1], i+1, Event{Type: EvRunFinished, Stop: "stop"}), true
}

func secondStart(r *rand.Rand, ev []Event) ([]Event, bool) {
	if len(ev) < 2 {
		return nil, false
	}
	return insert(ev, 1+r.Intn(len(ev)-1), ev[0]), true
}

// reAskRejected: after a rejection is resolved (refused), the same operation is asked about again under
// another call id.
func reAskRejected(r *rand.Rand, ev []Event) ([]Event, bool) {
	i := pick(r, ev, func(e Event) bool { return e.Type == EvApprovalDecided && e.Decision == "rejected" })
	if i < 0 {
		return nil, false
	}
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
	out, again := callToAskAgain(st, out, ev[i].CallID)
	if again == "" {
		return nil, false
	}
	return append(out, Event{Type: EvApprovalRequested, CallID: again, Key: ev[i].Key, Tool: "pay"}), true
}

// callToAskAgain finds a call to attach the rejected operation to: another unstarted call of the same turn,
// or a new one the model proposes. "" if the run can't reach either.
func callToAskAgain(st State, out []Event, rejectedCall string) ([]Event, string) {
	switch {
	case st.Status == StatusExecuting:
		again := ""
		for _, c := range st.Pending {
			if !st.Started[c.ID] && st.Approvals[c.ID] == "" {
				again = c.ID
			}
		}
		return out, again
	case st.Status == StatusAwaitingModel && st.Step < st.Budget:
		again := "again-" + rejectedCall
		return append(out, Event{Type: EvModelDecided, Step: st.Step + 1, Message: &Message{Role: RoleAssistant,
			ToolCalls: []ToolCall{{ID: again, Type: "function", Function: FunctionCall{Name: "pay", Arguments: "{}"}}}}}), again
	}
	return out, ""
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

var crashPoints = []string{"after_model_call", "after_model_logged", "before_tool_executed", "after_tool_executed",
	"after_result_logged", "approval_requested"}

// crashRun is one randomized run, driven to the end through processes that may crash.
type crashRun struct {
	t        *testing.T
	r        *rand.Rand
	gw       *propGateway
	tool     Tool
	plan     []FunctionCall
	path     string
	rejected map[string]bool
}

func runOnce(t *testing.T, seed int64) error {
	r := rand.New(rand.NewSource(seed))
	c := &crashRun{t: t, r: r, gw: &propGateway{paid: map[string]int{}}, rejected: map[string]bool{},
		path: filepath.Join(t.TempDir(), "run.jsonl")}
	opts := []FuncOption{Idempotent("ref")}
	if r.Intn(2) == 0 {
		opts = append(opts, NeedsApproval(func(p struct {
			Ref string `json:"ref"`
		}) any {
			return p
		}))
	}
	c.tool = Func("pay", "pay", c.gw.pay, opts...)
	for range 1 + r.Intn(4) {
		c.plan = append(c.plan, FunctionCall{Name: "pay", Arguments: fmt.Sprintf(`{"ref":"R%d"}`, r.Intn(3))}) // repeats on purpose
	}
	if err := c.drive(context.Background()); err != nil {
		return err
	}
	return c.check()
}

// newRunner is a fresh "process"; a crashing one panics at a random point, like kill -9.
func (c *crashRun) newRunner(crash bool) *Runner {
	var hook func(string)
	if crash {
		point, nth, seen := crashPoints[c.r.Intn(len(crashPoints))], 1+c.r.Intn(2), 0
		hook = func(p string) {
			if p == point {
				if seen++; seen == nth {
					panic("kill -9 at " + p)
				}
			}
		}
	}
	run, err := New(&ScriptedModel{Plan: c.plan, Final: "done"}, &FileLog{Path: c.path},
		WithTools(c.tool), WithMaxSteps(20), WithAnyApprover(), WithHook(hook))
	if err != nil {
		c.t.Fatal(err)
	}
	return run
}

// step runs f in a new process; a third of the processes crash somewhere.
func (c *crashRun) step(f func(*Runner) (State, error)) (err error) {
	run := c.newRunner(c.r.Intn(3) == 0)
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("crashed: %v", p)
		}
	}()
	_, err = f(run)
	return err
}

// drive restarts the run until it finishes, deciding approvals at random.
func (c *crashRun) drive(ctx context.Context) error {
	start := func(run *Runner) (State, error) { return run.Start(ctx, "sys", "task") }
	if err := c.step(start); err != nil && !isCrash(err) {
		return err
	}
	for attempt := 0; attempt <= 50; attempt++ {
		events, err := (&FileLog{Path: c.path}).Read(ctx)
		if err != nil {
			return fmt.Errorf("log unreadable: %w", err)
		}
		if len(events) == 0 { // crashed before run_started was written
			if err := c.step(start); err != nil && !isCrash(err) {
				return err
			}
			continue
		}
		st, err := Rebuild(events)
		if err != nil {
			return fmt.Errorf("log doesn't rebuild: %w", err)
		}
		if st.Status == StatusFinished {
			return nil
		}
		if err := c.step(c.next(ctx, st)); err != nil && !isCrash(err) && !errors.Is(err, ErrAlreadyDecided) {
			return err
		}
	}
	return errors.New("the run never finished")
}

// next is what a process does with the run: decide the waiting approval (a third rejected), or continue.
func (c *crashRun) next(ctx context.Context, st State) func(*Runner) (State, error) {
	if st.Status != StatusAwaitingApproval {
		return func(run *Runner) (State, error) { return run.Continue(ctx) }
	}
	key := st.Waiting.Key
	if c.r.Intn(3) == 0 {
		c.rejected[key] = true
		return func(run *Runner) (State, error) { return run.Reject(ctx, key, "ops", "no") }
	}
	return func(run *Runner) (State, error) { return run.Approve(ctx, key, "ops") }
}

// check: no key paid twice, no rejected key paid, distinct refs paid at most once each.
func (c *crashRun) check() error {
	for key, n := range c.gw.paid {
		if n > 1 {
			return fmt.Errorf("key %s paid %d times", key, n)
		}
		if c.rejected[key] && n > 0 {
			return fmt.Errorf("rejected key %s was paid", key)
		}
	}
	refs := map[string]bool{}
	for _, call := range c.plan {
		refs[call.Arguments] = true
	}
	if len(c.gw.paid) > len(refs) {
		return fmt.Errorf("%d keys paid for %d distinct operations", len(c.gw.paid), len(refs))
	}
	return nil
}

func isCrash(err error) bool {
	return err != nil && len(err.Error()) > 7 && err.Error()[:7] == "crashed"
}
