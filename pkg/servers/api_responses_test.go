package servers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/client"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/payload"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/toolcalling"
)

func responsesTestTools() []toolcalling.ToolDef {
	return []toolcalling.ToolDef{
		{Type: "function", Name: "read_nonce"},
		{Type: "function", Name: "write_nonce"},
	}
}

func TestInjectSimulatedPromptResponsesUsesOneCanonicalMessage(t *testing.T) {
	messages := []payload.Message{
		{Role: "user", Content: "old user message"},
		{Role: "assistant", Content: "old assistant message"},
		{Role: "user", Content: "latest user message"},
	}
	requestJSON := `{"input":[{"role":"user","content":"old user message"},{"role":"assistant","content":"old assistant message"},{"role":"user","content":"latest user message"}]}`

	injectSimulatedPromptResponses(&messages, requestJSON, "auto")

	if len(messages) != 1 {
		t.Fatalf("expected one canonical simulation message, got %d: %#v", len(messages), messages)
	}
	if messages[0].Role != "user" {
		t.Fatalf("canonical simulation message role = %q, want user", messages[0].Role)
	}
	if !strings.Contains(messages[0].Content, requestJSON) {
		t.Fatalf("canonical simulation message does not preserve the raw Responses request")
	}
	if strings.Count(messages[0].Content, requestJSON) != 1 {
		t.Fatalf("raw Responses request was embedded more than once")
	}
}

func TestResponsesToolPolicy(t *testing.T) {
	tools := responsesTestTools()
	tests := []struct {
		name         string
		choice       interface{}
		wantSimulate bool
		wantRequired bool
		wantName     string
	}{
		{name: "default", choice: nil, wantSimulate: true},
		{name: "auto", choice: "auto", wantSimulate: true},
		{name: "required", choice: "required", wantSimulate: true, wantRequired: true},
		{name: "none", choice: "none", wantSimulate: false},
		{
			name:         "named",
			choice:       map[string]interface{}{"type": "function", "name": "read_nonce"},
			wantSimulate: true,
			wantRequired: true,
			wantName:     "read_nonce",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, err := newResponsesToolPolicy(tools, tt.choice)
			if err != nil {
				t.Fatalf("newResponsesToolPolicy returned error: %v", err)
			}
			if policy.simulate != tt.wantSimulate {
				t.Fatalf("simulate = %v, want %v", policy.simulate, tt.wantSimulate)
			}
			if policy.required != tt.wantRequired {
				t.Fatalf("required = %v, want %v", policy.required, tt.wantRequired)
			}
			if policy.requiredName != tt.wantName {
				t.Fatalf("requiredName = %q, want %q", policy.requiredName, tt.wantName)
			}
		})
	}
}

func TestResponsesToolPolicyRejectsUnknownNamedTool(t *testing.T) {
	_, err := newResponsesToolPolicy(
		responsesTestTools(),
		map[string]interface{}{"type": "function", "name": "invented_tool"},
	)
	if err == nil {
		t.Fatal("expected unknown named tool to be rejected")
	}
}

func TestResponsesToolPolicyAcceptsBuiltInToolType(t *testing.T) {
	tools := []toolcalling.ToolDef{{Type: "tool_search"}}

	policy, err := newResponsesToolPolicy(
		tools,
		map[string]interface{}{"type": "tool_search"},
	)
	if err != nil {
		t.Fatalf("built-in Responses tool rejected: %v", err)
	}
	if len(policy.allowedToolNames) != 1 || policy.allowedToolNames[0] != "tool_search" {
		t.Fatalf("unexpected built-in allowlist: %#v", policy.allowedToolNames)
	}
}

func TestToolNamesFromDefsIncludesFlatAndBuiltInTools(t *testing.T) {
	tools := []toolcalling.ToolDef{
		{Name: "anthropic_tool"},
		{Type: "tool_search"},
	}

	if got := strings.Join(toolNamesFromDefs(tools), ","); got != "anthropic_tool,tool_search" {
		t.Fatalf("flat tool allowlist = %q", got)
	}
}

func TestParseResponsesSimulationRequiredRejectsPlainContent(t *testing.T) {
	policy, err := newResponsesToolPolicy(responsesTestTools(), "required")
	if err != nil {
		t.Fatal(err)
	}
	text := "```json\n" +
		`{"choices":[{"message":{"role":"assistant","content":"I will not call a tool."},"finish_reason":"stop"}]}` +
		"\n```"

	_, err = parseResponsesSimulation(text, policy)
	if err == nil {
		t.Fatal("required tool choice accepted a plain-content response")
	}
}

