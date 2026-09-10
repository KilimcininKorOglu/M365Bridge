// Package client provides WebSocket client for M365 Copilot communication.
// It handles SignalR protocol, message parsing, streaming responses, and tool call extraction.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/payload"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/textcut"
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
	// defaultImageRecvTimeout is the read timeout that applies once image
	// generation has started. M365 answers a picture prompt by going quiet for
	// a minute or more, and it normally holds the connection open with SignalR
	// type 6 pings about every fifteen seconds. This is the bound for a turn
	// where that ping flow stops too.
	defaultImageRecvTimeout = 180 * time.Second
	// progressMessageType marks a status message rather than answer text.
	progressMessageType = "Progress"
	// NoticeImageGenerating tells the caller M365 started generating a picture
	// and the turn will stay silent until it finishes.
	NoticeImageGenerating = "image_generating"
	// uploadResponseMax caps an upload response. The body is a small JSON
	// object naming the stored file, so a body near this size is a redirected
	// or hostile endpoint rather than an upload result.
	uploadResponseMax = 1 << 20
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
	Namespace string `json:"namespace,omitempty"`
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
	imageRecvTimeout time.Duration
	// throttlingObserver is configuration set once before serving, not
	// per-request state. It reports quota counters from both the streaming and
	// the aggregating paths, which discard the final chunk.
	throttlingObserver func(*ThrottlingInfo)
	// webSearchEnabled is configuration set once before serving, not
	// per-request state. It declares the BingWebSearch built-in on outgoing
	// payloads.
	webSearchEnabled bool
}

// SetWebSearchEnabled declares or withholds the BingWebSearch built-in on every
// request. Call it before serving requests.
func (c *M365Client) SetWebSearchEnabled(enabled bool) {
	c.webSearchEnabled = enabled
}

// SetThrottlingObserver registers a callback invoked whenever the backend
// reports conversation quota counters. Call it before serving requests; the
// callback runs on the WebSocket read goroutine and must be safe for
// concurrent use.
func (c *M365Client) SetThrottlingObserver(observer func(*ThrottlingInfo)) {
	c.throttlingObserver = observer
}

// NewM365Client creates a new M365 client instance.
func NewM365Client(tokenManager *auth.TokenManager) *M365Client {
	return &M365Client{
		tokenManager:     tokenManager,
		webSearchEnabled: true,
		handshakeTimeout: defaultHandshakeTimeout,
		recvTimeout:      defaultRecvTimeout,
		recvFinalTimeout: defaultRecvFinalTimeout,
		imageRecvTimeout: defaultImageRecvTimeout,
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
	_ = writer.WriteField("scenario", "UploadImage")
	_ = writer.WriteField("conversationId", conversationID)
	_ = writer.WriteField("FileBase64", dataURL)
	_ = writer.WriteField("optionsSets", "gptvnorm2048")
	_ = writer.Close()

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
	defer func() { _ = resp.Body.Close() }()

	// One extra byte distinguishes "exactly at the limit" from "truncated".
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, uploadResponseMax+1))
	if err != nil {
		logging.Errorf("UploadFile: failed to read response: %v", err)
		return nil, fmt.Errorf("failed to read upload response: %w", err)
	}
	if len(respBody) > uploadResponseMax {
		return nil, fmt.Errorf("upload response exceeds %d bytes", uploadResponseMax)
	}

	if resp.StatusCode != http.StatusOK {
		logging.Errorf("UploadFile: upload failed status=%d body=%s", resp.StatusCode, string(respBody)[:min(300, len(respBody))])
		return nil, &UpstreamError{
			Op:     "upload",
			Status: resp.StatusCode,
			Err:    errors.New(string(respBody)),
		}
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

	// The dial response carries the status the backend refused with. Discarding
	// it would leave an expired token, a throttled account and a backend outage
	// indistinguishable at the HTTP layer.
	conn, dialResp, err := dialer.Dial(url, nil)
	if err != nil {
		status := 0
		if dialResp != nil {
			status = dialResp.StatusCode
			_ = dialResp.Body.Close()
		}
		logging.Errorf("dialConnection: WebSocket dial failed: status=%d err=%v", status, err)
		return nil, "", "", &UpstreamError{Op: "dial", Status: status, Err: err}
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(handshakeMessage)); err != nil {
		_ = conn.Close()
		logging.Errorf("dialConnection: handshake write failed: %v", err)
		return nil, "", "", fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(c.handshakeTimeout))
	_, _, err = conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		logging.Errorf("dialConnection: handshake read failed: %v", err)
		return nil, "", "", fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	_ = conn.SetReadDeadline(time.Time{})

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
	defer func() { _ = conn.Close() }()

	payloadStr, err := payload.BuildPayload(hexSID, uuidSID, text, tone, gptOverride, false, hasTools, c.webSearchEnabled, nil)
	if err != nil {
		return "", err
	}

	return c.sendRecv(conn, payloadStr)
}

