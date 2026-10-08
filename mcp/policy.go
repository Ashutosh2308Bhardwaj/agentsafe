package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// KeyMode is how an operation's idempotency key reaches the upstream server. MCP's tools/call has no key
// field, so it's a choice per tool.
type KeyMode string

// The ways a key can reach the upstream.
const (
	// KeyNone: the upstream can't receive a key. A repeated operation is still answered from the log, but an
	// outcome the proxy couldn't see (a crash or timeout mid-call) is never retried: it's recorded as
	// unknown, and asking again gets that answer.
	KeyNone KeyMode = "none"
	// KeyMeta: in the call's _meta, under MetaPrefix+"idempotency-key", for servers that read it.
	KeyMeta KeyMode = "meta"
	// KeyArgument: as a tool argument the upstream already deduplicates on (Policy.KeyArgument).
	KeyArgument KeyMode = "argument"
)

// MetaKeyIdempotency is the _meta key the idempotency key travels under in KeyMeta mode.
const MetaKeyIdempotency = MetaPrefix + "idempotency-key"

// Policy is how the proxy protects one upstream tool. A tool without one isn't exposed to the agent at all: the
// proxy fails closed, so a tool the server adds tomorrow isn't forwarded until someone writes its policy.
type Policy struct {
	// Pass forwards the tool unprotected: logged, but no key and no approval. It's how you say "I know", for
	// reads and for tools whose effects don't matter; it can't be combined with the fields below.
	Pass bool `json:"pass"`
	// Identity names the arguments that make a call one operation: they're hashed into its key. A refund's
	// might be ticket_id and charge_id. The other arguments are its payload: the same operation with a
	// different payload is a conflict, never a second effect.
	Identity []string `json:"identity"`
	// Key is how the key reaches the upstream: none (the default), meta, or argument.
	Key KeyMode `json:"key"`
	// KeyArgument is the argument the upstream deduplicates on, for Key: argument.
	KeyArgument string `json:"key_argument"`
	// Timeout bounds one call; past it the outcome is unknown. 0: the gateway's.
	Timeout Duration `json:"timeout"`
	// Approval is "always" (every call waits for a human; it needs identity: a decision is addressed by the
	// operation's key) or "never" (the default).
	Approval string `json:"approval"`
}

// Duration is a time.Duration written as text in a policy file ("10s", "2m").
type Duration time.Duration

// UnmarshalJSON reads "10s".
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}

// check validates a policy: consistent in itself, and naming only arguments the tool has.
func (p *Policy) check(t *sdk.Tool) error {
	if p.Key == "" {
		p.Key = KeyNone
	}
	if err := p.consistent(); err != nil {
		return err
	}
	props := schemaProperties(t.InputSchema)
	for _, f := range append(slices.Clone(p.Identity), p.KeyArgument) {
		if f != "" && props != nil && !props[f] {
			return fmt.Errorf("the tool has no argument %q", f)
		}
	}
	return nil
}

// consistent checks the policy's fields against each other.
func (p *Policy) consistent() error {
	if p.Pass && (len(p.Identity) > 0 || p.Key != KeyNone || p.KeyArgument != "" || p.Approval == "always") {
		return errors.New("pass forwards the tool unprotected: it can't have identity, a key or an approval")
	}
	if err := p.keyRules(); err != nil {
		return err
	}
	return p.approvalRules()
}

// keyRules: a key needs identity to be made from, and a way to reach the upstream that's fully specified.
func (p *Policy) keyRules() error {
	switch {
	case p.Key != KeyNone && p.Key != KeyMeta && p.Key != KeyArgument:
		return fmt.Errorf("key %q: none, meta or argument", p.Key)
	case len(p.Identity) == 0 && p.Key != KeyNone:
		return errors.New("a key needs identity fields to be made from")
	case p.Key == KeyArgument && p.KeyArgument == "":
		return errors.New("key: argument needs key_argument, the argument the upstream deduplicates on")
	case p.Key != KeyArgument && p.KeyArgument != "":
		return errors.New("key_argument is only used with key: argument")
	case p.KeyArgument != "" && slices.Contains(p.Identity, p.KeyArgument):
		return fmt.Errorf("%s can't be both an identity field and the key argument", p.KeyArgument)
	}
	return nil
}

// approvalRules: always or never, and approval needs identity: a decision is addressed by the operation's key.
func (p *Policy) approvalRules() error {
	switch {
	case p.Approval != "" && p.Approval != "always" && p.Approval != "never":
		return fmt.Errorf("approval %q: always or never", p.Approval)
	case p.Approval == "always" && len(p.Identity) == 0:
		return errors.New("approval needs identity fields: a decision is addressed by the operation's key")
	}
	return nil
}

