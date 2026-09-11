package servers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A JSON body from this gateway carries conversation content, a session mapping
// or account data. A 200 that declares no expiry lets a cache assign its own
// freshness, so the shared writer has to refuse storage on its own.
func TestSendJSONRefusesStorageByDefault(t *testing.T) {
	api := &APIServer{}
	rec := httptest.NewRecorder()
	api.sendJSON(rec, http.StatusOK, map[string]string{"status": "ok"})

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

// A handler that serves cacheable data sets its own rule first, and the shared
// writer must not overwrite it with the refusal.
func TestSendJSONKeepsARuleTheHandlerAlreadySet(t *testing.T) {
	api := &APIServer{}
	rec := httptest.NewRecorder()
	rec.Header().Set("Cache-Control", "public, max-age=300")
	api.sendJSON(rec, http.StatusOK, map[string]string{"status": "ok"})

	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("Cache-Control = %q, want the handler's own rule", got)
	}
}

// A stream must not be stored at all. no-cache permits storage and only forces
// revalidation, which is the wrong rule for an answer that exists once.
func TestSSERefusesStorage(t *testing.T) {
	api := &APIServer{}
	rec := httptest.NewRecorder()
	if _, ok := api.beginSSE(rec); !ok {
		t.Fatal("beginSSE refused a recorder that flushes")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
}

// The probe writes its own envelope, so it has to reach the shared header block
// rather than repeat it, and its buffered branch needs the refusal too.
func TestResponsesProbeRefusesStorageOnBothBranches(t *testing.T) {
	api := &APIServer{}

	streamed := httptest.NewRecorder()
	api.respondResponsesProbe(streamed, "gpt5.5-reasoning", true)
	if got := streamed.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("stream Cache-Control = %q, want no-store", got)
	}

	buffered := httptest.NewRecorder()
	api.respondResponsesProbe(buffered, "gpt5.5-reasoning", false)
	if got := buffered.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("buffered Cache-Control = %q, want no-store", got)
	}
}

// An error body travels the same writer, so it inherits the refusal.
func TestErrorBodyRefusesStorage(t *testing.T) {
	api := &APIServer{}
	rec := httptest.NewRecorder()
	api.sendError(rec, http.StatusNotFound, "Not found")

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}
