package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Gateway handles tool calls that arrive from outside an agent loop: an MCP proxy forwarding a client's calls,
// a sidecar in front of an API. Each call gets what a Runner gives a model's call: it's logged before it runs
// (write-ahead), a repeated operation is answered from the log instead of running again, the same operation
// with different values is a conflict, and an IdempotentTool gets its key. The log holds a proxy run (format
// v5) that stays open, call after call.
//
// A Gateway holds its log's lease from OpenGateway to Close: one Gateway per log, in one process. Calls are
// handled one at a time, so after a crash at most one call is in doubt. OpenGateway settles it.
type Gateway struct {
	mu      sync.Mutex
	r       *Runner
	st      State
	release func()
}

// GatewayResult is what became of a call.
type GatewayResult struct {
	Result   string // the tool's result as JSON, or {"error": ...} when it failed, was refused or is in doubt
	Replayed bool   // answered from the log: the operation had already happened, and nothing ran
	Refused  bool   // never attempted (refused before it ran)
}

// ErrGatewayClosed is returned by Call after Close.
var ErrGatewayClosed = errors.New("agentsafe: gateway is closed")

// OpenGateway takes the log's lease, starts a proxy run if the log is empty, and settles any call a crash
// left unfinished. Options are the Runner's: WithTools, WithScope, WithStartedBy, WithToolTimeout, WithHook,
// WithLogf, WithRedactor, WithoutLease. A model and approvals don't apply (approvals: not yet).
func OpenGateway(ctx context.Context, log Log, opts ...Option) (*Gateway, error) {
	r := &Runner{Model: noModel{}, Log: log}
	for _, o := range opts {
		o(r)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	for _, t := range r.Tools {
		if _, gated := t.(Gated); gated {
			return nil, fmt.Errorf("%w: %s needs approval, and approvals through a Gateway aren't supported yet", ErrConfig, t.Spec().Name)
		}
	}
	release, err := r.lock(ctx)
	if err != nil {
		return nil, err
	}
	g := &Gateway{r: r, release: release}
	if err := g.open(ctx); err != nil {
		release()
		return nil, err
	}
	return g, nil
}

func (g *Gateway) open(ctx context.Context) error {
	st, err := g.r.rebuild(ctx)
	switch {
	case errors.Is(err, ErrNoRun):
		g.st = st
		return g.r.emit(ctx, &g.st, Event{Type: EvRunStarted, Kind: KindProxy, KeyBits: KeyBits, By: g.r.StartedBy})
	case err != nil:
		return err
	case st.Kind != KindProxy:
		return fmt.Errorf("%w: the log holds an agent run, not a proxy run", ErrConfig)
	}
	g.st = st
	return g.settle(ctx)
}

// settle resolves a call a crash left unfinished, before any new call is taken:
//   - never forwarded (no tool_started): refused; nothing was done;
//   - forwarded, an IdempotentTool with a key: retried with the same key, which the tool must honour;
//   - forwarded, no key: recorded as an unknown outcome. It may have happened, so it's never run again.
func (g *Gateway) settle(ctx context.Context) error {
	if g.st.Status != StatusExecuting {
		return nil
	}
	c := g.st.Pending[0]
	if !g.st.Started[c.ID] {
		res := errorJSON(errors.New("the gateway stopped before this call was forwarded; nothing was done"))
		return g.r.emit(ctx, &g.st, Event{Type: EvToolRefused, CallID: c.ID, Tool: c.Function.Name, Result: res})
	}
	if it, ok := g.r.find(c.Function.Name).(IdempotentTool); ok && json.Valid([]byte(c.Function.Arguments)) {
		if _, _, err := keyFor(g.r.scope(), it, json.RawMessage(c.Function.Arguments), g.st.KeyBits); err == nil {
			return g.r.step(ctx, &g.st, c) // the same key goes back to the tool
		}
	}
	res := errorJSON(fmt.Errorf("%w: the gateway stopped while %s was running, and it has no idempotency key to "+
		"retry with. Check before doing it again", ErrOutcomeUnknown, c.Function.Name))
	g.r.logf("    ? %s (%s): outcome unknown after a restart; recorded, not retried", c.Function.Name, c.ID)
	return g.r.emit(ctx, &g.st, Event{Type: EvToolResult, CallID: c.ID, Tool: c.Function.Name, Result: res})
}

// Call handles one call. client is who sent it, as the client reports itself: recorded for audit, not trusted.
// An error means the call's outcome is unknown, or the log couldn't be written: nothing more can be said
// about it, and the next Call (or a restart) settles it first.
func (g *Gateway) Call(ctx context.Context, client, tool string, args json.RawMessage) (GatewayResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.release == nil {
		return GatewayResult{}, ErrGatewayClosed
	}
	if err := g.settle(ctx); err != nil {
		return GatewayResult{}, fmt.Errorf("settling the previous call first: %w", err)
	}
	c := ToolCall{ID: fmt.Sprintf("call-%d", g.st.Events+1), Type: "function", Function: FunctionCall{Name: tool, Arguments: string(args)}}
	if err := g.r.emit(ctx, &g.st, Event{Type: EvCallReceived, CallID: c.ID, Tool: tool, Args: string(args), Client: client}); err != nil {
		return GatewayResult{}, err
	}
	g.r.hook("call_received")
	if err := g.r.step(ctx, &g.st, c); err != nil {
		return GatewayResult{}, err
	}
	e := g.r.last
	return GatewayResult{Result: e.Result, Replayed: e.Replayed, Refused: e.Type == EvToolRefused}, nil
}

// Tools are the tools the gateway serves.
func (g *Gateway) Tools() []ToolSpec { return g.r.specs() }

// Close releases the log's lease. Calls in flight finish first.
func (g *Gateway) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.release != nil {
		g.release()
		g.release = nil
	}
	return nil
}

// noModel stands in for a Runner's model in a Gateway: a proxy run never asks one.
type noModel struct{}

func (noModel) Decide(context.Context, []Message, []ToolSpec) (Decision, error) {
	return Decision{}, errors.New("agentsafe: a Gateway has no model")
}
