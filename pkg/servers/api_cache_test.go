package servers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
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

// The catalog is built from the registry and from configuration fixed at
// startup, so two identical requests must carry the same validator. A volatile
// field anywhere in the body would move the tag on every request and defeat the
// cache it exists to serve.
func TestModelsCarriesAStableValidator(t *testing.T) {
	api := &APIServer{config: &models.Config{}}

	first := httptest.NewRecorder()
	api.handleModels(first, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag was sent, so a repeat request can never be answered with 304")
	}
	if got := first.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Errorf("Cache-Control = %q, want public, max-age=300", got)
	}

	second := httptest.NewRecorder()
	api.handleModels(second, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if got := second.Header().Get("ETag"); got != etag {
		t.Fatalf("the validator moved between two identical requests: %q then %q", etag, got)
	}
}

func TestModelsAnswersARevalidationWith304(t *testing.T) {
	api := &APIServer{config: &models.Config{}}

	first := httptest.NewRecorder()
	api.handleModels(first, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag to revalidate with")
	}

	for name, header := range map[string]string{
		"exact":    etag,
		"weak":     "W/" + etag,
		"any":      "*",
		"in a set": `"other", ` + etag,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("If-None-Match", header)
		api.handleModels(rec, req)

		if rec.Code != http.StatusNotModified {
			t.Errorf("%s: status = %d, want 304", name, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("%s: a 304 carried a body of %d bytes", name, rec.Body.Len())
		}
	}

	stale := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("If-None-Match", `"stale"`)
	api.handleModels(stale, req)
	if stale.Code != http.StatusOK {
		t.Errorf("a stale validator got %d, want a fresh 200", stale.Code)
	}
}

// Every validator this package sends comes from here, so the quoting and the
// stability are pinned once.
func TestContentETagIsAStableQuotedValidator(t *testing.T) {
	body := []byte(`{"status":"ok"}`)

	tag := contentETag(body)
	if !strings.HasPrefix(tag, `"`) || !strings.HasSuffix(tag, `"`) {
		t.Fatalf("tag = %s, want it wrapped in double quotes", tag)
	}
	if again := contentETag(body); again != tag {
		t.Fatalf("the same bytes produced %s and then %s", tag, again)
	}
	if other := contentETag([]byte(`{"status":"down"}`)); other == tag {
		t.Fatal("two different bodies produced the same tag")
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
