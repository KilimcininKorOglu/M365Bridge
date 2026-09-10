// Package payload provides request payload builders for M365 Copilot WebSocket communication.
// It constructs JSON payloads for chat requests, conversation history, and various options.
package payload

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
	"github.com/google/uuid"
)

const (
	// variants is the feature flags string sent with requests.
	//
	// feature.EnableMergingPureDeltas is measured, not copied: without it one
	// long answer arrived as about 840 writeAtCursor deltas, with it as about
	// 130, carrying the same bytes and the same answer. That is six times fewer
	// SSE frames per turn for the same content. Every other flag a browser
	// capture carries and this list does not was measured as inert on a chat
	// turn and on an image turn, so none of them is here.
	variants = "EnableMcpServerWidgets,feature.EnableLuForChatCIQ,feature.enableChatCIQPlugin," +
		"EnableRequestPlugins,feature.IsCustomEngineCopilotEnabled,feature.bizchatfluxv3," +
		"feature.enablechatpages,feature.IsStreamingModeInChatEnabled," +
		"IncludeSourceAttributionsConcise,SkipPublishEmptyMessage," +
		"feature.EnableDeduplicatingSourceAttributions,feature.enableDeltaStreamingForReferences," +
		"feature.enableIncludeReferencesInDeltaResponse,feature.enablereferencesforagents," +
		"feature.EnableReferencesListCompleteSignal,SingletonEnvOn,cdxenablefccinmainline," +
		"feature.disabledisallowedmsgs,cdxenablerenderforisocomp," +
		"feature.EnablePersonalization,feature.EnableSkipEmittingMessageOnFlush," +
		"feature.EnableRemoveEmptySourceAttributions,feature.EnableRemoveStreamingMode," +
		"feature.OfficeWebToHelix,feature.OfficeDesktopToHelix,feature.M365TeamsHubToHelix," +
		"feature.OwaHubToHelix,feature.MonarchHubToHelix,feature.Win32OutlookHubToHelix," +
		"feature.MacOutlookHubToHelix,Agt_bizchat_enableGpt5ForHelix," +
		"feature.EnableMergingPureDeltas"
)

// optionsSetsFull contains the full set of option flags for complete functionality.
var optionsSetsFull = []string{
	"search_result_progress_messages_with_search_queries",
	"update_textdoc_response_after_streaming",
	"deepleo_networking_timeout_10minutes_canmore",
	"cwc_code_interpreter",
	"cwc_code_interpreter_amsfix", "cwcfluxgptv",
	"gptvnorm2048", "cwc_code_interpreter_citation_fix",
	"code_interpreter_interactive_charts",
	"cwc_code_interpreter_interactive_charts_inline_image",
	"code_interpreter_matplotlib_patching",
	"cwc_fileupload_odb", "update_memory_plugin",
	"add_custom_instructions",
	"enable_batch_token_processing",
	"enable_gg_gpt",
	"rich_responses",
	"pages_citations", "pages_citations_multiturn",
}

// fileUploadOptions contains option flags specific to file upload.
var fileUploadOptions = map[string]bool{
	"cwc_fileupload_odb": true,
}

// codeInterpreterOptions contains option flags that activate M365's built-in
// code_interpreter sandbox. When client-defined tools are present, these MUST be
// stripped to prevent M365 from intercepting file/code operations and routing
// them to its own sandbox instead of emitting tool calls for the client.
// This is the primary infrastructure lever (cramt/m365-copilot-proxy approach).
var codeInterpreterOptions = map[string]bool{
	"cwc_code_interpreter":                                 true,
	"cwc_code_interpreter_amsfix":                          true,
	"cwcfluxgptv":                                          true,
	"gptvnorm2048":                                         true,
	"cwc_code_interpreter_citation_fix":                    true,
	"code_interpreter_interactive_charts":                  true,
	"cwc_code_interpreter_interactive_charts_inline_image": true,
	"code_interpreter_matplotlib_patching":                 true,
}

// imageUploadOptions contains option flags needed for image upload support.
var imageUploadOptions = map[string]bool{
	"cwc_flux_image": true,
	"flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch": true,
}