// ChatStream sends a message and streams the response to stdout.
// Returns the complete text.
func (c *M365Client) ChatStream(text, tone, gptOverride, conversationID, userOID, tenantID string, hasTools bool) (string, error) {
	var fullText strings.Builder

	ch := c.ChatStreamGen(text, tone, gptOverride, conversationID, userOID, tenantID, hasTools)
	for chunk := range ch {
		if chunk.Error != nil {
			return "", chunk.Error
		}
		if !chunk.IsFinal {
			fullText.WriteString(chunk.Text)
		}
	}

	return fullText.String(), nil
}

// StreamChunk represents a chunk of streamed response.
type StreamChunk struct {
	Text     string
	Thinking string
	IsFinal  bool
	Error    error
	// Notice names a state of the turn the caller may show while it waits. It
	// is a machine token, never text to display, and it is not part of the
	// answer: nothing that carries it also carries Text, and it never reaches
	// a transcript.
	Notice         string
	ConversationID string          // set on final chunk
	ToolCalls      []ToolCall      // set on final chunk
	FinishReason   string          // set on final chunk
	Throttling     *ThrottlingInfo // latest quota counters, when the backend sent them
}

// ThrottlingInfo carries M365's per-conversation quota counters. The backend
// sends them in a `throttling` object on type 1 update frames. Counters are
// pointers because a frame may carry only some of them, and zero is a
// meaningful value that must not be confused with absent.
type ThrottlingInfo struct {
	// NumUserMessages is the user message count consumed in this conversation.
	NumUserMessages *int
	// MaxNumUserMessages is the ceiling M365 enforces per conversation.
	MaxNumUserMessages *int
	// Extra holds every other key the backend sent, so a counter this build
	// does not know about is still observable instead of silently dropped.
	Extra map[string]any
}

// Exhausted reports whether the conversation reached its message ceiling.
func (t *ThrottlingInfo) Exhausted() bool {
	if t == nil || t.NumUserMessages == nil || t.MaxNumUserMessages == nil {
		return false
	}
	return *t.MaxNumUserMessages > 0 && *t.NumUserMessages >= *t.MaxNumUserMessages
}

// Summary renders the counters as a compact log string.
func (t *ThrottlingInfo) Summary() string {
	if t == nil {
		return ""
	}
	parts := make([]string, 0, 3+len(t.Extra))
	if t.NumUserMessages != nil {
		parts = append(parts, fmt.Sprintf("used=%d", *t.NumUserMessages))
	}
	if t.MaxNumUserMessages != nil {
		parts = append(parts, fmt.Sprintf("max=%d", *t.MaxNumUserMessages))
	}
	if t.NumUserMessages != nil && t.MaxNumUserMessages != nil {
		parts = append(parts, fmt.Sprintf("headroom=%d", *t.MaxNumUserMessages-*t.NumUserMessages))
	}
	for _, key := range slices.Sorted(maps.Keys(t.Extra)) {
		parts = append(parts, fmt.Sprintf("%s=%v", key, t.Extra[key]))
	}
	return strings.Join(parts, " ")
}

// parseThrottling converts the backend's `throttling` object into
// ThrottlingInfo. Keys beyond the two documented counters are preserved in
// Extra rather than dropped by a hardcoded key list.
func parseThrottling(raw map[string]any) *ThrottlingInfo {
	if len(raw) == 0 {
		return nil
	}
	info := &ThrottlingInfo{}
	for key, value := range raw {
		switch key {
		case "numUserMessagesInConversation":
			if n, ok := jsonInt(value); ok {
				info.NumUserMessages = &n
				continue
			}
		case "maxNumUserMessagesInConversation":
			if n, ok := jsonInt(value); ok {
				info.MaxNumUserMessages = &n
				continue
			}
		}
		if info.Extra == nil {
			info.Extra = make(map[string]any)
		}
		info.Extra[key] = value
	}
	return info
}

// jsonInt converts a JSON number to int, rejecting bools and non-numbers.
func jsonInt(value any) (int, bool) {
	f, ok := value.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
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
	return c.ChatConversationContext(
		context.Background(),
		messages,
		tone,
		gptOverride,
		conversationID,
		userOID,
		tenantID,
		hasTools,
	)
}

