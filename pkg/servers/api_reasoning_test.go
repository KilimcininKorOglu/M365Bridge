package servers

import (
	"strings"
	"testing"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
)

func TestReasoningEffortValidation(t *testing.T) {
	if deliberate, err := reasoningEffortRequestsDeliberation(nil); err != nil || deliberate {
		t.Fatalf("absent reasoning block: deliberate=%v err=%v, want false and no error", deliberate, err)
	}

	// Codex sends the effort with varying case and spacing.
	for _, effort := range []string{"none", "minimal", "low", "MEDIUM", " high ", "xhigh", "max"} {
		if _, err := reasoningEffortRequestsDeliberation(&responsesReasoning{Effort: effort}); err != nil {
			t.Fatalf("effort %q rejected: %v", effort, err)
		}
	}

	err := reasoningEffortRequestsDeliberationError(t, "turbo")
	if !strings.Contains(err, "turbo") {
		t.Fatalf("error %q does not name the rejected value", err)
	}
	for _, name := range models.ReasoningEffortNames() {
		if !strings.Contains(err, name) {
			t.Fatalf("error %q does not list the accepted value %q", err, name)
		}
	}
}

func reasoningEffortRequestsDeliberationError(t *testing.T, effort string) string {
	t.Helper()
	_, err := reasoningEffortRequestsDeliberation(&responsesReasoning{Effort: effort})
	if err == nil {
		t.Fatalf("effort %q was accepted", effort)
	}
	return err.Error()
}

func TestReasoningEffortThreshold(t *testing.T) {
	cases := map[string]bool{
		"none":    false,
		"minimal": false,
		"low":     false,
		"medium":  true,
		"high":    true,
		"xhigh":   true,
		"max":     true,
	}
	for effort, want := range cases {
		got, err := reasoningEffortRequestsDeliberation(&responsesReasoning{Effort: effort})
		if err != nil {
			t.Fatalf("effort %q rejected: %v", effort, err)
		}
		if got != want {
			t.Fatalf("effort %q asks to deliberate = %v, want %v", effort, got, want)
		}
	}
}

func TestApplyReasoningEffortRoutesToTheVariant(t *testing.T) {
	base := models.ModelRegistry["gpt5.5"]
	got := applyReasoningEffort("gpt5.5", base, true)
	if got.Tone != models.ModelRegistry["gpt5.5-reasoning"].Tone {
		t.Fatalf("tone = %q, want the reasoning variant", got.Tone)
	}
}

func TestApplyReasoningEffortLeavesModelsWithoutAVariant(t *testing.T) {
	// The tone is the only lever, so a model with no reasoning variant must be
	// left alone rather than silently swapped for an unrelated one.
	base := models.ModelRegistry["claude-opus"]
	if got := applyReasoningEffort("claude-opus", base, true); got.Tone != base.Tone {
		t.Fatalf("model without a variant was rerouted to %q", got.Tone)
	}

	// A key that already names a reasoning variant must not grow a second suffix.
	variant := models.ModelRegistry["gpt5.5-reasoning"]
	if got := applyReasoningEffort("gpt5.5-reasoning", variant, true); got.Tone != variant.Tone {
		t.Fatalf("reasoning variant was rerouted to %q", got.Tone)
	}

	// Low effort leaves the tone alone even when a variant exists.
	base = models.ModelRegistry["gpt5.5"]
	if got := applyReasoningEffort("gpt5.5", base, false); got.Tone != base.Tone {
		t.Fatalf("low effort rerouted the model to %q", got.Tone)
	}
}

func assertOpenAIDeliberation(t *testing.T, req chatCompletionsRequest, want bool) {
	t.Helper()
	got, err := openAIChatRequestsDeliberation(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("openAIChatRequestsDeliberation = %v, want %v", got, want)
	}
}

func TestOpenAIChatRequestsDeliberation(t *testing.T) {
	assertOpenAIDeliberation(t, chatCompletionsRequest{}, false)
	assertOpenAIDeliberation(t, chatCompletionsRequest{ReasoningEffort: "high"}, true)
	assertOpenAIDeliberation(t, chatCompletionsRequest{ReasoningEffort: "low"}, false)
	assertOpenAIDeliberation(t, chatCompletionsRequest{Reasoning: &responsesReasoning{Effort: "medium"}}, true)
	if _, err := openAIChatRequestsDeliberation(chatCompletionsRequest{ReasoningEffort: "invalid"}); err == nil {
		t.Fatal("expected error for invalid effort")
	}
}

func assertAnthropicDeliberation(t *testing.T, req anthropicMessagesRequest, want bool) {
	t.Helper()
	got, err := anthropicRequestsDeliberation(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("anthropicRequestsDeliberation = %v, want %v", got, want)
	}
}

func TestAnthropicRequestsDeliberation(t *testing.T) {
	assertAnthropicDeliberation(t, anthropicMessagesRequest{}, false)
	assertAnthropicDeliberation(t, anthropicMessagesRequest{Thinking: &anthropicThinking{Type: "enabled"}}, true)
	assertAnthropicDeliberation(t, anthropicMessagesRequest{Thinking: &anthropicThinking{Type: "disabled"}}, false)
	assertAnthropicDeliberation(t, anthropicMessagesRequest{OutputConfig: &anthropicOutputConfig{Effort: "high"}}, true)
	assertAnthropicDeliberation(t, anthropicMessagesRequest{ReasoningEffort: "high"}, true)
}
