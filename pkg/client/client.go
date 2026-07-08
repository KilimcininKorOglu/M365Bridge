// Package client provides WebSocket client for M365 Copilot communication.
// It handles SignalR protocol, message parsing, streaming responses, and tool call extraction.
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/payload"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	// signalRDelimiter is the delimiter used in SignalR protocol.
	signalRDelimiter = "\x1e"
	// handshakeMessage is the SignalR handshake message.
	handshakeMessage = `{"protocol":"json","version":1}` + signalRDelimiter
	// defaultHandshakeTimeout is the timeout for WebSocket handshake.
	defaultHandshakeTimeout = 15 * time.Second
	// defaultRecvTimeout is the timeout for receiving messages.
	defaultRecvTimeout = 45 * time.Second
	// defaultRecvFinalTimeout is the timeout for final message in streaming.
	defaultRecvFinalTimeout = 60 * time.Second
)

var (
	// ErrConnectionClosed is returned when the WebSocket connection is closed.
	ErrConnectionClosed = errors.New("connection closed")
	// ErrHandshakeFailed is returned when SignalR handshake fails.
	ErrHandshakeFailed = errors.New("handshake failed")
)

// ToolCall represents a detected tool call from the response.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction represents the function part of a tool call.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// M365Client handles WebSocket communication with M365 Copilot.
// All state is per-request (carried via channel chunks), so the client
// is safe for concurrent use without any mutex.
type M365Client struct {
	tokenManager     *auth.TokenManager
	handshakeTimeout time.Duration
	recvTimeout      time.Duration
	recvFinalTimeout time.Duration
}

// NewM365Client creates a new M365 client instance.
func NewM365Client(tokenManager *auth.TokenManager) *M365Client {
	return &M365Client{
		tokenManager:     tokenManager,
		handshakeTimeout: defaultHandshakeTimeout,
		recvTimeout:      defaultRecvTimeout,
		recvFinalTimeout: defaultRecvFinalTimeout,
	}
}

// Close is a no-op now; per-request connections are closed by dialConnection callers.
func (c *M365Client) Close() error {
	return nil
}

// UploadResult represents the response from the M365 UploadFile endpoint.
type UploadResult struct {
	DocID     string `json:"docId"`
	FileName  string `json:"fileName"`
	FileType  string `json:"fileType"`
	IsSuccess bool
}

