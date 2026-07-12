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
	result := ParseSimulatedResponseResponses(
		e.ParseText(),
		allowedToolNames,
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

	candidate := streamContentCandidate{
		start: start,
		end:   len(raw),
	}
	curlyDepth := 0
	squareDepth := 0
	choicesArrayDepth := -1
	choiceObjectDepth := -1
	messageObjectDepth := -1
	expectedChoicesArray := -1
	expectedMessageObject := -1
	inString := false
	escaped := false
	stringStart := -1

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
			if char != '"' {
				continue
			}

			var token string
			if err := json.Unmarshal(
				[]byte(raw[stringStart-1:index+1]),
				&token,
			); err == nil {
				colon := skipStreamWhitespace(raw, index+1)
				if colon < len(raw) && raw[colon] == ':' {
					value := skipStreamWhitespace(raw, colon+1)
					switch {
					case curlyDepth == 1 &&
						squareDepth == 0 &&
						token == "choices":
						expectedChoicesArray = value
					case choiceObjectDepth > 0 &&
						curlyDepth == choiceObjectDepth &&
						squareDepth == choicesArrayDepth &&
						token == "message":
						expectedMessageObject = value
					case messageObjectDepth > 0 &&
						curlyDepth == messageObjectDepth &&
						squareDepth == choicesArrayDepth &&
						token == "content":
						candidate.contentFound = true
						if value < len(raw) && raw[value] == '"' {
							escapedContent, complete := scanStreamJSONString(
								raw,
								value+1,
							)
							candidate.content = decodeStreamJSONPrefix(
								escapedContent,
								complete,
							)
						}
						candidate.end, candidate.complete = streamJSONEnd(
							raw,
							start,
						)
						return candidate, true
					}
				}
			}
			inString = false
			continue
		}

		switch char {
		case '"':
			inString = true
			escaped = false
			stringStart = index + 1
		case '{':
			curlyDepth++
			if index == expectedMessageObject {
				messageObjectDepth = curlyDepth
				candidate.shapeFound = true
			} else if choicesArrayDepth > 0 &&
				choiceObjectDepth < 0 &&
				squareDepth == choicesArrayDepth &&
				curlyDepth == 2 {
				choiceObjectDepth = curlyDepth
			}
		case '}':
			curlyDepth--
			if curlyDepth == 0 {
				candidate.end = index + 1
				candidate.complete = true
				return candidate, candidate.shapeFound
			}
			if curlyDepth < 0 {
				return streamContentCandidate{}, false
			}
		case '[':
			squareDepth++
			if index == expectedChoicesArray {
				choicesArrayDepth = squareDepth
			}
		case ']':
			squareDepth--
			if squareDepth < 0 {
				return streamContentCandidate{}, false
			}
		}
	}

	candidate.end = len(raw)
	return candidate, candidate.shapeFound
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
	minimum := len(raw) - 16
	if minimum < 0 {
		minimum = 0
	}
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