// ChatConversationContext sends a conversation and stops waiting when ctx is
// canceled.
func (c *M365Client) ChatConversationContext(
	ctx context.Context,
	messages []payload.Message,
	tone, gptOverride, conversationID, userOID, tenantID string,
	hasTools bool,
) (string, string, []ToolCall, string, string, error) {
	logging.Infof("ChatConversation: tone=%s override=%s convID=%s hasTools=%v msgs=%d", tone, gptOverride, conversationID, hasTools, len(messages))
	ch := c.ChatConversationStreamGenContext(
		ctx,
		messages,
		tone,
		gptOverride,
		conversationID,
		userOID,
		tenantID,
		hasTools,
	)

	var convID string
	var toolCalls []ToolCall
	var finishReason string
	// A long answer arrives as hundreds of chunks. String concatenation would
	// copy the whole text on every one of them.
	var fullText, thinking strings.Builder

	for chunk := range ch {
		if chunk.Error != nil {
			return "", "", nil, "", "", chunk.Error
		}
		if chunk.IsFinal {
			convID = chunk.ConversationID
			toolCalls = chunk.ToolCalls
			finishReason = chunk.FinishReason
		} else {
			fullText.WriteString(chunk.Text)
			thinking.WriteString(chunk.Thinking)
		}
	}

	if err := ctx.Err(); err != nil {
		return "", "", nil, "", "", err
	}
	return cleanText(fullText.String()), thinking.String(), toolCalls, finishReason, convID, nil
}

// ChatConversationStreamGen generates a stream of conversation response chunks.
// When hasTools is true, code_interpreter option flags are stripped from the payload.
func (c *M365Client) ChatConversationStreamGen(messages []payload.Message, tone, gptOverride, conversationID, userOID, tenantID string, hasTools bool) <-chan StreamChunk {
	return c.ChatConversationStreamGenContext(
		context.Background(),
		messages,
		tone,
		gptOverride,
		conversationID,
		userOID,
		tenantID,
		hasTools,
	)
}

