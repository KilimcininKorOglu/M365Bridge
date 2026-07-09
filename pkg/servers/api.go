// Package servers provides HTTP API server for M365 Copilot.
// This file implements OpenAI-compatible and Anthropic-compatible API endpoints.
package servers

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/client"
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
)

// ContextCache provides session-based conversation persistence across requests.
type ContextCache struct {
	cacheDir string
	mu       sync.RWMutex
	mem      map[string]string
	order    []string
}

// NewContextCache creates a new context cache instance.
func NewContextCache(cacheDir string) *ContextCache {
	os.MkdirAll(cacheDir, 0700)
	return &ContextCache{
		cacheDir: cacheDir,
		mem:      make(map[string]string),
	}
}

// path returns the file path for a cache key.
func (cc *ContextCache) path(key string) string {
	hash := md5.Sum([]byte(key))
	safe := hex.EncodeToString(hash[:])
	return filepath.Join(cc.cacheDir, safe+".json")
}

// Get retrieves a conversation ID by session key.
func (cc *ContextCache) Get(key string) string {
	cc.mu.RLock()
	if val, ok := cc.mem[key]; ok {
		cc.mu.RUnlock()
		return val
	}
	cc.mu.RUnlock()

	data, err := os.ReadFile(cc.path(key))
	if err != nil {
		return ""
	}
	var convID string
	if err := json.Unmarshal(data, &convID); err != nil {
		return ""
	}

	cc.mu.Lock()
	cc.mem[key] = convID
	cc.order = append(cc.order, key)
	cc.evict()
	cc.mu.Unlock()

	return convID
}

// Set stores a conversation ID by session key.
func (cc *ContextCache) Set(key, convID string) {
	cc.mu.Lock()
	cc.mem[key] = convID
	if idx := indexOf(cc.order, key); idx >= 0 {
		cc.order = append(cc.order[:idx], cc.order[idx+1:]...)
	}
	cc.order = append(cc.order, key)
	cc.evict()
	cc.mu.Unlock()

	data, _ := json.Marshal(convID)
	os.WriteFile(cc.path(key), data, 0600)
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
	ctxCache     *ContextCache
	server       *http.Server
	stopCh       chan struct{}
	mu           sync.RWMutex
}

// NewAPIServer creates a new API server instance.
func NewAPIServer(config *models.Config, tokenManager *auth.TokenManager) *APIServer {
	return &APIServer{
		config:       config,
		tokenManager: tokenManager,
		ctxCache:     NewContextCache(contextCacheDir),
	}
}

// tokenRefreshInterval is the interval for periodic access token refresh.
const tokenRefreshInterval = 30 * time.Minute

// Start starts the HTTP server on the specified port.
func (api *APIServer) Start(port int) error {
	api.mu.Lock()
	// Initialize client
	api.m365Client = client.NewM365Client(api.tokenManager)
	api.stopCh = make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", api.withAuth(api.handleChatCompletions))
	mux.HandleFunc("/v1/completions", api.withAuth(api.handleCompletions))
	mux.HandleFunc("/v1/responses", api.withAuth(api.handleResponses))
	mux.HandleFunc("/v1/responses/compact", api.withAuth(api.handleResponsesCompact))
	mux.HandleFunc("/v1/messages", api.withAuth(api.handleAnthropicMessages))
	mux.HandleFunc("/v1/complete", api.withAuth(api.handleAnthropicComplete))
	mux.HandleFunc("/v1/images/generations", api.withAuth(api.handleImageGenerations))
	mux.HandleFunc("/v1/images/edits", api.withAuth(api.handleImageEdits))
	mux.HandleFunc("/v1/models", api.handleModels)
	mux.HandleFunc("/health", api.handleHealth)

	api.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
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
			provided := r.Header.Get("Authorization")
			if provided == "" {
				logging.Warnf("Auth: missing Authorization header from %s", r.RemoteAddr)
				api.sendError(w, http.StatusUnauthorized, "Missing Authorization header")
				return
			}
			token := strings.TrimSpace(strings.TrimPrefix(provided, "Bearer "))
			if !api.isValidAPIKey(token) {
				logging.Warnf("Auth: invalid API key from %s", r.RemoteAddr)
				api.sendError(w, http.StatusUnauthorized, "Invalid API key")
				return
			}
		}
		next(w, r)
	}
}

// isValidAPIKey checks if the given token matches any configured API key.
func (api *APIServer) isValidAPIKey(token string) bool {
	for _, k := range api.config.APIKeys {
		if token == k {
			return true
		}
	}
	return false
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
	w.Write([]byte("OK"))
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

	modelList := []map[string]interface{}{}
	for _, cfg := range models.ModelRegistry {
		modelList = append(modelList, map[string]interface{}{
			"id":       cfg.OpenAIID,
			"object":   "model",
			"created":  1700000000,
			"owned_by": "microsoft",
		})
	}

	response := map[string]interface{}{
		"object": "list",
		"data":   modelList,
	}

	api.sendJSON(w, http.StatusOK, response)
}

// handleCORS handles CORS preflight requests.
func (api *APIServer) handleCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-Id")
	w.WriteHeader(http.StatusOK)
}

// getSessionID extracts session ID from headers or request body.
// Priority: X-Session-Id header > session_id body field > user body field > hash(api_key + first_user_message)
func (api *APIServer) getSessionID(r *http.Request, reqBody map[string]interface{}) string {
	sid := r.Header.Get("X-Session-Id")
	if sid == "" {
		if v, ok := reqBody["session_id"].(string); ok {
			sid = v
		}
	}
	if sid == "" {
		if v, ok := reqBody["user"].(string); ok {
			sid = v
		}
	}
	if sid == "" {
		sid = api.hashSessionID(r, reqBody)
	}
	return sid
}

// hashSessionID derives a session ID from the API key and the first user message.
// When auth is enabled, the hash includes the API key so that different keys
// produce different sessions even with the same first message.
// When auth is disabled, only the first user message is hashed.
func (api *APIServer) hashSessionID(r *http.Request, reqBody map[string]interface{}) string {
	firstMsg := extractFirstUserMessage(reqBody)
	if firstMsg == "" {
		return ""
	}
	apiKey := api.extractAPIKey(r)
	h := md5.Sum([]byte(apiKey + "\x00" + firstMsg))
	return "h:" + hex.EncodeToString(h[:])
}

// extractFirstUserMessage scans the messages array and returns the first user message content.
func extractFirstUserMessage(reqBody map[string]interface{}) string {
	msgs, ok := reqBody["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		return ""
	}
	for _, m := range msgs {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "user" {
			continue
		}
		// Content can be a string or an array of content blocks
		switch c := msg["content"].(type) {
		case string:
			if c != "" {
				return c
			}
		case []interface{}:
			for _, block := range c {
				bm, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				if t, _ := bm["type"].(string); t == "text" {
					if txt, _ := bm["text"].(string); txt != "" {
						return txt
					}
				}
			}
		}
	}
	return ""
}

// hashSessionIDFromMessages derives a session ID from the API key and the first user message
// in a typed Message slice. Used by handleChatCompletions which decodes into a struct.
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
	h := md5.Sum([]byte(apiKey + "\x00" + firstMsg))
	return "h:" + hex.EncodeToString(h[:])
}

