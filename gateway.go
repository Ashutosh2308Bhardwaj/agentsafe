package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Gateway handles tool calls that arrive from outside an agent loop: an MCP proxy forwarding a client's calls,
// a sidecar in front of an API. Each call gets what a Runner gives a model's call: it's logged before it runs
// (write-ahead), a repeated operation is answered from the log instead of running again, the same operation
// with different values is a conflict, an IdempotentTool gets its key, and a Gated tool waits for a human.
// The log holds a proxy run (format v5) that stays open, call after call.
//
// Every Call, Approve, Reject and Pending takes the log's lease, reads the run, settles a call a crash left
// unfinished, acts, and releases: so a decision can come from another process (a person approving from the
// command line), and gateways in several processes can share one log, their calls taking turns.
type Gateway struct {
	mu     sync.Mutex
	r      *Runner
	closed bool
}

// GatewayResult is what became of a call.
type GatewayResult struct {
	Result   string // the tool's result as JSON, or {"error": ...} when it failed, was refused or is in doubt
	Replayed bool   // answered from the log: the operation had already happened, and nothing ran
	Refused  bool   // never attempted (refused before it ran, or waiting for approval)
	Pending  bool   // waiting for a human decision: call again with the same arguments for the outcome
}

// ErrGatewayClosed is returned after Close.
var ErrGatewayClosed = errors.New("agentsafe: gateway is closed")

// OpenGateway checks the configuration, starts a proxy run if the log is empty, and settles any call a crash
// left unfinished. Options are the Runner's: WithTools, WithScope, WithStartedBy, WithAuthorizer (or
// WithAnyApprover), WithToolTimeout, WithHook, WithLogf, WithRedactor, WithoutLease. A model doesn't apply.
func OpenGateway(ctx context.Context, log Log, opts ...Option) (*Gateway, error) {
	r := &Runner{Model: noModel{}, Log: log}
	for _, o := range opts {
		o(r)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	g := &Gateway{r: r}
	if err := g.locked(ctx, func(*State) error { return nil }); err != nil {
		return nil, err
	}
	return g, nil
}

// locked takes the lease, reads the run (starting a proxy run on an empty log), settles a call a crash left
// unfinished, and runs f on the state. Calls in this process take turns; other processes, through the lease.
func (g *Gateway) locked(ctx context.Context, f func(*State) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return ErrGatewayClosed
	}
	release, err := g.r.lock(ctx)
	if err != nil {
		return err
	}
	defer release()
	st, err := g.r.rebuild(ctx)
	switch {
	case errors.Is(err, ErrNoRun):
		err = g.r.emit(ctx, &st, Event{Type: EvRunStarted, Kind: KindProxy, KeyBits: KeyBits, By: g.r.StartedBy})
	case err == nil && st.Kind != KindProxy:
		err = fmt.Errorf("%w: the log holds an agent run, not a proxy run", ErrConfig)
	}
	if err != nil {
		return err
	}
	if err := g.settle(ctx, &st); err != nil {
		return fmt.Errorf("settling the call a crash left unfinished: %w", err)
	}
	return f(&st)
}

// settle resolves a call a crash left unfinished, before anything else happens, if this gateway serves its tool
// (one that doesn't, such as an approver's, leaves it to one that does):
//   - never forwarded (no tool_started): refused; nothing was done;
//   - forwarded, an IdempotentTool with a key: retried with the same key, unless its downstream can't
//     deduplicate (KeyHonouring), in which case it's recorded as unknown under the key;
//   - forwarded, no key: recorded as an unknown outcome. It may have happened, so it's never run again.
func (g *Gateway) settle(ctx context.Context, st *State) error {
	if st.Status != StatusExecuting {
		return nil
	}
	c := st.Pending[0]
	if g.r.find(c.Function.Name) == nil {
		return nil // only a gateway serving the tool can settle its call: an approver's gateway has no tools
	}
	if !st.Started[c.ID] {
		res := errorJSON(errors.New("the gateway stopped before this call was forwarded; nothing was done"))
		return g.r.emit(ctx, st, Event{Type: EvToolRefused, CallID: c.ID, Tool: c.Function.Name, Result: res})
	}
	if key, _ := g.r.operation(g.r.find(c.Function.Name), c, st.KeyBits); key != "" {
		return g.r.step(ctx, st, c) // the same key goes back to the tool (or the unknown is recorded under it)
	}
	res := errorJSON(fmt.Errorf("%w: the gateway stopped while %s was running, and it has no idempotency key to "+
		"retry with. Check before doing it again", ErrOutcomeUnknown, c.Function.Name))
	g.r.logf("    ? %s (%s): outcome unknown after a restart; recorded, not retried", c.Function.Name, c.ID)
	return g.r.emit(ctx, st, Event{Type: EvToolResult, CallID: c.ID, Tool: c.Function.Name, Result: res})
}

