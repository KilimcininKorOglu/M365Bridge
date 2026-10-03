package servers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
)

type modelListResponse struct {
	Object string `json:"object"`
	Data   []struct {
		ID              string `json:"id"`
		OwnedBy         string `json:"owned_by"`
		ContextWindow   int    `json:"context_window"`
		MaxInputTokens  int    `json:"max_input_tokens"`
		MaxOutputTokens int    `json:"max_output_tokens"`
		SupportsTools   bool   `json:"supports_tools"`
		BaseInstruct    string `json:"base_instructions"`
		ApplyPatchType  string `json:"apply_patch_tool_type"`
		DefaultLevel    string `json:"default_reasoning_level"`
		Capabilities    struct {
			SupportsTools bool `json:"supports_tools"`
		} `json:"capabilities"`
	} `json:"data"`
	ReasoningEffortPresets []models.ReasoningEffortPreset `json:"reasoning_effort_presets"`
}

func fetchModels(t *testing.T, cfg *models.Config) modelListResponse {
	t.Helper()
	api := &APIServer{config: cfg}
	recorder := httptest.NewRecorder()
	api.handleModels(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var got modelListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return got
}

func TestModelsListIsDeduplicatedAndStable(t *testing.T) {
	cfg := &models.Config{ContextWindowTokens: 1_000_000, MaxOutputTokens: 1_000_000}
	first := fetchModels(t, cfg)

	seen := make(map[string]bool, len(first.Data))
	for _, model := range first.Data {
		if seen[model.ID] {
			t.Fatalf("model %q listed twice", model.ID)
		}
		seen[model.ID] = true
	}

	// Several registry keys alias the same model, and a map range would report
	// them in a different order on every request.
	second := fetchModels(t, cfg)
	if len(first.Data) != len(second.Data) {
		t.Fatalf("list length changed between requests: %d then %d", len(first.Data), len(second.Data))
	}
	for i := range first.Data {
		if first.Data[i].ID != second.Data[i].ID {
			t.Fatalf("order changed at %d: %q then %q", i, first.Data[i].ID, second.Data[i].ID)
		}
	}
}

func TestAdditionalTonesResolveAndAppearInCatalog(t *testing.T) {
	want := map[string]struct {
		id   string
		tone string
	}{
		"gpt6-astra":                   {"gpt-6-astra", "GPT_6_Astra"},
		"gpt5.6-chat-uppercase":        {"gpt-5.6-chat-uppercase", "GPT_5_6_Chat"},
		"gpt5.6-reasoning-uppercase":   {"gpt-5.6-reasoning-uppercase", "GPT_5_6_Reasoning"},
		"gpt6-sol-chat":                {"gpt-6-sol-chat", "Gpt_6_Sol_Chat"},
		"gpt6-sol-reasoning":           {"gpt-6-sol-reasoning", "Gpt_6_Sol_Reasoning"},
		"gpt6-sol-chat-uppercase":      {"gpt-6-sol-chat-uppercase", "GPT_6_Sol_Chat"},
		"gpt6-sol-reasoning-uppercase": {"gpt-6-sol-reasoning-uppercase", "GPT_6_Sol_Reasoning"},
	}
	listed := make(map[string]bool)
	for _, entry := range fetchModels(t, &models.Config{}).Data {
		listed[entry.ID] = true
	}
	for key, expected := range want {
		cfg, ok := models.FindModel(key)
		if !ok || cfg.OpenAIID != expected.id || cfg.Tone != expected.tone {
			t.Errorf("%s: resolved (%v, %q, %q), want (%q, %q)", key, ok, cfg.OpenAIID, cfg.Tone, expected.id, expected.tone)
		}
		if !listed[expected.id] {
			t.Errorf("%s is absent from /v1/models", expected.id)
		}
	}
}

func TestModelsListReportsOwnerAndCapabilities(t *testing.T) {
	got := fetchModels(t, &models.Config{ContextWindowTokens: 1_000_000, MaxOutputTokens: 1_000_000})

	owners := make(map[string]string, len(got.Data))
	for _, model := range got.Data {
		owners[model.ID] = model.OwnedBy
		if !model.SupportsTools {
			t.Fatalf("model %q reports no tool support", model.ID)
		}
	}
	if owners["claude-sonnet-4.6"] != models.OwnerAnthropic {
		t.Fatalf("claude model owner = %q, want %q", owners["claude-sonnet-4.6"], models.OwnerAnthropic)
	}
	if owners["gpt-5.5"] != models.OwnerMicrosoft {
		t.Fatalf("gpt model owner = %q, want %q", owners["gpt-5.5"], models.OwnerMicrosoft)
	}

	if len(got.ReasoningEffortPresets) != len(models.ReasoningEffortPresets) {
		t.Fatalf("reasoning effort presets = %v, want %v", got.ReasoningEffortPresets, models.ReasoningEffortPresets)
	}
	for _, preset := range got.ReasoningEffortPresets {
		if preset.Description == "" {
			t.Fatalf("preset %q carries no description", preset.Effort)
		}
	}
}

// Codex reads a further set of catalog fields that plain OpenAI clients ignore,
// and different clients look for capabilities in different places, so the same
// facts appear at the top level and under capabilities.
func TestModelsListCarriesCodexCatalogFields(t *testing.T) {
	got := fetchModels(t, &models.Config{ContextWindowTokens: 200_000, MaxOutputTokens: 64_000})

	for _, model := range got.Data {
		if model.BaseInstruct == "" {
			t.Fatalf("model %q has no base_instructions", model.ID)
		}
		if model.ApplyPatchType != "freeform" {
			t.Fatalf("model %q apply_patch_tool_type = %q", model.ID, model.ApplyPatchType)
		}
		if model.DefaultLevel != "medium" {
			t.Fatalf("model %q default_reasoning_level = %q", model.ID, model.DefaultLevel)
		}
		if !model.Capabilities.SupportsTools {
			t.Fatalf("model %q reports no tool support under capabilities", model.ID)
		}
	}
}

func TestModelsInputBudgetNeverReachesZero(t *testing.T) {
	// The defaults advertise the same value for both hints. Subtracting one
	// from the other would tell clients they may send nothing at all.
	same := fetchModels(t, &models.Config{ContextWindowTokens: 1_000_000, MaxOutputTokens: 1_000_000})
	if same.Data[0].MaxInputTokens != 1_000_000 {
		t.Fatalf("max_input_tokens = %d, want the full window", same.Data[0].MaxInputTokens)
	}

	carved := fetchModels(t, &models.Config{ContextWindowTokens: 200_000, MaxOutputTokens: 64_000})
	if carved.Data[0].MaxInputTokens != 136_000 {
		t.Fatalf("max_input_tokens = %d, want 136000", carved.Data[0].MaxInputTokens)
	}
}
