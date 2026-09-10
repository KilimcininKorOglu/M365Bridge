// Package servers provides HTTP API server for M365 Copilot.
// This file implements OpenAI-compatible and Anthropic-compatible API endpoints.
package servers

import (
	"bytes"
	"cmp"
	"context"
	// md5 is used only to derive cache file names and session ids, never as a
	// security primitive.
	"crypto/md5" // #nosec G501
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/atomicfile"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/client"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/codingtools"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/payload"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/toolcalling"
	"github.com/google/uuid"
	"github.com/pkoukk/tiktoken-go"
)

const (
	// contextCacheDir is the directory for context cache files.
	contextCacheDir = "data/cache"
	// contextCacheMaxSize is the maximum number of in-memory cache entries.
	contextCacheMaxSize = 256
	// serverReadHeaderTimeout bounds how long a client may take to send its
	// request headers.
	serverReadHeaderTimeout = 20 * time.Second
	// serverIdleTimeout bounds how long a kept-alive connection may sit unused.
	// It is long enough that an agent client reusing one connection between
	// turns does not have to redial.
	serverIdleTimeout = 120 * time.Second
)

// sessionKeyPrefix namespaces the conversation mapping inside the cache. It is
// the only key shape the cache holds.
const sessionKeyPrefix = "session:"

// sessionRecord is the stored form of one session-to-conversation mapping.
//
// The file name is an md5 of the cache key, so the session ID cannot be read
// back from disk. Storing it inside the file is what makes the mapping
// listable at all.
type sessionRecord struct {
	SessionID      string `json:"session_id"`
	ConversationID string `json:"conversation_id"`
	UpdatedAt      int64  `json:"updated_at"`
}

// ContextCache provides session-based conversation persistence across requests.
type ContextCache struct {
	cacheDir   string
	mu         sync.RWMutex
	mem        map[string]string
	order      []string
	writeFile  func(string, []byte, os.FileMode) error
	removeFile func(string) error
}

// NewContextCache creates a new context cache instance.
func NewContextCache(cacheDir string) *ContextCache {
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		logging.Errorf("context cache: cannot create %s: %v", cacheDir, err)
	}
	return &ContextCache{
		cacheDir:   cacheDir,
		mem:        make(map[string]string),
		writeFile:  atomicfile.Write,
		removeFile: os.Remove,
	}
}

// path returns the file path for a cache key.
//
// The digest turns a session key into a file name; it protects nothing and is
// never compared against an attacker-supplied value.
func (cc *ContextCache) path(key string) string {
	// #nosec G401
	hash := md5.Sum([]byte(key))
	safe := hex.EncodeToString(hash[:])
	return filepath.Join(cc.cacheDir, safe+".json")
}

// Get retrieves a conversation ID by session key.
func (cc *ContextCache) Get(key string) string {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if val, ok := cc.mem[key]; ok {
		return val
	}

	data, err := os.ReadFile(cc.path(key))
	if err != nil {
		return ""
	}
	convID, ok := decodeCacheEntry(data)
	if !ok {
		return ""
	}

	cc.mem[key] = convID
	cc.order = append(cc.order, key)
	cc.evict()

	return convID
}

// decodeCacheEntry reads either stored shape. Entries written before the
// record format are a bare JSON string, and they must keep working, because
// they are live session mappings.
func decodeCacheEntry(data []byte) (string, bool) {
	var record sessionRecord
	if err := json.Unmarshal(data, &record); err == nil && record.ConversationID != "" {
		return record.ConversationID, true
	}
	var convID string
	if err := json.Unmarshal(data, &convID); err == nil {
		return convID, true
	}
	return "", false
}

// Set stores a conversation ID by session key.
func (cc *ContextCache) Set(key, convID string) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.mem[key] = convID
	if idx := indexOf(cc.order, key); idx >= 0 {
		cc.order = append(cc.order[:idx], cc.order[idx+1:]...)
	}
	cc.order = append(cc.order, key)
	cc.evict()

	data, _ := json.Marshal(sessionRecord{
		SessionID:      strings.TrimPrefix(key, sessionKeyPrefix),
		ConversationID: convID,
		UpdatedAt:      time.Now().Unix(),
	})
	// The mapping stays in memory either way, but a failed write means the next
	// process starts a new conversation for this session with no record of why.
	if err := cc.writeFile(cc.path(key), data, 0600); err != nil {
		logging.Errorf("context cache: cannot persist the mapping for a session: %v", err)
	}
}

// List returns every mapping the cache dir holds, newest first, together with
// the number of entries written before the record format. Those carry no
// session ID and none can be recovered, because the file name is an md5, so
// they are reported as a count rather than dropped silently.
func (cc *ContextCache) List() ([]sessionRecord, int) {
	entries, err := os.ReadDir(cc.cacheDir)
	if err != nil {
		return nil, 0
	}

	var records []sessionRecord
	legacy := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(cc.cacheDir, entry.Name()))
		if err != nil {
			continue
		}
		var record sessionRecord
		if json.Unmarshal(data, &record) != nil || record.SessionID == "" {
			legacy++
			continue
		}
		records = append(records, record)
	}

	slices.SortFunc(records, func(a, b sessionRecord) int {
		return cmp.Compare(b.UpdatedAt, a.UpdatedAt)
	})
	return records, legacy
}

// Lookup returns the stored mapping for one session ID. An entry in the legacy
// format has no timestamp of its own, so the file modification time stands in.
func (cc *ContextCache) Lookup(sid string) (sessionRecord, bool) {
	convID := cc.Get(sessionKeyPrefix + sid)
	if convID == "" {
		return sessionRecord{}, false
	}
	record := sessionRecord{SessionID: sid, ConversationID: convID}

	data, err := os.ReadFile(cc.path(sessionKeyPrefix + sid))
	if err == nil {
		var stored sessionRecord
		if json.Unmarshal(data, &stored) == nil && stored.UpdatedAt > 0 {
			record.UpdatedAt = stored.UpdatedAt
		}
	}
	if record.UpdatedAt == 0 {
		if info, err := os.Stat(cc.path(sessionKeyPrefix + sid)); err == nil {
			record.UpdatedAt = info.ModTime().Unix()
		}
	}
	return record, true
}

// SessionsFor returns the session IDs mapped to one conversation.
//
// A session and the conversation it points at are one thing to a user, so
// deleting either side must delete the other. Deleting the conversation alone
// leaves a session pointing at a conversation that no longer exists, and its
// next turn silently opens a new one under the old id.
//
// More than one session can name the same conversation, because `PUT
// /v1/sessions/{id}` binds without requiring the conversation to be free.
func (cc *ContextCache) SessionsFor(convID string) []string {
	if strings.TrimSpace(convID) == "" {
		return nil
	}
	records, _ := cc.List()
	var sessions []string
	for _, record := range records {
		if record.ConversationID == convID {
			sessions = append(sessions, record.SessionID)
		}
	}
	return sessions
}

// Delete removes a conversation ID from memory and disk.
func (cc *ContextCache) Delete(key string) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	delete(cc.mem, key)
	if idx := indexOf(cc.order, key); idx >= 0 {
		cc.order = append(cc.order[:idx], cc.order[idx+1:]...)
	}
	_ = cc.removeFile(cc.path(key))
}

// evict removes oldest entries when cache exceeds max size.
func (cc *ContextCache) evict() {
	for len(cc.order) > contextCacheMaxSize {
		old := cc.order[0]
		cc.order = cc.order[1:]
		delete(cc.mem, old)
	}
}

// indexOf returns the index of a string in a slice, or -1.
func indexOf(slice []string, s string) int {
	for i, v := range slice {
		if v == s {
			return i
		}
	}
	return -1
}

// APIServer handles HTTP API requests.
type APIServer struct {
	config       *models.Config
	tokenManager *auth.TokenManager
	m365Client   *client.M365Client
	codeTools    *codingtools.Manager
	ctxCache     *ContextCache
	// transcripts records the turns of a session so the browser interface can
	// redraw a conversation. It is nil when the interface is disabled, which
	// is what keeps message content off disk in a plain gateway deployment.
	transcripts *TranscriptStore
	// imageRefs resolves the references routeGeneratedImages mints for the
	// generated-image addresses it takes out of an answer.
	imageRefs *imageRefStore
	server    *http.Server
	stopCh    chan struct{}
	mu        sync.RWMutex

	// throttlingMu guards lastThrottling, which holds the most recent quota
	// counters M365 reported. Handlers read it to turn an exhausted quota into
	// a 429 instead of an unexplained empty response.
	throttlingMu   sync.RWMutex
	lastThrottling *client.ThrottlingInfo
}

// noteThrottling records the quota counters carried by a final stream chunk.
// Chunks without counters leave the previous value untouched, because M365
// sends the throttling object on its own update frames rather than on every
// turn.
func (api *APIServer) noteThrottling(info *client.ThrottlingInfo) {
	if info == nil {
		return
	}
	api.throttlingMu.Lock()
	api.lastThrottling = info
	api.throttlingMu.Unlock()
}

// currentThrottling returns the last known quota counters, or nil.
func (api *APIServer) currentThrottling() *client.ThrottlingInfo {
	api.throttlingMu.RLock()
	defer api.throttlingMu.RUnlock()
	return api.lastThrottling
}

// NewAPIServer creates a new API server instance.
func NewAPIServer(config *models.Config, tokenManager *auth.TokenManager) *APIServer {
	api := &APIServer{
		config:       config,
		tokenManager: tokenManager,
		ctxCache:     NewContextCache(contextCacheDir),
		imageRefs:    newImageRefStore(),
	}
	if config.EnableWebUI {
		api.transcripts = NewTranscriptStore(transcriptDir)
	}
	return api
}

// tokenRefreshInterval is the interval for periodic access token refresh.
const tokenRefreshInterval = 30 * time.Minute

// Start starts the HTTP server on the specified port.
func (api *APIServer) Start(port int) error {
	api.mu.Lock()
	// Initialize request transports and optional local coding tools.
	api.m365Client = client.NewM365Client(api.tokenManager)
	api.m365Client.SetThrottlingObserver(api.noteThrottling)
	api.m365Client.SetWebSearchEnabled(api.config.EnableWebSearch)
	if api.config.EnableCodeTools {
		manager, err := codingtools.New(codingtools.Config{
			Enabled:       true,
			WorkspaceDir:  api.config.WorkspaceDir,
			Timeout:       api.config.CodeToolTimeout,
			MaxOutput:     api.config.CodeToolMaxOutput,
			MaxReadBytes:  api.config.CodeToolMaxReadBytes,
			MaxIterations: api.config.CodeToolMaxIterations,
		})
		if err != nil {
			api.mu.Unlock()
			return fmt.Errorf("initialize coding tools: %w", err)
		}
		api.codeTools = manager
	}
	api.stopCh = make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", api.withAuth(api.handleChatCompletions))
	mux.HandleFunc("/v1/completions", api.withAuth(api.handleCompletions))
	mux.HandleFunc("/v1/responses", api.withAuth(api.handleResponses))
	mux.HandleFunc("/v1/responses/compact", api.withAuth(api.handleResponsesCompact))
	mux.HandleFunc("/v1/messages", api.withAuth(api.handleAnthropicMessages))
	mux.HandleFunc("/v1/messages/count_tokens", api.withAuth(api.handleAnthropicCountTokens))
	mux.HandleFunc("/v1/complete", api.withAuth(api.handleAnthropicComplete))
	mux.HandleFunc("/v1/images/generations", api.withAuth(api.handleImageGenerations))
	mux.HandleFunc("/v1/images/edits", api.withAuth(api.handleImageEdits))
	// A generated image the gateway put in an answer. The two routes above are
	// exact patterns, so the longest match keeps them on their own handlers.
	mux.HandleFunc("/v1/images/", api.withAuth(api.handleGeneratedImage))
	mux.HandleFunc("/v1/conversations", api.withAuth(api.handleConversations))
	mux.HandleFunc("/v1/conversations/", api.withAuth(api.handleConversation))
	// Session routes expose conversation IDs, so they stay behind the API key
	// middleware.
	mux.HandleFunc("/v1/sessions", api.withAuth(api.handleSessions))
	mux.HandleFunc("/v1/sessions/", api.withAuth(api.handleSession))
	mux.HandleFunc("/v1/models", api.handleModels)
	// Codex probes /v1/health before it sends any chat request and treats a
	// 404 as an unreachable provider. It stays public alongside /v1/models
	// because the probe carries no credential.
	mux.HandleFunc("/v1/health", api.handleV1Health)
	// The browser interface is served without a credential, so it has to be
	// told what to ask for before it can ask. Both routes are public for that
	// reason: one reports which gate to show, the other says whether an offered
	// credential is one this gateway accepts.
	mux.HandleFunc("/v1/auth", api.handleAuthMode)
	mux.HandleFunc("/v1/auth/verify", api.handleAuthVerify)
	// MCP exposes Copilot as a tool; it stays behind the API key middleware
	// because it drives real upstream turns.
	mux.HandleFunc("/mcp", api.withAuth(api.handleMCP))
	// Quota counters expose account usage, so this route stays behind the API
	// key middleware unlike the public /v1/models and /health routes.
	mux.HandleFunc("/v1/quota", api.withAuth(api.handleQuota))
	mux.HandleFunc("/v1/personalization", api.withAuth(api.handlePersonalization))
	mux.HandleFunc("/health", api.handleHealth)

	// Registered last on purpose: "/" is the pattern the mux falls back to,
	// so every route above still wins its own path.
	mux.HandleFunc("/", api.handleWebUI)

	api.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
		// A client that dribbles out its headers holds a connection open for
		// as long as it likes without these. There is deliberately no
		// WriteTimeout: it bounds the whole response, and every streaming
		// route here writes for as long as the upstream turn lasts.
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
	api.mu.Unlock()

	// Start background token refresher
	go api.runTokenRefresher()

	if len(api.config.APIKeys) > 0 {
		logging.Infof("Starting API server on port %d (API key required, %d key(s) configured)", port, len(api.config.APIKeys))
	} else {
		logging.Infof("Starting API server on port %d (no API key required)", port)
	}
	return api.server.ListenAndServe()
}

// runTokenRefresher periodically refreshes the access token in the background.
// This prevents the first request after token expiry from blocking 1-2 seconds.
// Also refreshes the designerapp broker token to keep the broker refresh token
// alive (broker RT has a 24h lifetime and must be rotated before expiry).
func (api *APIServer) runTokenRefresher() {
	ticker := time.NewTicker(tokenRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-api.stopCh:
			logging.Info("Token refresher stopping")
			return
		case <-ticker.C:
			logging.Debug("Token refresher: starting periodic refresh")
			if _, err := api.tokenManager.Refresh(); err != nil {
				logging.Errorf("Background token refresh failed: %v", err)
			} else {
				logging.Info("Background token refresh succeeded")
			}
			// Refresh designer token to keep broker RT rotated
			if _, err := api.tokenManager.GetDesignerToken(); err != nil {
				logging.Errorf("Background designer token refresh failed: %v", err)
			} else {
				logging.Debug("Background designer token refresh succeeded")
			}
		}
	}
}

// withAuth wraps a handler with API key authentication.
// If no API keys are configured, all requests are allowed (backward compatible).
func (api *APIServer) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logging.Debugf("Request: %s %s %s", r.Method, r.URL.Path, r.RemoteAddr)
		if r.Method == http.MethodOptions {
			next(w, r)
			return
		}
		if len(api.config.APIKeys) > 0 {
			offered := apiKeyCandidates(r)
			if len(offered) == 0 {
				logging.Warnf("Auth: no API key header from %s", r.RemoteAddr)
				api.sendError(w, http.StatusUnauthorized, "Missing API key; send Authorization: Bearer <key> or x-api-key: <key>")
				return
			}
			if !slices.ContainsFunc(offered, api.isValidAPIKey) {
				logging.Warnf("Auth: invalid API key from %s", r.RemoteAddr)
				api.sendError(w, http.StatusUnauthorized, "Invalid API key")
				return
			}
		}
		next(w, r)
	}
}

// apiKeyCandidates returns every credential the client offered, in priority
// order. An Anthropic SDK client sends the key as `x-api-key` under
// ANTHROPIC_API_KEY and as `Authorization: Bearer` under ANTHROPIC_AUTH_TOKEN,
// and Claude Code can send both at once with only one of them valid, so every
// offered credential is checked rather than just the first.
func apiKeyCandidates(r *http.Request) []string {
	var offered []string
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		offered = append(offered, key)
	}
	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		// A bare token without the scheme is tolerated because some clients
		// send the key unprefixed.
		if key := strings.TrimSpace(trimBearerPrefix(header)); key != "" {
			offered = append(offered, key)
		}
	}
	return offered
}

// trimBearerPrefix removes a case-insensitive "Bearer " scheme prefix.
func trimBearerPrefix(header string) string {
	if len(header) >= 7 && strings.EqualFold(header[:7], "bearer ") {
		return header[7:]
	}
	return header
}

// isValidAPIKey reports whether the offered token is a credential this gateway
// accepts: a configured API key, or the browser interface's password.
//
// The interface holds its password in a cookie and sends it in the same header
// an API client sends its key, so accepting it here is what lets the interface
// reach the routes behind withAuth without a session mechanism of its own.
//
// Every comparison is constant-time. A plain string compare returns as soon as
// two bytes differ, which tells a caller how much of a guess was right.
func (api *APIServer) isValidAPIKey(token string) bool {
	match := false
	for _, key := range api.config.APIKeys {
		if secretEqual(key, token) {
			match = true
		}
	}
	if api.config.WebUIPassword != "" && secretEqual(api.config.WebUIPassword, token) {
		match = true
	}
	return match
}

// secretEqual compares two secrets without returning early on the first
// differing byte. An empty expectation never matches.
func secretEqual(expected, offered string) bool {
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(offered)) == 1
}

// extractAPIKey gets the bearer token from the Authorization header.
// Used as a fallback session ID when no explicit session ID is provided.
func (api *APIServer) extractAPIKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}

// Stop stops the HTTP server and background token refresher.
func (api *APIServer) Stop() error {
	api.mu.Lock()
	defer api.mu.Unlock()

	// Signal background token refresher to stop
	if api.stopCh != nil {
		close(api.stopCh)
		api.stopCh = nil
	}

	if api.server != nil {
		return api.server.Close()
	}
	return nil
}

