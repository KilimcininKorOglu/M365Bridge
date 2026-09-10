package servers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
)

// rawModels returns the catalog as decoded JSON, so a test can assert on the
// wire shape rather than on a Go struct that would hide a missing field.
func rawModels(t *testing.T) map[string]any {
	t.Helper()
	api := &APIServer{config: &models.Config{ContextWindowTokens: 1000000, MaxOutputTokens: 1000000}}
	rec := httptest.NewRecorder()
	api.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func entries(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	data, ok := body["data"].([]any)
	if !ok || len(data) == 0 {
		t.Fatalf("data is missing or empty: %v", body["data"])
	}
	out := make([]map[string]any, 0, len(data))
	for _, e := range data {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("entry is not an object: %v", e)
		}
		out = append(out, m)
	}
	return out
}

// The OpenAI Model schema requires id, object, created and owned_by, and the
// list response requires object and data.
func TestModelsCarriesTheRequiredOpenAIFields(t *testing.T) {
	body := rawModels(t)
	if body["object"] != "list" {
		t.Fatalf("object = %v, want list", body["object"])
	}
	for _, e := range entries(t, body) {
		if e["object"] != "model" {
			t.Errorf("%v: object = %v, want model", e["id"], e["object"])
		}
		if s, ok := e["id"].(string); !ok || s == "" {
			t.Errorf("entry has no id: %v", e)
		}
		if s, ok := e["owned_by"].(string); !ok || s == "" {
			t.Errorf("%v: owned_by is missing", e["id"])
		}
		if _, ok := e["created"].(float64); !ok {
			t.Errorf("%v: created is not a number: %v", e["id"], e["created"])
		}
		if _, present := e["shutdown_date"]; !present {
			t.Errorf("%v: shutdown_date is absent; the schema types it as nullable", e["id"])
		}
	}
}

// The Anthropic ModelInfo schema requires id, type, display_name and
// created_at, and the list response carries first_id, last_id and has_more.
func TestModelsCarriesTheRequiredAnthropicFields(t *testing.T) {
	body := rawModels(t)
	list := entries(t, body)
	assertAnthropicListFields(t, body, list)
	for _, e := range list {
		assertAnthropicModelInfo(t, e)
	}
}

// assertAnthropicListFields pins the paging fields of the list response, which
// Anthropic clients read to decide whether another page exists.
func assertAnthropicListFields(t *testing.T, body map[string]any, list []map[string]any) {
	t.Helper()
	if body["has_more"] != false {
		t.Fatalf("has_more = %v, want false", body["has_more"])
	}
	for _, key := range []string{"first_id", "last_id"} {
		if s, ok := body[key].(string); !ok || s == "" {
			t.Fatalf("%s is missing: %v", key, body[key])
		}
	}
	if body["first_id"] != list[0]["id"] {
		t.Errorf("first_id = %v, want %v", body["first_id"], list[0]["id"])
	}
	if body["last_id"] != list[len(list)-1]["id"] {
		t.Errorf("last_id = %v, want %v", body["last_id"], list[len(list)-1]["id"])
	}
}

// assertAnthropicModelInfo pins the four fields the ModelInfo schema requires
// of one entry.
func assertAnthropicModelInfo(t *testing.T, e map[string]any) {
	t.Helper()
	if e["type"] != "model" {
		t.Errorf("%v: type = %v, want model", e["id"], e["type"])
	}
	name, ok := e["display_name"].(string)
	if !ok || name == "" {
		t.Errorf("%v: display_name is missing", e["id"])
	}
	if name == e["id"] {
		t.Errorf("%v: display_name repeats the id instead of naming the model", e["id"])
	}
	created, ok := e["created_at"].(string)
	if !ok {
		t.Errorf("%v: created_at is missing", e["id"])
		return
	}
	if _, err := time.Parse(time.RFC3339, created); err != nil {
		t.Errorf("%v: created_at %q is not RFC 3339: %v", e["id"], created, err)
	}
}