// ChatConversationStreamGenContext generates a stream that stops when ctx is
// canceled. This lets HTTP handlers release the upstream WebSocket as soon as
// their client disconnects or a proxy timeout expires.
func (c *M365Client) ChatConversationStreamGenContext(
	ctx context.Context,
	messages []payload.Message,
	tone, gptOverride, conversationID, userOID, tenantID string,
	hasTools bool,
) <-chan StreamChunk {
	logging.Infof("ChatConversationStreamGen: tone=%s override=%s convID=%s hasTools=%v msgs=%d", tone, gptOverride, conversationID, hasTools, len(messages))
	ch := make(chan StreamChunk)

	// The goroutine runs on the request context this function was given; the
	// nil branch below is only a guard for a caller that passes none, and every
	// caller in this repository passes one.
	// #nosec G118
	go func() {
		defer close(ch)
		if ctx == nil {
			ctx = context.Background()
		}
		if ctx.Err() != nil {
			return
		}
		emit := func(chunk StreamChunk) bool {
			select {
			case ch <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

		c.runConversationStream(ctx, emit, messages, tone, gptOverride, conversationID, userOID, tenantID, hasTools)
	}()

	return ch
}

// runConversationStream dials one WebSocket, sends the turn and reads its
// frames until the turn ends or the caller goes away.
func (c *M365Client) runConversationStream(
	ctx context.Context,
	emit func(StreamChunk) bool,
	messages []payload.Message,
	tone, gptOverride, conversationID, userOID, tenantID string,
	hasTools bool,
) {
	conn, hexSID, uuidSID, err := c.dialConnection(conversationID, userOID, tenantID)
	if err != nil {
		logging.Errorf("ChatConversationStreamGen: dial failed: %v", err)
		if ctx.Err() == nil {
			emit(StreamChunk{Error: err})
		}
		return
	}
	defer func() { _ = conn.Close() }()
	contextWatchDone := make(chan struct{})
	defer close(contextWatchDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-contextWatchDone:
		}
	}()

	payloadStr, err := payload.BuildConversationPayload(
		hexSID,
		uuidSID,
		messages,
		conversationID == "",
		tone,
		gptOverride,
		false,
		hasTools,
		c.webSearchEnabled,
		nil,
	)
	if err != nil {
		logging.Errorf("ChatConversationStreamGen: payload build failed: %v", err)
		emit(StreamChunk{Error: err})
		return
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(payloadStr+signalRDelimiter)); err != nil {
		logging.Errorf("ChatConversationStreamGen: write failed: %v", err)
		emit(StreamChunk{Error: err})
		return
	}
	logging.Debug("ChatConversationStreamGen: payload sent, waiting for response")

	stream := &conversationStream{
		client:     c,
		emit:       emit,
		toolCalls:  []ToolCall{},
		seenImages: map[string]bool{},
	}
	stream.read(ctx, conn)
}

// streamStep says whether a streamed turn keeps reading.
type streamStep int

const (
	// streamContinue keeps reading frames.
	streamContinue streamStep = iota
	// streamStop ends the turn. Everything the caller needed was already
	// emitted, including an error chunk when there was one.
	streamStop
)

// conversationStream holds what one streamed turn has accumulated.
type conversationStream struct {
	client *M365Client
	emit   func(StreamChunk) bool

	acc      answerAccumulator
	thinking strings.Builder
	// citations holds back a citation run that has not finished arriving,
	// because a delta already emitted cannot be retracted.
	citations   citationFilter
	toolCalls   []ToolCall
	seenImages  map[string]bool
	finalConvID string
	throttling  *ThrottlingInfo
}

// read consumes frames until the turn ends, the caller goes away, or the
// connection fails.
func (s *conversationStream) read(ctx context.Context, conn *websocket.Conn) {
	for {
		// Image generation moves the read deadline out for the rest of the
		// turn. The backend goes quiet for a minute or more while it draws,
		// and its own pings normally carry the connection through that; this
		// is what keeps the turn alive when they stop.
		readTimeout := s.client.recvFinalTimeout
		if s.acc.noticedImage {
			readTimeout = s.client.imageRecvTimeout
		}
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		msgType, message, err := conn.ReadMessage()
		if err != nil {
			s.reportReadError(ctx, err)
			return
		}
		_ = conn.SetReadDeadline(time.Time{})

		if msgType != websocket.TextMessage {
			continue
		}

		for part := range strings.SplitSeq(string(message), signalRDelimiter) {
			if s.frame(part) == streamStop {
				return
			}
		}
	}
}

// reportReadError tells the caller why the turn stopped, unless the caller is
// the reason it stopped.
func (s *conversationStream) reportReadError(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	if websocket.IsCloseError(err) || websocket.IsUnexpectedCloseError(err) {
		logging.Warnf("ChatConversationStreamGen: connection closed: %v", err)
		s.emit(StreamChunk{Error: ErrConnectionClosed})
		return
	}
	logging.Errorf("ChatConversationStreamGen: read error: %v", err)
	s.emit(StreamChunk{Error: err})
}

// frame reads one SignalR frame.
func (s *conversationStream) frame(part string) streamStep {
	data, ok := decodeSignalRFrame(part)
	if !ok {
		return streamContinue
	}
	logStreamFrame(data)

	switch signalRFrameType(data) {
	case 1:
		return s.update(data)
	case 2:
		return s.completion(data)
	case 3:
		return s.finish()
	case -1:
		logging.Errorf("ChatConversationStreamGen: server error: %v", data)
		s.emit(StreamChunk{Error: fmt.Errorf("server error: %v", data)})
		return streamStop
	}
	return streamContinue
}

// logStreamFrame records every frame's type and target, and dumps a type 6
// frame in full because that is the one whose shape is undocumented.
func logStreamFrame(data map[string]any) {
	mt, ok := data["type"].(float64)
	if !ok {
		return
	}
	target, _ := data["target"].(string)
	logging.Debugf("ConvStream raw: type=%d target=%s", int(mt), target)
	if int(mt) != 6 {
		return
	}
	j, _ := json.Marshal(data)
	dump := string(j)
	if len(dump) > 3000 {
		dump = textcut.Truncate(dump, 3000) + "...(truncated)"
	}
	logging.Debugf("ConvStream type=6: %s", dump)
}

// update reads a type 1 frame, which carries the turn's incremental content.
func (s *conversationStream) update(data map[string]any) streamStep {
	if target, ok := data["target"].(string); !ok || target != "update" {
		return streamContinue
	}
	args, ok := data["arguments"].([]any)
	if !ok {
		return streamContinue
	}
	for _, arg := range args {
		argMap, ok := arg.(map[string]any)
		if !ok {
			continue
		}
		if s.updateArg(argMap) == streamStop {
			return streamStop
		}
	}
	return streamContinue
}

// updateArg reads one argument of an update frame.
func (s *conversationStream) updateArg(argMap map[string]any) streamStep {
	// DEBUG: log all keys in argMap
	logging.Debugf("ConvStream argMap keys: %v", mapKeys(argMap))
	s.applyThrottling(argMap)

	// Extract conversationId from type:1 update if present (rare)
	if convID, ok := argMap["conversationId"].(string); ok && convID != "" {
		s.finalConvID = convID
	}
	if msgs, ok := argMap["messages"].([]any); ok {
		if s.applyMessages(msgs) == streamStop {
			return streamStop
		}
	}
	return s.applyCursor(argMap)
}

// applyThrottling captures the conversation quota counters. They arrive on
// their own update frames, separate from message frames.
func (s *conversationStream) applyThrottling(argMap map[string]any) {
	rawThrottling, ok := argMap["throttling"].(map[string]any)
	if !ok {
		return
	}
	info := parseThrottling(rawThrottling)
	if info == nil {
		return
	}
	s.throttling = info
	logging.Infof("ConvStream throttling: %s", info.Summary())
	if s.client.throttlingObserver != nil {
		s.client.throttlingObserver(info)
	}
}

// applyMessages reads the messages of one update argument.
func (s *conversationStream) applyMessages(msgs []any) streamStep {
	logMessageTypes(msgs)

	// Check all messages for tool calls and thinking
	for _, msg := range msgs {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if s.applyMessage(msgMap) == streamStop {
			return streamStop
		}
	}
	return s.applyAnswerSnapshot(msgs)
}

// logMessageTypes records what the backend sent, which is how a message type
// this client does not handle yet gets noticed.
func logMessageTypes(msgs []any) {
	for _, msg := range msgs {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		mt, _ := msgMap["messageType"].(string)
		co, _ := msgMap["contentOrigin"].(string)
		logging.Debugf("ConvWS msg: messageType=%s contentOrigin=%s keys=%v", mt, co, mapKeys(msgMap))
	}
}

// applyMessage reads one message: the backend's own tool calls, and the
// progress notes that carry reasoning and generated images.
func (s *conversationStream) applyMessage(msgMap map[string]any) streamStep {
	messageType, ok := msgMap["messageType"].(string)
	if !ok {
		return streamContinue
	}
	if funcName, exists := models.ToolMessageType[messageType]; exists {
		if tc := extractToolCall(msgMap, funcName); tc != nil {
			s.toolCalls = append(s.toolCalls, *tc)
		}
	}
	if messageType != progressMessageType {
		return streamContinue
	}
	return s.applyProgress(msgMap)
}

// applyProgress reads a Progress message, which carries the reasoning summary,
// the generated image and the backend's own web searches.
func (s *conversationStream) applyProgress(msgMap map[string]any) streamStep {
	co, _ := msgMap["contentOrigin"].(string)
	switch co {
	case "ChainOfThoughtSummary":
		// Extract thinking from Progress + ChainOfThoughtSummary
		if s.applyThinking(msgMap) == streamStop {
			return streamStop
		}
	case "ImageGeneration":
		// Extract generated image URLs from contentGenerationProgressList
		if !s.acc.emitGeneratedImage(msgMap, s.seenImages, s.emit) {
			return streamStop
		}
	}
	s.applySearchQueries(msgMap)
	return streamContinue
}

// applyThinking emits the reasoning summary as it arrives.
func (s *conversationStream) applyThinking(msgMap map[string]any) streamStep {
	t, _ := msgMap["text"].(string)
	if t == "" {
		return streamContinue
	}
	s.thinking.WriteString(t)
	if !s.emit(StreamChunk{Thinking: t, IsFinal: false}) {
		return streamStop
	}
	return streamContinue
}

// applySearchQueries records the backend's own web searches as tool calls.
func (s *conversationStream) applySearchQueries(msgMap map[string]any) {
	sq, ok := msgMap["searchQueries"].([]any)
	if !ok || len(sq) == 0 {
		return
	}
	for _, q := range sq {
		if query, ok := q.(string); ok && query != "" {
			s.toolCalls = append(s.toolCalls, *makeSearchToolCall(query, msgMap))
		}
	}
}

// applyAnswerSnapshot reads the answer restatement the last message carries.
//
// Only the last message is read, and only when it is the answer rather than
// the backend's own tool traffic.
func (s *conversationStream) applyAnswerSnapshot(msgs []any) streamStep {
	newText, ok := lastAnswerText(msgs)
	if !ok || newText == "" {
		return streamContinue
	}

	// The snapshot restates the whole answer, so it is stripped whole rather
	// than through the streaming filter, and stays comparable with the
	// accumulation that was already filtered.
	newText = stripCitations(newText)
	chunk, advanced := snapshotDelta(s.acc.baseline(), newText)
	if !advanced {
		// A diverging snapshot is normally a re-encoding of text already
		// delivered, which is why it is dropped. Count it so a turn that really
		// did lose content is not invisible: two drops on an ordinary turn is
		// the measured norm, and a spike is not.
		s.acc.dropSnapshot()
		return streamContinue
	}

	s.acc.replaceAnswer(newText)
	if chunk != "" && !s.emit(StreamChunk{Text: chunk, IsFinal: false}) {
		return streamStop
	}
	return streamContinue
}

// applyCursor emits the incremental answer text. A citation run can straddle
// two deltas, so the filter holds the tail back rather than emitting text it
// would have to retract.
func (s *conversationStream) applyCursor(argMap map[string]any) streamStep {
	writeAtCursor, ok := argMap["writeAtCursor"].(string)
	if !ok {
		return streamContinue
	}
	emitText := s.citations.push(writeAtCursor)
	if emitText == "" {
		return streamContinue
	}
	s.acc.appendAnswer(emitText)
	if !s.emit(StreamChunk{Text: emitText, IsFinal: false}) {
		return streamStop
	}
	return streamContinue
}

// completion reads a type 2 frame, the invocation completion. It carries
// item.conversationId and the backend's verdict on the turn.
func (s *conversationStream) completion(data map[string]any) streamStep {
	item, ok := data["item"].(map[string]any)
	if !ok {
		return streamContinue
	}
	if convID, ok := item["conversationId"].(string); ok && convID != "" {
		s.finalConvID = convID
	}
	failure := parseTurnResult(item)
	if failure == nil {
		return streamContinue
	}

	// Text already on the wire cannot be retracted, so a partial answer is
	// delivered rather than replaced by an error.
	if s.acc.emittedBytes() > 0 {
		logging.Warnf("ChatConversationStreamGen: %v, keeping the %d bytes already emitted", failure, s.acc.emittedBytes())
		return streamContinue
	}
	logging.Errorf("ChatConversationStreamGen: %v", failure)
	s.emit(StreamChunk{Error: failure})
	return streamStop
}

// finish reads a type 3 frame, which ends the turn.
func (s *conversationStream) finish() streamStep {
	// A run still held here never closed, so it was a truncated citation
	// marker rather than answer text. Report the drop instead of letting it
	// vanish silently.
	if held := s.citations.flush(); held != "" {
		logging.Warnf("ChatConversationStreamGen: dropped %d bytes of an unterminated citation marker", len(held))
	}
	if s.acc.emptyTurn(s.toolCalls) {
		logging.Errorf("ChatConversationStreamGen: %v (thinking=%d bytes)", ErrEmptyTurn, s.thinking.Len())
		s.emit(StreamChunk{Error: ErrEmptyTurn})
		return streamStop
	}

	finishReason := "stop"
	if len(s.toolCalls) > 0 {
		finishReason = "tool_calls"
	}
	logging.Infof("ChatConversationStreamGen: completed finishReason=%s toolCalls=%d bytes=%d droppedSnapshots=%d",
		finishReason, len(s.toolCalls), s.acc.emittedBytes(), s.acc.droppedSnapshots)
	s.emit(StreamChunk{Text: "", IsFinal: true, ConversationID: s.finalConvID, ToolCalls: s.toolCalls, FinishReason: finishReason, Throttling: s.throttling})
	return streamStop
}

// sendRecv sends a payload and waits for the complete response.
func (c *M365Client) sendRecv(conn *websocket.Conn, payload string) (string, error) {
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload+signalRDelimiter)); err != nil {
		logging.Errorf("sendRecv: write failed: %v", err)
		return "", err
	}

	var state sendRecvState
	for {
		_ = conn.SetReadDeadline(time.Now().Add(c.recvTimeout))
		msgType, message, err := conn.ReadMessage()
		if err != nil {
			logging.Errorf("sendRecv: read error: %v", err)
			return "", err
		}
		_ = conn.SetReadDeadline(time.Time{})

		if msgType != websocket.TextMessage {
			continue
		}

		for part := range strings.SplitSeq(string(message), signalRDelimiter) {
			step, err := state.frame(part)
			if err != nil {
				return "", err
			}
			if step == frameDone {
				return state.fullText, nil
			}
		}
	}
}