// handleHealth handles health check requests.
func (api *APIServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// handleV1Health answers the OpenAI-style health probe. It reports reachability
// only and never touches the upstream, so it stays cheap enough for a client to
// call it before every session.
//
// It returns JSON rather than the plain "OK" of /health, because every other
// /v1 route speaks JSON and a probe that parses the body would choke on text.
func (api *APIServer) handleV1Health(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	api.sendJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleModels handles model list requests.
func (api *APIServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodGet {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Advertised context/output hints so harnesses do not pre-truncate prompts
	// or output. M365 enforces its own server-side limits regardless; these are
	// client-facing hints only, overridable via M365_CONTEXT_WINDOW and
	// M365_MAX_OUTPUT_TOKENS.
	contextWindow := api.config.ContextWindowTokens
	maxOutput := api.config.MaxOutputTokens

	// The advertised output budget is only carved out of the window when it is
	// smaller than the window. With the defaults both hints are the same value,
	// so subtracting would advertise an input budget of zero.
	maxInput := contextWindow
	if maxOutput < contextWindow {
		maxInput = contextWindow - maxOutput
	}

	// Several registry keys are aliases for the same model, so the list is keyed
	// by the advertised id and sorted; a map range would otherwise return
	// duplicates in a different order on every request. The keys are kept
	// because reasoning routing is a property of the key, not of the id.
	byID := make(map[string]models.ModelConfig, len(models.ModelRegistry))
	keysByID := make(map[string][]string, len(models.ModelRegistry))
	for key, cfg := range models.ModelRegistry {
		byID[cfg.OpenAIID] = cfg
		keysByID[cfg.OpenAIID] = append(keysByID[cfg.OpenAIID], key)
	}
	ids := slices.Sorted(maps.Keys(byID))

	modelList := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		modelList = append(modelList, modelCatalogEntry(
			id, byID[id], contextWindow, maxInput, maxOutput, supportsReasoningRoute(keysByID[id])))
	}

	response := map[string]any{
		"object":                   "list",
		"data":                     modelList,
		"reasoning_effort_presets": models.ReasoningEffortPresets,

		// Anthropic's Models API paginates its list and its clients read these
		// three fields. The whole registry always fits in one page, so the
		// cursors are the first and last advertised id and there is never more.
		"has_more": false,
		"first_id": firstOrNil(ids),
		"last_id":  lastOrNil(ids),
	}

	api.sendJSON(w, http.StatusOK, response)
}

// supportsReasoningRoute reports whether any registry key for one advertised
// model reaches a reasoning variant. applyReasoningEffort routes `key` to
// `key+"-reasoning"`, so a model without that sibling ignores the effort a
// caller asks for and must not advertise the capability.
func supportsReasoningRoute(keys []string) bool {
	for _, key := range keys {
		if strings.HasSuffix(key, "-reasoning") {
			return true
		}
		if _, ok := models.ModelRegistry[key+"-reasoning"]; ok {
			return true
		}
	}
	return false
}

// firstOrNil returns the first id, or nil for an empty registry. Anthropic
// types the cursor as nullable, so an empty string would be a wrong value
// rather than an absent one.
func firstOrNil(ids []string) any {
	if len(ids) == 0 {
		return nil
	}
	return ids[0]
}

// lastOrNil returns the last id, or nil for an empty registry.
func lastOrNil(ids []string) any {
	if len(ids) == 0 {
		return nil
	}
	return ids[len(ids)-1]
}

// codexBaseInstructions is advertised in the model catalog. Codex builds its
// own request instructions from it; the proxy never forwards it upstream.
const codexBaseInstructions = "You are a helpful AI assistant. When asked to write code, always provide the complete implementation — never truncate, abbreviate, or return only a fragment. Write full, working code with all logic included."

// modelCreated is the advertised creation time. The registry records no real
// release date, so one fixed value is published in both encodings rather than
// two that could disagree.
const modelCreated = 1700000000

// capabilitySupport renders one Anthropic capability leaf.
func capabilitySupport(supported bool) map[string]any {
	return map[string]any{"supported": supported}
}

// anthropicCapabilities describes one model in the shape Anthropic's Models API
// publishes. Every leaf states what this proxy actually does, so a client that
// reads the tree is not told about a feature the proxy never implements.
func anthropicCapabilities(reasoningRoute, thinking bool) map[string]any {
	return map[string]any{
		// No Batch API, no Anthropic citations, no code execution tool, and no
		// context management strategies are implemented by this proxy.
		"batch":          capabilitySupport(false),
		"citations":      capabilitySupport(false),
		"code_execution": capabilitySupport(false),
		"context_management": map[string]any{
			"supported":                false,
			"clear_thinking_20251015":  capabilitySupport(false),
			"clear_tool_uses_20250919": capabilitySupport(false),
			"compact_20260112":         capabilitySupport(false),
		},
		// Effort is honoured only when the model has a reasoning variant to
		// route to; every advertised preset name is otherwise accepted.
		"effort": map[string]any{
			"supported": reasoningRoute,
			"low":       capabilitySupport(reasoningRoute),
			"medium":    capabilitySupport(reasoningRoute),
			"high":      capabilitySupport(reasoningRoute),
			"max":       capabilitySupport(reasoningRoute),
			"xhigh":     capabilitySupport(reasoningRoute),
		},
		"image_input": capabilitySupport(true),
		// PDF content blocks are not converted for the backend, and no strict
		// schema or JSON mode is enforced on a reply.
		"pdf_input":          capabilitySupport(false),
		"structured_outputs": capabilitySupport(false),
		// Thinking is surfaced only for the reasoning tones, which emit
		// ChainOfThoughtSummary; there is no adaptive mode to select.
		"thinking": map[string]any{
			"supported": thinking,
			"types": map[string]any{
				"enabled":  capabilitySupport(thinking),
				"adaptive": capabilitySupport(false),
			},
		},
	}
}

// modelCatalogEntry builds one /v1/models entry.
//
// The entry carries the OpenAI model object and the Anthropic ModelInfo at
// once, because both protocols are served from this one route and each reads
// only the fields it knows. OpenAI requires id, object, created and owned_by;
// Anthropic requires id, type, display_name and created_at. The two field sets
// do not collide, so neither client needs a separate endpoint.
//
// Capability fields appear both at the top level and under `capabilities`
// because OpenAI-compatible clients disagree on where to look, and Codex reads
// a further set of fields that plain OpenAI clients ignore. Anthropic's
// capability tree is merged into the same map; its key names are disjoint from
// the flat OpenAI-style ones, so both survive.
func modelCatalogEntry(id string, cfg models.ModelConfig, contextWindow, maxInput, maxOutput int, reasoningRoute bool) map[string]any {
	modalities := []string{"text", "image"}
	features := []string{"tools", "function_calling", "streaming", "reasoning", "vision"}
	thinking := cfg.Thinking
	capabilities := map[string]any{
		"chat_completions":           true,
		"responses":                  true,
		"streaming":                  true,
		"tools":                      true,
		"supports_tools":             true,
		"tool_calls":                 true,
		"function_calling":           true,
		"supports_function_calling":  true,
		"reasoning":                  true,
		"reasoning_efforts":          models.ReasoningEffortPresets,
		"supported_reasoning_levels": models.ReasoningEffortPresets,
		"reasoning_mode":             "gateway_tone_routing",
		"vision":                     true,
		"supports_vision":            true,
		"modalities":                 modalities,
		"input_modalities":           modalities,
		"output_modalities":          []string{"text"},
		"supported_features":         features,
	}
	// Anthropic's tree uses key names none of the flat entries above take, so
	// merging leaves both readable from the one map.
	maps.Copy(capabilities, anthropicCapabilities(reasoningRoute, thinking))

	return map[string]any{
		// OpenAI model object: id, object, created and owned_by are required,
		// shutdown_date is nullable and nothing is scheduled to retire.
		"id":            id,
		"slug":          id,
		"object":        "model",
		"created":       modelCreated,
		"owned_by":      cfg.OwnerOrDefault(),
		"shutdown_date": nil,

		// Anthropic ModelInfo: type, display_name and created_at complete the
		// object, and max_tokens is its name for the output ceiling.
		"type":       "model",
		"created_at": time.Unix(modelCreated, 0).UTC().Format(time.RFC3339),
		"max_tokens": maxOutput,

		"display_name":      cfg.DisplayNameOrDefault(),
		"description":       "Public model endpoint.",
		"visibility":        "list",
		"supported_in_api":  true,
		"priority":          1,
		"base_instructions": codexBaseInstructions,
		"model_messages":    codexModelMessages(),

		"context_window":                   contextWindow,
		"max_context_window":               contextWindow,
		"effective_context_window_percent": 95,
		"max_input_tokens":                 maxInput,
		"max_output_tokens":                maxOutput,
		"truncation_policy":                map[string]any{"mode": "tokens", "limit": 10000},

		"default_reasoning_level":      "medium",
		"supports_reasoning_summaries": true,
		"default_reasoning_summary":    "none",
		"supported_reasoning_levels":   models.ReasoningEffortPresets,
		"support_verbosity":            true,
		"default_verbosity":            "low",

		// Every model reaches caller-defined tools through the simulated tool
		// calling layer, so tool support is a property of the proxy rather than
		// of the tone.
		"supports_tools":                 true,
		"tool_calls":                     true,
		"function_calling":               true,
		"supports_function_calling":      true,
		"supports_parallel_tool_calls":   true,
		"tool_mode":                      "code_mode_only",
		"shell_type":                     "shell_command",
		"apply_patch_tool_type":          "freeform",
		"web_search_tool_type":           "text_and_image",
		"supports_search_tool":           true,
		"experimental_supported_tools":   []any{},
		"supports_image_detail_original": true,

		"vision":             true,
		"supports_vision":    true,
		"modalities":         modalities,
		"input_modalities":   modalities,
		"output_modalities":  []string{"text"},
		"supported_features": features,

		"additional_speed_tiers":            []string{},
		"service_tiers":                     []any{},
		"availability_nux":                  nil,
		"upgrade":                           nil,
		"include_skills_usage_instructions": false,
		"use_responses_lite":                false,
		"multi_agent_version":               "v2",

		"capabilities": capabilities,
	}
}

// codexModelMessages is the instruction template block Codex expects alongside
// each catalog entry.
func codexModelMessages() map[string]any {
	return map[string]any{
		"instructions_template": codexBaseInstructions,
		"instructions_variables": map[string]string{
			"personality_default":   "",
			"personality_friendly":  "",
			"personality_pragmatic": "",
		},
		"approvals":   nil,
		"auto_review": nil,
	}
}

// upstreamThrottledCode marks a response rejected because the M365
// conversation exhausted its message quota.
const upstreamThrottledCode = "upstream_throttled"

// handleQuota reports the last known M365 conversation quota counters. The
// backend only sends them while a turn is in flight, so the values reflect the
// most recent chat request rather than a live lookup.
func (api *APIServer) handleQuota(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodGet {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	info := api.currentThrottling()
	if info == nil {
		api.sendJSON(w, http.StatusOK, map[string]any{
			"object":    "quota",
			"available": false,
			"detail":    "no quota counters observed yet; send a chat request first",
		})
		return
	}

	response := map[string]any{
		"object":    "quota",
		"available": true,
		"exhausted": info.Exhausted(),
	}
	if info.NumUserMessages != nil {
		response["used"] = *info.NumUserMessages
	}
	if info.MaxNumUserMessages != nil {
		response["max"] = *info.MaxNumUserMessages
	}
	if info.NumUserMessages != nil && info.MaxNumUserMessages != nil {
		response["headroom"] = *info.MaxNumUserMessages - *info.NumUserMessages
	}
	if len(info.Extra) > 0 {
		response["extra"] = info.Extra
	}
	api.sendJSON(w, http.StatusOK, response)
}

// quotaExhausted reports whether the last observed counters show the
// conversation at its message ceiling.
func (api *APIServer) quotaExhausted() bool {
	return api.currentThrottling().Exhausted()
}

// sendThrottledError reports an exhausted conversation quota as HTTP 429 so
// clients back off instead of retrying against an unexplained empty response.
func (api *APIServer) sendThrottledError(w http.ResponseWriter) {
	info := api.currentThrottling()
	message := "M365 conversation message quota exhausted; start a new session to continue"
	if summary := info.Summary(); summary != "" {
		message = message + " (" + summary + ")"
	}
	api.sendErrorCode(w, http.StatusTooManyRequests, upstreamThrottledCode, message)
}

// upstreamContentBlockedCode marks a reply that is M365's canned content
// refusal rather than an answer.
const upstreamContentBlockedCode = "upstream_content_blocked"

// sendContentBlockedError reports a backend content refusal as HTTP 502.
//
// The refusal reads like an ordinary short answer, so an agent client would
// otherwise accept it and continue on nothing. A distinct status lets the
// client tell "the backend declined this request" apart from "here is the
// answer".
func (api *APIServer) sendContentBlockedError(w http.ResponseWriter, reply string) {
	logging.Warn("upstream content refusal: M365 declined the request instead of answering")
	api.sendErrorCode(w, http.StatusBadGateway, upstreamContentBlockedCode,
		"M365 declined this request: "+strings.TrimSpace(reply))
}

// blockedByContentPolicy reports whether a finished turn is a content refusal
// with nothing else to deliver. A turn that also produced tool calls is real
// work and is left alone.
func blockedByContentPolicy(respText string, toolCalls []client.ToolCall) bool {
	return len(toolCalls) == 0 && toolcalling.IsContentPolicyBlock(respText)
}

// unverifiedCompletionNotice replaces an answer that reports finished work the
// server has no evidence for.
const unverifiedCompletionNotice = "No tool was called in this turn and no tool result exists, so the reported completion is not verified. Nothing was executed."

// withoutUnverifiedCompletionClaim replaces an answer in which the model says
// it carried the work out itself, while the turn emitted no tool call and the
// history holds no tool result to support it. An agent client would otherwise
// accept the report and stop.
//
// The guard is limited to requests that declared tools. The reference this is
// drawn from omits that condition, so a plain chat answer such as "Go was
// created at Google" trips it; here such an answer is never touched.
func withoutUnverifiedCompletionClaim(respText string, hasTools bool, ledger toolcalling.Ledger, toolCalls []client.ToolCall) string {
	return replaceUnverifiedCompletionClaim(respText, hasTools, ledger, len(toolCalls))
}

// replaceUnverifiedCompletionClaim is the count-based form, for the streaming
// paths whose parsed calls are toolcalling.ToolCall rather than client.ToolCall.
func replaceUnverifiedCompletionClaim(respText string, hasTools bool, ledger toolcalling.Ledger, toolCallCount int) string {
	if !unverifiedCompletionClaim(respText, hasTools, ledger, toolCallCount) {
		return respText
	}
	logging.Warn("replacing an unverified completion claim: the turn called no tool and holds no tool result")
	logging.Debugf("unverified completion claim was: %q", respText)
	return unverifiedCompletionNotice
}

// unverifiedCompletionClaim is the condition behind the guard, separated so a
// streaming turn can report it without holding client-shaped tool calls.
func unverifiedCompletionClaim(respText string, hasTools bool, ledger toolcalling.Ledger, toolCallCount int) bool {
	if !hasTools || toolCallCount > 0 || len(ledger.Completed) > 0 {
		return false
	}
	return toolcalling.ClaimsUnverifiedCompletion(respText)
}

// warnOnUnverifiedCompletionClaim reports the same failure where the text has
// already reached the client and can no longer be replaced. Only the Responses
// stream is in that position: it publishes assistant content as it decodes it,
// while the other streaming paths buffer a tool-enabled turn until the parse is
// done and can still replace the answer.
func warnOnUnverifiedCompletionClaim(respText string, hasTools bool, ledger toolcalling.Ledger, toolCallCount int) {
	if unverifiedCompletionClaim(respText, hasTools, ledger, toolCallCount) {
		logging.Warn("unverified completion claim on a streaming turn: the text was already sent to the client and could not be replaced")
		logging.Debugf("unverified completion claim was: %q", respText)
	}
}

// handleConversations lists or creates M365 conversations.
func (api *APIServer) handleConversations(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method == http.MethodPost {
		api.createConversation(w, r)
		return
	}
	if r.Method != http.MethodGet {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	conversationClient := client.NewConversationClient(api.tokenManager)
	conversations, err := conversationClient.ListConversations(r.Context())
	if err != nil {
		api.sendConversationError(w, err)
		return
	}
	api.sendJSON(w, http.StatusOK, map[string]any{"conversations": conversations})
}

func (api *APIServer) createConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message   string `json:"message"`
		Name      string `json:"name"`
		Model     string `json:"model"`
		SessionID string `json:"session_id,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		api.sendError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		api.sendError(w, http.StatusBadRequest, "message is required")
		return
	}
	if req.Model == "" {
		req.Model = "gpt5.5-reasoning"
	}
	cfg, ok := api.resolveModel(w, req.Model)
	if !ok {
		return
	}
	messages := []payload.Message{{Role: "user", Content: req.Message}}
	_, _, _, _, conversationID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, "", api.config.UserOID, api.config.TenantID, false)
	if err != nil {
		api.sendError(w, http.StatusBadGateway, "M365 conversation creation failed")
		return
	}
	if conversationID == "" {
		api.sendError(w, http.StatusBadGateway, "M365 conversation creation returned no conversation ID")
		return
	}
	if strings.TrimSpace(req.Name) != "" {
		conversationClient := client.NewConversationClient(api.tokenManager)
		if err := conversationClient.RenameConversation(r.Context(), conversationID, strings.TrimSpace(req.Name)); err != nil {
			api.sendConversationError(w, err)
			return
		}
	}
	api.sendJSON(w, http.StatusCreated, map[string]any{"id": conversationID, "name": req.Name})
}

// handleConversation reads, renames or permanently deletes one M365
// conversation.
func (api *APIServer) handleConversation(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	conversationID, subresource, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/conversations/"), "/")
	if conversationID == "" {
		api.sendError(w, http.StatusNotFound, "Conversation not found")
		return
	}
	if subresource != "" {
		if subresource != "messages" || r.Method != http.MethodGet {
			api.sendError(w, http.StatusNotFound, "Conversation not found")
			return
		}
		api.readConversationHistory(w, r, conversationID)
		return
	}
	conversationClient := client.NewConversationClient(api.tokenManager)
	switch r.Method {
	case http.MethodPatch:
		api.renameConversation(w, r, conversationClient, conversationID)
	case http.MethodDelete:
		api.deleteConversation(w, r, conversationClient, conversationID)
	default:
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// renameConversation gives an M365 web conversation a new name.
func (api *APIServer) renameConversation(w http.ResponseWriter, r *http.Request, conversationClient *client.ConversationClient, conversationID string) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		api.sendError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		api.sendError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := conversationClient.RenameConversation(r.Context(), conversationID, name); err != nil {
		api.sendConversationError(w, err)
		return
	}
	api.sendJSON(w, http.StatusOK, map[string]any{"id": conversationID, "name": name})
}

// deleteConversation removes the conversation upstream and drops every session
// bound to it, because more than one session can point at one conversation.
func (api *APIServer) deleteConversation(w http.ResponseWriter, r *http.Request, conversationClient *client.ConversationClient, conversationID string) {
	if err := conversationClient.DeleteConversation(r.Context(), conversationID); err != nil {
		api.sendConversationError(w, err)
		return
	}
	api.dropSessionsFor(conversationID)
	w.WriteHeader(http.StatusNoContent)
}

// readConversationHistory imports the turns of a conversation this gateway
// never carried.
//
// The transcript store only knows the turns that went through this process, so
// a conversation started in the M365 web or mobile client opens empty. Reading
// it costs a page download and a walk of a serialization this project does not
// control, which is why it runs on request instead of on every open.
//
// A session_id parameter also stores the result under that session, so the
// interface can continue the conversation with its history in place and does
// not pay the download again on the next open.
func (api *APIServer) readConversationHistory(w http.ResponseWriter, r *http.Request, conversationID string) {
	conversationClient := client.NewConversationClient(api.tokenManager)
	history, err := conversationClient.FetchHistory(r.Context(), conversationID)
	if err != nil {
		if errors.Is(err, client.ErrHistoryUnavailable) {
			logging.Errorf("Conversation history could not be read: %v", err)
			api.sendError(w, http.StatusBadGateway, "M365 did not return a readable conversation history")
			return
		}
		api.sendConversationError(w, err)
		return
	}

	// The entries carry the stored-transcript shape, so the interface renders an
	// imported conversation with the code it already has.
	now := time.Now().Unix()
	entries := make([]TranscriptEntry, 0, len(history))
	for _, msg := range history {
		created := now
		if parsed, err := time.Parse(time.RFC3339, msg.CreatedAt); err == nil {
			created = parsed.Unix()
		}
		entries = append(entries, TranscriptEntry{Role: msg.Role, Content: msg.Text, CreatedAt: created})
	}

	imported := false
	if sid := strings.TrimSpace(r.URL.Query().Get("session_id")); sid != "" && api.transcripts != nil {
		api.transcripts.Replace(sid, entries)
		api.ctxCache.Set(sessionKeyPrefix+sid, conversationID)
		imported = true
	}

	api.sendJSON(w, http.StatusOK, map[string]any{
		"conversation_id": conversationID,
		"messages":        entries,
		"imported":        imported,
	})
}

func (api *APIServer) sendConversationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrM365CookiesUnavailable):
		api.sendError(w, http.StatusUnauthorized, "M365 web app cookies are not configured")
	case errors.Is(err, client.ErrConversationAuthentication):
		api.sendError(w, http.StatusUnauthorized, "M365 web app cookies are invalid or expired")
	default:
		logging.Errorf("Conversation management request failed: %v", err)
		api.sendError(w, http.StatusBadGateway, "M365 conversation service request failed")
	}
}

// handleCORS handles CORS preflight requests.
func (api *APIServer) handleCORS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-Id, X-Claude-Code-Session-Id, Session-Id")
	w.WriteHeader(http.StatusOK)
}

// clientSessionHeaders name the header each agent client stamps its own session
// on. Claude Code sends X-Claude-Code-Session-Id on every request of a session,
// and Codex sends a bare session-id. Neither client can be told to send
// X-Session-Id, so without these names both fall through to the message hash.
//
// Codex also sends thread-id carrying the same value, which is why it is absent
// here: it could only ever answer for a request that already carries session-id.
//
// Deliberately not read: Codex's x-codex-turn-metadata. It carries an
// installation_id that stays the same across every session on one machine, so
// keying a conversation on it would merge unrelated sessions into one.
var clientSessionHeaders = []string{"X-Claude-Code-Session-Id", "Session-Id"}

// clientStampedSessionID returns the session a client stamped on the request
// itself. It ranks below every field the caller sets deliberately, because the
// client writes this header without being asked.
func clientStampedSessionID(r *http.Request) string {
	for _, name := range clientSessionHeaders {
		if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

// sessionSources carries the places a request can name its own session, other
// than the headers, which resolveSessionID reads from the request itself.
type sessionSources struct {
	// ModelSuffix is the part after the colon in "modelKey:sessionID".
	ModelSuffix string
	// PreviousResponseID chains one Responses call to the last; the other
	// endpoints leave it empty.
	PreviousResponseID string
	BodySessionID      string
	BodyUser           string
}

// resolveSessionID picks the session a request belongs to.
//
// Every endpoint calls this, so the order is decided once. It used to live
// inline at each call site, and the copies disagreed: three endpoints ranked
// the X-Session-Id header above the body fields while the rest ranked the body
// first, so the same request produced different sessions on different routes.
//
// The order runs from what the caller chose most deliberately to what it never
// chose at all. A header a client stamps by itself therefore ranks last, and
// the caller's own values always win over it.
//
// An empty result means the request named no session; the caller decides what
// to fall back to, because a chat hashes its first message while an image edit
// mints a fresh id.
func resolveSessionID(r *http.Request, src sessionSources) string {
	for _, candidate := range []string{
		src.ModelSuffix,
		src.PreviousResponseID,
		src.BodySessionID,
		src.BodyUser,
		r.Header.Get("X-Session-Id"),
		clientStampedSessionID(r),
	} {
		if v := strings.TrimSpace(candidate); v != "" {
			return v
		}
	}
	return ""
}

// hashSessionIDFromMessages derives a session ID from the API key and the first
// user message, for a request that named no session of its own.
//
// When auth is enabled the API key is hashed in, so two keys sending the same
// first message get separate conversations. The digest is stable for one first
// message, which is what gives an unconfigured client continuity across the
// turns of a conversation.
func (api *APIServer) hashSessionIDFromMessages(r *http.Request, messages []payload.Message) string {
	firstMsg := ""
	for _, m := range messages {
		if m.Role == "user" && m.Content != "" {
			firstMsg = m.Content
			break
		}
	}
	if firstMsg == "" {
		return ""
	}
	apiKey := api.extractAPIKey(r)
	// The digest derives a stable session id from the caller's first message;
	// it protects nothing and is never compared against a supplied value.
	// #nosec G401
	h := md5.Sum([]byte(apiKey + "\x00" + firstMsg))
	return "h:" + hex.EncodeToString(h[:])
}

type toolLoopProvider int

const (
	toolLoopOpenAI toolLoopProvider = iota
	toolLoopAnthropic
)

type toolLoopResult struct {
	text           string
	thinking       string
	toolCalls      []client.ToolCall
	finishReason   string
	conversationID string
}

func clientToolCallFromSimulated(call toolcalling.ToolCall) client.ToolCall {
	return client.ToolCall{
		ID:   call.ID,
		Type: "function",
		Function: client.ToolCallFunction{
			Name:      call.Name,
			Namespace: call.Namespace,
			Arguments: string(call.Arguments),
		},
	}
}

func (api *APIServer) prepareCodingTools(tools []toolcalling.ToolDef, anthropic bool) ([]toolcalling.ToolDef, map[string]bool) {
	local := make(map[string]bool)
	if api.codeTools == nil {
		return tools, local
	}
	available := make(map[string]codingtools.Tool)
	for _, schema := range api.codeTools.Tools() {
		available[schema.Name] = schema
	}
	for _, definition := range tools {
		name := toolcalling.ToolName(&definition)
		if _, ok := available[name]; ok {
			local[name] = true
		}
	}
	if !api.config.AutoExposeTools {
		return tools, local
	}
	seen := make(map[string]bool, len(tools))
	for i := range tools {
		seen[toolcalling.ToolName(&tools[i])] = true
	}
	for _, schema := range api.codeTools.Tools() {
		local[schema.Name] = true
		if seen[schema.Name] {
			continue
		}
		definition := toolcalling.ToolDef{Name: schema.Name, Description: schema.Description, InputSchema: schema.InputSchema}
		if !anthropic {
			definition = toolcalling.ToolDef{Type: "function", Function: toolcalling.ToolDefFunc{Name: schema.Name, Description: schema.Description, Parameters: schema.InputSchema}}
		}
		tools = append(tools, definition)
	}
	return tools, local
}

func replaceRequestTools(body []byte, tools []toolcalling.ToolDef) string {
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return string(body)
	}
	request["tools"] = tools
	updated, err := json.Marshal(request)
	if err != nil {
		return string(body)
	}
	return string(updated)
}

// runToolLoop drives the request-local built-in coding tool loop.
//
// It owns the session-to-conversation mapping for this path and writes it as
// soon as the backend names a conversation, not on the way out. The loop has
// eight exits, half of them errors, and a store placed on the way out would be
// missed by whichever exit is added next; a turn that aborts mid-loop would
// then orphan the conversation M365 had already created. The responders this
// loop feeds therefore do not store the mapping again.
func (api *APIServer) runToolLoop(r *http.Request, provider toolLoopProvider, messages []payload.Message, cfg models.ModelConfig, sid, convID string, tools []toolcalling.ToolDef, noParallel bool, local map[string]bool) (toolLoopResult, error) {
	currentConvID := convID
	seen := make(map[string]bool)
	for iteration := 0; ; iteration++ {
		text, thinking, backendCalls, finishReason, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, currentConvID, api.config.UserOID, api.config.TenantID, len(tools) > 0)
		if err != nil {
			return toolLoopResult{conversationID: currentConvID}, err
		}
		currentConvID = api.rememberConversation(sid, currentConvID, finalConvID)
		if len(tools) == 0 {
			_, finishReason = withoutBackendToolCalls(backendCalls, finishReason)
			return toolLoopResult{text: text, thinking: thinking, finishReason: finishReason, conversationID: currentConvID}, nil
		}

		contracts := toolcalling.ContractsFor(tools).WithoutParallel(noParallel)
		simulated := parseLoopSimulation(provider, text, tools, contracts)
		simulated = api.repairSimulatedToolCalls(provider, messages, cfg, tools, contracts, text, simulated)
		if answer, done := answerWithoutCalls(simulated, text, thinking, finishReason, currentConvID); done {
			return answer, nil
		}

		callerCalls, localCalls := splitToolCalls(simulated.ToolCalls, local)
		if len(callerCalls) > 0 {
			return toolLoopResult{thinking: thinking, toolCalls: callerCalls, finishReason: "tool_calls", conversationID: currentConvID}, nil
		}
		if iteration >= api.config.CodeToolMaxIterations-1 {
			return toolLoopResult{conversationID: currentConvID}, errors.New("coding tool iteration limit reached")
		}

		resultParts, err := api.executeLocalCalls(r, localCalls, seen)
		if err != nil {
			return toolLoopResult{conversationID: currentConvID}, err
		}
		messages, err = appendToolLoopContinuation(messages, resultParts, cfg, tools, provider)
		if err != nil {
			return toolLoopResult{conversationID: currentConvID}, err
		}
	}
}

// rememberConversation stores the conversation the backend answered on, so the
// next turn of this session continues it rather than starting a new one.
func (api *APIServer) rememberConversation(sid, currentConvID, finalConvID string) string {
	if finalConvID == "" {
		return currentConvID
	}
	if sid != "" {
		api.ctxCache.Set(sessionKeyPrefix+sid, finalConvID)
	}
	return finalConvID
}

// parseLoopSimulation parses the backend's answer in the provider's own shape.
func parseLoopSimulation(provider toolLoopProvider, text string, tools []toolcalling.ToolDef, contracts toolcalling.ToolContracts) toolcalling.SimulatedResult {
	if provider == toolLoopAnthropic {
		return toolcalling.ParseSimulatedResponseAnthropic(text, toolNamesFromDefs(tools), contracts)
	}
	return toolcalling.ParseSimulatedResponse(text, toolNamesFromDefs(tools), contracts)
}

// answerWithoutCalls reports the turn's plain answer when the simulation
// produced no tool call, which is a legitimate outcome rather than a failure.
func answerWithoutCalls(sim toolcalling.SimulatedResult, text, thinking, finishReason, convID string) (toolLoopResult, bool) {
	if sim.HasPayload && len(sim.ToolCalls) > 0 {
		return toolLoopResult{}, false
	}
	if sim.HasPayload {
		text, finishReason = sim.Content, "stop"
	}
	return toolLoopResult{text: text, thinking: thinking, finishReason: finishReason, conversationID: convID}, true
}

// splitToolCalls separates the calls this gateway runs itself from the ones
// only the client can run.
func splitToolCalls(calls []toolcalling.ToolCall, local map[string]bool) ([]client.ToolCall, []toolcalling.ToolCall) {
	var callerCalls []client.ToolCall
	var localCalls []toolcalling.ToolCall
	for _, call := range calls {
		if local[call.Name] {
			localCalls = append(localCalls, call)
			continue
		}
		callerCalls = append(callerCalls, clientToolCallFromSimulated(call))
	}
	return callerCalls, localCalls
}

// executeLocalCalls runs the built-in coding tools and renders their results.
//
// The arguments are normalized before they identify a call, because a model
// that re-emits the same call with its JSON keys in another order is repeating
// it, not asking something new.
func (api *APIServer) executeLocalCalls(r *http.Request, localCalls []toolcalling.ToolCall, seen map[string]bool) ([]string, error) {
	var resultParts []string
	for _, call := range localCalls {
		key := toolcalling.CallSignature(call.Name, string(call.Arguments))
		if seen[key] {
			return nil, fmt.Errorf("duplicate coding tool call %q", call.Name)
		}
		seen[key] = true
		var arguments map[string]any
		if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
			arguments = map[string]any{}
		}
		encoded, err := codingtools.MarshalResult(api.codeTools.Execute(r.Context(), call.Name, arguments))
		if err != nil {
			return nil, fmt.Errorf("serialize coding tool result: %w", err)
		}
		resultParts = append(resultParts, toolcalling.FormatSimulatedToolResult(call.ID, call.Name, string(encoded)))
	}
	return resultParts, nil
}

// appendToolLoopContinuation adds the tool results to the conversation and
// re-injects the request envelope, so the next turn sees the same contract.
func appendToolLoopContinuation(messages []payload.Message, resultParts []string, cfg models.ModelConfig, tools []toolcalling.ToolDef, provider toolLoopProvider) ([]payload.Message, error) {
	messages = append(messages, payload.Message{Role: "user", Content: strings.Join(resultParts, "\n\n")})
	request := map[string]any{"model": cfg.OpenAIID, "messages": messages, "tools": tools, "stream": false}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("serialize coding tool continuation: %w", err)
	}
	// The synthetic messages of the built-in loop carry no client tool
	// structure, so there is no ledger to pass here.
	if provider == toolLoopAnthropic {
		injectSimulatedPromptAnthropic(&messages, string(requestJSON), "auto", "")
	} else {
		injectSimulatedPrompt(&messages, string(requestJSON), "auto", "")
	}
	return messages, nil
}

// repairSimulatedToolCalls performs a single corrective re-ask when the initial
// simulated parse dropped every tool call for missing required arguments. The
// request envelope already lives in the last message, so the retry re-sends it
// with an appended corrective note on a fresh conversation and re-parses. It
// returns the recovered result when the backend supplies valid tool calls, and
// the original result otherwise so callers keep their existing fallback path.
func (api *APIServer) repairSimulatedToolCalls(provider toolLoopProvider, messages []payload.Message, cfg models.ModelConfig, tools []toolcalling.ToolDef, contracts toolcalling.ToolContracts, rawText string, sim toolcalling.SimulatedResult) toolcalling.SimulatedResult {
	if len(sim.ToolCalls) > 0 || len(messages) == 0 || len(tools) == 0 {
		return sim
	}

	note, narrated, worthRetrying := repairNote(sim, tools, contracts, rawText)
	if !worthRetrying {
		return sim
	}

	retry := make([]payload.Message, len(messages))
	copy(retry, messages)
	last := retry[len(retry)-1]
	last.Content = last.Content + "\n\n" + note
	retry[len(retry)-1] = last

	text, _, _, _, _, err := api.m365Client.ChatConversation(retry, cfg.Tone, cfg.Override, "", api.config.UserOID, api.config.TenantID, true)
	if err != nil {
		logging.Errorf("repairSimulatedToolCalls: retry failed: %v", err)
		return sim
	}

	retried := parseLoopSimulation(provider, text, tools, contracts)
	if len(retried.ToolCalls) > 0 {
		logging.Infof("repairSimulatedToolCalls: recovered %d tool call(s) after re-ask", len(retried.ToolCalls))
		return retried
	}
	// An announcement that survived the re-ask is worse than useless to the
	// client: it reads as work in progress that never arrives.
	if narrated {
		logging.Warn("repairSimulatedToolCalls: re-ask stayed an announcement; replacing the answer text")
		sim.Content = toolcalling.ToolNarrationNotice
		sim.FinishReason = "stop"
		sim.HasPayload = true
		return sim
	}
	logging.Warn("repairSimulatedToolCalls: re-ask did not yield valid tool calls; keeping original response")
	return sim
}

// repairNote decides whether a re-ask is worth making, and what note to append.
//
// A dropped call is always worth re-asking. With no call at all it is only
// worth it when the reply denies the tools exist, claims the work already ran
// elsewhere, or only announces which tool it means to use; an ordinary text
// answer is a legitimate outcome.
func repairNote(sim toolcalling.SimulatedResult, tools []toolcalling.ToolDef, contracts toolcalling.ToolContracts, rawText string) (note string, narrated, worthRetrying bool) {
	if len(sim.DroppedCalls) > 0 {
		logging.Warnf("repairSimulatedToolCalls: re-asking backend, tool calls failed validation: %v", sim.DroppedCalls)
		return toolcalling.BuildRepairNote(sim.DroppedCalls, contracts), false, true
	}

	answer := sim.Content
	if answer == "" {
		answer = rawText
	}
	switch {
	case toolcalling.IsToolRefusal(answer):
		logging.Warn("repairSimulatedToolCalls: re-asking backend, reply denied the declared tools exist")
	case toolcalling.IsSandboxHallucination(answer):
		logging.Warn("repairSimulatedToolCalls: re-asking backend, reply claimed to have run the work itself")
	case toolcalling.IsToolIntentNarration(answer, toolNamesFromDefs(tools)):
		narrated = true
		logging.Warn("repairSimulatedToolCalls: re-asking backend, reply only announced which tool it would use")
	default:
		return "", false, false
	}
	return toolcalling.BuildNativeToolBanNote(), narrated, true
}

// handleChatCompletions handles OpenAI chat completion requests.
// chatCompletionsRequest is the wire shape of a /v1/chat/completions request.
type chatCompletionsRequest struct {
	Model          string                `json:"model"`
	Messages       []payload.Message     `json:"messages"`
	Stream         bool                  `json:"stream"`
	MaxTokens      int                   `json:"max_tokens"`
	ResponseFormat map[string]any        `json:"response_format"`
	SessionID      string                `json:"session_id"`
	User           string                `json:"user"`
	Tools          []toolcalling.ToolDef `json:"tools"`
	ToolChoice     any                   `json:"tool_choice"`
	// A pointer, because an absent parallel_tool_calls means the OpenAI
	// default of true rather than false.
	ParallelToolCalls *bool          `json:"parallel_tool_calls"`
	StreamOptions     *streamOptions `json:"stream_options"`
	// Either a single string or an array of them, so it arrives untyped.
	Stop any `json:"stop"`
}

// readJSONRequest answers the method gate and decodes the body into dst. It
// returns the raw bytes, because a tool-enabled request re-serializes them into
// the simulation prompt. ok is false when the request has already been answered.
func (api *APIServer) readJSONRequest(w http.ResponseWriter, r *http.Request, dst any, op string) (bodyBytes []byte, ok bool) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return nil, false
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return nil, false
	}

	limitRequestBody(w, r, requestBodyMax)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		api.sendRequestBodyError(w, err)
		return nil, false
	}
	_ = r.Body.Close()

	if err := json.Unmarshal(bodyBytes, dst); err != nil {
		logging.Errorf("%s: invalid JSON: %v", op, err)
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return nil, false
	}
	return bodyBytes, true
}

// prepareToolLedger rebuilds the evidence of a client-driven tool loop and
// refuses a request whose tool results do not line up with its calls. ok is
// false when the request has already been answered.
func (api *APIServer) prepareToolLedger(w http.ResponseWriter, messages []payload.Message, op string) (toolcalling.Ledger, bool) {
	if err := validateToolResultMessages(messages); err != nil {
		logging.Errorf("%s: %v", op, err)
		api.sendError(w, http.StatusBadRequest, err.Error())
		return toolcalling.Ledger{}, false
	}
	ledger := buildToolLedger(messages)
	if api.exceededToolRoundLimit(ledger) {
		api.sendToolRoundLimitError(w, ledger)
		return toolcalling.Ledger{}, false
	}
	return ledger, true
}

// sessionAndConversation resolves the session this request belongs to and the
// conversation that session is bound to.
func (api *APIServer) sessionAndConversation(r *http.Request, sources sessionSources, messages []payload.Message) (sid, convID string) {
	sid = resolveSessionID(r, sources)
	if sid == "" {
		sid = api.hashSessionIDFromMessages(r, messages)
	}
	if sid == "" {
		return "", ""
	}
	return sid, api.ctxCache.Get(sessionKeyPrefix + sid)
}

func (api *APIServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatCompletionsRequest
	bodyBytes, ok := api.readJSONRequest(w, r, &req, "handleChatCompletions")
	if !ok {
		return
	}

	// Parse optional session ID encoded in model name: "gpt5.5:my-session"
	modelKey, modelSessionID := parseModelSessionID(req.Model)
	cfg, ok := api.resolveModel(w, modelKey)
	if !ok {
		return
	}
	logging.Infof("handleChatCompletions: model=%s stream=%v tools=%d sid=%s", modelKey, req.Stream, len(req.Tools), modelSessionID)

	ledger, ok := api.prepareToolLedger(w, req.Messages, "handleChatCompletions")
	if !ok {
		return
	}

	// Handle JSON mode
	if instruction, ok := jsonModeInstruction(req.ResponseFormat); ok {
		injectJSONMode(&req.Messages, instruction)
	}

	preparedTools, localTools := api.prepareCodingTools(req.Tools, false)
	req.Tools = preparedTools
	requestJSON := replaceRequestTools(bodyBytes, req.Tools)
	// The declaration list stays whole in requestJSON so the model sees every
	// capability, but simulation only runs when something is left for the
	// client to execute.
	hasTools := len(toolcalling.RouteableTools(req.Tools)) > 0
	if hasTools {
		injectSimulatedPrompt(&req.Messages, requestJSON, toolChoiceString(req.ToolChoice), ledger.EvidenceNote())
	}

	sid, convID := api.sessionAndConversation(r, sessionSources{
		ModelSuffix:   modelSessionID,
		BodySessionID: req.SessionID,
		BodyUser:      req.User,
	}, req.Messages)

	// Upload any images found in multimodal content and attach annotations
	api.uploadImagesAndAnnotate(&req.Messages, convID)

	// Recorded before the turn runs, so a turn that fails still shows the
	// message the caller sent rather than losing it.
	api.recordUserTurn(sid, req.Messages)

	noParallel := refusesParallelToolCalls(req.ParallelToolCalls)
	stopSequences := openAIStopSequences(req.Stop)

	if len(localTools) > 0 {
		api.runChatToolLoop(w, r, req, cfg, sid, convID, localTools, noParallel, stopSequences)
		return
	}
	if req.Stream {
		api.streamChatCompletions(r.Context(), w, req.Messages, cfg, sid, convID, req.MaxTokens, hasTools, req.Tools, toolChoiceString(req.ToolChoice), stopSequences, noParallel, includeStreamUsage(req.StreamOptions))
		return
	}
	api.nonStreamChatCompletions(w, req.Messages, cfg, sid, convID, req.MaxTokens, hasTools, req.Tools, toolChoiceString(req.ToolChoice), stopSequences, noParallel)
}

// runChatToolLoop runs the built-in coding tool loop for a chat request, which
// answers whole rather than streaming because the loop takes several turns.
func (api *APIServer) runChatToolLoop(w http.ResponseWriter, r *http.Request, req chatCompletionsRequest, cfg models.ModelConfig, sid, convID string, localTools map[string]bool, noParallel bool, stopSequences []string) {
	result, err := api.runToolLoop(r, toolLoopOpenAI, req.Messages, cfg, sid, convID, req.Tools, noParallel, localTools)
	if err != nil {
		api.sendUpstreamError(w, "chat", err)
		return
	}
	api.respondBufferedChat(w, result, req.Messages, cfg, sid, req.MaxTokens, req.Stream, req.Tools, toolChoiceString(req.ToolChoice), stopSequences)
}

// handleCompletions handles OpenAI text completion requests.
func (api *APIServer) handleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	limitRequestBody(w, r, requestBodyMax)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		api.sendRequestBodyError(w, err)
		return
	}
	_ = r.Body.Close()

	var req struct {
		Model         string                `json:"model"`
		Prompt        string                `json:"prompt"`
		Suffix        string                `json:"suffix"`
		Stream        bool                  `json:"stream"`
		MaxTokens     int                   `json:"max_tokens"`
		Tools         []toolcalling.ToolDef `json:"tools"`
		ToolChoice    any                   `json:"tool_choice"`
		StreamOptions *streamOptions        `json:"stream_options"`
		// Either a single string or an array of them, so it arrives untyped.
		Stop      any    `json:"stop"`
		SessionID string `json:"session_id"`
		User      string `json:"user"`
	}

	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Parse optional session ID encoded in model name: "gpt5.5:my-session"
	modelKey, modelSessionID := parseModelSessionID(req.Model)
	cfg, ok := api.resolveModel(w, modelKey)
	if !ok {
		return
	}

	// Convert FIM to chat format
	messages := api.fimToChat(req.Prompt, req.Suffix)

	// Inject simulated tool prompt if tool calling is enabled
	if len(toolcalling.RouteableTools(req.Tools)) > 0 {
		// A FIM completion carries no tool history, so there is no evidence.
		injectSimulatedPrompt(&messages, string(bodyBytes), toolChoiceString(req.ToolChoice), "")
	}

	// Resolve session ID and conversation ID
	sid := resolveSessionID(r, sessionSources{
		ModelSuffix:   modelSessionID,
		BodySessionID: req.SessionID,
		BodyUser:      req.User,
	})
	if sid == "" {
		sid = api.hashSessionIDFromMessages(r, messages)
	}
	var convID string
	if sid != "" {
		convID = api.ctxCache.Get(sessionKeyPrefix + sid)
	}

	hasTools := len(toolcalling.RouteableTools(req.Tools)) > 0

	stopSequences := openAIStopSequences(req.Stop)

	if req.Stream {
		api.streamCompletions(r.Context(), w, messages, cfg, req.MaxTokens, sid, convID, hasTools, req.Tools, toolChoiceString(req.ToolChoice), stopSequences, includeStreamUsage(req.StreamOptions))
	} else {
		api.nonStreamCompletions(w, messages, cfg, req.MaxTokens, sid, convID, hasTools, req.Tools, toolChoiceString(req.ToolChoice), stopSequences)
	}
}

func normalizeAnthropicSystem(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}

	var systemText string
	if err := json.Unmarshal(raw, &systemText); err == nil {
		return systemText, nil
	}

	var blocks []struct {
		Type string
		Text string
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", err
	}

	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}

	return strings.Join(parts, "\n\n"), nil
}

// handleAnthropicCountTokens handles Anthropic token counting requests.
func (api *APIServer) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	limitRequestBody(w, r, requestBodyMax)
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages json.RawMessage `json:"messages"`
		Tools    json.RawMessage `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.sendRequestBodyError(w, err)
		return
	}
	if len(req.Messages) == 0 || string(req.Messages) == "null" {
		api.sendError(w, http.StatusBadRequest, "messages is required")
		return
	}

	countable, err := json.Marshal(struct {
		System   json.RawMessage `json:"system,omitempty"`
		Messages json.RawMessage `json:"messages"`
		Tools    json.RawMessage `json:"tools,omitempty"`
	}{
		System:   req.System,
		Messages: req.Messages,
		Tools:    req.Tools,
	})
	if err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid token input: %v", err))
		return
	}

	api.sendJSON(w, http.StatusOK, map[string]int{"input_tokens": countTokens(string(countable))})
}

// handleAnthropicMessages handles Anthropic messages API requests.
// anthropicMessagesRequest is the wire shape of a /v1/messages request.
type anthropicMessagesRequest struct {
	Model       string                `json:"model"`
	Messages    []payload.Message     `json:"messages"`
	System      json.RawMessage       `json:"system"`
	MaxTokens   int                   `json:"max_tokens"`
	Stream      bool                  `json:"stream"`
	Temperature float64               `json:"temperature"`
	Tools       []toolcalling.ToolDef `json:"tools"`
	ToolChoice  map[string]any        `json:"tool_choice"`
	// Anthropic declares stop_sequences as an array of strings, unlike
	// OpenAI's stop, which is also a bare string.
	StopSequences []string `json:"stop_sequences"`
	SessionID     string   `json:"session_id"`
	User          string   `json:"user"`
}

func (api *APIServer) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	var req anthropicMessagesRequest
	bodyBytes, ok := api.readJSONRequest(w, r, &req, "handleAnthropicMessages")
	if !ok {
		return
	}

	// Parse optional session ID encoded in model name: "gpt5.5:my-session"
	modelKey, modelSessionID := parseModelSessionID(req.Model)
	// Map Anthropic model to internal model
	cfg, ok := api.resolveModel(w, modelKey)
	if !ok {
		return
	}
	logging.Infof("handleAnthropicMessages: model=%s stream=%v tools=%d sid=%s", modelKey, req.Stream, len(req.Tools), modelSessionID)

	ledger, ok := api.prepareToolLedger(w, req.Messages, "handleAnthropicMessages")
	if !ok {
		return
	}

	chatMessages, ok := api.anthropicChatMessages(w, req)
	if !ok {
		return
	}

	preparedTools, localTools := api.prepareCodingTools(req.Tools, true)
	req.Tools = preparedTools
	requestJSON := replaceRequestTools(bodyBytes, req.Tools)
	hasTools := len(toolcalling.RouteableTools(req.Tools)) > 0
	if hasTools {
		injectSimulatedPromptAnthropic(&chatMessages, requestJSON, anthropicToolChoiceString(req.ToolChoice), ledger.EvidenceNote())
	}

	sid, convID := api.sessionAndConversation(r, sessionSources{
		ModelSuffix:   modelSessionID,
		BodySessionID: req.SessionID,
		BodyUser:      req.User,
	}, chatMessages)

	// Upload any images found in multimodal content and attach annotations
	api.uploadImagesAndAnnotate(&chatMessages, convID)

	noParallel := anthropicRefusesParallelToolCalls(req.ToolChoice)

	if len(localTools) > 0 {
		api.runAnthropicToolLoop(w, r, req, chatMessages, cfg, sid, convID, localTools, noParallel)
		return
	}
	if req.Stream {
		api.streamAnthropicMessages(r.Context(), w, chatMessages, cfg, req.Model, req.MaxTokens, sid, convID, hasTools, req.Tools, anthropicToolChoiceEnforcement(req.ToolChoice), req.StopSequences, noParallel)
		return
	}
	api.nonStreamAnthropicMessages(w, chatMessages, cfg, req.Model, req.MaxTokens, sid, convID, hasTools, req.Tools, anthropicToolChoiceEnforcement(req.ToolChoice), req.StopSequences, noParallel)
}