// UploadFile uploads an image to M365 Copilot via the UploadFile HTTP endpoint.
// The base64Data should be raw base64 (without data: prefix).
// conversationID is the M365 conversation ID (use a random UUID for new conversations).
// userOID and tenantID are used for the x-anchormailbox header.
func (c *M365Client) UploadFile(base64Data, mediaType, fileName, conversationID, userOID, tenantID string) (*UploadResult, error) {
	logging.Infof("UploadFile: starting upload fileName=%s mediaType=%s convID=%s", fileName, mediaType, conversationID)
	token, err := c.tokenManager.Get()
	if err != nil {
		logging.Errorf("UploadFile: failed to get token: %v", err)
		return nil, fmt.Errorf("failed to get token: %w", err)
	}

	dataURL := fmt.Sprintf("data:%s;base64,%s", mediaType, base64Data)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writer.WriteField("scenario", "UploadImage")
	writer.WriteField("conversationId", conversationID)
	writer.WriteField("FileBase64", dataURL)
	writer.WriteField("optionsSets", "gptvnorm2048")
	writer.Close()

	req, err := http.NewRequest("POST", "https://substrate.office.com/m365Copilot/UploadFile", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "https://m365.cloud.microsoft")
	req.Header.Set("x-scenario", "OfficeWebIncludedCopilot")
	req.Header.Set("x-variants", "feature.EnableImageSupportInUploadFile")
	if userOID != "" && tenantID != "" {
		req.Header.Set("x-anchormailbox", fmt.Sprintf("Oid:%s@%s", userOID, tenantID))
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		logging.Errorf("UploadFile: request failed: %v", err)
		return nil, fmt.Errorf("upload request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logging.Errorf("UploadFile: failed to read response: %v", err)
		return nil, fmt.Errorf("failed to read upload response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		logging.Errorf("UploadFile: upload failed status=%d body=%s", resp.StatusCode, string(respBody)[:min(300, len(respBody))])
		return nil, fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		FileName string `json:"fileName"`
		FileType string `json:"fileType"`
		DocID    string `json:"docId"`
		Result   struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to parse upload response: %w", err)
	}

	return &UploadResult{
		DocID:     result.DocID,
		FileName:  result.FileName,
		FileType:  result.FileType,
		IsSuccess: result.Result.Value == "Success",
	}, nil
}

// dialConnection opens a new WebSocket connection for a single request.
// The caller is responsible for closing the connection when done.
func (c *M365Client) dialConnection(conversationID, userOID, tenantID string) (*websocket.Conn, string, string, error) {
	logging.Debugf("dialConnection: convID=%s", conversationID)
	token, err := c.tokenManager.Get()
	if err != nil {
		logging.Errorf("dialConnection: failed to get token: %v", err)
		return nil, "", "", fmt.Errorf("failed to get token: %w", err)
	}

	hexSID := strings.ReplaceAll(uuid.New().String(), "-", "")
	uuidSID := formatUUID(hexSID)

	url, _, _, err := payload.BuildURL(token, hexSID, conversationID, userOID, tenantID)
	if err != nil {
		return nil, "", "", err
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: c.handshakeTimeout,
	}

	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		logging.Errorf("dialConnection: WebSocket dial failed: %v", err)
		return nil, "", "", fmt.Errorf("failed to dial: %w", err)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(handshakeMessage)); err != nil {
		conn.Close()
		logging.Errorf("dialConnection: handshake write failed: %v", err)
		return nil, "", "", fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}

	conn.SetReadDeadline(time.Now().Add(c.handshakeTimeout))
	_, _, err = conn.ReadMessage()
	if err != nil {
		conn.Close()
		logging.Errorf("dialConnection: handshake read failed: %v", err)
		return nil, "", "", fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	conn.SetReadDeadline(time.Time{})

	logging.Debug("dialConnection: WebSocket connected and handshake OK")
	return conn, hexSID, uuidSID, nil
}

// Chat sends a single message and returns the complete response.
// When hasTools is true, code_interpreter option flags are stripped from the payload.
func (c *M365Client) Chat(text, tone, gptOverride, conversationID, userOID, tenantID string, hasTools bool) (string, error) {
	logging.Infof("Chat: tone=%s override=%s convID=%s hasTools=%v", tone, gptOverride, conversationID, hasTools)
	conn, hexSID, uuidSID, err := c.dialConnection(conversationID, userOID, tenantID)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	payloadStr, err := payload.BuildPayload(hexSID, uuidSID, text, tone, gptOverride, false, hasTools, nil)
	if err != nil {
		return "", err
	}

	return c.sendRecv(conn, payloadStr)
}

// ChatStream sends a message and streams the response to stdout.
// Returns the complete text.
func (c *M365Client) ChatStream(text, tone, gptOverride, conversationID, userOID, tenantID string, hasTools bool) (string, error) {
	fullText := ""

	ch := c.ChatStreamGen(text, tone, gptOverride, conversationID, userOID, tenantID, hasTools)
	for chunk := range ch {
		if chunk.Error != nil {
			return "", chunk.Error
		}
		if !chunk.IsFinal {
			fullText += chunk.Text
		}
	}

	return fullText, nil
}

// StreamChunk represents a chunk of streamed response.
type StreamChunk struct {
	Text           string
	Thinking       string
	IsFinal        bool
	Error          error
	ConversationID string     // set on final chunk
	ToolCalls      []ToolCall // set on final chunk
	FinishReason   string     // set on final chunk
}

// ChatStreamGen generates a stream of response chunks for a single text prompt.
// It delegates to ChatConversationStreamGen with a single-user-message payload.
// When hasTools is true, code_interpreter option flags are stripped from the payload.
func (c *M365Client) ChatStreamGen(text, tone, gptOverride, conversationID, userOID, tenantID string, hasTools bool) <-chan StreamChunk {
	messages := []payload.Message{{Role: "user", Content: text}}
	return c.ChatConversationStreamGen(messages, tone, gptOverride, conversationID, userOID, tenantID, hasTools)
}

// ChatConversation sends a conversation with history and returns the response.
// When hasTools is true, code_interpreter option flags are stripped from the payload.
// Returns (text, thinking, toolCalls, finishReason, conversationID, error).
func (c *M365Client) ChatConversation(messages []payload.Message, tone, gptOverride, conversationID, userOID, tenantID string, hasTools bool) (string, string, []ToolCall, string, string, error) {
	logging.Infof("ChatConversation: tone=%s override=%s convID=%s hasTools=%v msgs=%d", tone, gptOverride, conversationID, hasTools, len(messages))
	ch := c.ChatConversationStreamGen(messages, tone, gptOverride, conversationID, userOID, tenantID, hasTools)

	var fullText, thinking, convID string
	var toolCalls []ToolCall
	var finishReason string

	for chunk := range ch {
		if chunk.Error != nil {
			return "", "", nil, "", "", chunk.Error
		}
		if chunk.IsFinal {
			convID = chunk.ConversationID
			toolCalls = chunk.ToolCalls
			finishReason = chunk.FinishReason
		} else {
			fullText += chunk.Text
			thinking += chunk.Thinking
		}
	}

	return cleanText(fullText), thinking, toolCalls, finishReason, convID, nil
}

// ChatConversationStreamGen generates a stream of conversation response chunks.
// When hasTools is true, code_interpreter option flags are stripped from the payload.
func (c *M365Client) ChatConversationStreamGen(messages []payload.Message, tone, gptOverride, conversationID, userOID, tenantID string, hasTools bool) <-chan StreamChunk {
	logging.Infof("ChatConversationStreamGen: tone=%s override=%s convID=%s hasTools=%v msgs=%d", tone, gptOverride, conversationID, hasTools, len(messages))
	ch := make(chan StreamChunk)

	go func() {
		defer close(ch)

		conn, hexSID, uuidSID, err := c.dialConnection(conversationID, userOID, tenantID)
		if err != nil {
			logging.Errorf("ChatConversationStreamGen: dial failed: %v", err)
			ch <- StreamChunk{Error: err}
			return
		}
		defer conn.Close()

		payloadStr, err := payload.BuildConversationPayload(hexSID, uuidSID, messages, tone, gptOverride, false, hasTools, nil)
		if err != nil {
			logging.Errorf("ChatConversationStreamGen: payload build failed: %v", err)
			ch <- StreamChunk{Error: err}
			return
		}

		if err := conn.WriteMessage(websocket.TextMessage, []byte(payloadStr+signalRDelimiter)); err != nil {
			logging.Errorf("ChatConversationStreamGen: write failed: %v", err)
			ch <- StreamChunk{Error: err}
			return
		}
		logging.Debug("ChatConversationStreamGen: payload sent, waiting for response")

		toolCalls := []ToolCall{}
		seenImages := map[string]bool{}
		accText := ""
		accThinking := ""
		var finalConvID string

		for {
			conn.SetReadDeadline(time.Now().Add(c.recvFinalTimeout))
			msgType, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err) || websocket.IsUnexpectedCloseError(err) {
					logging.Warnf("ChatConversationStreamGen: connection closed: %v", err)
					ch <- StreamChunk{Error: ErrConnectionClosed}
				} else {
					logging.Errorf("ChatConversationStreamGen: read error: %v", err)
					ch <- StreamChunk{Error: err}
				}
				return
			}
			conn.SetReadDeadline(time.Time{})

			if msgType != websocket.TextMessage {
				continue
			}

			text := string(message)
			parts := strings.Split(text, signalRDelimiter)

			for _, part := range parts {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}

				var data map[string]interface{}
				if err := json.Unmarshal([]byte(part), &data); err != nil {
					continue
				}

				// DEBUG: log every WebSocket message type and target (ConvStream)
				if mt, ok := data["type"].(float64); ok {
					target, _ := data["target"].(string)
					logging.Debugf("ConvStream raw: type=%d target=%s", int(mt), target)
				}
				// DEBUG: log type=6 message content
				if mt, ok := data["type"].(float64); ok && int(mt) == 6 {
					j, _ := json.Marshal(data)
					s := string(j)
					if len(s) > 3000 {
						s = s[:3000] + "...(truncated)"
					}
					logging.Debugf("ConvStream type=6: %s", s)
				}
				if msgType, ok := data["type"].(float64); ok && int(msgType) == 1 {
					if target, ok := data["target"].(string); ok && target == "update" {
						if args, ok := data["arguments"].([]interface{}); ok {
							for _, arg := range args {
								if argMap, ok := arg.(map[string]interface{}); ok {
									// DEBUG: log all keys in argMap
									logging.Debugf("ConvStream argMap keys: %v", mapKeys(argMap))
									// Extract conversationId from type:1 update if present (rare)
									if convID, ok := argMap["conversationId"].(string); ok && convID != "" {
										finalConvID = convID
									}
									if msgs, ok := argMap["messages"].([]interface{}); ok {
										// DEBUG: log all messages' messageType and contentOrigin
										for _, msg := range msgs {
											if msgMap, ok := msg.(map[string]interface{}); ok {
												mt, _ := msgMap["messageType"].(string)
												co, _ := msgMap["contentOrigin"].(string)
												logging.Debugf("ConvWS msg: messageType=%s contentOrigin=%s keys=%v", mt, co, mapKeys(msgMap))
											}
										}
										// Check all messages for tool calls and thinking
										for _, msg := range msgs {
											if msgMap, ok := msg.(map[string]interface{}); ok {
												if messageType, ok := msgMap["messageType"].(string); ok {
													if funcName, exists := models.ToolMessageType[messageType]; exists {
														if tc := extractToolCall(msgMap, funcName); tc != nil {
															toolCalls = append(toolCalls, *tc)
														}
													}
													// Extract thinking from Progress + ChainOfThoughtSummary
													if messageType == "Progress" {
														if co, _ := msgMap["contentOrigin"].(string); co == "ChainOfThoughtSummary" {
															if t, _ := msgMap["text"].(string); t != "" {
																accThinking += t
																ch <- StreamChunk{Thinking: t, IsFinal: false}
															}
														}
														// Extract generated image URLs from contentGenerationProgressList
														if co, _ := msgMap["contentOrigin"].(string); co == "ImageGeneration" {
															if imgMD := extractImageGenerationMarkdown(msgMap, seenImages); imgMD != "" {
																accText += imgMD
																ch <- StreamChunk{Text: imgMD, IsFinal: false}
															}
														}
														// Extract web search tool calls from searchQueries field
														if sq, ok := msgMap["searchQueries"].([]interface{}); ok && len(sq) > 0 {
															for _, q := range sq {
																if query, ok := q.(string); ok && query != "" {
																	tc := makeSearchToolCall(query, msgMap)
																	toolCalls = append(toolCalls, *tc)
																}
															}
														}
													}
												}
											}
										}
										// Only process text from the last message (skip Progress messages)
										if len(msgs) > 0 {
											if lastMsg, ok := msgs[len(msgs)-1].(map[string]interface{}); ok {
												if lastMsgType, _ := lastMsg["messageType"].(string); lastMsgType != "Progress" {
													if newText, ok := lastMsg["text"].(string); ok && newText != "" {
														if newText != accText {
															var chunk string
															if strings.HasPrefix(newText, accText) {
																chunk = newText[len(accText):]
															} else {
																chunk = newText
															}
															accText = newText
															if chunk != "" {
																ch <- StreamChunk{Text: chunk, IsFinal: false}
															}
														}
													}
												}
											}
										}
									}
									if writeAtCursor, ok := argMap["writeAtCursor"].(string); ok {
										accText += writeAtCursor
										ch <- StreamChunk{Text: writeAtCursor, IsFinal: false}
									}
								}
							}
						}
					}
				} else if msgType, ok := data["type"].(float64); ok && int(msgType) == 2 {
					// type: 2 is invocation completion; contains item.conversationId
					if item, ok := data["item"].(map[string]interface{}); ok {
						if convID, ok := item["conversationId"].(string); ok && convID != "" {
							finalConvID = convID
						}
					}
				} else if msgType, ok := data["type"].(float64); ok && int(msgType) == 3 {
					finishReason := "stop"
					if len(toolCalls) > 0 {
						finishReason = "tool_calls"
					}
					logging.Infof("ChatConversationStreamGen: completed finishReason=%s toolCalls=%d", finishReason, len(toolCalls))
					ch <- StreamChunk{Text: "", IsFinal: true, ConversationID: finalConvID, ToolCalls: toolCalls, FinishReason: finishReason}
					return
				} else if msgType, ok := data["type"].(float64); ok && int(msgType) == -1 {
					logging.Errorf("ChatConversationStreamGen: server error: %v", data)
					ch <- StreamChunk{Error: fmt.Errorf("server error: %v", data)}
					return
				}
			}
		}
	}()

	return ch
}

