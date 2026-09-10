package toolcalling

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ContentStreamExtractor incrementally exposes only the assistant content
// string inside the first in-progress chat-completion-shaped JSON candidate.
// Transport JSON and tool call fields remain buffered for the final parser.
type ContentStreamExtractor struct {
	buffer            string
	text              string
	done              bool
	candidateLocked   bool
	candidateStart    int
	candidateEnd      int
	candidateComplete bool
}

type streamContentCandidate struct {
	start        int
	end          int
	complete     bool
	shapeFound   bool
	contentFound bool
	content      string
}

// Feed appends a raw response chunk and returns newly decoded assistant
// content. Once the first outer response candidate is selected, Commit uses
// that same candidate so chunk boundaries cannot change the final result.
func (e *ContentStreamExtractor) Feed(chunk string) string {
	if e.done || chunk == "" {
		return ""
	}
	e.buffer += chunk

	candidate, ok := e.currentCandidate()
	if !ok {
		return ""
	}
	if !e.candidateLocked {
		e.candidateLocked = true
		e.candidateStart = candidate.start
	}
	e.candidateEnd = candidate.end
	e.candidateComplete = candidate.complete
	if !candidate.contentFound || !strings.HasPrefix(candidate.content, e.text) {
		return ""
	}

	delta := candidate.content[len(e.text):]
	e.text = candidate.content
	return delta
}

// ParseText returns the exact candidate used for incremental output once that
// candidate is complete. Otherwise it preserves the existing final parser
// behavior over the full raw response.
func (e *ContentStreamExtractor) ParseText() string {
	if e.candidateLocked &&
		e.candidateComplete &&
		e.candidateStart >= 0 &&
		e.candidateEnd > e.candidateStart &&
		e.candidateEnd <= len(e.buffer) {
		return e.buffer[e.candidateStart:e.candidateEnd]
	}
	return e.buffer
}

// Commit parses the selected Responses transport payload and returns only the
// content suffix that Feed has not already published.
func (e *ContentStreamExtractor) Commit(allowedToolNames []string) string {
	if e.done {
		return ""
	}
	// Commit consumes only the parsed content delta, never tool calls, so
	// argument validation is unnecessary here.
	result := ParseSimulatedResponseResponses(
		e.ParseText(),
		allowedToolNames,
		ToolContracts{},
	)
	e.done = true
	if !strings.HasPrefix(result.Content, e.text) {
		return ""
	}
	delta := result.Content[len(e.text):]
	e.text = result.Content
	return delta
}

// Text returns all assistant content published or committed so far.
func (e *ContentStreamExtractor) Text() string {
	return e.text
}

func (e *ContentStreamExtractor) currentCandidate() (streamContentCandidate, bool) {
	if e.candidateLocked {
		return scanStreamContentCandidate(e.buffer, e.candidateStart)
	}
	for index := 0; index < len(e.buffer); {
		start := strings.IndexByte(e.buffer[index:], '{')
		if start < 0 {
			break
		}
		start += index
		candidate, ok := scanStreamContentCandidate(e.buffer, start)
		if ok && candidate.shapeFound {
			return candidate, true
		}
		end, complete := streamJSONEnd(e.buffer, start)
		if !complete {
			break
		}
		index = end
	}
	return streamContentCandidate{}, false
}

func scanStreamContentCandidate(raw string, start int) (streamContentCandidate, bool) {
	if start < 0 || start >= len(raw) || raw[start] != '{' {
		return streamContentCandidate{}, false
	}
	return newStreamContentScanner(raw, start).scan()
}

// scanStep says what the scan should do after reading one character.
type scanStep int

const (
	// scanContinue keeps reading.
	scanContinue scanStep = iota
	// scanContentFound means the assistant content was located.
	scanContentFound
	// scanClosed means the top-level object ended.
	scanClosed
	// scanInvalid means the text cannot be the response this scanner reads.
	scanInvalid
)