// anthropicChatMessages prepends the system prompt to the turn. Claude Code can
// send Anthropic system as either a string or an array of text content blocks.
// ok is false when the request has already been answered.
func (api *APIServer) anthropicChatMessages(w http.ResponseWriter, req anthropicMessagesRequest) ([]payload.Message, bool) {
	systemPrompt, err := normalizeAnthropicSystem(req.System)
	if err != nil {
		logging.Errorf("handleAnthropicMessages: invalid system field: %v", err)
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid system field: %v", err))
		return nil, false
	}

	chatMessages := []payload.Message{}
	if systemPrompt != "" {
		chatMessages = append(chatMessages, payload.Message{Role: "system", Content: systemPrompt})
	}
	return append(chatMessages, req.Messages...), true
}

// runAnthropicToolLoop runs the built-in coding tool loop for an Anthropic
// request, which answers whole because the loop takes several turns.
func (api *APIServer) runAnthropicToolLoop(w http.ResponseWriter, r *http.Request, req anthropicMessagesRequest, chatMessages []payload.Message, cfg models.ModelConfig, sid, convID string, localTools map[string]bool, noParallel bool) {
	result, err := api.runToolLoop(r, toolLoopAnthropic, chatMessages, cfg, sid, convID, req.Tools, noParallel, localTools)
	if err != nil {
		api.sendUpstreamError(w, "chat", err)
		return
	}
	api.respondBufferedAnthropic(w, result, chatMessages, req.Model, req.MaxTokens, req.Stream, req.Tools, anthropicToolChoiceEnforcement(req.ToolChoice), req.StopSequences)
}

// handleAnthropicComplete handles Anthropic complete (FIM) requests.
func (api *APIServer) handleAnthropicComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		Model             string   `json:"model"`
		Prompt            string   `json:"prompt"`
		MaxTokensToSample int      `json:"max_tokens_to_sample"`
		Stream            bool     `json:"stream"`
		StopSequences     []string `json:"stop_sequences"`
		SessionID         string   `json:"session_id"`
		User              string   `json:"user"`
	}

	limitRequestBody(w, r, requestBodyMax)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.sendRequestBodyError(w, err)
		return
	}

	// Parse optional session ID encoded in model name: "gpt5.5:my-session"
	modelKey, modelSessionID := parseModelSessionID(req.Model)
	cfg, ok := api.resolveModel(w, modelKey)
	if !ok {
		return
	}

	messages := api.fimToChat(req.Prompt, "")

	// Resolve session ID and conversation ID
	sid := resolveSessionID(r, sessionSources{
		ModelSuffix:   modelSessionID,
		BodySessionID: req.SessionID,
		BodyUser:      req.User,
	})
	if sid == "" {
		sid = api.hashSessionIDFromMessages(r, messages)
	}
	var convID string
	if sid != "" {
		convID = api.ctxCache.Get(sessionKeyPrefix + sid)
	}

	if req.Stream {
		api.streamAnthropicComplete(r.Context(), w, messages, cfg, req.Model, req.MaxTokensToSample, req.StopSequences, sid, convID)
	} else {
		api.nonStreamAnthropicComplete(w, messages, cfg, req.Model, req.MaxTokensToSample, req.StopSequences, sid, convID)
	}
}

// nonStreamAnthropicComplete handles non-streaming Anthropic complete (FIM) requests.
func (api *APIServer) nonStreamAnthropicComplete(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, model string, maxTokens int, stopSequences []string, sid, convID string) {
	respText, thinking, _, _, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, false)
	if err != nil {
		api.sendUpstreamError(w, "completion", err)
		return
	}
	respText = api.routeGeneratedImages(respText)

	// The completion ends where the caller said it ends. Reporting
	// stop_sequence while still returning the text past the sequence would
	// contradict the response's own stop_reason.
	stopReason := "end_turn"
	cut, matchedStop := cutAtStopSequence(respText, stopSequences)
	if matchedStop != "" {
		respText = cut
		stopReason = "stop_sequence"
	}

	// Enforce max_tokens_to_sample on response text
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(respText, maxTokens); ok {
			respText = truncated
			stopReason = "max_tokens"
		}
	}

	// The legacy Complete format defines no usage object. It is reported anyway
	// so a caller reads the same counts here as on every other endpoint; a
	// client written to the original format ignores the extra field.
	response := map[string]any{
		"completion":  respText,
		"stop_reason": stopReason,
		"model":       model,
		// The Complete format names the sequence that ended the answer here.
		// It stays null when the answer ended on its own.
		"stop":   nullableString(matchedStop),
		"log_id": fmt.Sprintf("cmpl_%s", uuid.New().String()),
		"usage":  anthropicUsage(messages, nil, "", respText, thinking),
	}

	api.storeSessionMapping(sid, finalConvID)
	api.sendJSON(w, http.StatusOK, response)
}

// streamAnthropicComplete streams Anthropic complete (FIM) responses.
// Anthropic Complete streaming uses SSE with event: completion and
// data containing {"type":"completion","completion":"<delta>","stop_reason":null}.
// The final event has stop_reason set and completion empty.
func (api *APIServer) streamAnthropicComplete(ctx context.Context, w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, model string, maxTokens int, stopSequences []string, sid, convID string) {
	flusher, ok := api.beginSSE(w)
	if !ok {
		return
	}

	stream := &completeStream{
		api:     api,
		w:       w,
		flusher: flusher,
		model:   model,
		logID:   fmt.Sprintf("cmpl_%s", uuid.New().String()),
	}

	// Send ping event (Anthropic streaming starts with ping)
	pingJSON, _ := json.Marshal(map[string]any{"type": "ping"})
	_, _ = fmt.Fprintf(w, "event: ping\ndata: %s\n\n", pingJSON)
	flusher.Flush()

	ch := api.m365Client.ChatConversationStreamGenContext(ctx, messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, false)

	// A stop sequence can straddle two chunks, so the deltas pass through a
	// writer that holds back the tail which could still complete one. Without
	// it the first half of the sequence would already be on the wire by the
	// time the completion is known to have ended.
	stopWriter := newStopSequenceWriter(stopSequences)

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	for {
		chunk, more := nextStreamChunk(ctx, ch, keepalive, w, flusher, func() error { return writeAnthropicKeepalive(w, flusher) })
		if !more {
			break
		}
		step := stream.chunk(chunk, ch, maxTokens, stopWriter)
		if step == streamLoopFailed {
			return
		}
		if step == streamLoopStop {
			break
		}
	}

	// Whatever the writer held back belongs to the answer when no stop sequence
	// ever arrived.
	stream.delta(stopWriter.flush())

	// Determine stop reason
	stopReason := "end_turn"
	if stopWriter.stoppedEarly() {
		stopReason = "stop_sequence"
	}
	if stream.truncated {
		stopReason = "max_tokens"
	}

	// Send final completion event with stop_reason
	finalData := map[string]any{
		"type":        "completion",
		"completion":  "",
		"stop_reason": stopReason,
		"model":       model,
		"stop":        nullableString(stopWriter.matched()),
		"log_id":      stream.logID,
		// The intermediate events carry deltas, so usage belongs on the last
		// one. The legacy Complete format defines no such field; it is reported
		// so a caller reads the same counts here as on every other endpoint.
		"usage": anthropicUsage(messages, nil, "", stream.fullText.String(), stream.thinking.String()),
	}
	api.storeSessionMapping(sid, stream.convID)

	finalJSON, _ := json.Marshal(finalData)
	_, _ = fmt.Fprintf(w, "event: completion\ndata: %s\n\n", finalJSON)
	flusher.Flush()
}

// beginSSE writes the event-stream headers and reports the flusher every
// streaming responder needs. ok is false when the request has already been
// answered, which happens when the writer cannot flush.
func (api *APIServer) beginSSE(w http.ResponseWriter) (http.Flusher, bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		api.sendError(w, http.StatusInternalServerError, "Streaming not supported")
		return nil, false
	}
	return flusher, true
}

// drainStream reads the rest of an upstream turn, so its goroutine finishes
// rather than blocking on a channel nobody reads.
func drainStream(ch <-chan client.StreamChunk) {
	for range ch {
	}
}

// streamLoopStep says what a streaming responder does after one chunk.
type streamLoopStep int

const (
	// streamLoopContinue keeps reading chunks.
	streamLoopContinue streamLoopStep = iota
	// streamLoopStop ends the turn and writes its final event.
	streamLoopStop
	// streamLoopFailed means the error event was already written.
	streamLoopFailed
)

// completeStream carries what one legacy Complete turn accumulates.
type completeStream struct {
	api     *APIServer
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	logID   string

	fullText  strings.Builder
	thinking  strings.Builder
	truncated bool
	convID    string
}

// delta writes one completion event and records the text it carried.
func (s *completeStream) delta(text string) {
	if text == "" {
		return
	}
	s.fullText.WriteString(text)
	compJSON, _ := json.Marshal(map[string]any{
		"type":        "completion",
		"completion":  text,
		"stop_reason": nil,
		"model":       s.model,
		"log_id":      s.logID,
	})
	_, _ = fmt.Fprintf(s.w, "event: completion\ndata: %s\n\n", compJSON)
	s.flusher.Flush()
}

// chunk reads one upstream chunk of a Complete turn.
func (s *completeStream) chunk(chunk client.StreamChunk, ch <-chan client.StreamChunk, maxTokens int, stopWriter *stopSequenceWriter) streamLoopStep {
	if chunk.Error != nil {
		// The classification rather than the transport error, which names
		// request URLs and credential file paths.
		_, code, message := streamErrorFields("complete", chunk.Error)
		errJSON, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": code, "message": message},
		})
		_, _ = fmt.Fprintf(s.w, "event: error\ndata: %s\n\n", errJSON)
		s.flusher.Flush()
		return streamLoopFailed
	}

	if chunk.IsFinal {
		s.convID = chunk.ConversationID
		return streamLoopStop
	}

	chunk.Text = s.api.routeGeneratedImages(chunk.Text)

	// The Complete wire format carries no thinking block, so reasoning is
	// counted for the usage object but never emitted.
	if chunk.Thinking != "" {
		s.thinking.WriteString(chunk.Thinking)
		return streamLoopContinue
	}

	// Check max_tokens limit
	if maxTokens > 0 && countTokens(s.fullText.String()) >= maxTokens {
		s.truncated = true
		drainStream(ch)
		return streamLoopStop
	}

	// Once a stop sequence is reached the writer releases nothing more, but the
	// loop keeps reading so the final frame still names the conversation this
	// turn belongs to.
	emit, _ := stopWriter.next(chunk.Text)
	s.delta(emit)
	return streamLoopContinue
}

// streamChatCompletions streams chat completion responses in OpenAI format.
func (api *APIServer) streamChatCompletions(ctx context.Context, w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxTokens int, hasTools bool, tools []toolcalling.ToolDef, toolChoice string, stopSequences []string, noParallel, includeUsage bool) {
	flusher, ok := api.beginSSE(w)
	if !ok {
		return
	}

	// Commit the response headers before the upstream turn starts. The first
	// chunk can take almost ten seconds on a tool-enabled turn, and a client
	// that sees no bytes at all cannot tell a slow provider from a dead one.
	refreshStreamDeadline(w)
	if err := writeSSEKeepalive(w, flusher); err != nil {
		return
	}

	stream := &chatStream{
		api:     api,
		w:       w,
		flusher: flusher,
		chunkID: fmt.Sprintf("chatcmpl-%s", uuid.New().String()),
		model:   cfg.OpenAIID,
	}

	ch := api.m365Client.ChatConversationStreamGenContext(ctx, messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)

	// A stop sequence can straddle two chunks, so the deltas of a directly
	// streamed answer pass through a writer that holds back the tail which
	// could still complete one. A tool-enabled turn is buffered whole, so it
	// takes the trailing cut below instead.
	stopWriter := newStopSequenceWriter(stopSequences)

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	for {
		chunk, more := nextStreamChunk(ctx, ch, keepalive, w, flusher, func() error { return writeSSEKeepalive(w, flusher) })
		if !more {
			break
		}
		step := stream.chunk(chunk, ch, maxTokens, hasTools, stopWriter, sid)
		if step == streamLoopFailed {
			return
		}
		if step == streamLoopStop {
			break
		}
	}
	if !hasTools {
		// Whatever the writer held back belongs to the answer when no stop
		// sequence ever arrived.
		stream.delta(stopWriter.flush())
	}

	fullText, simToolCalls := api.chatToolCalls(stream, messages, cfg, tools, toolChoice, stopSequences, hasTools, noParallel)

	// Send tool calls in stream if any (from M365 backend or simulated)
	toolCalls, _ := withoutBackendToolCalls(stream.backendCalls, "")
	toolCalls = appendSimulatedCalls(toolCalls, simToolCalls)
	stream.emitToolCalls(toolCalls)

	api.updateChatStreamSession(sid, cfg.OpenAIID, stream.convID, fullText, stream.thinking.String(), toolCalls)

	// Send final chunk with usage
	api.sendSSEDone(w, stream.chunkID, stream.model, streamFinishReason(stream.truncated, toolCalls),
		chatStreamUsage(includeUsage, messages, tools, toolChoice, fullText, stream.thinking.String()))
	flusher.Flush()
}

// chatStream carries what one OpenAI chat completion turn streams.
//
// When tool calling is enabled AND tools are present, all text is buffered and
// parsed for tool calls at the end: a tool call block may span several chunks,
// so it cannot be parsed incrementally. With no tools the text streams directly.
type chatStream struct {
	api     *APIServer
	w       http.ResponseWriter
	flusher http.Flusher
	chunkID string
	model   string

	hasContent     bool
	fullText       strings.Builder
	thinking       strings.Builder
	thinkingFilter toolcalling.ThinkingStreamFilter
	truncated      bool
	convID         string
	backendCalls   []client.ToolCall
}

// send writes one delta, naming the assistant role on the first one the client
// receives.
func (s *chatStream) send(delta map[string]any) {
	if !s.hasContent {
		delta["role"] = "assistant"
		s.hasContent = true
	}
	s.api.sendSSEChunk(s.w, s.chunkID, s.model, delta)
	s.flusher.Flush()
}

// delta writes one content delta and records the text it carried.
func (s *chatStream) delta(text string) {
	if text == "" {
		return
	}
	s.fullText.WriteString(text)
	s.send(map[string]any{"content": text})
}

// reasoning writes one reasoning delta.
func (s *chatStream) reasoning(text string) {
	if text == "" {
		return
	}
	s.send(map[string]any{"reasoning_content": text})
}

// streamThinking emits the reasoning summary as it arrives. A tool-enabled turn
// filters it, because the transport envelope travels in the same channel.
func (s *chatStream) streamThinking(text string, hasTools bool) {
	s.thinking.WriteString(text)
	if !hasTools {
		s.reasoning(text)
		return
	}
	// Live-stream filtered thinking so the transport envelope never leaks,
	// matching the Anthropic streaming path.
	s.reasoning(s.thinkingFilter.Feed(text))
}

// emitToolCalls writes the calls of a turn. They carry no content of their own,
// so a turn that produced nothing else names the role on a null content delta.
func (s *chatStream) emitToolCalls(toolCalls []client.ToolCall) {
	if len(toolCalls) == 0 {
		return
	}
	if !s.hasContent {
		s.api.sendSSEChunk(s.w, s.chunkID, s.model, map[string]any{
			"role":    "assistant",
			"content": nil,
		})
	}
	for i, tc := range toolCalls {
		s.api.sendSSEChunk(s.w, s.chunkID, s.model, map[string]any{
			"tool_calls": []map[string]any{
				{
					"index": i,
					"id":    tc.ID,
					"type":  "function",
					"function": map[string]string{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					},
				},
			},
		})
	}
	s.flusher.Flush()
}

// chunk reads one upstream chunk of a chat completion turn.
func (s *chatStream) chunk(chunk client.StreamChunk, ch <-chan client.StreamChunk, maxTokens int, hasTools bool, stopWriter *stopSequenceWriter, sid string) streamLoopStep {
	if chunk.Error != nil {
		s.api.forgetSession(sid)
		s.api.sendSSEError(s.w, "chat", s.chunkID, s.model, chunk.Error)
		return streamLoopFailed
	}
	if chunk.IsFinal {
		s.convID = chunk.ConversationID
		s.backendCalls = chunk.ToolCalls
		return streamLoopStop
	}

	// A generated image arrives as a link to an address the browser cannot
	// fetch, so it is rewritten to a route on this gateway before it goes out.
	// The link arrives as a chunk of its own, so it is never split.
	chunk.Text = s.api.routeGeneratedImages(chunk.Text)

	// Send thinking as reasoning_content (OpenAI extended thinking format)
	if chunk.Thinking != "" {
		s.streamThinking(chunk.Thinking, hasTools)
		return streamLoopContinue
	}

	// Check max_tokens limit before sending more content
	if maxTokens > 0 && countTokens(s.fullText.String()) >= maxTokens {
		s.truncated = true
		drainStream(ch)
		return streamLoopStop
	}

	// If tool calling is not enabled, stream text directly. The writer decides
	// what is safe to release, and it also feeds the accumulator, so the
	// recorded answer matches what the client received.
	if !hasTools {
		emit, _ := stopWriter.next(chunk.Text)
		s.delta(emit)
		return streamLoopContinue
	}
	s.fullText.WriteString(chunk.Text)
	return streamLoopContinue
}

// chatToolCalls parses the simulated tool calls out of a buffered tool-enabled
// turn and emits whatever text is left as one chunk. A turn with no tools
// streamed its text already and passes through untouched.
func (api *APIServer) chatToolCalls(stream *chatStream, messages []payload.Message, cfg models.ModelConfig, tools []toolcalling.ToolDef, toolChoice string, stopSequences []string, hasTools, noParallel bool) (string, []toolcalling.ToolCall) {
	fullText := stream.fullText.String()
	if !hasTools {
		return fullText, nil
	}

	var simToolCalls []toolcalling.ToolCall
	contracts := toolcalling.ContractsFor(tools).WithChoice(toolChoice).WithoutParallel(noParallel)
	sim := toolcalling.ParseSimulatedResponse(fullText, toolNamesFromDefs(tools), contracts)
	sim = api.repairSimulatedToolCalls(toolLoopOpenAI, messages, cfg, tools, contracts, fullText, sim)
	sim = dropSettledToolCalls(buildToolLedger(messages), toolChoice, sim)
	switch {
	case !sim.HasPayload:
		fullText = toolcalling.WithholdTransportEnvelope(fullText)
		if toolcalling.IsContentPolicyBlock(fullText) {
			// The stream is already open, so the refusal cannot be turned into
			// an HTTP error the way the non-streaming paths do.
			logging.Warn("upstream content refusal on a streaming turn: M365 declined the request instead of answering")
		}
	case len(sim.ToolCalls) > 0:
		simToolCalls = sim.ToolCalls
		fullText = ""
	default:
		fullText = sim.Content
	}

	fullText = replaceUnverifiedCompletionClaim(fullText, hasTools, buildToolLedger(messages), len(simToolCalls))
	// A tool-enabled turn is buffered whole and emitted here, so its stop
	// sequence is applied now rather than through the writer.
	if cut, matched := cutAtStopSequence(fullText, stopSequences); matched != "" {
		fullText = cut
	}

	// Whatever the thinking filter held back is released before the answer.
	stream.reasoning(stream.thinkingFilter.Flush())

	// If tool calling buffered text, send it now as a single chunk
	if fullText != "" && len(simToolCalls) == 0 {
		stream.send(map[string]any{"content": fullText})
	}
	return fullText, simToolCalls
}

// appendSimulatedCalls converts the parsed calls into the shape the responders
// emit.
func appendSimulatedCalls(toolCalls []client.ToolCall, simToolCalls []toolcalling.ToolCall) []client.ToolCall {
	for _, stc := range simToolCalls {
		toolCalls = append(toolCalls, client.ToolCall{
			ID:       stc.ID,
			Type:     "function",
			Function: client.ToolCallFunction{Name: stc.Name, Namespace: stc.Namespace, Arguments: string(stc.Arguments)},
		})
	}
	return toolCalls
}

// streamFinishReason names why a streamed turn ended. A turn that produced
// tool calls reports them even when it also hit the token ceiling.
func streamFinishReason(truncated bool, toolCalls []client.ToolCall) string {
	if len(toolCalls) > 0 {
		return "tool_calls"
	}
	if truncated {
		return "length"
	}
	return "stop"
}

// chatStreamUsage renders the usage object when the caller asked for one.
func chatStreamUsage(includeUsage bool, messages []payload.Message, tools []toolcalling.ToolDef, toolChoice, fullText, thinking string) map[string]any {
	if !includeUsage {
		return nil
	}
	promptTok := countPromptTokens(messages, tools, toolChoice)
	completionTok := countTokens(fullText) + outputProtocolTokens
	reasoningTok := countTokens(thinking)
	return map[string]any{
		"prompt_tokens":     promptTok,
		"completion_tokens": completionTok,
		"reasoning_tokens":  reasoningTok,
		"total_tokens":      promptTok + completionTok + reasoningTok,
		"usage_source":      usageSource(),
	}
}

// storeSessionMapping records the conversation a session now points at.
//
// Every responder calls this before it writes the end of its response, never
// after. A client that reads the terminator and immediately asks about its
// session would otherwise be told the session does not exist, and would treat
// the turn it just completed as one that started no conversation.
func (api *APIServer) storeSessionMapping(sid, finalConvID string) {
	if sid == "" || finalConvID == "" {
		return
	}
	api.ctxCache.Set(sessionKeyPrefix+sid, finalConvID)
}

func (api *APIServer) updateChatStreamSession(sid, model, finalConvID, fullText, thinkingText string, toolCalls []client.ToolCall) {
	if sid == "" {
		return
	}

	if strings.TrimSpace(fullText) == "" &&
		strings.TrimSpace(thinkingText) == "" &&
		len(toolCalls) == 0 {
		api.ctxCache.Delete(sessionKeyPrefix + sid)
		// The session now starts a new conversation, so the stored turns
		// belong to a conversation this id no longer points at.
		api.dropTranscript(sid)
		return
	}

	api.storeSessionMapping(sid, finalConvID)
	api.recordAssistantTurn(sid, model, fullText, thinkingText)
}

func (api *APIServer) respondBufferedChat(w http.ResponseWriter, result toolLoopResult, messages []payload.Message, cfg models.ModelConfig, sid string, maxTokens int, stream bool, tools []toolcalling.ToolDef, toolChoice string, stopSequences []string) {
	// A stop sequence ends the answer where the caller said it ends. OpenAI
	// reports that as the ordinary "stop", the same as an answer that ended on
	// its own, so only the text changes.
	if cut, matched := cutAtStopSequence(result.text, stopSequences); matched != "" {
		result.text, result.finishReason = cut, "stop"
	}
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(result.text, maxTokens); ok {
			result.text, result.finishReason = truncated, "length"
		}
	}
	api.recordAssistantTurn(sid, cfg.OpenAIID, result.text, result.thinking)
	usage := openAIUsage(messages, tools, toolChoice, result.text, result.thinking)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		id := fmt.Sprintf("chatcmpl-%s", uuid.New().String())
		if result.text != "" {
			api.sendSSEChunk(w, id, cfg.OpenAIID, map[string]any{"role": "assistant", "content": result.text})
		}
		for i, call := range result.toolCalls {
			api.sendSSEChunk(w, id, cfg.OpenAIID, map[string]any{"tool_calls": []map[string]any{{"index": i, "id": call.ID, "type": "function", "function": map[string]string{"name": call.Function.Name, "arguments": call.Function.Arguments}}}})
		}
		api.sendSSEDone(w, id, cfg.OpenAIID, result.finishReason, usage)
		return
	}
	message := map[string]any{"role": "assistant", "content": result.text}
	if len(result.toolCalls) > 0 {
		message["content"] = nil
		message["tool_calls"] = result.toolCalls
	}
	api.sendJSON(w, http.StatusOK, map[string]any{"id": fmt.Sprintf("chatcmpl-%s", uuid.New().String()), "object": "chat.completion", "created": time.Now().Unix(), "model": cfg.OpenAIID, "choices": []map[string]any{{"index": 0, "message": message, "finish_reason": result.finishReason}}, "usage": usage})
}

// nonStreamChatCompletions handles non-streaming chat completion in OpenAI format.
func (api *APIServer) nonStreamChatCompletions(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxTokens int, hasTools bool, tools []toolcalling.ToolDef, toolChoice string, stopSequences []string, noParallel bool) {
	respText, thinking, toolCalls, finishReason, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
	if err != nil {
		api.forgetSession(sid)
		api.sendUpstreamError(w, "chat", err)
		return
	}
	respText = api.routeGeneratedImages(respText)

	toolCalls, finishReason = withoutBackendToolCalls(toolCalls, finishReason)
	// The transport thinking filter belongs to simulated mode only, where the
	// envelope travels inside the reasoning text.
	if len(tools) > 0 {
		thinking = chatAnthropicThinkingForOutput(thinking, true)
	}

	// Parse simulated tool calls from response text if tool calling is enabled
	if hasTools {
		respText, toolCalls, finishReason = api.applySimulatedToolCalls(
			toolLoopOpenAI, messages, cfg, tools, toolChoice, noParallel, respText, toolCalls,
		)
	}

	if blockedByContentPolicy(respText, toolCalls) {
		api.forgetSession(sid)
		api.sendContentBlockedError(w, respText)
		return
	}
	respText = withoutUnverifiedCompletionClaim(respText, hasTools, buildToolLedger(messages), toolCalls)
	respText, finishReason = cutChatAnswer(respText, finishReason, stopSequences, maxTokens)

	msg := openAIAssistantMessage(respText, thinking, toolCalls)

	promptTok := countPromptTokens(messages, tools, toolChoice)
	completionTok := countTokens(respText) + outputProtocolTokens
	reasoningTok := countTokens(thinking)
	response := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%s", uuid.New().String()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   cfg.OpenAIID,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     promptTok,
			"completion_tokens": completionTok,
			"reasoning_tokens":  reasoningTok,
			"total_tokens":      promptTok + completionTok + reasoningTok,
			"usage_source":      usageSource(),
		},
	}

	api.storeSessionMapping(sid, finalConvID)
	api.recordAssistantTurn(sid, cfg.OpenAIID, respText, thinking)
	api.sendJSON(w, http.StatusOK, response)
}

// forgetSession drops a session's conversation binding, so the next turn does
// not continue a conversation this one could not use.
func (api *APIServer) forgetSession(sid string) {
	if sid != "" {
		api.ctxCache.Delete(sessionKeyPrefix + sid)
	}
}

// applySimulatedToolCalls parses the simulated tool calls out of the answer in
// the provider's own shape, and reports the text, the calls and the finish
// reason that follow from it.
func (api *APIServer) applySimulatedToolCalls(provider toolLoopProvider, messages []payload.Message, cfg models.ModelConfig, tools []toolcalling.ToolDef, toolChoice string, noParallel bool, respText string, toolCalls []client.ToolCall) (string, []client.ToolCall, string) {
	contracts := toolcalling.ContractsFor(tools).WithChoice(toolChoice).WithoutParallel(noParallel)
	sim := parseLoopSimulation(provider, respText, tools, contracts)
	sim = api.repairSimulatedToolCalls(provider, messages, cfg, tools, contracts, respText, sim)
	sim = dropSettledToolCalls(buildToolLedger(messages), toolChoice, sim)

	if !sim.HasPayload {
		// M365 did not return a simulated JSON payload (e.g. it ran its own
		// server-side tools and returned plain text). Since the backend-injected
		// tool calls were discarded above, reset the finish reason so we do not
		// report tool_use with no blocks.
		return toolcalling.WithholdTransportEnvelope(respText), toolCalls, "stop"
	}
	if len(sim.ToolCalls) == 0 {
		return sim.Content, toolCalls, "stop"
	}
	for _, pc := range sim.ToolCalls {
		toolCalls = append(toolCalls, client.ToolCall{
			ID:       pc.ID,
			Type:     "function",
			Function: client.ToolCallFunction{Name: pc.Name, Namespace: pc.Namespace, Arguments: string(pc.Arguments)},
		})
	}
	return "", toolCalls, "tool_calls"
}

// cutChatAnswer applies the caller's stop sequence and max_tokens.
//
// A stop sequence ends the answer where the caller said it ends. OpenAI reports
// that as the ordinary "stop", the same as an answer that ended on its own, so
// only the text changes. max_tokens wins when it is reached first.
func cutChatAnswer(respText, finishReason string, stopSequences []string, maxTokens int) (string, string) {
	if cut, matched := cutAtStopSequence(respText, stopSequences); matched != "" {
		respText = cut
		finishReason = "stop"
	}
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(respText, maxTokens); ok {
			respText = truncated
			finishReason = "length"
		}
	}
	return respText, finishReason
}

// openAIAssistantMessage builds the assistant message of a chat completion. A
// turn that produced only tool calls carries a null content, which is what the
// OpenAI schema declares.
func openAIAssistantMessage(respText, thinking string, toolCalls []client.ToolCall) map[string]any {
	msg := map[string]any{
		"role":    "assistant",
		"content": respText,
	}
	if thinking != "" {
		msg["reasoning_content"] = thinking
	}
	if len(toolCalls) == 0 {
		return msg
	}

	openaiToolCalls := make([]map[string]any, len(toolCalls))
	for i, tc := range toolCalls {
		openaiToolCalls[i] = map[string]any{
			"index": i,
			"id":    tc.ID,
			"type":  "function",
			"function": map[string]string{
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			},
		}
	}
	msg["tool_calls"] = openaiToolCalls
	if respText == "" {
		msg["content"] = nil
	}
	return msg
}

