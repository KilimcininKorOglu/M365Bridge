package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
)

// withEndpoint points the personalization calls at a test server and restores
// the real address afterwards.
func withEndpoint(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	previous := personalizationEndpoint
	personalizationEndpoint = server.URL + "/m365Copilot/PersonalizationUserFlags?variants=feature.EnablePersonalization"
	t.Cleanup(func() {
		personalizationEndpoint = previous
		server.Close()
	})
}

// testClient returns a client whose token manager answers from a cache file
// alone, so no test reaches Entra or reads a real credential. The cache is the
// regenerable plaintext one, and an unexpired entry is all Get needs.
func testClient(t *testing.T) *M365Client {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "token_cache.json")
	body, err := json.Marshal(auth.TokenCache{
		AccessToken: "test-access-token",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(cache, body, 0600); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	return &M365Client{
		tokenManager: auth.NewTokenManager("tid-2", "client", "scope", filepath.Join(t.TempDir(), "rt.txt"), cache),
	}
}

const flagsOn = `{"isMemoryEnabled":true,"isCustomInstructionEnabled":true,` +
	`"isPersonalizationEnabledByTenant":true,"isInsightsFromConversationHistoryEnabled":true,` +
	`"isM365GraphContentEnabled":false,"result":{"value":"Success"}}`

const flagsOff = `{"isMemoryEnabled":false,"isCustomInstructionEnabled":true,` +
	`"isPersonalizationEnabledByTenant":true,"isInsightsFromConversationHistoryEnabled":false,` +
	`"isM365GraphContentEnabled":false,"result":{"value":"Success"}}`

// capturedRequest is what the test endpoint saw.
type capturedRequest struct {
	Method   string
	Path     string
	Anchor   string
	Scenario string
	Auth     string
}

// captureRequest records the parts of a request this test asserts on.
func captureRequest(r *http.Request) capturedRequest {
	return capturedRequest{
		Method:   r.Method,
		Path:     r.URL.RequestURI(),
		Anchor:   r.Header.Get("X-AnchorMailbox"),
		Scenario: r.Header.Get("X-Scenario"),
		Auth:     r.Header.Get("Authorization"),
	}
}

func TestGetPersonalizationFlagsReadsEveryFlag(t *testing.T) {
	var got capturedRequest
	withEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		got = captureRequest(r)
		_, _ = io.WriteString(w, flagsOn)
	})

	flags, err := testClient(t).GetPersonalizationFlags("oid-1", "tid-2")
	if err != nil {
		t.Fatalf("GetPersonalizationFlags: %v", err)
	}

	assertPersonalizationRouting(t, got)
	assertFlagsOn(t, flags)
}

// assertPersonalizationRouting pins the request half of the read.
func assertPersonalizationRouting(t *testing.T, got capturedRequest) {
	t.Helper()
	if got.Method != http.MethodGet {
		t.Errorf("method = %q", got.Method)
	}
	// The variants query gates the endpoint's own feature, so it has to travel.
	if !strings.Contains(got.Path, "variants=feature.EnablePersonalization") {
		t.Errorf("path = %q, missing the variants query", got.Path)
	}
	// Without the routing headers the request reaches the wrong mailbox.
	if got.Anchor != "Oid:oid-1@tid-2" {
		t.Errorf("X-AnchorMailbox = %q", got.Anchor)
	}
	if got.Scenario != "OfficeWebIncludedCopilot" {
		t.Errorf("X-Scenario = %q", got.Scenario)
	}
	if !strings.HasPrefix(got.Auth, "Bearer ") {
		t.Errorf("Authorization = %q", got.Auth)
	}
}

// assertFlagsOn pins every flag the flagsOn body declares, so a field dropped
// from the struct fails here instead of quietly reading false.
func assertFlagsOn(t *testing.T, flags *PersonalizationFlags) {
	t.Helper()
	if !flags.MemoryEnabled || !flags.InsightsFromHistoryEnabled || !flags.CustomInstructionEnabled {
		t.Errorf("flags = %+v, want the enabled ones true", flags)
	}
	if flags.GraphContentEnabled {
		t.Errorf("GraphContentEnabled = true, want false")
	}
	if !flags.AllowedByTenant {
		t.Errorf("AllowedByTenant = false")
	}
}

// The POST answers Success with a body carrying no flags at all, so it reports
// that the request was accepted and not what the account now holds. Only the
// read that follows says that.
func TestSetMemoryEnabledVerifiesWithARead(t *testing.T) {
	var posts, gets int
	var gotBody map[string]any
	withEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &gotBody)
			_, _ = io.WriteString(w, `{"result":{"value":"Success"}}`)
			return
		}
		gets++
		_, _ = io.WriteString(w, flagsOff)
	})

	flags, err := testClient(t).SetMemoryEnabled("oid-1", "tid-2", false)
	if err != nil {
		t.Fatalf("SetMemoryEnabled: %v", err)
	}
	if posts != 1 || gets != 1 {
		t.Fatalf("posts=%d gets=%d, want one write verified by one read", posts, gets)
	}
	if enabled, ok := gotBody["isMemoryEnabled"].(bool); !ok || enabled {
		t.Fatalf("body = %v, want isMemoryEnabled false", gotBody)
	}
	// The request names isMemoryEnabled alone, because the backend moves the
	// insights flag with it.
	if _, sent := gotBody["isInsightsFromConversationHistoryEnabled"]; sent {
		t.Error("the request named the insights flag; the backend moves it on its own")
	}
	if flags.MemoryEnabled || flags.InsightsFromHistoryEnabled {
		t.Errorf("flags = %+v, want both off", flags)
	}
}

// A write the backend accepted but did not apply must be reported, not returned
// as success; the whole point of the read-back is to catch that.
func TestSetMemoryEnabledReportsASettingThatDidNotTake(t *testing.T) {
	withEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"result":{"value":"Success"}}`)
			return
		}
		_, _ = io.WriteString(w, flagsOn) // still on
	})

	flags, err := testClient(t).SetMemoryEnabled("oid-1", "tid-2", false)
	if err == nil {
		t.Fatal("a setting that did not take was reported as success")
	}
	if !strings.Contains(err.Error(), "did not take") {
		t.Errorf("error = %q, does not name the cause", err)
	}
	// The caller still gets the real state, so an interface can show it.
	if flags == nil || !flags.MemoryEnabled {
		t.Errorf("flags = %+v, want the state the account really reports", flags)
	}
}

func TestPersonalizationReportsAnUpstreamFailure(t *testing.T) {
	withEndpoint(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"no"}`)
	})

	_, err := testClient(t).GetPersonalizationFlags("oid-1", "tid-2")
	if err == nil {
		t.Fatal("a refused request was reported as success")
	}
	// The status has to survive, or classifyUpstreamError cannot tell a caller
	// what to do about it.
	if status, ok := UpstreamStatus(err); !ok || status != http.StatusForbidden {
		t.Fatalf("UpstreamStatus = %d, %v; want 403", status, ok)
	}
}

func TestPersonalizationRefusesAnIncompleteIdentity(t *testing.T) {
	if _, err := testClient(t).GetPersonalizationFlags("", "tid"); err == nil {
		t.Error("an empty OID was accepted")
	}
	if _, err := testClient(t).GetPersonalizationFlags("oid", ""); err == nil {
		t.Error("an empty tenant was accepted")
	}
}