// schemaProperties is the set of argument names a tool's input schema declares; nil if it declares none.
func schemaProperties(schema any) map[string]bool {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if b, err := json.Marshal(schema); err != nil || json.Unmarshal(b, &s) != nil || s.Properties == nil {
		return nil
	}
	names := map[string]bool{}
	for k := range s.Properties {
		names[k] = true
	}
	return names
}

// keyedTool is an upstream tool with a policy: an agentsafe.IdempotentTool, so the Gateway gives each call a
// key, answers repeats from the log, and refuses a changed payload.
type keyedTool struct {
	upstreamTool
	policy Policy
}

// Identity splits the arguments into the operation's identity and its payload. The key argument, if the
// model sent one, is part of neither: a model inventing a new key per try must not turn a retry into a
// conflict.
func (t *keyedTool) Identity(args json.RawMessage) (any, any, error) {
	var obj map[string]any
	if err := json.Unmarshal(args, &obj); err != nil {
		return nil, nil, err
	}
	identity := map[string]any{}
	for _, f := range t.policy.Identity {
		v, ok := obj[f]
		if !ok || v == nil {
			return nil, nil, fmt.Errorf("missing %s: it identifies the operation", f)
		}
		identity[f] = v
		delete(obj, f)
	}
	delete(obj, t.policy.KeyArgument)
	return identity, obj, nil
}

// Validate refuses a call that can't be identified before anything runs: unidentified, it would have no key.
func (t *keyedTool) Validate(_ context.Context, args json.RawMessage) error {
	_, _, err := t.Identity(args)
	return err
}

// CallWithKey forwards the call with its key, where the policy says the upstream reads it.
func (t *keyedTool) CallWithKey(ctx context.Context, key string, args json.RawMessage) (any, error) {
	params := &sdk.CallToolParams{Name: t.spec.Name, Arguments: args, Meta: upstreamMeta(ctx)}
	switch t.policy.Key {
	case KeyMeta:
		params.Meta[MetaKeyIdempotency] = key // merged into the client's metadata; the proxy's key wins
	case KeyArgument:
		var obj map[string]any
		if err := json.Unmarshal(args, &obj); err != nil {
			return nil, err
		}
		obj[t.policy.KeyArgument] = key // the proxy's key, never the model's
		params.Arguments = obj
	case KeyNone:
	}
	return t.session.CallTool(ctx, params)
}

// HonoursKey is false for KeyNone: the upstream can't deduplicate, so an unknown outcome is never retried.
func (t *keyedTool) HonoursKey() bool { return t.policy.Key != KeyNone }

// gatedTool is a keyed tool whose every call waits for a human (Approval: always). It's a separate type so that
// only a tool whose policy asks for approval is an agentsafe.Gated: the core treats any Gated as needing one.
type gatedTool struct{ keyedTool }

// NeedsApproval: every call (agentsafe.Gated).
func (t *gatedTool) NeedsApproval(json.RawMessage) bool { return true }

// Summary is what the approver is shown: the call's arguments, without the key argument (agentsafe.Gated).
func (t *gatedTool) Summary(args json.RawMessage) (any, error) {
	var obj map[string]any
	if err := json.Unmarshal(args, &obj); err != nil {
		return nil, err
	}
	delete(obj, t.policy.KeyArgument)
	return obj, nil
}

// Timeout is the policy's per-call timeout (agentsafe.TimeoutTool).
func (t *keyedTool) Timeout() time.Duration { return time.Duration(t.policy.Timeout) }

// KeyedTool is one of the upstream's tools, protected by policy, as the proxy calls it: an agentsafe.IdempotentTool
// whose CallWithKey sends the key where the policy says. It's for checking that the server really deduplicates on
// that key (mcptest.SameKey), not for serving: Open builds the proxy's own.
func KeyedTool(ctx context.Context, upstream *sdk.ClientSession, name string, policy Policy) (agentsafe.IdempotentTool, error) {
	for t, err := range upstream.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("listing the upstream's tools: %w", err)
		}
		if t.Name != name {
			continue
		}
		if err := policy.check(t); err != nil {
			return nil, fmt.Errorf("%w: policy for %s: %w", agentsafe.ErrConfig, name, err)
		}
		if policy.Pass || len(policy.Identity) == 0 || policy.Key == KeyNone {
			return nil, fmt.Errorf("%w: %s: only a policy that sends a key (meta or argument) can be checked", agentsafe.ErrConfig, name)
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, err
		}
		return &keyedTool{upstreamTool: upstreamTool{session: upstream, spec: agentsafe.ToolSpec{Name: t.Name,
			Description: t.Description, Parameters: schema}}, policy: policy}, nil
	}
	return nil, fmt.Errorf("%w: the upstream has no tool %q", agentsafe.ErrConfig, name)
}

var (
	_ agentsafe.IdempotentTool = (*keyedTool)(nil)
	_ agentsafe.Gated          = (*gatedTool)(nil)
)