// streamAnthropicMessages streams messages in Anthropic SSE format.
func (api *APIServer) streamAnthropicMessages(ctx context.Context, w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, anthropicModel string, maxTokens int, sid, convID string, hasTools bool, tools []toolcalling.ToolDef, toolChoice string, stopSequences []string, noParallel bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		api.sendError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	msgID := fmt.Sprintf("msg_%s", uuid.New().String())
	// The Anthropic wire format splits usage across message_start and
	// message_delta, so the input side is counted once here and not repeated.
	promptTok := countPromptTokens(messages, tools, toolChoice)

	// Send message_start event
	header := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         anthropicModel,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  promptTok,
				"output_tokens": 0,
				"usage_source":  usageSource(),
			},
		},
	}
	api.sendAnthropicSSE(w, "message_start", header)
	flusher.Flush()

	// Stream content with optional thinking block
	var fullTextBuilder strings.Builder
	var thinkingText strings.Builder
	var thinkingFilter toolcalling.ThinkingStreamFilter
	thinkingClosed := false
	truncated := false
	thinkingBlockOpen := false
	textBlockOpen := false
	blockIndex := 0
	toolCallingEnabled := hasTools

	// A stop sequence can straddle two chunks, so the deltas of a directly
	// streamed answer pass through a writer that holds back the tail which
	// could still complete one. A tool-enabled turn is buffered whole, so it
	// takes the trailing cut below instead.
	stopWriter := newStopSequenceWriter(stopSequences)
	sendTextDelta := func(text string) {
		if text == "" {
			return
		}
		fullTextBuilder.WriteString(text)
		api.sendAnthropicSSE(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
		flusher.Flush()
	}

	ch := api.m365Client.ChatConversationStreamGenContext(ctx, messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)

	var finalConvID string
	var finalToolCalls []client.ToolCall
	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	for {
		chunk, more := nextStreamChunk(ctx, ch, keepalive, w, flusher, func() error { return writeAnthropicKeepalive(w, flusher) })
		if !more {
			break
		}
		if chunk.Error != nil {
			if sid != "" {
				api.ctxCache.Delete(sessionKeyPrefix + sid)
			}
			_, code, message := streamErrorFields("message", chunk.Error)
			errEvent := map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    code,
					"message": message,
				},
			}
			api.sendAnthropicSSE(w, "error", errEvent)
			flusher.Flush()
			return
		}

		if chunk.IsFinal {
			finalConvID = chunk.ConversationID
			finalToolCalls = chunk.ToolCalls
			break
		}

		chunk.Text = api.routeGeneratedImages(chunk.Text)

		// Handle thinking content
		if chunk.Thinking != "" {
			thinkingText.WriteString(chunk.Thinking)
			if toolCallingEnabled {
				// Live-stream thinking through a stateful filter that strips the
				// simulated transport envelope (fenced blocks + meta-prose) so
				// the model's reasoning is visible without exposing the mechanism.
				if emit := thinkingFilter.Feed(chunk.Thinking); emit != "" {
					if !thinkingBlockOpen {
						api.sendAnthropicSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": blockIndex, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
						thinkingBlockOpen = true
					}
					api.sendAnthropicSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIndex, "delta": map[string]any{"type": "thinking_delta", "thinking": emit}})
					flusher.Flush()
				}
				continue
			}
			if !thinkingBlockOpen {
				cbStart := map[string]any{
					"type":          "content_block_start",
					"index":         blockIndex,
					"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
				}
				api.sendAnthropicSSE(w, "content_block_start", cbStart)
				thinkingBlockOpen = true
			}
			delta := map[string]any{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]any{"type": "thinking_delta", "thinking": chunk.Thinking},
			}
			api.sendAnthropicSSE(w, "content_block_delta", delta)
			flusher.Flush()
			continue
		}

		// Transition from thinking to text: flush the filter remainder (tool
		// path) then close any open thinking block before content follows.
		if toolCallingEnabled && !thinkingClosed {
			if rem := thinkingFilter.Flush(); rem != "" {
				if !thinkingBlockOpen {
					api.sendAnthropicSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": blockIndex, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
					thinkingBlockOpen = true
				}
				api.sendAnthropicSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIndex, "delta": map[string]any{"type": "thinking_delta", "thinking": rem}})
			}
			thinkingClosed = true
		}
		if thinkingBlockOpen && !textBlockOpen {
			api.sendAnthropicSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
			blockIndex++
			thinkingBlockOpen = false
		}

		// Open text block on first text chunk (only if not buffering for tool calling)
		if !textBlockOpen && !toolCallingEnabled {
			cbStart := map[string]any{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]any{"type": "text", "text": ""},
			}
			api.sendAnthropicSSE(w, "content_block_start", cbStart)
			textBlockOpen = true
		}

		// Check max_tokens limit before sending more content
		if maxTokens > 0 && countTokens(fullTextBuilder.String()) >= maxTokens {
			truncated = true
			for range ch {
			}
			break
		}

		// If tool calling is not enabled, stream text deltas directly. The
		// writer decides what is safe to release, and it also feeds the
		// accumulator, so the reported usage matches what the client received.
		if !toolCallingEnabled {
			emit, _ := stopWriter.next(chunk.Text)
			sendTextDelta(emit)
			continue
		}
		fullTextBuilder.WriteString(chunk.Text)
	}
	if !toolCallingEnabled {
		// Whatever the writer held back belongs to the answer when no stop
		// sequence ever arrived.
		sendTextDelta(stopWriter.flush())
	}
	fullText := fullTextBuilder.String()
	// Empty on the tool-enabled path, where the writer never ran and the cut
	// below sets it instead.
	matchedStop := stopWriter.matched()

	// Parse simulated tool calls from full text if tool calling is enabled
	var simToolCalls []toolcalling.ToolCall
	if toolCallingEnabled {
		contracts := toolcalling.ContractsFor(tools).WithChoice(toolChoice).WithoutParallel(noParallel)
		sim := toolcalling.ParseSimulatedResponseAnthropic(fullText, toolNamesFromDefs(tools), contracts)
		sim = api.repairSimulatedToolCalls(toolLoopAnthropic, messages, cfg, tools, contracts, fullText, sim)
		sim = dropSettledToolCalls(buildToolLedger(messages), toolChoice, sim)
		if sim.HasPayload {
			if len(sim.ToolCalls) > 0 {
				simToolCalls = sim.ToolCalls
				fullText = ""
			} else {
				fullText = sim.Content
			}
		} else {
			fullText = toolcalling.WithholdTransportEnvelope(fullText)
			if toolcalling.IsContentPolicyBlock(fullText) {
				// The stream is already open, so the refusal cannot be turned
				// into an HTTP error the way the non-streaming paths do.
				logging.Warn("upstream content refusal on a streaming turn: M365 declined the request instead of answering")
			}
		}
		fullText = replaceUnverifiedCompletionClaim(fullText, hasTools, buildToolLedger(messages), len(simToolCalls))
		// A tool-enabled turn is buffered whole and emitted below, so its stop
		// sequence is applied here rather than through the writer.
		if cut, matched := cutAtStopSequence(fullText, stopSequences); matched != "" {
			fullText = cut
			matchedStop = matched
		}
	}

	// Flush any remaining filtered thinking when the response was thinking-only
	// (no content chunk triggered the in-loop transition) and close its block.
	if toolCallingEnabled && !thinkingClosed {
		if rem := thinkingFilter.Flush(); rem != "" {
			if !thinkingBlockOpen {
				api.sendAnthropicSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": blockIndex, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
				thinkingBlockOpen = true
			}
			api.sendAnthropicSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIndex, "delta": map[string]any{"type": "thinking_delta", "thinking": rem}})
		}
		if thinkingBlockOpen {
			api.sendAnthropicSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
			blockIndex++
			thinkingBlockOpen = false
		}
	}

	// If tool calling buffered text, send it now as a text block
	if toolCallingEnabled && fullText != "" {
		cbStart := map[string]any{
			"type":          "content_block_start",
			"index":         blockIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		}
		api.sendAnthropicSSE(w, "content_block_start", cbStart)
		textBlockOpen = true
		delta := map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": fullText},
		}
		api.sendAnthropicSSE(w, "content_block_delta", delta)
		flusher.Flush()
	}

	// Close any open blocks
	if thinkingBlockOpen {
		api.sendAnthropicSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
		blockIndex++
	}
	if textBlockOpen {
		api.sendAnthropicSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
		blockIndex++
	}

	// Send tool_use content blocks if any (server-side tools from M365 backend or simulated)
	toolCalls, _ := withoutBackendToolCalls(finalToolCalls, "")

	// Append simulated tool calls
	for _, stc := range simToolCalls {
		toolCalls = append(toolCalls, client.ToolCall{
			ID:       stc.ID,
			Type:     "function",
			Function: client.ToolCallFunction{Name: stc.Name, Namespace: stc.Namespace, Arguments: string(stc.Arguments)},
		})
	}

	for _, tc := range toolCalls {
		// Anthropic streaming delivers tool_use input as input_json_delta
		// fragments, not inside content_block_start. SDK clients (e.g. Claude
		// Code) accumulate partial_json and ignore any input in the start
		// event, so the full arguments must be sent as a delta or the client
		// sees an empty input and loops.
		partialJSON := strings.TrimSpace(tc.Function.Arguments)
		if partialJSON == "" {
			partialJSON = "{}"
		}
		api.sendAnthropicSSE(w, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": blockIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": map[string]any{},
			},
		})
		api.sendAnthropicSSE(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": partialJSON,
			},
		})
		api.sendAnthropicSSE(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": blockIndex,
		})
		blockIndex++
	}
	flusher.Flush()

	// Send message_delta event
	stopReason := "end_turn"
	if matchedStop != "" {
		stopReason = "stop_sequence"
	}
	if truncated {
		stopReason = "max_tokens"
		matchedStop = ""
	}
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
		matchedStop = ""
	}
	msgDelta := map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nullableString(matchedStop),
		},
		"usage": map[string]any{
			"output_tokens":    countTokens(fullText) + outputProtocolTokens,
			"reasoning_tokens": countTokens(thinkingText.String()),
			"usage_source":     usageSource(),
		},
	}
	api.sendAnthropicSSE(w, "message_delta", msgDelta)
	flusher.Flush()

	api.storeSessionMapping(sid, finalConvID)

	// Send message_stop event
	msgStop := map[string]any{"type": "message_stop"}
	api.sendAnthropicSSE(w, "message_stop", msgStop)
	flusher.Flush()
}

// The session's conversation is already stored by runToolLoop, so this
// responder takes no session id.
func (api *APIServer) respondBufferedAnthropic(w http.ResponseWriter, result toolLoopResult, messages []payload.Message, model string, maxTokens int, stream bool, tools []toolcalling.ToolDef, toolChoice string, stopSequences []string) {
	stopReason := "end_turn"
	if len(result.toolCalls) > 0 {
		stopReason = "tool_use"
	}
	var matchedStop string
	result.text, stopReason, matchedStop = cutAnthropicAnswer(result.text, stopReason, stopSequences, maxTokens)

	content := []map[string]any{}
	if result.text != "" {
		content = append(content, map[string]any{"type": "text", "text": result.text})
	}
	for _, call := range result.toolCalls {
		var input any
		if json.Unmarshal([]byte(call.Function.Arguments), &input) != nil {
			input = map[string]any{}
		}
		content = append(content, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input})
	}

	usage := anthropicUsage(messages, tools, toolChoice, result.text, result.thinking)
	response := map[string]any{"id": fmt.Sprintf("msg_%s", uuid.New().String()), "type": "message", "role": "assistant", "content": content, "model": model, "stop_reason": stopReason, "stop_sequence": nullableString(matchedStop), "usage": usage}
	if !stream {
		api.sendJSON(w, http.StatusOK, response)
		return
	}
	api.replayAnthropicSSE(w, response, content, model, usage, stopReason, matchedStop, countTokens(result.text))
}

// cutAnthropicAnswer applies the caller's stop sequence and max_tokens.
//
// The answer ends where the caller said it ends, and Anthropic names the
// sequence that ended it alongside the reason. max_tokens wins when it is
// reached first, and clears the match.
func cutAnthropicAnswer(respText, stopReason string, stopSequences []string, maxTokens int) (string, string, string) {
	cut, matchedStop := cutAtStopSequence(respText, stopSequences)
	if matchedStop != "" {
		respText, stopReason = cut, "stop_sequence"
	}
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(respText, maxTokens); ok {
			respText, stopReason = truncated, "max_tokens"
			matchedStop = ""
		}
	}
	return respText, stopReason, matchedStop
}

// anthropicToolUseInput decodes a call's arguments for a tool_use block.
//
// Arguments that do not parse become an empty object rather than a missing
// field, because a tool_use block without input is not a shape the Anthropic
// clients accept.
func anthropicToolUseInput(arguments string) any {
	var input any
	if err := json.Unmarshal([]byte(arguments), &input); err != nil || input == nil {
		return map[string]any{}
	}
	return input
}

// anthropicContentBlocks renders one answer as Anthropic content blocks, in the
// order the protocol expects: reasoning, then text, then the tool calls.
func anthropicContentBlocks(thinking, respText string, toolCalls []client.ToolCall) []map[string]any {
	content := []map[string]any{}
	if thinking != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": thinking, "signature": ""})
	}
	if respText != "" {
		content = append(content, map[string]any{"type": "text", "text": respText})
	}
	for _, tc := range toolCalls {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Function.Name,
			"input": anthropicToolUseInput(tc.Function.Arguments),
		})
	}
	return content
}

// replayAnthropicSSE writes an already complete answer as the Anthropic event
// stream, which is what a streaming request whose turn was buffered receives.
func (api *APIServer) replayAnthropicSSE(w http.ResponseWriter, response map[string]any, content []map[string]any, model string, usage map[string]any, stopReason, matchedStop string, outputTokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	api.sendAnthropicSSE(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": response["id"], "type": "message", "role": "assistant", "content": []any{}, "model": model, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": usage["input_tokens"], "output_tokens": 0, "usage_source": usageSource()}}})
	for i, block := range content {
		api.replayAnthropicBlock(w, i, block)
		api.sendAnthropicSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	api.sendAnthropicSSE(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nullableString(matchedStop)}, "usage": map[string]any{"output_tokens": outputTokens}})
	api.sendAnthropicSSE(w, "message_stop", map[string]any{"type": "message_stop"})
}

// replayAnthropicBlock writes the start and the content of one block.
func (api *APIServer) replayAnthropicBlock(w http.ResponseWriter, index int, block map[string]any) {
	switch block["type"] {
	case "tool_use":
		// tool_use input must stream as an input_json_delta fragment, not
		// inline in content_block_start; SDK clients accumulate partial_json
		// and otherwise see an empty input and loop forever.
		start := map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}}
		api.sendAnthropicSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": start})
		partial, err := json.Marshal(block["input"])
		if err != nil || len(partial) == 0 {
			partial = []byte("{}")
		}
		api.sendAnthropicSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(partial)}})
	case "text":
		api.sendAnthropicSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "text", "text": ""}})
		if txt, _ := block["text"].(string); txt != "" {
			api.sendAnthropicSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": txt}})
		}
	default:
		api.sendAnthropicSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block})
	}
}

// nonStreamAnthropicMessages handles non-streaming Anthropic messages response.
func (api *APIServer) nonStreamAnthropicMessages(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, anthropicModel string, maxTokens int, sid, convID string, hasTools bool, tools []toolcalling.ToolDef, toolChoice string, stopSequences []string, noParallel bool) {
	respText, thinking, toolCalls, finishReason, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
	if err != nil {
		api.forgetSession(sid)
		api.sendUpstreamError(w, "chat", err)
		return
	}
	respText = api.routeGeneratedImages(respText)

	toolCalls, finishReason = withoutBackendToolCalls(toolCalls, finishReason)
	// The transport thinking filter belongs to simulated mode only, where the
	// envelope travels inside the reasoning text.
	if len(tools) > 0 {
		thinking = chatAnthropicThinkingForOutput(thinking, true)
	}

	// Parse simulated tool calls from response text if tool calling is enabled
	if hasTools {
		respText, toolCalls, finishReason = api.applySimulatedToolCalls(
			toolLoopAnthropic, messages, cfg, tools, toolChoice, noParallel, respText, toolCalls,
		)
	}

	stopReason := "end_turn"
	if finishReason == "tool_calls" {
		stopReason = "tool_use"
	}

	if blockedByContentPolicy(respText, toolCalls) {
		api.forgetSession(sid)
		api.sendContentBlockedError(w, respText)
		return
	}
	respText = withoutUnverifiedCompletionClaim(respText, hasTools, buildToolLedger(messages), toolCalls)

	var matchedStop string
	respText, stopReason, matchedStop = cutAnthropicAnswer(respText, stopReason, stopSequences, maxTokens)
	content := anthropicContentBlocks(thinking, respText, toolCalls)

	response := map[string]any{
		"id":            fmt.Sprintf("msg_%s", uuid.New().String()),
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         anthropicModel,
		"stop_reason":   stopReason,
		"stop_sequence": nullableString(matchedStop),
		"usage": map[string]any{
			"input_tokens":     countPromptTokens(messages, tools, toolChoice),
			"output_tokens":    countTokens(respText) + outputProtocolTokens,
			"reasoning_tokens": countTokens(thinking),
			"usage_source":     usageSource(),
		},
	}

	api.storeSessionMapping(sid, finalConvID)
	api.sendJSON(w, http.StatusOK, response)
}

// streamCompletions streams text completion responses in OpenAI text_completion format.
func (api *APIServer) streamCompletions(ctx context.Context, w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, maxTokens int, sid, convID string, hasTools bool, tools []toolcalling.ToolDef, toolChoice string, stopSequences []string, includeUsage bool) {
	flusher, ok := api.beginSSE(w)
	if !ok {
		return
	}

	// Commit the response headers before the upstream turn starts. The first
	// chunk can take almost ten seconds on a tool-enabled turn, and a client
	// that sees no bytes at all cannot tell a slow provider from a dead one.
	refreshStreamDeadline(w)
	if err := writeSSEKeepalive(w, flusher); err != nil {
		return
	}

	stream := &completionStream{
		api:     api,
		w:       w,
		flusher: flusher,
		compID:  fmt.Sprintf("cmpl-%s", uuid.New().String()),
		model:   cfg.OpenAIID,
	}

	ch := api.m365Client.ChatConversationStreamGenContext(ctx, messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)

	// A stop sequence can straddle two chunks, so the deltas of a directly
	// streamed answer pass through a writer that holds back the tail which
	// could still complete one. A tool-enabled turn is buffered whole, so it
	// takes the trailing cut below instead.
	stopWriter := newStopSequenceWriter(stopSequences)

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	for {
		chunk, more := nextStreamChunk(ctx, ch, keepalive, w, flusher, func() error { return writeSSEKeepalive(w, flusher) })
		if !more {
			break
		}
		step := stream.chunk(chunk, ch, maxTokens, hasTools, stopWriter)
		if step == streamLoopFailed {
			return
		}
		if step == streamLoopStop {
			break
		}
	}
	if !hasTools {
		// Whatever the writer held back belongs to the answer when no stop
		// sequence ever arrived.
		stream.delta(stopWriter.flush())
	}

	fullText, simToolCalls := api.completionToolCalls(stream, messages, cfg, tools, toolChoice, stopSequences, hasTools)

	api.storeSessionMapping(sid, stream.convID)
	stream.finish(fullText, simToolCalls, messages, tools, toolChoice, includeUsage)
}

// completionStream carries what one text_completion turn streams.
type completionStream struct {
	api     *APIServer
	w       http.ResponseWriter
	flusher http.Flusher
	compID  string
	model   string

	fullText  strings.Builder
	thinking  strings.Builder
	truncated bool
	convID    string
}

// textChunk builds one text_completion chunk object.
func (s *completionStream) textChunk(text string, finishReason any) map[string]any {
	return map[string]any{
		"id":      s.compID,
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   s.model,
		"choices": []map[string]any{
			{
				"index":         0,
				"text":          text,
				"finish_reason": finishReason,
				"logprobs":      nil,
			},
		},
	}
}

// sendText writes one chunk without recording it, which is what a buffered
// answer needs because it was accumulated as it arrived.
func (s *completionStream) sendText(text string) {
	jsonData, _ := json.Marshal(s.textChunk(text, nil))
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", jsonData)
	s.flusher.Flush()
}

// delta writes one streamed chunk and records the text it carried.
func (s *completionStream) delta(text string) {
	if text == "" {
		return
	}
	s.fullText.WriteString(text)
	s.sendText(text)
}

// fail reports a failed turn as an error object rather than as completion text,
// for the same reason the chat stream carries one: text would be stored as the
// answer.
func (s *completionStream) fail(err error) {
	status, code, message := streamErrorFields("completion", err)
	errChunk := map[string]any{
		"id":      s.compID,
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   s.model,
		"error": map[string]any{
			"message": message,
			"type":    openAIErrorType(status),
			"code":    code,
		},
	}
	jsonData, _ := json.Marshal(errChunk)
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", jsonData)
	_, _ = fmt.Fprintf(s.w, "data: [DONE]\n\n")
	s.flusher.Flush()
}

// chunk reads one upstream chunk of a text_completion turn.
func (s *completionStream) chunk(chunk client.StreamChunk, ch <-chan client.StreamChunk, maxTokens int, hasTools bool, stopWriter *stopSequenceWriter) streamLoopStep {
	if chunk.Error != nil {
		s.fail(chunk.Error)
		return streamLoopFailed
	}
	if chunk.IsFinal {
		s.convID = chunk.ConversationID
		return streamLoopStop
	}

	chunk.Text = s.api.routeGeneratedImages(chunk.Text)

	// Accumulate thinking text (not sent as content for text_completion)
	if chunk.Thinking != "" {
		s.thinking.WriteString(chunk.Thinking)
		return streamLoopContinue
	}

	// Check max_tokens limit before sending more content
	if maxTokens > 0 && countTokens(s.fullText.String()) >= maxTokens {
		s.truncated = true
		drainStream(ch)
		return streamLoopStop
	}

	// If tool calling is not enabled, stream text directly. The writer decides
	// what is safe to release, and it also feeds the accumulator, so the
	// reported usage matches what the client received.
	if !hasTools {
		emit, _ := stopWriter.next(chunk.Text)
		s.delta(emit)
		return streamLoopContinue
	}
	s.fullText.WriteString(chunk.Text)
	return streamLoopContinue
}

// finish writes the terminating chunk with the turn's finish reason and usage.
func (s *completionStream) finish(fullText string, simToolCalls []toolcalling.ToolCall, messages []payload.Message, tools []toolcalling.ToolDef, toolChoice string, includeUsage bool) {
	finishReason := "stop"
	if s.truncated {
		finishReason = "length"
	}
	if len(simToolCalls) > 0 {
		finishReason = "tool_calls"
	}
	doneChunk := s.textChunk("", finishReason)

	// This route reported no usage at all, unlike every other streaming
	// endpoint, so the reasoning it accumulated reached no client.
	if includeUsage {
		promptTok := countPromptTokens(messages, tools, toolChoice)
		completionTok := countTokens(fullText) + outputProtocolTokens
		reasoningTok := countTokens(s.thinking.String())
		doneChunk["usage"] = map[string]any{
			"prompt_tokens":     promptTok,
			"completion_tokens": completionTok,
			"reasoning_tokens":  reasoningTok,
			"total_tokens":      promptTok + completionTok + reasoningTok,
			"usage_source":      usageSource(),
		}
	}

	jsonData, _ := json.Marshal(doneChunk)
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", jsonData)
	_, _ = fmt.Fprintf(s.w, "data: [DONE]\n\n")
	s.flusher.Flush()
}

// completionToolCalls parses the simulated tool calls out of a buffered
// tool-enabled turn and emits whatever text is left as one chunk. A turn with
// no tools streamed its text already and passes through untouched.
func (api *APIServer) completionToolCalls(stream *completionStream, messages []payload.Message, cfg models.ModelConfig, tools []toolcalling.ToolDef, toolChoice string, stopSequences []string, hasTools bool) (string, []toolcalling.ToolCall) {
	fullText := stream.fullText.String()
	if !hasTools {
		return fullText, nil
	}

	var simToolCalls []toolcalling.ToolCall
	contracts := toolcalling.ContractsFor(tools).WithChoice(toolChoice)
	sim := toolcalling.ParseSimulatedResponse(fullText, toolNamesFromDefs(tools), contracts)
	sim = api.repairSimulatedToolCalls(toolLoopOpenAI, messages, cfg, tools, contracts, fullText, sim)
	sim = dropSettledToolCalls(buildToolLedger(messages), toolChoice, sim)
	switch {
	case !sim.HasPayload:
		fullText = toolcalling.WithholdTransportEnvelope(fullText)
		if toolcalling.IsContentPolicyBlock(fullText) {
			// The stream is already open, so the refusal cannot be turned into
			// an HTTP error the way the non-streaming paths do.
			logging.Warn("upstream content refusal on a streaming turn: M365 declined the request instead of answering")
		}
	case len(sim.ToolCalls) > 0:
		simToolCalls = sim.ToolCalls
		fullText = ""
	default:
		fullText = sim.Content
	}

	fullText = replaceUnverifiedCompletionClaim(fullText, hasTools, buildToolLedger(messages), len(simToolCalls))
	// A tool-enabled turn is buffered whole and emitted here, so its stop
	// sequence is applied now rather than through the writer.
	if cut, matched := cutAtStopSequence(fullText, stopSequences); matched != "" {
		fullText = cut
	}

	// If tool calling buffered text, send it now as a single chunk
	if fullText != "" && len(simToolCalls) == 0 {
		stream.sendText(fullText)
	}
	return fullText, simToolCalls
}

// nonStreamCompletions handles non-streaming text completion.
func (api *APIServer) nonStreamCompletions(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, maxTokens int, sid, convID string, hasTools bool, tools []toolcalling.ToolDef, toolChoice string, stopSequences []string) {
	respText, thinking, toolCalls, finishReason, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
	if err != nil {
		api.sendUpstreamError(w, "completion", err)
		return
	}
	respText = api.routeGeneratedImages(respText)

	toolCalls, finishReason = withoutBackendToolCalls(toolCalls, finishReason)

	// Parse simulated tool calls from response text. Completions declares no
	// parallel_tool_calls field, so parallel calls stay allowed here.
	if hasTools {
		respText, toolCalls, finishReason = api.applySimulatedToolCalls(
			toolLoopOpenAI, messages, cfg, tools, toolChoice, false, respText, toolCalls,
		)
	}

	if blockedByContentPolicy(respText, toolCalls) {
		api.forgetSession(sid)
		api.sendContentBlockedError(w, respText)
		return
	}
	respText = withoutUnverifiedCompletionClaim(respText, hasTools, buildToolLedger(messages), toolCalls)
	respText, finishReason = cutChatAnswer(respText, finishReason, stopSequences, maxTokens)

	promptTok := countPromptTokens(messages, tools, toolChoice)
	completionTok := countTokens(respText) + outputProtocolTokens
	reasoningTok := countTokens(thinking)

	// Build choices
	choices := []map[string]any{
		{
			"index":         0,
			"text":          respText,
			"finish_reason": finishReason,
			"logprobs":      nil,
		},
	}

	// Add tool calls to response if present (non-standard extension for text_completion)
	response := map[string]any{
		"id":      fmt.Sprintf("cmpl-%s", uuid.New().String()),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   cfg.OpenAIID,
		"choices": choices,
		"usage": map[string]any{
			"prompt_tokens":     promptTok,
			"completion_tokens": completionTok,
			"reasoning_tokens":  reasoningTok,
			"total_tokens":      promptTok + completionTok + reasoningTok,
			"usage_source":      usageSource(),
		},
	}

	if len(toolCalls) > 0 {
		openaiToolCalls := make([]map[string]any, len(toolCalls))
		for i, tc := range toolCalls {
			openaiToolCalls[i] = map[string]any{
				"index": i,
				"id":    tc.ID,
				"type":  "function",
				"function": map[string]string{
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			}
		}
		response["tool_calls"] = openaiToolCalls
	}

	api.storeSessionMapping(sid, finalConvID)
	api.sendJSON(w, http.StatusOK, response)
}

// sendJSON sends a JSON response.
func (api *APIServer) sendJSON(w http.ResponseWriter, statusCode int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(statusCode)

	writeJSONBody(w, data)
}

// writeJSONBody encodes the body of a response whose status line has already
// gone out.
//
// The status cannot be taken back at this point, so a failure is reported to
// the log rather than to the client. Discarding it left a body that stops in
// the middle of an object with nothing anywhere saying why.
func writeJSONBody(w http.ResponseWriter, data any) {
	if err := json.NewEncoder(w).Encode(data); err != nil {
		logging.Errorf("response body encode failed: %v", err)
	}
}

// sendError sends an error response.
func (api *APIServer) sendError(w http.ResponseWriter, statusCode int, message string) {
	api.sendErrorCode(w, statusCode, openAIErrorCode(statusCode), message)
}

// sendErrorCode sends an error body in the OpenAI shape: the category in
// "type", a machine-readable string in "code". Callers with a specific code,
// such as an exhausted quota, pass it here; sendError derives the default one
// from the status.
func (api *APIServer) sendErrorCode(w http.ResponseWriter, statusCode int, code, message string) {
	api.sendJSON(w, statusCode, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    openAIErrorType(statusCode),
			"code":    code,
		},
	})
}

// sendSSEChunk sends a Server-Sent Events chunk in OpenAI chat.completion.chunk format.
func (api *APIServer) sendSSEChunk(w http.ResponseWriter, chunkID, model string, data map[string]any) {
	chunk := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         data,
				"finish_reason": nil,
			},
		},
	}

	jsonData, _ := json.Marshal(chunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", jsonData)
}

// sendSSEDone sends the final SSE chunk.
func (api *APIServer) sendSSEDone(w http.ResponseWriter, chunkID, model, finishReason string, usage map[string]any) {
	if finishReason == "" {
		finishReason = "stop"
	}
	chunk := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finishReason,
			},
		},
	}

	if usage != nil {
		chunk["usage"] = usage
	}

	jsonData, _ := json.Marshal(chunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", jsonData)
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
}

// sendSSEError sends an error via SSE.
// sendSSEError reports a failed turn on an OpenAI-shaped stream.
//
// The failure used to be written as assistant content with finish_reason
// "stop", so a client stored the error text as the model's answer and had no
// way to tell a failure from a reply. OpenAI puts an error object on the data
// line instead, which is what a client checks for. [DONE] still follows, so a
// reader waiting for the terminator does not hang.
func (api *APIServer) sendSSEError(w http.ResponseWriter, op, chunkID, model string, err error) {
	status, code, message := streamErrorFields(op, err)
	body := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"error": map[string]any{
			"message": message,
			"type":    openAIErrorType(status),
			"code":    code,
		},
	}

	jsonData, _ := json.Marshal(body)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", jsonData)
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
}

// sendAnthropicSSE sends an Anthropic-format SSE event.
func (api *APIServer) sendAnthropicSSE(w http.ResponseWriter, eventType string, data map[string]any) {
	jsonData, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
}

// uploadImagesAndAnnotate uploads any images found in message Images fields
// to the M365 backend and attaches the resulting docId annotations to the
// last message with images. This enables multimodal image input support.
// Limits for caller-supplied remote images. They bound how much work one
// request can make the proxy do on someone else's behalf.
const (
	remoteImageMaxBytes   = 20 << 20
	remoteImageMaxPerTurn = 16
	remoteImageTimeout    = 30 * time.Second
)

// errorBodyExcerptMax caps a failed response body read for logging. Only the
// first two hundred characters reach the log, so the rest never has to be read.
const errorBodyExcerptMax = 64 << 10

// Caps on how much of a request body this service will read. A caller can
// otherwise make the process hold an unbounded amount of memory by sending one
// request that never ends.
const (
	// requestBodyMax bounds a chat, completion or Responses request. Such a
	// request carries the whole conversation the client holds and may inline a
	// base64 image, so the cap sits far above a normal turn.
	requestBodyMax = 32 << 20
	// imageRequestBodyMax bounds an image generation request, which carries a
	// prompt and no image data.
	imageRequestBodyMax = 1 << 20
	// imageEditBodyMax bounds an image edit upload, which carries the images to
	// edit as multipart parts.
	imageEditBodyMax = 64 << 20
)

// limitRequestBody caps how much of a request body a handler reads. The read
// fails once the limit is passed, so an oversize body is refused rather than
// buffered whole.
func limitRequestBody(w http.ResponseWriter, r *http.Request, max int64) {
	r.Body = http.MaxBytesReader(w, r.Body, max)
}

// errRemoteImageRejected marks a caller-supplied image URL the proxy refuses to
// fetch.
var errRemoteImageRejected = errors.New("remote image URL rejected")

// validateRemoteImageURL decides whether a caller-supplied image URL may be
// fetched. No credential travels on this request, so any public https host is
// acceptable; the guard exists to stop the proxy being used to reach addresses
// inside its own network.
func validateRemoteImageURL(rawURL string) error {
	parsed, err := neturl.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %v", errRemoteImageRejected, err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q is not https", errRemoteImageRejected, parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("%w: URL has no host", errRemoteImageRejected)
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("%w: %q does not resolve", errRemoteImageRejected, host)
	}
	if slices.ContainsFunc(ips, ipDisallowed) {
		return fmt.Errorf("%w: %q resolves to a non-public address", errRemoteImageRejected, host)
	}
	return nil
}