// allowedMessageTypes lists the message types allowed in requests.
var allowedMessageTypes = []string{
	"Chat", "Suggestion", "InternalSearchQuery", "Disengaged",
	"InternalLoaderMessage", "Progress", "GeneratedCode",
	"RenderCardRequest", "AdsQuery", "SemanticSerp",
	"GenerateContentQuery", "SearchQuery",
	"ConfirmationCard", "AuthError", "DeveloperLogs",
	"TriggerPlugin", "HintInvocation", "MemoryUpdate",
	"EndOfRequest", "TriggerConfirmation",
	"ResumeInvokeAction", "ResumeUserInputRequest",
}

// Message represents a chat message in the conversation history.
type Message struct {
	Role        string              `json:"role"`
	Content     string              `json:"content"`
	Name        string              `json:"name,omitempty"`
	Images      []ImageData         `json:"-"`
	Annotations []MessageAnnotation `json:"-"`
	ToolCallID  string              `json:"tool_call_id,omitempty"` // OpenAI tool role messages
	// ToolCalls lists the tool calls this assistant message announced and
	// ToolResults lists the results it carries. The conversation payload
	// flattens both into text, so the structure is kept here for the server to
	// check that a result answers a call the same request declared, and to
	// rebuild the evidence ledger for a client-driven tool loop.
	ToolCalls   []ToolCallRecord   `json:"-"`
	ToolResults []ToolResultRecord `json:"-"`
	// ToolProgress marks a message that reports a client tool still running.
	// It travels as a user message so the model reads it, but it neither
	// answers the pending call nor starts a new user turn.
	ToolProgress bool `json:"-"`
}

// ToolCallRecord is one tool call announced by an assistant message, kept in
// its structured form after the message content has been flattened to text.
type ToolCallRecord struct {
	ID        string
	Name      string
	Arguments string
}

// ToolResultRecord is one tool result carried by a message, kept in its
// structured form after the message content has been flattened to text.
type ToolResultRecord struct {
	ID      string
	Content string
}

// ImageData represents an image extracted from multimodal content.
type ImageData struct {
	Base64    string // raw base64 data without data: prefix
	MediaType string // e.g. "image/png"
	FileName  string // e.g. "upload.png"
	// RemoteURL holds a caller-supplied https URL whose bytes have not been
	// fetched yet. This package performs no network I/O, so the server layer
	// resolves it before upload; Base64 is empty until then.
	RemoteURL string
}

// MessageAnnotation represents an image annotation attached to a WebSocket message.
type MessageAnnotation struct {
	ID                        string            `json:"id"`
	MessageAnnotationType     string            `json:"messageAnnotationType"`
	MessageAnnotationMetadata map[string]string `json:"messageAnnotationMetadata"`
}

// appendImageURL records one image the caller named by url. A data URL is
// decoded here; a remote address is kept for the server layer to fetch, because
// this package does no network I/O. Anything else names no image and is
// ignored.
func (m *Message) appendImageURL(url string) {
	if img := parseDataURL(url); img != nil {
		m.Images = append(m.Images, *img)
		return
	}
	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
		m.Images = append(m.Images, ImageData{RemoteURL: url})
	}
}

// rawToolCall is one entry of the OpenAI tool_calls field as it arrives.
type rawToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// rawMessage is the wire shape of a message before its content is decoded.
// Content stays raw because it is a string on one request and an array of
// content blocks on the next.
type rawMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []rawToolCall   `json:"tool_calls,omitempty"`
}

// isToolRole reports whether a message answers a tool call the caller made.
func isToolRole(role, toolCallID string) bool {
	return role == "tool" && toolCallID != ""
}

// UnmarshalJSON implements custom JSON unmarshaling for Message to handle
// both string content and multimodal content arrays (OpenAI/Anthropic format).
// It also converts tool-related messages (tool role, tool_calls, tool_result,
// tool_use blocks) into plain text so the M365 backend can process them.
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw rawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.Name = raw.Name
	m.ToolCallID = raw.ToolCallID
	m.applyToolCalls(raw.ToolCalls)
	return m.applyContent(raw.Content, isToolRole(raw.Role, raw.ToolCallID))
}

