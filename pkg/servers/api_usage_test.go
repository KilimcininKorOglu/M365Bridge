package servers

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/payload"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/toolcalling"
)

// usageProbeMessages is the turn every usage test counts, so a divergence
// between two endpoints shows up as a difference in the reported number rather
// than a difference in the input.
func usageProbeMessages() []payload.Message {
	return []payload.Message{
		{Role: "system", Content: "You are terse."},
		{Role: "user", Content: "Reply with exactly: OK"},
	}
}

// The Go representation of a message slice prints every field of every
// message, empty ones included, wrapped in braces and brackets. Running the
// encoder over that counts punctuation the request never carried and skips the
// protocol framing the request did carry, so it answers a different question
// from countPromptTokens. This test guards the difference.
func TestStructPrintIsNotAPromptCount(t *testing.T) {
	messages := usageProbeMessages()

	correct := countPromptTokens(messages, nil, "")
	structPrint := countTokens(fmt.Sprint(messages))
	if correct == structPrint {
		t.Fatalf("both counters returned %d, so the test cannot tell them apart", correct)
	}

	// The empty fields are what makes the printed form diverge from the text
	// that was actually sent.
	printed := fmt.Sprint(messages)
	if !strings.Contains(printed, "[] []") {
		t.Fatalf("fmt.Sprint no longer prints the empty fields; this guard is stale: %q", printed)
	}
}

// The Responses policy defaults its prompt choice to "auto" for every request,
// including one that declares no tools. Billing that framing would make the
// same turn cost more on /v1/responses than on /v1/chat/completions.
func TestToolChoiceFramingNeedsDeclaredTools(t *testing.T) {
	messages := usageProbeMessages()

	bare := countPromptTokens(messages, nil, "")
	withDefaultChoice := countPromptTokens(messages, nil, "auto")
	if bare != withDefaultChoice {
		t.Fatalf("a tool choice without tools changed the count from %d to %d", bare, withDefaultChoice)
	}

	tools := []toolcalling.ToolDef{{
		Type:     "function",
		Function: toolcalling.ToolDefFunc{Name: "get_weather"},
	}}
	withTools := countPromptTokens(messages, tools, "")
	withToolsAndChoice := countPromptTokens(messages, tools, "auto")
	if withToolsAndChoice != withTools+toolChoiceProtocolTokens {
		t.Fatalf("a real tool choice added %d tokens, want %d",
			withToolsAndChoice-withTools, toolChoiceProtocolTokens)
	}
}

// decodeUsage reads the usage object out of a JSON body under the given key.
func decodeUsage(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode body %s: %v", body, err)
	}
	usage, ok := decoded["usage"].(map[string]any)
	if !ok {
		t.Fatalf("body carries no usage object: %s", body)
	}
	return usage
}

// usageNumber reads one field as an int, failing when it is absent.
func usageNumber(t *testing.T, usage map[string]any, key string) int {
	t.Helper()
	value, ok := usage[key].(float64)
	if !ok {
		t.Fatalf("usage has no numeric %s: %#v", key, usage)
	}
	return int(value)
}

// requireUsageSource fails when the usage object does not name its encoder.
func requireUsageSource(t *testing.T, usage map[string]any) {
	t.Helper()
	source, ok := usage["usage_source"].(string)
	if !ok || source == "" {
		t.Fatalf("usage does not name its source: %#v", usage)
	}
}

// bufferedProbeResult is the finished turn the buffered responders receive from
// runToolLoop. They reach no upstream, so they can be driven directly.
func bufferedProbeResult() toolLoopResult {
	return toolLoopResult{
		text:           "OK",
		thinking:       "Checking the request.",
		finishReason:   "stop",
		conversationID: "conv-usage-probe",
	}
}

