// Package anthropic is an agentsafe.Model for Claude, on the official Anthropic Go SDK.
//
//	m := anthropic.New(sdk.NewClient()) // ANTHROPIC_API_KEY, or an `ant auth login` profile
//	r, err := agentsafe.New(m, log, agentsafe.WithTools(...))
//
// Resuming a run is the hard part. agentsafe rebuilds the conversation from its log after every crash, approval
// pause or restart, and Claude's thinking blocks are bound to the exact conversation before them: a replay
// that differs in any byte from what was first sent is rejected (400) for accounts created on or after
// 2026-08-31, or silently loses the model's reasoning. So this adapter:
//   - stores each assistant turn in the log as Claude returned it (agentsafe.Message.Native) and sends it
//     back verbatim;
//   - rebuilds every other turn deterministically from the log (all tool results for one assistant turn in
//     one user message, in the order of the calls);
//   - asks the API to reject, not silently drop, a replay that doesn't match (PrefixMismatch "error"), so a
//     replay bug fails loudly.
//
// The system prompt and the tools must not change for the life of a run: they are part of what the thinking
// blocks are bound to. agentsafe logs the system prompt; keep the Runner's tools the same across restarts.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// Defaults. Effort is set explicitly because Claude Opus 5.5 defaults to "medium"; agentic tool use does
// better at "high" or above.
const (
	DefaultModel     = "claude-opus-5-5"
	DefaultMaxTokens = 16000
	DefaultEffort    = "high"
)

// Model calls Claude through the Messages API.
type Model struct {
	Client sdk.Client

	Model     string // default DefaultModel
	MaxTokens int64  // per response; default DefaultMaxTokens
	Effort    string // "low", "medium", "high", "xhigh", "max"; default DefaultEffort

	// PrefixMismatch is what the API does with a replayed thinking block whose conversation changed: "error"
	// (the default: for agentsafe a changed replay is a bug, and it should stop the run) or "drop_block"
	// (continue without that reasoning).
	PrefixMismatch string

	// NoFallbacks turns off server-side refusal fallback. By default a request Claude's safety classifiers
	// decline is re-run on the model Anthropic recommends for that refusal category, inside the same call
	// (the fallback model runs without the declined model's thinking blocks).
	NoFallbacks bool
}

// New returns a Model with the defaults.
func New(client sdk.Client) *Model { return &Model{Client: client} }

// Describe implements agentsafe.Describer.
func (m *Model) Describe() (string, string) { return "anthropic", m.model() }

func (m *Model) model() string {
	if m.Model != "" {
		return m.Model
	}
	return DefaultModel
}

// Decide implements agentsafe.Model.
func (m *Model) Decide(ctx context.Context, messages []agentsafe.Message, tools []agentsafe.ToolSpec) (agentsafe.Decision, error) {
	params, err := m.Params(messages, tools)
	if err != nil {
		return agentsafe.Decision{}, err
	}
	resp, err := m.Client.Beta.Messages.New(ctx, params)
	if err != nil {
		return agentsafe.Decision{}, fmt.Errorf("anthropic: %w", err)
	}
	return decision(resp)
}

// Params builds the request for a conversation. Exported so the replay can be inspected and tested: for the
// same log it must always produce the same request, and each request must extend the one before.
func (m *Model) Params(messages []agentsafe.Message, tools []agentsafe.ToolSpec) (sdk.BetaMessageNewParams, error) {
	maxTokens, effort, mismatch := m.MaxTokens, m.Effort, m.PrefixMismatch
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}
	if effort == "" {
		effort = DefaultEffort
	}
	if mismatch == "" {
		mismatch = "error"
	}
	p := sdk.BetaMessageNewParams{
		Model:     m.model(),
		MaxTokens: maxTokens,
		Thinking: sdk.BetaThinkingConfigParamUnion{OfAdaptive: &sdk.BetaThinkingConfigAdaptiveParam{
			BlockBinding: sdk.BetaThinkingBlockBindingParam{PrefixMismatchBehavior: sdk.BetaThinkingPrefixMismatchBehavior(mismatch)},
		}},
		OutputConfig: sdk.BetaOutputConfigParam{Effort: sdk.BetaOutputConfigEffort(effort)},
		Betas:        []sdk.AnthropicBeta{sdk.AnthropicBetaThinkingBindingControls2026_08_01},
	}
	if !m.NoFallbacks {
		p.Fallbacks = sdk.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
		p.Betas = append(p.Betas, sdk.AnthropicBetaServerSideFallback2026_07_01)
	}
	for _, t := range tools {
		tp, err := toolParam(t)
		if err != nil {
			return p, err
		}
		p.Tools = append(p.Tools, sdk.BetaToolUnionParam{OfTool: &tp})
	}

	var results []sdk.BetaContentBlockParamUnion // tool results waiting to go out as one user message
	flush := func() {
		if len(results) > 0 {
			p.Messages = append(p.Messages, sdk.NewBetaUserMessage(results...))
			results = nil
		}
	}
	for i, msg := range messages {
		switch msg.Role {
		case agentsafe.RoleSystem:
			if i != 0 {
				return p, fmt.Errorf("anthropic: a system message at position %d; only the first message may be one", i)
			}
			p.System = []sdk.BetaTextBlockParam{{Text: text(msg)}}
		case agentsafe.RoleUser:
			flush()
			p.Messages = append(p.Messages, sdk.NewBetaUserMessage(sdk.NewBetaTextBlock(text(msg))))
		case agentsafe.RoleTool:
			results = append(results, sdk.NewBetaToolResultBlock(msg.ToolCallID, text(msg), isError(text(msg))))
		case agentsafe.RoleAssistant:
			flush()
			am, err := assistant(msg)
			if err != nil {
				return p, fmt.Errorf("anthropic: message %d: %w", i, err)
			}
			p.Messages = append(p.Messages, am)
		default:
			return p, fmt.Errorf("anthropic: message %d has unknown role %q", i, msg.Role)
		}
	}
	flush()
	return p, nil
}