// applyToolCalls records an OpenAI assistant message's tool_calls field and
// flattens it into the content text the backend reads.
func (m *Message) applyToolCalls(calls []rawToolCall) {
	if len(calls) == 0 {
		return
	}
	var sb strings.Builder
	for _, tc := range calls {
		fmt.Fprintf(&sb, "[Previous Tool Call: %s]\nArguments: %s\n\n", tc.Function.Name, tc.Function.Arguments)
		m.ToolCalls = append(m.ToolCalls, ToolCallRecord{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	m.Content = strings.TrimSpace(sb.String())
}

// applyContent decodes the content field, which is a string on one request and
// an array of content blocks on the next.
func (m *Message) applyContent(content json.RawMessage, toolRole bool) error {
	// An absent content field and the null an assistant message with tool_calls
	// carries both name a message that has no text of its own.
	if len(content) == 0 || string(content) == "null" {
		m.recordEmptyToolResult(toolRole)
		return nil
	}

	// Try string content first
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		m.applyStringContent(s, toolRole)
		return nil
	}

	// Try array of content blocks
	var blocks []map[string]any
	if err := json.Unmarshal(content, &blocks); err != nil {
		return fmt.Errorf("content must be string or array of content blocks")
	}
	m.applyContentBlocks(blocks)
	m.recordBlockToolResult(toolRole)
	return nil
}

// recordEmptyToolResult keeps a tool role message's result record even when the
// message carries no content, so the server still sees which call it answers.
func (m *Message) recordEmptyToolResult(toolRole bool) {
	if toolRole {
		m.ToolResults = append(m.ToolResults, ToolResultRecord{ID: m.ToolCallID})
	}
}

// recordBlockToolResult covers a tool role message whose content is a block
// array carrying its result as text blocks rather than a tool_result block, so
// the record is built from the accumulated text instead.
func (m *Message) recordBlockToolResult(toolRole bool) {
	if toolRole && len(m.ToolResults) == 0 {
		m.ToolResults = append(m.ToolResults, ToolResultRecord{ID: m.ToolCallID, Content: m.Content})
	}
}

// applyStringContent stores plain string content and converts a tool role
// message into the formatted text the backend reads.
func (m *Message) applyStringContent(s string, toolRole bool) {
	m.Content = s
	if toolRole {
		m.ToolResults = append(m.ToolResults, ToolResultRecord{ID: m.ToolCallID, Content: s})
		m.Content = fmt.Sprintf("[Tool Result (call_id: %s)]\n%s", m.ToolCallID, s)
	}
}

// applyContentBlocks routes each multimodal content block to the reader for its
// type. A type with no case names nothing this gateway can forward.
func (m *Message) applyContentBlocks(blocks []map[string]any) {
	for _, block := range blocks {
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text", "input_text", "output_text":
			m.appendTextBlock(block)
		case "input_image":
			m.appendResponsesImageBlock(block)
		case "image_url":
			m.appendChatImageBlock(block)
		case "image":
			m.appendAnthropicImageBlock(block)
		case "tool_use":
			m.appendToolUseBlock(block)
		case "tool_result":
			m.appendToolResultBlock(block)
		case "input_file", "file", "input_audio", "audio":
			// The M365 backend accepts image attachments only, so these blocks
			// cannot be forwarded. They were already skipped by falling through
			// the switch; the case exists so the drop is visible in the log
			// rather than looking like the client never sent anything.
			logging.Debugf("Message.UnmarshalJSON: dropping unsupported %q content block", blockType)
		}
	}
}

// appendTextBlock appends a text block's text. The Responses API names the same
// block input_text on the way in and output_text on the way back.
func (m *Message) appendTextBlock(block map[string]any) {
	if txt, ok := block["text"].(string); ok {
		m.Content += txt
	}
}

// appendResponsesImageBlock reads the Responses format
// {"type":"input_image","image_url":"data:image/png;base64,..."}. The url is a
// bare string here, not the object Chat Completions wraps it in. A file_id
// reference is not supported, because this gateway serves no Files API to
// resolve it against.
func (m *Message) appendResponsesImageBlock(block map[string]any) {
	if url, ok := block["image_url"].(string); ok {
		m.appendImageURL(url)
	}
}

// appendChatImageBlock reads the OpenAI format
// {"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}. The
// url may also be a remote https address, which the server layer fetches later
// because this package does no network I/O.
//
// Clients also send the url bare under this type, the way the Responses
// input_image block carries it. Both shapes name the same image, so both are
// read rather than one being dropped.
func (m *Message) appendChatImageBlock(block map[string]any) {
	switch imgURL := block["image_url"].(type) {
	case string:
		m.appendImageURL(imgURL)
	case map[string]any:
		if url, ok := imgURL["url"].(string); ok {
			m.appendImageURL(url)
		}
	}
}

// appendAnthropicImageBlock reads the Anthropic format
// {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "..."}}.
func (m *Message) appendAnthropicImageBlock(block map[string]any) {
	src, ok := block["source"].(map[string]any)
	if !ok {
		return
	}
	if srcType, ok := src["type"].(string); !ok || srcType != "base64" {
		return
	}
	mediaType, _ := src["media_type"].(string)
	base64Data, _ := src["data"].(string)
	if base64Data == "" {
		return
	}
	m.Images = append(m.Images, ImageData{
		Base64:    base64Data,
		MediaType: mediaType,
		FileName:  "upload." + extFromMediaType(mediaType),
	})
}

// appendToolUseBlock records an Anthropic assistant message's previous tool call.
func (m *Message) appendToolUseBlock(block map[string]any) {
	name, _ := block["name"].(string)
	id, _ := block["id"].(string)
	arguments := ""
	if inputBytes, err := json.Marshal(block["input"]); err == nil {
		arguments = string(inputBytes)
		m.Content += fmt.Sprintf("\n[Previous Tool Call: %s]\nArguments: %s\n", name, arguments)
	}
	m.ToolCalls = append(m.ToolCalls, ToolCallRecord{ID: id, Name: name, Arguments: arguments})
}

// appendToolResultBlock records an Anthropic user message's tool result.
func (m *Message) appendToolResultBlock(block map[string]any) {
	toolUseID, _ := block["tool_use_id"].(string)
	resultContent := toolResultText(block["content"])
	m.ToolResults = append(m.ToolResults, ToolResultRecord{ID: toolUseID, Content: resultContent})
	m.Content += fmt.Sprintf("\n[Tool Result (call_id: %s)]\n%s\n", toolUseID, resultContent)
}

// toolResultText reads a tool_result block's content, which is a string on one
// request and an array of text blocks on the next.
func toolResultText(content any) string {
	if c, ok := content.(string); ok {
		return c
	}
	cArr, ok := content.([]any)
	if !ok {
		return ""
	}
	var text strings.Builder
	for _, cItem := range cArr {
		cMap, ok := cItem.(map[string]any)
		if !ok {
			continue
		}
		if txt, ok := cMap["text"].(string); ok {
			text.WriteString(txt)
		}
	}
	return text.String()
}

// parseDataURL parses a data URL (data:image/png;base64,...) and returns ImageData.
func parseDataURL(url string) *ImageData {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return nil
	}
	rest := url[len(prefix):]
	semiIdx := strings.Index(rest, ";")
	if semiIdx < 0 {
		return nil
	}
	mediaType := rest[:semiIdx]
	rest = rest[semiIdx+1:]
	encoding, base64Data, found := strings.Cut(rest, ",")
	if !found {
		return nil
	}
	if encoding != "base64" {
		return nil
	}
	return &ImageData{
		Base64:    base64Data,
		MediaType: mediaType,
		FileName:  "upload." + extFromMediaType(mediaType),
	}
}

