// Package agentsafe provides correctness primitives for LLM agents that act on consequential systems:
// a durable event log, an explicit run state machine, and (from week 3 S2) idempotent tool execution
// and resume. It has no dependencies outside the Go standard library.
package agentsafe

import (
	"context"
	"encoding/json"
)

// Role is who wrote a message in the conversation.
type Role string

// The roles in a conversation.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one entry in the conversation, in the OpenAI-compatible wire format.
// Content is a pointer because an assistant message that only carries tool calls has content null.
type Message struct {
	Role       Role       `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`

	// Native is the provider's own form of an assistant message, kept so the adapter that produced it can send
	// it back byte for byte (e.g. Claude's thinking blocks, which are bound to the exact conversation before
	// them: a resumed run that rebuilt them differently would be rejected). Other adapters ignore it. It is
	// logged, and sealed with the rest of the message.
	Native json.RawMessage `json:"native,omitempty"`
}

// ToolCall is a tool invocation the model PROPOSED. Nothing has run yet: the runner decides.
// ID is minted by the model and changes on every retry (week 1 F9), so it is never used as an
// idempotency key, only to pair a result with its call.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names the tool and carries its arguments as a JSON string, exactly as the model produced
// them. Parse, never trust (week 1 S2).
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Usage is the provider's token count for one model call. Useful, not authoritative (week 1 F1, F5).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Decision is one model response: either tool calls to run, or a final answer.
type Decision struct {
	Message      Message
	FinishReason string // "tool_calls", "stop", "length" (truncated: week 1 F1), ...
	Usage        Usage
}

// ToolSpec describes a tool to the model. Parameters is a JSON Schema.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Model turns a conversation into the next Decision. It must be side-effect free: the runner may call it
// again after a crash that lost a response (week 2 S3, after_model_call), and that has to be harmless.
type Model interface {
	Decide(ctx context.Context, messages []Message, tools []ToolSpec) (Decision, error)
}

// Describer is optionally implemented by a Model so the run's log records which provider and model ran it.
type Describer interface {
	Describe() (provider, model string)
}

// Str returns a pointer to s, for Message.Content.
func Str(s string) *string { return &s }