// handleChatCompletions handles OpenAI chat completion requests.
func (api *APIServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to read request body: %v", err))
		return
	}
	r.Body.Close()

	var req struct {
		Model          string                 `json:"model"`
		Messages       []payload.Message      `json:"messages"`
		Stream         bool                   `json:"stream"`
		MaxTokens      int                    `json:"max_tokens"`
		ResponseFormat map[string]interface{} `json:"response_format"`
		SessionID      string                 `json:"session_id"`
		User           string                 `json:"user"`
		Tools          []toolcalling.ToolDef  `json:"tools"`
		ToolChoice     interface{}            `json:"tool_choice"`
	}

	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		logging.Errorf("handleChatCompletions: invalid JSON: %v", err)
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Parse optional session ID encoded in model name: "gpt5.5:my-session"
	modelKey, modelSessionID := parseModelSessionID(req.Model)
	cfg := models.LookupModel(modelKey)
	if cfg.OpenAIID == "" {
		logging.Errorf("handleChatCompletions: unknown model: %s", modelKey)
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Unknown model: %s", modelKey))
		return
	}
	logging.Infof("handleChatCompletions: model=%s stream=%v tools=%d sid=%s", modelKey, req.Stream, len(req.Tools), modelSessionID)

	// Handle JSON mode
	if req.ResponseFormat != nil {
		if format, ok := req.ResponseFormat["type"].(string); ok && format == "json_object" {
			api.injectJSONMode(&req.Messages)
		}
	}

	// Inject simulated tool prompt if tool calling is enabled.
	// The entire request JSON is sent as the prompt; M365 returns a full
	// chat.completion response in a ```json block.
	if len(req.Tools) > 0 {
		injectSimulatedPrompt(&req.Messages, string(bodyBytes), toolChoiceString(req.ToolChoice))
	}

	// Resolve session ID and conversation ID
	// Priority: model-name session ID > request body session_id > request body user > X-Session-Id header > hash(api_key + first_user_message)
	sid := modelSessionID
	if sid == "" {
		sid = req.SessionID
	}
	if sid == "" {
		sid = req.User
	}
	if sid == "" {
		sid = r.Header.Get("X-Session-Id")
	}
	if sid == "" {
		sid = api.hashSessionIDFromMessages(r, req.Messages)
	}

	var convID string
	if sid != "" {
		convID = api.ctxCache.Get("session:" + sid)
	}

	// Upload any images found in multimodal content and attach annotations
	api.uploadImagesAndAnnotate(&req.Messages, convID)

	// Determine if client-defined tools are present (for optionsSets stripping)
	hasTools := len(req.Tools) > 0

	if req.Stream {
		api.streamChatCompletions(w, req.Messages, cfg, sid, convID, req.MaxTokens, hasTools, req.Tools)
	} else {
		api.nonStreamChatCompletions(w, req.Messages, cfg, sid, convID, req.MaxTokens, hasTools, req.Tools)
	}
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

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to read request body: %v", err))
		return
	}
	r.Body.Close()

	var req struct {
		Model      string                `json:"model"`
		Prompt     string                `json:"prompt"`
		Suffix     string                `json:"suffix"`
		Stream     bool                  `json:"stream"`
		MaxTokens  int                   `json:"max_tokens"`
		Tools      []toolcalling.ToolDef `json:"tools"`
		ToolChoice interface{}           `json:"tool_choice"`
	}

	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Parse optional session ID encoded in model name: "gpt5.5:my-session"
	modelKey, modelSessionID := parseModelSessionID(req.Model)
	cfg := models.LookupModel(modelKey)
	if cfg.OpenAIID == "" {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Unknown model: %s", modelKey))
		return
	}

	// Convert FIM to chat format
	messages := api.fimToChat(req.Prompt, req.Suffix)

	// Inject simulated tool prompt if tool calling is enabled
	if len(req.Tools) > 0 {
		injectSimulatedPrompt(&messages, string(bodyBytes), toolChoiceString(req.ToolChoice))
	}

	// Resolve session ID and conversation ID
	sid := modelSessionID
	if sid == "" {
		sid = api.getSessionID(r, nil)
	}
	var convID string
	if sid != "" {
		convID = api.ctxCache.Get("session:" + sid)
	}

	hasTools := len(req.Tools) > 0

	if req.Stream {
		api.streamCompletions(w, messages, cfg, req.MaxTokens, sid, convID, hasTools, req.Tools)
	} else {
		api.nonStreamCompletions(w, messages, cfg, req.MaxTokens, sid, convID, hasTools, req.Tools)
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

// handleAnthropicMessages handles Anthropic messages API requests.
func (api *APIServer) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to read request body: %v", err))
		return
	}
	r.Body.Close()

	var req struct {
		Model       string                 `json:"model"`
		Messages    []payload.Message      `json:"messages"`
		System      json.RawMessage        `json:"system"`
		MaxTokens   int                    `json:"max_tokens"`
		Stream      bool                   `json:"stream"`
		Temperature float64                `json:"temperature"`
		Tools       []toolcalling.ToolDef  `json:"tools"`
		ToolChoice  map[string]interface{} `json:"tool_choice"`
	}

	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		logging.Errorf("handleAnthropicMessages: invalid JSON: %v", err)
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Parse optional session ID encoded in model name: "gpt5.5:my-session"
	modelKey, modelSessionID := parseModelSessionID(req.Model)
	// Map Anthropic model to internal model
	cfg := models.LookupModel(modelKey)
	logging.Infof("handleAnthropicMessages: model=%s stream=%v tools=%d sid=%s", modelKey, req.Stream, len(req.Tools), modelSessionID)

	// Build chat messages with system prompt prepended. Claude Code can send
	// Anthropic system as either a string or an array of text content blocks.
	systemPrompt, err := normalizeAnthropicSystem(req.System)
	if err != nil {
		logging.Errorf("handleAnthropicMessages: invalid system field: %v", err)
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid system field: %v", err))
		return
	}

	chatMessages := []payload.Message{}
	if systemPrompt != "" {
		chatMessages = append(chatMessages, payload.Message{Role: "system", Content: systemPrompt})
	}
	chatMessages = append(chatMessages, req.Messages...)

	// Inject simulated tool prompt if tool calling is enabled.
	// The entire Anthropic request JSON is sent as the prompt; M365 returns
	// a full Anthropic Messages response in a ```json block.
	if len(req.Tools) > 0 {
		injectSimulatedPromptAnthropic(&chatMessages, string(bodyBytes), anthropicToolChoiceString(req.ToolChoice))
	}

	// Resolve session ID and conversation ID
	sid := modelSessionID
	if sid == "" {
		sid = api.getSessionID(r, nil)
	}
	var convID string
	if sid != "" {
		convID = api.ctxCache.Get("session:" + sid)
	}

	// Upload any images found in multimodal content and attach annotations
	api.uploadImagesAndAnnotate(&chatMessages, convID)

	// Determine if client-defined tools are present (for optionsSets stripping)
	hasTools := len(req.Tools) > 0

	if req.Stream {
		api.streamAnthropicMessages(w, chatMessages, cfg, req.Model, req.MaxTokens, sid, convID, hasTools, req.Tools)
	} else {
		api.nonStreamAnthropicMessages(w, chatMessages, cfg, req.Model, req.MaxTokens, sid, convID, hasTools, req.Tools)
	}
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
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Parse optional session ID encoded in model name: "gpt5.5:my-session"
	modelKey, modelSessionID := parseModelSessionID(req.Model)
	cfg := models.LookupModel(modelKey)

	messages := api.fimToChat(req.Prompt, "")

	// Resolve session ID and conversation ID
	sid := modelSessionID
	if sid == "" {
		sid = api.getSessionID(r, nil)
	}
	var convID string
	if sid != "" {
		convID = api.ctxCache.Get("session:" + sid)
	}

	if req.Stream {
		api.streamAnthropicComplete(w, messages, cfg, req.Model, req.MaxTokensToSample, req.StopSequences, sid, convID)
	} else {
		api.nonStreamAnthropicComplete(w, messages, cfg, req.Model, req.MaxTokensToSample, req.StopSequences, sid, convID)
	}
}

// nonStreamAnthropicComplete handles non-streaming Anthropic complete (FIM) requests.
func (api *APIServer) nonStreamAnthropicComplete(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, model string, maxTokens int, stopSequences []string, sid, convID string) {
	respText, _, _, _, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, false)
	if err != nil {
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Completion failed: %v", err))
		return
	}

	stopReason := "end_turn"
	for _, s := range stopSequences {
		if strings.Contains(respText, s) {
			stopReason = "stop_sequence"
			break
		}
	}

	// Enforce max_tokens_to_sample on response text
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(respText, maxTokens); ok {
			respText = truncated
			stopReason = "max_tokens"
		}
	}

	response := map[string]interface{}{
		"completion":  respText,
		"stop_reason": stopReason,
		"model":       model,
		"stop":        nil,
		"log_id":      fmt.Sprintf("cmpl_%s", uuid.New().String()),
	}

	api.sendJSON(w, http.StatusOK, response)

	// Cache conversation ID for session continuity
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}
}

// streamAnthropicComplete streams Anthropic complete (FIM) responses.
// Anthropic Complete streaming uses SSE with event: completion and
// data containing {"type":"completion","completion":"<delta>","stop_reason":null}.
// The final event has stop_reason set and completion empty.
func (api *APIServer) streamAnthropicComplete(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, model string, maxTokens int, stopSequences []string, sid, convID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		api.sendError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	logID := fmt.Sprintf("cmpl_%s", uuid.New().String())

	// Send ping event (Anthropic streaming starts with ping)
	pingData := map[string]interface{}{"type": "ping"}
	pingJSON, _ := json.Marshal(pingData)
	fmt.Fprintf(w, "event: ping\ndata: %s\n\n", pingJSON)
	flusher.Flush()

	ch := api.m365Client.ChatConversationStreamGen(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, false)

	fullText := ""
	thinkingText := ""
	truncated := false

	var finalConvID string
	var finalToolCalls []client.ToolCall
	for chunk := range ch {
		if chunk.Error != nil {
			errData := map[string]interface{}{
				"type":    "error",
				"error":   map[string]interface{}{"type": "server_error", "message": chunk.Error.Error()},
			}
			errJSON, _ := json.Marshal(errData)
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", errJSON)
			flusher.Flush()
			return
		}

		if chunk.IsFinal {
			finalConvID = chunk.ConversationID
			finalToolCalls = chunk.ToolCalls
			break
		}

		// Accumulate thinking (not sent as content for Complete API)
		if chunk.Thinking != "" {
			thinkingText += chunk.Thinking
			continue
		}

		// Check max_tokens limit
		if maxTokens > 0 && countTokens(fullText) >= maxTokens {
			truncated = true
			for range ch {
			}
			break
		}

		fullText += chunk.Text

		// Send completion event with delta text
		compData := map[string]interface{}{
			"type":        "completion",
			"completion":  chunk.Text,
			"stop_reason": nil,
			"model":       model,
			"log_id":      logID,
		}
		compJSON, _ := json.Marshal(compData)
		fmt.Fprintf(w, "event: completion\ndata: %s\n\n", compJSON)
		flusher.Flush()
	}
	_ = finalToolCalls

	// Determine stop reason
	stopReason := "end_turn"
	for _, s := range stopSequences {
		if strings.Contains(fullText, s) {
			stopReason = "stop_sequence"
			break
		}
	}
	if truncated {
		stopReason = "max_tokens"
	}

	// Send final completion event with stop_reason
	finalData := map[string]interface{}{
		"type":        "completion",
		"completion":  "",
		"stop_reason": stopReason,
		"model":       model,
		"stop":        nil,
		"log_id":      logID,
	}
	finalJSON, _ := json.Marshal(finalData)
	fmt.Fprintf(w, "event: completion\ndata: %s\n\n", finalJSON)
	flusher.Flush()

	// Cache conversation ID for session continuity
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}
}

