// Package servers provides HTTP API server for M365 Copilot.
// This file implements OpenAI-compatible and Anthropic-compatible API endpoints.
package servers

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

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
)

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
	os.MkdirAll(cacheDir, 0700)
	return &ContextCache{
		cacheDir:   cacheDir,
		mem:        make(map[string]string),
		writeFile:  os.WriteFile,
		removeFile: os.Remove,
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
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if val, ok := cc.mem[key]; ok {
		return val
	}

	data, err := os.ReadFile(cc.path(key))
	if err != nil {
		return ""
	}
	var convID string
	if err := json.Unmarshal(data, &convID); err != nil {
		return ""
	}

	cc.mem[key] = convID
	cc.order = append(cc.order, key)
	cc.evict()

	return convID
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

	data, _ := json.Marshal(convID)
	_ = cc.writeFile(cc.path(key), data, 0600)
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
	// Initialize request transports and optional local coding tools.
	api.m365Client = client.NewM365Client(api.tokenManager)
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
	mux.HandleFunc("/v1/conversations", api.withAuth(api.handleConversations))
	mux.HandleFunc("/v1/conversations/", api.withAuth(api.handleConversation))
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
	return slices.Contains(api.config.APIKeys, token)
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

	modelList := []map[string]any{}
	for _, cfg := range models.ModelRegistry {
		modelList = append(modelList, map[string]any{
			"id":       cfg.OpenAIID,
			"object":   "model",
			"created":  1700000000,
			"owned_by": "microsoft",
		})
	}

	response := map[string]any{
		"object": "list",
		"data":   modelList,
	}

	api.sendJSON(w, http.StatusOK, response)
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
	cfg := models.LookupModel(req.Model)
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

// handleConversation renames or permanently deletes one M365 conversation.
func (api *APIServer) handleConversation(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		api.handleCORS(w, r)
		return
	}
	conversationID := strings.TrimPrefix(r.URL.Path, "/v1/conversations/")
	if conversationID == "" || strings.Contains(conversationID, "/") {
		api.sendError(w, http.StatusNotFound, "Conversation not found")
		return
	}
	conversationClient := client.NewConversationClient(api.tokenManager)
	switch r.Method {
	case http.MethodPatch:
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			api.sendError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			api.sendError(w, http.StatusBadRequest, "name is required")
			return
		}
		if err := conversationClient.RenameConversation(r.Context(), conversationID, strings.TrimSpace(req.Name)); err != nil {
			api.sendConversationError(w, err)
			return
		}
		api.sendJSON(w, http.StatusOK, map[string]any{"id": conversationID, "name": strings.TrimSpace(req.Name)})
	case http.MethodDelete:
		if err := conversationClient.DeleteConversation(r.Context(), conversationID); err != nil {
			api.sendConversationError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		api.sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
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
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-Id")
	w.WriteHeader(http.StatusOK)
}

// getSessionID extracts session ID from headers or request body.
// Priority: X-Session-Id header > session_id body field > user body field > hash(api_key + first_user_message)
func (api *APIServer) getSessionID(r *http.Request, reqBody map[string]any) string {
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
func (api *APIServer) hashSessionID(r *http.Request, reqBody map[string]any) string {
	firstMsg := extractFirstUserMessage(reqBody)
	if firstMsg == "" {
		return ""
	}
	apiKey := api.extractAPIKey(r)
	h := md5.Sum([]byte(apiKey + "\x00" + firstMsg))
	return "h:" + hex.EncodeToString(h[:])
}

// extractFirstUserMessage scans the messages array and returns the first user message content.
func extractFirstUserMessage(reqBody map[string]any) string {
	msgs, ok := reqBody["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
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
		case []any:
			for _, block := range c {
				bm, ok := block.(map[string]any)
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

func (api *APIServer) runToolLoop(r *http.Request, provider toolLoopProvider, messages []payload.Message, cfg models.ModelConfig, convID string, tools []toolcalling.ToolDef, local map[string]bool) (toolLoopResult, error) {
	currentConvID := convID
	seen := make(map[string]bool)
	for iteration := 0; ; iteration++ {
		text, thinking, backendCalls, finishReason, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, currentConvID, api.config.UserOID, api.config.TenantID, len(tools) > 0)
		if err != nil {
			return toolLoopResult{}, err
		}
		if finalConvID != "" {
			currentConvID = finalConvID
		}
		if len(tools) == 0 {
			return toolLoopResult{text: text, thinking: thinking, toolCalls: backendCalls, finishReason: finishReason, conversationID: currentConvID}, nil
		}
		var simulated toolcalling.SimulatedResult
		if provider == toolLoopAnthropic {
			simulated = toolcalling.ParseSimulatedResponseAnthropic(text, toolNamesFromDefs(tools))
		} else {
			simulated = toolcalling.ParseSimulatedResponse(text, toolNamesFromDefs(tools))
		}
		if !simulated.HasPayload || len(simulated.ToolCalls) == 0 {
			if simulated.HasPayload {
				text, finishReason = simulated.Content, "stop"
			}
			return toolLoopResult{text: text, thinking: thinking, finishReason: finishReason, conversationID: currentConvID}, nil
		}
		var callerCalls []client.ToolCall
		var localCalls []toolcalling.ToolCall
		for _, call := range simulated.ToolCalls {
			converted := client.ToolCall{ID: call.ID, Type: "function", Function: client.ToolCallFunction{Name: call.Name, Arguments: string(call.Arguments)}}
			if local[call.Name] {
				localCalls = append(localCalls, call)
			} else {
				callerCalls = append(callerCalls, converted)
			}
		}
		if len(callerCalls) > 0 {
			return toolLoopResult{thinking: thinking, toolCalls: callerCalls, finishReason: "tool_calls", conversationID: currentConvID}, nil
		}
		if iteration >= api.config.CodeToolMaxIterations-1 {
			return toolLoopResult{}, errors.New("coding tool iteration limit reached")
		}
		var resultParts []string
		for _, call := range localCalls {
			key := call.Name + "\x00" + string(call.Arguments)
			if seen[key] {
				return toolLoopResult{}, fmt.Errorf("duplicate coding tool call %q", call.Name)
			}
			seen[key] = true
			var arguments map[string]any
			if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
				arguments = map[string]any{}
			}
			encoded, err := codingtools.MarshalResult(api.codeTools.Execute(r.Context(), call.Name, arguments))
			if err != nil {
				return toolLoopResult{}, fmt.Errorf("serialize coding tool result: %w", err)
			}
			resultParts = append(resultParts, toolcalling.FormatSimulatedToolResult(call.ID, call.Name, string(encoded)))
		}
		messages = append(messages, payload.Message{Role: "user", Content: strings.Join(resultParts, "\n\n")})
		request := map[string]any{"model": cfg.OpenAIID, "messages": messages, "tools": tools, "stream": false}
		requestJSON, err := json.Marshal(request)
		if err != nil {
			return toolLoopResult{}, fmt.Errorf("serialize coding tool continuation: %w", err)
		}
		if provider == toolLoopAnthropic {
			injectSimulatedPromptAnthropic(&messages, string(requestJSON), "auto")
		} else {
			injectSimulatedPrompt(&messages, string(requestJSON), "auto")
		}
	}
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
		Model          string                `json:"model"`
		Messages       []payload.Message     `json:"messages"`
		Stream         bool                  `json:"stream"`
		MaxTokens      int                   `json:"max_tokens"`
		ResponseFormat map[string]any        `json:"response_format"`
		SessionID      string                `json:"session_id"`
		User           string                `json:"user"`
		Tools          []toolcalling.ToolDef `json:"tools"`
		ToolChoice     any                   `json:"tool_choice"`
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

	preparedTools, localTools := api.prepareCodingTools(req.Tools, false)
	req.Tools = preparedTools
	requestJSON := replaceRequestTools(bodyBytes, req.Tools)
	if len(req.Tools) > 0 {
		injectSimulatedPrompt(&req.Messages, requestJSON, toolChoiceString(req.ToolChoice))
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

	if len(localTools) > 0 {
		result, err := api.runToolLoop(r, toolLoopOpenAI, req.Messages, cfg, convID, req.Tools, localTools)
		if err != nil {
			api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Chat failed: %v", err))
			return
		}
		api.respondBufferedChat(w, result, cfg, sid, req.MaxTokens, req.Stream)
		return
	}
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
		ToolChoice any                   `json:"tool_choice"`
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

	var req struct {
		System   json.RawMessage `json:"system"`
		Messages json.RawMessage `json:"messages"`
		Tools    json.RawMessage `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
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
		Model       string                `json:"model"`
		Messages    []payload.Message     `json:"messages"`
		System      json.RawMessage       `json:"system"`
		MaxTokens   int                   `json:"max_tokens"`
		Stream      bool                  `json:"stream"`
		Temperature float64               `json:"temperature"`
		Tools       []toolcalling.ToolDef `json:"tools"`
		ToolChoice  map[string]any        `json:"tool_choice"`
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

	preparedTools, localTools := api.prepareCodingTools(req.Tools, true)
	req.Tools = preparedTools
	requestJSON := replaceRequestTools(bodyBytes, req.Tools)
	if len(req.Tools) > 0 {
		injectSimulatedPromptAnthropic(&chatMessages, requestJSON, anthropicToolChoiceString(req.ToolChoice))
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

	if len(localTools) > 0 {
		result, err := api.runToolLoop(r, toolLoopAnthropic, chatMessages, cfg, convID, req.Tools, localTools)
		if err != nil {
			api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Chat failed: %v", err))
			return
		}
		api.respondBufferedAnthropic(w, result, chatMessages, req.Model, sid, req.MaxTokens, req.Stream)
		return
	}
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

	response := map[string]any{
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
	pingData := map[string]any{"type": "ping"}
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
			errData := map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "server_error", "message": chunk.Error.Error()},
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
		compData := map[string]any{
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
	finalData := map[string]any{
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
	var thinkingText strings.Builder
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
			if sid != "" {
				api.ctxCache.Delete("session:" + sid)
			}
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
			thinkingText.WriteString(chunk.Thinking)
			if toolCallingEnabled {
				continue
			}
			if !hasContent {
				api.sendSSEChunk(w, chunkID, openaiModel, map[string]any{
					"role":              "assistant",
					"reasoning_content": chunk.Thinking,
				})
				hasContent = true
			} else {
				api.sendSSEChunk(w, chunkID, openaiModel, map[string]any{
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
				api.sendSSEChunk(w, chunkID, openaiModel, map[string]any{
					"role":    "assistant",
					"content": chunk.Text,
				})
				hasContent = true
			} else {
				api.sendSSEChunk(w, chunkID, openaiModel, map[string]any{
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

	if toolCallingEnabled {
		thinkingOutput := chatAnthropicThinkingForOutput(thinkingText.String(), true)
		if thinkingOutput != "" {
			if !hasContent {
				api.sendSSEChunk(w, chunkID, openaiModel, map[string]any{
					"role":              "assistant",
					"reasoning_content": thinkingOutput,
				})
				hasContent = true
			} else {
				api.sendSSEChunk(w, chunkID, openaiModel, map[string]any{
					"reasoning_content": thinkingOutput,
				})
			}
			flusher.Flush()
		}
	}

	// If tool calling buffered text, send it now as a single chunk
	if toolCallingEnabled && fullText != "" && len(simToolCalls) == 0 {
		if !hasContent {
			api.sendSSEChunk(w, chunkID, openaiModel, map[string]any{
				"role":    "assistant",
				"content": fullText,
			})
			hasContent = true
		} else {
			api.sendSSEChunk(w, chunkID, openaiModel, map[string]any{
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
			api.sendSSEChunk(w, chunkID, openaiModel, map[string]any{
				"role":    "assistant",
				"content": nil,
			})
		}
		for i, tc := range toolCalls {
			api.sendSSEChunk(w, chunkID, openaiModel, map[string]any{
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
	reasoningTok := countTokens(thinkingText.String())
	usage := map[string]any{
		"prompt_tokens":     promptTok,
		"completion_tokens": completionTok,
		"reasoning_tokens":  reasoningTok,
		"total_tokens":      promptTok + completionTok + reasoningTok,
	}

	api.sendSSEDone(w, chunkID, openaiModel, finishReason, usage)
	flusher.Flush()

	api.updateChatStreamSession(sid, finalConvID, fullText, thinkingText.String(), toolCalls)
}

func (api *APIServer) updateChatStreamSession(sid, finalConvID, fullText, thinkingText string, toolCalls []client.ToolCall) {
	if sid == "" {
		return
	}

	if strings.TrimSpace(fullText) == "" &&
		strings.TrimSpace(thinkingText) == "" &&
		len(toolCalls) == 0 {
		api.ctxCache.Delete("session:" + sid)
		return
	}

	if finalConvID != "" {
		api.ctxCache.Set("session:"+sid, finalConvID)
	}
}

func (api *APIServer) respondBufferedChat(w http.ResponseWriter, result toolLoopResult, cfg models.ModelConfig, sid string, maxTokens int, stream bool) {
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(result.text, maxTokens); ok {
			result.text, result.finishReason = truncated, "length"
		}
	}
	if sid != "" && result.conversationID != "" {
		api.ctxCache.Set("session:"+sid, result.conversationID)
	}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		id := fmt.Sprintf("chatcmpl-%s", uuid.New().String())
		if result.text != "" {
			api.sendSSEChunk(w, id, cfg.OpenAIID, map[string]any{"role": "assistant", "content": result.text})
		}
		for i, call := range result.toolCalls {
			api.sendSSEChunk(w, id, cfg.OpenAIID, map[string]any{"tool_calls": []map[string]any{{"index": i, "id": call.ID, "type": "function", "function": map[string]string{"name": call.Function.Name, "arguments": call.Function.Arguments}}}})
		}
		api.sendSSEDone(w, id, cfg.OpenAIID, result.finishReason, nil)
		return
	}
	message := map[string]any{"role": "assistant", "content": result.text}
	if len(result.toolCalls) > 0 {
		message["content"] = nil
		message["tool_calls"] = result.toolCalls
	}
	api.sendJSON(w, http.StatusOK, map[string]any{"id": fmt.Sprintf("chatcmpl-%s", uuid.New().String()), "object": "chat.completion", "created": time.Now().Unix(), "model": cfg.OpenAIID, "choices": []map[string]any{{"index": 0, "message": message, "finish_reason": result.finishReason}}})
}

// nonStreamChatCompletions handles non-streaming chat completion in OpenAI format.
func (api *APIServer) nonStreamChatCompletions(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxTokens int, hasTools bool, tools []toolcalling.ToolDef) {
	respText, thinking, toolCalls, finishReason, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
	if err != nil {
		if sid != "" {
			api.ctxCache.Delete("session:" + sid)
		}
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Chat failed: %v", err))
		return
	}

	// In simulated mode, discard backend-injected tool calls (e.g.
	// code_interpreter) — only client-declared tools parsed from the
	// simulated JSON response are valid.
	if hasTools {
		toolCalls = nil
		thinking = chatAnthropicThinkingForOutput(thinking, true)
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

	msg := map[string]any{
		"role":    "assistant",
		"content": respText,
	}

	if thinking != "" {
		msg["reasoning_content"] = thinking
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
		msg["tool_calls"] = openaiToolCalls
		if respText == "" {
			msg["content"] = nil
		}
	}

	promptStr := fmt.Sprint(messages)
	promptTok := countTokens(promptStr)
	completionTok := countTokens(respText)
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
				"input_tokens":  countTokens(fmt.Sprint(messages)),
				"output_tokens": 0,
			},
		},
	}
	api.sendAnthropicSSE(w, "message_start", header)
	flusher.Flush()

	// Stream content with optional thinking block
	fullText := ""
	var thinkingText strings.Builder
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
			if sid != "" {
				api.ctxCache.Delete("session:" + sid)
			}
			errEvent := map[string]any{
				"type": "error",
				"error": map[string]any{
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
			thinkingText.WriteString(chunk.Thinking)
			if toolCallingEnabled {
				continue
			}
			if !thinkingBlockOpen {
				cbStart := map[string]any{
					"type":          "content_block_start",
					"index":         blockIndex,
					"content_block": map[string]any{"type": "thinking", "thinking": ""},
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

		// Transition from thinking to text
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
		if maxTokens > 0 && countTokens(fullText) >= maxTokens {
			truncated = true
			for range ch {
			}
			break
		}

		fullText += chunk.Text

		// If tool calling is not enabled, stream text deltas directly
		if !toolCallingEnabled {
			delta := map[string]any{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]any{"type": "text_delta", "text": chunk.Text},
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

	if toolCallingEnabled {
		thinkingOutput := chatAnthropicThinkingForOutput(thinkingText.String(), true)
		if thinkingOutput != "" {
			api.sendAnthropicSSE(w, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			})
			api.sendAnthropicSSE(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]any{"type": "thinking_delta", "thinking": thinkingOutput},
			})
			api.sendAnthropicSSE(w, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": blockIndex,
			})
			blockIndex++
			flusher.Flush()
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
		var input any
		json.Unmarshal([]byte(tc.Function.Arguments), &input)
		if input == nil {
			input = map[string]any{}
		}
		api.sendAnthropicSSE(w, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": blockIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
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
	if truncated {
		stopReason = "max_tokens"
	}
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	}
	msgDelta := map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens":    countTokens(fullText),
			"reasoning_tokens": countTokens(thinkingText.String()),
		},
	}
	api.sendAnthropicSSE(w, "message_delta", msgDelta)
	flusher.Flush()

	// Send message_stop event
	msgStop := map[string]any{"type": "message_stop"}
	api.sendAnthropicSSE(w, "message_stop", msgStop)
	flusher.Flush()

	// Cache conversation ID for session continuity
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}
}

func (api *APIServer) respondBufferedAnthropic(w http.ResponseWriter, result toolLoopResult, messages []payload.Message, model, sid string, maxTokens int, stream bool) {
	stopReason := "end_turn"
	if len(result.toolCalls) > 0 {
		stopReason = "tool_use"
	}
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(result.text, maxTokens); ok {
			result.text, stopReason = truncated, "max_tokens"
		}
	}
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
	if sid != "" && result.conversationID != "" {
		api.ctxCache.Set("session:"+sid, result.conversationID)
	}
	response := map[string]any{"id": fmt.Sprintf("msg_%s", uuid.New().String()), "type": "message", "role": "assistant", "content": content, "model": model, "stop_reason": stopReason, "stop_sequence": nil, "usage": map[string]any{"input_tokens": countTokens(fmt.Sprint(messages)), "output_tokens": countTokens(result.text)}}
	if !stream {
		api.sendJSON(w, http.StatusOK, response)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	api.sendAnthropicSSE(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": response["id"], "type": "message", "role": "assistant", "content": []any{}, "model": model, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": countTokens(fmt.Sprint(messages)), "output_tokens": 0}}})
	for i, block := range content {
		api.sendAnthropicSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": block})
		api.sendAnthropicSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	api.sendAnthropicSSE(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": countTokens(result.text)}})
	api.sendAnthropicSSE(w, "message_stop", map[string]any{"type": "message_stop"})
}

// nonStreamAnthropicMessages handles non-streaming Anthropic messages response.
func (api *APIServer) nonStreamAnthropicMessages(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, anthropicModel string, maxTokens int, sid, convID string, hasTools bool, tools []toolcalling.ToolDef) {
	respText, thinking, toolCalls, finishReason, finalConvID, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
	if err != nil {
		if sid != "" {
			api.ctxCache.Delete("session:" + sid)
		}
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Chat failed: %v", err))
		return
	}

	// In simulated mode, discard backend-injected tool calls (e.g.
	// code_interpreter) — only client-declared tools parsed from the
	// simulated JSON response are valid.
	if hasTools {
		toolCalls = nil
		thinking = chatAnthropicThinkingForOutput(thinking, true)
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

	content := []map[string]any{}
	if thinking != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": thinking})
	}
	if respText != "" {
		content = append(content, map[string]any{"type": "text", "text": respText})
	}

	if len(toolCalls) > 0 {
		for _, tc := range toolCalls {
			var input any
			json.Unmarshal([]byte(tc.Function.Arguments), &input)
			if input == nil {
				input = map[string]any{}
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			})
		}
	}

	response := map[string]any{
		"id":            fmt.Sprintf("msg_%s", uuid.New().String()),
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         anthropicModel,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
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
			errChunk := map[string]any{
				"id":      compID,
				"object":  "text_completion",
				"created": time.Now().Unix(),
				"model":   openaiModel,
				"choices": []map[string]any{
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
			chunkData := map[string]any{
				"id":      compID,
				"object":  "text_completion",
				"created": time.Now().Unix(),
				"model":   openaiModel,
				"choices": []map[string]any{
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
		chunkData := map[string]any{
			"id":      compID,
			"object":  "text_completion",
			"created": time.Now().Unix(),
			"model":   openaiModel,
			"choices": []map[string]any{
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
	doneChunk := map[string]any{
		"id":      compID,
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   openaiModel,
		"choices": []map[string]any{
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

	api.sendJSON(w, http.StatusOK, response)

	// Cache conversation ID for session continuity
	if sid != "" {
		if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}
}

// sendJSON sends a JSON response.
func (api *APIServer) sendJSON(w http.ResponseWriter, statusCode int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(statusCode)

	json.NewEncoder(w).Encode(data)
}

// sendError sends an error response.
func (api *APIServer) sendError(w http.ResponseWriter, statusCode int, message string) {
	api.sendJSON(w, statusCode, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "error",
			"code":    statusCode,
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
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
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
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	fmt.Fprintf(w, "data: [DONE]\n\n")
}

// sendSSEError sends an error via SSE.
func (api *APIServer) sendSSEError(w http.ResponseWriter, chunkID, model string, err error) {
	chunk := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{"content": fmt.Sprintf("Error: %v", err)},
				"finish_reason": "stop",
			},
		},
	}

	jsonData, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	fmt.Fprintf(w, "data: [DONE]\n\n")
}

// sendAnthropicSSE sends an Anthropic-format SSE event.
func (api *APIServer) sendAnthropicSSE(w http.ResponseWriter, eventType string, data map[string]any) {
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

var chatAnthropicSimulationMetaPattern = regexp.MustCompile(
	`(?i)(generat\w*\s+(a\s+|the\s+)?json|chatcmpl-|chat\.completion|simulat\w+\s+(an?\s+)?(openai|anthropic)?\s*response|json\s+(code\s+)?block|"?tool_calls"?|"?finish_reason"?)`,
)

func chatAnthropicThinkingForOutput(thinking string, simulated bool) string {
	if !simulated || thinking == "" {
		return thinking
	}

	var output []string
	inFence := false
	for _, line := range strings.Split(thinking, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence || chatAnthropicSimulationMetaPattern.MatchString(line) {
			continue
		}
		output = append(output, line)
	}

	return strings.TrimSpace(strings.Join(output, "\n"))
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
			if currentUserMessage := strings.TrimSpace((*messages)[i].Content); currentUserMessage != "" {
				prompt += "\n\nCURRENT USER MESSAGE\n" + currentUserMessage
			}
			(*messages)[i].Content = prompt
			break
		}
	}
}

// injectSimulatedPromptResponses replaces the converted Responses history with
// one canonical simulation message. The full history remains present exactly
// once inside requestJSON, avoiding duplicated context at the M365 layer.
func injectSimulatedPromptResponses(messages *[]payload.Message, requestJSON, toolChoice string) {
	prompt := toolcalling.BuildSimulatedPromptResponses(requestJSON, true, toolChoice)
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
func injectSimulatedPromptAnthropic(messages *[]payload.Message, requestJSON, toolChoice string) {
	if len(*messages) == 0 {
		return
	}
	prompt := toolcalling.BuildSimulatedPromptAnthropic(requestJSON, true, toolChoice)
	for i := len(*messages) - 1; i >= 0; i-- {
		if (*messages)[i].Role == "user" {
			if currentUserMessage := strings.TrimSpace((*messages)[i].Content); currentUserMessage != "" {
				prompt += "\n\nCURRENT USER MESSAGE\n" + currentUserMessage
			}
			(*messages)[i].Content = prompt
			break
		}
	}
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
}

type responsesSimulationResult struct {
	content      string
	toolCalls    []client.ToolCall
	finishReason string
}

func newResponsesToolPolicy(tools []toolcalling.ToolDef, toolChoice interface{}) (responsesToolPolicy, error) {
	allNames := responsesToolNames(tools)
	knownNames := make(map[string]bool, len(tools))
	for _, name := range allNames {
		knownNames[name] = true
	}

	policy := responsesToolPolicy{
		simulate:         len(tools) > 0,
		promptChoice:     "auto",
		allowedToolNames: allNames,
		tools:            tools,
	}

	switch choice := toolChoice.(type) {
	case nil:
		// Responses defaults to auto when tools are present.
	case string:
		normalized := strings.ToLower(strings.TrimSpace(choice))
		switch normalized {
		case "", "auto":
		case "none":
			policy.simulate = false
			policy.promptChoice = "none"
			policy.allowedToolNames = nil
		case "required":
			policy.required = true
			policy.promptChoice = "required"
		default:
			if !knownNames[choice] {
				return responsesToolPolicy{}, fmt.Errorf("invalid Responses tool_choice %q", choice)
			}
			policy.required = true
			policy.requiredName = choice
			policy.promptChoice = choice
			policy.allowedToolNames = []string{choice}
		}
	case map[string]interface{}:
		name, _ := choice["name"].(string)
		choiceType, _ := choice["type"].(string)
		if name == "" {
			if function, ok := choice["function"].(map[string]interface{}); ok {
				name, _ = function["name"].(string)
			}
		}
		if name == "" && choiceType != "" && choiceType != "function" && choiceType != "custom" {
			name = choiceType
		}
		name = strings.TrimSpace(name)
		if name == "" || !knownNames[name] {
			return responsesToolPolicy{}, fmt.Errorf("invalid Responses named tool_choice %q", name)
		}
		policy.required = true
		policy.requiredName = name
		policy.promptChoice = name
		policy.allowedToolNames = []string{name}
	default:
		return responsesToolPolicy{}, fmt.Errorf("invalid Responses tool_choice type %T", toolChoice)
	}

	if policy.simulate && len(policy.allowedToolNames) == 0 {
		return responsesToolPolicy{}, errors.New("Responses tools must include at least one function name")
	}
	if policy.required && !policy.simulate {
		return responsesToolPolicy{}, errors.New("Responses tool_choice requires at least one tool")
	}
	return policy, nil
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
		types[responsesToolKey(tool.Namespace, name)] = toolType
	}
	return types
}

func responsesToolDefsFromRaw(raw interface{}) []toolcalling.ToolDef {
	return responsesToolDefsFromRawNamespace(raw, "")
}

func responsesToolDefsFromRawNamespace(raw interface{}, inheritedNamespace string) []toolcalling.ToolDef {
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	var definitions []toolcalling.ToolDef
	for _, item := range items {
		tool, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		toolType, _ := tool["type"].(string)
		if toolType == "namespace" {
			namespace, _ := tool["name"].(string)
			if namespace == "" {
				namespace = inheritedNamespace
			}
			definitions = append(
				definitions,
				responsesToolDefsFromRawNamespace(tool["tools"], namespace)...,
			)
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" && toolType != "" && toolType != "function" && toolType != "custom" {
			name = toolType
		}
		if name == "" {
			continue
		}
		namespace, _ := tool["namespace"].(string)
		if namespace == "" {
			namespace = inheritedNamespace
		}
		definitions = append(definitions, toolcalling.ToolDef{
			Type:      toolType,
			Name:      name,
			Namespace: namespace,
		})
	}
	return definitions
}

func mergeLoadedResponsesTools(input interface{}, tools []toolcalling.ToolDef) []toolcalling.ToolDef {
	items, ok := input.([]interface{})
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
		record, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		itemType, _ := record["type"].(string)
		if itemType != "tool_search_output" && itemType != "additional_tools" {
			continue
		}
		for _, tool := range responsesToolDefsFromRaw(record["tools"]) {
			name := responsesToolName(tool)
			key := responsesToolKey(tool.Namespace, name)
			if name == "" || seen[key] {
				continue
			}
			seen[key] = true
			tools = append(tools, tool)
		}
	}
	return tools
}

func buildResponsesToolCallItem(callID string, call client.ToolCall, toolTypes map[string]string, status string) map[string]interface{} {
	toolKey := responsesToolKey(call.Function.Namespace, call.Function.Name)
	if toolTypes[toolKey] == "tool_search" {
		var arguments interface{}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &arguments); err != nil || arguments == nil {
			arguments = map[string]interface{}{"query": call.Function.Arguments}
		}
		return map[string]interface{}{
			"id":        callID,
			"type":      "tool_search_call",
			"execution": "client",
			"status":    status,
			"call_id":   callID,
			"arguments": arguments,
		}
	}
	item := map[string]interface{}{
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
	simulated := toolcalling.ParseSimulatedResponseResponses(text, policy.allowedToolNames)
	if simulated.HasPayload {
		result.content = simulated.Content
		if len(simulated.ToolCalls) > 0 {
			result.finishReason = "tool_calls"
			for _, parsed := range simulated.ToolCalls {
				namespace, ok := resolveResponsesToolNamespace(
					parsed.Name,
					parsed.Namespace,
					policy.tools,
				)
				if !ok {
					continue
				}
				result.toolCalls = append(result.toolCalls, client.ToolCall{
					ID:   parsed.ID,
					Type: "function",
					Function: client.ToolCallFunction{
						Name:      parsed.Name,
						Namespace: namespace,
						Arguments: string(parsed.Arguments),
					},
				})
			}
		}
	}
	if len(result.toolCalls) > 0 && strings.TrimSpace(result.content) == "" {
		result.content = "I'm using the relevant tool now and will continue with its result."
	}

	if policy.required && len(result.toolCalls) == 0 {
		if policy.requiredName != "" {
			return responsesSimulationResult{}, fmt.Errorf("%w: required tool %q was not emitted", errSimulatedToolCallRequired, policy.requiredName)
		}
		return responsesSimulationResult{}, fmt.Errorf("%w: no valid client tool call was emitted", errSimulatedToolCallRequired)
	}
	return result, nil
}

func parseResponsesSimulationWithRetry(
	text string,
	policy responsesToolPolicy,
	requiredRetry func() (string, error),
	emptyRetry func() (string, error),
) (responsesSimulationResult, error) {
	result, err := parseResponsesSimulation(text, policy)
	if err == nil {
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
	if requiredRetry == nil ||
		!errors.Is(err, errSimulatedToolCallRequired) {
		return result, err
	}

	for attempt := 0; attempt < 2; attempt++ {
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

	retried := append([]payload.Message(nil), messages...)
	for index := len(retried) - 1; index >= 0; index-- {
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
			stream := call(ctx, conversationID)
			sawVisibleChunk := false
			sawFinal := false

			for chunk := range stream {
				if chunk.Error != nil {
					emit(chunk)
					return
				}
				if chunk.IsFinal {
					sawFinal = true
					if sawVisibleChunk || attempt >= len(retryDelays) {
						emit(chunk)
						return
					}
					break
				}
				if simulatedTransport {
					// Simulated prompts can put the transport envelope in the
					// upstream thinking channel. The Responses handler only
					// needs raw text for its safe content extractor.
					chunk.Thinking = ""
				}
				if chunk.Text == "" && chunk.Thinking == "" {
					continue
				}
				sawVisibleChunk = true
				if !emit(chunk) {
					return
				}
			}

			if !sawFinal {
				if ctx.Err() == nil {
					emit(client.StreamChunk{Error: client.ErrConnectionClosed})
				}
				return
			}
			if attempt >= len(retryDelays) {
				return
			}

			logging.Warnf(
				"Responses upstream stream completed empty; retrying attempt=%d/%d",
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
				return
			}
			conversationID = ""
		}
	}()
	return output
}

func buildResponsesFailedEvent(
	responseID, model, code, message string,
	sequenceNumber int,
) map[string]interface{} {
	return map[string]interface{}{
		"type":            "response.failed",
		"sequence_number": sequenceNumber,
		"response": map[string]interface{}{
			"id":     responseID,
			"object": "response",
			"status": "failed",
			"model":  model,
			"error": map[string]interface{}{
				"message": message,
				"type":    "server_error",
				"code":    code,
			},
		},
	}
}

func writeResponsesServerError(w http.ResponseWriter, stream bool, responseID, model, code, message string) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		event := buildResponsesFailedEvent(
			responseID,
			model,
			code,
			message,
			0,
		)
		jsonData, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", jsonData)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "server_error",
			"code":    code,
		},
	})
}

func writeResponsesSimulationError(w http.ResponseWriter, stream bool, responseID, model string, err error) {
	writeResponsesServerError(
		w,
		stream,
		responseID,
		model,
		simulatedToolCallRequiredCode,
		err.Error(),
	)
}

func writeResponsesUpstreamEmptyError(w http.ResponseWriter, stream bool, responseID, model string) {
	writeResponsesServerError(
		w,
		stream,
		responseID,
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
func parseModelSessionID(model string) (modelKey, sessionID string) {
	modelKey, sessionID, found := strings.Cut(model, ":")
	if !found {
		return model, ""
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

	req.Tools = mergeLoadedResponsesTools(req.Input, req.Tools)
	preparedTools, localTools := api.prepareCodingTools(req.Tools, false)
	req.Tools = preparedTools
	toolPolicy, err := newResponsesToolPolicy(req.Tools, req.ToolChoice)
	if err != nil {
		api.sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	requestJSON := replaceRequestTools(bodyBytes, req.Tools)

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

	// Inject one Responses-aware simulation prompt unless tool_choice disables
	// client tool use.
	if toolPolicy.simulate {
		injectSimulatedPromptResponses(&messages, requestJSON, toolPolicy.promptChoice)
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

	if len(localTools) > 0 {
		result, err := api.runToolLoop(r, toolLoopOpenAI, messages, cfg, convID, req.Tools, localTools)
		if err != nil {
			api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Response failed: %v", err))
			return
		}
		api.respondBufferedResponses(
			w,
			result,
			messages,
			cfg,
			sid,
			req.MaxOutputTokens,
			req.Stream,
			responsesToolTypes(toolPolicy.tools),
		)
		return
	}
	if req.Stream {
		api.streamResponses(
			r.Context(),
			w,
			messages,
			cfg,
			sid,
			convID,
			req.MaxOutputTokens,
			toolPolicy,
		)
	} else {
		api.nonStreamResponses(
			r.Context(),
			w,
			messages,
			cfg,
			sid,
			convID,
			req.MaxOutputTokens,
			toolPolicy,
		)
	}
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

		itemType, _ := m["type"].(string)

		// Handle function_call_output items (tool results)
		if itemType == "function_call_output" {
			callID, _ := m["call_id"].(string)
			output, _ := m["output"].(string)
			if output == "" && m["output"] != nil {
				encoded, _ := json.Marshal(m["output"])
				output = string(encoded)
			}
			messages = append(messages, payload.Message{
				Role: "tool",
				Content: fmt.Sprintf(
					"Authoritative tool result (call_id: %s):\n%s",
					callID,
					output,
				),
				ToolCallID: callID,
			})
			continue
		}

		// Handle function_call items (assistant tool calls in input history)
		if itemType == "function_call" {
			name, _ := m["name"].(string)
			namespace, _ := m["namespace"].(string)
			args, _ := m["arguments"].(string)
			qualifiedName := name
			if namespace != "" {
				qualifiedName = namespace + "/" + name
			}
			messages = append(messages, payload.Message{
				Role:    "assistant",
				Content: fmt.Sprintf("Tool call: %s(%s)", qualifiedName, args),
			})
			continue
		}

		// Handle reasoning items (skip, M365 generates its own)
		if itemType == "reasoning" {
			continue
		}

		if itemType == "tool_search_call" {
			arguments, _ := json.Marshal(m["arguments"])
			messages = append(messages, payload.Message{
				Role:    "assistant",
				Content: fmt.Sprintf("Tool search call: tool_search(%s)", string(arguments)),
			})
			continue
		}

		if itemType == "tool_search_output" {
			toolsJSON, _ := json.Marshal(m["tools"])
			messages = append(messages, payload.Message{
				Role:    "tool",
				Content: "tool_search_output: preserve these loaded tools with their exact namespace, name, and schema: " + string(toolsJSON),
			})
			continue
		}

		if itemType == "additional_tools" {
			toolsJSON, _ := json.Marshal(m["tools"])
			messages = append(messages, payload.Message{
				Role:    "tool",
				Content: "additional_tools: preserve these callable tools with their exact namespace, name, and schema: " + string(toolsJSON),
			})
			continue
		}

		if itemType == "compaction" {
			summary, _ := m["encrypted_content"].(string)
			if strings.TrimSpace(summary) != "" {
				messages = append(messages, payload.Message{
					Role:    "user",
					Content: "Summary of the earlier conversation:\n" + summary,
				})
			}
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

// buildResponsesObject constructs the non-streaming Responses API response object.
func buildResponsesObject(responseID, model, text, thinking string, toolCalls []client.ToolCall, toolTypes map[string]string, finishReason string, promptTok, completionTok, reasoningTok int) map[string]any {
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
		phase := "final_answer"
		if len(toolCalls) > 0 {
			phase = "commentary"
		}
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
	for i, tc := range toolCalls {
		callID := tc.ID
		if callID == "" {
			callID = fmt.Sprintf("call_%d", i)
		}
		output = append(output, buildResponsesToolCallItem(callID, tc, toolTypes, "completed"))
		outputIndex++
	}

	resp := map[string]any{
		"id":          responseID,
		"object":      "response",
		"created_at":  time.Now().Unix(),
		"status":      status,
		"model":       model,
		"output":      output,
		"output_text": text,
		"usage": map[string]any{
			"input_tokens":     promptTok,
			"output_tokens":    completionTok,
			"reasoning_tokens": reasoningTok,
			"total_tokens":     promptTok + completionTok + reasoningTok,
		},
	}
	return resp
}

func (api *APIServer) respondBufferedResponses(w http.ResponseWriter, result toolLoopResult, messages []payload.Message, cfg models.ModelConfig, sid string, maxTokens int, stream bool, toolTypes map[string]string) {
	if maxTokens > 0 {
		if truncated, ok := truncateToTokens(result.text, maxTokens); ok {
			result.text, result.finishReason = truncated, "length"
		}
	}
	if sid != "" && result.conversationID != "" {
		api.ctxCache.Set("session:"+sid, result.conversationID)
	}
	responseID := fmt.Sprintf("resp_%s", uuid.New().String())
	response := buildResponsesObject(responseID, cfg.OpenAIID, result.text, result.thinking, result.toolCalls, toolTypes, result.finishReason, countTokens(fmt.Sprint(messages)), countTokens(result.text), countTokens(result.thinking))
	if !stream {
		api.sendJSON(w, http.StatusOK, response)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range []string{"response.created", "response.in_progress"} {
		data, _ := json.Marshal(map[string]any{"type": event, "response": map[string]any{"id": responseID, "object": "response", "status": "in_progress", "model": cfg.OpenAIID}})
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", completed)
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
		api.ctxCache.Delete("session:" + sid)
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
) {
	result, err := api.responsesConversation(
		ctx,
		messages,
		cfg,
		convID,
		toolPolicy.simulate,
		func() {
			if sid != "" {
				api.ctxCache.Delete("session:" + sid)
			}
		},
	)
	if err != nil {
		if sid != "" {
			api.ctxCache.Delete("session:" + sid)
		}
		if api.responsesRequestCanceled(ctx, sid) {
			return
		}
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Chat failed: %v", err))
		return
	}
	respText := result.text
	thinking := result.thinking
	toolCalls := result.toolCalls
	finishReason := result.finishReason
	finalConvID := result.conversationID

	// In simulated mode, discard backend-injected tool calls
	if toolPolicy.simulate {
		toolCalls = nil
	}

	// Parse simulated tool calls from response text
	if toolPolicy.simulate {
		simulated, parseErr := parseResponsesSimulationWithRetry(
			respText,
			toolPolicy,
			func() (string, error) {
				if sid != "" {
					api.ctxCache.Delete("session:" + sid)
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
				respText = retryResult.text
				thinking = retryResult.thinking
				toolCalls = retryResult.toolCalls
				finishReason = retryResult.finishReason
				finalConvID = retryResult.conversationID
				return retryResult.text, nil
			},
			func() (string, error) {
				if sid != "" {
					api.ctxCache.Delete("session:" + sid)
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
				respText = retryResult.text
				thinking = retryResult.thinking
				toolCalls = nil
				finishReason = retryResult.finishReason
				finalConvID = retryResult.conversationID
				return retryResult.text, nil
			},
		)
		if parseErr != nil {
			if sid != "" {
				api.ctxCache.Delete("session:" + sid)
			}
			if api.responsesRequestCanceled(ctx, sid) {
				return
			}
			if errors.Is(parseErr, errSimulatedToolCallRequired) {
				writeResponsesSimulationError(
					w,
					false,
					"",
					cfg.OpenAIID,
					parseErr,
				)
			} else {
				writeResponsesServerError(
					w,
					false,
					"",
					cfg.OpenAIID,
					"upstream_error",
					parseErr.Error(),
				)
			}
			return
		}
		respText = simulated.content
		toolCalls = simulated.toolCalls
		finishReason = simulated.finishReason
	}
	thinking = responsesReasoningForOutput(thinking, toolPolicy.simulate)
	if responsesResultEmpty(respText, toolCalls) {
		if sid != "" {
			api.ctxCache.Delete("session:" + sid)
		}
		writeResponsesUpstreamEmptyError(
			w,
			false,
			"",
			cfg.OpenAIID,
		)
		return
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
	response := buildResponsesObject(responseID, cfg.OpenAIID, respText, thinking, toolCalls, responsesToolTypes(toolPolicy.tools), finishReason, promptTok, completionTok, reasoningTok)

	api.sendJSON(w, http.StatusOK, response)

	// Cache conversation ID for session continuity
	if sid != "" {
		if shouldResetResponsesSession(respText, toolCalls, nil) {
			api.ctxCache.Delete("session:" + sid)
		} else if finalConvID != "" {
			api.ctxCache.Set("session:"+sid, finalConvID)
		}
	}
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

	responseID := fmt.Sprintf("resp_%s", uuid.New().String())
	openaiModel := cfg.OpenAIID

	// Helper to send a Responses SSE event
	sequenceNumber := 0
	sendEvent := func(eventType string, data map[string]any) {
		data["type"] = eventType
		data["sequence_number"] = sequenceNumber
		sequenceNumber++
		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(w, "data: %s\n\n", jsonData)
		flusher.Flush()
	}
	sendFailed := func(code, message string) {
		event := buildResponsesFailedEvent(
			responseID,
			openaiModel,
			code,
			message,
			sequenceNumber,
		)
		sequenceNumber++
		jsonData, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", jsonData)
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	// Send response.created event
	sendEvent("response.created", map[string]any{
		"response": map[string]any{
			"id":     responseID,
			"object": "response",
			"status": "in_progress",
			"model":  openaiModel,
		},
	})

	// Send response.in_progress event
	sendEvent("response.in_progress", map[string]any{
		"response": map[string]any{
			"id":     responseID,
			"object": "response",
			"status": "in_progress",
			"model":  openaiModel,
		},
	})

	ch := responsesStreamWithEmptyRetry(
		ctx,
		convID,
		responsesEmptyRetrySchedule(toolPolicy.simulate),
		toolPolicy.simulate,
		func() {
			if sid != "" {
				api.ctxCache.Delete("session:" + sid)
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

	fullText := ""
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

	var finalConvID string
	for chunk := range ch {
		if chunk.Error != nil {
			if sid != "" {
				api.ctxCache.Delete("session:" + sid)
			}
			sendFailed("upstream_error", chunk.Error.Error())
			return
		}

		if chunk.IsFinal {
			finalConvID = chunk.ConversationID
			break
		}

		// Simulated tool prompts contain transport JSON in M365 thinking
		// summaries. Never expose that content as Responses reasoning.
		if chunk.Thinking != "" && !toolCallingEnabled {
			thinkingText.WriteString(chunk.Thinking)

			if !reasoningItemEmitted {
				sendEvent("response.output_item.added", map[string]any{
					"output_index": 0,
					"item": map[string]any{
						"id":     reasoningID,
						"type":   "reasoning",
						"status": "in_progress",
						"summary": []map[string]any{
							{
								"type": "summary_text",
								"text": "",
							},
						},
					},
				})
				sendEvent("response.reasoning_summary_part.added", map[string]any{
					"item_id":       reasoningID,
					"output_index":  0,
					"summary_index": 0,
					"part": map[string]any{
						"type": "summary_text",
						"text": "",
					},
				})
				reasoningItemEmitted = true
			}
			sendEvent("response.reasoning_summary_text.delta", map[string]any{
				"item_id":       reasoningID,
				"output_index":  0,
				"summary_index": 0,
				"delta":         chunk.Thinking,
			})
		}

		// Handle text content
		if chunk.Text != "" {
			if toolCallingEnabled {
				// Keep the raw transport for final tool-call parsing, while
				// publishing only decoded assistant content.
				fullText += chunk.Text
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
							"id":      msgID,
							"type":    "message",
							"status":  "in_progress",
							"role":    "assistant",
							"phase":   "final_answer",
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

				fullText += chunk.Text
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
	if api.responsesRequestCanceled(ctx, sid) {
		return
	}

	if !toolCallingEnabled && responsesResultEmpty(fullText, nil) {
		if sid != "" {
			api.ctxCache.Delete("session:" + sid)
		}
		sendFailed(
			upstreamEmptyResponseCode,
			"The upstream completed without assistant content or a tool call.",
		)
		return
	}

	// Finalize reasoning item if emitted
	if reasoningItemEmitted && !toolCallingEnabled {
		sendEvent("response.reasoning_summary_text.done", map[string]any{
			"item_id":       reasoningID,
			"output_index":  0,
			"summary_index": 0,
			"text":          thinkingText.String(),
		})
		sendEvent("response.reasoning_summary_part.done", map[string]any{
			"item_id":       reasoningID,
			"output_index":  0,
			"summary_index": 0,
			"part": map[string]any{
				"type": "summary_text",
				"text": thinkingText.String(),
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
						"text": thinkingText.String(),
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
					api.ctxCache.Delete("session:" + sid)
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
					api.ctxCache.Delete("session:" + sid)
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
				api.ctxCache.Delete("session:" + sid)
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
		if truncated {
			finishReason = "length"
		}
		if responsesResultEmpty(fullText, toolCalls) {
			if sid != "" {
				api.ctxCache.Delete("session:" + sid)
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
			phase := "final_answer"
			if len(toolCalls) > 0 {
				phase = "commentary"
			}
			sendEvent("response.output_text.done", map[string]interface{}{
				"item_id":       msgID,
				"output_index":  messageOutputIndex,
				"content_index": 0,
				"text":          fullText,
			})
			sendEvent("response.content_part.done", map[string]interface{}{
				"item_id":       msgID,
				"output_index":  messageOutputIndex,
				"content_index": 0,
				"part": map[string]interface{}{
					"type":        "output_text",
					"text":        fullText,
					"annotations": []interface{}{},
				},
			})
			sendEvent("response.output_item.done", map[string]interface{}{
				"output_index": messageOutputIndex,
				"item": map[string]interface{}{
					"id":     msgID,
					"type":   "message",
					"status": "completed",
					"role":   "assistant",
					"phase":  phase,
					"content": []map[string]interface{}{
						{
							"type":        "output_text",
							"text":        fullText,
							"annotations": []interface{}{},
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
			phase := "final_answer"
			if len(toolCalls) > 0 {
				phase = "commentary"
			}

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
		for i, tc := range toolCalls {
			callID := tc.ID
			if callID == "" {
				callID = fmt.Sprintf("call_%d", i)
			}
			toolKey := responsesToolKey(
				tc.Function.Namespace,
				tc.Function.Name,
			)
			isToolSearch := toolTypes[toolKey] == "tool_search"
			sendEvent("response.output_item.added", map[string]interface{}{
				"output_index": outputIdx,
				"item":         buildResponsesToolCallItem(callID, tc, toolTypes, "in_progress"),
			})
			if !isToolSearch {
				sendEvent("response.function_call_arguments.delta", map[string]interface{}{
					"item_id":      callID,
					"output_index": outputIdx,
					"delta":        tc.Function.Arguments,
				})
				sendEvent("response.function_call_arguments.done", map[string]interface{}{
					"item_id":      callID,
					"output_index": outputIdx,
					"name":         tc.Function.Name,
					"arguments":    tc.Function.Arguments,
				})
			}
			sendEvent("response.output_item.done", map[string]interface{}{
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
					"phase":  "final_answer",
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

	promptStr := fmt.Sprint(messages)
	promptTok := countTokens(promptStr)
	completionTok := countTokens(fullText)
	reasoningText := thinkingText.String()
	reasoningTok := countTokens(reasoningText)

	finalResponse := buildResponsesObject(responseID, openaiModel, fullText, reasoningText, toolCalls, responsesToolTypes(toolPolicy.tools), finishReason, promptTok, completionTok, reasoningTok)
	finalResponse["status"] = status

	sendEvent("response.completed", map[string]any{
		"response": finalResponse,
	})

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	// Cache conversation ID for session continuity
	if sid != "" {
		if shouldResetResponsesSession(fullText, toolCalls, nil) {
			api.ctxCache.Delete("session:" + sid)
		} else if finalConvID != "" {
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
const defaultCompactionPrompt = "I need a concise summary of the following conversation between a user and an assistant. Please cover the main topics discussed, any decisions made, code or files mentioned, and what was being worked on. Keep it brief but preserve all important context. Explicitly preserve tool state: which tools were searched for, loaded, or called; their exact namespace and names; the results of those calls; and the user's current objective and next step. Do not describe transport JSON or protocol details; summarize only the actual work."

func responsesCompactionConversationID(string) string {
	return ""
}

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
		fmt.Fprintf(&conversationText, "%s: %s\n", m.Role, m.Content)
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

	convID := responsesCompactionConversationID(
		api.ctxCache.Get("session:" + sid),
	)

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
func buildCompactionResponseObject(responseID, model, summaryText string, promptTok, completionTok int) map[string]any {
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
		"created_at": time.Now().Unix(),
		"status":     "completed",
		"model":      model,
		"output":     output,
		"usage": map[string]any{
			"input_tokens":  promptTok,
			"output_tokens": completionTok,
			"total_tokens":  promptTok + completionTok,
		},
	}
}

// nonStreamResponsesCompact handles non-streaming compact requests.
func (api *APIServer) nonStreamResponsesCompact(w http.ResponseWriter, messages []payload.Message, cfg models.ModelConfig, sid, convID string, maxTokens int, hasTools bool, tools []toolcalling.ToolDef) {
	respText, _, _, _, _, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)
	if err != nil {
		logging.Errorf("nonStreamResponsesCompact: chat failed: %v", err)
		if sid != "" {
			api.ctxCache.Delete("session:" + sid)
		}
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

	if sid != "" && strings.TrimSpace(respText) != "" {
		api.ctxCache.Delete("session:" + sid)
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

	sendEvent := func(eventType string, data map[string]any) {
		data["type"] = eventType
		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(w, "data: %s\n\n", jsonData)
		flusher.Flush()
	}

	// Send response.created event
	sendEvent("response.created", map[string]any{
		"response": map[string]any{
			"id":     responseID,
			"object": "response",
			"status": "in_progress",
			"model":  openaiModel,
		},
	})

	// Send response.in_progress event
	sendEvent("response.in_progress", map[string]any{
		"response": map[string]any{
			"id":     responseID,
			"object": "response",
			"status": "in_progress",
			"model":  openaiModel,
		},
	})

	ch := api.m365Client.ChatConversationStreamGen(messages, cfg.Tone, cfg.Override, convID, api.config.UserOID, api.config.TenantID, hasTools)

	fullText := ""

	var finalToolCalls []client.ToolCall
	for chunk := range ch {
		if chunk.Error != nil {
			logging.Errorf("streamResponsesCompact: stream error: %v", chunk.Error)
			if sid != "" {
				api.ctxCache.Delete("session:" + sid)
			}
			sendEvent("response.failed", map[string]any{
				"response": map[string]any{
					"id":     responseID,
					"object": "response",
					"status": "failed",
					"model":  openaiModel,
					"error":  map[string]any{"message": chunk.Error.Error()},
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
	promptStr := fmt.Sprint(messages)
	promptTok := countTokens(promptStr)
	completionTok := countTokens(fullText)

	finalResponse := buildCompactionResponseObject(responseID, openaiModel, fullText, promptTok, completionTok)

	sendEvent("response.completed", map[string]any{
		"response": finalResponse,
	})

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	if sid != "" && strings.TrimSpace(fullText) != "" {
		api.ctxCache.Delete("session:" + sid)
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
	cfg := models.LookupModel(modelKey)
	if cfg.OpenAIID == "" {
		api.sendError(w, http.StatusBadRequest, fmt.Sprintf("Unknown model: %s", modelKey))
		return
	}

	messages := []payload.Message{{Role: "user", Content: fullPrompt}}

	// Image generation is a one-shot operation. Reusing a chat conversation can
	// cause M365 to disengage instead of routing the prompt to image generation.
	respText, _, _, _, _, err := api.m365Client.ChatConversation(messages, cfg.Tone, cfg.Override, "", api.config.UserOID, api.config.TenantID, false)
	if err != nil {
		api.sendError(w, http.StatusInternalServerError, fmt.Sprintf("Image generation failed: %v", err))
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

	api.sendJSON(w, http.StatusOK, map[string]any{
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
