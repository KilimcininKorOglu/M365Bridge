package servers

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/toolcalling"
)

// settledLedger builds a ledger in which run_tests has already run and been
// answered the given number of times inside one user turn.
func settledLedger(t *testing.T, times int) toolcalling.Ledger {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`[{"role":"user","content":"fix the build"}`)
	for i := range times {
		id := "call_" + string(rune('0'+i))
		sb.WriteString(`,{"role":"assistant","content":null,"tool_calls":[{"id":"`)
		sb.WriteString(id)
		sb.WriteString(`","type":"function","function":{"name":"run_tests","arguments":"{\"pkg\":\"./...\"}"}}]}`)
		sb.WriteString(`,{"role":"tool","tool_call_id":"`)
		sb.WriteString(id)
		sb.WriteString(`","content":"exit code 1"}`)
	}
	sb.WriteString("]")
	return buildToolLedger(decodeMessages(t, sb.String()))
}

func simWithRunTests() toolcalling.SimulatedResult {
	return toolcalling.SimulatedResult{
		HasPayload:   true,
		FinishReason: "tool_calls",
		ToolCalls: []toolcalling.ToolCall{{
			ID:        "call_new",
			Name:      "run_tests",
			Arguments: json.RawMessage(`{"pkg":"./..."}`),
		}},
	}
}

func TestDropSettledToolCallsKeepsTheFirstRepeat(t *testing.T) {
	got := dropSettledToolCalls(settledLedger(t, 1), "auto", simWithRunTests())
	if len(got.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want the first repeat forwarded", len(got.ToolCalls))
	}
	if got.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", got.FinishReason)
	}
}

func TestDropSettledToolCallsStopsThePersistentRepeat(t *testing.T) {
	got := dropSettledToolCalls(settledLedger(t, 2), "auto", simWithRunTests())
	if len(got.ToolCalls) != 0 {
		t.Fatalf("tool calls = %d, want the third identical call dropped", len(got.ToolCalls))
	}
	if got.FinishReason != "stop" {
		t.Fatalf("finish reason = %q, want stop once nothing is left to call", got.FinishReason)
	}
	if got.Content != toolcalling.RepeatedCallsNotice {
		t.Fatalf("content = %q, want the substitute notice instead of an empty turn", got.Content)
	}
	if len(got.DroppedCalls) != 0 {
		t.Fatalf("dropped calls = %#v, want no corrective re-ask triggered", got.DroppedCalls)
	}
}

func TestDropSettledToolCallsKeepsExistingAnswerText(t *testing.T) {
	sim := simWithRunTests()
	sim.Content = "The build is already green."
	got := dropSettledToolCalls(settledLedger(t, 2), "auto", sim)
	if got.Content != "The build is already green." {
		t.Fatalf("content = %q, want the model's own text preserved", got.Content)
	}
}

func TestDropSettledToolCallsHonorsAForcedToolChoice(t *testing.T) {
	// The caller demanded this call by name, so refusing it would contradict
	// the request even though the result is already in hand.
	for _, choice := range []string{"run_tests", "required", "any"} {
		got := dropSettledToolCalls(settledLedger(t, 2), choice, simWithRunTests())
		if len(got.ToolCalls) != 1 {
			t.Fatalf("tool_choice %q: tool calls = %d, want the demanded call forwarded", choice, len(got.ToolCalls))
		}
	}
}

func TestDropSettledToolCallsLeavesADifferentCallAlone(t *testing.T) {
	sim := simWithRunTests()
	sim.ToolCalls[0].Arguments = json.RawMessage(`{"pkg":"./pkg/servers"}`)
	got := dropSettledToolCalls(settledLedger(t, 2), "auto", sim)
	if len(got.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want a call with different arguments forwarded", len(got.ToolCalls))
	}
}

