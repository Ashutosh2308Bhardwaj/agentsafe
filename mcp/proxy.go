// Package mcp puts agentsafe between any MCP client and an MCP server: a proxy. The agent, in any language
// and framework, connects to the proxy as if it were the server; the proxy lists the server's tools as its own
// and sends every call through an agentsafe.Gateway: logged before it's forwarded, settled after a crash, and
// recorded with what came back. See docs/MCP_PROXY.md.
//
// Only tools with a Policy are exposed to the agent: the proxy fails closed. A policy protects a tool with an
// idempotency key (a repeated operation is answered from the log, a changed one is a conflict, and the key
// reaches the upstream where it can deduplicate) and, if it says so, a person's approval; or it passes the tool
// through, logged but unprotected ("pass": true). The upstream's tools are read once, at Open: a tool it adds
// later isn't exposed until the proxy restarts, and then only with a policy.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MetaPrefix namespaces what the proxy adds to a result's _meta (a reverse-DNS prefix, as MCP requires).
const MetaPrefix = "io.github.ashutosh2308bhardwaj.agentsafe/"

// Proxy is an MCP server whose tools are an upstream MCP server's, every call going through a Gateway.
type Proxy struct {
	gateway     *agentsafe.Gateway
	upstream    *sdk.ClientSession
	tools       []*sdk.Tool
	hidden      []string // upstream tools with no policy: not exposed
	unprotected []string // "pass" tools the upstream doesn't mark read-only: exposed, forwarded without protection
}

// Open lists the upstream's tools and opens a Gateway on log for them, each protected by its policy (keyed by
// tool name; nil: every tool passed through and logged). A policy for a tool the upstream doesn't have is an
// error: a misspelt name would leave the real tool unprotected. opts are the Gateway's (WithScope,
// WithStartedBy, WithAuthorizer, WithToolTimeout, WithLogf, WithRedactor, WithHook, ...). Each call takes the
// log's lease while it runs, so proxies in several processes can share a log, and approvals can be written to
// it from another process (agentsafe-mcp approve).
func Open(ctx context.Context, upstream *sdk.ClientSession, log agentsafe.Log, policies map[string]Policy, opts ...agentsafe.Option) (*Proxy, error) {
	p := &Proxy{upstream: upstream}
	var tools []agentsafe.Tool
	seen := map[string]bool{}
	for t, err := range upstream.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("listing the upstream's tools: %w", err)
		}
		seen[t.Name] = true
		tool, err := p.adopt(t, policies)
		if err != nil {
			return nil, err
		}
		if tool != nil {
			p.tools, tools = append(p.tools, t), append(tools, tool)
		}
	}
	for name := range policies {
		if !seen[name] {
			return nil, fmt.Errorf("%w: a policy for %q, which the upstream doesn't have", agentsafe.ErrConfig, name)
		}
	}
	g, err := agentsafe.OpenGateway(ctx, log, append([]agentsafe.Option{agentsafe.WithTools(tools...)}, opts...)...)
	if err != nil {
		return nil, err
	}
	p.gateway = g
	return p, nil
}

// Server is the MCP server the agent connects to: the upstream's tools, as the upstream describes them.
func (p *Proxy) Server(impl *sdk.Implementation) *sdk.Server {
	s := sdk.NewServer(impl, nil)
	for _, t := range p.tools {
		copied := *t
		s.AddTool(&copied, p.handle)
	}
	return s
}

// adopt turns one upstream tool into what the Gateway serves, as its policy says: nil (no policy: not exposed),
// passed through, keyed, or keyed and gated.
func (p *Proxy) adopt(t *sdk.Tool, policies map[string]Policy) (agentsafe.Tool, error) {
	policy, ok := policies[t.Name]
	if !ok {
		p.hidden = append(p.hidden, t.Name) // fail closed
		return nil, nil
	}
	if err := policy.check(t); err != nil {
		return nil, fmt.Errorf("%w: policy for %s: %w", agentsafe.ErrConfig, t.Name, err)
	}
	schema, err := json.Marshal(t.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("tool %s: input schema: %w", t.Name, err)
	}
	base := upstreamTool{session: p.upstream, spec: agentsafe.ToolSpec{Name: t.Name, Description: t.Description, Parameters: schema}}
	switch {
	case policy.Pass:
		if t.Annotations == nil || !t.Annotations.ReadOnlyHint {
			p.unprotected = append(p.unprotected, t.Name)
		}
		return &base, nil
	case policy.Approval == "always":
		return &gatedTool{keyedTool{upstreamTool: base, policy: policy}}, nil
	default:
		return &keyedTool{upstreamTool: base, policy: policy}, nil
	}
}

// Hidden are the upstream's tools that have no policy, and so aren't exposed to the agent.
func (p *Proxy) Hidden() []string { return p.hidden }

// Unprotected are the tools passed through ("pass": true) that the upstream doesn't mark read-only: each call
// is logged, but nothing stops a repeat or asks anyone first. Worth a warning at startup.
func (p *Proxy) Unprotected() []string { return p.unprotected }