// extFromMediaType returns the file extension for a media type.
func extFromMediaType(mediaType string) string {
	switch mediaType {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return "bin"
	}
}

// BuildURL constructs the WebSocket URL for M365 Copilot connection.
// Returns the complete URL, hex session ID, and UUID session ID.
func BuildURL(token, hexSID, conversationID, userOID, tenantID string) (string, string, string, error) {
	if userOID == "" || tenantID == "" {
		return "", "", "", fmt.Errorf("M365_USER_OID and M365_TENANT_ID are required")
	}

	if hexSID == "" {
		hexSID = uuid.New().String()
	}

	uuidSID := formatUUID(hexSID)

	baseURL := fmt.Sprintf("wss://substrate.office.com/m365Copilot/Chathub/%s@%s", userOID, tenantID)
	url := fmt.Sprintf("%s?chatsessionid=%s&XRoutingParameterSessionKey=%s&clientrequestid=%s&X-SessionId=%s",
		baseURL, hexSID, hexSID, hexSID, uuidSID)

	if conversationID != "" {
		url += fmt.Sprintf("&ConversationId=%s", conversationID)
	}

	url += fmt.Sprintf("&access_token=%s", token)
	url += fmt.Sprintf("&variants=%s", variants)
	url += "&source=%22officeweb%22&product=Office&agentHost=Bizchat.FullScreen"
	url += "&licenseType=Starter&isEdu=true&agent=web&scenario=OfficeWebIncludedCopilot"

	return url, hexSID, uuidSID, nil
}

