package servers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestV1HealthReportsReachability(t *testing.T) {
	api := &APIServer{}
	recorder := httptest.NewRecorder()
	api.handleV1Health(recorder, httptest.NewRequest(http.MethodGet, "/v1/health", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("health body is not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status field = %q, want ok", body["status"])
	}
}

func TestHealthStaysPlainText(t *testing.T) {
	// The original /health route predates any client contract on its body, so
	// it keeps returning OK as text.
	api := &APIServer{}
	recorder := httptest.NewRecorder()
	api.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if recorder.Body.String() != "OK" {
		t.Fatalf("body = %q, want OK", recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("content type = %q, want text/plain; charset=utf-8", got)
	}
	// A cached liveness answer would report a process that is no longer there.
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// The JSON probe inherits the refusal from the shared writer.
func TestV1HealthRefusesStorage(t *testing.T) {
	api := &APIServer{}
	recorder := httptest.NewRecorder()
	api.handleV1Health(recorder, httptest.NewRequest(http.MethodGet, "/v1/health", nil))

	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}