// Request is one call from outside.
type Request struct {
	Client string          // who sent it, as the client reports itself: recorded for audit, not trusted
	Tool   string          // the tool to call
	Args   json.RawMessage // its arguments
	// Meta is metadata the tool receives with the call (CallMetaFrom): a JSON object, or empty. It's recorded
	// (sealed, like the arguments) so a retry after a crash sends the same request, and it's not part of the
	// operation: two calls that differ only in Meta are the same operation.
	Meta json.RawMessage
}

// Call handles one call without metadata: Handle(ctx, Request{Client: client, Tool: tool, Args: args}).
func (g *Gateway) Call(ctx context.Context, client, tool string, args json.RawMessage) (GatewayResult, error) {
	return g.Handle(ctx, Request{Client: client, Tool: tool, Args: args})
}

// Handle handles one call. An error means the call's outcome is unknown, or the log couldn't be written:
// nothing more can be said about it, and the next call (or a restart) settles it first.
func (g *Gateway) Handle(ctx context.Context, req Request) (GatewayResult, error) {
	if len(req.Meta) > 0 && !isJSONObject(req.Meta) {
		return GatewayResult{}, fmt.Errorf("%w: request metadata must be a JSON object", ErrConfig)
	}
	var out GatewayResult
	err := g.locked(ctx, func(st *State) error {
		tool, args := req.Tool, req.Args
		c := ToolCall{ID: fmt.Sprintf("call-%d", st.Events+1), Type: "function", Function: FunctionCall{Name: tool, Arguments: string(args)}}
		if err := g.r.emit(ctx, st, Event{Type: EvCallReceived, CallID: c.ID, Tool: tool, Args: string(args), Client: req.Client, Meta: string(req.Meta)}); err != nil {
			return err
		}
		g.r.hook("call_received")
		if err := g.r.step(ctx, st, c); err != nil {
			return err
		}
		out = resultOf(g.r.last, st)
		return nil
	})
	return out, err
}

// resultOf is what a call's last event says became of it.
func resultOf(e Event, st *State) GatewayResult {
	switch e.Type {
	case EvApprovalRequested:
		return GatewayResult{Result: pendingJSON(e.Tool, e.Key), Refused: true, Pending: true}
	case EvToolRefused:
		return GatewayResult{Result: e.Result, Refused: true, Pending: e.Key != "" && st.ByKey[e.Key] == "requested"}
	default:
		return GatewayResult{Result: e.Result, Replayed: e.Replayed}
	}
}

// Approve records an approval for the operation key, by an identity your system verified, if the Authorizer
// allows it (a refused attempt is logged too). The operation runs when it's next called. Approving an
// approved operation again changes nothing.
func (g *Gateway) Approve(ctx context.Context, key, by string) error {
	return g.decide(ctx, key, "approved", by, "")
}

// Reject records a rejection: the operation is refused whenever it's called again.
func (g *Gateway) Reject(ctx context.Context, key, by, reason string) error {
	return g.decide(ctx, key, "rejected", by, reason)
}

func (g *Gateway) decide(ctx context.Context, key, decision, by, reason string) error {
	return g.locked(ctx, func(st *State) error {
		switch prev := st.ByKey[key]; {
		case prev == decision:
			return nil // the same decision twice changes nothing
		case prev == "":
			return fmt.Errorf("%w: no approval was requested for %s", ErrNotWaiting, key)
		case prev != "requested":
			return fmt.Errorf("%w: %s was already %s", ErrAlreadyDecided, key, prev)
		}
		w := st.Requests[key]
		if err := g.r.authorize(ctx, st, w, decision, by, reason); err != nil {
			return err
		}
		if err := g.r.emit(ctx, st, Event{Type: EvApprovalDecided, CallID: w.CallID, Key: key, Decision: decision, By: by, Reason: reason}); err != nil {
			return err
		}
		g.r.logf("[%s %s by %s]", key, decision, by)
		return nil
	})
}

// Pending lists the operations waiting for a decision, oldest first: what an approver is shown.
func (g *Gateway) Pending(ctx context.Context) ([]Waiting, error) {
	var out []Waiting
	err := g.locked(ctx, func(st *State) error {
		for _, w := range st.Requests {
			out = append(out, w)
		}
		sort.Slice(out, func(i, j int) bool { return callNumber(out[i].CallID) < callNumber(out[j].CallID) })
		return nil
	})
	return out, err
}

func isJSONObject(b json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil && m != nil
}

func callNumber(id string) int {
	var n int
	_, _ = fmt.Sscanf(id, "call-%d", &n)
	return n
}

// Tools are the tools the gateway serves.
func (g *Gateway) Tools() []ToolSpec { return g.r.specs() }

// Close stops the gateway: calls after it are refused. It holds no lease between calls, so there's nothing
// else to release.
func (g *Gateway) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	return nil
}

// noModel stands in for a Runner's model in a Gateway: a proxy run never asks one.
type noModel struct{}

func (noModel) Decide(context.Context, []Message, []ToolSpec) (Decision, error) {
	return Decision{}, errors.New("agentsafe: a Gateway has no model")
}
