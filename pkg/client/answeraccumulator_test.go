package client

import (
	"encoding/json"
	"strings"
	"testing"
)

// The markdown the gateway emits for a generated image, as
// extractImageGenerationMarkdown writes it.
const generatedImageMarkdown = "\n\n![image](https://designerapp.officeapps.live.com/i.png?fileToken=x)\n\n"

// imageProgressMessage decodes a Progress message in the shape the backend
// sends while it generates a picture. An empty url produces the announcement
// frame, whose ImageReferenceUrls list is empty; a url produces the frame that
// carries the finished picture.
func imageProgressMessage(t *testing.T, url string) map[string]any {
	t.Helper()
	urls := `[]`
	if url != "" {
		urls = `["` + url + `"]`
	}
	raw := `{"contentOrigin":"ImageGeneration",
		"contentGenerationProgressList":[{"ImageReferenceUrls":` + urls + `,
		"contentType":"image","fileToken":"d6b7191c","orientation":"Square"}]}`
	var msg map[string]any
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return msg
}

// The measured failure. An image went out first, and the answer's first
// snapshot was compared against a baseline that already held the image
// markdown. The snapshot diverged from it, so it was dropped as a re-encoding,
// and the opening words never reached the client: the reader saw the answer
// start mid-word.
func TestEmitGeneratedImageKeepsTheAnswersFirstSnapshot(t *testing.T) {
	const url = "https://designerapp.officeapps.live.com/i.png?fileToken=x"
	var acc answerAccumulator
	var emitted []string
	emit := func(chunk StreamChunk) bool {
		emitted = append(emitted, chunk.Text)
		return true
	}

	if !acc.emitGeneratedImage(imageProgressMessage(t, url), map[string]bool{}, emit) {
		t.Fatal("emitGeneratedImage stopped a stream the caller may continue")
	}
	if len(emitted) != 1 || !strings.Contains(emitted[0], url) {
		t.Fatalf("emitted = %q, want one chunk carrying the image link", emitted)
	}
	if acc.baseline() != "" {
		t.Fatalf("baseline = %q, want empty; the image link is not answer text", acc.baseline())
	}
	if acc.emittedBytes() != len(emitted[0]) {
		t.Fatalf("emittedBytes = %d, want %d", acc.emittedBytes(), len(emitted[0]))
	}

	const answer = "Buyur, turuncu kedi hazır!"
	chunk, advanced := snapshotDelta(acc.baseline(), answer)
	if !advanced || chunk != answer {
		t.Fatalf("chunk=%q advanced=%v; the answer's first snapshot was dropped", chunk, advanced)
	}
}

// M365 announces the work before it does it. The first ImageGeneration message
// of a turn carries an empty ImageReferenceUrls list, and the one carrying the
// address arrives a minute or more later. That first message becomes the notice
// a reader shows through the silence, and it is not answer content.
func TestEmitGeneratedImageAnnouncesAPictureBeforeItArrives(t *testing.T) {
	var acc answerAccumulator
	var chunks []StreamChunk
	emit := func(chunk StreamChunk) bool {
		chunks = append(chunks, chunk)
		return true
	}
	seen := map[string]bool{}

	// The announcement, then M365 repeating itself while it draws.
	started := imageProgressMessage(t, "")
	for range 3 {
		if !acc.emitGeneratedImage(started, seen, emit) {
			t.Fatal("the announcement stopped the stream")
		}
	}
	assertOnlyNotice(t, &acc, chunks)

	// The address arrives and goes out as the image itself.
	const url = "https://designerapp.officeapps.live.com/i.png?fileToken=x"
	if !acc.emitGeneratedImage(imageProgressMessage(t, url), seen, emit) {
		t.Fatal("the image stopped the stream")
	}
	assertImageFollowedNotice(t, &acc, chunks, url)
}

