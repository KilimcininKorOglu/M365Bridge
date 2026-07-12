package toolcalling

import (
	"strings"
	"testing"
)

func TestContentStreamExtractorCommitsEscapedContentAcrossChunks(t *testing.T) {
	var extractor ContentStreamExtractor
	chunks := []string{
		"noise```json\n{\"choices\":[{\"message\":{\"role\":\"assistant\",",
		"\"content\":\"hello\\nwor",
		"ld \\\\ path \\\"quo",
		"ted\\\" \\u263A\"},\"finish_reason\":\"stop\"}]}\n```",
	}

	var streamedBeforeCommit string
	for _, chunk := range chunks {
		streamedBeforeCommit += extractor.Feed(chunk)
	}
	streamed := streamedBeforeCommit + extractor.Commit(nil)

	want := "hello\nworld \\ path \"quoted\" ☺"
	if streamed != want {
		t.Fatalf("streamed content = %q, want %q", streamed, want)
	}
	if extractor.Text() != want {
		t.Fatalf("extractor text = %q, want %q", extractor.Text(), want)
	}
	if streamedBeforeCommit == "" {
		t.Fatal("assistant content was not streamed before Commit")
	}
}

func TestContentStreamExtractorCommitsSplitSurrogatePair(t *testing.T) {
	var extractor ContentStreamExtractor

	first := extractor.Feed(
		`{"choices":[{"message":{"content":"emoji \uD83D`,
	)
	second := extractor.Feed(`\uDE80 done"}}]}`)
	committed := extractor.Commit(nil)

	if first != "emoji " {
		t.Fatalf("first delta = %q, want %q", first, "emoji ")
	}
	if second != "🚀 done" {
		t.Fatalf("second delta = %q, want %q", second, "🚀 done")
	}
	if committed != "" {
		t.Fatalf("committed delta = %q, want empty", committed)
	}
	if extractor.Text() != "emoji 🚀 done" {
		t.Fatalf("full text = %q", extractor.Text())
	}
}

func TestContentStreamExtractorIgnoresRequestContentBeforeChoices(t *testing.T) {
	var extractor ContentStreamExtractor
	first := `{"input":[{"content":"do not stream me"}],"choices":[{"message":{"content":"stream`
	second := ` me"}}]}`

	streamed := extractor.Feed(first) + extractor.Feed(second)
	if streamed != "stream me" {
		t.Fatalf("streamed content = %q, want %q", streamed, "stream me")
	}
	if got := extractor.Commit(nil); got != "" {
		t.Fatalf("commit repeated streamed content: %q", got)
	}
}

func TestContentStreamExtractorIgnoresNullToolCallContent(t *testing.T) {
	var extractor ContentStreamExtractor
	payload := `{"choices":[{"message":{"content":null,"tool_calls":[{"function":{"name":"read_nonce"}}]}}]}`

	if got := extractor.Feed(payload); got != "" {
		t.Fatalf("tool-call payload emitted content: %q", got)
	}
	if got := extractor.Commit([]string{"read_nonce"}); got != "" {
		t.Fatalf("tool-call payload committed content: %q", got)
	}
	if extractor.Text() != "" {
		t.Fatalf("tool-call payload retained content: %q", extractor.Text())
	}
}

func TestContentStreamExtractorCommitsFinalParserCandidate(t *testing.T) {
	var extractor ContentStreamExtractor
	chunks := []string{
		"```json\n" +
			`{"choices":[{"message":{"role":"assistant","content":"wrong early answer"},"finish_reason":"stop"}]}` +
			"\n```\n",
		"```json\n" +
			`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","name":"read_nonce","arguments":"{}"}]},"finish_reason":"tool_calls"}]}` +
			"\n```",
	}

	var streamed string
	for _, chunk := range chunks {
		streamed += extractor.Feed(chunk)
	}
	final := ParseSimulatedResponseResponses(
		extractor.ParseText(),
		[]string{"read_nonce"},
	)
	streamed += extractor.Commit([]string{"read_nonce"})

	if streamed != final.Content {
		t.Fatalf("streamed content = %q, final parser content = %q", streamed, final.Content)
	}
}

func TestContentStreamExtractorSelectionDoesNotDependOnChunkBoundaries(t *testing.T) {
	raw := "```json\n" +
		`{"choices":[{"message":{"role":"assistant","content":"first answer"},"finish_reason":"stop"}]}` +
		"\n```\n```json\n" +
		`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","name":"read_nonce","arguments":"{}"}]},"finish_reason":"tool_calls"}]}` +
		"\n```"
	splits := [][]string{
		{raw},
		{raw[:70], raw[70:]},
		{raw[:110], raw[110:145], raw[145:]},
	}

	var outputs []string
	for _, chunks := range splits {
		var extractor ContentStreamExtractor
		var output string
		for _, chunk := range chunks {
			output += extractor.Feed(chunk)
		}
		output += extractor.Commit([]string{"read_nonce"})
		outputs = append(outputs, output)
	}

	for index, output := range outputs {
		if output != outputs[0] {
			t.Fatalf(
				"split %d output = %q, first split output = %q",
				index,
				output,
				outputs[0],
			)
		}
	}
	if outputs[0] != "first answer" {
		t.Fatalf("selected content = %q, want first response candidate", outputs[0])
	}
}

func TestContentStreamExtractorIgnoresNestedResponseShapeInsideRequest(t *testing.T) {
	var extractor ContentStreamExtractor
	chunks := []string{
		`{"input":[],"metadata":{"choices":[{"message":{"content":"do not stream`,
		` me"}}]}}` + "\n```json\n" +
			`{"choices":[{"message":{"content":"stream me`,
		` now"}}]}` + "\n```",
	}

	var output string
	for _, chunk := range chunks {
		output += extractor.Feed(chunk)
	}
	output += extractor.Commit(nil)

	if output != "stream me now" {
		t.Fatalf("streamed content = %q, want outer response content", output)
	}
}

func TestContentStreamExtractorNeverLeaksToolCallTransport(t *testing.T) {
	var extractor ContentStreamExtractor
	chunks := []string{
		"```json\n" +
			`{"choices":[{"message":{"role":"assistant","content":"Dosyayı kontrol ediyorum",`,
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_nonce",`,
		`"arguments":"{\"path\":\"/tmp/nonce\"}"}}]},"finish_reason":"tool_calls"}]}` +
			"\n```",
	}

	var streamed string
	for _, chunk := range chunks {
		streamed += extractor.Feed(chunk)
	}
	streamed += extractor.Commit([]string{"read_nonce"})

	if streamed != "Dosyayı kontrol ediyorum" {
		t.Fatalf("streamed content = %q", streamed)
	}
	for _, forbidden := range []string{
		"choices",
		"tool_calls",
		"read_nonce",
		"/tmp/nonce",
		"finish_reason",
	} {
		if strings.Contains(streamed, forbidden) {
			t.Fatalf("transport fragment %q leaked in %q", forbidden, streamed)
		}
	}
}