func TestParseResponsesSimulationWithRetryAcceptsRequiredToolCall(t *testing.T) {
	policy, err := newResponsesToolPolicy(
		responsesTestTools(),
		map[string]interface{}{"type": "function", "name": "read_nonce"},
	)
	if err != nil {
		t.Fatal(err)
	}
	first := "```json\n" +
		`{"choices":[{"message":{"role":"assistant","content":"No tool."},"finish_reason":"stop"}]}` +
		"\n```"
	retries := 0

	result, err := parseResponsesSimulationWithRetry(
		first,
		policy,
		func() (string, error) {
			retries++
			return simulatedToolCallEnvelope("read_nonce"), nil
		},
		nil,
	)
	if err != nil {
		t.Fatalf("required tool retry failed: %v", err)
	}
	if retries != 1 {
		t.Fatalf("retry count = %d, want 1", retries)
	}
	if len(result.toolCalls) != 1 ||
		result.toolCalls[0].Function.Name != "read_nonce" {
		t.Fatalf("unexpected retried tool calls: %#v", result.toolCalls)
	}
}

func TestParseResponsesSimulationWithRetryAllowsSecondRetry(t *testing.T) {
	policy, err := newResponsesToolPolicy(
		responsesTestTools(),
		map[string]interface{}{"type": "function", "name": "read_nonce"},
	)
	if err != nil {
		t.Fatal(err)
	}
	invalid := "```json\n" +
		`{"choices":[{"message":{"role":"assistant","content":"No tool."},"finish_reason":"stop"}]}` +
		"\n```"
	retries := 0

	result, err := parseResponsesSimulationWithRetry(
		invalid,
		policy,
		func() (string, error) {
			retries++
			if retries == 1 {
				return invalid, nil
			}
			return simulatedToolCallEnvelope("read_nonce"), nil
		},
		nil,
	)
	if err != nil {
		t.Fatalf("second required tool retry failed: %v", err)
	}
	if retries != 2 {
		t.Fatalf("retry count = %d, want 2", retries)
	}
	if len(result.toolCalls) != 1 ||
		result.toolCalls[0].Function.Name != "read_nonce" {
		t.Fatalf("unexpected second-retry tool calls: %#v", result.toolCalls)
	}
}

func TestParseResponsesSimulationWithRetryRetriesEmptyAutoResult(t *testing.T) {
	policy, err := newResponsesToolPolicy(responsesTestTools(), "auto")
	if err != nil {
		t.Fatal(err)
	}
	empty := "```json\n" +
		`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[]},"finish_reason":"stop"}]}` +
		"\n```"
	retries := 0

	result, err := parseResponsesSimulationWithRetry(
		empty,
		policy,
		nil,
		func() (string, error) {
			retries++
			return "```json\n" +
					`{"choices":[{"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}]}` +
					"\n```",
				nil
		},
	)
	if err != nil {
		t.Fatalf("empty auto simulation retry failed: %v", err)
	}
	if retries != 1 || result.content != "recovered" {
		t.Fatalf(
			"empty auto retry result = %#v after %d retries",
			result,
			retries,
		)
	}
}

func TestParseResponsesSimulationNamedRejectsWrongDeclaredTool(t *testing.T) {
	policy, err := newResponsesToolPolicy(
		responsesTestTools(),
		map[string]interface{}{"type": "function", "name": "read_nonce"},
	)
	if err != nil {
		t.Fatal(err)
	}
	text := simulatedToolCallEnvelope("write_nonce")

	_, err = parseResponsesSimulation(text, policy)
	if err == nil {
		t.Fatal("named tool choice accepted a different declared tool")
	}
}

func TestParseResponsesSimulationDropsInventedTool(t *testing.T) {
	policy, err := newResponsesToolPolicy(responsesTestTools(), "auto")
	if err != nil {
		t.Fatal(err)
	}

	result, err := parseResponsesSimulation(simulatedToolCallEnvelope("code_interpreter"), policy)
	if err != nil {
		t.Fatalf("auto tool choice returned an unexpected error: %v", err)
	}
	if len(result.toolCalls) != 0 {
		t.Fatalf("invented tool was not rejected: %#v", result.toolCalls)
	}
}

func TestParseResponsesSimulationAcceptsFlatResponsesToolDefinition(t *testing.T) {
	policy, err := newResponsesToolPolicy(responsesTestTools(), "required")
	if err != nil {
		t.Fatal(err)
	}

	result, err := parseResponsesSimulation(simulatedToolCallEnvelope("read_nonce"), policy)
	if err != nil {
		t.Fatalf("valid Responses tool call rejected: %v", err)
	}
	if len(result.toolCalls) != 1 || result.toolCalls[0].Function.Name != "read_nonce" {
		t.Fatalf("unexpected parsed tool calls: %#v", result.toolCalls)
	}
}

func TestParseResponsesSimulationPreservesToolPreamble(t *testing.T) {
	policy, err := newResponsesToolPolicy(responsesTestTools(), "required")
	if err != nil {
		t.Fatal(err)
	}
	const preamble = "Önce nonce dosyasını okuyup sonucu doğrulayacağım."
	raw := "```json\n" +
		`{"choices":[{"message":{"role":"assistant","content":"` +
		preamble +
		`","tool_calls":[{"id":"call_test","type":"function","function":{"name":"read_nonce","arguments":"{\"path\":\"nonce.txt\"}"}}]},"finish_reason":"tool_calls"}]}` +
		"\n```"

	result, err := parseResponsesSimulation(raw, policy)
	if err != nil {
		t.Fatal(err)
	}
	if result.content != preamble {
		t.Fatalf("tool preamble = %q, want %q", result.content, preamble)
	}
	if len(result.toolCalls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.toolCalls))
	}
}

