package servers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
)

func webUIServer(enabled bool) *APIServer {
	return &APIServer{config: &models.Config{EnableWebUI: enabled}}
}

func TestWebUIServesTheDocument(t *testing.T) {
	api := webUIServer(true)
	rec := httptest.NewRecorder()
	api.handleWebUI(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "<div id=\"root\"") {
		t.Errorf("body is not the interface document: %s", rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("no ETag was sent, so a reload can never be answered with 304")
	}
}

// "/" is the pattern the mux falls back to, so an unmatched API path lands
// here. Answering it with the interface document would turn a typo in a route
// into a 200 that no API client can parse.
func TestWebUIDoesNotAnswerAPIPaths(t *testing.T) {
	api := webUIServer(true)
	for _, path := range []string{"/v1/bogus", "/v1/", "/mcp", "/health"} {
		rec := httptest.NewRecorder()
		api.handleWebUI(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "<html") {
			t.Errorf("%s: answered with HTML: %s", path, rec.Body.String())
		}
	}
}

// The interface routes in the browser, so a reload on one of its own routes
// has to return the document rather than 404.
func TestWebUIFallsBackToTheDocumentForItsOwnRoutes(t *testing.T) {
	api := webUIServer(true)
	rec := httptest.NewRecorder()
	api.handleWebUI(rec, httptest.NewRequest(http.MethodGet, "/c/some-session", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<div id=\"root\"") {
		t.Fatalf("body is not the interface document: %s", rec.Body.String())
	}
}

// A session id comes from a caller and may contain a dot. The rule that reports
// a missing file rather than the document reads a dot as an extension, so a
// conversation route has to be decided before that rule runs.
func TestWebUIServesAConversationWhoseSessionIDLooksLikeAFileName(t *testing.T) {
	api := webUIServer(true)
	for _, path := range []string{"/c/agent.v2.session", "/c/report.json"} {
		rec := httptest.NewRecorder()
		api.handleWebUI(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<div id=\"root\"") {
			t.Fatalf("%s: body is not the interface document", path)
		}
	}
}

// A missing script must be reported as missing. Serving the document in its
// place would make the browser parse HTML as JavaScript and fail with a
// message that points nowhere near the real cause.
func TestWebUIReportsAMissingAssetInsteadOfTheDocument(t *testing.T) {
	api := webUIServer(true)
	rec := httptest.NewRecorder()
	api.handleWebUI(rec, httptest.NewRequest(http.MethodGet, "/assets/absent.js", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body %s", rec.Code, rec.Body.String())
	}
}

func TestWebUIAnswersARevalidationWith304(t *testing.T) {
	api := webUIServer(true)

	first := httptest.NewRecorder()
	api.handleWebUI(first, httptest.NewRequest(http.MethodGet, "/", nil))
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
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("If-None-Match", header)
		api.handleWebUI(rec, req)

		if rec.Code != http.StatusNotModified {
			t.Errorf("%s: status = %d, want 304", name, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("%s: a 304 carried a body of %d bytes", name, rec.Body.Len())
		}
	}

	stale := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", `"stale"`)
	api.handleWebUI(stale, req)
	if stale.Code != http.StatusOK {
		t.Errorf("a stale validator got %d, want a fresh 200", stale.Code)
	}
}

// The document names the hashed asset files, so holding it would keep serving
// the previous build after a deploy.
func TestWebUIRevalidatesTheDocumentButHoldsHashedAssets(t *testing.T) {
	if got := cacheControlFor("/index.html"); got != "no-cache" {
		t.Errorf("document Cache-Control = %q, want no-cache", got)
	}
	if got := cacheControlFor("/"); got != "no-cache" {
		t.Errorf("root Cache-Control = %q, want no-cache", got)
	}
	if got := cacheControlFor("/assets/main-a1b2c3.js"); !strings.Contains(got, "immutable") {
		t.Errorf("asset Cache-Control = %q, want immutable", got)
	}
}

// aHashedAssetPath returns one embedded build output, whose file name carries
// a content hash.
func aHashedAssetPath(t *testing.T) string {
	t.Helper()
	lookupAsset("/")
	for name := range assetsByID {
		if strings.HasPrefix(name, "/assets/") {
			return name
		}
	}
	t.Fatal("the embedded interface carries no hashed asset")
	return ""
}

// A hashed name changes whenever the bytes change, so a validator on one of
// these files can only ever confirm the copy the client already holds.
func TestWebUISendsNoValidatorForAnImmutableAsset(t *testing.T) {
	api := webUIServer(true)
	path := aHashedAssetPath(t)

	rec := httptest.NewRecorder()
	api.handleWebUI(rec, httptest.NewRequest(http.MethodGet, path, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d", path, rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("%s: Cache-Control = %q, want immutable", path, got)
	}
	if got := rec.Header().Get("ETag"); got != "" {
		t.Errorf("%s: an immutable asset carried a validator: %s", path, got)
	}
}

func TestWebUIIsAbsentWhenDisabled(t *testing.T) {
	api := webUIServer(false)
	rec := httptest.NewRecorder()
	api.handleWebUI(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestWebUIRejectsWritingMethods(t *testing.T) {
	api := webUIServer(true)
	rec := httptest.NewRecorder()
	api.handleWebUI(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// A request path is caller-controlled, so it must not be able to climb out of
// the embedded set.
func TestWebUIRejectsPathTraversal(t *testing.T) {
	api := webUIServer(true)
	for _, path := range []string{"/../go.mod", "/assets/../../go.mod", "/./../../data/.env"} {
		rec := httptest.NewRecorder()
		api.handleWebUI(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if strings.Contains(rec.Body.String(), "module github.com") {
			t.Fatalf("%s: served a file from outside the interface", path)
		}
	}
}

func TestWebUIHeadSendsHeadersWithoutABody(t *testing.T) {
	api := webUIServer(true)
	rec := httptest.NewRecorder()
	api.handleWebUI(rec, httptest.NewRequest(http.MethodHead, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("HEAD sent no ETag")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned %d bytes of body", rec.Body.Len())
	}
}
