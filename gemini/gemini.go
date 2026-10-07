// Package gemini is an agentsafe.Model for Google Gemini, on the official Go SDK (google.golang.org/genai).
//
//	client, err := genai.NewClient(ctx, &genai.ClientConfig{Backend: genai.BackendGeminiAPI}) // GEMINI_API_KEY
//	m := gemini.New(client, "gemini-…") // pass a current model id: Gemini's lineup changes fast
//	r, err := agentsafe.New(m, log, agentsafe.WithTools(...))
//
// It uses the stateless generateContent API on purpose: the conversation lives in agentsafe's log, not on
// Google's servers, so a run paused for an approval for weeks, or moved to another machine, still resumes.
// (The stateful Interactions API keeps the history server-side and continues from a previous interaction id.)
//
// Resuming is the hard part, as with Claude. Gemini's thinking models attach thought signatures to parts of
// their responses and require them back "exactly as they were received". So each model turn is stored in the
// log as Gemini returned it (agentsafe.Message.Native) and replayed verbatim, every part in order with its
// signature; every other turn is rebuilt deterministically from the log.
package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"google.golang.org/genai"
)

// DefaultMaxOutputTokens bounds each response.
const DefaultMaxOutputTokens = 16000

// Model calls Gemini through generateContent.
type Model struct {
	Client *genai.Client
	// Model is the model id, e.g. from https://ai.google.dev/gemini-api/docs/models. Required: there is no
	// default, because Gemini's model names change faster than this library.
	Model           string
	MaxOutputTokens int32 // default DefaultMaxOutputTokens
}

// New returns a Model.
func New(client *genai.Client, model string) *Model { return &Model{Client: client, Model: model} }

// Describe implements agentsafe.Describer.
func (m *Model) Describe() (string, string) { return "gemini", m.Model }

// Decide implements agentsafe.Model.
func (m *Model) Decide(ctx context.Context, messages []agentsafe.Message, tools []agentsafe.ToolSpec) (agentsafe.Decision, error) {
	if m.Model == "" {
		return agentsafe.Decision{}, errors.New("gemini: no Model id set")
	}
	contents, config, err := m.Request(messages, tools)
	if err != nil {
		return agentsafe.Decision{}, err
	}
	resp, err := m.Client.Models.GenerateContent(ctx, m.Model, contents, config)
	if err != nil {
		return agentsafe.Decision{}, fmt.Errorf("gemini: %w", err)
	}
	return decision(resp, len(messages))
}

// Request builds the request for a conversation. Exported so the replay can be inspected and tested: for the
// same log it must always produce the same request, and each request must extend the one before.
func (m *Model) Request(messages []agentsafe.Message, tools []agentsafe.ToolSpec) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	maxOut := m.MaxOutputTokens
	if maxOut == 0 {
		maxOut = DefaultMaxOutputTokens
	}
	config := &genai.GenerateContentConfig{MaxOutputTokens: maxOut}
	t, err := toolsConfig(tools)
	if err != nil {
		return nil, nil, err
	}
	config.Tools = t

	r := replay{names: map[string]string{}}
	for i, msg := range messages {
		if msg.Role == agentsafe.RoleSystem {
			if i != 0 {
				return nil, nil, fmt.Errorf("gemini: a system message at position %d; only the first message may be one", i)
			}
			config.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: text(msg)}}}
			continue
		}
		if err := r.add(i, msg); err != nil {
			return nil, nil, err
		}
	}
	r.flush()
	return r.contents, config, nil
}

// toolsConfig declares the tools, their JSON schemas passed through as they are.
func toolsConfig(tools []agentsafe.ToolSpec) ([]*genai.Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	decls := make([]*genai.FunctionDeclaration, len(tools))
	for i, t := range tools {
		var schema any
		if len(t.Parameters) > 0 {
			if err := json.Unmarshal(t.Parameters, &schema); err != nil {
				return nil, fmt.Errorf("gemini: tool %s: parameters aren't JSON: %w", t.Name, err)
			}
		}
		decls[i] = &genai.FunctionDeclaration{Name: t.Name, Description: t.Description, ParametersJsonSchema: schema}
	}
	return []*genai.Tool{{FunctionDeclarations: decls}}, nil
}