func TestParseResponsesSimulationAddsSafeFallbackPreamble(t *testing.T) {
	policy, err := newResponsesToolPolicy(responsesTestTools(), "required")
	if err != nil {
		t.Fatal(err)
	}

	result, err := parseResponsesSimulation(
		simulatedToolCallEnvelope("read_nonce"),
		policy,
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.content) == "" {
		t.Fatal("tool call without model content did not receive a visible fallback preamble")
	}
	for _, forbidden := range []string{"chatcmpl-", `"choices"`, `"tool_calls"`} {
		if strings.Contains(result.content, forbidden) {
			t.Fatalf("fallback preamble leaked transport marker %q: %q", forbidden, result.content)
		}
	}
}

func TestBuildResponsesObjectPlacesCommentaryBeforeToolCall(t *testing.T) {
	call := client.ToolCall{
		ID:   "call_read",
		Type: "function",
		Function: client.ToolCallFunction{
			Name:      "read_nonce",
			Arguments: `{"path":"nonce.txt"}`,
		},
	}

	response := buildResponsesObject(
		"resp_test",
		"gpt-test",
		"Nonce dosyasını şimdi okuyorum.",
		"",
		[]client.ToolCall{call},
		map[string]string{"read_nonce": "function"},
		"tool_calls",
		1,
		1,
		0,
	)
	output, ok := response["output"].([]map[string]interface{})
	if !ok {
		t.Fatalf("response output has unexpected type: %T", response["output"])
	}
	if len(output) != 2 {
		t.Fatalf("output item count = %d, want 2: %#v", len(output), output)
	}
	if output[0]["type"] != "message" {
		t.Fatalf("first output item type = %#v, want message", output[0]["type"])
	}
	if output[0]["phase"] != "commentary" {
		t.Fatalf("tool preamble phase = %#v, want commentary", output[0]["phase"])
	}
	if output[1]["type"] != "function_call" {
		t.Fatalf("second output item type = %#v, want function_call", output[1]["type"])
	}
}

func TestBuildResponsesToolCallItemUsesDeclaredBuiltInType(t *testing.T) {
	call := client.ToolCall{
		ID:   "call_search",
		Type: "function",
		Function: client.ToolCallFunction{
			Name:      "tool_search",
			Arguments: `{"query":"node repl"}`,
		},
	}

	item := buildResponsesToolCallItem(
		"call_search",
		call,
		map[string]string{"tool_search": "tool_search"},
		"completed",
	)

	if item["type"] != "tool_search_call" {
		t.Fatalf("tool_search item type = %#v", item["type"])
	}
	if item["execution"] != "client" {
		t.Fatalf("tool_search execution = %#v", item["execution"])
	}
	if _, ok := item["arguments"].(map[string]interface{}); !ok {
		t.Fatalf("tool_search arguments are not a JSON object: %#v", item["arguments"])
	}
}

func TestBuildResponsesToolCallItemIncludesNamespace(t *testing.T) {
	call := client.ToolCall{
		ID:   "call_js",
		Type: "function",
		Function: client.ToolCallFunction{
			Name:      "js",
			Namespace: "mcp__node_repl",
			Arguments: `{"code":"1+1"}`,
		},
	}

	item := buildResponsesToolCallItem(
		"call_js",
		call,
		map[string]string{"mcp__node_repl/js": "function"},
		"completed",
	)

	if item["namespace"] != "mcp__node_repl" {
		t.Fatalf("function_call namespace = %#v", item["namespace"])
	}
	if item["name"] != "js" {
		t.Fatalf("function_call name = %#v", item["name"])
	}
}

func TestBuildResponsesToolCallItemIncludesEmptyArgumentsWhileInProgress(t *testing.T) {
	call := client.ToolCall{
		ID:   "call_read",
		Type: "function",
		Function: client.ToolCallFunction{
			Name:      "read_nonce",
			Arguments: `{"path":"nonce.txt"}`,
		},
	}

	item := buildResponsesToolCallItem(
		"call_read",
		call,
		map[string]string{"read_nonce": "function"},
		"in_progress",
	)

	if arguments, ok := item["arguments"]; !ok || arguments != "" {
		t.Fatalf("in-progress arguments = %#v, want empty string", arguments)
	}
}