// sendRecv sends a payload and waits for the complete response.
func (c *M365Client) sendRecv(conn *websocket.Conn, payload string) (string, error) {
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload+signalRDelimiter)); err != nil {
		logging.Errorf("sendRecv: write failed: %v", err)
		return "", err
	}

	fullText := ""

	for {
		conn.SetReadDeadline(time.Now().Add(c.recvTimeout))
		msgType, message, err := conn.ReadMessage()
		if err != nil {
			logging.Errorf("sendRecv: read error: %v", err)
			return "", err
		}
		conn.SetReadDeadline(time.Time{})

		if msgType != websocket.TextMessage {
			continue
		}

		text := string(message)
		parts := strings.Split(text, signalRDelimiter)

		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}

			var data map[string]interface{}
			if err := json.Unmarshal([]byte(part), &data); err != nil {
				continue
			}

			if msgType, ok := data["type"].(float64); ok && int(msgType) == 1 {
				if target, ok := data["target"].(string); ok && target == "update" {
					if args, ok := data["arguments"].([]interface{}); ok {
						for _, arg := range args {
							if argMap, ok := arg.(map[string]interface{}); ok {
								if msgs, ok := argMap["messages"].([]interface{}); ok && len(msgs) > 0 {
									if lastMsg, ok := msgs[len(msgs)-1].(map[string]interface{}); ok {
										if lastMsgType, _ := lastMsg["messageType"].(string); lastMsgType != "Progress" {
											if text, ok := lastMsg["text"].(string); ok && text != "" && text != fullText {
												if strings.HasPrefix(text, fullText) {
													fullText += text[len(fullText):]
												} else {
													fullText = text
												}
											}
										}
									}
								}
								if writeAtCursor, ok := argMap["writeAtCursor"].(string); ok && writeAtCursor != "" {
									fullText += writeAtCursor
								}
							}
						}
					}
				}
			} else if msgType, ok := data["type"].(float64); ok && int(msgType) == 3 {
				return fullText, nil
			}
		}
	}
}

