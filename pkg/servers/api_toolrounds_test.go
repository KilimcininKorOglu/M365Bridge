package servers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
)

// openAIToolRounds builds an OpenAI-shaped history with the given number of
// completed tool rounds, all inside one user turn.
func openAIToolRounds(t *testing.T, rounds int) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`[{"role":"user","content":"fix the build"}`)
	for i := range rounds {
		id := fmt.Sprintf("call_%d", i)
		sb.WriteString(`,{"role":"assistant","content":null,"tool_calls":[{"id":"`)
		sb.WriteString(id)
		sb.WriteString(`","type":"function","function":{"name":"run_tests","arguments":"{}"}}]}`)
		sb.WriteString(`,{"role":"tool","tool_call_id":"`)
		sb.WriteString(id)
		sb.WriteString(`","content":"ok"}`)
	}
	sb.WriteString("]")
	return sb.String()
}

func TestBuildToolLedgerCountsOnlyTheCurrentTurn(t *testing.T) {
	messages := decodeMessages(t, `[
		{"role":"user","content":"first question"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"contents"},
		{"role":"assistant","content":"here it is"},
		{"role":"user","content":"second question"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_2","type":"function","function":{"name":"run_tests","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_2","content":"ok"}
	]`)

	ledger := buildToolLedger(messages)
	if ledger.Rounds != 1 {
		t.Fatalf("rounds = %d, want only the current turn counted", ledger.Rounds)
	}
	if len(ledger.Completed) != 1 || ledger.Completed[0].Name != "run_tests" {
		t.Fatalf("completed = %#v, want only the current turn's call", ledger.Completed)
	}
}

func TestBuildToolLedgerKeepsAnthropicResultTurnsInsideTheSameTurn(t *testing.T) {
	// Anthropic delivers every tool result as a user message. Treating those as
	// turn boundaries would reset the round count on every round.
	messages := decodeMessages(t, `[
		{"role":"user","content":"fix the build"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"run_tests","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"exit code 1"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"run_tests","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"exit code 1"}]}
	]`)

	ledger := buildToolLedger(messages)
	if ledger.Rounds != 2 {
		t.Fatalf("rounds = %d, want both Anthropic rounds counted", ledger.Rounds)
	}
	if !ledger.RepeatedFailure {
		t.Fatal("the same failing call twice was not recognized")
	}
}

func TestChatCompletionsRejectsARunawayToolLoop(t *testing.T) {
	api := NewAPIServer(&models.Config{MaxToolRounds: 3}, nil)

	body := `{"model":"gpt5.5","messages":` + openAIToolRounds(t, 5) + `}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleChatCompletions(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	var decoded struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if decoded.Error.Code != toolRoundLimitCode {
		t.Fatalf("error code = %q, want %q", decoded.Error.Code, toolRoundLimitCode)
	}
	if decoded.Error.Type != "invalid_request_error" {
		t.Fatalf("error type = %q, want the OpenAI category", decoded.Error.Type)
	}
	if !strings.Contains(decoded.Error.Message, "5 tool rounds") {
		t.Fatalf("message %q does not report the round count", decoded.Error.Message)
	}
}

func TestToolRoundLimitLeavesALoopUnderTheCapAlone(t *testing.T) {
	// Driving the handler here would reach the absent upstream, so the check
	// itself is exercised instead of the whole request path.
	api := NewAPIServer(&models.Config{MaxToolRounds: 8}, nil)
	ledger := buildToolLedger(decodeMessages(t, openAIToolRounds(t, 5)))

	if ledger.Rounds != 5 {
		t.Fatalf("rounds = %d, want 5", ledger.Rounds)
	}
	if api.exceededToolRoundLimit(ledger) {
		t.Fatal("a loop under the cap was refused")
	}
}

func TestToolRoundLimitFallsBackToTheDefaultWhenUnset(t *testing.T) {
	api := NewAPIServer(&models.Config{}, nil)
	ledger := buildToolLedger(decodeMessages(t, openAIToolRounds(t, models.DefaultMaxToolRounds+1)))
	if !api.exceededToolRoundLimit(ledger) {
		t.Fatalf("a zero MaxToolRounds removed the limit at %d rounds", ledger.Rounds)
	}
}

func TestMaxToolRoundsCeilingCapsTheConfiguredValue(t *testing.T) {
	t.Setenv("M365_MAX_TOOL_ROUNDS", "100000")
	cfg := models.LoadConfig()
	if cfg.MaxToolRounds != models.MaxToolRoundsCeiling {
		t.Fatalf("MaxToolRounds = %d, want the ceiling %d", cfg.MaxToolRounds, models.MaxToolRoundsCeiling)
	}
}
