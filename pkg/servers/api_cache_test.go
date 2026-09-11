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

// An error body travels the same writer, so it inherits the refusal.
func TestErrorBodyRefusesStorage(t *testing.T) {
	api := &APIServer{}
	rec := httptest.NewRecorder()
	api.sendError(rec, http.StatusNotFound, "Not found")

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}