func TestMergeLoadedResponsesTools(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{
			"type": "tool_search_output",
			"tools": []interface{}{
				map[string]interface{}{
					"type": "namespace",
					"tools": []interface{}{
						map[string]interface{}{
							"type": "function",
							"name": "node_repl",
						},
					},
				},
			},
		},
	}

	tools := mergeLoadedResponsesTools(
		input,
		[]toolcalling.ToolDef{{Type: "tool_search"}},
	)
	names := responsesToolNames(tools)
	if strings.Join(names, ",") != "tool_search,node_repl" {
		t.Fatalf("loaded Responses tools not merged: %#v", names)
	}
}

func TestMergeLoadedResponsesToolsPreservesDuplicateNamespacedTools(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{
			"type": "tool_search_output",
			"tools": []interface{}{
				map[string]interface{}{
					"type": "namespace",
					"name": "mcp__node_repl",
					"tools": []interface{}{
						map[string]interface{}{"type": "function", "name": "js"},
					},
				},
				map[string]interface{}{
					"type": "namespace",
					"name": "mcp__browser",
					"tools": []interface{}{
						map[string]interface{}{"type": "function", "name": "js"},
					},
				},
			},
		},
	}

	tools := mergeLoadedResponsesTools(input, nil)
	if len(tools) != 2 {
		t.Fatalf("namespaced duplicate tools collapsed: %#v", tools)
	}
	if tools[0].Namespace != "mcp__node_repl" || tools[1].Namespace != "mcp__browser" {
		t.Fatalf("loaded namespaces not preserved: %#v", tools)
	}
}

func TestParseResponsesSimulationPreservesNamespace(t *testing.T) {
	tools := []toolcalling.ToolDef{
		{Type: "function", Name: "js", Namespace: "mcp__node_repl"},
	}
	policy, err := newResponsesToolPolicy(tools, "required")
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call_js","name":"js","namespace":"mcp__node_repl","arguments":"{\"code\":\"1+1\"}"}]},"finish_reason":"tool_calls"}]}`

	result, err := parseResponsesSimulation(raw, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.toolCalls) != 1 {
		t.Fatalf("tool call count = %d", len(result.toolCalls))
	}
	if result.toolCalls[0].Function.Namespace != "mcp__node_repl" {
		t.Fatalf("namespace = %q", result.toolCalls[0].Function.Namespace)
	}
}

func TestParseResponsesSimulationRejectsAmbiguousUnqualifiedNamespace(t *testing.T) {
	tools := []toolcalling.ToolDef{
		{Type: "function", Name: "js", Namespace: "mcp__node_repl"},
		{Type: "function", Name: "js", Namespace: "mcp__browser"},
	}
	policy, err := newResponsesToolPolicy(tools, "required")
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call_js","name":"js","arguments":"{}"}]},"finish_reason":"tool_calls"}]}`

	if _, err := parseResponsesSimulation(raw, policy); err == nil {
		t.Fatal("ambiguous unqualified namespaced tool call was accepted")
	}
}

func TestResponsesInputPreservesToolSearchAndCompactionHistory(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{
			"type":      "tool_search_call",
			"arguments": map[string]interface{}{"query": "node repl"},
		},
		map[string]interface{}{
			"type": "tool_search_output",
			"tools": []interface{}{
				map[string]interface{}{
					"type": "function",
					"name": "node_repl",
				},
			},
		},
		map[string]interface{}{
			"type":              "compaction",
			"encrypted_content": "Earlier work summary",
		},
	}

	messages := responsesInputToMessages(input)
	combined := ""
	for _, message := range messages {
		combined += message.Content + "\n"
	}
	for _, expected := range []string{
		"Tool search call",
		"node_repl",
		"Earlier work summary",
	} {
		if !strings.Contains(combined, expected) {
			t.Fatalf("Responses history lost %q: %#v", expected, messages)
		}
	}
}

func TestResponsesFunctionCallOutputBecomesAuthoritativeToolHistory(t *testing.T) {
	messages := responsesInputToMessages([]interface{}{
		map[string]interface{}{
			"type":    "function_call_output",
			"call_id": "call_nonce",
			"output":  "NONCE-EXACT",
		},
	})

	if len(messages) != 1 {
		t.Fatalf("message count = %d", len(messages))
	}
	if messages[0].Role != "tool" {
		t.Fatalf("function result role = %q, want tool", messages[0].Role)
	}
	if messages[0].ToolCallID != "call_nonce" {
		t.Fatalf("function result call id = %q, want call_nonce", messages[0].ToolCallID)
	}
	for _, expected := range []string{
		"authoritative tool result",
		"call_nonce",
		"NONCE-EXACT",
	} {
		if !strings.Contains(strings.ToLower(messages[0].Content), strings.ToLower(expected)) {
			t.Fatalf("function result lost %q: %s", expected, messages[0].Content)
		}
	}
}