// streamContentScanner walks a partial chat completion looking for the
// assistant message content, without waiting for the document to be complete.
//
// The three keys it tracks are nested: choices holds an array, a choice holds
// message, and message holds content. Each one is therefore recognized only at
// the depth the previous one opened, which is what the depth fields record.
type streamContentScanner struct {
	raw       string
	start     int
	candidate streamContentCandidate

	curlyDepth  int
	squareDepth int

	choicesArrayDepth  int
	choiceObjectDepth  int
	messageObjectDepth int

	expectedChoicesArray  int
	expectedMessageObject int

	inString    bool
	escaped     bool
	stringStart int
}

// newStreamContentScanner starts a scan at the opening brace at start.
func newStreamContentScanner(raw string, start int) *streamContentScanner {
	return &streamContentScanner{
		raw:                   raw,
		start:                 start,
		candidate:             streamContentCandidate{start: start, end: len(raw)},
		choicesArrayDepth:     -1,
		choiceObjectDepth:     -1,
		messageObjectDepth:    -1,
		expectedChoicesArray:  -1,
		expectedMessageObject: -1,
		stringStart:           -1,
	}
}

// scan reads the text and reports what it found. Text that runs out mid-object
// still yields a candidate, because the caller is reading a stream.
func (s *streamContentScanner) scan() (streamContentCandidate, bool) {
	for index := s.start; index < len(s.raw); index++ {
		switch s.step(index) {
		case scanContentFound:
			return s.candidate, true
		case scanClosed:
			return s.candidate, s.candidate.shapeFound
		case scanInvalid:
			return streamContentCandidate{}, false
		}
	}
	s.candidate.end = len(s.raw)
	return s.candidate, s.candidate.shapeFound
}

// step reads one character.
func (s *streamContentScanner) step(index int) scanStep {
	if s.inString {
		return s.stringChar(index, s.raw[index])
	}
	return s.structuralChar(index, s.raw[index])
}

// stringChar reads one character inside a JSON string.
func (s *streamContentScanner) stringChar(index int, char byte) scanStep {
	if s.escaped {
		s.escaped = false
		return scanContinue
	}
	if char == '\\' {
		s.escaped = true
		return scanContinue
	}
	if char != '"' {
		return scanContinue
	}
	step := s.closeString(index)
	s.inString = false
	return step
}

// closeString reads the token that just ended. A token this scanner tracks is
// only a key when a colon follows it.
func (s *streamContentScanner) closeString(index int) scanStep {
	var token string
	if err := json.Unmarshal([]byte(s.raw[s.stringStart-1:index+1]), &token); err != nil {
		return scanContinue
	}
	colon := skipStreamWhitespace(s.raw, index+1)
	if colon >= len(s.raw) || s.raw[colon] != ':' {
		return scanContinue
	}
	return s.keyValue(token, skipStreamWhitespace(s.raw, colon+1))
}

// keyValue records where the value of a tracked key begins.
func (s *streamContentScanner) keyValue(token string, value int) scanStep {
	switch {
	case s.atChoicesKey(token):
		s.expectedChoicesArray = value
	case s.atMessageKey(token):
		s.expectedMessageObject = value
	case s.atContentKey(token):
		s.readContent(value)
		return scanContentFound
	}
	return scanContinue
}

// atChoicesKey reports whether the token is the top-level choices key.
func (s *streamContentScanner) atChoicesKey(token string) bool {
	return s.curlyDepth == 1 && s.squareDepth == 0 && token == "choices"
}

// atMessageKey reports whether the token is the message key of a choice.
func (s *streamContentScanner) atMessageKey(token string) bool {
	return s.choiceObjectDepth > 0 &&
		s.curlyDepth == s.choiceObjectDepth &&
		s.squareDepth == s.choicesArrayDepth &&
		token == "message"
}

// atContentKey reports whether the token is the content key of a message.
func (s *streamContentScanner) atContentKey(token string) bool {
	return s.messageObjectDepth > 0 &&
		s.curlyDepth == s.messageObjectDepth &&
		s.squareDepth == s.choicesArrayDepth &&
		token == "content"
}