// assistant replays a turn exactly as Claude returned it, or (a turn from another provider, or written before
// Native existed) rebuilds it from its text and tool calls.
func assistant(msg agentsafe.Message) (sdk.BetaMessageParam, error) {
	if len(msg.Native) > 0 {
		var p sdk.BetaMessageParam
		if err := json.Unmarshal(msg.Native, &p); err != nil {
			return p, fmt.Errorf("stored Claude message can't be read: %w", err)
		}
		return p, nil
	}
	var blocks []sdk.BetaContentBlockParamUnion
	if t := text(msg); t != "" {
		blocks = append(blocks, sdk.NewBetaTextBlock(t))
	}
	for _, c := range msg.ToolCalls {
		blocks = append(blocks, sdk.NewBetaToolUseBlock(c.ID, json.RawMessage(c.Function.Arguments), c.Function.Name))
	}
	return sdk.BetaMessageParam{Role: sdk.BetaMessageParamRoleAssistant, Content: blocks}, nil
}

func decision(resp *sdk.BetaMessage) (agentsafe.Decision, error) {
	native, err := json.Marshal(resp.ToParam())
	if err != nil {
		return agentsafe.Decision{}, fmt.Errorf("anthropic: storing the response: %w", err)
	}
	msg := agentsafe.Message{Role: agentsafe.RoleAssistant, Native: native}
	var texts []string
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case sdk.BetaTextBlock:
			texts = append(texts, b.Text)
		case sdk.BetaToolUseBlock:
			msg.ToolCalls = append(msg.ToolCalls, agentsafe.ToolCall{ID: b.ID, Type: "function",
				Function: agentsafe.FunctionCall{Name: b.Name, Arguments: b.JSON.Input.Raw()}})
		}
	}
	var finish string
	switch resp.StopReason {
	case sdk.BetaStopReasonEndTurn, sdk.BetaStopReasonStopSequence:
		finish = "stop"
	case sdk.BetaStopReasonToolUse:
		finish = "tool_calls"
	case sdk.BetaStopReasonMaxTokens, sdk.BetaStopReasonModelContextWindowExceeded:
		finish = "length" // truncated: the runner must not take a cut-off answer as the result
	case sdk.BetaStopReasonRefusal:
		// The whole chain declined (with fallbacks on, the fallback model too). The run ends with this text,
		// and reconciliation finds whatever work wasn't done.
		finish = "refusal"
		texts = append(texts, fmt.Sprintf("[declined by safety classifiers: %s %s]", resp.StopDetails.Category, resp.StopDetails.Explanation))
	case sdk.BetaStopReasonPauseTurn, sdk.BetaStopReasonCompaction:
		return agentsafe.Decision{}, fmt.Errorf("anthropic: stop_reason %s (server tools / compaction) isn't supported by this adapter", resp.StopReason)
	default: // a stop reason added after this adapter: stop rather than guess the run is finished
		return agentsafe.Decision{}, fmt.Errorf("anthropic: unknown stop_reason %q", resp.StopReason)
	}
	if len(texts) > 0 {
		msg.Content = agentsafe.Str(strings.Join(texts, "\n"))
	}
	return agentsafe.Decision{Message: msg, FinishReason: finish,
		Usage: agentsafe.Usage{PromptTokens: int(resp.Usage.InputTokens), CompletionTokens: int(resp.Usage.OutputTokens)}}, nil
}

func toolParam(t agentsafe.ToolSpec) (sdk.BetaToolParam, error) {
	var schema map[string]any
	if len(t.Parameters) > 0 {
		if err := json.Unmarshal(t.Parameters, &schema); err != nil {
			return sdk.BetaToolParam{}, fmt.Errorf("anthropic: tool %s: parameters aren't a JSON object: %w", t.Name, err)
		}
	}
	in := sdk.BetaToolInputSchemaParam{Properties: schema["properties"], ExtraFields: map[string]any{}}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				in.Required = append(in.Required, s)
			}
		}
	}
	for k, v := range schema {
		if k != "type" && k != "properties" && k != "required" {
			in.ExtraFields[k] = v // additionalProperties, $defs, ...
		}
	}
	tp := sdk.BetaToolParam{Name: t.Name, InputSchema: in}
	if t.Description != "" {
		tp.Description = param.NewOpt(t.Description)
	}
	return tp, nil
}

func text(m agentsafe.Message) string {
	if m.Content == nil {
		return ""
	}
	return *m.Content
}

// isError: agentsafe reports a failed or refused call to the model as {"error": "..."}.
func isError(result string) bool {
	var v map[string]json.RawMessage
	if json.Unmarshal([]byte(result), &v) != nil {
		return false
	}
	_, ok := v["error"]
	return ok && len(v) == 1
}

var _ agentsafe.Model = (*Model)(nil)
var _ agentsafe.Describer = (*Model)(nil)
