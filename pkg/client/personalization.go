package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
	"github.com/google/uuid"
)

// personalizationEndpoint reads and writes the account's Copilot
// personalization settings. It is the endpoint the M365 web client uses for its
// own settings screen rather than a documented public API, which is why every
// failure here is reported instead of being treated as a settings screen that
// simply has nothing to show.
//
// personalizationEndpoint is a var so a test can point it at an httptest server.
var personalizationEndpoint = "https://substrate.office.com/m365Copilot/PersonalizationUserFlags?variants=feature.EnablePersonalization"

// personalizationResponseMax caps the response. The body is a small flat object
// of booleans, so anything near this size is a redirected or hostile endpoint.
const personalizationResponseMax = 1 << 16

// personalizationTimeout bounds one settings request. It is a small call on the
// interface's path, not a chat turn, so it must not hold a page open.
const personalizationTimeout = 30 * time.Second

// PersonalizationFlags is what M365 Copilot remembers about an account and
// applies to every turn it serves, whatever conversation the turn belongs to.
type PersonalizationFlags struct {
	// MemoryEnabled lets Copilot keep what it learns about the account and
	// apply it to later conversations. It is the setting a caller of this
	// gateway cannot see and never asked for.
	MemoryEnabled bool `json:"isMemoryEnabled"`
	// InsightsFromHistoryEnabled lets it draw on earlier conversations. The
	// backend moves this with MemoryEnabled rather than holding it apart.
	InsightsFromHistoryEnabled bool `json:"isInsightsFromConversationHistoryEnabled"`
	// CustomInstructionEnabled is the account's saved custom instructions,
	// which are a separate setting from memory.
	CustomInstructionEnabled bool `json:"isCustomInstructionEnabled"`
	// GraphContentEnabled lets it reach the account's Microsoft Graph content.
	GraphContentEnabled bool `json:"isM365GraphContentEnabled"`
	// AllowedByTenant reports whether the tenant permits personalization at
	// all. When it is false the other flags cannot be changed.
	AllowedByTenant bool `json:"isPersonalizationEnabledByTenant"`
}

// personalizationRequest issues one settings request with the account-routing
// headers the M365 web client sends, and returns the flags it answered with.
func (c *M365Client) personalizationRequest(method, userOID, tenantID string, body []byte) (*PersonalizationFlags, error) {
	req, err := c.buildPersonalizationRequest(method, userOID, tenantID, body)
	if err != nil {
		return nil, err
	}

	resp, err := (&http.Client{Timeout: personalizationTimeout}).Do(req)
	if err != nil {
		return nil, &UpstreamError{Op: "personalization", Status: 0, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := readPersonalizationBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		logging.Errorf("personalization: %s failed status=%d body=%s",
			method, resp.StatusCode, string(respBody)[:min(300, len(respBody))])
		return nil, &UpstreamError{Op: "personalization", Status: resp.StatusCode, Err: errors.New(string(respBody))}
	}

	var flags PersonalizationFlags
	if err := json.Unmarshal(respBody, &flags); err != nil {
		return nil, fmt.Errorf("failed to parse personalization response: %w", err)
	}
	return &flags, nil
}

// buildPersonalizationRequest stamps the account-routing headers the M365 web
// client sends. The setting lives on the account, so the request must name
// which account it is for.
func (c *M365Client) buildPersonalizationRequest(method, userOID, tenantID string, body []byte) (*http.Request, error) {
	if userOID == "" || tenantID == "" {
		return nil, errors.New("personalization: user OID and tenant ID are required")
	}
	token, err := c.tokenManager.Get()
	if err != nil {
		return nil, fmt.Errorf("failed to get token: %w", err)
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, personalizationEndpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-us")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://m365.cloud.microsoft")
	req.Header.Set("Referer", "https://m365.cloud.microsoft/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
	req.Header.Set("X-AnchorMailbox", fmt.Sprintf("Oid:%s@%s", userOID, tenantID))
	req.Header.Set("X-ClientRequestId", uuid.New().String())
	req.Header.Set("X-RoutingParameter-SessionKey", userOID)
	req.Header.Set("X-Scenario", "OfficeWebIncludedCopilot")
	return req, nil
}

// readPersonalizationBody reads the response under a byte cap.
func readPersonalizationBody(resp *http.Response) ([]byte, error) {
	// One extra byte distinguishes "exactly at the limit" from "truncated".
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, personalizationResponseMax+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read personalization response: %w", err)
	}
	if len(respBody) > personalizationResponseMax {
		return nil, fmt.Errorf("personalization response exceeds %d bytes", personalizationResponseMax)
	}
	return respBody, nil
}

// GetPersonalizationFlags reports what Copilot is allowed to remember about the
// account.
func (c *M365Client) GetPersonalizationFlags(userOID, tenantID string) (*PersonalizationFlags, error) {
	return c.personalizationRequest(http.MethodGet, userOID, tenantID, nil)
}

// SetMemoryEnabled turns the account's Copilot memory on or off and returns the
// state the backend reports afterwards.
//
// The write is followed by a read, because the POST answers `Success` with a
// body that carries no flags at all: it reports that the request was accepted,
// not what the account now holds. Only the read says that.
//
// The request names `isMemoryEnabled` alone. Measured against the live backend,
// turning that off also turns off `isInsightsFromConversationHistoryEnabled`,
// so sending the second field would only risk disagreeing with the first.
func (c *M365Client) SetMemoryEnabled(userOID, tenantID string, enabled bool) (*PersonalizationFlags, error) {
	body, err := json.Marshal(map[string]bool{"isMemoryEnabled": enabled})
	if err != nil {
		return nil, err
	}
	if _, err := c.personalizationRequest(http.MethodPost, userOID, tenantID, body); err != nil {
		return nil, err
	}

	flags, err := c.GetPersonalizationFlags(userOID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("memory setting was sent but could not be verified: %w", err)
	}
	if flags.MemoryEnabled != enabled {
		return flags, fmt.Errorf("memory setting did not take: asked for %v, account reports %v", enabled, flags.MemoryEnabled)
	}
	logging.Infof("personalization: memory enabled=%v insights=%v", flags.MemoryEnabled, flags.InsightsFromHistoryEnabled)
	return flags, nil
}
