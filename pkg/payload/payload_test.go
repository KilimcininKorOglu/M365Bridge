package payload

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestBuildURLUsesEduStarterRoute(t *testing.T) {
	raw, _, _, err := BuildURL(
		"token",
		"0123456789abcdef0123456789abcdef",
		"",
		"user",
		"tenant",
	)
	if err != nil {
		t.Fatalf("BuildURL returned error: %v", err)
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("BuildURL returned invalid URL: %v", err)
	}
	query := parsed.Query()
	if got := query.Get("licenseType"); got != "Starter" {
		t.Fatalf("licenseType = %q, want Starter", got)
	}
	if got := query.Get("isEdu"); got != "true" {
		t.Fatalf("isEdu = %q, want true", got)
	}
	if got := query.Get("scenario"); got != "OfficeWebIncludedCopilot" {
		t.Fatalf("scenario = %q, want OfficeWebIncludedCopilot", got)
	}
}

func TestBuildURLWithScenarioElevates(t *testing.T) {
	raw, _, _, err := BuildURLWithScenario(
		"token",
		"0123456789abcdef0123456789abcdef",
		"",
		"user",
		"tenant",
		"OfficeWebPaidCopilot",
		"Premium",
	)
	if err != nil {
		t.Fatalf("BuildURLWithScenario returned error: %v", err)
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("BuildURLWithScenario returned invalid URL: %v", err)
	}
	query := parsed.Query()
	if got := query.Get("licenseType"); got != "Premium" {
		t.Fatalf("licenseType = %q, want Premium", got)
	}
	if got := query.Get("scenario"); got != "OfficeWebPaidCopilot" {
		t.Fatalf("scenario = %q, want OfficeWebPaidCopilot", got)
	}
}

// The flag is measured rather than copied from a browser capture: without it a
// long answer arrived as about 840 writeAtCursor deltas, with it as about 130,
// carrying the same bytes. Losing it costs six times more SSE frames per turn
// for the same content, and nothing in the answer would look wrong.
func TestBuildURLAsksTheBackendToMergePureDeltas(t *testing.T) {
	raw, _, _, err := BuildURL("token", "0123456789abcdef0123456789abcdef", "", "user", "tenant")
	if err != nil {
		t.Fatalf("BuildURL returned error: %v", err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("BuildURL returned invalid URL: %v", err)
	}

	flags := strings.Split(parsed.Query().Get("variants"), ",")
	if !slices.Contains(flags, "feature.EnableMergingPureDeltas") {
		t.Fatalf("variants does not carry feature.EnableMergingPureDeltas: %v", flags)
	}
}

func TestConversationTextForM365IncludesClientHistoryWhenRequested(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "Read nonce.txt."},
		{Role: "assistant", Content: "Tool call: read_nonce({})"},
		{Role: "user", Content: "Authoritative tool result: NONCE-EXACT"},
		{Role: "user", Content: "Return the exact tool result now."},
	}

	got := conversationTextForM365(messages, true)

	for _, expected := range []string{
		"CLIENT-PROVIDED CONVERSATION HISTORY",
		"Read nonce.txt.",
		"Tool call: read_nonce({})",
		"Authoritative tool result: NONCE-EXACT",
		"CURRENT USER MESSAGE",
		"Return the exact tool result now.",
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("flattened conversation lost %q:\n%s", expected, got)
		}
	}
}

func TestConversationTextForM365KeepsOnlyCurrentMessageForStickyConversation(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "Earlier user message"},
		{Role: "assistant", Content: "Earlier assistant message"},
		{Role: "user", Content: "Current request"},
	}

	got := conversationTextForM365(messages, false)

	if got != "Current request" {
		t.Fatalf("sticky conversation text = %q, want current request only", got)
	}
}

// The M365 backend accepts image attachments only. A file or audio block must
// not break the request and must not silently turn into text either.
func TestUnsupportedContentBlocksAreDropped(t *testing.T) {
	raw := `{"role":"user","content":[
		{"type":"text","text":"summarize this"},
		{"type":"input_file","file_id":"file_1"},
		{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}
	]}`

	var message Message
	if err := json.Unmarshal([]byte(raw), &message); err != nil {
		t.Fatalf("unsupported blocks broke decoding: %v", err)
	}
	if message.Content != "summarize this" {
		t.Fatalf("content = %q, want only the text block", message.Content)
	}
	if len(message.Images) != 0 {
		t.Fatalf("a non-image block became an attachment: %#v", message.Images)
	}
}

func TestOpenAIToolStructureSurvivesFlattening(t *testing.T) {
	// The content is flattened to text for the backend, but the name, the
	// arguments and the result body have to stay reachable so the server can
	// tell which call a later turn is repeating.
	var call Message
	raw := `{"role":"assistant","content":null,"tool_calls":[
		{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Ankara\"}"}}
	]}`
	if err := json.Unmarshal([]byte(raw), &call); err != nil {
		t.Fatalf("decode assistant tool call: %v", err)
	}
	if len(call.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v, want one record", call.ToolCalls)
	}
	if call.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("tool name = %q, want get_weather", call.ToolCalls[0].Name)
	}
	if !strings.Contains(call.ToolCalls[0].Arguments, "Ankara") {
		t.Fatalf("arguments = %q, want the city preserved", call.ToolCalls[0].Arguments)
	}

	var result Message
	if err := json.Unmarshal([]byte(`{"role":"tool","tool_call_id":"call_1","content":"sunny, 24C"}`), &result); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	if len(result.ToolResults) != 1 {
		t.Fatalf("tool results = %#v, want one record", result.ToolResults)
	}
	if result.ToolResults[0].Content != "sunny, 24C" {
		t.Fatalf("result body = %q, want the raw result without the text wrapper", result.ToolResults[0].Content)
	}
}

func TestAnthropicToolStructureSurvivesFlattening(t *testing.T) {
	var call Message
	raw := `{"role":"assistant","content":[
		{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"main.go"}}
	]}`
	if err := json.Unmarshal([]byte(raw), &call); err != nil {
		t.Fatalf("decode tool_use: %v", err)
	}
	if len(call.ToolCalls) != 1 || call.ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool calls = %#v, want one read_file record", call.ToolCalls)
	}
	if !strings.Contains(call.ToolCalls[0].Arguments, "main.go") {
		t.Fatalf("arguments = %q, want the path preserved", call.ToolCalls[0].Arguments)
	}

	var result Message
	raw = `{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"toolu_1","content":"package main"}
	]}`
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode tool_result: %v", err)
	}
	if len(result.ToolResults) != 1 || result.ToolResults[0].Content != "package main" {
		t.Fatalf("tool results = %#v, want the raw result body", result.ToolResults)
	}
}

func TestWebSearchPluginIsGated(t *testing.T) {
	raw, err := BuildConversationPayload("sid", "uuid", []Message{{Role: "user", Content: "hi"}}, false, "Magic", "", false, false, true, nil)
	if err != nil {
		t.Fatalf("build with web search: %v", err)
	}
	if !strings.Contains(raw, "BingWebSearch") {
		t.Fatal("the built-in was withheld while web search is enabled")
	}

	raw, err = BuildConversationPayload("sid", "uuid", []Message{{Role: "user", Content: "hi"}}, false, "Magic", "", false, false, false, nil)
	if err != nil {
		t.Fatalf("build without web search: %v", err)
	}
	if strings.Contains(raw, "BingWebSearch") {
		t.Fatal("the built-in was declared while web search is disabled")
	}
}