// frameStep says what one SignalR frame means to a non-streaming turn.
type frameStep int

const (
	// frameContinue keeps reading.
	frameContinue frameStep = iota
	// frameDone means the turn ended and the text is complete.
	frameDone
)

// sendRecvState holds the answer of a non-streaming turn.
type sendRecvState struct {
	fullText string
}

// frame reads one SignalR frame.
func (s *sendRecvState) frame(part string) (frameStep, error) {
	data, ok := decodeSignalRFrame(part)
	if !ok {
		return frameContinue, nil
	}
	switch signalRFrameType(data) {
	case 1:
		s.applyUpdate(data)
	case 2:
		return frameContinue, s.turnResult(data)
	case 3:
		return frameDone, s.completion()
	}
	return frameContinue, nil
}

// applyUpdate adopts the answer snapshot an update frame carried.
func (s *sendRecvState) applyUpdate(data map[string]any) {
	if target, ok := data["target"].(string); !ok || target != "update" {
		return
	}
	args, ok := data["arguments"].([]any)
	if !ok {
		return
	}
	for _, arg := range args {
		argMap, ok := arg.(map[string]any)
		if !ok {
			continue
		}
		msgs, ok := argMap["messages"].([]any)
		if !ok {
			continue
		}
		if text, ok := lastAnswerText(msgs); ok {
			// This path replaces rather than accumulates, so the whole text is
			// stripped each time.
			s.fullText = stripCitations(text)
		}
	}
}