// streamChatCompletions streams chat completion responses in OpenAI format.
func (api *APIServer) streamChatCompletions(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxTokens int, hasTools bool, tools []toolcalling.ToolDef) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		api.sendError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	chunkID := fmt.Sprintf("chatcmpl-%s", uuid.New().String())
	openaiModel := cfg.OpenAIID

	ch := api.m365Client.ChatConversationStreamGen(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)

	hasContent := false
	fullText := ""
	thinkingText := ""
	truncated := false

	// When tool calling is enabled AND tools are present, buffer all text and
	// parse for tool calls at the end. Tool call blocks may span multiple
	// chunks, so we can't parse incrementally. When no tools are present, stream
	// text directly regardless of the global ToolCalling flag.
	toolCallingEnabled := hasTools

	var finalConvID string
	var finalToolCalls []client.ToolCall
	for chunk := range ch {
		if chunk.Error != nil {
			api.sendSSEError(w, chunkID, openaiModel, chunk.Error)
			return
		}

		if chunk.IsFinal {
			finalConvID = chunk.ConversationID
			finalToolCalls = chunk.ToolCalls
			break
		}

		// Send thinking as reasoning_content (OpenAI extended thinking format)
		if chunk.Thinking != "" {
			thinkingText += chunk.Thinking
			if !hasContent {
				api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
					"role":              "assistant",
					"reasoning_content": chunk.Thinking,
				})
				hasContent = true
			} else {
				api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
					"reasoning_content": chunk.Thinking,
				})
			}
			flusher.Flush()
			continue
		}

		// Check max_tokens limit before sending more content
		if maxTokens > 0 && countTokens(fullText) >= maxTokens {
			truncated = true
			// Drain remaining chunks
			for range ch {
			}
			break
		}

		fullText += chunk.Text

		// If tool calling is not enabled, stream text directly
		if !toolCallingEnabled {
			if !hasContent {
				api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
					"role":    "assistant",
					"content": chunk.Text,
				})
				hasContent = true
			} else {
				api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
					"content": chunk.Text,
				})
			}
			flusher.Flush()
		}
	}

	// Parse simulated tool calls from full text if tool calling is enabled
	var simToolCalls []toolcalling.ToolCall
	if toolCallingEnabled {
		sim := toolcalling.ParseSimulatedResponse(fullText, toolNamesFromDefs(tools))
		if sim.HasPayload {
			if len(sim.ToolCalls) > 0 {
				simToolCalls = sim.ToolCalls
				fullText = ""
			} else {
				fullText = sim.Content
			}
		}
	}

	// If tool calling buffered text, send it now as a single chunk
	if toolCallingEnabled && fullText != "" && len(simToolCalls) == 0 {
		if !hasContent {
			api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
				"role":    "assistant",
				"content": fullText,
			})
			hasContent = true
		} else {
			api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
				"content": fullText,
			})
		}
		flusher.Flush()
	}

	// Send tool calls in stream if any (from M365 backend or simulated)
	toolCalls := finalToolCalls

	// In simulated mode, discard backend-injected tool calls (e.g.
	// code_interpreter) — only client-declared tools parsed from the
	// simulated JSON response are valid.
	if hasTools {
		toolCalls = nil
	}

	// Append simulated tool calls
	for _, stc := range simToolCalls {
		toolCalls = append(toolCalls, client.ToolCall{
			ID:       stc.ID,
			Type:     "function",
			Function: client.ToolCallFunction{Name: stc.Name, Arguments: string(stc.Arguments)},
		})
	}

	if len(toolCalls) > 0 {
		if !hasContent {
			api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
				"role":    "assistant",
				"content": nil,
			})
		}
		for i, tc := range toolCalls {
			api.sendSSEChunk(w, chunkID, openaiModel, map[string]interface{}{
				"tool_calls": []map[string]interface{}{
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
		flusher.Flush()
	}

	// Send final chunk with usage
	promptStr := fmt.Sprint(messages)
	finishReason := "stop"
	if truncated {
		finishReason = "length"
	}
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}
	promptTok := countTokens(promptStr)
	completionTok := countTokens(fullText)
	reasoningTok := countTokens(thinkingText)
	usage := map[string]interface{}{
		"prompt_tokens":     promptTok,
		"completion_tokens": completionTok,
		"reasoning_tokens":  reasoningTok,
		"total_tokens":      promptTok + completionTok + reasoningTok,
	}

	api.sendSSEDone(w, chunkID, openaiModel, finishReason, usage)
	flusher.Flush()

	// Cache conversation ID for session continuity
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}
}

// nonStreamChatCompletions handles non-streaming chat completion in OpenAI format.
func (api *APIServer) nonStreamChatCompletions(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxTokens int, hasTools bool, tools []toolcalling.ToolDef) {
	respText, thinking, toolCalls, finishReason, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
	if err != nil {
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Chat failed: %v", err))
		return
	}

	// In simulated mode, discard backend-injected tool calls (e.g.
	// code_interpreter) — only client-declared tools parsed from the
	// simulated JSON response are valid.
	if hasTools {
		toolCalls = nil
	}

	// Parse simulated tool calls from response text if tool calling is enabled
	if hasTools {
		sim := toolcalling.ParseSimulatedResponse(respText, toolNamesFromDefs(tools))
		if sim.HasPayload {
			if len(sim.ToolCalls) > 0 {
				finishReason = "tool_calls"
				for _, pc := range sim.ToolCalls {
					toolCalls = append(toolCalls, client.ToolCall{
						ID:       pc.ID,
						Type:     "function",
						Function: client.ToolCallFunction{Name: pc.Name, Arguments: string(pc.Arguments)},
					})
				}
				respText = ""
			} else {
				respText = sim.Content
				finishReason = "stop"
			}
		} else {
			// M365 did not return a simulated JSON payload (e.g. it ran
			// its own server-side tools and returned plain text). Since
			// we discarded backend-injected toolCalls above, reset the
			// finish reason so we don't report tool_use with no blocks.
			finishReason = "stop"
		}
	}

	// Enforce max_tokens on response text
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(respText, maxTokens); ok {
			respText = truncated
			finishReason = "length"
		}
	}

	msg := map[string]interface{}{
		"role":    "assistant",
		"content": respText,
	}

	if thinking != "" {
		msg["reasoning_content"] = thinking
	}

	if len(toolCalls) > 0 {
		openaiToolCalls := make([]map[string]interface{}, len(toolCalls))
		for i, tc := range toolCalls {
			openaiToolCalls[i] = map[string]interface{}{
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
	}

	promptStr := fmt.Sprint(messages)
	promptTok := countTokens(promptStr)
	completionTok := countTokens(respText)
	reasoningTok := countTokens(thinking)
	response := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%s", uuid.New().String()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   cfg.OpenAIID,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     promptTok,
			"completion_tokens": completionTok,
			"reasoning_tokens":  reasoningTok,
			"total_tokens":      promptTok + completionTok + reasoningTok,
		},
	}

	api.sendJSON(w, http.StatusOK, response)

	// Cache conversation ID for session continuity
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}
}

// streamAnthropicMessages streams messages in Anthropic SSE format.
func (api *APIServer) streamAnthropicMessages(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, anthropicModel string, maxTokens int, sid, convID string, hasTools bool, tools []toolcalling.ToolDef) {
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

	// Send message_start event
	header := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         anthropicModel,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  countTokens(fmt.Sprint(messages)),
				"output_tokens": 0,
			},
		},
	}
	api.sendAnthropicSSE(w, "message_start", header)
	flusher.Flush()

	// Stream content with optional thinking block
	fullText := ""
	thinkingText := ""
	truncated := false
	thinkingBlockOpen := false
	textBlockOpen := false
	blockIndex := 0
	toolCallingEnabled := hasTools
	ch := api.m365Client.ChatConversationStreamGen(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)

	var finalConvID string
	var finalToolCalls []client.ToolCall
	for chunk := range ch {
		if chunk.Error != nil {
			errEvent := map[string]interface{}{
				"type": "error",
				"error": map[string]interface{}{
					"type":    "server_error",
					"message": chunk.Error.Error(),
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

		// Handle thinking content
		if chunk.Thinking != "" {
			thinkingText += chunk.Thinking
			if !thinkingBlockOpen {
				cbStart := map[string]interface{}{
					"type":          "content_block_start",
					"index":         blockIndex,
					"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
				}
				api.sendAnthropicSSE(w, "content_block_start", cbStart)
				thinkingBlockOpen = true
			}
			delta := map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": chunk.Thinking},
			}
			api.sendAnthropicSSE(w, "content_block_delta", delta)
			flusher.Flush()
			continue
		}

		// Transition from thinking to text
		if thinkingBlockOpen && !textBlockOpen {
			api.sendAnthropicSSE(w, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": blockIndex})
			blockIndex++
			thinkingBlockOpen = false
		}

		// Open text block on first text chunk (only if not buffering for tool calling)
		if !textBlockOpen && !toolCallingEnabled {
			cbStart := map[string]interface{}{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			}
			api.sendAnthropicSSE(w, "content_block_start", cbStart)
			textBlockOpen = true
		}

		// Check max_tokens limit before sending more content
		if maxTokens > 0 && countTokens(fullText) >= maxTokens {
			truncated = true
			for range ch {
			}
			break
		}

		fullText += chunk.Text

		// If tool calling is not enabled, stream text deltas directly
		if !toolCallingEnabled {
			delta := map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": chunk.Text},
			}
			api.sendAnthropicSSE(w, "content_block_delta", delta)
			flusher.Flush()
		}
	}

	// Parse simulated tool calls from full text if tool calling is enabled
	var simToolCalls []toolcalling.ToolCall
	if toolCallingEnabled {
		sim := toolcalling.ParseSimulatedResponseAnthropic(fullText, toolNamesFromDefs(tools))
		if sim.HasPayload {
			if len(sim.ToolCalls) > 0 {
				simToolCalls = sim.ToolCalls
				fullText = ""
			} else {
				fullText = sim.Content
			}
		}
	}

	// If tool calling buffered text, send it now as a text block
	if toolCallingEnabled && fullText != "" {
		cbStart := map[string]interface{}{
			"type":          "content_block_start",
			"index":         blockIndex,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		}
		api.sendAnthropicSSE(w, "content_block_start", cbStart)
		textBlockOpen = true
		delta := map[string]interface{}{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]interface{}{"type": "text_delta", "text": fullText},
		}
		api.sendAnthropicSSE(w, "content_block_delta", delta)
		flusher.Flush()
	}

	// Close any open blocks
	if thinkingBlockOpen {
		api.sendAnthropicSSE(w, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": blockIndex})
		blockIndex++
	}
	if textBlockOpen {
		api.sendAnthropicSSE(w, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": blockIndex})
		blockIndex++
	}

	// Send tool_use content blocks if any (server-side tools from M365 backend or simulated)
	toolCalls := finalToolCalls

	// In simulated mode, discard backend-injected tool calls (e.g.
	// code_interpreter) — only client-declared tools parsed from the
	// simulated JSON response are valid.
	if hasTools {
		toolCalls = nil
	}

	// Append simulated tool calls
	for _, stc := range simToolCalls {
		toolCalls = append(toolCalls, client.ToolCall{
			ID:       stc.ID,
			Type:     "function",
			Function: client.ToolCallFunction{Name: stc.Name, Arguments: string(stc.Arguments)},
		})
	}

	for _, tc := range toolCalls {
		var input interface{}
		json.Unmarshal([]byte(tc.Function.Arguments), &input)
		if input == nil {
			input = map[string]interface{}{}
		}
		api.sendAnthropicSSE(w, "content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": blockIndex,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			},
		})
		api.sendAnthropicSSE(w, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": blockIndex,
		})
		blockIndex++
	}
	flusher.Flush()

	// Send message_delta event
	stopReason := "end_turn"
	if truncated {
		stopReason = "max_tokens"
	}
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	}
	msgDelta := map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]interface{}{
			"output_tokens":    countTokens(fullText),
			"reasoning_tokens": countTokens(thinkingText),
		},
	}
	api.sendAnthropicSSE(w, "message_delta", msgDelta)
	flusher.Flush()

	// Send message_stop event
	msgStop := map[string]interface{}{"type": "message_stop"}
	api.sendAnthropicSSE(w, "message_stop", msgStop)
	flusher.Flush()

	// Cache conversation ID for session continuity
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}
}