func TestResponsesInputPreservesNamespacedToolState(t *testing.T) {
	namespacedTools := []interface{}{
		map[string]interface{}{
			"type": "namespace",
			"name": "mcp__node_repl",
			"tools": []interface{}{
				map[string]interface{}{
					"type": "function",
					"name": "js",
					"parameters": map[string]interface{}{
						"type": "object",
					},
				},
			},
		},
	}
	input := []interface{}{
		map[string]interface{}{
			"type":  "tool_search_output",
			"tools": namespacedTools,
		},
		map[string]interface{}{
			"type":  "additional_tools",
			"tools": namespacedTools,
		},
		map[string]interface{}{
			"type":      "function_call",
			"namespace": "mcp__node_repl",
			"name":      "js",
			"arguments": `{"code":"1+1"}`,
		},
		map[string]interface{}{
			"type":    "function_call_output",
			"call_id": "call_js",
			"output":  "2",
		},
	}

	messages := responsesInputToMessages(input)
	combined := ""
	for _, message := range messages {
		combined += message.Content + "\n"
	}
	for _, expected := range []string{
		"tool_search_output",
		"additional_tools",
		`"name":"mcp__node_repl"`,
		`"name":"js"`,
		`"parameters":{"type":"object"}`,
		"mcp__node_repl/js",
		"2",
	} {
		if !strings.Contains(combined, expected) {
			t.Fatalf("Responses tool state lost %q:\n%s", expected, combined)
		}
	}
}

func TestDefaultCompactionPromptPreservesExactToolState(t *testing.T) {
	for _, expected := range []string{
		"tool state",
		"exact names",
		"namespace",
		"results",
	} {
		if !strings.Contains(strings.ToLower(defaultCompactionPrompt), expected) {
			t.Fatalf("default compaction prompt missing %q: %s", expected, defaultCompactionPrompt)
		}
	}
}