// The buffered coding-tool responders answer a turn that runToolLoop already
// finished. They used to report either no usage at all or a count taken from
// the Go representation of the message slice.
func TestBufferedChatReportsUsage(t *testing.T) {
	api := &APIServer{ctxCache: NewContextCache(t.TempDir())}
	cfg := models.ModelConfig{OpenAIID: "gpt-5.5"}
	messages := usageProbeMessages()

	rec := httptest.NewRecorder()
	api.respondBufferedChat(rec, bufferedProbeResult(), messages, cfg, "", 0, false, nil, "", nil)

	usage := decodeUsage(t, rec.Body.Bytes())
	requireUsageSource(t, usage)
	prompt := usageNumber(t, usage, "prompt_tokens")
	completion := usageNumber(t, usage, "completion_tokens")
	reasoning := usageNumber(t, usage, "reasoning_tokens")
	if total := usageNumber(t, usage, "total_tokens"); total != prompt+completion+reasoning {
		t.Fatalf("total_tokens = %d, want %d", total, prompt+completion+reasoning)
	}
	if want := countPromptTokens(messages, nil, ""); prompt != want {
		t.Fatalf("prompt_tokens = %d, want %d", prompt, want)
	}
	if reasoning == 0 {
		t.Fatal("the turn carried thinking but reported no reasoning tokens")
	}
}

func TestBufferedChatStreamEndsWithUsage(t *testing.T) {
	api := &APIServer{ctxCache: NewContextCache(t.TempDir())}
	cfg := models.ModelConfig{OpenAIID: "gpt-5.5"}

	rec := httptest.NewRecorder()
	api.respondBufferedChat(rec, bufferedProbeResult(), usageProbeMessages(), cfg, "", 0, true, nil, "", nil)

	body := rec.Body.String()
	if !strings.Contains(body, `"usage"`) {
		t.Fatalf("the streamed turn carried no usage:\n%s", body)
	}
	if !strings.Contains(body, "usage_source") {
		t.Fatalf("the streamed usage did not name its source:\n%s", body)
	}
}

func TestBufferedAnthropicMatchesTheChatPromptCount(t *testing.T) {
	api := &APIServer{ctxCache: NewContextCache(t.TempDir())}
	messages := usageProbeMessages()

	rec := httptest.NewRecorder()
	api.respondBufferedAnthropic(rec, bufferedProbeResult(), messages, "claude-sonnet-4", 0, false, nil, "", nil)

	usage := decodeUsage(t, rec.Body.Bytes())
	requireUsageSource(t, usage)
	if got, want := usageNumber(t, usage, "input_tokens"), countPromptTokens(messages, nil, ""); got != want {
		t.Fatalf("input_tokens = %d, want %d", got, want)
	}

	// The same turn must cost the same on both wire formats.
	chatRec := httptest.NewRecorder()
	api.respondBufferedChat(chatRec, bufferedProbeResult(), messages, models.ModelConfig{OpenAIID: "gpt-5.5"}, "", 0, false, nil, "", nil)
	chatUsage := decodeUsage(t, chatRec.Body.Bytes())
	if a, o := usageNumber(t, usage, "input_tokens"), usageNumber(t, chatUsage, "prompt_tokens"); a != o {
		t.Fatalf("anthropic counted %d prompt tokens, openai counted %d", a, o)
	}
	if a, o := usageNumber(t, usage, "output_tokens"), usageNumber(t, chatUsage, "completion_tokens"); a != o {
		t.Fatalf("anthropic counted %d output tokens, openai counted %d", a, o)
	}
}

func TestBufferedResponsesMatchesTheChatPromptCount(t *testing.T) {
	api := &APIServer{ctxCache: NewContextCache(t.TempDir())}
	messages := usageProbeMessages()

	rec := httptest.NewRecorder()
	api.respondBufferedResponses(rec, bufferedProbeResult(), messages, models.ModelConfig{OpenAIID: "gpt-5.5"}, 0, false, nil, false, nil, "")

	usage := decodeUsage(t, rec.Body.Bytes())
	requireUsageSource(t, usage)
	if got, want := usageNumber(t, usage, "input_tokens"), countPromptTokens(messages, nil, ""); got != want {
		t.Fatalf("input_tokens = %d, want %d", got, want)
	}
	input := usageNumber(t, usage, "input_tokens")
	output := usageNumber(t, usage, "output_tokens")
	reasoning := usageNumber(t, usage, "reasoning_tokens")
	if total := usageNumber(t, usage, "total_tokens"); total != input+output+reasoning {
		t.Fatalf("total_tokens = %d, want %d", total, input+output+reasoning)
	}
}