// fetchRemoteImage downloads a caller-supplied image and returns it as base64
// with its media type. It sends no Authorization header, so a hostile URL
// learns nothing beyond the fact that the proxy fetched it.
func fetchRemoteImage(rawURL string) (base64Data, mediaType string, err error) {
	if err := validateRemoteImageURL(rawURL); err != nil {
		return "", "", err
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("fetch remote image: %w", err)
	}
	// Go's default agent string is rejected outright by several image CDNs,
	// including Wikimedia, so the request identifies the proxy instead.
	req.Header.Set("User-Agent", "M365Bridge/"+models.Version)
	req.Header.Set("Accept", "image/*")

	client := &http.Client{Timeout: remoteImageTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch remote image: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("fetch remote image: HTTP %d", resp.StatusCode)
	}

	// One extra byte distinguishes "exactly at the limit" from "truncated".
	body, err := io.ReadAll(io.LimitReader(resp.Body, remoteImageMaxBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("read remote image: %w", err)
	}
	if len(body) > remoteImageMaxBytes {
		return "", "", fmt.Errorf("%w: larger than %d bytes", errRemoteImageRejected, remoteImageMaxBytes)
	}

	mediaType = resp.Header.Get("Content-Type")
	if semicolon := strings.IndexByte(mediaType, ';'); semicolon >= 0 {
		mediaType = strings.TrimSpace(mediaType[:semicolon])
	}
	if !strings.HasPrefix(mediaType, "image/") {
		return "", "", fmt.Errorf("%w: content type %q is not an image", errRemoteImageRejected, mediaType)
	}
	return base64.StdEncoding.EncodeToString(body), mediaType, nil
}

// resolveRemoteImages fetches every caller-supplied image URL in a message and
// drops the ones that cannot be fetched, so a single bad URL does not fail the
// whole turn.
func resolveRemoteImages(msg *payload.Message) {
	resolved := make([]payload.ImageData, 0, len(msg.Images))
	fetched := 0
	for _, img := range msg.Images {
		if img.RemoteURL == "" {
			resolved = append(resolved, img)
			continue
		}
		if fetched >= remoteImageMaxPerTurn {
			logging.Warnf("resolveRemoteImages: skipping image beyond the per-turn limit of %d", remoteImageMaxPerTurn)
			continue
		}
		fetched++
		data, mediaType, err := fetchRemoteImage(img.RemoteURL)
		if err != nil {
			logging.Errorf("resolveRemoteImages: %v", err)
			continue
		}
		resolved = append(resolved, payload.ImageData{
			Base64:    data,
			MediaType: mediaType,
			FileName:  "upload." + extFromMediaType(mediaType),
		})
	}
	msg.Images = resolved
}

func (api *APIServer) uploadImagesAndAnnotate(messages *[]payload.Message, convID string) {
	// Caller-supplied URLs arrive unfetched, so resolve them before the last
	// message with images is chosen: a message whose only images were rejected
	// must not be treated as an image turn.
	for i := range *messages {
		resolveRemoteImages(&(*messages)[i])
	}

	// Find the last message with images
	lastImgIdx := -1
	for i := range slices.Backward(*messages) {
		if len((*messages)[i].Images) > 0 {
			lastImgIdx = i
			break
		}
	}
	if lastImgIdx < 0 {
		return
	}

	logging.Infof("uploadImagesAndAnnotate: uploading %d images for message[%d] convID=%s", len((*messages)[lastImgIdx].Images), lastImgIdx, convID)

	// Use existing convID or generate a temporary UUID for upload
	uploadConvID := convID
	if uploadConvID == "" {
		uploadConvID = uuid.New().String()
	}

	msg := &(*messages)[lastImgIdx]
	for _, img := range msg.Images {
		result, err := api.m365Client.UploadFile(img.Base64, img.MediaType, img.FileName, uploadConvID, api.config.UserOID, api.config.TenantID)
		if err != nil {
			logging.Errorf("Image upload failed: %v", err)
			continue
		}
		if !result.IsSuccess {
			logging.Warnf("Image upload returned non-success: %+v", result)
			continue
		}

		fileType := strings.TrimPrefix(result.FileType, ".")
		msg.Annotations = append(msg.Annotations, payload.MessageAnnotation{
			ID:                    result.DocID,
			MessageAnnotationType: "ImageFile",
			MessageAnnotationMetadata: map[string]string{
				"@type":          "File",
				"annotationType": "File",
				"fileType":       fileType,
				"fileName":       img.FileName,
			},
		})
	}
}

// jsonOnlyInstruction is what a request asking for JSON output adds to the
// prompt. M365 has no structured-output channel of its own, so the demand can
// only reach the model as text.
const jsonOnlyInstruction = "You MUST respond with valid JSON only. Do not include markdown code blocks, explanation, or any text outside the JSON object."

// jsonModeInstruction returns the instruction a response_format asks for, and
// reports whether the format demands JSON at all.
//
// A json_schema format used to be accepted and then ignored, so a client that
// asked for a strict shape received prose. The schema travels in the
// instruction because the prompt is the only channel that reaches the model.
func jsonModeInstruction(format map[string]any) (string, bool) {
	kind, _ := format["type"].(string)
	switch kind {
	case "json_object":
		return jsonOnlyInstruction, true
	case "json_schema":
		wrapper, ok := format["json_schema"].(map[string]any)
		if !ok || wrapper["schema"] == nil {
			return jsonOnlyInstruction, true
		}
		encoded, err := json.Marshal(wrapper["schema"])
		if err != nil {
			logging.Warnf("response_format: cannot encode the declared schema: %v", err)
			return jsonOnlyInstruction, true
		}
		return jsonOnlyInstruction + "\nThe JSON must validate against this JSON Schema:\n" + string(encoded), true
	default:
		return "", false
	}
}

// injectJSONMode injects JSON mode instructions into messages.
func injectJSONMode(messages *[]payload.Message, instruction string) {
	for i, msg := range *messages {
		if payload.IsSystemRole(msg.Role) {
			(*messages)[i].Content = msg.Content + "\n" + instruction
			return
		}
	}

	*messages = append([]payload.Message{{Role: "system", Content: instruction}}, *messages...)
}

// sseKeepaliveInterval bounds how long a streaming response may stay silent.
// A tool-enabled turn buffers its text until the parse completes, and a turn on
// a tone that emits no reasoning writes nothing at all until the first content
// chunk arrives; measured at over nine seconds on a live turn. Clients drop a
// stream that delivers no bytes for around thirty seconds.
const sseKeepaliveInterval = 10 * time.Second

// streamOptions carries the OpenAI stream_options object.
type streamOptions struct {
	IncludeUsage *bool `json:"include_usage"`
}

// includeStreamUsage reports whether a streaming turn should carry a usage
// object.
//
// OpenAI defaults include_usage to false. This proxy has always sent usage on
// every streaming chat turn, and clients here read it, so an absent
// stream_options keeps that behaviour. Only an explicit false withholds it.
func includeStreamUsage(opts *streamOptions) bool {
	return opts == nil || opts.IncludeUsage == nil || *opts.IncludeUsage
}

// sseWriteTimeout bounds how long one SSE write may block. Without it a client
// that stopped reading but never closed its socket holds the handler goroutine
// and its upstream WebSocket open for the rest of the turn.
const sseWriteTimeout = 30 * time.Second

// refreshStreamDeadline extends the write deadline of an SSE response.
//
// The error is deliberately ignored: httptest.ResponseRecorder and any wrapped
// writer that hides the underlying connection report ErrNotSupported, and a
// stream must not fail because the deadline could not be armed.
func refreshStreamDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(sseWriteTimeout))
}

// writeSSEKeepalive emits an SSE comment. Every client ignores a comment line,
// so it keeps the connection alive without entering any field contract.
func writeSSEKeepalive(w http.ResponseWriter, flusher http.Flusher) error {
	if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// writeSSENotice emits a state of the turn as an SSE comment.
//
// A comment is the only channel that reaches an interested client without
// entering any field contract: every client ignores a comment line, so this
// cannot change what an OpenAI or Anthropic SDK parses. The notice is a machine
// token rather than a sentence, because the reader owns the wording and its
// language. Nothing here is answer text, so it never reaches a transcript and
// there is nothing to take back once the turn produces content.
func writeSSENotice(w http.ResponseWriter, flusher http.Flusher, notice string) error {
	if _, err := fmt.Fprintf(w, ": notice %s\n\n", notice); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// writeAnthropicKeepalive emits the ping event the Anthropic wire format
// defines for this purpose. An SSE comment would work too, but the SDK already
// knows ping and dispatches it at any point in the stream.
func writeAnthropicKeepalive(w http.ResponseWriter, flusher http.Flusher) error {
	if _, err := fmt.Fprint(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// nextStreamChunk returns the next upstream chunk, writing a keepalive frame
// through write for every interval that passes while the upstream is silent.
// The second result is false once the channel closes, the request context is
// canceled, or a keepalive write fails; the last two are how a stream to a gone
// client ends while it sits idle.
//
// A chunk that carries a notice is written here as an SSE comment and never
// returned, so every streaming responder reports one without a line of its own
// and none of them can forget. A notice carries no text, so nothing is lost by
// consuming it.
//
// The keepalive shares the caller's goroutine on purpose: http.ResponseWriter
// tolerates no concurrent writes, so a background ticker goroutine would
// interleave frames with the chunk loop.
func nextStreamChunk(ctx context.Context, ch <-chan client.StreamChunk, keepalive *time.Ticker, w http.ResponseWriter, flusher http.Flusher, write func() error) (client.StreamChunk, bool) {
	for {
		select {
		case <-ctx.Done():
			return client.StreamChunk{}, false
		case chunk, ok := <-ch:
			if ok {
				keepalive.Reset(sseKeepaliveInterval)
				refreshStreamDeadline(w)
				if chunk.Notice != "" {
					if err := writeSSENotice(w, flusher, chunk.Notice); err != nil {
						logging.Debugf("nextStreamChunk: notice write failed, ending stream: %v", err)
						return client.StreamChunk{}, false
					}
					continue
				}
			}
			return chunk, ok
		case <-keepalive.C:
			refreshStreamDeadline(w)
			if err := write(); err != nil {
				logging.Debugf("nextStreamChunk: keepalive write failed, ending stream: %v", err)
				return client.StreamChunk{}, false
			}
		}
	}
}

// withoutBackendToolCalls drops the tool calls M365 raises for its own built-ins
// (search, code_interpreter, trigger_plugin, invoke_action) and returns the
// finish reason the turn must end on instead.
//
// The client never declared those names, so it cannot execute them. This holds
// whether or not the request carried tools: a plain chat turn that triggers a
// server-side search would otherwise end on a tool_calls finish reason with a
// call the caller has no handler for.
func withoutBackendToolCalls(calls []client.ToolCall, finishReason string) ([]client.ToolCall, string) {
	if len(calls) == 0 {
		return calls, finishReason
	}
	if finishReason == "tool_calls" || finishReason == "tool_use" {
		finishReason = "stop"
	}
	return nil, finishReason
}

// chatAnthropicThinkingForOutput strips the simulated transport envelope from a
// complete thinking string for non-streaming and OpenAI callers. It delegates
// to toolcalling.FilterTransportThinking so every endpoint (streaming and
// non-streaming) applies the exact same filter.
func chatAnthropicThinkingForOutput(thinking string, simulated bool) string {
	if !simulated || thinking == "" {
		return thinking
	}
	return toolcalling.FilterTransportThinking(thinking)
}

// injectSimulatedPrompt replaces the last user message with a simulated-mode
// prompt that embeds the entire OpenAI request JSON and asks M365 Copilot to
// produce a valid chat.completion response in a single ```json block.
func injectSimulatedPrompt(messages *[]payload.Message, requestJSON, toolChoice, evidence string) {
	if len(*messages) == 0 {
		return
	}
	prompt := toolcalling.BuildSimulatedPrompt(requestJSON, true, toolChoice, evidence)
	for i := range slices.Backward(*messages) {
		if (*messages)[i].Role == "user" {
			suffix := ""
			if currentUserMessage := strings.TrimSpace((*messages)[i].Content); currentUserMessage != "" {
				suffix = "\n\nCURRENT USER MESSAGE\n" + currentUserMessage
			}
			(*messages)[i].Content = prompt + suffix
			break
		}
	}
}

// injectSimulatedPromptResponses replaces the converted Responses history with
// one canonical simulation message. The full history remains present exactly
// once inside requestJSON, avoiding duplicated context at the M365 layer.
func injectSimulatedPromptResponses(messages *[]payload.Message, requestJSON, toolChoice, evidence string) {
	prompt := toolcalling.BuildSimulatedPromptResponses(requestJSON, true, toolChoice, evidence)
	canonical := payload.Message{Role: "user", Content: prompt}
	for _, message := range *messages {
		canonical.Images = append(canonical.Images, message.Images...)
		canonical.Annotations = append(canonical.Annotations, message.Annotations...)
	}
	*messages = []payload.Message{canonical}
}

// injectSimulatedPromptAnthropic replaces the last user message with a
// simulated-mode prompt that embeds the entire Anthropic request JSON and asks
// M365 Copilot to produce a valid Anthropic Messages response in a single
// ```json block.
func injectSimulatedPromptAnthropic(messages *[]payload.Message, requestJSON, toolChoice, evidence string) {
	if len(*messages) == 0 {
		return
	}
	prompt := toolcalling.BuildSimulatedPromptAnthropic(requestJSON, true, toolChoice, evidence)
	for i := range slices.Backward(*messages) {
		if (*messages)[i].Role == "user" {
			suffix := ""
			if currentUserMessage := strings.TrimSpace((*messages)[i].Content); currentUserMessage != "" {
				suffix = "\n\nCURRENT USER MESSAGE\n" + currentUserMessage
			}
			(*messages)[i].Content = prompt + suffix
			break
		}
	}
}

// reasoningEffortRank orders the accepted effort values. Anything at medium or
// above is treated as a request to deliberate, which is the only distinction
// M365 can act on.
var reasoningEffortRank = map[string]int{
	"none":    0,
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
}

// reasoningEffortRequestsDeliberation validates the effort value and reports
// whether it asks for a reasoning tone. An unset block leaves the tone alone.
func reasoningEffortRequestsDeliberation(reasoning *responsesReasoning) (bool, error) {
	if reasoning == nil {
		return false, nil
	}
	effort := strings.ToLower(strings.TrimSpace(reasoning.Effort))
	if effort == "" {
		return false, nil
	}
	rank, ok := reasoningEffortRank[effort]
	if !ok {
		return false, fmt.Errorf(
			"unsupported reasoning effort %q; use %s",
			reasoning.Effort,
			strings.Join(models.ReasoningEffortNames(), ", "),
		)
	}
	return rank >= reasoningEffortRank["medium"], nil
}

// applyReasoningEffort returns the model config to use, redirecting to the
// model key's reasoning variant when the caller asked to deliberate and such a
// variant exists. A model without a variant, or a key that is already one, is
// left untouched: M365 has no separate effort dial, so the tone is the only
// lever.
func applyReasoningEffort(modelKey string, cfg models.ModelConfig, deliberate bool) models.ModelConfig {
	if !deliberate {
		return cfg
	}
	// The requested name is resolved to registry keys first, because a caller
	// that took its id from /v1/models sends the advertised id ("gpt-5.5") and
	// appending the suffix to that never matches the key ("gpt5.5-reasoning").
	// The catalog advertised effort support for the same id, so the request ran
	// on the non-reasoning tone and the caller had no way to notice.
	for _, key := range models.RegistryKeysFor(modelKey) {
		if strings.HasSuffix(key, "-reasoning") {
			return cfg
		}
		// The registry is consulted directly rather than through FindModel,
		// because a model without a reasoning variant must be left alone rather
		// than reported as an unknown model.
		variantKey := key + "-reasoning"
		if variant, ok := models.ModelRegistry[variantKey]; ok {
			logging.Infof("applyReasoningEffort: routing %s to %s", modelKey, variantKey)
			return variant
		}
	}
	return cfg
}

// validateToolResultMessages rejects a tool result that answers nothing. The
// conversation payload flattens results into text, so an id the request never
// declared would silently reach the model as a plausible-looking result and
// desynchronize the client's own tool loop.
//
// When the request declares no tool calls at all, the id cannot be checked
// against anything: a client that trimmed its history to stay under the
// context window legitimately sends results whose calls are no longer present.
// Only a missing id is rejected in that case.
func validateToolResultMessages(messages []payload.Message) error {
	known, err := declaredToolCallIDs(messages)
	if err != nil {
		return err
	}
	return validateToolResults(messages, known)
}

// declaredToolCallIDs collects the call ids this request declares.
//
// A repeated id makes the loop ambiguous: neither the client nor this server
// can tell which call a later result answers.
func declaredToolCallIDs(messages []payload.Message) (map[string]bool, error) {
	known := make(map[string]bool)
	for i := range messages {
		for _, call := range messages[i].ToolCalls {
			if call.ID == "" {
				continue
			}
			if known[call.ID] {
				return nil, fmt.Errorf("tool call id %q is declared more than once in this request", call.ID)
			}
			known[call.ID] = true
		}
	}
	return known, nil
}

// validateToolResults checks that every result answers exactly one declared
// call.
func validateToolResults(messages []payload.Message, known map[string]bool) error {
	answered := make(map[string]bool)
	for i := range messages {
		if messages[i].Role == "tool" && messages[i].ToolCallID == "" {
			return errors.New(`a message with role "tool" is missing tool_call_id`)
		}
		if err := validateMessageToolResults(messages[i], known, answered); err != nil {
			return err
		}
	}
	return nil
}

// validateMessageToolResults checks the results one message carries.
func validateMessageToolResults(message payload.Message, known, answered map[string]bool) error {
	for _, result := range message.ToolResults {
		if result.ID == "" {
			return errors.New("a tool result is missing the id of the tool call it answers")
		}
		if len(known) > 0 && !known[result.ID] {
			return fmt.Errorf("tool result %q does not answer any tool call in this request", result.ID)
		}
		if answered[result.ID] {
			return fmt.Errorf("tool call %q is answered more than once in this request", result.ID)
		}
		answered[result.ID] = true
	}
	return nil
}

// activeToolMessages returns the messages belonging to the current user turn.
//
// The turn starts at the last user message that carries neither a tool result
// nor a progress note: in the Anthropic shape every tool result arrives as a
// user message, and a progress note is transport metadata, so taking the plain
// last user message would land inside the loop instead of at its start.
// Earlier turns stay out of the round count but remain in the history, where
// they are still evidence.
func activeToolMessages(messages []payload.Message) []payload.Message {
	start := 0
	for i := range messages {
		if messages[i].Role != "user" || len(messages[i].ToolResults) > 0 || messages[i].ToolProgress {
			continue
		}
		start = i
	}
	return messages[start:]
}

// buildToolLedger reconstructs the evidence of the client-driven tool loop from
// the current user turn. The server holds no state across such a loop, so the
// incoming history is the only record of what already ran.
func buildToolLedger(messages []payload.Message) toolcalling.Ledger {
	active := activeToolMessages(messages)

	var calls []toolcalling.LedgerCall
	var results []toolcalling.LedgerResult
	rounds := 0
	for i := range active {
		if len(active[i].ToolCalls) > 0 {
			rounds++
		}
		for _, call := range active[i].ToolCalls {
			calls = append(calls, toolcalling.LedgerCall{
				ID:        call.ID,
				Name:      call.Name,
				Arguments: call.Arguments,
			})
		}
		for _, result := range active[i].ToolResults {
			results = append(results, toolcalling.LedgerResult{
				ID:      result.ID,
				Content: result.Content,
			})
		}
	}
	return toolcalling.BuildLedger(calls, results, rounds)
}

// toolChoiceForcesACall reports whether the caller demanded a tool call in this
// turn, either by name or with "required"/"any".
func toolChoiceForcesACall(toolChoice string) bool {
	switch toolChoice {
	case "", "auto", "none":
		return false
	default:
		return true
	}
}

// dropSettledToolCalls removes a tool call the model is issuing for at least
// the third time with the same arguments, whose result is already in this
// turn's history.
//
// A call the caller demanded through tool_choice is forwarded regardless:
// refusing it would contradict the request. The drop is not recorded in
// DroppedCalls either, because that list drives a corrective re-ask and
// re-asking would produce the same call again. When nothing survives, the turn
// becomes a plain answer, which needs substitute text because the parser clears
// the content whenever tool calls are present.
func dropSettledToolCalls(ledger toolcalling.Ledger, toolChoice string, sim toolcalling.SimulatedResult) toolcalling.SimulatedResult {
	if len(sim.ToolCalls) == 0 || toolChoiceForcesACall(toolChoice) {
		return sim
	}
	kept, dropped := ledger.FilterRepeated(sim.ToolCalls)
	if len(dropped) == 0 {
		return sim
	}
	for _, call := range dropped {
		logging.Warnf("dropSettledToolCalls: dropping %q, its result is already in this turn's history", call.Name)
	}
	sim.ToolCalls = kept
	if len(kept) == 0 {
		sim.FinishReason = "stop"
		if strings.TrimSpace(sim.Content) == "" {
			sim.Content = toolcalling.RepeatedCallsNotice
		}
	}
	return sim
}

// toolRoundLimitCode marks a client-driven tool loop that ran past its cap.
const toolRoundLimitCode = "tool_round_limit"

// exceededToolRoundLimit reports whether the client has driven more tool rounds
// within this user turn than the configuration allows.
func (api *APIServer) exceededToolRoundLimit(ledger toolcalling.Ledger) bool {
	limit := api.config.MaxToolRounds
	if limit <= 0 {
		limit = models.DefaultMaxToolRounds
	}
	return ledger.Rounds > limit
}

// sendToolRoundLimitError stops a client-driven tool loop that is not
// converging. HTTP 409 is not a status the Anthropic SDK expects, but an
// explicit refusal is better than answering forever while the client keeps
// asking for one more round.
func (api *APIServer) sendToolRoundLimitError(w http.ResponseWriter, ledger toolcalling.Ledger) {
	limit := api.config.MaxToolRounds
	if limit <= 0 {
		limit = models.DefaultMaxToolRounds
	}
	logging.Warnf(
		"tool round limit reached: rounds=%d limit=%d completed=%d repeatedCall=%v repeatedFailure=%v",
		ledger.Rounds, limit, len(ledger.Completed), ledger.RepeatedCall, ledger.RepeatedFailure,
	)
	message := fmt.Sprintf(
		"tool round limit reached: this turn drove %d tool rounds with %d completed calls, above the limit of %d; raise M365_MAX_TOOL_ROUNDS or start a new turn",
		ledger.Rounds, len(ledger.Completed), limit,
	)
	api.sendErrorCode(w, http.StatusConflict, toolRoundLimitCode, message)
}

// anthropicToolChoiceString normalizes the Anthropic tool_choice field to a
// string ("any", "auto", "tool", or "") for prompt-building purposes.
func anthropicToolChoiceString(toolChoice map[string]any) string {
	if toolChoice == nil {
		return ""
	}
	if t, ok := toolChoice["type"].(string); ok {
		return t
	}
	return ""
}

// anthropicToolChoiceEnforcement resolves the Anthropic tool_choice field to
// the value the parser enforces: "none", "auto", "any", or the name of the one
// tool the caller pinned. It differs from anthropicToolChoiceString, which
// reports the raw type and would turn a pinned tool into the literal name
// "tool".
func anthropicToolChoiceEnforcement(toolChoice map[string]any) string {
	kind := anthropicToolChoiceString(toolChoice)
	if kind != "tool" {
		return kind
	}
	name, _ := toolChoice["name"].(string)
	return name
}

// refusesParallelToolCalls reports whether an OpenAI parallel_tool_calls field
// forbids more than one call per turn. An absent field means the protocol
// default, which allows them.
func refusesParallelToolCalls(parallel *bool) bool {
	return parallel != nil && !*parallel
}

// anthropicRefusesParallelToolCalls reads Anthropic's disable_parallel_tool_use,
// which carries the same meaning as OpenAI's parallel_tool_calls but lives
// inside the tool_choice object rather than beside it.
func anthropicRefusesParallelToolCalls(toolChoice map[string]any) bool {
	disabled, _ := toolChoice["disable_parallel_tool_use"].(bool)
	return disabled
}

// toolChoiceString normalizes the tool_choice field to a string ("auto",
// "required", "none", or a function name) for prompt-building purposes.
func toolChoiceString(toolChoice any) string {
	if toolChoice == nil {
		return ""
	}
	if s, ok := toolChoice.(string); ok {
		return s
	}
	if m, ok := toolChoice.(map[string]any); ok {
		if fn, ok := m["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return name
			}
		}
	}
	return ""
}

const simulatedToolCallRequiredCode = "simulated_tool_call_required"
const upstreamEmptyResponseCode = "upstream_empty_response"

var errSimulatedToolCallRequired = errors.New(simulatedToolCallRequiredCode)

type responsesToolPolicy struct {
	simulate         bool
	required         bool
	requiredName     string
	promptChoice     string
	allowedToolNames []string
	tools            []toolcalling.ToolDef
	// ledger carries the evidence of the client-driven tool loop. The
	// Responses input is collapsed into one canonical prompt message before
	// parsing, so the ledger has to travel with the policy instead of being
	// rebuilt from the messages the parser sees.
	ledger toolcalling.Ledger
	// noParallel carries the request's parallel_tool_calls: false. It rides on
	// the policy because the policy is what reaches both Responses paths.
	noParallel bool
}

// allows reports whether this policy still permits a call to the named tool.
// allowedToolNames is already narrowed to the pinned tool when tool_choice
// names one, so membership is the whole test.
func (p responsesToolPolicy) allows(name string) bool {
	return slices.Contains(p.allowedToolNames, name)
}

type responsesSimulationResult struct {
	content      string
	toolCalls    []client.ToolCall
	finishReason string
}

func newResponsesToolPolicy(tools []toolcalling.ToolDef, toolChoice any) (responsesToolPolicy, error) {
	allNames := responsesToolNames(tools)
	knownNames := make(map[string]bool, len(tools))
	for _, name := range allNames {
		knownNames[name] = true
	}

	policy := responsesToolPolicy{
		// With only web search declared there is nothing for the client to
		// run, so the request takes the plain answer path.
		simulate:         len(allNames) > 0,
		promptChoice:     "auto",
		allowedToolNames: allNames,
		tools:            tools,
	}

	if err := policy.applyToolChoice(toolChoice, knownNames); err != nil {
		return responsesToolPolicy{}, err
	}

	if policy.simulate && len(policy.allowedToolNames) == 0 {
		return responsesToolPolicy{}, errors.New("responses tools must include at least one function name")
	}
	if policy.required && !policy.simulate {
		return responsesToolPolicy{}, errors.New("responses tool_choice requires at least one tool")
	}
	return policy, nil
}

// applyToolChoice narrows the policy to what tool_choice asked for. Responses
// defaults to auto when tools are present, which is what a nil choice means.
func (p *responsesToolPolicy) applyToolChoice(toolChoice any, knownNames map[string]bool) error {
	switch choice := toolChoice.(type) {
	case nil:
		return nil
	case string:
		return p.applyStringChoice(choice, knownNames)
	case map[string]any:
		return p.applyNamedChoice(choice, knownNames)
	}
	return fmt.Errorf("invalid Responses tool_choice type %T", toolChoice)
}

// applyStringChoice reads the string form of tool_choice, which is either a
// mode or the name of one tool.
func (p *responsesToolPolicy) applyStringChoice(choice string, knownNames map[string]bool) error {
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "", "auto":
		return nil
	case "none":
		p.simulate = false
		p.promptChoice = "none"
		p.allowedToolNames = nil
		return nil
	case "required":
		p.required = true
		p.promptChoice = "required"
		return nil
	}

	if strings.EqualFold(choice, toolcalling.WebSearchToolName) {
		// The backend performs the search itself, so the pin is satisfied by
		// the answer rather than by a call.
		return nil
	}
	if !knownNames[choice] {
		return fmt.Errorf("invalid Responses tool_choice %q", choice)
	}
	p.pinTo(choice)
	return nil
}

// applyNamedChoice reads the object form of tool_choice.
func (p *responsesToolPolicy) applyNamedChoice(choice map[string]any, knownNames map[string]bool) error {
	name := responsesChoiceName(choice)
	if name == "" || !knownNames[name] {
		return fmt.Errorf("invalid Responses named tool_choice %q", name)
	}
	p.pinTo(name)
	return nil
}

// responsesChoiceName reads the tool name out of an object tool_choice, which
// carries it under any of three keys depending on the client.
func responsesChoiceName(choice map[string]any) string {
	name, _ := choice["name"].(string)
	choiceType, _ := choice["type"].(string)
	if name == "" {
		if function, ok := choice["function"].(map[string]any); ok {
			name, _ = function["name"].(string)
		}
	}
	if name == "" && choiceType != "" && choiceType != "function" && choiceType != "custom" {
		name = choiceType
	}
	return strings.TrimSpace(name)
}

// pinTo requires the named tool and hides every other one from the prompt.
func (p *responsesToolPolicy) pinTo(name string) {
	p.required = true
	p.requiredName = name
	p.promptChoice = name
	p.allowedToolNames = []string{name}
}

func responsesToolName(tool toolcalling.ToolDef) string {
	name := strings.TrimSpace(toolcalling.ToolName(&tool))
	if name == "" && tool.Type != "" && tool.Type != "function" && tool.Type != "custom" {
		name = tool.Type
	}
	return name
}

func responsesToolNames(tools []toolcalling.ToolDef) []string {
	names := make([]string, 0, len(tools))
	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		// Web search is a server-side built-in: the backend answers with
		// search results inline and the client has nothing to execute, so a
		// call to it must never be routed even though the declaration stays
		// visible in the prompt.
		if toolcalling.IsWebSearchTool(&tool) {
			continue
		}
		name := responsesToolName(tool)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

func responsesToolKey(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

func responsesToolTypes(tools []toolcalling.ToolDef) map[string]string {
	types := make(map[string]string, len(tools))
	for _, tool := range tools {
		name := responsesToolName(tool)
		if name == "" {
			continue
		}
		toolType := tool.Type
		if toolType == "" {
			toolType = "function"
		}
		key := responsesToolKey(tool.Namespace, name)
		// A client may declare one name twice, once freeform and once as a
		// function, so a model that cannot follow a grammar still has a call
		// shape. The custom declaration wins, because a freeform body emitted
		// as function_call arguments is not the JSON a client parses there,
		// while JSON carried as a custom tool's input still arrives intact.
		if types[key] == "custom" {
			continue
		}
		types[key] = toolType
	}
	return types
}

func responsesToolDefsFromRaw(raw any) []toolcalling.ToolDef {
	return responsesToolDefsFromRawNamespace(raw, "")
}

func responsesToolDefsFromRawNamespace(raw any, inheritedNamespace string) []toolcalling.ToolDef {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	var definitions []toolcalling.ToolDef
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if toolType, _ := tool["type"].(string); toolType == "namespace" {
			definitions = append(definitions, expandToolNamespace(tool, inheritedNamespace)...)
			continue
		}
		definition, ok := responsesToolDefFromRaw(tool, inheritedNamespace)
		if !ok {
			continue
		}
		definitions = append(definitions, definition)
	}
	return definitions
}

// expandToolNamespace flattens a namespace entry, whose own name scopes every
// tool it holds.
func expandToolNamespace(tool map[string]any, inheritedNamespace string) []toolcalling.ToolDef {
	namespace, _ := tool["name"].(string)
	if namespace == "" {
		namespace = inheritedNamespace
	}
	return responsesToolDefsFromRawNamespace(tool["tools"], namespace)
}

// responsesToolDefFromRaw reads one tool declaration. A declaration with no
// usable name is not a tool this gateway can route a call to.
func responsesToolDefFromRaw(tool map[string]any, inheritedNamespace string) (toolcalling.ToolDef, bool) {
	toolType, _ := tool["type"].(string)
	name, _ := tool["name"].(string)
	if name == "" && toolType != "" && toolType != "function" && toolType != "custom" {
		name = toolType
	}
	if name == "" {
		return toolcalling.ToolDef{}, false
	}
	namespace, _ := tool["namespace"].(string)
	if namespace == "" {
		namespace = inheritedNamespace
	}
	description, _ := tool["description"].(string)
	definition := toolcalling.ToolDef{
		Type:        toolType,
		Name:        name,
		Namespace:   namespace,
		Description: description,
	}
	if parameters, ok := tool["parameters"].(map[string]any); ok {
		definition.Parameters = parameters
	}
	if inputSchema, ok := tool["input_schema"].(map[string]any); ok {
		definition.InputSchema = inputSchema
	}
	if nestedTools, ok := tool["tools"].([]any); ok {
		definition.Tools = responsesToolDefsFromRawNamespace(nestedTools, namespace)
	}
	return definition, true
}

func mergeLoadedResponsesTools(input any, tools []toolcalling.ToolDef) []toolcalling.ToolDef {
	items, ok := input.([]any)
	if !ok {
		return tools
	}
	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		name := responsesToolName(tool)
		if name != "" {
			seen[responsesToolKey(tool.Namespace, name)] = true
		}
	}
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := record["type"].(string)
		if itemType != "tool_search_output" && itemType != "additional_tools" {
			continue
		}
		tools = appendUnseenTools(tools, responsesToolDefsFromRaw(record["tools"]), seen)
	}
	return tools
}

// appendUnseenTools adds the declarations this request has not already seen,
// keyed by namespace and name so two tools of the same name in different
// namespaces both survive.
func appendUnseenTools(tools, loaded []toolcalling.ToolDef, seen map[string]bool) []toolcalling.ToolDef {
	for _, tool := range loaded {
		name := responsesToolName(tool)
		key := responsesToolKey(tool.Namespace, name)
		if name == "" || seen[key] {
			continue
		}
		seen[key] = true
		tools = append(tools, tool)
	}
	return tools
}

// responsesCustomToolItemID derives the output item id of a custom tool call
// from its call id. The streaming path announces the item twice, in_progress
// then completed, and streams its input in between; all three must name the
// same id, so every one of them goes through this helper.
func responsesCustomToolItemID(callID string) string {
	return "ctc_" + strings.TrimPrefix(callID, "call_")
}

// responsesStreamEvent is one SSE event of the Responses stream, named
// separately from its payload so the events of a tool call can be built and
// tested without a stream to write them to.
type responsesStreamEvent struct {
	name string
	data map[string]any
}

// responsesToolInputEvents returns the events that stream one tool call's input.
//
// Each item type carries its input over its own pair of events. A tool_search
// call carries its query inside the item and streams nothing. A custom tool
// call takes free-form input rather than JSON arguments, so a client reading
// response.function_call_arguments would never see it, and its events name the
// item by the id the surrounding output_item events announced rather than by
// the call id.
func responsesToolInputEvents(callID string, call client.ToolCall, toolTypes map[string]string, outputIndex int) []responsesStreamEvent {
	toolKey := responsesToolKey(call.Function.Namespace, call.Function.Name)
	switch toolTypes[toolKey] {
	case "tool_search":
		return nil
	case "custom":
		itemID := responsesCustomToolItemID(callID)
		return []responsesStreamEvent{
			{"response.custom_tool_call_input.delta", map[string]any{
				"item_id":      itemID,
				"output_index": outputIndex,
				"delta":        call.Function.Arguments,
			}},
			{"response.custom_tool_call_input.done", map[string]any{
				"item_id":      itemID,
				"output_index": outputIndex,
				"input":        call.Function.Arguments,
			}},
		}
	default:
		return []responsesStreamEvent{
			{"response.function_call_arguments.delta", map[string]any{
				"item_id":      callID,
				"output_index": outputIndex,
				"delta":        call.Function.Arguments,
			}},
			{"response.function_call_arguments.done", map[string]any{
				"item_id":      callID,
				"output_index": outputIndex,
				"name":         call.Function.Name,
				"arguments":    call.Function.Arguments,
			}},
		}
	}
}

func buildResponsesToolCallItem(callID string, call client.ToolCall, toolTypes map[string]string, status string) map[string]any {
	toolKey := responsesToolKey(call.Function.Namespace, call.Function.Name)
	if toolTypes[toolKey] == "tool_search" {
		var arguments any
		if err := json.Unmarshal([]byte(call.Function.Arguments), &arguments); err != nil || arguments == nil {
			arguments = map[string]any{"query": call.Function.Arguments}
		}
		return map[string]any{
			"id":        callID,
			"type":      "tool_search_call",
			"execution": "client",
			"status":    status,
			"call_id":   callID,
			"arguments": arguments,
		}
	}
	if toolTypes[toolKey] == "custom" {
		// A custom tool takes free-form input rather than JSON arguments, so it
		// travels as its own item type.
		item := map[string]any{
			"id":      responsesCustomToolItemID(callID),
			"type":    "custom_tool_call",
			"status":  status,
			"call_id": callID,
			"name":    call.Function.Name,
			"input":   "",
		}
		if status == "completed" {
			item["input"] = call.Function.Arguments
		}
		return item
	}
	item := map[string]any{
		"id":      callID,
		"type":    "function_call",
		"status":  status,
		"call_id": callID,
		"name":    call.Function.Name,
	}
	if call.Function.Namespace != "" {
		item["namespace"] = call.Function.Namespace
	}
	if status == "completed" {
		item["arguments"] = call.Function.Arguments
	} else {
		item["arguments"] = ""
	}
	return item
}

func resolveResponsesToolNamespace(
	name string,
	namespace string,
	tools []toolcalling.ToolDef,
) (string, bool) {
	namespaces := make(map[string]bool)
	for _, tool := range tools {
		if responsesToolName(tool) != name {
			continue
		}
		if namespace != "" {
			if tool.Namespace == namespace {
				return namespace, true
			}
			continue
		}
		namespaces[tool.Namespace] = true
	}
	if namespace != "" || len(namespaces) != 1 {
		return "", false
	}
	for candidate := range namespaces {
		return candidate, true
	}
	return "", false
}

func shouldResetResponsesSession(content string, toolCalls []client.ToolCall, err error) bool {
	return err != nil || (strings.TrimSpace(content) == "" && len(toolCalls) == 0)
}

func parseResponsesSimulation(text string, policy responsesToolPolicy) (responsesSimulationResult, error) {
	result := responsesSimulationResult{
		content:      text,
		finishReason: "stop",
	}
	simulated := toolcalling.ParseSimulatedResponseResponses(text, policy.allowedToolNames, toolcalling.ContractsFor(policy.tools).WithoutParallel(policy.noParallel))
	simulated = claimGrammarBodyCall(text, simulated, policy)
	if !policy.required {
		simulated = dropSettledToolCalls(policy.ledger, "", simulated)
	}
	if simulated.HasPayload {
		result.content = simulated.Content
		if len(simulated.ToolCalls) > 0 {
			result.finishReason = "tool_calls"
			result.toolCalls = responsesToolCallsFrom(simulated.ToolCalls, policy)
		}
	}
	if len(result.toolCalls) > 0 && strings.TrimSpace(result.content) == "" {
		result.content = "I'm using the relevant tool now and will continue with its result."
	}

	if err := policy.requireEmittedCall(result.toolCalls); err != nil {
		return responsesSimulationResult{}, err
	}
	return result, nil
}

// claimGrammarBodyCall claims an unfenced grammar tool body as a call.
//
// A grammar tool's body arrives unfenced, either as the lone bridge envelope or
// as bare source. It may be the whole reply, or it may end up as the extracted
// content of an otherwise valid envelope; either way it would reach the client
// as escaped source in an assistant message instead of a call. The candidate is
// whatever text is about to be forwarded.
func claimGrammarBodyCall(text string, simulated toolcalling.SimulatedResult, policy responsesToolPolicy) toolcalling.SimulatedResult {
	if len(simulated.ToolCalls) > 0 {
		return simulated
	}
	candidate := text
	if simulated.HasPayload {
		candidate = simulated.Content
	}
	call, ok := toolcalling.GrammarBodyCall(candidate, policy.tools, policy.allows)
	if !ok {
		return simulated
	}
	logging.Infof("parseResponsesSimulation: claimed an unfenced grammar body as a %q call", call.Name)
	simulated.HasPayload = true
	simulated.FinishReason = "tool_calls"
	simulated.Content = ""
	simulated.ToolCalls = []toolcalling.ToolCall{call}
	return simulated
}

// responsesToolCallsFrom resolves each parsed call's namespace and drops the
// ones that name no declared tool.
func responsesToolCallsFrom(parsedCalls []toolcalling.ToolCall, policy responsesToolPolicy) []client.ToolCall {
	var calls []client.ToolCall
	for _, parsed := range parsedCalls {
		namespace, ok := resolveResponsesToolNamespace(
			parsed.Name,
			parsed.Namespace,
			policy.tools,
		)
		if !ok {
			continue
		}
		calls = append(calls, client.ToolCall{
			ID:   parsed.ID,
			Type: "function",
			Function: client.ToolCallFunction{
				Name:      parsed.Name,
				Namespace: namespace,
				Arguments: string(parsed.Arguments),
			},
		})
	}
	return calls
}

// requireEmittedCall enforces a tool_choice that demanded a call.
func (p responsesToolPolicy) requireEmittedCall(toolCalls []client.ToolCall) error {
	if !p.required || len(toolCalls) > 0 {
		return nil
	}
	if p.requiredName != "" {
		return fmt.Errorf("%w: required tool %q was not emitted", errSimulatedToolCallRequired, p.requiredName)
	}
	return fmt.Errorf("%w: no valid client tool call was emitted", errSimulatedToolCallRequired)
}

func parseResponsesSimulationWithRetry(
	text string,
	policy responsesToolPolicy,
	requiredRetry func() (string, error),
	emptyRetry func() (string, error),
) (responsesSimulationResult, error) {
	result, err := parseResponsesSimulation(text, policy)
	if err == nil {
		return retryEmptyResponsesSimulation(result, policy, emptyRetry)
	}
	if requiredRetry == nil ||
		!errors.Is(err, errSimulatedToolCallRequired) {
		return result, err
	}
	return retryRequiredResponsesSimulation(result, err, policy, requiredRetry)
}

// retryEmptyResponsesSimulation asks again when the turn parsed cleanly but
// carried neither content nor a call.
func retryEmptyResponsesSimulation(
	result responsesSimulationResult,
	policy responsesToolPolicy,
	emptyRetry func() (string, error),
) (responsesSimulationResult, error) {
	if emptyRetry == nil ||
		!responsesResultEmpty(result.content, result.toolCalls) {
		return result, nil
	}
	retryText, retryErr := emptyRetry()
	if retryErr != nil {
		return responsesSimulationResult{}, fmt.Errorf(
			"empty simulated response retry failed: %w",
			retryErr,
		)
	}
	return parseResponsesSimulation(retryText, policy)
}

// retryRequiredResponsesSimulation asks again when tool_choice demanded a call
// the model did not emit. Two further attempts, then the failure stands.
func retryRequiredResponsesSimulation(
	result responsesSimulationResult,
	err error,
	policy responsesToolPolicy,
	requiredRetry func() (string, error),
) (responsesSimulationResult, error) {
	for range 2 {
		retryText, retryErr := requiredRetry()
		if retryErr != nil {
			return responsesSimulationResult{}, fmt.Errorf(
				"%w: retry failed: %v",
				errSimulatedToolCallRequired,
				retryErr,
			)
		}
		result, err = parseResponsesSimulation(retryText, policy)
		if err == nil || !errors.Is(err, errSimulatedToolCallRequired) {
			return result, err
		}
	}
	return result, err
}

func responsesSimulationRetryMessages(
	messages []payload.Message,
	policy responsesToolPolicy,
) []payload.Message {
	retryInstruction := "RETRY: The previous result was invalid. "
	if policy.requiredName != "" {
		retryInstruction += fmt.Sprintf(
			"Return exactly one valid tool call named %q inside the required chat-completion JSON envelope. Plain content is invalid.",
			policy.requiredName,
		)
	} else {
		retryInstruction += fmt.Sprintf(
			"Return at least one valid tool call using only these client tools: %s. Plain content is invalid.",
			strings.Join(policy.allowedToolNames, ", "),
		)
	}
	retryInstruction += " Every tool call MUST include all of its schema-required fields, each with a concrete non-empty value; a tool call with a missing or empty required field is invalid."

	retried := append([]payload.Message(nil), messages...)
	for index := range slices.Backward(retried) {
		if retried[index].Role == "user" {
			retried[index].Content += "\n\n" + retryInstruction
			return retried
		}
	}
	return append(retried, payload.Message{
		Role:    "user",
		Content: retryInstruction,
	})
}

func responsesReasoningForOutput(thinking string, simulated bool) string {
	if simulated {
		return ""
	}
	return thinking
}

func responsesResultEmpty(text string, toolCalls []client.ToolCall) bool {
	return strings.TrimSpace(text) == "" && len(toolCalls) == 0
}

var responsesPlainEmptyRetryDelays = []time.Duration{
	10 * time.Second,
	30 * time.Second,
}

func responsesEmptyRetrySchedule(simulateTools bool) []time.Duration {
	if simulateTools {
		return responsesPlainEmptyRetryDelays[:1]
	}
	return responsesPlainEmptyRetryDelays
}

type responsesConversationResult struct {
	text           string
	thinking       string
	toolCalls      []client.ToolCall
	finishReason   string
	conversationID string
}

type responsesConversationCall func(
	context.Context,
	string,
) (responsesConversationResult, error)

func waitForResponsesEmptyRetry(
	ctx context.Context,
	delay time.Duration,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func responsesConversationWithEmptyRetry(
	ctx context.Context,
	initialConversationID string,
	retryDelays []time.Duration,
	onRetry func(),
	call responsesConversationCall,
) (responsesConversationResult, error) {
	conversationID := initialConversationID
	for attempt := 0; ; attempt++ {
		result, err := call(ctx, conversationID)
		if err != nil ||
			!responsesResultEmpty(result.text, result.toolCalls) ||
			attempt >= len(retryDelays) {
			return result, err
		}

		logging.Warnf(
			"Responses upstream completed empty; retrying attempt=%d/%d",
			attempt+2,
			len(retryDelays)+1,
		)
		if onRetry != nil {
			onRetry()
		}
		if err := waitForResponsesEmptyRetry(
			ctx,
			retryDelays[attempt],
		); err != nil {
			return responsesConversationResult{}, err
		}
		conversationID = ""
	}
}

type responsesStreamCall func(
	context.Context,
	string,
) <-chan client.StreamChunk

func responsesStreamWithEmptyRetry(
	ctx context.Context,
	initialConversationID string,
	retryDelays []time.Duration,
	simulatedTransport bool,
	onRetry func(),
	call responsesStreamCall,
) <-chan client.StreamChunk {
	output := make(chan client.StreamChunk)
	go func() {
		defer close(output)
		conversationID := initialConversationID
		emit := func(chunk client.StreamChunk) bool {
			select {
			case output <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

		for attempt := 0; ; attempt++ {
			result := forwardResponsesAttempt(
				call(ctx, conversationID),
				emit,
				simulatedTransport,
				attempt >= len(retryDelays),
			)
			if result.stop {
				return
			}
			if !result.sawFinal {
				if ctx.Err() == nil {
					emit(client.StreamChunk{Error: client.ErrConnectionClosed})
				}
				return
			}
			if !prepareResponsesRetry(ctx, attempt, retryDelays, onRetry) {
				return
			}
			conversationID = ""
		}
	}()
	return output
}

// responsesAttempt is what one upstream attempt produced.
type responsesAttempt struct {
	// sawVisibleChunk records that the caller received answer content.
	sawVisibleChunk bool
	// sawFinal records that the upstream turn ended rather than the connection.
	sawFinal bool
	// stop means nothing further is owed to the caller.
	stop bool
}

// visibleChunk reports whether a chunk carries anything the caller can show.
func visibleChunk(chunk client.StreamChunk) bool {
	return chunk.Text != "" || chunk.Thinking != ""
}

// forwardResponsesAttempt forwards one upstream attempt to the caller.
//
// lastAttempt makes an empty turn terminal rather than a reason to retry.
func forwardResponsesAttempt(stream <-chan client.StreamChunk, emit func(client.StreamChunk) bool, simulatedTransport, lastAttempt bool) responsesAttempt {
	var result responsesAttempt
	for chunk := range stream {
		if chunk.Error != nil {
			emit(chunk)
			result.stop = true
			return result
		}
		if chunk.IsFinal {
			result.sawFinal = true
			if result.sawVisibleChunk || lastAttempt {
				emit(chunk)
				result.stop = true
			}
			return result
		}
		if simulatedTransport {
			// Simulated prompts can put the transport envelope in the upstream
			// thinking channel. The Responses handler only needs raw text for
			// its safe content extractor.
			chunk.Thinking = ""
		}
		if !visibleChunk(chunk) {
			continue
		}
		result.sawVisibleChunk = true
		if !emit(chunk) {
			result.stop = true
			return result
		}
	}
	return result
}

// prepareResponsesRetry logs the retry, tells the caller it is happening and
// waits out the backoff. It reports false when the turn must end instead.
func prepareResponsesRetry(ctx context.Context, attempt int, retryDelays []time.Duration, onRetry func()) bool {
	if attempt >= len(retryDelays) {
		return false
	}
	logging.Warnf(
		"Responses upstream stream completed empty; retrying attempt=%d/%d",
		attempt+2,
		len(retryDelays)+1,
	)
	if onRetry != nil {
		onRetry()
	}
	return waitForResponsesEmptyRetry(ctx, retryDelays[attempt]) == nil
}

// newResponsesIdentity mints the id and the creation time of one Responses
// answer. The two travel together because every lifecycle event of that answer
// reports the same pair; a time taken per event would let a client read two
// different creation times for one response.
func newResponsesIdentity() (string, int64) {
	return fmt.Sprintf("resp_%s", uuid.New().String()), time.Now().Unix()
}

// responsesStatusObject builds the Response object a lifecycle event carries
// before the answer exists. The format defines created_at on every Response
// object, including the partial one on response.created, so a client that
// reads the field from the first event finds it there.
func responsesStatusObject(responseID, model, status string, createdAt int64) map[string]any {
	return map[string]any{
		"id":         responseID,
		"object":     "response",
		"created_at": createdAt,
		"status":     status,
		"model":      model,
	}
}

func buildResponsesFailedEvent(
	responseID string,
	createdAt int64,
	model, code, message string,
	sequenceNumber int,
) map[string]any {
	return map[string]any{
		"type":            "response.failed",
		"sequence_number": sequenceNumber,
		"response": map[string]any{
			"id":         responseID,
			"object":     "response",
			"created_at": createdAt,
			"status":     "failed",
			"model":      model,
			"error": map[string]any{
				"message": message,
				"type":    "server_error",
				"code":    code,
			},
		},
	}
}

// writeResponsesServerError reports a failure before any answer exists. The
// response identity is only read on the streaming branch, which carries it in
// the failed event; the non-streaming branch answers with a plain error body.
// A caller with no identity to give passes an empty id and a zero time, the
// pair that means the same thing.
func writeResponsesServerError(w http.ResponseWriter, stream bool, responseID string, createdAt int64, model, code, message string) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		event := buildResponsesFailedEvent(
			responseID,
			createdAt,
			model,
			code,
			message,
			0,
		)
		jsonData, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", jsonData)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusBadGateway)
	writeJSONBody(w, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "server_error",
			"code":    code,
		},
	})
}

func writeResponsesSimulationError(w http.ResponseWriter, stream bool, responseID string, createdAt int64, model string, err error) {
	writeResponsesServerError(
		w,
		stream,
		responseID,
		createdAt,
		model,
		simulatedToolCallRequiredCode,
		err.Error(),
	)
}

func writeResponsesUpstreamEmptyError(w http.ResponseWriter, stream bool, responseID string, createdAt int64, model string) {
	writeResponsesServerError(
		w,
		stream,
		responseID,
		createdAt,
		model,
		upstreamEmptyResponseCode,
		"M365 returned an empty response without a completion message",
	)
}

// parseModelSessionID splits a model string of the form "modelKey:sessionID"
// into its components. If there is no colon, sessionID is empty.
// This allows clients that cannot send custom headers/body fields (e.g. Droid
// CLI) to encode a session ID directly in the model name, e.g.
// "gpt5.5-reasoning:dev-test-session-001".
//
// An empty model key defaults to "gpt5.5-reasoning", the reasoning tone that is
// reliable for tool calling, rather than falling back to the "auto" (Magic)
// tone. This keeps the text endpoints consistent with the conversation and
// image routes, which already default empty models to the same key.
func parseModelSessionID(model string) (modelKey, sessionID string) {
	modelKey, sessionID, found := strings.Cut(model, ":")
	if !found {
		modelKey = model
	}
	if modelKey == "" {
		modelKey = "gpt5.5-reasoning"
	}
	return modelKey, sessionID
}

// toolNamesFromDefs extracts the function names from a slice of tool
// definitions, for filtering M365-invented tool calls (e.g. code_interpreter)
// out of simulated responses.
func toolNamesFromDefs(tools []toolcalling.ToolDef) []string {
	return responsesToolNames(tools)
}

// fimToChat converts FIM (fill-in-the-middle) prompts to chat format.
func (api *APIServer) fimToChat(prompt, suffix string) []payload.Message {
	if suffix != "" {
		return []payload.Message{
			{
				Role:    "user",
				Content: fmt.Sprintf("Complete the middle of the following text naturally.\n\n--- BEGIN TEXT ---\n%s\n--- MIDDLE ---\n%s\n--- END ---\n\nWrite only the middle part that connects the two sections.", prompt, suffix),
			},
		}
	}

	return []payload.Message{
		{
			Role:    "user",
			Content: fmt.Sprintf("Continue writing from this point:\n\n%s", prompt),
		},
	}
}

// tokenEncoder is the tiktoken encoder used for every token count. o200k_base
// is the encoding of the GPT-5 family the backend serves; cl100k_base is kept
// as a fallback because the vocabulary is fetched at first use and the fetch
// can fail.
var tokenEncoder *tiktoken.Tiktoken

// tokenEncodingName names the encoding actually in use, for usage reporting.
var tokenEncodingName string

// Usage source values reported alongside the token counts, so a caller can tell
// a real BPE count from the character estimate that stands in when the
// vocabulary could not be fetched.
const (
	usageSourceHeuristic = "heuristic_character_estimate"
)

func init() {
	for _, name := range []string{"o200k_base", "cl100k_base"} {
		enc, err := tiktoken.GetEncoding(name)
		if err != nil {
			logging.Warnf("token encoding %s unavailable: %v", name, err)
			continue
		}
		tokenEncoder = enc
		tokenEncodingName = name
		break
	}
	if tokenEncoder == nil {
		logging.Warn("no token encoding available, falling back to a character estimate")
	}
}

// usageSource reports how the token counts in a response were produced.
func usageSource() string {
	if tokenEncoder == nil {
		return usageSourceHeuristic
	}
	return "tiktoken_" + tokenEncodingName + "_estimate"
}

// heuristicTokenCount estimates tokens from character classes when no encoding
// is available. Latin text averages roughly four characters per token while
// CJK and other non-ASCII scripts average closer to one, so a single divisor
// would be wrong for one of them.
func heuristicTokenCount(text string) int {
	ascii, other := 0, 0
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		if r <= 0x7f {
			ascii++
		} else {
			other++
		}
	}
	if ascii == 0 && other == 0 {
		return 0
	}
	return max(ascii/4+other, 1)
}

