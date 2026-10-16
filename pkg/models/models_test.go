package models

import (
	"testing"
)

func TestEffectiveScenarioAndLicenseElevatesPaidModels(t *testing.T) {
	paidModels := []struct {
		tone  string
		model string
	}{
		{"Gpt_6_Reasoning", "gpt-6"},
		{"Gpt_6_Reasoning", "gpt-6-reasoning"},
		{"Claude_Sonnet_5", "claude-sonnet-5"},
		{"Claude_Opus", "claude-opus-5.5"},
		{"", "claude-opus"},
		{"Gpt_6_Reasoning", ""},
		{"Claude_Sonnet_5", ""},
	}

	for _, tc := range paidModels {
		scenario, license := EffectiveScenarioAndLicense(tc.tone, tc.model)
		if scenario != "OfficeWebPaidCopilot" {
			t.Errorf("EffectiveScenarioAndLicense(%q, %q) scenario = %q, want OfficeWebPaidCopilot", tc.tone, tc.model, scenario)
		}
		if license != "Premium" {
			t.Errorf("EffectiveScenarioAndLicense(%q, %q) license = %q, want Premium", tc.tone, tc.model, license)
		}
	}
}

func TestEffectiveScenarioAndLicenseStandardModels(t *testing.T) {
	standardModels := []struct {
		tone  string
		model string
	}{
		{"Gpt_61_Sol_Reasoning", "gpt-6.1-sol"},
		{"Gpt_6_Sol_Reasoning", "gpt-6-sol"},
		{"Gpt_5_5_Reasoning", "gpt-5.5"},
		{"Magic", "auto"},
	}

	for _, tc := range standardModels {
		scenario, license := EffectiveScenarioAndLicense(tc.tone, tc.model)
		if scenario != "OfficeWebIncludedCopilot" {
			t.Errorf("EffectiveScenarioAndLicense(%q, %q) scenario = %q, want OfficeWebIncludedCopilot", tc.tone, tc.model, scenario)
		}
		if license != "Starter" {
			t.Errorf("EffectiveScenarioAndLicense(%q, %q) license = %q, want Starter", tc.tone, tc.model, license)
		}
	}
}

func TestEffectiveScenarioAndLicenseEnvOverrides(t *testing.T) {
	t.Setenv("M365_SCENARIO", "CustomScenario")
	t.Setenv("M365_LICENSE_TYPE", "CustomLicense")

	scenario, license := EffectiveScenarioAndLicense("Magic", "auto")
	if scenario != "CustomScenario" || license != "CustomLicense" {
		t.Errorf("got (%q, %q), want (CustomScenario, CustomLicense)", scenario, license)
	}
}

func TestToneToPublicModel(t *testing.T) {
	tests := []struct {
		tone       string
		wantID     string
		wantName   string
		wantOk     bool
		wantReason bool
	}{
		{"Gpt_61_Sol_Reasoning", "gpt-6.1-sol-reasoning", "GPT-6.1 Sol Reasoning", true, true},
		{"Gpt_7_Chat", "gpt-7", "GPT-7", true, false},
		{"Gpt_7_Reasoning", "gpt-7-reasoning", "GPT-7 Reasoning", true, true},
		{"Claude_Sonnet_6", "claude-sonnet-6", "Claude Sonnet 6", true, true},
		{"Grok_5", "grok-5", "Grok 5", true, false},
		{"Magic", "", "", false, false},
		{"Chat", "", "", false, false},
		{"Gpt_6_Auto", "", "", false, false},
	}

	for _, tc := range tests {
		cfg, ok := ToneToPublicModel(tc.tone)
		if ok != tc.wantOk {
			t.Errorf("ToneToPublicModel(%q) ok = %v, want %v", tc.tone, ok, tc.wantOk)
			continue
		}
		if !ok {
			continue
		}
		if cfg.OpenAIID != tc.wantID {
			t.Errorf("ToneToPublicModel(%q) ID = %q, want %q", tc.tone, cfg.OpenAIID, tc.wantID)
		}
		if cfg.DisplayName != tc.wantName {
			t.Errorf("ToneToPublicModel(%q) DisplayName = %q, want %q", tc.tone, cfg.DisplayName, tc.wantName)
		}
		if cfg.Thinking != tc.wantReason {
			t.Errorf("ToneToPublicModel(%q) Thinking = %v, want %v", tc.tone, cfg.Thinking, tc.wantReason)
		}
	}
}

func TestRegisterDynamicToneAndFindModel(t *testing.T) {
	spec, ok := RegisterDynamicTone("Gpt_72_Reasoning")
	if !ok {
		t.Fatalf("RegisterDynamicTone failed")
	}
	if spec.OpenAIID != "gpt-7.2-reasoning" {
		t.Fatalf("OpenAIID = %q, want gpt-7.2-reasoning", spec.OpenAIID)
	}

	found, ok := FindModel("gpt-7.2-reasoning")
	if !ok {
		t.Fatalf("FindModel failed to find dynamic model")
	}
	if found.Tone != "Gpt_72_Reasoning" {
		t.Fatalf("found Tone = %q, want Gpt_72_Reasoning", found.Tone)
	}

	// Test tone: prefix passthrough
	toneModel, ok := FindModel("tone:Gpt_8_Experimental")
	if !ok || toneModel.Tone != "Gpt_8_Experimental" {
		t.Fatalf("FindModel with tone: prefix failed: got (%v, %v)", toneModel, ok)
	}
}