// created and created_at describe the same instant, so a client reading either
// encoding must not see a different release time.
func TestModelsCreatedEncodingsAgree(t *testing.T) {
	for _, e := range entries(t, rawModels(t)) {
		unix := int64(e["created"].(float64))
		parsed, err := time.Parse(time.RFC3339, e["created_at"].(string))
		if err != nil {
			t.Fatalf("%v: %v", e["id"], err)
		}
		if parsed.Unix() != unix {
			t.Errorf("%v: created=%d but created_at=%s", e["id"], unix, e["created_at"])
		}
	}
}

// Merging Anthropic's capability tree into the flat OpenAI-style map is only
// safe while the key names stay disjoint. A future addition that reuses a name
// would silently overwrite the other protocol's value.
func TestModelsCapabilityNamespacesDoNotCollide(t *testing.T) {
	flat := map[string]any{
		"chat_completions": true, "responses": true, "streaming": true, "tools": true,
		"supports_tools": true, "tool_calls": true, "function_calling": true,
		"supports_function_calling": true, "reasoning": true, "reasoning_efforts": nil,
		"supported_reasoning_levels": nil, "reasoning_mode": "", "vision": true,
		"supports_vision": true, "modalities": nil, "input_modalities": nil,
		"output_modalities": nil, "supported_features": nil,
	}
	for name := range anthropicCapabilities(true, true) {
		if _, clash := flat[name]; clash {
			t.Errorf("capability %q exists in both namespaces and one would overwrite the other", name)
		}
	}
}

// wantEffort and wantThinking are written out per model rather than derived,
// because deriving them from the production helper would move the expectation
// in step with a broken implementation and the test would pass either way.
//
// effort is true where applyReasoningEffort has a `-reasoning` variant to route
// to; thinking is true where the tone was measured emitting a
// ChainOfThoughtSummary, which is not the same as its name carrying
// "Reasoning".
var wantEffort = map[string]bool{
	"gpt-4-auto": false, "gpt-4-quick": false, "gpt-4-reasoning": false,
	"gpt-5.2": true, "gpt-5.3": false, "gpt-5.4": true,
	"gpt-5.2-reasoning": true, "gpt-5.4-reasoning": true,
	"gpt-5.5": true, "gpt-5.5-reasoning": true, "gpt-5.6-reasoning": true,
	"claude-sonnet-4.6": false, "claude-opus-4.6": false,
}

var wantThinking = map[string]bool{
	"gpt-4-auto": false, "gpt-4-quick": false, "gpt-4-reasoning": true,
	"gpt-5.2": false, "gpt-5.3": false, "gpt-5.4": false,
	"gpt-5.2-reasoning": true, "gpt-5.4-reasoning": true,
	"gpt-5.5": false, "gpt-5.5-reasoning": true, "gpt-5.6-reasoning": true,
	"claude-sonnet-4.6": false, "claude-opus-4.6": true,
}

// A new registry entry must be added to both tables, otherwise the capability
// tests below would silently skip it.
func TestModelsCapabilityTablesCoverTheRegistry(t *testing.T) {
	for _, cfg := range models.ModelRegistry {
		if _, ok := wantEffort[cfg.OpenAIID]; !ok {
			t.Errorf("%s is missing from wantEffort", cfg.OpenAIID)
		}
		if _, ok := wantThinking[cfg.OpenAIID]; !ok {
			t.Errorf("%s is missing from wantThinking", cfg.OpenAIID)
		}
	}
}