// countTokens returns the real BPE token count using tiktoken.
func countTokens(text string) int {
	if strings.TrimSpace(text) == "" {
		return 0
	}
	if tokenEncoder != nil {
		return len(tokenEncoder.Encode(text, nil, nil))
	}
	return heuristicTokenCount(text)
}

// Protocol framing costs. The request carries structure the message text does
// not represent: role markers, tool schemas and the priming that starts the
// reply. These are conservative estimates of that framing, not billing figures
// from the backend.
const (
	requestProtocolTokens    = 4
	messageProtocolTokens    = 4
	toolProtocolTokens       = 6
	toolChoiceProtocolTokens = 2
	replyPrimingTokens       = 3
	outputProtocolTokens     = 3
)

// countPromptTokens estimates the prompt cost of a request from its parts.
//
// The previous count ran tiktoken over fmt.Sprint of the message slice, which
// counted Go struct field names and slice punctuation as prompt content. Tools
// and tool_choice were not counted at all even though they travel in the
// request.
func countPromptTokens(messages []payload.Message, tools []toolcalling.ToolDef, toolChoice string) int {
	total := requestProtocolTokens + replyPrimingTokens
	for i := range messages {
		total += messageProtocolTokens + countTokens(messages[i].Role) + countTokens(messages[i].Content)
	}
	for i := range tools {
		total += toolProtocolTokens
		if encoded, err := json.Marshal(tools[i]); err == nil {
			total += countTokens(string(encoded))
		}
	}
	// A tool choice only travels with the tools it selects from. The Responses
	// policy defaults promptChoice to "auto" even for a request that declares
	// none, so billing it unconditionally would charge every toolless turn for
	// framing the backend never received.
	if len(tools) > 0 && strings.TrimSpace(toolChoice) != "" {
		total += toolChoiceProtocolTokens
	}
	return total
}

// openAIUsage builds the usage object for the OpenAI wire format. The buffered
// coding-tool responder has no streaming loop to accumulate counts in, so it
// takes the finished text and counts it the same way the streaming handlers do.
func openAIUsage(messages []payload.Message, tools []toolcalling.ToolDef, toolChoice, answer, thinking string) map[string]any {
	promptTok := countPromptTokens(messages, tools, toolChoice)
	completionTok := countTokens(answer) + outputProtocolTokens
	reasoningTok := countTokens(thinking)
	return map[string]any{
		"prompt_tokens":     promptTok,
		"completion_tokens": completionTok,
		"reasoning_tokens":  reasoningTok,
		"total_tokens":      promptTok + completionTok + reasoningTok,
		"usage_source":      usageSource(),
	}
}

// anthropicUsage builds the usage object for the Anthropic wire format. The
// field names differ from OpenAI's and the format carries no total, but the
// counts behind them are the same.
func anthropicUsage(messages []payload.Message, tools []toolcalling.ToolDef, toolChoice, answer, thinking string) map[string]any {
	return map[string]any{
		"input_tokens":     countPromptTokens(messages, tools, toolChoice),
		"output_tokens":    countTokens(answer) + outputProtocolTokens,
		"reasoning_tokens": countTokens(thinking),
		"usage_source":     usageSource(),
	}
}

// truncateToTokens truncates text to at most maxTokens tokens using tiktoken.
// Returns the truncated text and true if truncation occurred.
func truncateToTokens(text string, maxTokens int) (string, bool) {
	if maxTokens <= 0 {
		return text, false
	}
	if tokenEncoder != nil {
		tokens := tokenEncoder.Encode(text, nil, nil)
		if len(tokens) <= maxTokens {
			return text, false
		}
		return tokenEncoder.Decode(tokens[:maxTokens]), true
	}
	// Without an encoder there is no token boundary to cut on, so the word
	// split stands in; heuristicTokenCount decides whether a cut is needed.
	if heuristicTokenCount(text) <= maxTokens {
		return text, false
	}
	words := strings.Split(text, " ")
	if len(words) <= maxTokens {
		return text, false
	}
	return strings.Join(words[:maxTokens], " "), true
}

func limitResponsesStreamDelta(
	published string,
	delta string,
	maxTokens int,
) (string, string, bool) {
	if delta == "" {
		return "", published, false
	}
	if maxTokens <= 0 ||
		countTokens(published+delta) <= maxTokens {
		return delta, published + delta, false
	}
	remaining := maxTokens - countTokens(published)
	if remaining <= 0 {
		return "", published, true
	}
	limited, _ := truncateToTokens(delta, remaining)
	return limited, published + limited, true
}

// ===================================================================
// OpenAI Responses API (/v1/responses)
// ===================================================================

// responsesRequest is the JSON body for POST /v1/responses.
type responsesRequest struct {
	Model              string                `json:"model"`
	Input              any                   `json:"input"`
	Instructions       string                `json:"instructions"`
	Stream             bool                  `json:"stream"`
	MaxOutputTokens    int                   `json:"max_output_tokens"`
	Tools              []toolcalling.ToolDef `json:"tools"`
	ToolChoice         any                   `json:"tool_choice"`
	Temperature        float64               `json:"temperature"`
	PreviousResponseID string                `json:"previous_response_id"`
	SessionID          string                `json:"session_id"`
	User               string                `json:"user"`
	Metadata           map[string]any        `json:"metadata"`
	Reasoning          *responsesReasoning   `json:"reasoning"`
	// A pointer, because an absent parallel_tool_calls means the OpenAI
	// default of true rather than false.
	ParallelToolCalls *bool `json:"parallel_tool_calls"`
}

// responsesReasoning is the reasoning block Codex CLI sends. M365 decides how
// much it deliberates through the tone rather than through a knob, so effort
// steers the tone choice and summary is accepted but not acted on.
type responsesReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

// handleResponses handles OpenAI Responses API requests.
func (api *APIServer) handleResponses(w http.ResponseWriter, r *http.Request) {
	var req responsesRequest
	bodyBytes, ok := api.readJSONRequest(w, r, &req, "handleResponses")
	if !ok {
		return
	}

	if responsesInputHasCompactionTrigger(req.Input) {
		logging.Infof("handleResponses: detected compaction_trigger; routing to /v1/responses/compact")
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		api.handleResponsesCompact(w, r)
		return
	}

	// Parse model (may contain session ID suffix: "gpt5.5:my-session")
	modelKey, modelSessionID := parseModelSessionID(req.Model)
	cfg, ok := api.responsesModelConfig(w, modelKey, req.Reasoning)
	if !ok {
		return
	}

	toolPolicy, localTools, ok := api.responsesToolSetup(w, &req)
	if !ok {
		return
	}
	requestJSON := replaceRequestTools(bodyBytes, req.Tools)

	// Convert Responses API input to payload.Message list
	messages := responsesInputToMessages(req.Input)

	if api.answeredResponsesProbe(w, req, cfg, messages) {
		return
	}

	ledger, ok := api.prepareToolLedger(w, messages, "handleResponses")
	if !ok {
		return
	}
	// The simulation prompt collapses the input into one message, so the
	// evidence has to travel with the policy to reach the parser.
	toolPolicy.ledger = ledger

	messages = prependResponsesInstructions(messages, req.Instructions)

	// Inject one Responses-aware simulation prompt unless tool_choice disables
	// client tool use.
	if toolPolicy.simulate {
		injectSimulatedPromptResponses(&messages, requestJSON, toolPolicy.promptChoice, toolPolicy.ledger.EvidenceNote())
	}

	sid, convID := api.sessionAndConversation(r, sessionSources{
		ModelSuffix:        modelSessionID,
		PreviousResponseID: req.PreviousResponseID,
		BodySessionID:      req.SessionID,
		BodyUser:           req.User,
	}, messages)

	// Upload any images found in multimodal content
	api.uploadImagesAndAnnotate(&messages, convID)

	// Read once here rather than at each phase decision; the answer depends on
	// the request alone and does not change while the response is produced.
	goalOpen := responsesGoalContinuationOpen(req.Input)

	if len(localTools) > 0 {
		api.runResponsesToolLoop(w, r, req, messages, cfg, sid, convID, toolPolicy, localTools, goalOpen)
		return
	}
	api.dispatchResponses(r.Context(), w, messages, cfg, sid, convID, req.MaxOutputTokens, req.Stream, toolPolicy, goalOpen)
}

// answeredResponsesProbe answers Codex CLI's reachability probe, which is a
// POST carrying no input at all. Answering it here avoids a round trip and one
// message of the conversation quota.
func (api *APIServer) answeredResponsesProbe(w http.ResponseWriter, req responsesRequest, cfg models.ModelConfig, messages []payload.Message) bool {
	if strings.TrimSpace(req.Instructions) != "" || !responsesInputIsEmpty(messages) {
		return false
	}
	api.respondResponsesProbe(w, cfg.OpenAIID, req.Stream)
	return true
}

// dispatchResponses routes a Responses turn to the streaming or the buffered
// responder.
func (api *APIServer) dispatchResponses(ctx context.Context, w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxOutputTokens int, stream bool, toolPolicy responsesToolPolicy, goalOpen bool) {
	if stream {
		api.streamResponses(ctx, w, messages, cfg, sid, convID, maxOutputTokens, toolPolicy, goalOpen)
		return
	}
	api.nonStreamResponses(ctx, w, messages, cfg, sid, convID, maxOutputTokens, toolPolicy, goalOpen)
}

// responsesModelConfig resolves the model and applies the reasoning effort the
// request asked for. ok is false when the request has already been answered.
func (api *APIServer) responsesModelConfig(w http.ResponseWriter, modelKey string, reasoning *responsesReasoning) (models.ModelConfig, bool) {
	cfg, ok := api.resolveModel(w, modelKey)
	if !ok {
		return models.ModelConfig{}, false
	}
	deliberate, err := reasoningEffortRequestsDeliberation(reasoning)
	if err != nil {
		logging.Errorf("handleResponses: %v", err)
		api.sendError(w, http.StatusBadRequest, err.Error())
		return models.ModelConfig{}, false
	}
	return applyReasoningEffort(modelKey, cfg, deliberate), true
}

// responsesToolSetup merges the loaded tools, separates the built-in ones and
// builds the request's tool policy. ok is false when the request has already
// been answered.
func (api *APIServer) responsesToolSetup(w http.ResponseWriter, req *responsesRequest) (responsesToolPolicy, map[string]bool, bool) {
	req.Tools = mergeLoadedResponsesTools(req.Input, req.Tools)
	preparedTools, localTools := api.prepareCodingTools(req.Tools, false)
	req.Tools = preparedTools

	toolPolicy, err := newResponsesToolPolicy(req.Tools, req.ToolChoice)
	if err != nil {
		api.sendError(w, http.StatusBadRequest, err.Error())
		return responsesToolPolicy{}, nil, false
	}
	toolPolicy.noParallel = refusesParallelToolCalls(req.ParallelToolCalls)
	return toolPolicy, localTools, true
}

// prependResponsesInstructions puts the instructions first as a user message,
// because M365 has no system role.
func prependResponsesInstructions(messages []payload.Message, instructions string) []payload.Message {
	trimmed := strings.TrimSpace(instructions)
	if trimmed == "" || len(messages) == 0 {
		return messages
	}
	instrMsg := payload.Message{
		Role:    "user",
		Content: "Instructions: " + trimmed,
	}
	return append([]payload.Message{instrMsg}, messages...)
}

// runResponsesToolLoop runs the built-in coding tool loop for a Responses
// request, which answers whole because the loop takes several turns.
func (api *APIServer) runResponsesToolLoop(w http.ResponseWriter, r *http.Request, req responsesRequest, messages []payload.Message, cfg models.ModelConfig, sid, convID string, toolPolicy responsesToolPolicy, localTools map[string]bool, goalOpen bool) {
	result, err := api.runToolLoop(r, toolLoopOpenAI, messages, cfg, sid, convID, req.Tools, toolPolicy.noParallel, localTools)
	if err != nil {
		api.sendUpstreamError(w, "response", err)
		return
	}
	api.respondBufferedResponses(
		w,
		result,
		messages,
		cfg,
		req.MaxOutputTokens,
		req.Stream,
		responsesToolTypes(toolPolicy.tools),
		goalOpen,
		toolPolicy.tools,
		toolPolicy.promptChoice,
	)
}

// responsesInputToMessages converts the Responses API input field (string or
// array of input items) to a slice of payload.Message.
func responsesInputToMessages(input any) []payload.Message {
	if input == nil {
		return []payload.Message{{Role: "user", Content: ""}}
	}

	// Simple string input
	if s, ok := input.(string); ok {
		return []payload.Message{{Role: "user", Content: s}}
	}

	// Array input
	arr, ok := input.([]any)
	if !ok {
		return []payload.Message{{Role: "user", Content: ""}}
	}

	var messages []payload.Message
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if message, ok := responsesInputItem(m); ok {
			messages = append(messages, message)
		}
	}

	if len(messages) == 0 {
		return []payload.Message{{Role: "user", Content: ""}}
	}
	return messages
}

// responsesInputItem converts one input item. An item type that contributes
// nothing to the turn reports false.
func responsesInputItem(m map[string]any) (payload.Message, bool) {
	itemType, _ := m["type"].(string)
	switch itemType {
	case "compaction_trigger", "reasoning":
		// A compaction trigger is a request rather than history, and M365
		// generates its own reasoning.
		return payload.Message{}, false
	case "custom_tool_call_output", "function_call_output":
		// A custom tool reports its result the same way as a function, only
		// under its own item type.
		return responsesToolResultMessage(m, itemType == "function_call_output"), true
	case "custom_tool_call":
		return responsesCustomToolCallMessage(m), true
	case "function_call_progress":
		return responsesToolProgressMessage(m)
	case "function_call":
		return responsesFunctionCallMessage(m), true
	case "tool_search_call":
		return responsesToolSearchCallMessage(m), true
	case "tool_search_output", "additional_tools":
		return responsesLoadedToolsMessage(itemType, m), true
	case "compaction":
		return responsesCompactionMessage(m)
	}
	return responsesPlainMessage(m), true
}

// responsesToolResultMessage reads a tool result item. encodeNonString covers
// function_call_output, whose output may arrive as a structure rather than a
// string; a custom tool always reports its result as text.
func responsesToolResultMessage(m map[string]any, encodeNonString bool) payload.Message {
	callID, _ := m["call_id"].(string)
	output, _ := m["output"].(string)
	if encodeNonString && output == "" && m["output"] != nil {
		encoded, _ := json.Marshal(m["output"])
		output = string(encoded)
	}
	return payload.Message{
		Role: "tool",
		Content: fmt.Sprintf(
			"Authoritative tool result (call_id: %s):\n%s",
			callID,
			output,
		),
		ToolCallID:  callID,
		ToolResults: []payload.ToolResultRecord{{ID: callID, Content: output}},
	}
}

// responsesCustomToolCallMessage reads a custom tool call out of the history.
func responsesCustomToolCallMessage(m map[string]any) payload.Message {
	name, _ := m["name"].(string)
	input, _ := m["input"].(string)
	callID, _ := m["call_id"].(string)
	return payload.Message{
		Role:      "assistant",
		Content:   fmt.Sprintf("Tool call: %s(%s)", name, input),
		ToolCalls: []payload.ToolCallRecord{{ID: callID, Name: name, Arguments: input}},
	}
}