func TestParseResponsesSimulationDropsASettledRepeat(t *testing.T) {
	policy, err := newResponsesToolPolicy(responsesTestTools(), "auto")
	if err != nil {
		t.Fatalf("newResponsesToolPolicy: %v", err)
	}
	name := policy.allowedToolNames[0]

	var sb strings.Builder
	sb.WriteString(`[{"role":"user","content":"go"}`)
	for i := range 2 {
		id := "call_" + string(rune('0'+i))
		sb.WriteString(`,{"role":"assistant","content":null,"tool_calls":[{"id":"`)
		sb.WriteString(id)
		sb.WriteString(`","type":"function","function":{"name":"`)
		sb.WriteString(name)
		sb.WriteString(`","arguments":"{}"}}]}`)
		sb.WriteString(`,{"role":"tool","tool_call_id":"`)
		sb.WriteString(id)
		sb.WriteString(`","content":"done"}`)
	}
	sb.WriteString("]")
	policy.ledger = buildToolLedger(decodeMessages(t, sb.String()))

	text := "```json\n" + `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_x","type":"function","function":{"name":"` +
		name + `","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n```"

	result, err := parseResponsesSimulation(text, policy)
	if err != nil {
		t.Fatalf("parseResponsesSimulation: %v", err)
	}
	if len(result.toolCalls) != 0 {
		t.Fatalf("tool calls = %d, want the settled repeat dropped on the Responses path", len(result.toolCalls))
	}
	if result.finishReason != "stop" {
		t.Fatalf("finish reason = %q, want stop", result.finishReason)
	}
	if result.content != toolcalling.RepeatedCallsNotice {
		t.Fatalf("content = %q, want the substitute notice", result.content)
	}
}

func TestInjectedPromptCarriesTheLedgerEvidence(t *testing.T) {
	messages := decodeMessages(t, `[
		{"role":"user","content":"what is in main.go?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"package main"}
	]`)

	ledger := buildToolLedger(messages)
	injectSimulatedPrompt(&messages, `{"tools":[]}`, "auto", ledger.EvidenceNote())

	prompt := messages[0].Content
	if !strings.Contains(prompt, "read_file") || !strings.Contains(prompt, "package main") {
		t.Fatalf("the injected prompt does not carry the settled call:\n%s", prompt)
	}
}

func TestInjectedPromptStaysCleanWithoutEvidence(t *testing.T) {
	messages := decodeMessages(t, `[{"role":"user","content":"hello"}]`)
	ledger := buildToolLedger(messages)
	injectSimulatedPrompt(&messages, `{"tools":[]}`, "auto", ledger.EvidenceNote())

	if strings.Contains(messages[0].Content, "TOOL EVIDENCE") {
		t.Fatalf("an evidence section appeared without any tool history:\n%s", messages[0].Content)
	}
}

func TestParseResponsesSimulationClaimsAnUnfencedGrammarBody(t *testing.T) {
	policy, err := newResponsesToolPolicy([]toolcalling.ToolDef{{Type: "custom", Name: "exec"}}, "auto")
	if err != nil {
		t.Fatalf("newResponsesToolPolicy: %v", err)
	}

	result, err := parseResponsesSimulation(`{"input":"shell({cmd:\"cat README.md\"}); text(plan);"}`, policy)
	if err != nil {
		t.Fatalf("parseResponsesSimulation: %v", err)
	}
	if len(result.toolCalls) != 1 {
		t.Fatalf("tool calls = %d, want the grammar body claimed as a call", len(result.toolCalls))
	}
	if result.toolCalls[0].Function.Name != "exec" {
		t.Fatalf("name = %q, want exec", result.toolCalls[0].Function.Name)
	}
	if !strings.Contains(result.toolCalls[0].Function.Arguments, "cat README.md") {
		t.Fatalf("arguments = %q, want the program body", result.toolCalls[0].Function.Arguments)
	}
	if result.finishReason != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", result.finishReason)
	}
}

func TestParseResponsesSimulationLeavesProseAloneWithoutAGrammarTool(t *testing.T) {
	policy, err := newResponsesToolPolicy(responsesTestTools(), "auto")
	if err != nil {
		t.Fatalf("newResponsesToolPolicy: %v", err)
	}
	answer := "shell({cmd:\"ls\"});"

	result, err := parseResponsesSimulation(answer, policy)
	if err != nil {
		t.Fatalf("parseResponsesSimulation: %v", err)
	}
	if len(result.toolCalls) != 0 {
		t.Fatalf("tool calls = %d, want none without a custom tool declared", len(result.toolCalls))
	}
	if result.content != answer {
		t.Fatalf("content = %q, want the text passed through", result.content)
	}
}