// assertOnlyNotice pins that the repeated announcement produced one notice and
// no answer content.
func assertOnlyNotice(t *testing.T, acc *answerAccumulator, chunks []StreamChunk) {
	t.Helper()
	if len(chunks) != 1 {
		t.Fatalf("emitted %d chunks, want one notice", len(chunks))
	}
	if chunks[0].Notice != NoticeImageGenerating {
		t.Fatalf("notice = %q, want %q", chunks[0].Notice, NoticeImageGenerating)
	}
	if chunks[0].Text != "" {
		t.Fatalf("the notice carried text %q; it is not answer content", chunks[0].Text)
	}
	if acc.emittedBytes() != 0 {
		t.Fatalf("emittedBytes = %d, want 0; a notice is not content", acc.emittedBytes())
	}
}

// assertImageFollowedNotice pins that the address went out as the image and
// stayed out of the answer baseline.
func assertImageFollowedNotice(t *testing.T, acc *answerAccumulator, chunks []StreamChunk, url string) {
	t.Helper()
	if len(chunks) != 2 || !strings.Contains(chunks[1].Text, url) {
		t.Fatalf("chunks = %+v, want the image after the notice", chunks)
	}
	if acc.baseline() != "" {
		t.Fatalf("baseline = %q, want empty", acc.baseline())
	}
}

// The same comparison with the image in the baseline, which is what the code
// did before. It states the cost of putting injected text there.
func TestSnapshotIsLostWhenInjectedTextEntersTheBaseline(t *testing.T) {
	chunk, advanced := snapshotDelta(generatedImageMarkdown, "Buyur, turuncu kedi hazır!")
	if advanced || chunk != "" {
		t.Fatal("fixture no longer reproduces the failure this rule guards against")
	}
}

// A dropped snapshot is normally a re-encoding of text already delivered, so it
// is dropped on purpose. It was dropped silently, which made a turn that really
// lost answer text look exactly like a turn that lost nothing. Two drops on an
// ordinary turn is the measured norm, so the count is what tells the two apart.
func TestAccumulatorCountsTheSnapshotsItRefuses(t *testing.T) {
	var acc answerAccumulator
	if acc.droppedSnapshots != 0 {
		t.Fatalf("droppedSnapshots = %d on a fresh turn", acc.droppedSnapshots)
	}

	acc.appendAnswer("Buyur, turuncu kedi")
	// The same answer re-encoded: it ends with delivered text but is not a
	// prefix extension, which is exactly what snapshotDelta refuses.
	if _, advanced := snapshotDelta(acc.baseline(), "Buyur, [1] turuncu kedi"); advanced {
		t.Fatal("the fixture no longer produces a dropped snapshot")
	}
	acc.dropSnapshot()

	if acc.droppedSnapshots != 1 {
		t.Fatalf("droppedSnapshots = %d, want 1", acc.droppedSnapshots)
	}
	if acc.baseline() != "Buyur, turuncu kedi" {
		t.Fatalf("a dropped snapshot moved the baseline to %q", acc.baseline())
	}
}

func TestAccumulatorTracksAnswerAndInjectedTextTogether(t *testing.T) {
	var acc answerAccumulator
	acc.appendAnswer("Buy")
	acc.inject(len(generatedImageMarkdown))
	acc.appendAnswer("ur")

	if acc.baseline() != "Buyur" {
		t.Fatalf("baseline = %q, want %q", acc.baseline(), "Buyur")
	}
	if want := len("Buyur") + len(generatedImageMarkdown); acc.emittedBytes() != want {
		t.Fatalf("emittedBytes = %d, want %d", acc.emittedBytes(), want)
	}

	// A snapshot restates the answer alone, so it replaces the answer and
	// leaves the injected count where it is.
	acc.replaceAnswer("Buyur, turuncu kedi hazır!")
	if acc.baseline() != "Buyur, turuncu kedi hazır!" {
		t.Fatalf("baseline after a snapshot = %q", acc.baseline())
	}
	if want := len("Buyur, turuncu kedi hazır!") + len(generatedImageMarkdown); acc.emittedBytes() != want {
		t.Fatalf("emittedBytes after a snapshot = %d, want %d", acc.emittedBytes(), want)
	}
}