// replay turns the conversation into Gemini contents, one message at a time.
type replay struct {
	contents []*genai.Content
	names    map[string]string // tool call id -> function name, for the responses
	results  []*genai.Part     // function responses waiting to go out as one turn
}

func (r *replay) add(i int, msg agentsafe.Message) error {
	switch msg.Role {
	case agentsafe.RoleUser:
		r.flush()
		r.contents = append(r.contents, &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: text(msg)}}})
	case agentsafe.RoleAssistant:
		r.flush()
		c, err := modelTurn(msg)
		if err != nil {
			return fmt.Errorf("gemini: message %d: %w", i, err)
		}
		for _, tc := range msg.ToolCalls {
			r.names[tc.ID] = tc.Function.Name
		}
		r.contents = append(r.contents, c)
	case agentsafe.RoleTool:
		return r.addResult(i, msg)
	default:
		return fmt.Errorf("gemini: message %d has unknown role %q", i, msg.Role)
	}
	return nil
}

func (r *replay) addResult(i int, msg agentsafe.Message) error {
	name, ok := r.names[msg.ToolCallID]
	if !ok {
		return fmt.Errorf("gemini: message %d answers call %q, which no earlier turn made", i, msg.ToolCallID)
	}
	fr := &genai.FunctionResponse{Name: name, Response: responseObject(text(msg))}
	if !strings.HasPrefix(msg.ToolCallID, derivedPrefix) {
		fr.ID = msg.ToolCallID // Gemini gave the call an id: echo it
	}
	r.results = append(r.results, &genai.Part{FunctionResponse: fr})
	return nil
}

// flush sends the waiting function responses as one user turn.
func (r *replay) flush() {
	if len(r.results) > 0 {
		r.contents = append(r.contents, &genai.Content{Role: genai.RoleUser, Parts: r.results})
		r.results = nil
	}
}

// modelTurn replays a turn exactly as Gemini returned it, or (a turn from another provider) rebuilds it.
func modelTurn(msg agentsafe.Message) (*genai.Content, error) {
	if len(msg.Native) > 0 {
		var c genai.Content
		if err := json.Unmarshal(msg.Native, &c); err != nil {
			return nil, fmt.Errorf("stored Gemini turn can't be read: %w", err)
		}
		return &c, nil
	}
	c := &genai.Content{Role: genai.RoleModel}
	if t := text(msg); t != "" {
		c.Parts = append(c.Parts, &genai.Part{Text: t})
	}
	for _, tc := range msg.ToolCalls {
		var args map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return nil, fmt.Errorf("tool call %s: arguments aren't a JSON object: %w", tc.ID, err)
		}
		fc := &genai.FunctionCall{Name: tc.Function.Name, Args: args}
		if !strings.HasPrefix(tc.ID, derivedPrefix) {
			fc.ID = tc.ID
		}
		c.Parts = append(c.Parts, &genai.Part{FunctionCall: fc})
	}
	return c, nil
}

// derivedPrefix marks tool call ids this adapter made up because Gemini sent none. They're never sent back.
const derivedPrefix = "gemini-"

func decision(resp *genai.GenerateContentResponse, historyLen int) (agentsafe.Decision, error) {
	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
			return refused(fmt.Sprintf("the prompt was blocked: %s %s", resp.PromptFeedback.BlockReason, resp.PromptFeedback.BlockReasonMessage)), nil
		}
		return agentsafe.Decision{}, errors.New("gemini: the response has no candidate")
	}
	cand := resp.Candidates[0]
	content := *cand.Content
	content.Role = genai.RoleModel
	native, err := json.Marshal(content)
	if err != nil {
		return agentsafe.Decision{}, fmt.Errorf("gemini: storing the response: %w", err)
	}
	msg, err := parseParts(content.Parts, historyLen)
	if err != nil {
		return agentsafe.Decision{}, err
	}
	msg.Native = native

	finish, err := finishReason(cand, len(msg.ToolCalls) > 0)
	if err != nil {
		return agentsafe.Decision{}, err
	}
	if finish == "refusal" {
		d := refused(fmt.Sprintf("the response was blocked: %s %s", cand.FinishReason, cand.FinishMessage))
		d.Message.Native = native
		return d, nil
	}
	d := agentsafe.Decision{Message: msg, FinishReason: finish}
	if u := resp.UsageMetadata; u != nil {
		d.Usage = agentsafe.Usage{PromptTokens: int(u.PromptTokenCount), CompletionTokens: int(u.CandidatesTokenCount + u.ThoughtsTokenCount)}
	}
	return d, nil
}