func TestContextCacheDeleteRemovesMemoryAndDisk(t *testing.T) {
	cacheDir := t.TempDir()
	cache := NewContextCache(cacheDir)
	cache.Set("session:test", "conv-poisoned")
	cache.Delete("session:test")

	if got := cache.Get("session:test"); got != "" {
		t.Fatalf("deleted context cache returned %q", got)
	}
	matches, err := filepath.Glob(filepath.Join(cacheDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("deleted context cache left disk entries: %#v", matches)
	}
}

func TestContextCacheDeleteWinsAgainstInFlightSet(t *testing.T) {
	cacheDir := t.TempDir()
	cache := NewContextCache(cacheDir)
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	cache.writeFile = func(path string, data []byte, mode os.FileMode) error {
		close(writeStarted)
		<-releaseWrite
		return os.WriteFile(path, data, mode)
	}

	setDone := make(chan struct{})
	go func() {
		defer close(setDone)
		cache.Set("session:test", "conv-poisoned")
	}()
	<-writeStarted

	deleteDone := make(chan struct{})
	go func() {
		defer close(deleteDone)
		cache.Delete("session:test")
	}()

	select {
	case <-deleteDone:
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseWrite)
	<-setDone
	<-deleteDone

	if got := cache.Get("session:test"); got != "" {
		t.Fatalf("late Set resurrected deleted context %q", got)
	}
	if _, err := os.Stat(cache.path("session:test")); !os.IsNotExist(err) {
		t.Fatalf("late Set recreated deleted cache file: %v", err)
	}
}

func TestShouldResetResponsesSession(t *testing.T) {
	call := client.ToolCall{
		Function: client.ToolCallFunction{Name: "read_nonce"},
	}
	tests := []struct {
		name    string
		content string
		calls   []client.ToolCall
		err     error
		want    bool
	}{
		{name: "error", err: errors.New("failed"), want: true},
		{name: "empty", want: true},
		{name: "content", content: "ok", want: false},
		{name: "tool call", calls: []client.ToolCall{call}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldResetResponsesSession(tt.content, tt.calls, tt.err); got != tt.want {
				t.Fatalf("shouldResetResponsesSession = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResponsesCompactionAlwaysUsesFreshConversation(t *testing.T) {
	if got := responsesCompactionConversationID("conv-poisoned"); got != "" {
		t.Fatalf("compaction reused sticky conversation %q", got)
	}
}

func TestResponsesReasoningIsSuppressedDuringToolSimulation(t *testing.T) {
	const leaked = `{"id":"chatcmpl-1234567890","object":"chat.completion"}`

	if got := responsesReasoningForOutput(leaked, true); got != "" {
		t.Fatalf("tool simulation leaked reasoning payload: %q", got)
	}
	if got := responsesReasoningForOutput("normal reasoning", false); got != "normal reasoning" {
		t.Fatalf("non-simulated reasoning changed: %q", got)
	}
}

func TestWriteResponsesSimulationErrorNonStreaming(t *testing.T) {
	rec := httptest.NewRecorder()

	writeResponsesSimulationError(rec, false, "resp_test", "gpt-test", errSimulatedToolCallRequired)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	errorObject, _ := body["error"].(map[string]interface{})
	if errorObject["code"] != simulatedToolCallRequiredCode {
		t.Fatalf("error code = %#v, want %q", errorObject["code"], simulatedToolCallRequiredCode)
	}
}

func TestWriteResponsesSimulationErrorStreaming(t *testing.T) {
	rec := httptest.NewRecorder()

	writeResponsesSimulationError(rec, true, "resp_test", "gpt-test", errSimulatedToolCallRequired)

	body := rec.Body.String()
	if !strings.Contains(body, `"type":"response.failed"`) {
		t.Fatalf("stream missing response.failed event:\n%s", body)
	}
	if !strings.Contains(body, `"code":"`+simulatedToolCallRequiredCode+`"`) {
		t.Fatalf("stream missing stable error code:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream missing terminal marker:\n%s", body)
	}
}

func TestBuildResponsesFailedEventIncludesSequenceNumber(t *testing.T) {
	event := buildResponsesFailedEvent(
		"resp_test",
		"gpt-test",
		"upstream_timeout",
		"timed out",
		7,
	)
	if got := event["sequence_number"]; got != 7 {
		t.Fatalf("sequence_number = %#v, want 7", got)
	}
	response, ok := event["response"].(map[string]interface{})
	if !ok {
		t.Fatalf("response payload has wrong type: %#v", event["response"])
	}
	errorPayload, ok := response["error"].(map[string]interface{})
	if !ok || errorPayload["code"] != "upstream_timeout" {
		t.Fatalf("error payload mismatch: %#v", response["error"])
	}
}

func TestResponsesResultRequiresVisibleOutput(t *testing.T) {
	call := client.ToolCall{ID: "call_test"}
	tests := []struct {
		name  string
		text  string
		calls []client.ToolCall
		empty bool
	}{
		{name: "empty", empty: true},
		{name: "content", text: "ok"},
		{name: "tool call", calls: []client.ToolCall{call}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := responsesResultEmpty(tt.text, tt.calls); got != tt.empty {
				t.Fatalf("responsesResultEmpty = %v, want %v", got, tt.empty)
			}
		})
	}
}

func TestResponsesEmptyRetryBudgetDependsOnSimulation(t *testing.T) {
	plain := responsesEmptyRetrySchedule(false)
	if got := len(plain); got != 2 {
		t.Fatalf(
			"plain empty retry delays = %d, want two retries",
			got,
		)
	}
	if plain[0] != 10*time.Second || plain[1] != 30*time.Second {
		t.Fatalf("plain retry schedule = %v, want [10s 30s]", plain)
	}
	simulated := responsesEmptyRetrySchedule(true)
	if got := len(simulated); got != 1 {
		t.Fatalf(
			"simulated empty retry delays = %d, want one retry",
			got,
		)
	}
	if simulated[0] != 10*time.Second {
		t.Fatalf("simulated retry schedule = %v, want [10s]", simulated)
	}
}

func TestResponsesConversationRetriesEmptyCompletion(t *testing.T) {
	attempts := 0
	conversationIDs := []string{}
	retryHooks := 0

	result, err := responsesConversationWithEmptyRetry(
		context.Background(),
		"conv-poisoned",
		[]time.Duration{0},
		func() {
			retryHooks++
		},
		func(_ context.Context, conversationID string) (responsesConversationResult, error) {
			attempts++
			conversationIDs = append(conversationIDs, conversationID)
			if attempts == 1 {
				return responsesConversationResult{}, nil
			}
			return responsesConversationResult{
				text:           "recovered",
				finishReason:   "stop",
				conversationID: "conv-recovered",
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("empty completion retry failed: %v", err)
	}
	if result.text != "recovered" {
		t.Fatalf("retried text = %q, want recovered", result.text)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if retryHooks != 1 {
		t.Fatalf("retry hooks = %d, want 1", retryHooks)
	}
	if got := strings.Join(conversationIDs, ","); got != "conv-poisoned," {
		t.Fatalf("conversation IDs = %q, want poisoned then fresh", got)
	}
}

func TestResponsesConversationEmptyRetryIsBounded(t *testing.T) {
	attempts := 0

	result, err := responsesConversationWithEmptyRetry(
		context.Background(),
		"",
		[]time.Duration{0},
		nil,
		func(_ context.Context, _ string) (responsesConversationResult, error) {
			attempts++
			return responsesConversationResult{}, nil
		},
	)
	if err != nil {
		t.Fatalf("bounded empty retry returned error: %v", err)
	}
	if !responsesResultEmpty(result.text, result.toolCalls) {
		t.Fatalf("exhausted retry unexpectedly returned visible output: %#v", result)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestResponsesConversationRecoversOnSecondEmptyRetry(t *testing.T) {
	attempts := 0

	result, err := responsesConversationWithEmptyRetry(
		context.Background(),
		"",
		[]time.Duration{0, 0},
		nil,
		func(_ context.Context, _ string) (responsesConversationResult, error) {
			attempts++
			if attempts < 3 {
				return responsesConversationResult{}, nil
			}
			return responsesConversationResult{text: "recovered"}, nil
		},
	)
	if err != nil {
		t.Fatalf("second empty retry failed: %v", err)
	}
	if result.text != "recovered" || attempts != 3 {
		t.Fatalf(
			"second retry result = %#v after %d attempts",
			result,
			attempts,
		)
	}
}

func TestResponsesStreamRetriesEmptyCompletionBeforeEmitting(t *testing.T) {
	attempts := 0
	conversationIDs := []string{}
	retryHooks := 0

	ch := responsesStreamWithEmptyRetry(
		context.Background(),
		"conv-poisoned",
		[]time.Duration{0},
		false,
		func() {
			retryHooks++
		},
		func(_ context.Context, conversationID string) <-chan client.StreamChunk {
			attempts++
			conversationIDs = append(conversationIDs, conversationID)
			if attempts == 1 {
				return responsesTestChunkStream(client.StreamChunk{
					IsFinal:        true,
					ConversationID: "conv-empty",
					FinishReason:   "stop",
				})
			}
			return responsesTestChunkStream(
				client.StreamChunk{Text: "recovered"},
				client.StreamChunk{
					IsFinal:        true,
					ConversationID: "conv-recovered",
					FinishReason:   "stop",
				},
			)
		},
	)

	chunks := []client.StreamChunk{}
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if retryHooks != 1 {
		t.Fatalf("retry hooks = %d, want 1", retryHooks)
	}
	if got := strings.Join(conversationIDs, ","); got != "conv-poisoned," {
		t.Fatalf("conversation IDs = %q, want poisoned then fresh", got)
	}
	if len(chunks) != 2 || chunks[0].Text != "recovered" ||
		!chunks[1].IsFinal {
		t.Fatalf("unexpected retried stream: %#v", chunks)
	}
}

func TestResponsesStreamEmptyRetryStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0

	ch := responsesStreamWithEmptyRetry(
		ctx,
		"",
		[]time.Duration{time.Hour},
		false,
		nil,
		func(_ context.Context, _ string) <-chan client.StreamChunk {
			attempts++
			cancel()
			return responsesTestChunkStream(client.StreamChunk{
				IsFinal: true,
			})
		},
	)

	for range ch {
	}
	if attempts != 1 {
		t.Fatalf("attempts after cancellation = %d, want 1", attempts)
	}
}

func TestResponsesStreamDoesNotRetryAfterVisibleChunk(t *testing.T) {
	attempts := 0

	ch := responsesStreamWithEmptyRetry(
		context.Background(),
		"",
		[]time.Duration{0, 0},
		false,
		nil,
		func(_ context.Context, _ string) <-chan client.StreamChunk {
			attempts++
			return responsesTestChunkStream(
				client.StreamChunk{Thinking: "working"},
				client.StreamChunk{IsFinal: true},
			)
		},
	)

	chunks := []client.StreamChunk{}
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 after visible output", attempts)
	}
	if len(chunks) != 2 || chunks[0].Thinking != "working" ||
		!chunks[1].IsFinal {
		t.Fatalf("visible stream changed: %#v", chunks)
	}
}

func TestResponsesStreamRetriesHiddenBackendToolCall(t *testing.T) {
	attempts := 0
	toolCall := client.ToolCall{
		ID:   "call_test",
		Type: "function",
		Function: client.ToolCallFunction{
			Name:      "read_nonce",
			Arguments: `{}`,
		},
	}

	ch := responsesStreamWithEmptyRetry(
		context.Background(),
		"",
		[]time.Duration{0, 0},
		false,
		nil,
		func(_ context.Context, _ string) <-chan client.StreamChunk {
			attempts++
			if attempts == 2 {
				return responsesTestChunkStream(
					client.StreamChunk{Text: "recovered"},
					client.StreamChunk{IsFinal: true},
				)
			}
			return responsesTestChunkStream(client.StreamChunk{
				IsFinal:   true,
				ToolCalls: []client.ToolCall{toolCall},
			})
		},
	)

	chunks := []client.StreamChunk{}
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want hidden tool call to retry", attempts)
	}
	if len(chunks) != 2 || chunks[0].Text != "recovered" ||
		!chunks[1].IsFinal {
		t.Fatalf("hidden backend tool call escaped: %#v", chunks)
	}
}

func TestResponsesStreamBufferedSimulationRetriesBeforePublishing(t *testing.T) {
	attempts := 0

	ch := responsesStreamWithEmptyRetry(
		context.Background(),
		"conv-poisoned",
		[]time.Duration{0},
		true,
		nil,
		func(_ context.Context, _ string) <-chan client.StreamChunk {
			attempts++
			if attempts == 1 {
				return responsesTestChunkStream(
					client.StreamChunk{Thinking: "hidden transport reasoning"},
					client.StreamChunk{
						IsFinal: true,
						ToolCalls: []client.ToolCall{{
							ID: "backend-only",
						}},
					},
				)
			}
			return responsesTestChunkStream(
				client.StreamChunk{Text: "recovered"},
				client.StreamChunk{IsFinal: true},
			)
		},
	)

	chunks := []client.StreamChunk{}
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if len(chunks) != 2 || chunks[0].Text != "recovered" ||
		chunks[0].Thinking != "" || !chunks[1].IsFinal {
		t.Fatalf("buffered retry exposed discarded attempt: %#v", chunks)
	}
}

func TestResponsesStreamSimulationPublishesTextBeforeFinal(t *testing.T) {
	upstream := make(chan client.StreamChunk, 2)
	ch := responsesStreamWithEmptyRetry(
		context.Background(),
		"",
		nil,
		true,
		nil,
		func(_ context.Context, _ string) <-chan client.StreamChunk {
			return upstream
		},
	)

	upstream <- client.StreamChunk{
		Text: `{"choices":[{"message":{"content":"çalışıyor"}}`,
	}

	select {
	case chunk := <-ch:
		if chunk.Text == "" {
			t.Fatalf("first streamed chunk had no text: %#v", chunk)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("simulated Responses text remained buffered until final")
	}

	upstream <- client.StreamChunk{IsFinal: true}
	close(upstream)
	for range ch {
	}
}

func TestLimitResponsesStreamDeltaHonorsMaxTokens(t *testing.T) {
	emitted, published, truncated := limitResponsesStreamDelta(
		"",
		"hello world",
		1,
	)
	if emitted == "" || published != emitted || !truncated {
		t.Fatalf(
			"first limited delta = (%q, %q, %t)",
			emitted,
			published,
			truncated,
		)
	}
	if countTokens(published) > 1 {
		t.Fatalf("published content exceeds token limit: %q", published)
	}

	emitted, next, truncated := limitResponsesStreamDelta(
		published,
		" ignored",
		1,
	)
	if emitted != "" || next != published || !truncated {
		t.Fatalf(
			"post-limit delta = (%q, %q, %t)",
			emitted,
			next,
			truncated,
		)
	}
}

func TestResponsesStreamWithoutFinalReturnsUpstreamError(t *testing.T) {
	attempts := 0

	ch := responsesStreamWithEmptyRetry(
		context.Background(),
		"",
		[]time.Duration{0},
		false,
		nil,
		func(_ context.Context, _ string) <-chan client.StreamChunk {
			attempts++
			return responsesTestChunkStream(client.StreamChunk{Text: "partial"})
		},
	)

	chunks := []client.StreamChunk{}
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 for a broken stream", attempts)
	}
	if len(chunks) != 2 || chunks[0].Text != "partial" ||
		chunks[1].Error == nil {
		t.Fatalf("broken stream did not end with an error: %#v", chunks)
	}
}

func TestCanceledResponsesRequestClearsStickySession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cache := NewContextCache(t.TempDir())
	sid := "canceled-session"
	cache.Set("session:"+sid, "conv-poisoned")
	api := &APIServer{ctxCache: cache}

	if !api.responsesRequestCanceled(ctx, sid) {
		t.Fatal("canceled Responses request was not detected")
	}
	if got := cache.Get("session:" + sid); got != "" {
		t.Fatalf("canceled Responses request kept sticky session %q", got)
	}
}

func responsesTestChunkStream(chunks ...client.StreamChunk) <-chan client.StreamChunk {
	ch := make(chan client.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		ch <- chunk
	}
	close(ch)
	return ch
}

func TestWriteResponsesUpstreamEmptyErrorNonStreaming(t *testing.T) {
	rec := httptest.NewRecorder()

	writeResponsesUpstreamEmptyError(rec, false, "resp_test", "gpt-test")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	errorObject, _ := body["error"].(map[string]interface{})
	if errorObject["code"] != upstreamEmptyResponseCode {
		t.Fatalf("error code = %#v, want %q", errorObject["code"], upstreamEmptyResponseCode)
	}
	if message, _ := errorObject["message"].(string); strings.Contains(
		strings.ToLower(message),
		"throttl",
	) {
		t.Fatalf("empty response error speculates about throttling: %q", message)
	}
}

func TestWriteResponsesUpstreamEmptyErrorStreaming(t *testing.T) {
	rec := httptest.NewRecorder()

	writeResponsesUpstreamEmptyError(rec, true, "resp_test", "gpt-test")

	body := rec.Body.String()
	if !strings.Contains(body, `"type":"response.failed"`) {
		t.Fatalf("stream missing response.failed event:\n%s", body)
	}
	if !strings.Contains(body, `"code":"`+upstreamEmptyResponseCode+`"`) {
		t.Fatalf("stream missing stable error code:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream missing terminal marker:\n%s", body)
	}
}

func simulatedToolCallEnvelope(name string) string {
	return "```json\n" +
		`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_test","type":"function","function":{"name":"` +
		name +
		`","arguments":"{\"path\":\"nonce.txt\"}"}}]},"finish_reason":"tool_calls"}]}` +
		"\n```"
}