// Close stops the proxy taking calls. It doesn't close the upstream session: whoever opened it does.
func (p *Proxy) Close() error { return p.gateway.Close() }

// handle is every tools/call: through the Gateway, then back into an MCP result.
func (p *Proxy) handle(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	args := req.Params.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	meta, err := forwardable(req.Params.Meta)
	if err != nil {
		return nil, err
	}
	res, err := p.gateway.Handle(ctx, agentsafe.Request{Client: clientName(req), Tool: req.Params.Name, Args: args, Meta: meta})
	if err != nil {
		// The outcome is unknown, or the log couldn't be written. Say so to the model, as a tool error it can
		// reason about, rather than as a protocol error some clients turn into a crash.
		return errorResult(fmt.Sprintf("agentsafe: %v. The call may or may not have taken effect: don't repeat it before checking", err)), nil
	}
	return toResult(res), nil
}

// forwardable is the part of the client's _meta that travels to the upstream: everything except what belongs to
// the client's own connection to the proxy (keys under a reserved MCP prefix, such as the protocol version and
// client info; progressToken) and agentsafe's own namespace, which only the proxy writes (a client can't set the
// idempotency key). nil if nothing is left.
func forwardable(meta sdk.Meta) (json.RawMessage, error) {
	out := map[string]any{}
	for k, v := range meta {
		if k == "progressToken" || strings.HasPrefix(k, MetaPrefix) || reservedMeta(k) {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, nil
	}
	return json.Marshal(out)
}

// reservedMeta: a _meta key whose prefix's second label is "modelcontextprotocol" or "mcp" is reserved for MCP
// itself (io.modelcontextprotocol/, dev.mcp/), so it describes this hop, not the request.
func reservedMeta(key string) bool {
	prefix, _, ok := strings.Cut(key, "/")
	if !ok {
		return false
	}
	labels := strings.Split(prefix, ".")
	return len(labels) >= 2 && (labels[1] == "modelcontextprotocol" || labels[1] == "mcp")
}

// upstreamMeta is the metadata to send upstream: what the Gateway recorded for this call (the same on a retry).
func upstreamMeta(ctx context.Context) sdk.Meta {
	m := sdk.Meta{}
	if raw := agentsafe.CallMetaFrom(ctx); len(raw) > 0 {
		_ = json.Unmarshal(raw, &m) // recorded by Handle, which only accepts a JSON object
	}
	return m
}

// toResult turns what the Gateway logged back into an MCP result. A forwarded call's result is the upstream's
// own, verbatim: content, structured content, isError. Anything else is agentsafe's own message (refused,
// unknown tool, an outcome that can't be known), which is an error.
func toResult(r agentsafe.GatewayResult) *sdk.CallToolResult {
	if r.Pending { // not an error and not a result: nothing was done yet, and the structured form says so
		var status map[string]any
		_ = json.Unmarshal([]byte(r.Result), &status)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: r.Result}}, StructuredContent: status}
	}
	var res sdk.CallToolResult
	if isToolResult(r.Result) && json.Unmarshal([]byte(r.Result), &res) == nil {
		if r.Replayed {
			if res.Meta == nil {
				res.Meta = sdk.Meta{}
			}
			res.Meta[MetaPrefix+"replayed"] = true
		}
		return &res
	}
	return errorResult(r.Result)
}

// isToolResult reports whether a logged result is an upstream's CallToolResult: an object with a "content"
// field. Checked on the JSON itself: the SDK's decoder fills in an empty Content for any object, so decoding
// can't tell agentsafe's {"error": ...} from a result.
func isToolResult(result string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(result), &fields) != nil {
		return false
	}
	_, ok := fields["content"]
	return ok
}

func errorResult(text string) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: text}}}
}

// clientName is who sent the call, as the client reports itself: recorded for audit, never trusted.
func clientName(req *sdk.CallToolRequest) string {
	if ci := req.ClientInfo(); ci != nil {
		return ci.Name
	}
	if req.Session != nil {
		if ip := req.Session.InitializeParams(); ip != nil && ip.ClientInfo != nil {
			return ip.ClientInfo.Name
		}
	}
	return ""
}

// upstreamTool is one of the upstream's tools as an agentsafe.Tool: calling it forwards the call.
type upstreamTool struct {
	session *sdk.ClientSession
	spec    agentsafe.ToolSpec
}

func (t *upstreamTool) Spec() agentsafe.ToolSpec { return t.spec }

// Call forwards the call. Its result is the upstream's whole CallToolResult, logged as JSON. A protocol or
// transport error is an error: the Gateway records that the call may or may not have taken effect.
func (t *upstreamTool) Call(ctx context.Context, args json.RawMessage) (any, error) {
	return t.session.CallTool(ctx, &sdk.CallToolParams{Name: t.spec.Name, Arguments: args, Meta: upstreamMeta(ctx)})
}