// nonStreamAnthropicMessages handles non-streaming Anthropic messages response.
func (api *APIServer) nonStreamAnthropicMessages(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, anthropicModel string, maxTokens int, sid, convID string, hasTools bool, tools []toolcalling.ToolDef) {
	respText, thinking, toolCalls, finishReason, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
	if err != nil {
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Chat failed: %v", err))
		return
	}

	// In simulated mode, discard backend-injected tool calls (e.g.
	// code_interpreter) — only client-declared tools parsed from the
	// simulated JSON response are valid.
	if hasTools {
		toolCalls = nil
	}

	// Parse simulated tool calls from response text if tool calling is enabled
	if hasTools {
		sim := toolcalling.ParseSimulatedResponseAnthropic(respText, toolNamesFromDefs(tools))
		if sim.HasPayload {
			if len(sim.ToolCalls) > 0 {
				finishReason = "tool_calls"
				for _, pc := range sim.ToolCalls {
					toolCalls = append(toolCalls, client.ToolCall{
						ID:       pc.ID,
						Type:     "function",
						Function: client.ToolCallFunction{Name: pc.Name, Arguments: string(pc.Arguments)},
					})
				}
				respText = ""
			} else {
				respText = sim.Content
				finishReason = "stop"
			}
		} else {
			// M365 did not return a simulated JSON payload (e.g. it ran
			// its own server-side tools and returned plain text). Since
			// we discarded backend-injected toolCalls above, reset the
			// finish reason so we don't report tool_use with no blocks.
			finishReason = "stop"
		}
	}

	stopReason := "end_turn"
	if finishReason == "tool_calls" {
		stopReason = "tool_use"
	}

	// Enforce max_tokens on response text
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(respText, maxTokens); ok {
			respText = truncated
			stopReason = "max_tokens"
		}
	}

	content := []map[string]interface{}{}
	if thinking != "" {
		content = append(content, map[string]interface{}{"type": "thinking", "thinking": thinking})
	}
	if respText != "" {
		content = append(content, map[string]interface{}{"type": "text", "text": respText})
	}

	if len(toolCalls) > 0 {
		for _, tc := range toolCalls {
			var input interface{}
			json.Unmarshal([]byte(tc.Function.Arguments), &input)
			if input == nil {
				input = map[string]interface{}{}
			}
			content = append(content, map[string]interface{}{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			})
		}
	}

	response := map[string]interface{}{
		"id":            fmt.Sprintf("msg_%s", uuid.New().String()),
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         anthropicModel,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":     countTokens(fmt.Sprint(messages)),
			"output_tokens":    countTokens(respText),
			"reasoning_tokens": countTokens(thinking),
		},
	}

	api.sendJSON(w, http.StatusOK, response)

	// Cache conversation ID for session continuity
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}
}

// streamCompletions streams text completion responses in OpenAI text_completion format.
func (api *APIServer) streamCompletions(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, maxTokens int, sid, convID string, hasTools bool, tools []toolcalling.ToolDef) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		api.sendError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	compID := fmt.Sprintf("cmpl-%s", uuid.New().String())
	openaiModel := cfg.OpenAIID

	ch := api.m365Client.ChatConversationStreamGen(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)

	fullText := ""
	thinkingText := ""
	truncated := false
	toolCallingEnabled := hasTools

	var finalConvID string
	var finalToolCalls []client.ToolCall
	for chunk := range ch {
		if chunk.Error != nil {
			errChunk := map[string]interface{}{
				"id":      compID,
				"object":  "text_completion",
				"created": time.Now().Unix(),
				"model":   openaiModel,
				"choices": []map[string]interface{}{
					{
						"index":         0,
						"text":          fmt.Sprintf("Error: %v", chunk.Error),
						"finish_reason": "stop",
						"logprobs":      nil,
					},
				},
			}
			jsonData, _ := json.Marshal(errChunk)
			fmt.Fprintf(w, "data: %s\n\n", jsonData)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		if chunk.IsFinal {
			finalConvID = chunk.ConversationID
			finalToolCalls = chunk.ToolCalls
			break
		}

		// Accumulate thinking text (not sent as content for text_completion)
		if chunk.Thinking != "" {
			thinkingText += chunk.Thinking
			continue
		}

		// Check max_tokens limit before sending more content
		if maxTokens > 0 && countTokens(fullText) >= maxTokens {
			truncated = true
			for range ch {
			}
			break
		}

		fullText += chunk.Text

		// If tool calling is not enabled, stream text directly
		if !toolCallingEnabled {
			chunkData := map[string]interface{}{
				"id":      compID,
				"object":  "text_completion",
				"created": time.Now().Unix(),
				"model":   openaiModel,
				"choices": []map[string]interface{}{
					{
						"index":         0,
						"text":          chunk.Text,
						"finish_reason": nil,
						"logprobs":      nil,
					},
				},
			}

			jsonData, _ := json.Marshal(chunkData)
			fmt.Fprintf(w, "data: %s\n\n", jsonData)
			flusher.Flush()
		}
	}
	_ = finalToolCalls

	// Parse simulated tool calls from buffered text if tool calling is enabled
	var simToolCalls []toolcalling.ToolCall
	if toolCallingEnabled {
		sim := toolcalling.ParseSimulatedResponse(fullText, toolNamesFromDefs(tools))
		if sim.HasPayload {
			if len(sim.ToolCalls) > 0 {
				simToolCalls = sim.ToolCalls
				fullText = ""
			} else {
				fullText = sim.Content
			}
		}
	}


	// If tool calling buffered text, send it now as a single chunk
	if toolCallingEnabled && fullText != "" && len(simToolCalls) == 0 {
		chunkData := map[string]interface{}{
			"id":      compID,
			"object":  "text_completion",
			"created": time.Now().Unix(),
			"model":   openaiModel,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"text":          fullText,
					"finish_reason": nil,
					"logprobs":      nil,
				},
			},
		}
		jsonData, _ := json.Marshal(chunkData)
		fmt.Fprintf(w, "data: %s\n\n", jsonData)
		flusher.Flush()
	}

	// Send final done chunk
	finishReason := "stop"
	if truncated {
		finishReason = "length"
	}
	if len(simToolCalls) > 0 {
		finishReason = "tool_calls"
	}
	doneChunk := map[string]interface{}{
		"id":      compID,
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   openaiModel,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"text":          "",
				"finish_reason": finishReason,
				"logprobs":      nil,
			},
		},
	}
	jsonData, _ := json.Marshal(doneChunk)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	// Cache conversation ID for session continuity
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}
}

// nonStreamCompletions handles non-streaming text completion.
func (api *APIServer) nonStreamCompletions(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, maxTokens int, sid, convID string, hasTools bool, tools []toolcalling.ToolDef) {
	respText, thinking, toolCalls, finishReason, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
	if err != nil {
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Completion failed: %v", err))
		return
	}

	// In simulated mode, discard backend-injected tool calls
	if hasTools {
		toolCalls = nil
	}

	// Parse simulated tool calls from response text
	if hasTools {
		sim := toolcalling.ParseSimulatedResponse(respText, toolNamesFromDefs(tools))
		if sim.HasPayload {
			if len(sim.ToolCalls) > 0 {
				finishReason = "tool_calls"
				for _, pc := range sim.ToolCalls {
					toolCalls = append(toolCalls, client.ToolCall{
						ID:       pc.ID,
						Type:     "function",
						Function: client.ToolCallFunction{Name: pc.Name, Arguments: string(pc.Arguments)},
					})
				}
				respText = ""
			} else {
				respText = sim.Content
				finishReason = "stop"
			}
		} else {
			finishReason = "stop"
		}
	}

	// Enforce max_tokens on response text
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(respText, maxTokens); ok {
			respText = truncated
			finishReason = "length"
		}
	}

	promptStr := fmt.Sprint(messages)
	promptTok := countTokens(promptStr)
	completionTok := countTokens(respText)
	reasoningTok := countTokens(thinking)

	// Build choices
	choices := []map[string]interface{}{
		{
			"index":         0,
			"text":          respText,
			"finish_reason": finishReason,
			"logprobs":      nil,
		},
	}

	// Add tool calls to response if present (non-standard extension for text_completion)
	response := map[string]interface{}{
		"id":      fmt.Sprintf("cmpl-%s", uuid.New().String()),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   cfg.OpenAIID,
		"choices": choices,
		"usage": map[string]interface{}{
			"prompt_tokens":     promptTok,
			"completion_tokens": completionTok,
			"reasoning_tokens":  reasoningTok,
			"total_tokens":      promptTok + completionTok + reasoningTok,
		},
	}

	if len(toolCalls) > 0 {
		openaiToolCalls := make([]map[string]interface{}, len(toolCalls))
		for i, tc := range toolCalls {
			openaiToolCalls[i] = map[string]interface{}{
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

	api.sendJSON(w, http.StatusOK, response)

	// Cache conversation ID for session continuity
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}
}

// sendJSON sends a JSON response.
func (api *APIServer) sendJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(statusCode)

	json.NewEncoder(w).Encode(data)
}

// sendError sends an error response.
func (api *APIServer) sendError(w http.ResponseWriter, statusCode int, message string) {
	api.sendJSON(w, statusCode, map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "error",
			"code":    statusCode,
		},
	})
}

// sendSSEChunk sends a Server-Sent Events chunk in OpenAI chat.completion.chunk format.
func (api *APIServer) sendSSEChunk(w http.ResponseWriter, chunkID, model string, data map[string]interface{}) {
	chunk := map[string]interface{}{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         data,
				"finish_reason": nil,
			},
		},
	}

	jsonData, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
}

// sendSSEDone sends the final SSE chunk.
func (api *APIServer) sendSSEDone(w http.ResponseWriter, chunkID, model, finishReason string, usage map[string]interface{}) {
	if finishReason == "" {
		finishReason = "stop"
	}
	chunk := map[string]interface{}{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finishReason,
			},
		},
	}

	if usage != nil {
		chunk["usage"] = usage
	}

	jsonData, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	fmt.Fprintf(w, "data: [DONE]\n\n")
}