// readContent records the content string, which may still be incomplete
// because the document read so far is only a prefix of the response.
func (s *streamContentScanner) readContent(value int) {
	s.candidate.contentFound = true
	if value < len(s.raw) && s.raw[value] == '"' {
		escapedContent, complete := scanStreamJSONString(s.raw, value+1)
		s.candidate.content = decodeStreamJSONPrefix(escapedContent, complete)
	}
	s.candidate.end, s.candidate.complete = streamJSONEnd(s.raw, s.start)
}

// structuralChar reads one character outside a JSON string.
func (s *streamContentScanner) structuralChar(index int, char byte) scanStep {
	switch char {
	case '"':
		s.inString = true
		s.escaped = false
		s.stringStart = index + 1
	case '{':
		s.openObject(index)
	case '}':
		return s.closeObject(index)
	case '[':
		s.squareDepth++
		if index == s.expectedChoicesArray {
			s.choicesArrayDepth = s.squareDepth
		}
	case ']':
		s.squareDepth--
		if s.squareDepth < 0 {
			return scanInvalid
		}
	}
	return scanContinue
}

// openObject enters an object and records it when it is the message object the
// message key pointed at, or the choice object inside the choices array.
func (s *streamContentScanner) openObject(index int) {
	s.curlyDepth++
	if index == s.expectedMessageObject {
		s.messageObjectDepth = s.curlyDepth
		s.candidate.shapeFound = true
		return
	}
	if s.atChoiceObject() {
		s.choiceObjectDepth = s.curlyDepth
	}
}

// atChoiceObject reports whether the object being entered is the first choice.
func (s *streamContentScanner) atChoiceObject() bool {
	return s.choicesArrayDepth > 0 &&
		s.choiceObjectDepth < 0 &&
		s.squareDepth == s.choicesArrayDepth &&
		s.curlyDepth == 2
}

// closeObject leaves an object and reports the end of the top-level one.
func (s *streamContentScanner) closeObject(index int) scanStep {
	s.curlyDepth--
	if s.curlyDepth == 0 {
		s.candidate.end = index + 1
		s.candidate.complete = true
		return scanClosed
	}
	if s.curlyDepth < 0 {
		return scanInvalid
	}
	return scanContinue
}

func skipStreamWhitespace(raw string, index int) int {
	for index < len(raw) {
		switch raw[index] {
		case ' ', '\t', '\r', '\n':
			index++
		default:
			return index
		}
	}
	return index
}

func scanStreamJSONString(raw string, start int) (string, bool) {
	escaped := false
	for index := start; index < len(raw); index++ {
		char := raw[index]
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' {
			escaped = true
			continue
		}
		if char == '"' {
			return raw[start:index], true
		}
	}
	return raw[start:], false
}

func decodeStreamJSONPrefix(raw string, complete bool) string {
	minimum := max(len(raw)-16, 0)
	for end := len(raw); end >= minimum; end-- {
		prefix := raw[:end]
		if !complete && endsWithStreamHighSurrogate(prefix) {
			continue
		}
		var decoded string
		if json.Unmarshal([]byte(`"`+prefix+`"`), &decoded) == nil {
			return decoded
		}
	}
	return ""
}

func endsWithStreamHighSurrogate(raw string) bool {
	if len(raw) < 6 {
		return false
	}
	start := len(raw) - 6
	if raw[start] != '\\' || (raw[start+1] != 'u' && raw[start+1] != 'U') {
		return false
	}
	backslashes := 0
	for index := start; index >= 0 && raw[index] == '\\'; index-- {
		backslashes++
	}
	if backslashes%2 == 0 {
		return false
	}
	value, err := strconv.ParseUint(raw[start+2:], 16, 16)
	return err == nil && value >= 0xD800 && value <= 0xDBFF
}

func streamJSONEnd(raw string, start int) (int, bool) {
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(raw); index++ {
		char := raw[index]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == '"' {
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return index + 1, true
			}
		}
	}
	return len(raw), false
}