// responsesToolProgressMessage reads an intermediate progress report.
//
// A long-running client tool can report progress before it has a result. The
// item is transport metadata: it must reach the model as context but must never
// satisfy the pending call, or the loop would continue on an unfinished tool.
func responsesToolProgressMessage(m map[string]any) (payload.Message, bool) {
	callID, _ := m["call_id"].(string)
	message, _ := m["message"].(string)
	if strings.TrimSpace(callID) == "" || strings.TrimSpace(message) == "" {
		logging.Debugf("responsesInputToMessages: dropping a function_call_progress item without call_id or message")
		return payload.Message{}, false
	}
	phase, _ := m["phase"].(string)
	if phase == "" {
		phase = "running"
	}
	text := fmt.Sprintf("[Tool Progress (call_id: %s, phase: %s)]\n%s", callID, phase, message)
	if output, _ := m["output"].(string); output != "" {
		text += "\n" + output
	}
	// The role stays "user": a "tool" role would be flattened and counted as a
	// result by the history and the evidence ledger.
	return payload.Message{Role: "user", Content: text, ToolProgress: true}, true
}

// responsesFunctionCallMessage reads an assistant tool call out of the history.
func responsesFunctionCallMessage(m map[string]any) payload.Message {
	name, _ := m["name"].(string)
	namespace, _ := m["namespace"].(string)
	args, _ := m["arguments"].(string)
	callID, _ := m["call_id"].(string)
	qualifiedName := name
	if namespace != "" {
		qualifiedName = namespace + "/" + name
	}
	return payload.Message{
		Role:      "assistant",
		Content:   fmt.Sprintf("Tool call: %s(%s)", qualifiedName, args),
		ToolCalls: []payload.ToolCallRecord{{ID: callID, Name: qualifiedName, Arguments: args}},
	}
}

// responsesToolSearchCallMessage reads a tool_search call out of the history.
func responsesToolSearchCallMessage(m map[string]any) payload.Message {
	arguments, _ := json.Marshal(m["arguments"])
	return payload.Message{
		Role:    "assistant",
		Content: fmt.Sprintf("Tool search call: tool_search(%s)", string(arguments)),
	}
}

// responsesLoadedToolsMessage preserves the tools a search or an explicit list
// loaded, with the namespace and schema the client declared.
func responsesLoadedToolsMessage(itemType string, m map[string]any) payload.Message {
	toolsJSON, _ := json.Marshal(m["tools"])
	if itemType == "additional_tools" {
		return payload.Message{
			Role:    "tool",
			Content: "additional_tools: preserve these callable tools with their exact namespace, name, and schema: " + string(toolsJSON),
		}
	}
	return payload.Message{
		Role:    "tool",
		Content: "tool_search_output: preserve these loaded tools with their exact namespace, name, and schema: " + string(toolsJSON),
	}
}

// responsesCompactionMessage reads a compaction summary of the earlier turns.
func responsesCompactionMessage(m map[string]any) (payload.Message, bool) {
	summary, _ := m["encrypted_content"].(string)
	if strings.TrimSpace(summary) == "" {
		return payload.Message{}, false
	}
	return payload.Message{
		Role:    "user",
		Content: "Summary of the earlier conversation:\n" + summary,
	}, true
}

// responsesPlainMessage reads a message item, which is any item that carries a
// role rather than one of the tool item types.
func responsesPlainMessage(m map[string]any) payload.Message {
	role, _ := m["role"].(string)
	if role == "" {
		role = "user"
	}
	return responsesContentMessage(role, m["content"])
}

func responsesInputHasCompactionTrigger(input any) bool {
	arr, ok := input.([]any)
	if !ok {
		return false
	}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if itemType, _ := m["type"].(string); itemType == "compaction_trigger" {
			return true
		}
	}
	return false
}

// responsesExtractContent extracts text from a content field that may be a
// string or an array of content parts (input_text, output_text, text types).
// goalContextMarker opens the block Codex puts in a user item while it is
// working towards a goal it set itself.
const goalContextMarker = `<codex_internal_context source="goal">`

// updateGoalToolName is the tool Codex calls to report on that goal. Its output
// carries the status that closes the goal.
const updateGoalToolName = "update_goal"

// responsesGoalContinuationOpen reports whether the request is a turn inside an
// unfinished Codex goal.
//
// Codex marks such a turn in its last user item and reports progress by calling
// update_goal. The goal is closed once one of those calls returns a status of
// complete or blocked; until then the client expects the turn to continue, so
// the assistant message is commentary rather than a final answer.
//
// Both markers are Codex internals rather than part of the Responses API, and
// this behaviour has not been measured against a Codex session here. A request
// that carries neither is unaffected.
func responsesGoalContinuationOpen(input any) bool {
	items, ok := input.([]any)
	if !ok {
		return false
	}

	goalItem := latestGoalMarkerItem(items)
	if goalItem < 0 {
		return false
	}

	rest := items[goalItem+1:]
	return !goalClosedAfter(rest, updateGoalCallIDs(rest))
}

// latestGoalMarkerItem reports the index of the user item that opened the goal.
//
// Only the latest user item counts. A later user item without the marker is a
// new request, and its turn is not part of the earlier goal.
func latestGoalMarkerItem(items []any) int {
	goalItem := -1
	for index, item := range items {
		record, ok := item.(map[string]any)
		if !ok || record["role"] != "user" {
			continue
		}
		if strings.Contains(responsesExtractContent(record["content"]), goalContextMarker) {
			goalItem = index
		} else {
			goalItem = -1
		}
	}
	return goalItem
}

// updateGoalCallIDs collects the ids of the update_goal calls made since the
// goal opened, so their outputs can be told apart from every other tool result.
func updateGoalCallIDs(items []any) map[string]bool {
	updateGoalCalls := map[string]bool{}
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch itemType, _ := record["type"].(string); itemType {
		case "function_call", "custom_tool_call":
		default:
			continue
		}
		name, _ := record["name"].(string)
		callID, _ := record["call_id"].(string)
		if name == updateGoalToolName && callID != "" {
			updateGoalCalls[callID] = true
		}
	}
	return updateGoalCalls
}

// goalClosedAfter reports whether one of the update_goal outputs closed the
// goal, which it does with a status of complete or blocked.
func goalClosedAfter(items []any, updateGoalCalls map[string]bool) bool {
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch itemType, _ := record["type"].(string); itemType {
		case "function_call_output", "custom_tool_call_output":
		default:
			continue
		}
		callID, _ := record["call_id"].(string)
		if !updateGoalCalls[callID] {
			continue
		}
		if goalReportCloses(record["output"]) {
			return true
		}
	}
	return false
}

// goalReportCloses reads the status out of one update_goal output.
func goalReportCloses(rawOutput any) bool {
	output, _ := rawOutput.(string)
	var report struct {
		Goal struct {
			Status string `json:"status"`
		} `json:"goal"`
	}
	if json.Unmarshal([]byte(output), &report) != nil {
		return false
	}
	return report.Goal.Status == "complete" || report.Goal.Status == "blocked"
}

// responsesMessagePhase names the phase of an assistant message item. A turn
// that ends in a tool call is commentary, and so is a turn inside a goal the
// client has not closed.
func responsesMessagePhase(goalOpen bool, toolCalls []client.ToolCall) string {
	if len(toolCalls) > 0 || goalOpen {
		return "commentary"
	}
	return "final_answer"
}

// responsesContentMessage converts one Responses message item into a payload
// message.
//
// It routes the content blocks through payload.Message so an image block
// survives the conversion. responsesExtractContent returns text alone, so a
// message built from it dropped every image the caller sent, and the upload
// step further down the Responses path had nothing left to upload.
func responsesContentMessage(role string, content any) payload.Message {
	encoded, err := json.Marshal(map[string]any{"role": role, "content": content})
	if err == nil {
		var message payload.Message
		if err := json.Unmarshal(encoded, &message); err == nil {
			return message
		}
	}
	// A block shape payload.Message rejects still contributes its text.
	return payload.Message{Role: role, Content: responsesExtractContent(content)}
}

func responsesExtractContent(content any) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	arr, ok := content.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, part := range arr {
		p, ok := part.(map[string]any)
		if !ok {
			continue
		}
		ptype, _ := p["type"].(string)
		if ptype == "input_text" || ptype == "output_text" || ptype == "text" {
			if text, ok := p["text"].(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// responsesInputIsEmpty reports whether a converted Responses input carries no
// work at all: no text, no image, no tool call and no tool result.
//
// responsesInputToMessages returns one empty user message for an empty input,
// so the message count alone cannot tell a probe from a real turn.
func responsesInputIsEmpty(messages []payload.Message) bool {
	for i := range messages {
		m := &messages[i]
		if strings.TrimSpace(m.Content) != "" {
			return false
		}
		if len(m.Images) > 0 || len(m.ToolCalls) > 0 || len(m.ToolResults) > 0 {
			return false
		}
	}
	return true
}

// respondResponsesProbe answers a reachability probe without reaching M365. It
// writes a well-formed but empty Response, streaming or not, and touches no
// session state.
func (api *APIServer) respondResponsesProbe(w http.ResponseWriter, model string, stream bool) {
	responseID, createdAt := newResponsesIdentity()
	response := buildResponsesObject(responseID, createdAt, model, "", "", nil, nil, false, "stop", 0, 0, 0)

	if !stream {
		w.Header().Set("Content-Type", "application/json")
		writeJSONBody(w, response)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	flusher, ok := w.(http.Flusher)
	if !ok {
		api.sendError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	sequenceNumber := 0
	sendEvent := func(eventType string, data map[string]any) {
		data["type"] = eventType
		data["sequence_number"] = sequenceNumber
		sequenceNumber++
		jsonData, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", jsonData)
		flusher.Flush()
	}

	// Codex validates the item lifecycle, so the probe emits the envelope
	// events even though it carries no output item.
	sendEvent("response.created", map[string]any{"response": response})
	sendEvent("response.in_progress", map[string]any{"response": response})
	sendEvent("response.completed", map[string]any{"response": response})
}

// buildResponsesObject constructs the non-streaming Responses API response object.
func buildResponsesObject(responseID string, createdAt int64, model, text, thinking string, toolCalls []client.ToolCall, toolTypes map[string]string, goalOpen bool, finishReason string, promptTok, completionTok, reasoningTok int) map[string]any {
	status := "completed"
	if finishReason == "length" {
		status = "incomplete"
	}

	output := []map[string]any{}
	outputIndex := 0

	// Add reasoning item if thinking is present
	if thinking != "" {
		reasoningID := fmt.Sprintf("rs_%s", responseID)
		output = append(output, map[string]any{
			"id":     reasoningID,
			"type":   "reasoning",
			"status": "completed",
			"summary": []map[string]any{
				{
					"type": "summary_text",
					"text": thinking,
				},
			},
		})
		outputIndex++
	}

	// Add message item with output_text (only if there is text content)
	if text != "" || len(toolCalls) == 0 {
		msgID := fmt.Sprintf("msg_%s", responseID)
		phase := responsesMessagePhase(goalOpen, toolCalls)
		output = append(output, map[string]any{
			"id":     msgID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"phase":  phase,
			"content": []map[string]any{
				{
					"type":        "output_text",
					"text":        text,
					"annotations": []any{},
				},
			},
		})
		outputIndex++
	}

	// Add function_call or built-in client tool items after commentary.
	for _, tc := range toolCalls {
		callID := tc.ID
		if callID == "" {
			callID = "call_" + uuid.NewString()
		}
		output = append(output, buildResponsesToolCallItem(callID, tc, toolTypes, "completed"))
		outputIndex++
	}

	resp := map[string]any{
		"id":          responseID,
		"object":      "response",
		"created_at":  createdAt,
		"status":      status,
		"model":       model,
		"output":      output,
		"output_text": text,
		"usage": map[string]any{
			"input_tokens":     promptTok,
			"output_tokens":    completionTok,
			"reasoning_tokens": reasoningTok,
			"total_tokens":     promptTok + completionTok + reasoningTok,
			"usage_source":     usageSource(),
		},
	}
	return resp
}

// The session's conversation is already stored by runToolLoop, so this
// responder takes no session id.
func (api *APIServer) respondBufferedResponses(w http.ResponseWriter, result toolLoopResult, messages []payload.Message, cfg models.ModelConfig, maxTokens int, stream bool, toolTypes map[string]string, goalOpen bool, tools []toolcalling.ToolDef, toolChoice string) {
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(result.text, maxTokens); ok {
			result.text, result.finishReason = truncated, "length"
		}
	}
	responseID, createdAt := newResponsesIdentity()
	response := buildResponsesObject(responseID, createdAt, cfg.OpenAIID, result.text, result.thinking, result.toolCalls, toolTypes, goalOpen, result.finishReason, countPromptTokens(messages, tools, toolChoice), countTokens(result.text)+outputProtocolTokens, countTokens(result.thinking))
	if !stream {
		api.sendJSON(w, http.StatusOK, response)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range []string{"response.created", "response.in_progress"} {
		data, _ := json.Marshal(map[string]any{"type": event, "response": responsesStatusObject(responseID, cfg.OpenAIID, "in_progress", createdAt)})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
	completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", completed)
}

func (api *APIServer) responsesConversationOnce(
	ctx context.Context,
	messages []payload.Message,
	cfg models.ModelConfig,
	conversationID string,
	simulateTools bool,
) (responsesConversationResult, error) {
	text, thinking, toolCalls, finishReason, finalConversationID, err :=
		api.m365Client.ChatConversationContext(
			ctx,
			messages,
			cfg.Tone,
			cfg.Override,
			conversationID,
			api.config.UserOID,
			api.config.TenantID,
			simulateTools,
		)
	return responsesConversationResult{
		text:           text,
		thinking:       thinking,
		toolCalls:      toolCalls,
		finishReason:   finishReason,
		conversationID: finalConversationID,
	}, err
}

func (api *APIServer) responsesRequestCanceled(
	ctx context.Context,
	sid string,
) bool {
	if ctx.Err() == nil {
		return false
	}
	if sid != "" && api.ctxCache != nil {
		api.ctxCache.Delete(sessionKeyPrefix + sid)
	}
	return true
}

func (api *APIServer) responsesConversation(
	ctx context.Context,
	messages []payload.Message,
	cfg models.ModelConfig,
	conversationID string,
	simulateTools bool,
	onRetry func(),
) (responsesConversationResult, error) {
	return responsesConversationWithEmptyRetry(
		ctx,
		conversationID,
		responsesEmptyRetrySchedule(simulateTools),
		onRetry,
		func(
			callContext context.Context,
			callConversationID string,
		) (responsesConversationResult, error) {
			result, err := api.responsesConversationOnce(
				callContext,
				messages,
				cfg,
				callConversationID,
				simulateTools,
			)
			if simulateTools {
				result.toolCalls = nil
			}
			return result, err
		},
	)
}

// nonStreamResponses handles non-streaming Responses API requests.
func (api *APIServer) nonStreamResponses(
	ctx context.Context,
	w http.ResponseWriter,
	messages []payload.Message,
	cfg models.ModelConfig,
	sid, convID string,
	maxTokens int,
	toolPolicy responsesToolPolicy,
	goalOpen bool,
) {
	result, err := api.responsesConversation(
		ctx,
		messages,
		cfg,
		convID,
		toolPolicy.simulate,
		func() { api.forgetSession(sid) },
	)
	if err != nil {
		api.forgetSession(sid)
		if api.responsesRequestCanceled(ctx, sid) {
			return
		}
		api.sendUpstreamError(w, "chat", err)
		return
	}

	turn := responsesTurn{
		text:         api.routeGeneratedImages(result.text),
		thinking:     result.thinking,
		toolCalls:    result.toolCalls,
		finishReason: result.finishReason,
		convID:       result.conversationID,
	}
	turn.toolCalls, turn.finishReason = withoutBackendToolCalls(turn.toolCalls, turn.finishReason)

	// Parse simulated tool calls from response text
	if toolPolicy.simulate && !api.applyResponsesSimulation(ctx, w, &turn, messages, cfg, sid, toolPolicy) {
		return
	}

	turn.thinking = responsesReasoningForOutput(turn.thinking, toolPolicy.simulate)
	if blockedByContentPolicy(turn.text, turn.toolCalls) {
		api.forgetSession(sid)
		api.sendContentBlockedError(w, turn.text)
		return
	}
	// The Responses input is collapsed into one prompt message, so the
	// evidence comes from the policy rather than from the messages here.
	turn.text = withoutUnverifiedCompletionClaim(turn.text, toolPolicy.simulate, toolPolicy.ledger, turn.toolCalls)
	if responsesResultEmpty(turn.text, turn.toolCalls) {
		api.forgetSession(sid)
		api.answerResponsesEmpty(w, cfg)
		return
	}

	// Enforce max_output_tokens
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(turn.text, maxTokens); ok {
			turn.text = truncated
			turn.finishReason = "length"
		}
	}

	promptTok := countPromptTokens(messages, toolPolicy.tools, toolPolicy.promptChoice)
	completionTok := countTokens(turn.text) + outputProtocolTokens
	reasoningTok := countTokens(turn.thinking)

	responseID, createdAt := newResponsesIdentity()
	response := buildResponsesObject(responseID, createdAt, cfg.OpenAIID, turn.text, turn.thinking, turn.toolCalls, responsesToolTypes(toolPolicy.tools), goalOpen, turn.finishReason, promptTok, completionTok, reasoningTok)

	api.storeResponsesSession(sid, turn)
	api.sendJSON(w, http.StatusOK, response)
}

// responsesTurn carries the answer of one Responses turn, which a retry can
// replace wholesale.
type responsesTurn struct {
	text         string
	thinking     string
	toolCalls    []client.ToolCall
	finishReason string
	convID       string
}

// retryResponsesTurn runs a fresh turn on a new conversation and adopts its
// answer. dropToolCalls covers the empty-answer retry, where the calls of the
// turn being replaced must not survive it.
func (api *APIServer) retryResponsesTurn(ctx context.Context, turn *responsesTurn, messages []payload.Message, cfg models.ModelConfig, sid string, dropToolCalls bool) (string, error) {
	api.forgetSession(sid)
	retryResult, retryErr := api.responsesConversationOnce(ctx, messages, cfg, "", true)
	if retryErr != nil {
		return "", retryErr
	}
	turn.text = retryResult.text
	turn.thinking = retryResult.thinking
	turn.toolCalls = retryResult.toolCalls
	if dropToolCalls {
		turn.toolCalls = nil
	}
	turn.finishReason = retryResult.finishReason
	turn.convID = retryResult.conversationID
	return retryResult.text, nil
}

// applyResponsesSimulation parses the simulated tool calls, re-asking the
// backend when the model owed a call it did not make or answered with nothing.
// ok is false when the request has already been answered.
func (api *APIServer) applyResponsesSimulation(ctx context.Context, w http.ResponseWriter, turn *responsesTurn, messages []payload.Message, cfg models.ModelConfig, sid string, toolPolicy responsesToolPolicy) bool {
	simulated, parseErr := parseResponsesSimulationWithRetry(
		turn.text,
		toolPolicy,
		func() (string, error) {
			return api.retryResponsesTurn(ctx, turn, responsesSimulationRetryMessages(messages, toolPolicy), cfg, sid, false)
		},
		func() (string, error) {
			return api.retryResponsesTurn(ctx, turn, messages, cfg, sid, true)
		},
	)
	if parseErr != nil {
		api.forgetSession(sid)
		if api.responsesRequestCanceled(ctx, sid) {
			return false
		}
		writeResponsesParseError(w, cfg.OpenAIID, parseErr)
		return false
	}
	turn.text = simulated.content
	turn.toolCalls = simulated.toolCalls
	turn.finishReason = simulated.finishReason
	return true
}

// writeResponsesParseError reports a simulation that could not be parsed. A
// required call the model never made has its own code, because a client can
// act on that one.
func writeResponsesParseError(w http.ResponseWriter, model string, parseErr error) {
	if errors.Is(parseErr, errSimulatedToolCallRequired) {
		writeResponsesSimulationError(w, false, "", 0, model, parseErr)
		return
	}
	writeResponsesServerError(w, false, "", 0, model, "upstream_error", parseErr.Error())
}

// answerResponsesEmpty reports a turn that produced nothing.
//
// An exhausted conversation quota is the one empty-response cause the client
// can act on, so it is reported as 429 rather than as a generic empty answer.
func (api *APIServer) answerResponsesEmpty(w http.ResponseWriter, cfg models.ModelConfig) {
	if api.quotaExhausted() {
		api.sendThrottledError(w)
		return
	}
	writeResponsesUpstreamEmptyError(w, false, "", 0, cfg.OpenAIID)
}

// storeResponsesSession binds the session to the conversation this turn ran on,
// or drops the binding when the turn produced nothing worth continuing.
func (api *APIServer) storeResponsesSession(sid string, turn responsesTurn) {
	if sid == "" {
		return
	}
	if shouldResetResponsesSession(turn.text, turn.toolCalls, nil) {
		api.ctxCache.Delete(sessionKeyPrefix + sid)
		return
	}
	api.storeSessionMapping(sid, turn.convID)
}

// streamResponses handles streaming Responses API requests.
func (api *APIServer) streamResponses(
	ctx context.Context,
	w http.ResponseWriter,
	messages []payload.Message,
	cfg models.ModelConfig,
	sid, convID string,
	maxTokens int,
	toolPolicy responsesToolPolicy,
	goalOpen bool,
) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		api.sendError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	responseID, createdAt := newResponsesIdentity()
	openaiModel := cfg.OpenAIID

	// Helper to send a Responses SSE event
	sequenceNumber := 0
	sendEvent := func(eventType string, data map[string]any) {
		data["type"] = eventType
		data["sequence_number"] = sequenceNumber
		sequenceNumber++
		jsonData, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", jsonData)
		flusher.Flush()
	}
	sendFailed := func(code, message string) {
		event := buildResponsesFailedEvent(
			responseID,
			createdAt,
			openaiModel,
			code,
			message,
			sequenceNumber,
		)
		sequenceNumber++
		jsonData, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", jsonData)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	// Send response.created event
	sendEvent("response.created", map[string]any{
		"response": responsesStatusObject(responseID, openaiModel, "in_progress", createdAt),
	})

	// Send response.in_progress event
	sendEvent("response.in_progress", map[string]any{
		"response": responsesStatusObject(responseID, openaiModel, "in_progress", createdAt),
	})

	ch := responsesStreamWithEmptyRetry(
		ctx,
		convID,
		responsesEmptyRetrySchedule(toolPolicy.simulate),
		toolPolicy.simulate,
		func() {
			if sid != "" {
				api.ctxCache.Delete(sessionKeyPrefix + sid)
			}
		},
		func(
			callContext context.Context,
			callConversationID string,
		) <-chan client.StreamChunk {
			return api.m365Client.ChatConversationStreamGenContext(
				callContext,
				messages,
				cfg.Tone,
				cfg.Override,
				callConversationID,
				api.config.UserOID,
				api.config.TenantID,
				toolPolicy.simulate,
			)
		},
	)

	var fullTextBuilder strings.Builder
	var thinkingText strings.Builder
	truncated := false

	// When tool calling is enabled, buffer all text and parse at the end
	toolCallingEnabled := toolPolicy.simulate
	var contentExtractor toolcalling.ContentStreamExtractor

	// Track whether we've emitted the message output item
	messageItemEmitted := false
	reasoningItemEmitted := false
	messageOutputIndex := 0
	msgID := fmt.Sprintf("msg_%s", responseID)
	reasoningID := fmt.Sprintf("rs_%s", responseID)
	simulatedPublishedText := ""
	emitSimulatedDelta := func(delta string) {
		delta, published, limited := limitResponsesStreamDelta(
			simulatedPublishedText,
			delta,
			maxTokens,
		)
		simulatedPublishedText = published
		if limited {
			truncated = true
		}
		if delta == "" {
			return
		}
		if !messageItemEmitted {
			outputIdx := 0
			if reasoningItemEmitted {
				outputIdx = 1
			}
			messageOutputIndex = outputIdx
			sendEvent("response.output_item.added", map[string]any{
				"output_index": outputIdx,
				"item": map[string]any{
					"id":      msgID,
					"type":    "message",
					"status":  "in_progress",
					"role":    "assistant",
					"phase":   "commentary",
					"content": []any{},
				},
			})
			sendEvent("response.content_part.added", map[string]any{
				"item_id":       msgID,
				"output_index":  outputIdx,
				"content_index": 0,
				"part": map[string]any{
					"type":        "output_text",
					"text":        "",
					"annotations": []any{},
				},
			})
			messageItemEmitted = true
		}
		sendEvent("response.output_text.delta", map[string]any{
			"item_id":       msgID,
			"output_index":  messageOutputIndex,
			"content_index": 0,
			"delta":         delta,
		})
	}

	// Reasoning is streamed live; under simulated tool calling it passes through
	// thinkingFilter first so the transport envelope never leaks. reasoningEmitted
	// accumulates exactly what was published for the terminal done events.
	var thinkingFilter toolcalling.ThinkingStreamFilter
	var reasoningEmitted strings.Builder
	reasoningFlushed := false
	emitReasoning := func(delta string) {
		if delta == "" {
			return
		}
		reasoningEmitted.WriteString(delta)
		if !reasoningItemEmitted {
			sendEvent("response.output_item.added", map[string]any{
				"output_index": 0,
				"item": map[string]any{
					"id":      reasoningID,
					"type":    "reasoning",
					"status":  "in_progress",
					"summary": []map[string]any{{"type": "summary_text", "text": ""}},
				},
			})
			sendEvent("response.reasoning_summary_part.added", map[string]any{
				"item_id":       reasoningID,
				"output_index":  0,
				"summary_index": 0,
				"part":          map[string]any{"type": "summary_text", "text": ""},
			})
			reasoningItemEmitted = true
		}
		sendEvent("response.reasoning_summary_text.delta", map[string]any{
			"item_id":       reasoningID,
			"output_index":  0,
			"summary_index": 0,
			"delta":         delta,
		})
	}

	var finalConvID string
	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	for {
		chunk, more := nextStreamChunk(ctx, ch, keepalive, w, flusher, func() error { return writeSSEKeepalive(w, flusher) })
		if !more {
			break
		}
		if chunk.Error != nil {
			if sid != "" {
				api.ctxCache.Delete(sessionKeyPrefix + sid)
			}
			_, code, message := streamErrorFields("responses", chunk.Error)
			sendFailed(code, message)
			return
		}

		if chunk.IsFinal {
			finalConvID = chunk.ConversationID
			break
		}

		chunk.Text = api.routeGeneratedImages(chunk.Text)

		// Stream reasoning live. Under simulated tool calling it is filtered so
		// the transport envelope never leaks; otherwise it passes through raw.
		if chunk.Thinking != "" {
			thinkingText.WriteString(chunk.Thinking)
			if toolCallingEnabled {
				emitReasoning(thinkingFilter.Feed(chunk.Thinking))
			} else {
				emitReasoning(chunk.Thinking)
			}
		}

		// Handle text content
		if chunk.Text != "" {
			if toolCallingEnabled {
				// Flush remaining reasoning before content so the reasoning item
				// is fully emitted at output_index 0 ahead of the message item.
				if !reasoningFlushed {
					emitReasoning(thinkingFilter.Flush())
					reasoningFlushed = true
				}
				// Keep the raw transport for final tool-call parsing, while
				// publishing only decoded assistant content.
				fullTextBuilder.WriteString(chunk.Text)
				emitSimulatedDelta(contentExtractor.Feed(chunk.Text))
			} else {
				if !messageItemEmitted {
					// Emit message output item
					outputIdx := 0
					if reasoningItemEmitted {
						outputIdx = 1
					}
					sendEvent("response.output_item.added", map[string]any{
						"output_index": outputIdx,
						"item": map[string]any{
							"id":     msgID,
							"type":   "message",
							"status": "in_progress",
							"role":   "assistant",
							// No tool call is known yet; the terminal item below names
							// the phase again once the turn is parsed.
							"phase":   responsesMessagePhase(goalOpen, nil),
							"content": []any{},
						},
					})
					sendEvent("response.content_part.added", map[string]any{
						"item_id":       msgID,
						"output_index":  outputIdx,
						"content_index": 0,
						"part": map[string]any{
							"type":        "output_text",
							"text":        "",
							"annotations": []any{},
						},
					})
					messageItemEmitted = true
				}

				// Check max_tokens
				if maxTokens > 0 && countTokens(fullTextBuilder.String()+chunk.Text) > maxTokens {
					remaining := maxTokens - countTokens(fullTextBuilder.String())
					if remaining > 0 {
						delta, _ := truncateToTokens(chunk.Text, remaining)
						if delta != "" {
							fullTextBuilder.WriteString(delta)
							outputIdx := 0
							if reasoningItemEmitted {
								outputIdx = 1
							}
							sendEvent("response.output_text.delta", map[string]any{
								"item_id":       msgID,
								"output_index":  outputIdx,
								"content_index": 0,
								"delta":         delta,
							})
						}
					}
					truncated = true
					// Drain remaining chunks
					go func() {
						for range ch {
						}
					}()
					break
				}

				fullTextBuilder.WriteString(chunk.Text)
				outputIdx := 0
				if reasoningItemEmitted {
					outputIdx = 1
				}
				sendEvent("response.output_text.delta", map[string]any{
					"item_id":       msgID,
					"output_index":  outputIdx,
					"content_index": 0,
					"delta":         chunk.Text,
				})
			}
		}
	}
	fullText := fullTextBuilder.String()
	if api.responsesRequestCanceled(ctx, sid) {
		return
	}

	if !toolCallingEnabled && responsesResultEmpty(fullText, nil) {
		if sid != "" {
			api.ctxCache.Delete(sessionKeyPrefix + sid)
		}
		sendFailed(
			upstreamEmptyResponseCode,
			"The upstream completed without assistant content or a tool call.",
		)
		return
	}

	// Flush any remaining filtered reasoning for the thinking-only case where no
	// content chunk triggered the in-loop flush.
	if toolCallingEnabled && !reasoningFlushed {
		emitReasoning(thinkingFilter.Flush())
	}

	// Finalize reasoning item if emitted. reasoningEmitted holds exactly what was
	// published (raw for non-simulated, filtered under simulated tool calling).
	if reasoningItemEmitted {
		reasoningFinal := reasoningEmitted.String()
		sendEvent("response.reasoning_summary_text.done", map[string]any{
			"item_id":       reasoningID,
			"output_index":  0,
			"summary_index": 0,
			"text":          reasoningFinal,
		})
		sendEvent("response.reasoning_summary_part.done", map[string]any{
			"item_id":       reasoningID,
			"output_index":  0,
			"summary_index": 0,
			"part": map[string]any{
				"type": "summary_text",
				"text": reasoningFinal,
			},
		})
		sendEvent("response.output_item.done", map[string]any{
			"output_index": 0,
			"item": map[string]any{
				"id":     reasoningID,
				"type":   "reasoning",
				"status": "completed",
				"summary": []map[string]any{
					{
						"type": "summary_text",
						"text": reasoningFinal,
					},
				},
			},
		})
	}

	// Handle tool calling: parse buffered text for simulated tool calls
	var toolCalls []client.ToolCall
	finishReason := "stop"

	if toolCallingEnabled {
		initialParseText := contentExtractor.ParseText()
		simulated, parseErr := parseResponsesSimulationWithRetry(
			initialParseText,
			toolPolicy,
			func() (string, error) {
				if sid != "" {
					api.ctxCache.Delete(sessionKeyPrefix + sid)
				}
				retryResult, retryErr := api.responsesConversationOnce(
					ctx,
					responsesSimulationRetryMessages(messages, toolPolicy),
					cfg,
					"",
					true,
				)
				if retryErr != nil {
					return "", retryErr
				}
				fullText = retryResult.text
				finalConvID = retryResult.conversationID
				if !messageItemEmitted {
					contentExtractor = toolcalling.ContentStreamExtractor{}
					contentExtractor.Feed(retryResult.text)
				}
				return retryResult.text, nil
			},
			func() (string, error) {
				if sid != "" {
					api.ctxCache.Delete(sessionKeyPrefix + sid)
				}
				retryResult, retryErr := api.responsesConversationOnce(
					ctx,
					messages,
					cfg,
					"",
					true,
				)
				if retryErr != nil {
					return "", retryErr
				}
				fullText = retryResult.text
				finalConvID = retryResult.conversationID
				if !messageItemEmitted {
					contentExtractor = toolcalling.ContentStreamExtractor{}
					contentExtractor.Feed(retryResult.text)
				}
				return retryResult.text, nil
			},
		)
		if parseErr != nil {
			if sid != "" {
				api.ctxCache.Delete(sessionKeyPrefix + sid)
			}
			if api.responsesRequestCanceled(ctx, sid) {
				return
			}
			if errors.Is(parseErr, errSimulatedToolCallRequired) {
				sendFailed(
					simulatedToolCallRequiredCode,
					parseErr.Error(),
				)
			} else {
				sendFailed("upstream_error", parseErr.Error())
			}
			return
		}
		committedContent := contentExtractor.Commit(
			toolPolicy.allowedToolNames,
		)
		emitSimulatedDelta(committedContent)
		if len(simulated.toolCalls) == 0 &&
			(committedContent != "" || simulated.content == "") {
			simulated.content = simulatedPublishedText
		} else if messageItemEmitted {
			simulated.content = simulatedPublishedText
		}
		fullText = simulated.content
		toolCalls = simulated.toolCalls
		finishReason = simulated.finishReason
		// This is the one streaming path that publishes assistant content as it
		// decodes it, so the claim cannot be replaced here.
		warnOnUnverifiedCompletionClaim(fullText, toolPolicy.simulate, toolPolicy.ledger, len(toolCalls))
		if truncated {
			finishReason = "length"
		}
		if responsesResultEmpty(fullText, toolCalls) {
			if sid != "" {
				api.ctxCache.Delete(sessionKeyPrefix + sid)
			}
			sendFailed(
				upstreamEmptyResponseCode,
				"The upstream completed without assistant content or a tool call.",
			)
			return
		}

		// Now emit the buffered text and tool calls as Responses events
		outputIdx := 0
		if reasoningItemEmitted {
			outputIdx = 1
		}
		if messageItemEmitted {
			outputIdx = messageOutputIndex + 1
		}

		if messageItemEmitted {
			phase := responsesMessagePhase(goalOpen, toolCalls)
			sendEvent("response.output_text.done", map[string]any{
				"item_id":       msgID,
				"output_index":  messageOutputIndex,
				"content_index": 0,
				"text":          fullText,
			})
			sendEvent("response.content_part.done", map[string]any{
				"item_id":       msgID,
				"output_index":  messageOutputIndex,
				"content_index": 0,
				"part": map[string]any{
					"type":        "output_text",
					"text":        fullText,
					"annotations": []any{},
				},
			})
			sendEvent("response.output_item.done", map[string]any{
				"output_index": messageOutputIndex,
				"item": map[string]any{
					"id":     msgID,
					"type":   "message",
					"status": "completed",
					"role":   "assistant",
					"phase":  phase,
					"content": []map[string]any{
						{
							"type":        "output_text",
							"text":        fullText,
							"annotations": []any{},
						},
					},
				},
			})
		} else if fullText != "" || len(toolCalls) == 0 {
			// Emit buffered text when no incremental content was available.
			// Enforce max_output_tokens
			if maxTokens > 0 {
				if truncated, ok := truncateToTokens(fullText, maxTokens); ok {
					fullText = truncated
					finishReason = "length"
				}
			}
			phase := responsesMessagePhase(goalOpen, toolCalls)

			sendEvent("response.output_item.added", map[string]any{
				"output_index": outputIdx,
				"item": map[string]any{
					"id":      msgID,
					"type":    "message",
					"status":  "in_progress",
					"role":    "assistant",
					"phase":   phase,
					"content": []any{},
				},
			})
			sendEvent("response.content_part.added", map[string]any{
				"item_id":       msgID,
				"output_index":  outputIdx,
				"content_index": 0,
				"part": map[string]any{
					"type":        "output_text",
					"text":        "",
					"annotations": []any{},
				},
			})
			sendEvent("response.output_text.delta", map[string]any{
				"item_id":       msgID,
				"output_index":  outputIdx,
				"content_index": 0,
				"delta":         fullText,
			})
			sendEvent("response.output_text.done", map[string]any{
				"item_id":       msgID,
				"output_index":  outputIdx,
				"content_index": 0,
				"text":          fullText,
			})
			sendEvent("response.content_part.done", map[string]any{
				"item_id":       msgID,
				"output_index":  outputIdx,
				"content_index": 0,
				"part": map[string]any{
					"type":        "output_text",
					"text":        fullText,
					"annotations": []any{},
				},
			})
			sendEvent("response.output_item.done", map[string]any{
				"output_index": outputIdx,
				"item": map[string]any{
					"id":     msgID,
					"type":   "message",
					"status": "completed",
					"role":   "assistant",
					"phase":  phase,
					"content": []map[string]any{
						{
							"type":        "output_text",
							"text":        fullText,
							"annotations": []any{},
						},
					},
				},
			})
			outputIdx++
		}

		// Emit tool call items after the user-facing commentary.
		toolTypes := responsesToolTypes(toolPolicy.tools)
		for _, tc := range toolCalls {
			callID := tc.ID
			if callID == "" {
				callID = "call_" + uuid.NewString()
			}
			sendEvent("response.output_item.added", map[string]any{
				"output_index": outputIdx,
				"item":         buildResponsesToolCallItem(callID, tc, toolTypes, "in_progress"),
			})
			for _, event := range responsesToolInputEvents(callID, tc, toolTypes, outputIdx) {
				sendEvent(event.name, event.data)
			}
			sendEvent("response.output_item.done", map[string]any{
				"output_index": outputIdx,
				"item":         buildResponsesToolCallItem(callID, tc, toolTypes, "completed"),
			})
			outputIdx++
		}
	} else {
		// Non-tool-calling mode: finalize message item if emitted
		if messageItemEmitted {
			outputIdx := 0
			if reasoningItemEmitted {
				outputIdx = 1
			}
			if truncated {
				finishReason = "length"
			}
			sendEvent("response.output_text.done", map[string]any{
				"item_id":       msgID,
				"output_index":  outputIdx,
				"content_index": 0,
				"text":          fullText,
			})
			sendEvent("response.content_part.done", map[string]any{
				"item_id":       msgID,
				"output_index":  outputIdx,
				"content_index": 0,
				"part": map[string]any{
					"type":        "output_text",
					"text":        fullText,
					"annotations": []any{},
				},
			})
			sendEvent("response.output_item.done", map[string]any{
				"output_index": outputIdx,
				"item": map[string]any{
					"id":     msgID,
					"type":   "message",
					"status": "completed",
					"role":   "assistant",
					"phase":  responsesMessagePhase(goalOpen, toolCalls),
					"content": []map[string]any{
						{
							"type":        "output_text",
							"text":        fullText,
							"annotations": []any{},
						},
					},
				},
			})
		}
	}

	// Build final response object for response.completed
	status := "completed"
	if finishReason == "length" {
		status = "incomplete"
	}

	promptTok := countPromptTokens(messages, toolPolicy.tools, toolPolicy.promptChoice)
	completionTok := countTokens(fullText) + outputProtocolTokens
	reasoningText := thinkingText.String()
	reasoningTok := countTokens(reasoningText)

	finalResponse := buildResponsesObject(responseID, createdAt, openaiModel, fullText, reasoningText, toolCalls, responsesToolTypes(toolPolicy.tools), goalOpen, finishReason, promptTok, completionTok, reasoningTok)
	finalResponse["status"] = status

	if sid != "" {
		if shouldResetResponsesSession(fullText, toolCalls, nil) {
			api.ctxCache.Delete(sessionKeyPrefix + sid)
		} else {
			api.storeSessionMapping(sid, finalConvID)
		}
	}

	sendEvent("response.completed", map[string]any{
		"response": finalResponse,
	})

	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ===================================================================
// OpenAI Responses Compact API (/v1/responses/compact)
// ===================================================================

// defaultCompactionPrompt is the system instruction sent to M365 Copilot when
// compacting a conversation. It asks the model to produce a concise summary
// that preserves key context for continuation.
const defaultCompactionPrompt = "I need a concise summary of the following conversation between a user and an assistant. Please cover the main topics discussed, any decisions made, code or files mentioned, and what was being worked on. Keep it brief but preserve all important context. Explicitly preserve tool state: which tools were searched for, loaded, or called; their exact namespace and names; the results of those calls; and the user's current objective and next step. Do not describe transport JSON or protocol details; summarize only the actual work."

func responsesCompactionConversationID(string) string {
	return ""
}

// handleResponsesCompact handles POST /v1/responses/compact requests from Codex.
// It sends the conversation history to M365 Copilot with a compaction prompt,
// then returns the summary wrapped in a compaction output item.
func (api *APIServer) handleResponsesCompact(w http.ResponseWriter, r *http.Request) {
	var req responsesRequest
	if _, ok := api.readJSONRequest(w, r, &req, "handleResponsesCompact"); !ok {
		return
	}

	// Parse model (may contain session ID suffix)
	modelKey, modelSessionID := parseModelSessionID(req.Model)
	cfg, ok := api.responsesModelConfig(w, modelKey, req.Reasoning)
	if !ok {
		return
	}

	messages := []payload.Message{
		{Role: "user", Content: compactionPromptText(req)},
	}

	sid, storedConvID := api.sessionAndConversation(r, sessionSources{
		ModelSuffix:        modelSessionID,
		PreviousResponseID: req.PreviousResponseID,
		BodySessionID:      req.SessionID,
		BodyUser:           req.User,
	}, messages)
	convID := responsesCompactionConversationID(storedConvID)

	// Upload any images found in multimodal content
	api.uploadImagesAndAnnotate(&messages, convID)

	hasTools := len(toolcalling.RouteableTools(req.Tools)) > 0

	logging.Infof("handleResponsesCompact: model=%s sid=%s convID=%s stream=%t tools=%d", modelKey, sid, convID, req.Stream, len(req.Tools))

	if req.Stream {
		api.streamResponsesCompact(r.Context(), w, messages, cfg, sid, convID, req.MaxOutputTokens, hasTools, req.Tools)
		return
	}
	api.nonStreamResponsesCompact(w, messages, cfg, sid, convID, req.MaxOutputTokens, hasTools, req.Tools)
}

// compactionPromptText flattens the conversation history into a single user
// message carrying the compaction instructions.
//
// M365 has no system role and answers the last user message, so everything has
// to be merged into one message; otherwise the model continues the conversation
// instead of summarizing it.
func compactionPromptText(req responsesRequest) string {
	compactionInstr := defaultCompactionPrompt
	if instructions := strings.TrimSpace(req.Instructions); instructions != "" {
		compactionInstr = instructions
	}

	var conversationText strings.Builder
	conversationText.WriteString(compactionInstr)
	conversationText.WriteString("\n\n")
	for _, m := range responsesInputToMessages(req.Input) {
		fmt.Fprintf(&conversationText, "%s: %s\n", m.Role, m.Content)
	}
	conversationText.WriteString("\nPlease provide the summary now.")
	return conversationText.String()
}

// buildCompactionResponseObject constructs the non-streaming compact response.
// The output contains exactly one compaction item with encrypted_content set
// to the M365 summary text.
func buildCompactionResponseObject(responseID string, createdAt int64, model, summaryText string, promptTok, completionTok int) map[string]any {
	compactionID := fmt.Sprintf("cmp_%s", responseID)
	output := []map[string]any{
		{
			"id":                compactionID,
			"type":              "compaction",
			"encrypted_content": summaryText,
		},
	}

	return map[string]any{
		"id":         responseID,
		"object":     "response",
		"created_at": createdAt,
		"status":     "completed",
		"model":      model,
		"output":     output,
		"usage": map[string]any{
			"input_tokens":  promptTok,
			"output_tokens": completionTok,
			"total_tokens":  promptTok + completionTok,
			"usage_source":  usageSource(),
		},
	}
}

// nonStreamResponsesCompact handles non-streaming compact requests.
func (api *APIServer) nonStreamResponsesCompact(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxTokens int, hasTools bool, tools []toolcalling.ToolDef) {
	respText, _, _, _, _, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
	if err != nil {
		logging.Errorf("nonStreamResponsesCompact: chat failed: %v", err)
		if sid != "" {
			api.ctxCache.Delete(sessionKeyPrefix + sid)
		}
		api.sendUpstreamError(w, "compaction", err)
		return
	}

	// In simulated mode, extract plain content
	if hasTools {
		sim := toolcalling.ParseSimulatedResponse(respText, toolNamesFromDefs(tools), toolcalling.ContractsFor(tools))
		if sim.HasPayload {
			respText = sim.Content
		} else {
			respText = toolcalling.WithholdTransportEnvelope(respText)
		}
	}

	// Enforce max_output_tokens
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(respText, maxTokens); ok {
			respText = truncated
		}
	}

	// The compaction request declares no tools, so only message framing counts.
	promptTok := countPromptTokens(messages, nil, "")
	completionTok := countTokens(respText) + outputProtocolTokens

	responseID, createdAt := newResponsesIdentity()
	response := buildCompactionResponseObject(responseID, createdAt, cfg.OpenAIID, respText, promptTok, completionTok)

	api.sendJSON(w, http.StatusOK, response)

	if sid != "" && strings.TrimSpace(respText) != "" {
		api.ctxCache.Delete(sessionKeyPrefix + sid)
	}
}

// failResponsesStream reports a failed turn on an open Responses stream, which
// cannot be turned into an HTTP error because the headers are already out.
func (api *APIServer) failResponsesStream(w http.ResponseWriter, flusher http.Flusher, sendEvent func(string, map[string]any), responseID, openaiModel string, createdAt int64, err error) {
	failed := responsesStatusObject(responseID, openaiModel, "failed", createdAt)
	failed["error"] = map[string]any{"message": err.Error()}
	sendEvent("response.failed", map[string]any{"response": failed})
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// compactionSummaryText extracts the summary from a turn that ran in simulated
// mode, where the answer travels inside the transport envelope.
func compactionSummaryText(fullText string, hasTools bool, tools []toolcalling.ToolDef) string {
	if !hasTools {
		return fullText
	}
	sim := toolcalling.ParseSimulatedResponse(fullText, toolNamesFromDefs(tools), toolcalling.ContractsFor(tools))
	if sim.HasPayload {
		return sim.Content
	}
	fullText = toolcalling.WithholdTransportEnvelope(fullText)
	if toolcalling.IsContentPolicyBlock(fullText) {
		// The stream is already open, so the refusal cannot be turned into an
		// HTTP error the way the non-streaming paths do.
		logging.Warn("upstream content refusal on a streaming turn: M365 declined the request instead of answering")
	}
	return fullText
}

// streamResponsesCompact handles streaming compact requests.
// It emits a standard Responses SSE stream but replaces the output item
// with a single compaction item containing the summary.
func (api *APIServer) streamResponsesCompact(ctx context.Context, w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxTokens int, hasTools bool, tools []toolcalling.ToolDef) {
	flusher, ok := api.beginSSE(w)
	if !ok {
		return
	}

	responseID, createdAt := newResponsesIdentity()
	openaiModel := cfg.OpenAIID
	compactionID := fmt.Sprintf("cmp_%s", responseID)

	sendEvent := func(eventType string, data map[string]any) {
		data["type"] = eventType
		jsonData, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", jsonData)
		flusher.Flush()
	}

	// Send response.created event
	sendEvent("response.created", map[string]any{
		"response": responsesStatusObject(responseID, openaiModel, "in_progress", createdAt),
	})

	// Send response.in_progress event
	sendEvent("response.in_progress", map[string]any{
		"response": responsesStatusObject(responseID, openaiModel, "in_progress", createdAt),
	})

	ch := api.m365Client.ChatConversationStreamGenContext(ctx, messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)

	var fullTextBuilder strings.Builder

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	for {
		chunk, more := nextStreamChunk(ctx, ch, keepalive, w, flusher, func() error { return writeSSEKeepalive(w, flusher) })
		if !more {
			break
		}
		if chunk.Error != nil {
			logging.Errorf("streamResponsesCompact: stream error: %v", chunk.Error)
			api.forgetSession(sid)
			api.failResponsesStream(w, flusher, sendEvent, responseID, openaiModel, createdAt, chunk.Error)
			return
		}
		if chunk.Text != "" {
			fullTextBuilder.WriteString(chunk.Text)
		}
	}

	fullText := compactionSummaryText(fullTextBuilder.String(), hasTools, tools)

	// Enforce max_output_tokens
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(fullText, maxTokens); ok {
			fullText = truncated
		}
	}

	// Emit the compaction output item
	sendEvent("response.output_item.added", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"id":   compactionID,
			"type": "compaction",
		},
	})

	sendEvent("response.output_item.done", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"id":                compactionID,
			"type":              "compaction",
			"encrypted_content": fullText,
		},
	})

	// Build final response object for response.completed
	promptTok := countPromptTokens(messages, nil, "")
	completionTok := countTokens(fullText) + outputProtocolTokens

	finalResponse := buildCompactionResponseObject(responseID, createdAt, openaiModel, fullText, promptTok, completionTok)

	sendEvent("response.completed", map[string]any{
		"response": finalResponse,
	})

	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	if sid != "" && strings.TrimSpace(fullText) != "" {
		api.ctxCache.Delete(sessionKeyPrefix + sid)
	}
}

// imageGenerationRequest represents an OpenAI /v1/images/generations request.
type imageGenerationRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
	Quality        string `json:"quality"`
	Style          string `json:"style"`
	SessionID      string `json:"session_id"`
	User           string `json:"user"`
}