// extractToolCall extracts a tool call from a message.
func extractToolCall(msg map[string]interface{}, funcName string) *ToolCall {
	messageType, _ := msg["messageType"].(string)
	text, _ := msg["text"].(string)

	if messageType == "" || text == "" {
		return nil
	}

	var args string
	switch messageType {
	case "InternalSearchQuery":
		query := strings.TrimPrefix(text, "search: ")
		argsMap := map[string]string{"query": query}
		argsBytes, _ := json.Marshal(argsMap)
		args = string(argsBytes)
	case "GeneratedCode":
		argsMap := map[string]string{"code": text}
		argsBytes, _ := json.Marshal(argsMap)
		args = string(argsBytes)
	default:
		argsMap := map[string]string{"input": text}
		argsBytes, _ := json.Marshal(argsMap)
		args = string(argsBytes)
	}

	messageID, _ := msg["messageId"].(string)
	if messageID == "" {
		messageID = generateUUID()
	}

	return &ToolCall{
		ID:   messageID,
		Type: "function",
		Function: ToolCallFunction{
			Name:      funcName,
			Arguments: args,
		},
	}
}

// generateUUID generates a random UUID string.
func generateUUID() string {
	return uuid.New().String()
}

// makeSearchToolCall creates a ToolCall for a web search query extracted from
// the searchQueries field of a Progress message.
func makeSearchToolCall(query string, msg map[string]interface{}) *ToolCall {
	argsMap := map[string]string{"query": query}
	argsBytes, _ := json.Marshal(argsMap)

	messageID, _ := msg["messageId"].(string)
	if messageID == "" {
		messageID = generateUUID()
	}

	return &ToolCall{
		ID:   messageID,
		Type: "function",
		Function: ToolCallFunction{
			Name:      "search",
			Arguments: string(argsBytes),
		},
	}
}