// parseParts reads the answer text and the tool calls out of a response, skipping thought summaries.
func parseParts(parts []*genai.Part, historyLen int) (agentsafe.Message, error) {
	msg := agentsafe.Message{Role: agentsafe.RoleAssistant}
	var texts []string
	for i, p := range parts {
		switch {
		case p.Thought: // a thought summary: not part of the answer
		case p.FunctionCall != nil:
			tc, err := toolCall(p.FunctionCall, historyLen, i)
			if err != nil {
				return msg, err
			}
			msg.ToolCalls = append(msg.ToolCalls, tc)
		case p.Text != "":
			texts = append(texts, p.Text)
		}
	}
	if len(texts) > 0 {
		msg.Content = agentsafe.Str(strings.Join(texts, ""))
	}
	return msg, nil
}

func toolCall(fc *genai.FunctionCall, historyLen, part int) (agentsafe.ToolCall, error) {
	args, err := json.Marshal(fc.Args)
	if err != nil {
		return agentsafe.ToolCall{}, fmt.Errorf("gemini: function call arguments: %w", err)
	}
	if fc.Args == nil {
		args = []byte("{}")
	}
	id := fc.ID
	if id == "" {
		// Unique within the run and the same on every replay: the history only grows.
		id = fmt.Sprintf("%s%d-%d", derivedPrefix, historyLen, part)
	}
	return agentsafe.ToolCall{ID: id, Type: "function",
		Function: agentsafe.FunctionCall{Name: fc.Name, Arguments: string(args)}}, nil
}

// finishReason maps Gemini's finish reason onto agentsafe's: stop, tool_calls, length or refusal. Anything it
// doesn't know is an error: stop, don't guess.
func finishReason(cand *genai.Candidate, hasCalls bool) (string, error) {
	switch cand.FinishReason {
	case genai.FinishReasonStop, genai.FinishReasonUnspecified, "":
		if hasCalls {
			return "tool_calls", nil
		}
		return "stop", nil
	case genai.FinishReasonMaxTokens:
		return "length", nil // truncated: the runner must not take a cut-off answer as the result
	case genai.FinishReasonSafety, genai.FinishReasonRecitation, genai.FinishReasonBlocklist,
		genai.FinishReasonProhibitedContent, genai.FinishReasonSPII, genai.FinishReasonImageSafety,
		genai.FinishReasonImageProhibitedContent, genai.FinishReasonImageRecitation:
		return "refusal", nil
	default: // MALFORMED_FUNCTION_CALL, UNEXPECTED_TOOL_CALL, OTHER, and anything newer
		return "", fmt.Errorf("gemini: finish reason %s %s", cand.FinishReason, cand.FinishMessage)
	}
}

func refused(why string) agentsafe.Decision {
	return agentsafe.Decision{FinishReason: "refusal",
		Message: agentsafe.Message{Role: agentsafe.RoleAssistant, Content: agentsafe.Str("[" + why + "]")}}
}

// responseObject turns a tool result into the JSON object Gemini expects: an object stays as it is (agentsafe
// errors are {"error": ...}, Gemini's own convention); anything else is wrapped as {"result": ...}.
func responseObject(result string) map[string]any {
	var obj map[string]any
	if json.Unmarshal([]byte(result), &obj) == nil && obj != nil {
		return obj
	}
	var v any
	if json.Unmarshal([]byte(result), &v) == nil {
		return map[string]any{"result": v}
	}
	return map[string]any{"result": result}
}

func text(m agentsafe.Message) string {
	if m.Content == nil {
		return ""
	}
	return *m.Content
}

var _ agentsafe.Model = (*Model)(nil)
var _ agentsafe.Describer = (*Model)(nil)
