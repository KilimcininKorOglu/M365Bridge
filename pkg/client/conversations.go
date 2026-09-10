package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/google/uuid"
)

const (
	conversationAPIURL      = "https://m365.cloud.microsoft/chat"
	conversationResponseMax = 4 << 20
)

// ErrConversationAuthentication indicates that M365 rejected the browser cookies.
var ErrConversationAuthentication = errors.New("M365 conversation authentication failed")

// ConversationClient manages M365 web conversations through the browser-backed API.
type ConversationClient struct {
	tokenManager *auth.TokenManager
	httpClient   *http.Client
	endpoint     string
}

// NewConversationClient creates a conversation management client.
func NewConversationClient(tokenManager *auth.TokenManager) *ConversationClient {
	return &ConversationClient{
		tokenManager: tokenManager,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
		endpoint:     conversationAPIURL,
	}
}

type conversationPageState struct {
	ConversationPageHistoryList struct {
		Chats []map[string]any `json:"chats"`
	} `json:"conversationPageHistoryList"`
}

type conversationListResponse struct {
	Store conversationPageState `json:"store"`
}

// ListConversations returns the conversations visible in the M365 navigation pane.
func (c *ConversationClient) ListConversations(ctx context.Context) ([]map[string]any, error) {
	state, err := c.loadConversationState(ctx)
	if err != nil {
		return nil, err
	}
	if state.ConversationPageHistoryList.Chats == nil {
		return []map[string]any{}, nil
	}
	return state.ConversationPageHistoryList.Chats, nil
}

// RenameConversation changes the display name of an M365 conversation.
func (c *ConversationClient) RenameConversation(ctx context.Context, conversationID, name string) error {
	state, err := c.loadConversationState(ctx)
	if err != nil {
		return err
	}
	return c.request(ctx, map[string]any{
		"action":         "RenameConversation",
		"conversationId": conversationID,
		"newName":        name,
		"traceId":        uuid.NewString(),
		"state":          state,
	}, nil)
}

// DeleteConversation permanently deletes an M365 conversation.
func (c *ConversationClient) DeleteConversation(ctx context.Context, conversationID string) error {
	state, err := c.loadConversationState(ctx)
	if err != nil {
		return err
	}
	return c.request(ctx, map[string]any{
		"action":         "DeleteConversation",
		"conversationId": conversationID,
		"state":          state,
	}, nil)
}

func (c *ConversationClient) loadConversationState(ctx context.Context) (conversationPageState, error) {
	var response conversationListResponse
	body := map[string]any{
		"action":                    "RefreshNavPane",
		"conversationHistoryFilter": nil,
		"skipNotebooks":             false,
		"skipAgentListCache":        true,
	}
	if err := c.request(ctx, body, &response); err != nil {
		return conversationPageState{}, err
	}
	return response.Store, nil
}

func (c *ConversationClient) request(ctx context.Context, payload any, destination any) error {
	req, err := c.buildRequest(ctx, payload)
	if err != nil {
		return err
	}

	response, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("M365 conversation request failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	responseBody, err := readConversationBody(response)
	if err != nil {
		return err
	}
	if destination == nil {
		return conversationErrorField(responseBody)
	}
	if err := json.Unmarshal(responseBody, destination); err != nil {
		return fmt.Errorf("parse M365 conversation response: %w", err)
	}
	return nil
}

// buildRequest stamps the browser headers the M365 chat dispatcher expects,
// including the web app cookies this endpoint authenticates with rather than a
// bearer token.
func (c *ConversationClient) buildRequest(ctx context.Context, payload any) (*http.Request, error) {
	cookieHeader, err := c.tokenManager.M365CookieHeader()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal conversation request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create conversation request: %w", err)
	}
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("Content-Type", "application/json;charset=utf-8")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://m365.cloud.microsoft")
	req.Header.Set("Referer", "https://m365.cloud.microsoft/chat")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	return req, nil
}

// readConversationBody reads the response under a byte cap and refuses a status
// or content type that cannot carry a conversation answer.
func readConversationBody(response *http.Response) ([]byte, error) {
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, conversationResponseMax+1))
	if err != nil {
		return nil, fmt.Errorf("read M365 conversation response: %w", err)
	}
	if len(responseBody) > conversationResponseMax {
		return nil, fmt.Errorf("M365 conversation response exceeds %d bytes", conversationResponseMax)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: status %d", ErrConversationAuthentication, response.StatusCode)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("M365 conversation request returned status %d", response.StatusCode)
	}
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "json") {
		return nil, fmt.Errorf("M365 conversation response is not JSON")
	}
	return responseBody, nil
}

// conversationErrorField reports the error the dispatcher answered with. It
// answers 200 for an action it does not know, so the body is the only place a
// failure appears when the caller wanted no result of its own.
func conversationErrorField(responseBody []byte) error {
	var result map[string]any
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return fmt.Errorf("parse M365 conversation response: %w", err)
	}
	if message, ok := result["error"].(string); ok && message != "" {
		return fmt.Errorf("M365 conversation request failed: %s", message)
	}
	return nil
}