// Effort is honoured only when applyReasoningEffort has a variant to route to,
// so a model without one must not claim the capability.
func TestModelsAdvertiseEffortOnlyWhereItRoutes(t *testing.T) {
	for _, e := range entries(t, rawModels(t)) {
		id := e["id"].(string)
		want, known := wantEffort[id]
		if !known {
			t.Errorf("%s has no expected effort value", id)
			continue
		}
		effort := e["capabilities"].(map[string]any)["effort"].(map[string]any)
		if effort["supported"] != want {
			t.Errorf("%s: effort.supported = %v, want %v", id, effort["supported"], want)
		}
		for _, level := range []string{"low", "medium", "high", "max", "xhigh"} {
			leaf, ok := effort[level].(map[string]any)
			if !ok {
				t.Fatalf("%s: effort.%s is missing", id, level)
			}
			if leaf["supported"] != want {
				t.Errorf("%s: effort.%s = %v, want %v", id, level, leaf["supported"], want)
			}
		}
	}
}

// Thinking reaches the client only from the reasoning tones, which emit
// ChainOfThoughtSummary.
func TestModelsAdvertiseThinkingOnlyForReasoningTones(t *testing.T) {
	for _, e := range entries(t, rawModels(t)) {
		id := e["id"].(string)
		want, known := wantThinking[id]
		if !known {
			t.Errorf("%s has no expected thinking value", id)
			continue
		}
		thinkingCap := e["capabilities"].(map[string]any)["thinking"].(map[string]any)
		if thinkingCap["supported"] != want {
			t.Errorf("%s: thinking.supported = %v, want %v", id, thinkingCap["supported"], want)
		}
		types := thinkingCap["types"].(map[string]any)
		if types["enabled"].(map[string]any)["supported"] != want {
			t.Errorf("%s: thinking.types.enabled disagrees with thinking.supported", id)
		}
	}
}

// Anthropic's own clients read max_tokens; the OpenAI-style hint must not
// disagree with it.
func TestModelsMaxTokensMatchesTheOutputHint(t *testing.T) {
	for _, e := range entries(t, rawModels(t)) {
		if e["max_tokens"] != e["max_output_tokens"] {
			t.Errorf("%v: max_tokens=%v but max_output_tokens=%v", e["id"], e["max_tokens"], e["max_output_tokens"])
		}
	}
}

// Claude Code only adds a gateway model to its picker when the id begins with
// claude or anthropic, so every Claude tone has to keep such an id.
func TestModelsKeepClaudeIdsDiscoverable(t *testing.T) {
	var claude []string
	for _, cfg := range models.ModelRegistry {
		if cfg.OwnerOrDefault() == models.OwnerAnthropic && !slices.Contains(claude, cfg.OpenAIID) {
			claude = append(claude, cfg.OpenAIID)
		}
	}
	if len(claude) == 0 {
		t.Fatal("no Anthropic-owned model is registered, so this test proves nothing")
	}
	for _, id := range claude {
		if !strings.HasPrefix(id, "claude") && !strings.HasPrefix(id, "anthropic") {
			t.Errorf("id %q would be dropped from the Claude Code model picker", id)
		}
	}
}

// The catalog advertises effort support per advertised id, but the routing used
// to append "-reasoning" to whatever name the request carried. A caller that
// took its id from this very catalog therefore reached no variant and ran on
// the non-reasoning tone while the catalog said otherwise. The advertisement
// and the routing have to agree on every id.
func TestAdvertisedEffortActuallyRoutes(t *testing.T) {
	for _, e := range entries(t, rawModels(t)) {
		id := e["id"].(string)
		supported := e["capabilities"].(map[string]any)["effort"].(map[string]any)["supported"].(bool)

		cfg, ok := models.FindModel(id)
		if !ok {
			t.Errorf("%s is advertised but does not resolve", id)
			continue
		}
		// An id whose own key already names the reasoning variant reaches a
		// reasoning tone without being rerouted, so both routes count.
		alreadyVariant := slices.ContainsFunc(models.RegistryKeysFor(id), func(k string) bool {
			return strings.HasSuffix(k, "-reasoning")
		})
		reaches := alreadyVariant || applyReasoningEffort(id, cfg, true) != cfg
		if reaches != supported {
			t.Errorf("%s: advertises effort=%v but reaches a reasoning tone=%v", id, supported, reaches)
		}
	}
}
