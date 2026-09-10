package toolcalling

import "strings"

// WithheldEnvelopeNotice replaces an answer that consisted solely of transport
// syntax. An empty assistant message reads downstream as an empty upstream
// response, so the client is told plainly that the turn produced nothing it can
// use.
const WithheldEnvelopeNotice = "The backend returned an unparseable tool-calling envelope instead of an answer. Please retry this request."

// StripTransportEnvelope removes the simulated transport envelope from text
// that is about to be forwarded as assistant content, reporting whether
// anything was withheld.
//
// It runs only on tool-enabled requests, where the model was told to answer
// with exactly one fenced JSON block carrying the response envelope. A block
// that failed to parse would otherwise reach the client as a wall of raw JSON:
// the turn ends with no tool call and the client shows transport internals as
// if they were the answer. A genuine JSON answer is not lost, because on that
// path it travels inside the envelope's own content field rather than as a
// sibling fence.
func StripTransportEnvelope(text string) (string, bool) {
	if text == "" {
		return "", false
	}

	var stripper envelopeStripper
	for line := range strings.SplitSeq(text, "\n") {
		stripper.feed(line)
	}

	if !stripper.withheld {
		return text, false
	}
	return strings.TrimSpace(strings.Join(stripper.kept, "\n")), true
}

// envelopeStripper scans an answer line by line and keeps the lines that are
// not transport syntax. It carries the fence state between lines, because a
// fence's info string decides what every line until its close is.
type envelopeStripper struct {
	kept            []string
	withheld        bool
	inFence         bool
	fenceIsEnvelope bool
}

// feed reads one line of the answer.
func (s *envelopeStripper) feed(line string) {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "```") {
		s.fenceLine(line, trimmed)
		return
	}
	if s.inFence {
		s.keepUnlessEnvelope(line)
		return
	}
	// Outside a fence, narration about producing the envelope is transport
	// noise too, and the existing thinking rules already recognize it.
	if trimmed != "" && isTransportThinkingLine(strings.ToLower(trimmed)) {
		s.withheld = true
		return
	}
	s.kept = append(s.kept, line)
}

// fenceLine reads a line that opens or closes a fence. An opening one decides
// from its info string whether the fence carries the envelope.
func (s *envelopeStripper) fenceLine(line, trimmed string) {
	if s.inFence {
		s.inFence = false
		s.keepUnlessEnvelope(line)
		return
	}
	s.inFence = true
	s.fenceIsEnvelope = isEnvelopeFenceInfo(strings.TrimPrefix(trimmed, "```"))
	s.keepUnlessEnvelope(line)
}

// keepUnlessEnvelope keeps a line, or records it as withheld when the current
// fence carries the envelope.
func (s *envelopeStripper) keepUnlessEnvelope(line string) {
	if s.fenceIsEnvelope {
		s.withheld = true
		return
	}
	s.kept = append(s.kept, line)
}

// WithholdTransportEnvelope strips the transport envelope and substitutes a
// notice when nothing usable remains.
func WithholdTransportEnvelope(text string) string {
	stripped, withheld := StripTransportEnvelope(text)
	if !withheld {
		return text
	}
	if strings.TrimSpace(stripped) == "" {
		return WithheldEnvelopeNotice
	}
	return stripped
}

// isEnvelopeFenceInfo reports whether a fence info string marks the transport
// envelope. An unlabeled fence counts because models frequently omit the tag on
// the envelope block.
func isEnvelopeFenceInfo(info string) bool {
	switch strings.ToLower(strings.TrimSpace(info)) {
	case "", "json", "jsonc", "json5":
		return true
	}
	return false
}