// sendSSEError sends an error via SSE.
func (api *APIServer) sendSSEError(w http.ResponseWriter, chunkID, model string, err error) {
	chunk := map[string]interface{}{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{"content": fmt.Sprintf("Error: %v", err)},
				"finish_reason": "stop",
			},
		},
	}

	jsonData, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	fmt.Fprintf(w, "data: [DONE]\n\n")
}

// sendAnthropicSSE sends an Anthropic-format SSE event.
func (api *APIServer) sendAnthropicSSE(w http.ResponseWriter, eventType string, data map[string]interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
}

// uploadImagesAndAnnotate uploads any images found in message Images fields
// to the M365 backend and attaches the resulting docId annotations to the
// last message with images. This enables multimodal image input support.
func (api *APIServer) uploadImagesAndAnnotate(messages *[]payload.Message, convID string) {
	// Find the last message with images
	lastImgIdx := -1
	for i := len(*messages) - 1; i >= 0; i-- {
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

// injectJSONMode injects JSON mode instructions into messages.
func (api *APIServer) injectJSONMode(messages *[]payload.Message) {
	instruction := "You MUST respond with valid JSON only. Do not include markdown code blocks, explanation, or any text outside the JSON object."

	for i, msg := range *messages {
		if msg.Role == "system" {
			(*messages)[i].Content = msg.Content + "\n" + instruction
			return
		}
	}

	*messages = append([]payload.Message{{Role: "system", Content: instruction}}, *messages...)
}

// injectSimulatedPrompt replaces the last user message with a simulated-mode
// prompt that embeds the entire OpenAI request JSON and asks M365 Copilot to
// produce a valid chat.completion response in a single ```json block.
func injectSimulatedPrompt(messages *[]payload.Message, requestJSON, toolChoice string) {
	if len(*messages) == 0 {
		return
	}
	prompt := toolcalling.BuildSimulatedPrompt(requestJSON, true, toolChoice)
	for i := len(*messages) - 1; i >= 0; i-- {
		if (*messages)[i].Role == "user" {
			(*messages)[i].Content = prompt
			break
		}
	}
}

// injectSimulatedPromptAnthropic replaces the last user message with a
// simulated-mode prompt that embeds the entire Anthropic request JSON and asks
// M365 Copilot to produce a valid Anthropic Messages response in a single
// ```json block.
func injectSimulatedPromptAnthropic(messages *[]payload.Message, requestJSON, toolChoice string) {
	if len(*messages) == 0 {
		return
	}
	prompt := toolcalling.BuildSimulatedPromptAnthropic(requestJSON, true, toolChoice)
	for i := len(*messages) - 1; i >= 0; i-- {
		if (*messages)[i].Role == "user" {
			(*messages)[i].Content = prompt
			break
		}
	}
}

// anthropicToolChoiceString normalizes the Anthropic tool_choice field to a
// string ("any", "auto", "tool", or "") for prompt-building purposes.
func anthropicToolChoiceString(toolChoice map[string]interface{}) string {
	if toolChoice == nil {
		return ""
	}
	if t, ok := toolChoice["type"].(string); ok {
		return t
	}
	return ""
}

// toolChoiceString normalizes the tool_choice field to a string ("auto",
// "required", "none", or a function name) for prompt-building purposes.
func toolChoiceString(toolChoice interface{}) string {
	if toolChoice == nil {
		return ""
	}
	if s, ok := toolChoice.(string); ok {
		return s
	}
	if m, ok := toolChoice.(map[string]interface{}); ok {
		if fn, ok := m["function"].(map[string]interface{}); ok {
			if name, ok := fn["name"].(string); ok {
				return name
			}
		}
	}
	return ""
}

// parseModelSessionID splits a model string of the form "modelKey:sessionID"
// into its components. If there is no colon, sessionID is empty.
// This allows clients that cannot send custom headers/body fields (e.g. Droid
// CLI) to encode a session ID directly in the model name, e.g.
// "gpt5.5-reasoning:dev-test-session-001".
func parseModelSessionID(model string) (modelKey, sessionID string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return model, ""
	}
	return model[:idx], model[idx+1:]
}

// toolNamesFromDefs extracts the function names from a slice of tool
// definitions, for filtering M365-invented tool calls (e.g. code_interpreter)
// out of simulated responses.
func toolNamesFromDefs(tools []toolcalling.ToolDef) []string {
	if len(tools) == 0 {
		return nil
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.Function.Name != "" {
			names = append(names, t.Function.Name)
		}
	}
	return names
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

// tokenEncoder is the tiktoken encoder for cl100k_base (GPT-4/5 family).
var tokenEncoder *tiktoken.Tiktoken

func init() {
	enc, err := tiktoken.EncodingForModel("gpt-4")
	if err != nil {
		enc, err = tiktoken.GetEncoding("cl100k_base")
		if err != nil {
			logging.Warnf("Failed to init tiktoken encoder, falling back to space split: %v", err)
		}
	}
	tokenEncoder = enc
}

// countTokens returns the real BPE token count using tiktoken.
func countTokens(text string) int {
	if strings.TrimSpace(text) == "" {
		return 0
	}
	if tokenEncoder != nil {
		return len(tokenEncoder.Encode(text, nil, nil))
	}
	return len(strings.Split(text, " "))
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
	words := strings.Split(text, " ")
	if len(words) <= maxTokens {
		return text, false
	}
	return strings.Join(words[:maxTokens], " "), true
}

// ===================================================================
// OpenAI Responses API (/v1/responses)
// ===================================================================

// responsesRequest is the JSON body for POST /v1/responses.
type responsesRequest struct {
	Model              string                 `json:"model"`
	Input              interface{}            `json:"input"`
	Instructions       string                 `json:"instructions"`
	Stream             bool                   `json:"stream"`
	MaxOutputTokens    int                    `json:"max_output_tokens"`
	Tools              []toolcalling.ToolDef  `json:"tools"`
	ToolChoice         interface{}            `json:"tool_choice"`
	Temperature        float64                `json:"temperature"`
	PreviousResponseID string                 `json:"previous_response_id"`
	SessionID          string                 `json:"session_id"`
	User               string                 `json:"user"`
	Metadata           map[string]interface{} `json:"metadata"`
}

// handleResponses handles OpenAI Responses API requests.
func (api *APIServer) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to read request body: %v", err))
		return
	}
	r.Body.Close()

	var req responsesRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Parse model (may contain session ID suffix: "gpt5.5:my-session")
	modelKey, modelSessionID := parseModelSessionID(req.Model)
	cfg := models.LookupModel(modelKey)
	if cfg.OpenAIID == "" {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Unknown model: %s", modelKey))
		return
	}

	// Convert Responses API input to payload.Message list
	messages := responsesInputToMessages(req.Input)

	// Prepend instructions as first user message (M365 has no system role)
	if strings.TrimSpace(req.Instructions) != "" && len(messages) > 0 {
		instrMsg := payload.Message{
			Role:    "user",
			Content: "Instructions: " + strings.TrimSpace(req.Instructions),
		}
		messages = append([]payload.Message{instrMsg}, messages...)
	}

	// Inject simulated tool prompt if tools are present
	if len(req.Tools) > 0 {
		injectSimulatedPrompt(&messages, string(bodyBytes), toolChoiceString(req.ToolChoice))
	}

	// Resolve session ID
	// Priority: model-name session > previous_response_id > body session_id > body user > header > hash
	sid := modelSessionID
	if sid == "" {
		sid = req.PreviousResponseID
	}
	if sid == "" {
		sid = req.SessionID
	}
	if sid == "" {
		sid = req.User
	}
	if sid == "" {
		sid = r.Header.Get("X-Session-Id")
	}
	if sid == "" {
		sid = api.hashSessionIDFromMessages(r, messages)
	}

	var convID string
	if sid != "" {
		convID = api.ctxCache.Get("session:" + sid)
	}

	// Upload any images found in multimodal content
	api.uploadImagesAndAnnotate(&messages, convID)

	hasTools := len(req.Tools) > 0

	if req.Stream {
		api.streamResponses(w, messages, cfg, sid, convID, req.MaxOutputTokens, hasTools, req.Tools)
	} else {
		api.nonStreamResponses(w, messages, cfg, sid, convID, req.MaxOutputTokens, hasTools, req.Tools)
	}
}

// responsesInputToMessages converts the Responses API input field (string or
// array of input items) to a slice of payload.Message.
func responsesInputToMessages(input interface{}) []payload.Message {
	if input == nil {
		return []payload.Message{{Role: "user", Content: ""}}
	}

	// Simple string input
	if s, ok := input.(string); ok {
		return []payload.Message{{Role: "user", Content: s}}
	}

	// Array input
	arr, ok := input.([]interface{})
	if !ok {
		return []payload.Message{{Role: "user", Content: ""}}
	}

	var messages []payload.Message
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		itemType, _ := m["type"].(string)

		// Handle function_call_output items (tool results)
		if itemType == "function_call_output" {
			callID, _ := m["call_id"].(string)
			output, _ := m["output"].(string)
			messages = append(messages, payload.Message{
				Role:    "tool",
				Content: output,
				Name:    callID,
			})
			continue
		}

		// Handle function_call items (assistant tool calls in input history)
		if itemType == "function_call" {
			name, _ := m["name"].(string)
			args, _ := m["arguments"].(string)
			messages = append(messages, payload.Message{
				Role:    "assistant",
				Content: fmt.Sprintf("Tool call: %s(%s)", name, args),
			})
			continue
		}

		// Handle reasoning items (skip, M365 generates its own)
		if itemType == "reasoning" {
			continue
		}

		// Message items (type "message" or items with role)
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}

		content := responsesExtractContent(m["content"])
		messages = append(messages, payload.Message{
			Role:    role,
			Content: content,
		})
	}

	if len(messages) == 0 {
		return []payload.Message{{Role: "user", Content: ""}}
	}
	return messages
}