// formatUUID converts a hex string (with or without dashes) to UUID format (8-4-4-4-12).
func formatUUID(hex string) string {
	hex = strings.ReplaceAll(hex, "-", "")
	if len(hex) < 32 {
		return hex
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex[0:8], hex[8:12], hex[12:16], hex[16:20], hex[20:32])
}

// webSearchPlugins returns the built-in plugin list for a request. BingWebSearch
// is the only server-side plugin the backend needs declared, and M365_ENABLE_WEB_SEARCH
// turns it off for callers that want the model to answer from the conversation alone.
func webSearchPlugins(enableWebSearch bool) []map[string]string {
	if !enableWebSearch {
		return []map[string]string{}
	}
	return []map[string]string{
		{"Id": "BingWebSearch", "Source": "BuiltIn"},
	}
}

// BuildPayload constructs a chat request payload for a single message.
// When hasTools is true, code_interpreter option flags are stripped to prevent
// M365 from intercepting file/code operations.
func BuildPayload(hexSID, uuidSID, text, tone, gptOverride string, enableFileUpload, hasTools, enableWebSearch bool, extraOptions []string) (string, error) {
	invocationID := uuid.New().String()
	options := getOptions(enableFileUpload, false, hasTools, extraOptions)

	payload := map[string]any{
		"type":         4,
		"invocationId": invocationID,
		"target":       "chat",
		"arguments": []map[string]any{
			{
				"source":                    "officeweb",
				"clientCorrelationId":       hexSID,
				"sessionId":                 uuidSID,
				"message":                   buildFullMessage(hexSID, text, nil),
				"optionsSets":               options,
				"streamingMode":             "ConciseWithPadding",
				"spokenTextMode":            "None",
				"options":                   map[string]any{},
				"extraExtensionParameters":  map[string]any{},
				"allowedMessageTypes":       allowedMessageTypes,
				"sliceIds":                  []string{},
				"tone":                      tone,
				"plugins":                   webSearchPlugins(enableWebSearch),
				"isStartOfSession":          false,
				"isSbsSupported":            true,
				"renderReferencesBehindEOS": true,
				"disconnectBehavior":        "continue",
			},
		},
	}

	if gptOverride != "" {
		args := payload["arguments"].([]map[string]any)
		args[0]["gptIdOverride"] = map[string]string{
			"id":     gptOverride,
			"source": "MOS3",
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// IsSystemRole reports whether a role carries instructions rather than
// conversation. OpenAI renamed the system role to developer for its reasoning
// models and both names remain valid, so every site that treats a system
// message specially has to accept the newer name too.
func IsSystemRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system", "developer":
		return true
	}
	return false
}

// carriesConversationText reports whether a message contributes a turn to the
// flattened history. A system message is merged in separately and an empty one
// names no turn.
func carriesConversationText(message Message) bool {
	return !IsSystemRole(message.Role) && strings.TrimSpace(message.Content) != ""
}

// lastConversationTurn returns the index of the final contributing message and
// how many of them there are.
func lastConversationTurn(messages []Message) (int, int) {
	lastIndex, count := -1, 0
	for index, message := range messages {
		if !carriesConversationText(message) {
			continue
		}
		lastIndex = index
		count++
	}
	return lastIndex, count
}

// historyLabel names a role in the flattened history.
func historyLabel(role string) string {
	if label := strings.ToUpper(role); label != "TOOL" {
		return label
	}
	return "TOOL RESULT"
}

// flattenConversation writes the whole history as one block of text, with the
// message at lastIndex marked as the turn the model must answer.
func flattenConversation(messages []Message, lastIndex int) string {
	var flattened strings.Builder
	flattened.WriteString("CLIENT-PROVIDED CONVERSATION HISTORY\n")
	for index, message := range messages {
		if !carriesConversationText(message) {
			continue
		}
		if index == lastIndex {
			flattened.WriteString("\nCURRENT USER MESSAGE\n")
			flattened.WriteString(message.Content)
			continue
		}
		flattened.WriteString(historyLabel(message.Role))
		flattened.WriteString(": ")
		flattened.WriteString(message.Content)
		flattened.WriteString("\n")
	}
	return flattened.String()
}

// conversationTextForM365 renders the turn text the backend receives. Without
// history that is the last message alone, because the backend tracks the rest
// by conversation ID.
func conversationTextForM365(messages []Message, includeHistory bool) string {
	if len(messages) == 0 {
		return ""
	}

	lastText := messages[len(messages)-1].Content
	if !includeHistory {
		return lastText
	}

	lastConversationIndex, conversationCount := lastConversationTurn(messages)
	if conversationCount <= 1 {
		return lastText
	}
	return flattenConversation(messages, lastConversationIndex)
}

// BuildConversationPayload constructs a chat request payload with conversation history.
// When hasTools is true, code_interpreter option flags are stripped to prevent
// M365 from intercepting file/code operations.
func BuildConversationPayload(hexSID, uuidSID string, messages []Message, includeHistory bool, tone, gptOverride string, enableFileUpload, hasTools, enableWebSearch bool, extraOptions []string) (string, error) {
	invocationID := uuid.New().String()

	// Extract annotations from the last message (images are attached to the last user message)
	var annotations []MessageAnnotation
	hasImages := false
	lastText := conversationTextForM365(messages, includeHistory)
	if len(messages) > 0 {
		annotations = messages[len(messages)-1].Annotations
		hasImages = len(annotations) > 0
	}

	// Merge system messages into the last message as a prefix.
	// M365 backend tracks conversation history via ConversationId, so only the
	// last message is sent. System prompts in earlier messages would be lost.
	// Prepending them to the last message ensures they reach the model.
	var systemParts []string
	for _, msg := range messages {
		if IsSystemRole(msg.Role) && strings.TrimSpace(msg.Content) != "" {
			systemParts = append(systemParts, msg.Content)
		}
	}
	if len(systemParts) > 0 {
		systemPrefix := strings.Join(systemParts, "\n\n")
		lastText = systemPrefix + "\n\n" + lastText
	}

	options := getOptions(enableFileUpload, hasImages, hasTools, extraOptions)

	payload := map[string]any{
		"type":         4,
		"invocationId": invocationID,
		"target":       "chat",
		"arguments": []map[string]any{
			{
				"source":                    "officeweb",
				"clientCorrelationId":       hexSID,
				"sessionId":                 uuidSID,
				"message":                   buildMinimalMessage(hexSID, lastText, annotations),
				"optionsSets":               options,
				"streamingMode":             "ConciseWithPadding",
				"spokenTextMode":            "None",
				"options":                   map[string]any{},
				"extraExtensionParameters":  map[string]any{},
				"allowedMessageTypes":       allowedMessageTypes,
				"sliceIds":                  []string{},
				"tone":                      tone,
				"plugins":                   webSearchPlugins(enableWebSearch),
				"isStartOfSession":          false,
				"isSbsSupported":            true,
				"renderReferencesBehindEOS": true,
				"disconnectBehavior":        "continue",
			},
		},
	}

	if gptOverride != "" {
		args := payload["arguments"].([]map[string]any)
		args[0]["gptIdOverride"] = map[string]string{
			"id":     gptOverride,
			"source": "MOS3",
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// buildFullMessage constructs a full message object for single-message requests.
// Uses the full entityAnnotationTypes and connectedFederatedConnections.
func buildFullMessage(hexSID, text string, annotations []MessageAnnotation) map[string]any {
	now := time.Now()
	_, offset := now.Zone()
	tzName := getTZName()

	msg := map[string]any{
		"author":                        "user",
		"inputMethod":                   "Keyboard",
		"text":                          text,
		"entityAnnotationTypes":         []string{"People", "File", "Event", "Email", "TeamsMessage"},
		"connectedFederatedConnections": []string{"dummyId"},
		"requestId":                     hexSID + "_0",
		"locationInfo": map[string]any{
			"timeZoneOffset": offset / 3600,
			"timeZone":       tzName,
		},
		"locale":            getLocale(),
		"messageType":       "Chat",
		"experienceType":    "Default",
		"adaptiveCards":     []any{},
		"clientPreferences": map[string]any{},
	}

	if len(annotations) > 0 {
		msg["messageAnnotations"] = annotations
	}

	return msg
}

// buildMinimalMessage constructs a minimal message object for conversation requests.
// Uses empty entityAnnotationTypes and no connectedFederatedConnections.
func buildMinimalMessage(hexSID, text string, annotations []MessageAnnotation) map[string]any {
	now := time.Now()
	_, offset := now.Zone()
	tzName := getTZName()

	msg := map[string]any{
		"author":                "user",
		"inputMethod":           "Keyboard",
		"text":                  text,
		"entityAnnotationTypes": []string{},
		"requestId":             hexSID + "_0",
		"locationInfo": map[string]any{
			"timeZoneOffset": offset / 3600,
			"timeZone":       tzName,
		},
		"locale":            getLocale(),
		"messageType":       "Chat",
		"experienceType":    "Default",
		"adaptiveCards":     []any{},
		"clientPreferences": map[string]any{},
	}

	if len(annotations) > 0 {
		msg["messageAnnotations"] = annotations
	}

	return msg
}

// getTZName returns the system timezone name.
// Tries TZ env var, then /etc/localtime symlink, then falls back to UTC.
func getTZName() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	// On Unix/macOS, /etc/localtime is a symlink to the timezone file
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		// Path looks like /var/db/timezone/zoneinfo/Europe/Istanbul
		// or ../zoneinfo/Europe/Istanbul
		parts := strings.Split(link, "/")
		for i, p := range parts {
			if p == "zoneinfo" && i+1 < len(parts) {
				return strings.Join(parts[i+1:], "/")
			}
		}
		// Try reading the link target directly
		if resolved, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
			parts := strings.Split(resolved, "/")
			for i, p := range parts {
				if p == "zoneinfo" && i+1 < len(parts) {
					return strings.Join(parts[i+1:], "/")
				}
			}
		}
	}
	return "UTC"
}

// getLocale returns the system locale, defaulting to "en-us".
func getLocale() string {
	for _, env := range []string{"LANG", "LC_ALL", "LC_MESSAGES"} {
		lang := os.Getenv(env)
		if lang == "" || lang == "C" || lang == "POSIX" || lang == "c" {
			continue
		}
		lang = strings.SplitN(lang, ".", 2)[0]
		lang = strings.ReplaceAll(lang, "_", "-")
		return strings.ToLower(lang)
	}
	return "en-us"
}

// getOptions returns the appropriate option set based on feature flags.
// When hasTools is true, code_interpreter flags are stripped to prevent M365
// from routing file/code operations to its own sandbox.
func getOptions(enableFileUpload, enableImageUpload, hasTools bool, extraOptions []string) []string {
	options := make([]string, 0, len(optionsSetsFull))
	options = append(options, optionsSetsFull...)

	if !enableFileUpload {
		filtered := make([]string, 0, len(options))
		for _, opt := range options {
			if !fileUploadOptions[opt] {
				filtered = append(filtered, opt)
			}
		}
		options = filtered
	}

	// Strip code_interpreter flags when client-defined tools are present.
	// This is the primary infrastructure lever: without it, M365 intercepts
	// file operations before the LLM processes text instructions.
	if hasTools {
		filtered := make([]string, 0, len(options))
		for _, opt := range options {
			if !codeInterpreterOptions[opt] {
				filtered = append(filtered, opt)
			}
		}
		options = filtered
	}

	if enableImageUpload {
		for opt := range imageUploadOptions {
			options = append(options, opt)
		}
	}

	if len(extraOptions) > 0 {
		options = append(options, extraOptions...)
	}

	return options
}
