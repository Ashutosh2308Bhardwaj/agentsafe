package agentsafe

import (
	"context"
	"fmt"
)

// ScriptedModel is a deterministic stand-in for a real model: it proposes Plan[i] once it has received i
// tool results, then answers Final. It decides from the conversation STATE, so it behaves correctly after
// a resume, like a real model. For tests and free harness runs; it is not a mock of any provider's quirks.
type ScriptedModel struct {
	Plan  []FunctionCall
	Final string
	Calls int // how many times Decide was called (to prove the runner never re-asks for a logged decision)
}

// Describe implements Describer.
func (m *ScriptedModel) Describe() (string, string) { return "scripted", "scripted" }

// Decide implements Model.
func (m *ScriptedModel) Decide(_ context.Context, messages []Message, _ []ToolSpec) (Decision, error) {
	m.Calls++
	results, turns := 0, 0
	for _, msg := range messages {
		switch msg.Role {
		case RoleTool:
			results++
		case RoleAssistant:
			turns++
		default: // system and user messages don't advance the plan
		}
	}
	if results < len(m.Plan) {
		return Decision{
			Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{
				ID: fmt.Sprintf("scripted_%d_%d", turns, results), Type: "function", Function: m.Plan[results]}}},
			FinishReason: "tool_calls",
		}, nil
	}
	return Decision{Message: Message{Role: RoleAssistant, Content: Str(m.Final)}, FinishReason: "stop"}, nil
}
