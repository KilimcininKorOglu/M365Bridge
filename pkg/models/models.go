// Package models provides constants, data structures, and configuration for M365 Copilot integration.
// It includes model definitions, environment configuration, and message type mappings.
package models

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
)

// Version is the application version, shared across all binaries.
// Overridable at build time via ldflags: -X github.com/KilimcininKorOglu/M365Bridge/pkg/models.Version=x.y.z
var Version = "1.5.0"

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
	Owner    string // Who built the model behind the tone; defaults to OwnerMicrosoft
	// DisplayName is the human-readable label Anthropic's Models API requires.
	// Claude Code labels its gateway model picker with it, so an id here would
	// show the caller a slug where a name belongs.
	DisplayName string
	// Thinking records whether the tone was measured emitting reasoning.
	//
	// The advertised capability used to be derived from the tone name carrying
	// "Reasoning", which was wrong in both directions: Claude_Opus reasons and
	// does not carry it, Gpt_5_6_Reasoning carries it and does not reason. A
	// name is not evidence, so each entry states what the tone was observed
	// doing.
	Thinking bool
}

// DisplayNameOrDefault returns the human-readable model name, falling back to
// the advertised id so a new registry entry never publishes an empty label.
func (c ModelConfig) DisplayNameOrDefault() string {
	if c.DisplayName == "" {
		return c.OpenAIID
	}
	return c.DisplayName
}

// Model owners advertised through /v1/models. The Claude tones reach real
// Anthropic models through Microsoft 365, and a client that routes by vendor
// needs to see that rather than a single blanket owner.
const (
	OwnerMicrosoft = "microsoft-365"
	OwnerAnthropic = "anthropic-via-microsoft-365"
)

// Owner returns the model owner, falling back to Microsoft for tones that do
// not name one.
func (c ModelConfig) OwnerOrDefault() string {
	if c.Owner == "" {
		return OwnerMicrosoft
	}
	return c.Owner
}

// ReasoningEffortPreset is one advertised reasoning effort value. Codex reads
// the description to label the choice in its own UI.
type ReasoningEffortPreset struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

// ReasoningEffortPresets lists the reasoning effort values the Responses API
// accepts, from cheapest to most thorough.
var ReasoningEffortPresets = []ReasoningEffortPreset{
	{Effort: "none", Description: "Disable additional reasoning."},
	{Effort: "minimal", Description: "Fast responses with minimal reasoning."},
	{Effort: "low", Description: "Fast responses with lighter reasoning."},
	{Effort: "medium", Description: "Balances speed and reasoning depth for everyday tasks."},
	{Effort: "high", Description: "Greater reasoning depth for complex problems."},
	{Effort: "xhigh", Description: "Extra high reasoning depth for complex problems."},
	{Effort: "max", Description: "Maximum reasoning depth for the most complex problems."},
}

// ReasoningEffortNames lists the accepted effort values without their
// descriptions, for error messages and validation.
func ReasoningEffortNames() []string {
	names := make([]string, len(ReasoningEffortPresets))
	for i, preset := range ReasoningEffortPresets {
		names[i] = preset.Effort
	}
	return names
}