// responsesExtractContent extracts text from a content field that may be a
// string or an array of content parts (input_text, output_text, text types).
func responsesExtractContent(content interface{}) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	arr, ok := content.([]interface{})
	if !ok {
		return ""
	}
	var parts []string
	for _, part := range arr {
		p, ok := part.(map[string]interface{})
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

// buildResponsesObject constructs the non-streaming Responses API response object.
func buildResponsesObject(responseID, model, text, thinking string, toolCalls []client.ToolCall, finishReason string, promptTok, completionTok, reasoningTok int) map[string]interface{} {
	status := "completed"
	if finishReason == "length" {
		status = "incomplete"
	}

	output := []map[string]interface{}{}
	outputIndex := 0

	// Add reasoning item if thinking is present
	if thinking != "" {
		reasoningID := fmt.Sprintf("rs_%s", responseID)
		output = append(output, map[string]interface{}{
			"id":     reasoningID,
			"type":   "reasoning",
			"status": "completed",
			"summary": []map[string]interface{}{
				{
					"type": "summary_text",
					"text": thinking,
				},
			},
		})
		outputIndex++
	}

	// Add function_call items for tool calls
	for i, tc := range toolCalls {
		callID := tc.ID
		if callID == "" {
			callID = fmt.Sprintf("call_%d", i)
		}
		output = append(output, map[string]interface{}{
			"id":        callID,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   callID,
			"name":      tc.Function.Name,
			"arguments": tc.Function.Arguments,
		})
		outputIndex++
	}

	// Add message item with output_text (only if there is text content)
	if text != "" || len(toolCalls) == 0 {
		msgID := fmt.Sprintf("msg_%s", responseID)
		output = append(output, map[string]interface{}{
			"id":     msgID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]interface{}{
				{
					"type":        "output_text",
					"text":        text,
					"annotations": []interface{}{},
				},
			},
		})
		outputIndex++
	}

	resp := map[string]interface{}{
		"id":         responseID,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      model,
		"output":     output,
		"output_text": text,
		"usage": map[string]interface{}{
			"input_tokens":     promptTok,
			"output_tokens":    completionTok,
			"reasoning_tokens": reasoningTok,
			"total_tokens":     promptTok + completionTok + reasoningTok,
		},
	}
	return resp
}

// nonStreamResponses handles non-streaming Responses API requests.
func (api *APIServer) nonStreamResponses(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxTokens int, hasTools bool, tools []toolcalling.ToolDef) {
	respText, thinking, toolCalls, finishReason, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
	if err != nil {
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Chat failed: %v", err))
		return
	}

	// In simulated mode, discard backend-injected tool calls
	if hasTools {
		toolCalls = nil
	}

	// Parse simulated tool calls from response text
	if hasTools {
		sim := toolcalling.ParseSimulatedResponse(respText, toolNamesFromDefs(tools))
		if sim.HasPayload {
			if len(sim.ToolCalls) > 0 {
				finishReason = "tool_calls"
				for _, pc := range sim.ToolCalls {
					toolCalls = append(toolCalls, client.ToolCall{
						ID:       pc.ID,
						Type:     "function",
						Function: client.ToolCallFunction{Name: pc.Name, Arguments: string(pc.Arguments)},
					})
				}
				respText = ""
			} else {
				respText = sim.Content
				finishReason = "stop"
			}
		} else {
			finishReason = "stop"
		}
	}

	// Enforce max_output_tokens
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(respText, maxTokens); ok {
			respText = truncated
			finishReason = "length"
		}
	}

	promptStr := fmt.Sprint(messages)
	promptTok := countTokens(promptStr)
	completionTok := countTokens(respText)
	reasoningTok := countTokens(thinking)

	responseID := fmt.Sprintf("resp_%s", uuid.New().String())
	response := buildResponsesObject(responseID, cfg.OpenAIID, respText, thinking, toolCalls, finishReason, promptTok, completionTok, reasoningTok)

	api.sendJSON(w, http.StatusOK, response)

	// Cache conversation ID for session continuity
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}
}

