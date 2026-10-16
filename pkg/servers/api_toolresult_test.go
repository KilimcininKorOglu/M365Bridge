package servers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/client"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/payload"
)

// decodeMessages parses messages the way a request body does, so the test
// exercises the same id extraction the handlers rely on.
func decodeMessages(t *testing.T, raw string) []payload.Message {
	t.Helper()
	var messages []payload.Message
	if err := json.Unmarshal([]byte(raw), &messages); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	return messages
}

func TestValidateToolResultMessagesAcceptsAnsweredCall(t *testing.T) {
	messages := decodeMessages(t, `[
		{"role":"user","content":"weather?"},
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"}
	]`)
	if err := validateToolResultMessages(messages); err != nil {
		t.Fatalf("a matching tool result was rejected: %v", err)
	}
}

func TestValidateToolResultMessagesRejectsMissingID(t *testing.T) {
	messages := decodeMessages(t, `[
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","content":"sunny"}
	]`)
	err := validateToolResultMessages(messages)
	if err == nil {
		t.Fatal("a tool message without tool_call_id was accepted")
	}
	if !strings.Contains(err.Error(), "tool_call_id") {
		t.Fatalf("error %q does not name the missing field", err)
	}
}

func TestValidateToolResultMessagesRejectsUnknownID(t *testing.T) {
	messages := decodeMessages(t, `[
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_ghost","content":"sunny"}
	]`)
	err := validateToolResultMessages(messages)
	if err == nil {
		t.Fatal("a tool result answering no declared call was accepted")
	}
	if !strings.Contains(err.Error(), "call_ghost") {
		t.Fatalf("error %q does not name the offending id", err)
	}
}

func TestValidateToolResultMessagesAllowsTrimmedHistory(t *testing.T) {
	// A client that trimmed its history to stay under the context window sends
	// results whose calls are gone. The id cannot be checked against anything,
	// so only a missing id is an error.
	messages := decodeMessages(t, `[
		{"role":"tool","tool_call_id":"call_from_an_earlier_turn","content":"sunny"}
	]`)
	if err := validateToolResultMessages(messages); err != nil {
		t.Fatalf("trimmed history was rejected: %v", err)
	}
}

func TestValidateToolResultMessagesChecksAnthropicBlocks(t *testing.T) {
	answered := decodeMessages(t, `[
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny"}]}
	]`)
	if err := validateToolResultMessages(answered); err != nil {
		t.Fatalf("a matching tool_result was rejected: %v", err)
	}

	unknown := decodeMessages(t, `[
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_ghost","content":"sunny"}]}
	]`)
	if err := validateToolResultMessages(unknown); err == nil {
		t.Fatal("a tool_result answering no declared tool_use was accepted")
	}

	missing := decodeMessages(t, `[
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","content":"sunny"}]}
	]`)
	if err := validateToolResultMessages(missing); err == nil {
		t.Fatal("a tool_result without tool_use_id was accepted")
	}
}

func TestResponsesInputCarriesToolCallIDs(t *testing.T) {
	messages := responsesInputToMessages([]any{
		map[string]any{"type": "function_call", "call_id": "fc_1", "name": "get_weather", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "fc_1", "output": "sunny"},
	})
	if err := validateToolResultMessages(messages); err != nil {
		t.Fatalf("a matching Responses pair was rejected: %v", err)
	}

	orphan := responsesInputToMessages([]any{
		map[string]any{"type": "function_call", "call_id": "fc_1", "name": "get_weather", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "fc_ghost", "output": "sunny"},
	})
	if err := validateToolResultMessages(orphan); err == nil {
		t.Fatal("a function_call_output answering no function_call was accepted")
	}
}

func TestValidateToolResultMessagesRejectsADuplicateCallID(t *testing.T) {
	messages := decodeMessages(t, `[
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"},
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}
	]`)
	err := validateToolResultMessages(messages)
	if err == nil {
		t.Fatal("a repeated tool call id was accepted")
	}
	if !strings.Contains(err.Error(), "call_1") {
		t.Fatalf("error %q does not name the repeated id", err)
	}
}

func TestValidateToolResultMessagesRejectsATwiceAnsweredCall(t *testing.T) {
	messages := decodeMessages(t, `[
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"},
		{"role":"tool","tool_call_id":"call_1","content":"rainy"}
	]`)
	err := validateToolResultMessages(messages)
	if err == nil {
		t.Fatal("two results for one call were accepted")
	}
	if !strings.Contains(err.Error(), "call_1") {
		t.Fatalf("error %q does not name the call", err)
	}
}

func TestOpenAIAssistantMessagePreservesCommentaryWithToolCalls(t *testing.T) {
	calls := []client.ToolCall{
		{
			ID:   "call_123",
			Type: "function",
			Function: client.ToolCallFunction{
				Name:      "read_file",
				Arguments: `{"path":"main.go"}`,
			},
		},
	}
	msg := openAIAssistantMessage("Checking main.go now.", "thinking...", calls)
	if msg["content"] != "Checking main.go now." {
		t.Fatalf("content = %v, want commentary preserved", msg["content"])
	}
	if msg["reasoning_content"] != "thinking..." {
		t.Fatalf("reasoning_content = %v, want thinking preserved", msg["reasoning_content"])
	}
	if tcs, ok := msg["tool_calls"].([]map[string]any); !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls = %v, want 1 call", msg["tool_calls"])
	}
}

func TestAnthropicContentBlocksPreservesTextWithToolCalls(t *testing.T) {
	calls := []client.ToolCall{
		{
			ID:   "toolu_123",
			Type: "function",
			Function: client.ToolCallFunction{
				Name:      "read_file",
				Arguments: `{"path":"main.go"}`,
			},
		},
	}
	blocks := anthropicContentBlocks("thinking...", "Checking main.go now.", calls)
	if len(blocks) != 3 {
		t.Fatalf("blocks count = %d, want 3 (thinking, text, tool_use)", len(blocks))
	}
	if blocks[0]["type"] != "thinking" || blocks[0]["thinking"] != "thinking..." {
		t.Fatalf("unexpected block 0: %v", blocks[0])
	}
	if blocks[1]["type"] != "text" || blocks[1]["text"] != "Checking main.go now." {
		t.Fatalf("unexpected block 1: %v", blocks[1])
	}
	if blocks[2]["type"] != "tool_use" || blocks[2]["name"] != "read_file" {
		t.Fatalf("unexpected block 2: %v", blocks[2])
	}
}
