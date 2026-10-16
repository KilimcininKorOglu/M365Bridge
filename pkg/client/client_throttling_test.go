package client

import (
	"strings"
	"testing"
)

// liveThrottlingFrame mirrors the object the backend sends on type 1 update
// frames, including the third counter beyond the two documented ones.
func liveThrottlingFrame() map[string]any {
	return map[string]any{
		"numUserMessagesInConversation":               float64(7),
		"maxNumUserMessagesInConversation":            float64(600),
		"numLongDocSummaryUserMessagesInConversation": float64(0),
	}
}

func TestParseThrottlingReadsCountersAndKeepsUnknownKeys(t *testing.T) {
	info := parseThrottling(liveThrottlingFrame())
	if info == nil {
		t.Fatal("parseThrottling returned nil for a populated frame")
	}
	if info.NumUserMessages == nil || *info.NumUserMessages != 7 {
		t.Fatalf("NumUserMessages = %v, want 7", info.NumUserMessages)
	}
	if info.MaxNumUserMessages == nil || *info.MaxNumUserMessages != 600 {
		t.Fatalf("MaxNumUserMessages = %v, want 600", info.MaxNumUserMessages)
	}
	// An unrecognized counter must stay observable instead of being dropped.
	if got, ok := info.Extra["numLongDocSummaryUserMessagesInConversation"]; !ok || got != float64(0) {
		t.Fatalf("Extra = %v, want the long-doc counter preserved", info.Extra)
	}
}

func TestParseThrottlingIgnoresEmptyAndNonNumericCounters(t *testing.T) {
	if info := parseThrottling(nil); info != nil {
		t.Fatalf("parseThrottling(nil) = %v, want nil", info)
	}
	// A bool must not be accepted as a counter; it belongs in Extra so the
	// unexpected shape stays visible.
	info := parseThrottling(map[string]any{"numUserMessagesInConversation": true})
	if info == nil {
		t.Fatal("parseThrottling returned nil for a non-empty frame")
	}
	if info.NumUserMessages != nil {
		t.Fatalf("NumUserMessages = %v, want nil for a non-numeric value", *info.NumUserMessages)
	}
	if _, ok := info.Extra["numUserMessagesInConversation"]; !ok {
		t.Fatalf("Extra = %v, want the non-numeric counter preserved", info.Extra)
	}
}

func TestThrottlingExhaustedAndSummary(t *testing.T) {
	full := parseThrottling(map[string]any{
		"numUserMessagesInConversation":    float64(600),
		"maxNumUserMessagesInConversation": float64(600),
	})
	if !full.Exhausted() {
		t.Fatal("Exhausted() = false at the message ceiling, want true")
	}
	if got, want := full.Summary(), "used=600 max=600 headroom=0"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}

	partial := parseThrottling(liveThrottlingFrame())
	if partial.Exhausted() {
		t.Fatal("Exhausted() = true below the ceiling, want false")
	}

	// A nil receiver is the normal state before any turn reports counters.
	var missing *ThrottlingInfo
	if missing.Exhausted() {
		t.Fatal("nil ThrottlingInfo reported exhausted")
	}
	if got := missing.Summary(); got != "" {
		t.Fatalf("nil Summary() = %q, want empty", got)
	}
}

func assertAllowance(t *testing.T, a ModelAllowance, model string, wantDaily, wantWeekly int) {
	t.Helper()
	if a.Daily == nil || *a.Daily != wantDaily {
		t.Fatalf("%s daily = %v, want %d", model, a.Daily, wantDaily)
	}
	if a.Weekly == nil || *a.Weekly != wantWeekly {
		t.Fatalf("%s weekly = %v, want %d", model, a.Weekly, wantWeekly)
	}
}

func TestParseThrottlingWithMetering(t *testing.T) {
	frame := map[string]any{
		"numUserMessagesInConversation":    float64(3),
		"maxNumUserMessagesInConversation": float64(600),
		"metering": map[string]any{
			"ClaudeSonnet55QueryDaily":  map[string]any{"remainingAllowance": float64(78)},
			"ClaudeSonnet55QueryWeekly": map[string]any{"remainingAllowance": float64(148)},
			"ClaudeOpusQueryDaily":      map[string]any{"remainingAllowance": float64(39)},
			"ClaudeOpusQuery75":         map[string]any{"remainingAllowance": float64(74)},
			"GPT61SolQueryDaily":        map[string]any{"remainingAllowance": float64(38)},
			"GPT61SolQueryWeekly":       map[string]any{"remainingAllowance": float64(73)},
			"DeepResearch":              float64(100),
		},
	}

	info := parseThrottling(frame)
	if info == nil {
		t.Fatal("parseThrottling returned nil")
	}
	if info.RawMetering["DeepResearch"] != 100 {
		t.Fatalf("RawMetering[DeepResearch] = %d, want 100", info.RawMetering["DeepResearch"])
	}

	assertAllowance(t, info.Allowances["claude-sonnet-5.5"], "claude-sonnet-5.5", 78, 148)
	assertAllowance(t, info.Allowances["claude-opus"], "claude-opus", 39, 74)
	assertAllowance(t, info.Allowances["gpt-6.1-sol"], "gpt-6.1-sol", 38, 73)

	summary := info.Summary()
	want := "allowance=[claude-opus(39d/74w) claude-sonnet-5.5(78d/148w) gpt-6.1-sol(38d/73w)]"
	if !strings.Contains(summary, want) {
		t.Fatalf("Summary %q does not contain %q", summary, want)
	}
}