// formatUUID converts a UUID string to standard UUID format (8-4-4-4-12).
// Accepts both dashed (36-char) and undashed (32-char) UUID strings.
func formatUUID(s string) string {
	// Strip dashes if present
	hex := strings.ReplaceAll(s, "-", "")
	if len(hex) < 32 {
		return s
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex[0:8], hex[8:12], hex[12:16], hex[16:20], hex[20:32])
}

// cleanText removes non-printable characters from text.
func cleanText(text string) string {
	if text == "" {
		return ""
	}

	// Remove non-printable characters except newline, tab, carriage return
	var result strings.Builder
	for _, r := range text {
		if r == '\n' || r == '\t' || r == '\r' || (r >= 32 && r <= 126) || r > 127 {
			result.WriteRune(r)
		}
	}

	cleaned := result.String()

	// Remove control characters at end
	re := regexp.MustCompile(`[\x00-\x1f\x7f]{1,3}$`)
	cleaned = re.ReplaceAllString(cleaned, "")

	return strings.TrimSpace(cleaned)
}

// extractImageGenerationMarkdown extracts image URLs from a Progress message
// with contentOrigin "ImageGeneration" and returns them as markdown image links.
// seenImages tracks already-emitted URLs to avoid duplicates (M365 sends the
// same URL in multiple Progress updates as the image generation completes).
func extractImageGenerationMarkdown(msg map[string]interface{}, seenImages map[string]bool) string {
	progressList, ok := msg["contentGenerationProgressList"].([]interface{})
	if !ok {
		return ""
	}

	var parts []string
	for _, item := range progressList {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		// DEBUG: log full progress item as JSON (truncated to 2000 chars)
		if j, err := json.Marshal(itemMap); err == nil {
			s := string(j)
			if len(s) > 2000 {
				s = s[:2000] + "...(truncated)"
			}
			logging.Debugf("ImageGen progress item JSON: %s", s)
		}
		urls, ok := itemMap["ImageReferenceUrls"].([]interface{})
		if !ok {
			continue
		}
		for _, urlVal := range urls {
			url, ok := urlVal.(string)
			if !ok || url == "" {
				continue
			}
			if seenImages[url] {
				continue
			}
			seenImages[url] = true
			logging.Infof("ImageGen: extracted image URL: %s", url)
			parts = append(parts, fmt.Sprintf("\n\n![image](%s)\n\n", url))
		}
	}

	return strings.Join(parts, "")
}

// mapKeys returns the keys of a map as a slice (for debug logging).
func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