// imageDataItem represents a single image in the OpenAI Images API response.
type imageDataItem struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// urlImagePattern matches markdown image links with HTTP(S) URLs.
var urlImagePattern = regexp.MustCompile(`!\[[^\]]*\]\((https://[^)]+)\)`)

// handleImageGenerations handles OpenAI /v1/images/generations requests.
// It wraps the prompt as a chat completions request to M365, extracts generated
// image URLs from the response, and returns them in OpenAI Images API format.
func (api *APIServer) handleImageGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	limitRequestBody(w, r, imageRequestBodyMax)
	var req imageGenerationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logging.Errorf("handleImageGenerations: invalid JSON: %v", err)
		api.sendRequestBodyError(w, err)
		return
	}
	if req.Prompt == "" {
		api.sendError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	logging.Infof("handleImageGenerations: model=%s n=%d size=%s responseFormat=%s", req.Model, req.N, req.Size, req.ResponseFormat)
	if req.N <= 0 {
		req.N = 1
	}

	// Build prompt with size/quality/style hints appended
	fullPrompt := buildImagePromptWithHints(req.Prompt, req.Size, req.Quality, req.Style)

	// Resolve model (default to gpt5.5-reasoning for image generation)
	modelKey := req.Model
	if modelKey == "" {
		modelKey = "gpt5.5-reasoning"
	}
	modelKey, _ = parseModelSessionID(modelKey)
	cfg, ok := api.resolveModel(w, modelKey)
	if !ok {
		return
	}

	messages := []payload.Message{{Role: "user", Content: fullPrompt}}

	// Image generation is a one-shot operation. Reusing a chat conversation can
	// cause M365 to disengage instead of routing the prompt to image generation.
	respText, _, _, _, _, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, "", api.config.UserOID, api.config.TenantID, false)
	if err != nil {
		api.sendUpstreamError(w, "image generation", err)
		return
	}

	// Extract image URLs from markdown in response text
	dataItems := api.buildOpenAIImageData(respText, req.N, req.Prompt, req.ResponseFormat)
	if len(dataItems) == 0 {
		api.sendError(w, http.StatusInternalServerError, "No images were generated. The model may not have produced an image.")
		return
	}

	api.sendJSON(w, http.StatusOK, map[string]any{
		"created": time.Now().Unix(),
		"data":    dataItems,
	})
}

// handleImageEdits handles OpenAI /v1/images/edits requests.
// It accepts multipart/form-data with an image file, prompt, and optional mask,
// uploads the image to M365, sends the edit prompt, and returns the result.
func (api *APIServer) handleImageEdits(w http.ResponseWriter, r *http.Request) {
	form, ok := api.parseImageEditForm(w, r)
	if !ok {
		return
	}

	images, ok := api.imageEditUploads(w, r)
	if !ok {
		return
	}
	mask, ok := api.imageEditMask(w, r)
	if !ok {
		return
	}
	if mask != nil {
		images = append(images, *mask)
	}

	sid := imageEditSessionID(r, form.modelSessionID)
	convID := api.ctxCache.Get(sessionKeyPrefix + sid)

	// Build the multimodal message: the prompt with its hints, and the images
	messages := []payload.Message{{
		Role: "user",
		Content: buildImagePromptWithHints(
			form.prompt,
			r.FormValue("size"),
			r.FormValue("quality"),
			r.FormValue("style"),
		),
		Images: images,
	}}

	// Upload images and attach annotations
	api.uploadImagesAndAnnotate(&messages, convID)

	respText, _, _, _, finalConvID, err := api.m365Client.ChatConversation(messages, form.cfg.Tone, form.cfg.Override, convID, api.config.UserOID, api.config.TenantID, false)
	if err != nil {
		api.sendUpstreamError(w, "image edit", err)
		return
	}

	api.storeSessionMapping(sid, finalConvID)

	// Extract image URLs from response
	dataItems := api.buildOpenAIImageData(respText, imageEditCount(r.FormValue("n")), form.prompt, r.FormValue("response_format"))
	if len(dataItems) == 0 {
		api.sendError(w, http.StatusInternalServerError, "No edited images were generated. The model may not have produced an image.")
		return
	}

	api.sendJSON(w, http.StatusOK, map[string]any{
		"created": time.Now().Unix(),
		"data":    dataItems,
	})
}

// imageEditForm is what an image edit request declares once its form is parsed.
type imageEditForm struct {
	prompt         string
	modelSessionID string
	cfg            models.ModelConfig
}

// parseImageEditForm answers the method gate, parses the multipart form and
// resolves the model. ok is false when the request has already been answered.
func (api *APIServer) parseImageEditForm(w http.ResponseWriter, r *http.Request) (imageEditForm, bool) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return imageEditForm{}, false
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return imageEditForm{}, false
	}

	// Parse multipart form
	limitRequestBody(w, r, imageEditBodyMax)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		logging.Errorf("handleImageEdits: failed to parse multipart form: %v", err)
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to parse multipart form: %v", err))
		return imageEditForm{}, false
	}

	prompt := r.FormValue("prompt")
	if prompt == "" {
		api.sendError(w, http.StatusBadRequest, "prompt is required")
		return imageEditForm{}, false
	}

	modelKey := r.FormValue("model")
	if modelKey == "" {
		modelKey = "gpt5.5-reasoning"
	}
	modelKey, modelSessionID := parseModelSessionID(modelKey)
	cfg, ok := api.resolveModel(w, modelKey)
	if !ok {
		return imageEditForm{}, false
	}
	logging.Infof("handleImageEdits: model=%s prompt_len=%d images=%d responseFormat=%s", modelKey, len(prompt), len(r.MultipartForm.File["image"]), r.FormValue("response_format"))
	return imageEditForm{prompt: prompt, modelSessionID: modelSessionID, cfg: cfg}, true
}

// imageEditSessionID resolves the session an edit belongs to. An edit carries
// no message to hash, so an unnamed one gets a fresh session rather than
// joining another request's conversation.
func imageEditSessionID(r *http.Request, modelSessionID string) string {
	sid := resolveSessionID(r, sessionSources{
		ModelSuffix:   modelSessionID,
		BodySessionID: r.FormValue("session_id"),
		BodyUser:      r.FormValue("user"),
	})
	if sid == "" {
		return "img-edit-" + uuid.New().String()[:8]
	}
	return sid
}

// imageEditCount reads how many images the caller asked for, defaulting to one.
func imageEditCount(raw string) int {
	if raw == "" {
		return 1
	}
	if v, err := fmtAtoi(raw); err == nil && v > 0 {
		return v
	}
	return 1
}

// imageEditUploads reads the uploaded images. OpenAI supports up to 16 of them
// for GPT image models, and multipart form-data may send "image" as several
// form files. ok is false when the request has already been answered.
func (api *APIServer) imageEditUploads(w http.ResponseWriter, r *http.Request) ([]payload.ImageData, bool) {
	imageFiles := r.MultipartForm.File["image"]
	if len(imageFiles) == 0 {
		api.sendError(w, http.StatusBadRequest, "image file is required")
		return nil, false
	}

	var images []payload.ImageData
	for i, fh := range imageFiles {
		if i >= 16 {
			break
		}
		image, err := readUploadedImage(fh, fmt.Sprintf("edit-%d", i))
		if err != nil {
			api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to read image %d: %v", i, err))
			return nil, false
		}
		images = append(images, image)
	}
	return images, true
}

// readUploadedImage reads one uploaded file, naming it after baseName and
// falling back to PNG when the client declared no type.
func readUploadedImage(fh *multipart.FileHeader, baseName string) (payload.ImageData, error) {
	f, err := fh.Open()
	if err != nil {
		return payload.ImageData{}, err
	}
	imgBytes, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		return payload.ImageData{}, err
	}
	mediaType := fh.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "image/png"
	}
	return payload.ImageData{
		Base64:    base64.StdEncoding.EncodeToString(imgBytes),
		MediaType: mediaType,
		FileName:  baseName + "." + extFromMediaType(mediaType),
	}, nil
}

// imageEditMask reads the optional mask. It reports a nil mask when the request
// carries none, and ok false when the request has already been answered.
func (api *APIServer) imageEditMask(w http.ResponseWriter, r *http.Request) (*payload.ImageData, bool) {
	maskFile, maskHeader, err := r.FormFile("mask")
	if err != nil {
		return nil, true
	}
	maskBytes, err := io.ReadAll(maskFile)
	_ = maskFile.Close()
	if err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to read mask: %v", err))
		return nil, false
	}
	mediaType := maskHeader.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "image/png"
	}
	return &payload.ImageData{
		Base64:    base64.StdEncoding.EncodeToString(maskBytes),
		MediaType: mediaType,
		FileName:  "mask." + extFromMediaType(mediaType),
	}, true
}

// buildImagePromptWithHints appends size, quality, and style hints to the prompt
// as natural language, since M365 does not accept these as direct parameters.
func buildImagePromptWithHints(prompt, size, quality, style string) string {
	var hints []string
	if size != "" && size != "1024x1024" {
		hints = append(hints, fmt.Sprintf("size: %s", size))
	}
	if quality != "" && quality != "standard" {
		hints = append(hints, fmt.Sprintf("quality: %s", quality))
	}
	if style != "" && style != "natural" {
		hints = append(hints, fmt.Sprintf("style: %s", style))
	}
	if len(hints) == 0 {
		return prompt
	}
	return fmt.Sprintf("%s\n\nImage specifications: %s", prompt, strings.Join(hints, ", "))
}

// buildOpenAIImageData extracts image URLs from markdown in the response text
// and converts them to OpenAI Images API data items. When responseFormat is
// "b64_json", it downloads each URL and base64-encodes the content. When
// responseFormat is "url", it also downloads the image and returns a
// data:image/png;base64,... data URL (falling back to the raw URL on error)
// since the raw designerapp URL is auth-gated and inaccessible to clients.
func (api *APIServer) buildOpenAIImageData(respText string, n int, revisedPrompt, responseFormat string) []imageDataItem {
	urls := urlImagePattern.FindAllStringSubmatch(respText, -1)
	if len(urls) == 0 {
		return nil
	}

	// Deduplicate URLs
	seen := map[string]bool{}
	var uniqueURLs []string
	for _, match := range urls {
		u := match[1]
		if !seen[u] {
			seen[u] = true
			uniqueURLs = append(uniqueURLs, u)
		}
	}

	if n > 0 && n < len(uniqueURLs) {
		uniqueURLs = uniqueURLs[:n]
	}

	var items []imageDataItem
	for _, u := range uniqueURLs {
		b64, err := api.downloadAndBase64(u)
		if err != nil {
			// A disallowed host is dropped outright. Returning the raw URL
			// would hand a model-controlled address back to the client, which
			// would then fetch it.
			if errors.Is(err, errImageHostNotAllowed) {
				logging.Errorf("Dropping generated image URL: %v", err)
				continue
			}
			// The host is allowed but the transfer failed, so the raw URL is
			// still a safe fallback.
			logging.Errorf("Failed to download image: %v", err)
			items = append(items, imageDataItem{
				URL:           u,
				RevisedPrompt: revisedPrompt,
			})
			continue
		}
		if responseFormat == "b64_json" {
			items = append(items, imageDataItem{
				B64JSON:       b64,
				RevisedPrompt: revisedPrompt,
			})
			continue
		}
		items = append(items, imageDataItem{
			URL:           "data:image/png;base64," + b64,
			RevisedPrompt: revisedPrompt,
		})
	}

	return items
}

// errImageHostNotAllowed reports a generated-image URL the proxy refuses to
// contact.
var errImageHostNotAllowed = errors.New("image URL host is not allowed")

// hostAllowed reports whether host matches an allowlist entry. An entry
// starting with a dot matches that domain and any subdomain of it; any other
// entry must match exactly.
func hostAllowed(host string, allowlist []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, entry := range allowlist {
		if domain, isSuffix := strings.CutPrefix(entry, "."); isSuffix {
			if host == domain || strings.HasSuffix(host, entry) {
				return true
			}
			continue
		}
		if host == entry {
			return true
		}
	}
	return false
}

// ipDisallowed reports whether an address must never be contacted. Loopback,
// private, link-local, multicast, unspecified and carrier-grade NAT ranges all
// sit inside the deployment's own network, and 169.254.169.254 is the cloud
// metadata endpoint.
func ipDisallowed(ip net.IP) bool {
	return ipInternal(ip) || ipCarrierGradeNAT(ip)
}

// ipInternal reports the ranges net.IP names itself.
func ipInternal(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

// ipCarrierGradeNAT reports 100.64.0.0/10, which net.IP.IsPrivate misses.
func ipCarrierGradeNAT(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127
}

// validateImageDownloadURL decides whether a generated-image URL may be
// fetched. The download sends the designerapp access token, so an attacker who
// can influence the model's output must not be able to redirect that token to
// their own host, nor use the proxy to reach internal addresses. The URL comes
// from model-generated markdown, which is untrusted input.
func (api *APIServer) validateImageDownloadURL(rawURL string) error {
	parsed, err := neturl.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid image URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q is not https", errImageHostNotAllowed, parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("%w: URL has no host", errImageHostNotAllowed)
	}
	if !hostAllowed(host, api.config.ImageHostAllowlist) {
		return fmt.Errorf("%w: %q", errImageHostNotAllowed, host)
	}

	// Resolve as defence in depth: an allowlisted name that resolves inward
	// still must not be contacted.
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("%w: %q does not resolve", errImageHostNotAllowed, host)
	}
	if slices.ContainsFunc(ips, ipDisallowed) {
		return fmt.Errorf("%w: %q resolves to a non-public address", errImageHostNotAllowed, host)
	}
	return nil
}

// downloadAndBase64 downloads an image from a designerapp URL and returns its
// base64-encoded content.
func (api *APIServer) downloadAndBase64(imageURL string) (string, error) {
	body, _, err := api.downloadImage(imageURL)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(body), nil
}

// downloadImage downloads an image from a designerapp URL and returns its bytes
// with the content type the server reported. designerapp URLs require a JWE
// access token (acquired via SSO cookies with the M365 web app client_id) and
// the fileToken query parameter sent as a header.
func (api *APIServer) downloadImage(imageURL string) ([]byte, string, error) {
	req, err := api.designerImageRequest(imageURL)
	if err != nil {
		return nil, "", err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	// validateImageDownloadURL ran inside designerImageRequest: it requires
	// https, an allowlisted host, a name that resolves, and no resolved address
	// outside public space. The taint analysis cannot follow that validator.
	// #nosec G704
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	return readDownloadedImage(resp)
}

// designerImageRequest builds the download, moving the fileToken query
// parameter into the header the backend expects it in.
func (api *APIServer) designerImageRequest(imageURL string) (*http.Request, error) {
	if err := api.validateImageDownloadURL(imageURL); err != nil {
		logging.Errorf("downloadImage: refusing download: %v", err)
		return nil, err
	}
	logging.Infof("downloadImage: downloading image from %s", imageURL[:min(100, len(imageURL))])
	parsedURL, err := neturl.Parse(imageURL)
	if err != nil {
		logging.Errorf("downloadImage: invalid URL: %v", err)
		return nil, fmt.Errorf("invalid image URL: %w", err)
	}

	// Extract fileToken from query params and remove it from the URL
	query := parsedURL.Query()
	fileToken := query.Get("fileToken")
	if fileToken == "" {
		logging.Errorf("downloadImage: no fileToken in URL")
		return nil, fmt.Errorf("no fileToken in image URL")
	}
	query.Del("fileToken")
	parsedURL.RawQuery = query.Encode()

	// Acquire designerapp access token via SSO cookies
	token, err := api.tokenManager.GetDesignerToken()
	if err != nil {
		return nil, fmt.Errorf("failed to acquire designer token: %w", err)
	}

	// The address is the one validateImageDownloadURL already cleared above,
	// minus its fileToken parameter.
	// #nosec G704
	req, err := http.NewRequest("GET", parsedURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create image request: %w", err)
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("filetoken", fileToken)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", "https://m365.cloud.microsoft")
	req.Header.Set("Referer", "https://m365.cloud.microsoft/")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")
	return req, nil
}

// readDownloadedImage reads the image under a byte cap and reports the type the
// server declared.
func readDownloadedImage(resp *http.Response) ([]byte, string, error) {
	if resp.StatusCode != http.StatusOK {
		return nil, "", downloadImageFailure(resp)
	}

	// One extra byte distinguishes "exactly at the limit" from "truncated".
	body, err := io.ReadAll(io.LimitReader(resp.Body, remoteImageMaxBytes+1))
	if err != nil {
		logging.Errorf("downloadImage: failed to read body: %v", err)
		return nil, "", fmt.Errorf("failed to read response body: %w", err)
	}
	if len(body) > remoteImageMaxBytes {
		return nil, "", fmt.Errorf("generated image exceeds %d bytes", remoteImageMaxBytes)
	}

	// A designerapp response states its own type. An answer without one is
	// treated as PNG, which is what this backend has been observed to return.
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		contentType = "image/png"
	}

	logging.Infof("downloadImage: success, size=%d bytes type=%s", len(body), contentType)
	return body, contentType, nil
}

// downloadImageFailure reports why the backend refused, from the headers it
// answers with.
//
// Only an excerpt of the body is logged, so a truncated read is enough and the
// whole error page never has to be held in memory.
func downloadImageFailure(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyExcerptMax))
	errCode := resp.Header.Get("X-Errorcode")
	failReason := resp.Header.Get("X-Failurereason")
	logging.Errorf("Image download failed: status=%d, x-errorcode=%s, x-failurereason=%s, body=%s",
		resp.StatusCode, errCode, failReason, string(body)[:min(200, len(body))])
	return fmt.Errorf("download returned status %d: x-errorcode=%s, x-failurereason=%s", resp.StatusCode, errCode, failReason)
}

// fmtAtoi parses an int from a string without importing strconv.
func fmtAtoi(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// extFromMediaType returns the file extension for a given MIME type.
func extFromMediaType(mediaType string) string {
	switch strings.ToLower(mediaType) {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return "png"
	}
}