// streamResponses handles streaming Responses API requests.
func (api *APIServer) streamResponses(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxTokens int, hasTools bool, tools []toolcalling.ToolDef) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		api.sendError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	responseID := fmt.Sprintf("resp_%s", uuid.New().String())
	openaiModel := cfg.OpenAIID

	// Helper to send a Responses SSE event
	sendEvent := func(eventType string, data map[string]interface{}) {
		data["type"] = eventType
		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(w, "data: %s\n\n", jsonData)
		flusher.Flush()
	}

	// Send response.created event
	sendEvent("response.created", map[string]interface{}{
		"response": map[string]interface{}{
			"id":     responseID,
			"object": "response",
			"status": "in_progress",
			"model":  openaiModel,
		},
	})

	// Send response.in_progress event
	sendEvent("response.in_progress", map[string]interface{}{
		"response": map[string]interface{}{
			"id":     responseID,
			"object": "response",
			"status": "in_progress",
			"model":  openaiModel,
		},
	})

	ch := api.m365Client.ChatConversationStreamGen(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)

	fullText := ""
	thinkingText := ""
	truncated := false

	// When tool calling is enabled, buffer all text and parse at the end
	toolCallingEnabled := hasTools

	// Track whether we've emitted the message output item
	messageItemEmitted := false
	reasoningItemEmitted := false
	msgID := fmt.Sprintf("msg_%s", responseID)
	reasoningID := fmt.Sprintf("rs_%s", responseID)

	var finalConvID string
	var finalToolCalls []client.ToolCall
	for chunk := range ch {
		if chunk.Error != nil {
			sendEvent("response.failed", map[string]interface{}{
				"response": map[string]interface{}{
					"id":     responseID,
					"object": "response",
					"status": "failed",
					"error": map[string]interface{}{
						"message": chunk.Error.Error(),
						"type":    "server_error",
					},
					"model": openaiModel,
				},
			})
			return
		}

		if chunk.IsFinal {
			finalConvID = chunk.ConversationID
			finalToolCalls = chunk.ToolCalls
			break
		}

		// Handle thinking/reasoning content
		if chunk.Thinking != "" {
			thinkingText += chunk.Thinking

			if !toolCallingEnabled {
				if !reasoningItemEmitted {
					sendEvent("response.output_item.added", map[string]interface{}{
						"output_index": 0,
						"item": map[string]interface{}{
							"id":     reasoningID,
							"type":   "reasoning",
							"status": "in_progress",
							"summary": []map[string]interface{}{
								{
									"type": "summary_text",
									"text": "",
								},
							},
						},
					})
					reasoningItemEmitted = true
				}
				sendEvent("response.reasoning_summary_text.delta", map[string]interface{}{
					"item_id":      reasoningID,
					"output_index": 0,
					"delta":        chunk.Thinking,
				})
			}
		}

		// Handle text content
		if chunk.Text != "" {
			if toolCallingEnabled {
				// Buffer text for tool call parsing at the end
				fullText += chunk.Text
			} else {
				if !messageItemEmitted {
					// Emit message output item
					outputIdx := 0
					if reasoningItemEmitted {
						outputIdx = 1
					}
					sendEvent("response.output_item.added", map[string]interface{}{
						"output_index": outputIdx,
						"item": map[string]interface{}{
							"id":     msgID,
							"type":   "message",
							"status": "in_progress",
							"role":   "assistant",
							"content": []interface{}{},
						},
					})
					sendEvent("response.content_part.added", map[string]interface{}{
						"item_id":      msgID,
						"output_index": outputIdx,
						"content_index": 0,
						"part": map[string]interface{}{
							"type": "output_text",
							"text": "",
							"annotations": []interface{}{},
						},
					})
					messageItemEmitted = true
				}

				// Check max_tokens
				if maxTokens > 0 && countTokens(fullText+chunk.Text) > maxTokens {
					remaining := maxTokens - countTokens(fullText)
					if remaining > 0 {
						delta, _ := truncateToTokens(chunk.Text, remaining)
						if delta != "" {
							fullText += delta
							outputIdx := 0
							if reasoningItemEmitted {
								outputIdx = 1
							}
							sendEvent("response.output_text.delta", map[string]interface{}{
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

				fullText += chunk.Text
				outputIdx := 0
				if reasoningItemEmitted {
					outputIdx = 1
				}
				sendEvent("response.output_text.delta", map[string]interface{}{
					"item_id":       msgID,
					"output_index":  outputIdx,
					"content_index": 0,
					"delta":         chunk.Text,
				})
			}
		}
	}
	_ = finalToolCalls

	// Finalize reasoning item if emitted
	if reasoningItemEmitted && !toolCallingEnabled {
		sendEvent("response.reasoning_summary_text.done", map[string]interface{}{
			"item_id":      reasoningID,
			"output_index": 0,
			"text":         thinkingText,
		})
		sendEvent("response.output_item.done", map[string]interface{}{
			"output_index": 0,
			"item": map[string]interface{}{
				"id":     reasoningID,
				"type":   "reasoning",
				"status": "completed",
				"summary": []map[string]interface{}{
					{
						"type": "summary_text",
						"text": thinkingText,
					},
				},
			},
		})
	}

	// Handle tool calling: parse buffered text for simulated tool calls
	var toolCalls []client.ToolCall
	finishReason := "stop"

	if toolCallingEnabled {
		sim := toolcalling.ParseSimulatedResponse(fullText, toolNamesFromDefs(tools))
		if sim.HasPayload {
			if len(sim.ToolCalls) > 0 {
				finishReason = "tool_calls"
				for _, pc := range sim.ToolCalls {
					toolCalls = append(toolCalls, client.ToolCall{
						ID:       pc.ID,
						Type:     "function",
						Function: client.ToolCallFunction{Name: pc.Name, Arguments: string(pc.Arguments)},
					})
				}
				fullText = ""
			} else {
				fullText = sim.Content
				finishReason = "stop"
			}
		} else {
			finishReason = "stop"
		}

		// Now emit the buffered text and tool calls as Responses events
		outputIdx := 0
		if reasoningItemEmitted {
			outputIdx = 1
		}

		// Emit tool call items
		for i, tc := range toolCalls {
			callID := tc.ID
			if callID == "" {
				callID = fmt.Sprintf("call_%d", i)
			}
			sendEvent("response.output_item.added", map[string]interface{}{
				"output_index": outputIdx,
				"item": map[string]interface{}{
					"id":      callID,
					"type":    "function_call",
					"status":  "in_progress",
					"call_id": callID,
					"name":    tc.Function.Name,
				},
			})
			sendEvent("response.function_call_arguments.delta", map[string]interface{}{
				"item_id":      callID,
				"output_index": outputIdx,
				"delta":        tc.Function.Arguments,
			})
			sendEvent("response.function_call_arguments.done", map[string]interface{}{
				"item_id":      callID,
				"output_index": outputIdx,
				"arguments":    tc.Function.Arguments,
			})
			sendEvent("response.output_item.done", map[string]interface{}{
				"output_index": outputIdx,
				"item": map[string]interface{}{
					"id":        callID,
					"type":      "function_call",
					"status":    "completed",
					"call_id":   callID,
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			})
			outputIdx++
		}

		// Emit text message item if there's text
		if fullText != "" || len(toolCalls) == 0 {
			// Enforce max_output_tokens
			if maxTokens > 0 {
				if truncated, ok := truncateToTokens(fullText, maxTokens); ok {
					fullText = truncated
					finishReason = "length"
				}
			}

			sendEvent("response.output_item.added", map[string]interface{}{
				"output_index": outputIdx,
				"item": map[string]interface{}{
					"id":     msgID,
					"type":   "message",
					"status": "in_progress",
					"role":   "assistant",
					"content": []interface{}{},
				},
			})
			sendEvent("response.content_part.added", map[string]interface{}{
				"item_id":      msgID,
				"output_index": outputIdx,
				"content_index": 0,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": "",
					"annotations": []interface{}{},
				},
			})
			sendEvent("response.output_text.delta", map[string]interface{}{
				"item_id":       msgID,
				"output_index":  outputIdx,
				"content_index": 0,
				"delta":         fullText,
			})
			sendEvent("response.output_text.done", map[string]interface{}{
				"item_id":       msgID,
				"output_index":  outputIdx,
				"content_index": 0,
				"text":          fullText,
			})
			sendEvent("response.content_part.done", map[string]interface{}{
				"item_id":      msgID,
				"output_index": outputIdx,
				"content_index": 0,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": fullText,
					"annotations": []interface{}{},
				},
			})
			sendEvent("response.output_item.done", map[string]interface{}{
				"output_index": outputIdx,
				"item": map[string]interface{}{
					"id":     msgID,
					"type":   "message",
					"status": "completed",
					"role":   "assistant",
					"content": []map[string]interface{}{
						{
							"type":        "output_text",
							"text":        fullText,
							"annotations": []interface{}{},
						},
					},
				},
			})
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
			sendEvent("response.output_text.done", map[string]interface{}{
				"item_id":       msgID,
				"output_index":  outputIdx,
				"content_index": 0,
				"text":          fullText,
			})
			sendEvent("response.content_part.done", map[string]interface{}{
				"item_id":      msgID,
				"output_index": outputIdx,
				"content_index": 0,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": fullText,
					"annotations": []interface{}{},
				},
			})
			sendEvent("response.output_item.done", map[string]interface{}{
				"output_index": outputIdx,
				"item": map[string]interface{}{
					"id":     msgID,
					"type":   "message",
					"status": "completed",
					"role":   "assistant",
					"content": []map[string]interface{}{
						{
							"type":        "output_text",
							"text":        fullText,
							"annotations": []interface{}{},
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

	promptStr := fmt.Sprint(messages)
	promptTok := countTokens(promptStr)
	completionTok := countTokens(fullText)
	reasoningTok := countTokens(thinkingText)

	finalResponse := buildResponsesObject(responseID, openaiModel, fullText, thinkingText, toolCalls, finishReason, promptTok, completionTok, reasoningTok)
	finalResponse["status"] = status

	sendEvent("response.completed", map[string]interface{}{
		"response": finalResponse,
	})

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	// Cache conversation ID for session continuity
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}
}

// ===================================================================
// OpenAI Responses Compact API (/v1/responses/compact)
// ===================================================================

// defaultCompactionPrompt is the system instruction sent to M365 Copilot when
// compacting a conversation. It asks the model to produce a concise summary
// that preserves key context for continuation.
const defaultCompactionPrompt = "I need a concise summary of the following conversation between a user and an assistant. Please cover the main topics discussed, any decisions made, code or files mentioned, and what was being worked on. Keep it brief but preserve all important context."

// handleResponsesCompact handles POST /v1/responses/compact requests from Codex.
// It sends the conversation history to M365 Copilot with a compaction prompt,
// then returns the summary wrapped in a compaction output item.
func (api *APIServer) handleResponsesCompact(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to read request body: %v", err))
		return
	}
	r.Body.Close()

	var req responsesRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Parse model (may contain session ID suffix)
	modelKey, modelSessionID := parseModelSessionID(req.Model)
	cfg := models.LookupModel(modelKey)
	if cfg.OpenAIID == "" {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Unknown model: %s", modelKey))
		return
	}

	// Convert Responses API input to payload.Message list
	inputMessages := responsesInputToMessages(req.Input)

	// Flatten the conversation history into a single user message with
	// compaction instructions. M365 has no system role and responds to the
	// last user message, so we must merge everything into one message to
	// prevent the model from answering the conversation instead of summarizing it.
	compactionInstr := defaultCompactionPrompt
	instructions := strings.TrimSpace(req.Instructions)
	if instructions != "" {
		compactionInstr = instructions
	}

	var conversationText strings.Builder
	conversationText.WriteString(compactionInstr)
	conversationText.WriteString("\n\n")
	for _, m := range inputMessages {
		conversationText.WriteString(fmt.Sprintf("%s: %s\n", m.Role, m.Content))
	}
	conversationText.WriteString("\nPlease provide the summary now.")

	messages := []payload.Message{
		{Role: "user", Content: conversationText.String()},
	}

	// Resolve session ID (same priority as handleResponses)
	sid := modelSessionID
	if sid == "" {
		sid = req.PreviousResponseID
	}
	if sid == "" {
		sid = req.SessionID
	}
	if sid == "" {
		sid = req.User
	}
	if sid == "" {
		sid = r.Header.Get("X-Session-Id")
	}
	if sid == "" {
		sid = api.hashSessionIDFromMessages(r, messages)
	}

	var convID string
	if sid != "" {
		convID = api.ctxCache.Get("session:" + sid)
	}

	// Upload any images found in multimodal content
	api.uploadImagesAndAnnotate(&messages, convID)

	hasTools := len(req.Tools) > 0

	logging.Infof("handleResponsesCompact: model=%s sid=%s convID=%s stream=%t tools=%d", modelKey, sid, convID, req.Stream, len(req.Tools))

	if req.Stream {
		api.streamResponsesCompact(w, messages, cfg, sid, convID, req.MaxOutputTokens, hasTools, req.Tools)
	} else {
		api.nonStreamResponsesCompact(w, messages, cfg, sid, convID, req.MaxOutputTokens, hasTools, req.Tools)
	}
}

// buildCompactionResponseObject constructs the non-streaming compact response.
// The output contains exactly one compaction item with encrypted_content set
// to the M365 summary text.
func buildCompactionResponseObject(responseID, model, summaryText string, promptTok, completionTok int) map[string]interface{} {
	compactionID := fmt.Sprintf("cmp_%s", responseID)
	output := []map[string]interface{}{
		{
			"id":                compactionID,
			"type":              "compaction",
			"encrypted_content": summaryText,
		},
	}

	return map[string]interface{}{
		"id":         responseID,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "completed",
		"model":      model,
		"output":     output,
		"usage": map[string]interface{}{
			"input_tokens":  promptTok,
			"output_tokens": completionTok,
			"total_tokens":  promptTok + completionTok,
		},
	}
}

// nonStreamResponsesCompact handles non-streaming compact requests.
func (api *APIServer) nonStreamResponsesCompact(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxTokens int, hasTools bool, tools []toolcalling.ToolDef) {
	respText, _, _, _, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
	if err != nil {
		logging.Errorf("nonStreamResponsesCompact: chat failed: %v", err)
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Compaction failed: %v", err))
		return
	}

	// In simulated mode, extract plain content
	if hasTools {
		sim := toolcalling.ParseSimulatedResponse(respText, toolNamesFromDefs(tools))
		if sim.HasPayload {
			respText = sim.Content
		}
	}

	// Enforce max_output_tokens
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(respText, maxTokens); ok {
			respText = truncated
		}
	}

	promptStr := fmt.Sprint(messages)
	promptTok := countTokens(promptStr)
	completionTok := countTokens(respText)

	responseID := fmt.Sprintf("resp_%s", uuid.New().String())
	response := buildCompactionResponseObject(responseID, cfg.OpenAIID, respText, promptTok, completionTok)

	api.sendJSON(w, http.StatusOK, response)

	// Cache conversation ID for session continuity
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}
}

// streamResponsesCompact handles streaming compact requests.
// It emits a standard Responses SSE stream but replaces the output item
// with a single compaction item containing the summary.
func (api *APIServer) streamResponsesCompact(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxTokens int, hasTools bool, tools []toolcalling.ToolDef) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		api.sendError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	responseID := fmt.Sprintf("resp_%s", uuid.New().String())
	openaiModel := cfg.OpenAIID
	compactionID := fmt.Sprintf("cmp_%s", responseID)

	sendEvent := func(eventType string, data map[string]interface{}) {
		data["type"] = eventType
		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(w, "data: %s\n\n", jsonData)
		flusher.Flush()
	}

	// Send response.created event
	sendEvent("response.created", map[string]interface{}{
		"response": map[string]interface{}{
			"id":     responseID,
			"object": "response",
			"status": "in_progress",
			"model":  openaiModel,
		},
	})

	// Send response.in_progress event
	sendEvent("response.in_progress", map[string]interface{}{
		"response": map[string]interface{}{
			"id":     responseID,
			"object": "response",
			"status": "in_progress",
			"model":  openaiModel,
		},
	})

	ch := api.m365Client.ChatConversationStreamGen(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)

	fullText := ""

	var finalConvID string
	var finalToolCalls []client.ToolCall
	for chunk := range ch {
		if chunk.Error != nil {
			logging.Errorf("streamResponsesCompact: stream error: %v", chunk.Error)
			sendEvent("response.failed", map[string]interface{}{
				"response": map[string]interface{}{
					"id":     responseID,
					"object": "response",
					"status": "failed",
					"model":  openaiModel,
					"error":  map[string]interface{}{"message": chunk.Error.Error()},
				},
			})
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		if chunk.Text != "" {
			fullText += chunk.Text
		}
	}
	_ = finalToolCalls

	// In simulated mode, extract plain content
	if hasTools {
		sim := toolcalling.ParseSimulatedResponse(fullText, toolNamesFromDefs(tools))
		if sim.HasPayload {
			fullText = sim.Content
		}
	}

	// Enforce max_output_tokens
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(fullText, maxTokens); ok {
			fullText = truncated
		}
	}

	// Emit the compaction output item
	sendEvent("response.output_item.added", map[string]interface{}{
		"output_index": 0,
		"item": map[string]interface{}{
			"id":   compactionID,
			"type": "compaction",
		},
	})

	sendEvent("response.output_item.done", map[string]interface{}{
		"output_index": 0,
		"item": map[string]interface{}{
			"id":                compactionID,
			"type":              "compaction",
			"encrypted_content": fullText,
		},
	})

	// Build final response object for response.completed
	promptStr := fmt.Sprint(messages)
	promptTok := countTokens(promptStr)
	completionTok := countTokens(fullText)

	finalResponse := buildCompactionResponseObject(responseID, openaiModel, fullText, promptTok, completionTok)

	sendEvent("response.completed", map[string]interface{}{
		"response": finalResponse,
	})

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	// Cache conversation ID for session continuity
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
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
var urlImagePattern = regexp.MustCompile(`!\[[^\]]*\]\((https?://[^)]+)\)`)

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

	var req imageGenerationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logging.Errorf("handleImageGenerations: invalid JSON: %v", err)
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if req.Prompt == "" {
		api.sendError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	logging.Infof("handleImageGenerations: model=%s n=%d size=%s responseFormat=%s sid=%s", req.Model, req.N, req.Size, req.ResponseFormat, req.SessionID)
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
	modelKey, modelSessionID := parseModelSessionID(modelKey)
	cfg := models.LookupModel(modelKey)
	if cfg.OpenAIID == "" {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Unknown model: %s", modelKey))
		return
	}

	// Resolve session ID
	sid := modelSessionID
	if sid == "" {
		sid = req.SessionID
	}
	if sid == "" {
		sid = req.User
	}
	if sid == "" {
		sid = r.Header.Get("X-Session-Id")
	}
	if sid == "" {
		sid = "img-" + uuid.New().String()[:8]
	}

	var convID string
	if sid != "" {
		convID = api.ctxCache.Get("session:" + sid)
	}

	messages := []payload.Message{{Role: "user", Content: fullPrompt}}

	respText, _, _, _, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, false)
	if err != nil {
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Image generation failed: %v", err))
		return
	}

	// Cache conversation ID
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}

	// Extract image URLs from markdown in response text
	dataItems := api.buildOpenAIImageData(respText, req.N, req.Prompt, req.ResponseFormat)
	if len(dataItems) == 0 {
		api.sendError(w, http.StatusInternalServerError, "No images were generated. The model may not have produced an image.")
		return
	}

	api.sendJSON(w, http.StatusOK, map[string]interface{}{
		"created": time.Now().Unix(),
		"data":    dataItems,
	})
}

// handleImageEdits handles OpenAI /v1/images/edits requests.
// It accepts multipart/form-data with an image file, prompt, and optional mask,
// uploads the image to M365, sends the edit prompt, and returns the result.
func (api *APIServer) handleImageEdits(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	if r.Method != http.MethodPost {
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Parse multipart form
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		logging.Errorf("handleImageEdits: failed to parse multipart form: %v", err)
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to parse multipart form: %v", err))
		return
	}

	prompt := r.FormValue("prompt")
	if prompt == "" {
		api.sendError(w, http.StatusBadRequest, "prompt is required")
		return
	}

	modelKey := r.FormValue("model")
	if modelKey == "" {
		modelKey = "gpt5.5-reasoning"
	}
	modelKey, modelSessionID := parseModelSessionID(modelKey)
	cfg := models.LookupModel(modelKey)
	if cfg.OpenAIID == "" {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Unknown model: %s", modelKey))
		return
	}
	logging.Infof("handleImageEdits: model=%s prompt_len=%d images=%d responseFormat=%s", modelKey, len(prompt), len(r.MultipartForm.File["image"]), r.FormValue("response_format"))

	n := 1
	if nStr := r.FormValue("n"); nStr != "" {
		if v, err := fmtAtoi(nStr); err == nil && v > 0 {
			n = v
		}
	}
	size := r.FormValue("size")
	quality := r.FormValue("quality")
	style := r.FormValue("style")
	responseFormat := r.FormValue("response_format")

	// Read image file(s). OpenAI API supports up to 16 images for GPT image models.
	// Multipart form-data may send "image" as multiple form files.
	imageFiles := r.MultipartForm.File["image"]
	if len(imageFiles) == 0 {
		api.sendError(w, http.StatusBadRequest, "image file is required")
		return
	}

	var images []payload.ImageData
	for i, fh := range imageFiles {
		if i >= 16 {
			break
		}
		f, err := fh.Open()
		if err != nil {
			api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to open image %d: %v", i, err))
			return
		}
		imgBytes, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to read image %d: %v", i, err))
			return
		}
		imgB64 := base64.StdEncoding.EncodeToString(imgBytes)
		imgMime := fh.Header.Get("Content-Type")
		if imgMime == "" {
			imgMime = "image/png"
		}
		imgExt := extFromMediaType(imgMime)
		imgName := fmt.Sprintf("edit-%d.%s", i, imgExt)
		images = append(images, payload.ImageData{
			Base64:    imgB64,
			MediaType: imgMime,
			FileName:  imgName,
		})
	}

	// Read optional mask
	var maskB64, maskFileName, maskMimeType string
	if maskFile, maskHeader, err := r.FormFile("mask"); err == nil {
		maskBytes, err := io.ReadAll(maskFile)
		maskFile.Close()
		if err != nil {
			api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to read mask: %v", err))
			return
		}
		maskB64 = base64.StdEncoding.EncodeToString(maskBytes)
		maskMimeType = maskHeader.Header.Get("Content-Type")
		if maskMimeType == "" {
			maskMimeType = "image/png"
		}
		maskFileName = "mask." + extFromMediaType(maskMimeType)
	}

	// Resolve session ID
	sid := modelSessionID
	if sid == "" {
		sid = r.FormValue("session_id")
	}
	if sid == "" {
		sid = r.FormValue("user")
	}
	if sid == "" {
		sid = r.Header.Get("X-Session-Id")
	}
	if sid == "" {
		sid = "img-edit-" + uuid.New().String()[:8]
	}

	var convID string
	if sid != "" {
		convID = api.ctxCache.Get("session:" + sid)
	}

	// Build prompt with hints
	fullPrompt := buildImagePromptWithHints(prompt, size, quality, style)

	// Build multimodal message with image annotations
	msg := payload.Message{Role: "user", Content: fullPrompt}
	msg.Images = images
	if maskB64 != "" {
		msg.Images = append(msg.Images, payload.ImageData{
			Base64:    maskB64,
			MediaType: maskMimeType,
			FileName:  maskFileName,
		})
	}

	messages := []payload.Message{msg}

	// Upload images and attach annotations
	api.uploadImagesAndAnnotate(&messages, convID)

	respText, _, _, _, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, false)
	if err != nil {
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Image edit failed: %v", err))
		return
	}

	// Cache conversation ID
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}

	// Extract image URLs from response
	dataItems := api.buildOpenAIImageData(respText, n, prompt, responseFormat)
	if len(dataItems) == 0 {
		api.sendError(w, http.StatusInternalServerError, "No edited images were generated. The model may not have produced an image.")
		return
	}

	api.sendJSON(w, http.StatusOK, map[string]interface{}{
		"created": time.Now().Unix(),
		"data":    dataItems,
	})
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
		if responseFormat == "b64_json" {
			b64, err := api.downloadAndBase64(u)
			if err != nil {
				logging.Errorf("Failed to download image %s: %v", u, err)
				items = append(items, imageDataItem{
					URL:           u,
					RevisedPrompt: revisedPrompt,
				})
				continue
			}
			items = append(items, imageDataItem{
				B64JSON:       b64,
				RevisedPrompt: revisedPrompt,
			})
		} else {
			// url format: try to download and return as data URL;
			// fall back to raw URL on error
			b64, err := api.downloadAndBase64(u)
			if err != nil {
				logging.Errorf("Failed to download image for data URL %s: %v", u, err)
				items = append(items, imageDataItem{
					URL:           u,
					RevisedPrompt: revisedPrompt,
				})
				continue
			}
			items = append(items, imageDataItem{
				URL:           "data:image/png;base64," + b64,
				RevisedPrompt: revisedPrompt,
			})
		}
	}

	return items
}