// ModelRegistry maps model keys to their configurations.
var ModelRegistry = map[string]ModelConfig{
	"auto": {
		Tone:        "Magic",
		Override:    "",
		OpenAIID:    "gpt-4-auto",
		DisplayName: "GPT Auto",
	},
	"quick": {
		Tone:        "Chat",
		Override:    "",
		OpenAIID:    "gpt-4-quick",
		DisplayName: "GPT Quick",
	},
	// The Magic tone answers without reasoning, so this entry used to be
	// indistinguishable from "auto". It routes to the reasoning tone measured
	// to emit chain-of-thought summaries on every turn instead.
	"reasoning": {
		Tone:        "Gpt_5_2_Reasoning",
		Override:    "",
		OpenAIID:    "gpt-4-reasoning",
		DisplayName: "GPT Reasoning",
		Thinking:    true,
	},
	"gpt5.2-reasoning": {
		Tone:        "Gpt_5_2_Reasoning",
		Override:    "",
		OpenAIID:    "gpt-5.2-reasoning",
		DisplayName: "GPT-5.2 Reasoning",
		Thinking:    true,
	},
	"gpt5.4-reasoning": {
		Tone:        "Gpt_5_4_Reasoning",
		Override:    "",
		OpenAIID:    "gpt-5.4-reasoning",
		DisplayName: "GPT-5.4 Reasoning",
		Thinking:    true,
	},
	"gpt5.2": {
		Tone:        "Gpt_5_2_Chat",
		Override:    "",
		OpenAIID:    "gpt-5.2",
		DisplayName: "GPT-5.2",
	},
	"gpt5.3": {
		Tone:        "Gpt_5_3_Chat",
		Override:    "",
		OpenAIID:    "gpt-5.3",
		DisplayName: "GPT-5.3",
	},
	"gpt5.4": {
		Tone:        "Gpt_5_4_Chat",
		Override:    "",
		OpenAIID:    "gpt-5.4",
		DisplayName: "GPT-5.4",
	},
	"gpt5.5": {
		Tone:        "Gpt_5_5_Chat",
		Override:    "",
		OpenAIID:    "gpt-5.5",
		DisplayName: "GPT-5.5",
	},
	"gpt5.5-reasoning": {
		Tone:        "Gpt_5_5_Reasoning",
		Override:    "",
		OpenAIID:    "gpt-5.5-reasoning",
		DisplayName: "GPT-5.5 Reasoning",
		Thinking:    true,
	},
	"gpt5.6-reasoning": {
		Tone:        "Gpt_5_6_Reasoning",
		Override:    "",
		OpenAIID:    "gpt-5.6-reasoning",
		DisplayName: "GPT-5.6 Reasoning",
		Thinking:    true,
	},
	// Claude — real Anthropic models (verified via tone test, July 2026)
	"claude": {
		Tone:        "Claude_Sonnet",
		Override:    "",
		OpenAIID:    "claude-sonnet-4.6",
		DisplayName: "Claude Sonnet 4.6",
		Owner:       OwnerAnthropic,
	},
	"claude-sonnet": {
		Tone:        "Claude_Sonnet",
		Override:    "",
		OpenAIID:    "claude-sonnet-4.6",
		DisplayName: "Claude Sonnet 4.6",
		Owner:       OwnerAnthropic,
	},
	"claude-opus": {
		Tone:        "Claude_Opus",
		Override:    "",
		OpenAIID:    "claude-opus-4.6",
		DisplayName: "Claude Opus 4.6",
		Thinking:    true,
		Owner:       OwnerAnthropic,
	},
	"claude-sonnet-4-20250514": {
		Tone:        "Claude_Sonnet",
		Override:    "",
		OpenAIID:    "claude-sonnet-4.6",
		DisplayName: "Claude Sonnet 4.6",
		Owner:       OwnerAnthropic,
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
	TenantID              string
	UserOID               string
	ClientID              string
	Scope                 string
	APIKeys               []string
	EnableCodeTools       bool
	AutoExposeTools       bool
	WorkspaceDir          string
	CodeToolTimeout       time.Duration
	CodeToolMaxOutput     int64
	CodeToolMaxReadBytes  int64
	CodeToolMaxIterations int
	ContextWindowTokens   int
	MaxOutputTokens       int
	// EnableWebSearch declares the BingWebSearch built-in on every request so
	// the backend can ground answers in live search results.
	EnableWebSearch bool
	// MaxToolRounds caps the tool rounds a client may drive within one user
	// turn. The server keeps no state across such a loop, so without a cap a
	// client that never stops calling tools keeps the upstream busy forever.
	MaxToolRounds int
	// ImageHostAllowlist names the hosts a generated-image URL may point at.
	// Downloads send an access token, so an unlisted host must never be
	// contacted. A leading dot matches that domain and its subdomains.
	ImageHostAllowlist []string
	// EnableWebUI serves the browser interface and records a transcript for
	// every session turn. The backend keeps no message history of its own, so
	// the interface cannot redraw a conversation without that record. Turning
	// the interface off also stops the recording, because a gateway that only
	// proxies should not write message content to disk.
	EnableWebUI bool
	// WebUIPassword gates the browser interface. An empty value opens the
	// interface to anyone who can reach it.
	//
	// The interface holds the password in a cookie and sends it in the same
	// header an API client sends its key, so the gateway accepts it as one more
	// credential rather than through a session of its own. That keeps every
	// credential on a header, where a cross-site form cannot carry it.
	//
	// This is a separate switch from APIKeys. A deployment that sets a password
	// and no API key gates the interface and leaves the API open, which is what
	// an empty key list means everywhere else.
	WebUIPassword string
}

const (
	// DefaultMaxToolRounds is the tool round cap for one user turn when
	// M365_MAX_TOOL_ROUNDS is unset. An agent session rarely needs more than a
	// few dozen rounds to answer a single request.
	DefaultMaxToolRounds = 32
	// MaxToolRoundsCeiling caps what M365_MAX_TOOL_ROUNDS can raise the limit
	// to, so a misconfigured value cannot remove the protection outright.
	MaxToolRoundsCeiling = 512
)

// Defaults for every remaining configurable value. They are named constants
// rather than literals inside LoadConfig because the usage text prints them:
// a literal in both places would let the documented default drift away from
// the one the binary applies.
const (
	// DefaultContextWindowTokens is the context window /v1/models advertises.
	DefaultContextWindowTokens = 1_000_000
	// DefaultMaxOutputTokens is the output budget /v1/models advertises.
	DefaultMaxOutputTokens = 1_000_000
	// DefaultWorkspaceDir is the directory the built-in coding tools are
	// confined to.
	DefaultWorkspaceDir = "."
	// DefaultCodeToolTimeout is the wall clock a single tool command may take.
	DefaultCodeToolTimeout = 30 * time.Second
	// DefaultCodeToolMaxOutput is the number of bytes kept from one command.
	DefaultCodeToolMaxOutput = 1 << 20
	// DefaultCodeToolMaxReadBytes is the number of bytes read from one file.
	DefaultCodeToolMaxReadBytes = 1 << 20
	// DefaultCodeToolMaxIterations is the tool round cap inside one request.
	DefaultCodeToolMaxIterations = 10
)

// DefaultImageHostAllowlist holds the Microsoft hosts that serve generated
// images. The designerapp access token is issued for this resource, so it must
// not be sent anywhere else.
var DefaultImageHostAllowlist = []string{".officeapps.live.com"}

// LoadConfig loads configuration from .env file and environment variables.
// Returns configuration with defaults for missing values.
func LoadConfig() *Config {
	// Load .env file if it exists
	loadDotEnv()

	cfg := &Config{
		TenantID:              os.Getenv("M365_TENANT_ID"),
		UserOID:               os.Getenv("M365_USER_OID"),
		ClientID:              getEnvWithDefault("M365_CLIENT_ID", DefaultClientID),
		Scope:                 DefaultScope,
		APIKeys:               parseAPIKeys(os.Getenv("M365_API_KEYS"), os.Getenv("M365_API_KEY")),
		EnableCodeTools:       getEnvBool("M365_ENABLE_CODE_TOOLS", false),
		AutoExposeTools:       getEnvBool("M365_AUTO_EXPOSE_TOOLS", false),
		WorkspaceDir:          getEnvWithDefault("M365_WORKSPACE_DIR", DefaultWorkspaceDir),
		CodeToolTimeout:       getEnvDuration("M365_CODE_TOOL_TIMEOUT", DefaultCodeToolTimeout),
		CodeToolMaxOutput:     getEnvInt64("M365_CODE_TOOL_MAX_OUTPUT", DefaultCodeToolMaxOutput),
		CodeToolMaxReadBytes:  getEnvInt64("M365_CODE_TOOL_MAX_READ_BYTES", DefaultCodeToolMaxReadBytes),
		CodeToolMaxIterations: getEnvInt("M365_CODE_TOOL_MAX_ITERATIONS", DefaultCodeToolMaxIterations),
		ImageHostAllowlist:    getEnvHostList("M365_IMAGE_HOST_ALLOWLIST", DefaultImageHostAllowlist),
		ContextWindowTokens:   getEnvInt("M365_CONTEXT_WINDOW", DefaultContextWindowTokens),
		MaxOutputTokens:       getEnvInt("M365_MAX_OUTPUT_TOKENS", DefaultMaxOutputTokens),
		MaxToolRounds:         min(getEnvInt("M365_MAX_TOOL_ROUNDS", DefaultMaxToolRounds), MaxToolRoundsCeiling),
		EnableWebSearch:       getEnvBool("M365_ENABLE_WEB_SEARCH", true),
		EnableWebUI:           getEnvBool("M365_ENABLE_WEB_UI", true),
		WebUIPassword:         strings.TrimSpace(os.Getenv("M365_WEB_UI_PASSWORD")),
	}

	// The password itself never reaches a log line; only whether one is set.
	logging.Infof("LoadConfig: tenantID=%s userOID=%s clientID=%s apiKeys=%d webUIPassword=%t",
		cfg.TenantID, cfg.UserOID, cfg.ClientID[:min(8, len(cfg.ClientID))]+"...",
		len(cfg.APIKeys), cfg.WebUIPassword != "")
	return cfg
}

// parseAPIKeys builds the API key list from M365_API_KEYS (comma-separated)
// and M365_API_KEY (singular, for backward compatibility).
// M365_API_KEYS takes precedence; M365_API_KEY is used only if M365_API_KEYS is empty.
func parseAPIKeys(keysCSV, singleKey string) []string {
	if keysCSV != "" {
		var keys []string
		for k := range strings.SplitSeq(keysCSV, ",") {
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

	for line := range strings.SplitSeq(string(data), "\n") {
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
			_ = os.Setenv(key, value)
		}
	}
}

// RegistryKeysFor reports every registry key a request may reach by name: the
// key itself when the name is one, otherwise every key sharing the advertised
// OpenAI ID.
//
// Reasoning routing needs this because the variant is found by appending
// "-reasoning" to a key. A caller that used the id from /v1/models sent
// "gpt-5.5", the append produced "gpt-5.5-reasoning", no key matched, and the
// request silently ran on the non-reasoning tone while the catalog advertised
// effort support for that same id.
func RegistryKeysFor(name string) []string {
	if _, ok := ModelRegistry[name]; ok {
		return []string{name}
	}
	var keys []string
	for key, cfg := range ModelRegistry {
		if cfg.OpenAIID == name {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// FindModel finds a model configuration by registry key or by advertised
// OpenAI ID, and reports whether the name is one this service serves.
//
// It replaced a lookup that answered an unknown name with the "auto" entry.
// That silently served the Magic tone to a caller who had asked for something
// else, so a model removed from the registry kept answering and a typo looked
// like a working model. Both OpenAI and Anthropic answer an unknown model with
// 404 instead, which is what a client checks for.
func FindModel(key string) (ModelConfig, bool) {
	if cfg, ok := ModelRegistry[key]; ok {
		return cfg, true
	}
	for _, cfg := range ModelRegistry {
		if cfg.OpenAIID == key {
			return cfg, true
		}
	}
	return ModelConfig{}, false
}

// getEnvWithDefault returns an environment variable value or a default fallback.
func getEnvWithDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvHostList reads a comma-separated host list, lowercased and trimmed.
// An unset or empty value keeps the supplied default.
func getEnvHostList(key string, defaultValue []string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return defaultValue
	}
	var hosts []string
	for entry := range strings.SplitSeq(raw, ",") {
		if host := strings.ToLower(strings.TrimSpace(entry)); host != "" {
			hosts = append(hosts, host)
		}
	}
	if len(hosts) == 0 {
		return defaultValue
	}
	return hosts
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

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return defaultValue
	}
	return parsed
}

func getEnvInt64(key string, defaultValue int64) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return defaultValue
	}
	return parsed
}

func getEnvInt(key string, defaultValue int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return defaultValue
	}
	return parsed
}
