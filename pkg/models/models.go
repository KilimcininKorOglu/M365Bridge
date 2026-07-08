// Package models provides constants, data structures, and configuration for M365 Copilot integration.
// It includes model definitions, environment configuration, and message type mappings.
package models

import (
	"os"
	"strings"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
)

// Version is the application version, shared across all binaries.
// Overridable at build time via ldflags: -X github.com/KilimcininKorOglu/M365Bridge/pkg/models.Version=x.y.z
var Version = "1.2.2"

const (
	// DefaultClientID is the default Microsoft 365 Copilot client ID.
	DefaultClientID = "4765445b-32c6-49b0-83e6-1d93765276ca"

	// DefaultScope is the OAuth2 scope for M365 Copilot access.
	DefaultScope = "https://substrate.office.com/sydney/.default openid profile offline_access"
)

// ModelConfig represents the configuration for a specific model variant.
type ModelConfig struct {
	Tone     string // The tone/style parameter sent to the backend
	Override string // Optional GPT model override identifier
	OpenAIID string // OpenAI-compatible model identifier
}

// ModelRegistry maps model keys to their configurations.
var ModelRegistry = map[string]ModelConfig{
	"auto": {
		Tone:     "Magic",
		Override: "",
		OpenAIID: "gpt-4-auto",
	},
	"quick": {
		Tone:     "Chat",
		Override: "",
		OpenAIID: "gpt-4-quick",
	},
	"reasoning": {
		Tone:     "Magic",
		Override: "",
		OpenAIID: "gpt-4-reasoning",
	},
	"gpt5.5": {
		Tone:     "Gpt_5_5_Chat",
		Override: "",
		OpenAIID: "gpt-5.5",
	},
	"gpt5.5-reasoning": {
		Tone:     "Gpt_5_5_Reasoning",
		Override: "",
		OpenAIID: "gpt-5.5-reasoning",
	},
	// Claude — real Anthropic models (verified via tone test, July 2026)
	"claude": {
		Tone:     "Claude_Sonnet",
		Override: "",
		OpenAIID: "claude-sonnet-4.6",
	},
	"claude-sonnet": {
		Tone:     "Claude_Sonnet",
		Override: "",
		OpenAIID: "claude-sonnet-4.6",
	},
	"claude-opus": {
		Tone:     "Claude_Opus",
		Override: "",
		OpenAIID: "claude-opus-4.6",
	},
	"claude-sonnet-4-20250514": {
		Tone:     "Claude_Sonnet",
		Override: "",
		OpenAIID: "claude-sonnet-4.6",
	},
}

// ToolMessageType maps WebSocket message types to tool function names.
var ToolMessageType = map[string]string{
	"InternalSearchQuery": "search",
	"GeneratedCode":       "code_interpreter",
	"TriggerPlugin":       "trigger_plugin",
	"InvokeAction":        "invoke_action",
}

// Config holds environment-based configuration.
type Config struct {
	TenantID        string
	UserOID         string
	ClientID        string
	Scope           string
	APIKeys         []string
}

// LoadConfig loads configuration from .env file and environment variables.
// Returns configuration with defaults for missing values.
func LoadConfig() *Config {
	// Load .env file if it exists
	loadDotEnv()

	cfg := &Config{
		TenantID:        os.Getenv("M365_TENANT_ID"),
		UserOID:         os.Getenv("M365_USER_OID"),
		ClientID:        getEnvWithDefault("M365_CLIENT_ID", DefaultClientID),
		Scope:           DefaultScope,
		APIKeys:         parseAPIKeys(os.Getenv("M365_API_KEYS"), os.Getenv("M365_API_KEY")),
	}

	logging.Infof("LoadConfig: tenantID=%s userOID=%s clientID=%s apiKeys=%d", cfg.TenantID, cfg.UserOID, cfg.ClientID[:min(8, len(cfg.ClientID))]+"...", len(cfg.APIKeys))
	return cfg
}

// parseAPIKeys builds the API key list from M365_API_KEYS (comma-separated)
// and M365_API_KEY (singular, for backward compatibility).
// M365_API_KEYS takes precedence; M365_API_KEY is used only if M365_API_KEYS is empty.
func parseAPIKeys(keysCSV, singleKey string) []string {
	if keysCSV != "" {
		var keys []string
		for _, k := range strings.Split(keysCSV, ",") {
			k = strings.TrimSpace(k)
			if k != "" {
				keys = append(keys, k)
			}
		}
		return keys
	}
	if singleKey != "" {
		return []string{strings.TrimSpace(singleKey)}
	}
	return nil
}

// loadDotEnv reads a .env file and sets environment variables.
// Checks data/.env first, then falls back to .env in the current directory.
// Lines starting with # are comments. Format: KEY=VALUE
func loadDotEnv() {
	data, err := os.ReadFile("data/.env")
	if err != nil {
		data, err = os.ReadFile(".env")
		if err != nil {
			return
		}
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// Only set if not already in environment (env vars take precedence)
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}

// LookupModel finds a model configuration by key or OpenAI ID.
// Returns the "auto" model configuration if not found.
func LookupModel(key string) ModelConfig {
	if cfg, ok := ModelRegistry[key]; ok {
		return cfg
	}
	// Try to find by OpenAI ID
	for _, cfg := range ModelRegistry {
		if cfg.OpenAIID == key {
			return cfg
		}
	}
	return ModelRegistry["auto"]
}

// getEnvWithDefault returns an environment variable value or a default fallback.
func getEnvWithDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvBool returns true for "true", "1", "yes", "on" (case-insensitive).
func getEnvBool(key string, defaultValue bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch value {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return defaultValue
	}
}