// downloadAndBase64 downloads an image from a designerapp URL and returns its
// base64-encoded content. designerapp URLs require a JWE access token (acquired
// via SSO cookies with the M365 web app client_id) and the fileToken query
// parameter sent as a header.
func (api *APIServer) downloadAndBase64(imageURL string) (string, error) {
	logging.Infof("downloadAndBase64: downloading image from %s", imageURL[:min(100, len(imageURL))])
	parsedURL, err := neturl.Parse(imageURL)
	if err != nil {
		logging.Errorf("downloadAndBase64: invalid URL: %v", err)
		return "", fmt.Errorf("invalid image URL: %w", err)
	}

	// Extract fileToken from query params and remove it from the URL
	query := parsedURL.Query()
	fileToken := query.Get("fileToken")
	if fileToken == "" {
		logging.Errorf("downloadAndBase64: no fileToken in URL")
		return "", fmt.Errorf("no fileToken in image URL")
	}
	query.Del("fileToken")
	parsedURL.RawQuery = query.Encode()
	cleanURL := parsedURL.String()

	// Acquire designerapp access token via SSO cookies
	token, err := api.tokenManager.GetDesignerToken()
	if err != nil {
		return "", fmt.Errorf("failed to acquire designer token: %w", err)
	}

	req, err := http.NewRequest("GET", cleanURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create image request: %w", err)
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

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		errCode := resp.Header.Get("X-Errorcode")
		failReason := resp.Header.Get("X-Failurereason")
		logging.Errorf("Image download failed: status=%d, x-errorcode=%s, x-failurereason=%s, body=%s",
			resp.StatusCode, errCode, failReason, string(body)[:min(200, len(body))])
		return "", fmt.Errorf("download returned status %d: x-errorcode=%s, x-failurereason=%s", resp.StatusCode, errCode, failReason)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logging.Errorf("downloadAndBase64: failed to read body: %v", err)
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	logging.Infof("downloadAndBase64: success, size=%d bytes", len(body))
	return base64.StdEncoding.EncodeToString(body), nil
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