// turnResult reads the backend's verdict. A failed turn sends no answer
// message, so without this the caller receives empty text and no error.
func (s *sendRecvState) turnResult(data map[string]any) error {
	item, ok := data["item"].(map[string]any)
	if !ok {
		return nil
	}
	failure := parseTurnResult(item)
	if failure == nil || s.fullText != "" {
		return nil
	}
	logging.Errorf("sendRecv: %v", failure)
	return failure
}

// completion answers the end-of-turn frame. A turn that produced no text is an
// error rather than an empty answer.
func (s *sendRecvState) completion() error {
	if s.fullText == "" {
		logging.Errorf("sendRecv: %v", ErrEmptyTurn)
		return ErrEmptyTurn
	}
	return nil
}

// lastAnswerText reads the last message of an update argument when it carries
// answer text. carriesAnswerText is what keeps the backend's own tool traffic
// out of the answer.
func lastAnswerText(msgs []any) (string, bool) {
	if len(msgs) == 0 {
		return "", false
	}
	lastMsg, ok := msgs[len(msgs)-1].(map[string]any)
	if !ok || !carriesAnswerText(lastMsg) {
		return "", false
	}
	text, ok := lastMsg["text"].(string)
	return text, ok
}

// decodeSignalRFrame decodes one delimiter-separated frame. Anything that is
// not a JSON object is not a frame this client reads.
func decodeSignalRFrame(part string) (map[string]any, bool) {
	part = strings.TrimSpace(part)
	if part == "" {
		return nil, false
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(part), &data); err != nil {
		return nil, false
	}
	return data, true
}

