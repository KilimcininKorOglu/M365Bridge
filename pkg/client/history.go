package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Reading a conversation that was started somewhere else has exactly one
// working route, and it is not an API. Three that look like APIs do not work:
//
//   - The /chat action dispatcher answers every unknown action with HTTP 200
//     and "(0 , this.executors[t]) is not a function". Its only conversation
//     actions are RefreshNavPane, GetConversationPageHistoryList (both list
//     conversations and carry titles, not turns), RenameConversation and
//     DeleteConversation.
//   - POST substrate.office.com/m365Copilot/ConversationHistory accepts the
//     request shape, then refuses it with ConversationHistoryApiDisabled.
//   - The chat hub replays nothing when dialled with an existing
//     ConversationId.
//
// What works is the conversation page. The M365 web client is server-rendered
// through React Router, and the router serializes its loader data into the
// document as window.__staticRouterHydrationData. The conversation's turns are
// in there, under a route entry's store.rawConversationResponse.messages, in
// the same shape the WebSocket delivers them. The browser then renders the DOM
// from that blob, which is why the served markup carries no message elements
// and only the hydrated DOM appears to.
//
// This reads the blob rather than the markup, so it depends on field names the
// rest of this package already knows (text, author, messageType) instead of on
// CSS classes or test ids.

const (
	// conversationPageURL is the page whose loader data carries the turns.
	conversationPageURL = "https://m365.cloud.microsoft/chat/conversation/"
	// conversationPageMax bounds the download. A measured page was 416 KB.
	conversationPageMax = 16 << 20
	// hydrationMarker introduces the router's serialized loader data. What
	// follows is a JavaScript string literal holding JSON, so the payload is
	// encoded twice.
	hydrationMarker = "window.__staticRouterHydrationData = JSON.parse("
	// conversationIDMax bounds the id accepted into the URL path.
	conversationIDMax = 128
)

// ErrHistoryUnavailable reports that the conversation page carried no turns
// this build could read.
var ErrHistoryUnavailable = errors.New("M365 conversation history could not be read")

// HistoryMessage is one stored turn recovered from a conversation page.
type HistoryMessage struct {
	Role      string // "user" or "assistant"
	Text      string
	MessageID string
	CreatedAt string // RFC 3339 as the backend wrote it, empty when absent
}

// FetchHistory returns the turns of a conversation as M365 stored them.
//
// It is deliberately not called on its own. The page is large and the read
// depends on a serialization this project does not control, so a caller asks
// for it only when a user wants the history of a conversation this gateway
// never carried.
func (c *ConversationClient) FetchHistory(ctx context.Context, conversationID string) ([]HistoryMessage, error) {
	if err := validateConversationID(conversationID); err != nil {
		return nil, err
	}
	cookieHeader, err := c.tokenManager.M365CookieHeader()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, conversationPageURL+conversationID, nil)
	if err != nil {
		return nil, fmt.Errorf("create conversation page request: %w", err)
	}
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Referer", "https://m365.cloud.microsoft/chat")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("M365 conversation page request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	page, err := readConversationPage(resp)
	if err != nil {
		return nil, err
	}
	return parseConversationPage(page)
}

// readConversationPage reads the rendered page under a byte cap, and refuses a
// status that carries no conversation.
func readConversationPage(resp *http.Response) (string, error) {
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("%w: status %d", ErrConversationAuthentication, resp.StatusCode)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("M365 conversation page returned status %d", resp.StatusCode)
	}

	page, err := io.ReadAll(io.LimitReader(resp.Body, conversationPageMax+1))
	if err != nil {
		return "", fmt.Errorf("read M365 conversation page: %w", err)
	}
	if len(page) > conversationPageMax {
		return "", fmt.Errorf("M365 conversation page exceeds %d bytes", conversationPageMax)
	}
	return string(page), nil
}