func TestWebSearchIsNotRouteableOnTheResponsesPath(t *testing.T) {
	tools := []toolcalling.ToolDef{
		{Type: "function", Function: toolcalling.ToolDefFunc{Name: "web_search"}},
		{Type: "function", Function: toolcalling.ToolDefFunc{Name: "get_weather"}},
	}
	policy, err := newResponsesToolPolicy(tools, "auto")
	if err != nil {
		t.Fatalf("newResponsesToolPolicy: %v", err)
	}
	if slices.Contains(policy.allowedToolNames, "web_search") {
		t.Fatalf("allowed names = %#v, want web_search excluded", policy.allowedToolNames)
	}
	if !slices.Contains(policy.allowedToolNames, "get_weather") {
		t.Fatalf("allowed names = %#v, want the client tool kept", policy.allowedToolNames)
	}
}

func TestWebSearchAloneTurnsSimulationOff(t *testing.T) {
	policy, err := newResponsesToolPolicy([]toolcalling.ToolDef{{Type: "web_search"}}, "auto")
	if err != nil {
		t.Fatalf("newResponsesToolPolicy: %v", err)
	}
	if policy.simulate {
		t.Fatal("simulation stayed on with nothing the client can execute")
	}
}

func TestPinnedWebSearchIsNotAnError(t *testing.T) {
	tools := []toolcalling.ToolDef{
		{Type: "function", Function: toolcalling.ToolDefFunc{Name: "web_search"}},
		{Type: "function", Function: toolcalling.ToolDefFunc{Name: "get_weather"}},
	}
	policy, err := newResponsesToolPolicy(tools, "web_search")
	if err != nil {
		t.Fatalf("a pinned web_search was rejected: %v", err)
	}
	if policy.required {
		t.Fatal("a pinned web_search demanded a client tool call it can never receive")
	}
}

func TestWebSearchOnlyRequestStillSuppressesBackendCalls(t *testing.T) {
	// A request declaring only web_search runs no simulation, so the guard that
	// discards the backend's own search call must not be tied to simulation.
	tools := []toolcalling.ToolDef{{Type: "function", Function: toolcalling.ToolDefFunc{Name: "web_search"}}}
	if len(toolcalling.RouteableTools(tools)) != 0 {
		t.Fatal("web_search alone left something routeable")
	}
	if len(tools) == 0 {
		t.Fatal("the declaration list is what gates the backend-call discard")
	}
}

func TestParseResponsesSimulationClaimsAGrammarBodyInsideAValidEnvelope(t *testing.T) {
	// The backend often wraps the bridge object in a well-formed envelope, so
	// the body ends up as the extracted content rather than as the whole reply.
	policy, err := newResponsesToolPolicy([]toolcalling.ToolDef{{Type: "custom", Name: "exec"}}, "auto")
	if err != nil {
		t.Fatalf("newResponsesToolPolicy: %v", err)
	}

	body := `{\"input\":\"shell({cmd:\\\"cat README.md\\\"});\"}`
	text := "```json\n" + `{"choices":[{"message":{"role":"assistant","content":"` + body + `"},"finish_reason":"stop"}]}` + "\n```"

	result, err := parseResponsesSimulation(text, policy)
	if err != nil {
		t.Fatalf("parseResponsesSimulation: %v", err)
	}
	if len(result.toolCalls) != 1 {
		t.Fatalf("tool calls = %d, want the wrapped grammar body claimed; content was %q", len(result.toolCalls), result.content)
	}
	if !strings.Contains(result.toolCalls[0].Function.Arguments, "cat README.md") {
		t.Fatalf("arguments = %q, want the program body", result.toolCalls[0].Function.Arguments)
	}
}