// signalRFrameType reads a frame's type, or 0 when it declares none.
func signalRFrameType(data map[string]any) int {
	frameType, ok := data["type"].(float64)
	if !ok {
		return 0
	}
	return int(frameType)
}

// answerAccumulator tracks what a turn has already put on the wire.
//
// It keeps two things apart on purpose. The answer is what the backend itself
// restates in a snapshot, and it is the only thing snapshotDelta may compare
// against. Injected text is what this package adds, today the generated-image
// markdown, and a snapshot never carries it: counting it into the baseline made
// the first snapshot diverge, and the opening words of the answer went with it.
type answerAccumulator struct {
	// answer is a Builder because a turn arrives as hundreds of appends.
	answer strings.Builder
	// injected counts bytes emitted that no snapshot restates.
	injected int
	// noticedImage records that the caller was already told a picture is being
	// generated, because M365 repeats the announcement.
	noticedImage bool
	// droppedSnapshots counts the snapshots snapshotDelta refused. They are
	// dropped on purpose, but silently, so a turn that lost answer text looked
	// exactly like a turn that lost nothing.
	droppedSnapshots int
}

// dropSnapshot records a snapshot that could not extend the answer.
func (a *answerAccumulator) dropSnapshot() {
	a.droppedSnapshots++
}

// appendAnswer records answer text that went out as a delta.
func (a *answerAccumulator) appendAnswer(text string) {
	a.answer.WriteString(text)
}

// replaceAnswer adopts a snapshot as the new baseline, because a snapshot
// restates the whole answer rather than extending it.
func (a *answerAccumulator) replaceAnswer(text string) {
	a.answer.Reset()
	a.answer.WriteString(text)
}

// inject records bytes this package emitted that no snapshot restates.
func (a *answerAccumulator) inject(n int) {
	a.injected += n
}

// emitGeneratedImage forwards the generated-image link a message carries, if it
// carries one this turn has not sent yet. It reports whether the caller may keep
// streaming.
//
// The link is recorded with inject rather than appendAnswer. A snapshot restates
// the answer alone and never this link, so a baseline holding it makes the first
// snapshot diverge and the answer's opening words are dropped with it.
//
// M365 announces the work before it does it: the first ImageGeneration message
// of a turn carries an empty ImageReferenceUrls list and a pollUrl, and the one
// that carries the address arrives a minute or more later. That first message
// becomes a notice, so a caller has something to show through the silence.
func (a *answerAccumulator) emitGeneratedImage(msg map[string]any, seenImages map[string]bool, emit func(StreamChunk) bool) bool {
	imgMD := extractImageGenerationMarkdown(msg, seenImages)
	if imgMD == "" {
		if a.noticedImage {
			return true
		}
		a.noticedImage = true
		return emit(StreamChunk{Notice: NoticeImageGenerating})
	}
	a.inject(len(imgMD))
	return emit(StreamChunk{Text: imgMD, IsFinal: false})
}

// baseline returns the text a snapshot must be compared against.
func (a *answerAccumulator) baseline() string {
	return a.answer.String()
}

// emittedBytes returns everything that went out, injected text included.
func (a *answerAccumulator) emittedBytes() int {
	return a.answer.Len() + a.injected
}

// emptyTurn reports whether a finished turn produced nothing the caller can
// use. A turn that answered with an image and no words still produced
// something, which is why the injected count is read here.
func (a *answerAccumulator) emptyTurn(toolCalls []ToolCall) bool {
	return a.emittedBytes() == 0 && len(toolCalls) == 0
}

