// Package mcp puts agentsafe between any MCP client and an MCP server: a proxy. The agent, in any language
// and framework, connects to the proxy as if it were the server; the proxy lists the server's tools as its own
// and sends every call through an agentsafe.Gateway: logged before it's forwarded, settled after a crash, and
// recorded with what came back. See docs/MCP_PROXY.md.
//
// This is phase 1 of that design: pass-through and logging. Idempotency keys, replays of completed
// operations and approvals come in later phases.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MetaPrefix namespaces what the proxy adds to a result's _meta (a reverse-DNS prefix, as MCP requires).
const MetaPrefix = "io.github.ashutosh2308bhardwaj.agentsafe/"

// Proxy is an MCP server whose tools are an upstream MCP server's, every call going through a Gateway.
type Proxy struct {
	gateway  *agentsafe.Gateway
	upstream *sdk.ClientSession
	tools    []*sdk.Tool
}

// Open lists the upstream's tools and opens a Gateway on log for them. opts are the Gateway's (WithScope,
// WithStartedBy, WithToolTimeout, WithLogf, WithRedactor, WithHook, ...). The Proxy holds the log's lease
// until Close.
func Open(ctx context.Context, upstream *sdk.ClientSession, log agentsafe.Log, opts ...agentsafe.Option) (*Proxy, error) {
	p := &Proxy{upstream: upstream}
	var tools []agentsafe.Tool
	for t, err := range upstream.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("listing the upstream's tools: %w", err)
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool %s: input schema: %w", t.Name, err)
		}
		p.tools = append(p.tools, t)
		tools = append(tools, &upstreamTool{session: upstream, spec: agentsafe.ToolSpec{Name: t.Name,
			Description: t.Description, Parameters: schema}})
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

// Close releases the log's lease. It doesn't close the upstream session: whoever opened it does.
func (p *Proxy) Close() error { return p.gateway.Close() }

// handle is every tools/call: through the Gateway, then back into an MCP result.
func (p *Proxy) handle(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	args := req.Params.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	res, err := p.gateway.Call(ctx, clientName(req), req.Params.Name, args)
	if err != nil {
		// The outcome is unknown, or the log couldn't be written. Say so to the model, as a tool error it can
		// reason about, rather than as a protocol error some clients turn into a crash.
		return errorResult(fmt.Sprintf("agentsafe: %v. The call may or may not have taken effect: don't repeat it before checking", err)), nil
	}
	return toResult(res), nil
}

// toResult turns what the Gateway logged back into an MCP result. A forwarded call's result is the upstream's
// own, verbatim: content, structured content, isError. Anything else is agentsafe's own message (refused,
// unknown tool, an outcome that can't be known), which is an error.
func toResult(r agentsafe.GatewayResult) *sdk.CallToolResult {
	var res sdk.CallToolResult
	if err := json.Unmarshal([]byte(r.Result), &res); err == nil && res.Content != nil {
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
	return t.session.CallTool(ctx, &sdk.CallToolParams{Name: t.spec.Name, Arguments: args})
}