// validateConversationID rejects anything that could leave the intended path.
func validateConversationID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: no conversation id", ErrHistoryUnavailable)
	}
	if len(id) > conversationIDMax {
		return fmt.Errorf("%w: conversation id is too long", ErrHistoryUnavailable)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf("%w: conversation id carries %q", ErrHistoryUnavailable, r)
		}
	}
	return nil
}

// parseConversationPage turns a rendered conversation page into its turns.
func parseConversationPage(page string) ([]HistoryMessage, error) {
	blob, err := extractHydrationData(page)
	if err != nil {
		return nil, err
	}

	var hydration struct {
		LoaderData map[string]json.RawMessage `json:"loaderData"`
	}
	if err := json.Unmarshal([]byte(blob), &hydration); err != nil {
		return nil, fmt.Errorf("%w: the loader data did not parse: %v", ErrHistoryUnavailable, err)
	}

	// The route id that owns the conversation is not fixed, so every loader
	// entry is offered the shape and the one that fits wins. Entries that do
	// not carry a conversation simply fail to decode.
	for _, entry := range hydration.LoaderData {
		var route struct {
			Store struct {
				RawConversationResponse *struct {
					Messages []map[string]any `json:"messages"`
				} `json:"rawConversationResponse"`
			} `json:"store"`
		}
		if err := json.Unmarshal(entry, &route); err != nil {
			continue
		}
		if route.Store.RawConversationResponse == nil {
			continue
		}
		messages := collectHistory(route.Store.RawConversationResponse.Messages)
		if len(messages) == 0 {
			continue
		}
		return messages, nil
	}
	// The page loaded and carried no turn. Either the conversation really is
	// empty or the serialization moved, and a caller cannot tell those apart,
	// so both are reported rather than answered with an empty conversation.
	return nil, fmt.Errorf("%w: the page carried no turn this build recognises", ErrHistoryUnavailable)
}

// extractHydrationData returns the JSON the router serialized into the page.
//
// The payload is a JavaScript string literal handed to JSON.parse, so it is
// JSON-encoded twice and the outer layer is decoded here.
func extractHydrationData(page string) (string, error) {
	_, rest, found := strings.Cut(page, hydrationMarker)
	if !found {
		return "", fmt.Errorf("%w: the page carried no router hydration data", ErrHistoryUnavailable)
	}
	if !strings.HasPrefix(rest, `"`) {
		return "", fmt.Errorf("%w: the hydration data is not a string literal", ErrHistoryUnavailable)
	}

	// Scan to the closing quote, honouring backslash escapes, so an escaped
	// quote inside the payload does not end it early.
	end := -1
	for i := 1; i < len(rest); i++ {
		switch rest[i] {
		case '\\':
			i++
		case '"':
			end = i
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return "", fmt.Errorf("%w: the hydration data never closed", ErrHistoryUnavailable)
	}

	var decoded string
	if err := json.Unmarshal([]byte(rest[:end+1]), &decoded); err != nil {
		return "", fmt.Errorf("%w: the hydration string did not decode: %v", ErrHistoryUnavailable, err)
	}
	return decoded, nil
}

// collectHistory turns stored messages into turns, in the order they were said.
func collectHistory(messages []map[string]any) []HistoryMessage {
	out := make([]HistoryMessage, 0, len(messages))
	for _, msg := range messages {
		// The same rule the live stream applies: M365 interleaves its own
		// progress notes and tool traffic into this array, and none of it is
		// answer text.
		if !carriesAnswerText(msg) {
			continue
		}
		text, _ := msg["text"].(string)
		text = strings.TrimSpace(stripCitations(text))
		if text == "" {
			continue
		}

		var role string
		switch author, _ := msg["author"].(string); author {
		case "user":
			role = "user"
		case "bot":
			role = "assistant"
		default:
			continue
		}

		messageID, _ := msg["messageId"].(string)
		createdAt, _ := msg["createdAt"].(string)
		out = append(out, HistoryMessage{
			Role:      role,
			Text:      text,
			MessageID: messageID,
			CreatedAt: createdAt,
		})
	}
	return out
}