// parseTurnResult reports the backend's verdict on a finished turn, or nil when
// the turn succeeded or carried no verdict.
//
// The completion frame's `result.value` is "Success" on a turn that answered.
// Anything else is the backend saying it produced nothing, which it does for a
// `tone` it accepts but no longer serves. A frame without `result` returns nil,
// because an unfamiliar frame shape must not turn a working turn into an error.
func parseTurnResult(item map[string]any) *TurnFailedError {
	result, ok := item["result"].(map[string]any)
	if !ok {
		return nil
	}
	value, ok := result["value"].(string)
	if !ok || value == "" || value == "Success" {
		return nil
	}
	message, _ := result["message"].(string)
	turnState, _ := item["turnState"].(string)
	return &TurnFailedError{Value: value, TurnState: turnState, Message: message}
}

// snapshotDelta reports the text a message snapshot adds to what was already
// emitted, and whether the snapshot may become the new baseline.
//
// A turn arrives over two channels that carry the same answer. writeAtCursor
// appends incrementally and renders citations as resolved markdown links, while
// the messages[] snapshots restate the whole answer with raw citation markers.
// The two encodings diverge mid-string, so a snapshot can fail a prefix test
// against an accumulation that already ends with the same text: one measured
// turn produced a 667-byte snapshot against 724 bytes emitted, with identical
// tails. Emitting that snapshot as new text delivered the answer twice.
//
// So a snapshot only ever contributes its prefix extension. When it diverges
// after text has already gone out, it is a re-encoding of delivered content and
// contributes nothing; the longer accumulation stays the baseline, because
// later writeAtCursor deltas append to what was emitted rather than to the
// snapshot. A snapshot that arrives before any text is the answer itself, which
// is how a turn that never streams still produces one.
func snapshotDelta(emitted, snapshot string) (string, bool) {
	if snapshot == emitted {
		return "", false
	}
	if strings.HasPrefix(snapshot, emitted) {
		return snapshot[len(emitted):], true
	}
	if emitted == "" {
		return snapshot, true
	}
	return "", false
}

// carriesAnswerText reports whether a backend message holds assistant answer
// text. M365 mixes its own tool traffic into the same messages array: a
// Progress message carries status, and a GeneratedCode message carries the
// code interpreter's source and then its raw result object. Treating those as
// answer text puts backend internals into the reply, which is the same reason
// servers.withoutBackendToolCalls discards the matching tool calls.
//
// The rule excludes the known backend types rather than admitting only the
// empty messageType that a plain answer carries. Dropping answer text is the
// worse failure, and no evidence rules out an answer arriving under a
// messageType this package has not seen.
func carriesAnswerText(msg map[string]any) bool {
	messageType, _ := msg["messageType"].(string)
	if messageType == "" {
		return true
	}
	if messageType == progressMessageType {
		return false
	}
	_, backendTool := models.ToolMessageType[messageType]
	return !backendTool
}

// extractToolCall extracts a tool call from a message.
func extractToolCall(msg map[string]any, funcName string) *ToolCall {
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
func makeSearchToolCall(query string, msg map[string]any) *ToolCall {
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
func extractImageGenerationMarkdown(msg map[string]any, seenImages map[string]bool) string {
	progressList, ok := msg["contentGenerationProgressList"].([]any)
	if !ok {
		return ""
	}

	var parts []string
	for _, item := range progressList {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		logProgressItem(itemMap)
		urls, ok := itemMap["ImageReferenceUrls"].([]any)
		if !ok {
			continue
		}
		parts = append(parts, unseenImageMarkdown(urls, seenImages)...)
	}

	return strings.Join(parts, "")
}

// logProgressItem records one progress item for diagnosis, truncated so a long
// one does not fill the log.
func logProgressItem(itemMap map[string]any) {
	j, err := json.Marshal(itemMap)
	if err != nil {
		return
	}
	s := string(j)
	if len(s) > 2000 {
		s = textcut.Truncate(s, 2000) + "...(truncated)"
	}
	logging.Debugf("ImageGen progress item JSON: %s", s)
}

// unseenImageMarkdown renders each address that has not been emitted yet, and
// records it. M365 sends the same URL in several Progress updates as the image
// generation completes.
func unseenImageMarkdown(urls []any, seenImages map[string]bool) []string {
	var parts []string
	for _, urlVal := range urls {
		url, ok := urlVal.(string)
		if !ok || url == "" || seenImages[url] {
			continue
		}
		seenImages[url] = true
		logging.Infof("ImageGen: extracted image URL: %s", url)
		parts = append(parts, fmt.Sprintf("\n\n![image](%s)\n\n", url))
	}
	return parts
}

// mapKeys returns the keys of a map as a slice (for debug logging).
func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
