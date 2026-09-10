// Package auth provides SSO cookie-based re-authentication as a fallback
// when the refresh token expires (AADSTS700084).
// SSO cookies (ESTSAUTH, ESTSAUTHPERSISTENT) on login.microsoftonline.com
// last weeks/months, unlike SPA refresh tokens which expire after 24 hours.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/atomicfile"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/crypto"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/textcut"
)

const (
	// ssoCookiesFile is the encrypted SSO cookie store.
	ssoCookiesFile = "data/tokens/sso_cookies.json"
	// m365CookiesFile is the browser-exported M365 web cookie store.
	m365CookiesFile = "data/tokens/m365_cookies.json"
	// authorizeURLTemplate is the OAuth2 authorize endpoint for silent re-auth.
	authorizeURLTemplate = "https://login.microsoftonline.com/%s/oauth2/v2.0/authorize"
	// defaultRedirectURI is the redirect URI registered for the M365 Copilot SPA app.
	defaultRedirectURI = "https://m365.cloud.microsoft/spalanding"
)

// ErrM365CookiesUnavailable indicates that no cookies for the M365 web app are stored.
var ErrM365CookiesUnavailable = errors.New("M365 web app cookies unavailable")

// SSOCookie represents a browser cookie used by M365 authentication or web APIs.
type SSOCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Path     string `json:"path,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Secure   bool   `json:"secure,omitempty"`
	HttpOnly bool   `json:"httpOnly,omitempty"`
}

// SSOCookieStore holds all SSO cookies needed for silent re-authentication.
type SSOCookieStore struct {
	Cookies    []SSOCookie `json:"cookies"`
	CapturedAt time.Time   `json:"capturedAt"`
}

// m365CookieStore holds browser cookies used by M365 web APIs.
type m365CookieStore struct {
	Domain      string      `json:"domain"`
	ExtractedAt time.Time   `json:"extracted_at"`
	Cookies     []SSOCookie `json:"cookies"`
}

// generatePKCE creates a PKCE code verifier and code challenge (S256).
func generatePKCE() (verifier, challenge string, err error) {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return "", "", fmt.Errorf("failed to generate code verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(verifierBytes)

	hash := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(hash[:])

	return verifier, challenge, nil
}

// SaveSSOCookies encrypts and stores SSO cookies to disk.
func SaveSSOCookies(cookies []SSOCookie) error {
	store := SSOCookieStore{
		Cookies:    cookies,
		CapturedAt: time.Now(),
	}

	data, err := json.Marshal(store)
	if err != nil {
		return fmt.Errorf("failed to marshal SSO cookies: %w", err)
	}

	encrypted, err := crypto.Encrypt(string(data))
	if err != nil {
		return fmt.Errorf("failed to encrypt SSO cookies: %w", err)
	}

	dir := filepath.Dir(ssoCookiesFile)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("failed to create directory: %w", err)
		}
	}

	return atomicWriteFile(ssoCookiesFile, []byte(encrypted), 0600)
}

// SaveM365Cookies encrypts and stores browser cookies used by M365 web APIs.
func SaveM365Cookies(cookies []SSOCookie) error {
	store := m365CookieStore{
		Domain:      "m365.cloud.microsoft",
		ExtractedAt: time.Now(),
		Cookies:     cookies,
	}
	return saveM365CookieStore(store)
}

func saveM365CookieStore(store m365CookieStore) error {
	data, err := json.Marshal(store)
	if err != nil {
		return fmt.Errorf("failed to marshal M365 cookies: %w", err)
	}
	encrypted, err := crypto.Encrypt(string(data))
	if err != nil {
		return fmt.Errorf("failed to encrypt M365 cookies: %w", err)
	}
	if err := atomicWriteFile(m365CookiesFile, []byte(encrypted), 0600); err != nil {
		return fmt.Errorf("failed to save M365 cookies: %w", err)
	}
	return nil
}

// atomicWriteFile writes a credential file through pkg/atomicfile, so a crash
// in the middle of a write never leaves a shorter file that still decrypts to
// nothing useful.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	return atomicfile.Write(path, data, mode)
}

// loadSSOCookies reads and decrypts SSO cookies from disk.
func (tm *TokenManager) loadSSOCookies() (*SSOCookieStore, error) {
	data, err := os.ReadFile(ssoCookiesFile)
	if err != nil {
		return nil, fmt.Errorf("SSO cookies file not found: %w", err)
	}

	decrypted, err := crypto.Decrypt(string(data))
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt SSO cookies: %w", err)
	}

	var store SSOCookieStore
	if err := json.Unmarshal([]byte(decrypted), &store); err != nil {
		return nil, fmt.Errorf("failed to parse SSO cookies: %w", err)
	}

	return &store, nil
}

// hasSSOCookies checks if SSO cookies are available on disk.
func hasSSOCookies() bool {
	_, err := os.Stat(ssoCookiesFile)
	return err == nil
}

func loadM365CookieStore() (*m365CookieStore, error) {
	data, err := os.ReadFile(m365CookiesFile)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrM365CookiesUnavailable, err)
	}

	decrypted, decryptErr := crypto.Decrypt(string(data))
	if decryptErr == nil {
		var store m365CookieStore
		if err := json.Unmarshal([]byte(decrypted), &store); err != nil {
			return nil, fmt.Errorf("%w: failed to parse decrypted M365 cookies: %v", ErrM365CookiesUnavailable, err)
		}
		return &store, nil
	}

	var legacyData struct {
		Domain  string      `json:"domain"`
		Cookies []SSOCookie `json:"cookies"`
	}
	if err := json.Unmarshal(data, &legacyData); err != nil {
		return nil, fmt.Errorf("%w: failed to decrypt M365 cookies: %v", ErrM365CookiesUnavailable, decryptErr)
	}
	if legacyData.Domain == "" || legacyData.Cookies == nil {
		return nil, fmt.Errorf("%w: invalid legacy M365 cookie store", ErrM365CookiesUnavailable)
	}
	legacyStore := m365CookieStore{
		Domain:      legacyData.Domain,
		ExtractedAt: time.Now(),
		Cookies:     legacyData.Cookies,
	}
	if err := saveM365CookieStore(legacyStore); err != nil {
		return nil, fmt.Errorf("%w: failed to migrate M365 cookies: %v", ErrM365CookiesUnavailable, err)
	}
	return &legacyStore, nil
}

// M365CookieHeader returns cookies scoped to the M365 web application.
func (tm *TokenManager) M365CookieHeader() (string, error) {
	store, err := loadM365CookieStore()
	if err != nil {
		return "", err
	}

	var cookieParts []string
	for _, cookie := range store.Cookies {
		domain := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cookie.Domain)), ".")
		if domain != "m365.cloud.microsoft" && domain != "microsoft.com" {
			continue
		}
		if cookie.Name == "" || cookie.Value == "" {
			continue
		}
		cookieParts = append(cookieParts, cookie.Name+"="+cookie.Value)
	}
	if len(cookieParts) == 0 {
		return "", ErrM365CookiesUnavailable
	}

	return strings.Join(cookieParts, "; "), nil
}

// cookieHeaderFrom renders stored SSO cookies as one Cookie header.
func cookieHeaderFrom(cookies []SSOCookie) string {
	var parts []string
	for _, c := range cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// ssoBrowserRequest builds a GET the sign-in flow accepts: the SSO cookies plus
// the browser headers the authorize endpoint expects.
func ssoBrowserRequest(target, cookieHeader string) (*http.Request, error) {
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://m365.cloud.microsoft/")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Cookie", cookieHeader)
	return req, nil
}

// authorizeTarget builds the silent authorize URL.
//
// sso_reload=True tells the server to use the SSO cookies and skip the
// BssoInterrupt page. prompt=none breaks SSO cookie recognition, so it is
// omitted.
func (tm *TokenManager) authorizeTarget(challenge string) string {
	params := url.Values{
		"client_id":             {tm.clientID},
		"response_type":         {"code"},
		"redirect_uri":          {defaultRedirectURI},
		"scope":                 {tm.scope + " offline_access"},
		"response_mode":         {"fragment"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"m365bridge-sso"},
		"sso_reload":            {"True"},
	}
	return fmt.Sprintf(authorizeURLTemplate, tm.tenant) + "?" + params.Encode()
}

// nextLocation reports where the sign-in flow goes next. With no Location
// header the page itself may carry a meta refresh, which is how the flow
// continues through an interstitial.
func nextLocation(resp *http.Response) (string, error) {
	if location := resp.Header.Get("Location"); location != "" {
		return location, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, authPageMax+1))
	if len(body) > authPageMax {
		logging.Warnf("sign-in page exceeds %d bytes; a meta refresh past the cap is not followed", authPageMax)
		body = body[:authPageMax]
	}
	bodyStr := string(body)
	if metaURL := extractMetaRefreshURL(bodyStr); metaURL != "" {
		return metaURL, nil
	}
	return "", fmt.Errorf("%w: no redirect from authorize (status %d): %s",
		ErrRefreshFailed, resp.StatusCode, textcut.Truncate(bodyStr, 2000))
}

// redirectOutcome is what one sign-in redirect carried. An empty Code with
// Failed unset means this redirect is not the last one.
type redirectOutcome struct {
	Code    string
	Failed  bool
	ErrCode string
	ErrDesc string
}

// redirectOutcomeFrom reads a redirect target. response_mode=fragment puts the
// authorization code in the fragment rather than the query, and reports a
// refused sign-in there too.
func redirectOutcomeFrom(location string) (redirectOutcome, error) {
	locURL, err := url.Parse(location)
	if err != nil {
		return redirectOutcome{}, err
	}
	if code := locURL.Query().Get("code"); code != "" {
		return redirectOutcome{Code: code}, nil
	}
	if locURL.Fragment == "" {
		return redirectOutcome{}, nil
	}
	fragParams, _ := url.ParseQuery(locURL.Fragment)
	if code := fragParams.Get("code"); code != "" {
		return redirectOutcome{Code: code}, nil
	}
	return redirectOutcome{
		Failed:  true,
		ErrCode: fragParams.Get("error"),
		ErrDesc: fragParams.Get("error_description"),
	}, nil
}

// authCodeFromRedirect reads the authorization code of the SSO sign-in flow.
func authCodeFromRedirect(location string) (string, error) {
	outcome, err := redirectOutcomeFrom(location)
	if err != nil {
		return "", fmt.Errorf("%w: failed to parse redirect URL: %v", ErrRefreshFailed, err)
	}
	if outcome.Failed {
		return "", fmt.Errorf("%w: authorize returned error: %s: %s", ErrRefreshFailed, outcome.ErrCode, outcome.ErrDesc)
	}
	return outcome.Code, nil
}

// brokerAuthCodeFromRedirect reads the authorization code of the broker sign-in
// flow, which reports its failures on its own contract.
func brokerAuthCodeFromRedirect(location string) (string, error) {
	outcome, err := redirectOutcomeFrom(location)
	if err != nil {
		return "", fmt.Errorf("failed to parse broker redirect URL: %w", err)
	}
	if outcome.Failed {
		return "", fmt.Errorf("broker authorize error: %s: %s", outcome.ErrCode, outcome.ErrDesc)
	}
	return outcome.Code, nil
}

// followSSORedirects walks the sign-in redirects by hand until one carries the
// authorization code. The client is configured not to follow them itself,
// because the code arrives on a redirect that is never fetched.
func followSSORedirects(client *http.Client, resp *http.Response, cookieHeader string) (string, error) {
	current := resp
	for {
		location, err := nextLocation(current)
		if err != nil {
			return "", err
		}

		if strings.Contains(location, "m365.cloud.microsoft") {
			authCode, codeErr := authCodeFromRedirect(location)
			if codeErr != nil {
				return "", codeErr
			}
			if authCode != "" {
				return authCode, nil
			}
		}

		redirectReq, err := ssoBrowserRequest(location, cookieHeader)
		if err != nil {
			return "", fmt.Errorf("%w: failed to create redirect request: %v", ErrRefreshFailed, err)
		}
		_ = current.Body.Close()
		current, err = client.Do(redirectReq)
		if err != nil {
			return "", fmt.Errorf("%w: redirect request failed: %v", ErrRefreshFailed, err)
		}
		defer func() { _ = current.Body.Close() }()
	}
}

// reauthWithSSO performs silent re-authentication using stored SSO cookies.
// It uses the OAuth2 authorize endpoint with prompt=none and PKCE.
// If the SSO session is still valid, it returns new access and refresh tokens.
func (tm *TokenManager) reauthWithSSO() (string, error) {
	logging.Info("reauthWithSSO: starting SSO cookie re-authentication")
	store, err := tm.loadSSOCookies()
	if err != nil {
		logging.Errorf("reauthWithSSO: no SSO cookies available: %v", err)
		return "", fmt.Errorf("%w: no SSO cookies available: %v", ErrRefreshFailed, err)
	}

	logging.Debugf("reauthWithSSO: loaded %d SSO cookies captured at %s", len(store.Cookies), store.CapturedAt.Format(time.RFC3339))
	cookieHeader := cookieHeaderFrom(store.Cookies)

	client := &http.Client{
		// Don't follow redirects automatically; we need to capture the auth code
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 15 * time.Second,
	}

	// Generate PKCE
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRefreshFailed, err)
	}

	authReq, err := ssoBrowserRequest(tm.authorizeTarget(challenge), cookieHeader)
	if err != nil {
		return "", fmt.Errorf("%w: failed to create authorize request: %v", ErrRefreshFailed, err)
	}

	authResp, err := client.Do(authReq)
	if err != nil {
		return "", fmt.Errorf("%w: authorize request failed: %v", ErrRefreshFailed, err)
	}
	defer func() { _ = authResp.Body.Close() }()

	authCode, err := followSSORedirects(client, authResp, cookieHeader)
	if err != nil {
		return "", err
	}

	// Exchange auth code for tokens
	logging.Info("reauthWithSSO: obtained auth code, exchanging for tokens")
	return tm.exchangeAuthCode(authCode, verifier)
}

// extractMetaRefreshURL parses an HTML body and extracts the URL from a
// <meta http-equiv="refresh" content="0; url=..."> tag. Returns empty string if not found.
func extractMetaRefreshURL(html string) string {
	// Find meta refresh tag
	idx := strings.Index(strings.ToLower(html), "http-equiv=\"refresh\"")
	if idx == -1 {
		idx = strings.Index(strings.ToLower(html), "http-equiv='refresh'")
	}
	if idx == -1 {
		return ""
	}

	// Find the content attribute after this position
	rest := html[idx:]
	contentIdx := strings.Index(strings.ToLower(rest), "content=\"")
	if contentIdx == -1 {
		contentIdx = strings.Index(strings.ToLower(rest), "content='")
	}
	if contentIdx == -1 {
		return ""
	}

	// Extract the content value
	rest = rest[contentIdx+9:]
	endIdx := strings.Index(rest, "\"")
	if endIdx == -1 {
		endIdx = strings.Index(rest, "'")
	}
	if endIdx == -1 {
		return ""
	}

	content := rest[:endIdx]
	// Parse "0; url=..." format
	urlIdx := strings.Index(strings.ToLower(content), "url=")
	if urlIdx == -1 {
		return ""
	}

	return strings.TrimSpace(content[urlIdx+4:])
}

func summarizeBrokerAuthorizeResponse(body string) string {
	const aadSTSMarker = "AADSTS"
	if start := strings.Index(body, aadSTSMarker); start >= 0 {
		details := body[start:]
		if end := strings.IndexAny(details, "<\r\n"); end >= 0 {
			details = details[:end]
		}
		if details = strings.TrimSpace(html.UnescapeString(details)); details != "" {
			return details
		}
	}

	lowerBody := strings.ToLower(body)
	if titleStart := strings.Index(lowerBody, "<title>"); titleStart >= 0 {
		contentStart := titleStart + len("<title>")
		if titleEnd := strings.Index(lowerBody[contentStart:], "</title>"); titleEnd >= 0 {
			title := strings.TrimSpace(html.UnescapeString(body[contentStart : contentStart+titleEnd]))
			if title != "" {
				return "page title: " + title
			}
		}
	}

	compactBody := strings.Join(strings.Fields(body), " ")
	return textcut.Truncate(compactBody, 300)
}

// exchangeAuthCode exchanges an authorization code for access and refresh tokens.
func (tm *TokenManager) exchangeAuthCode(authCode, verifier string) (string, error) {
	tokenData := url.Values{
		"client_id":     {tm.clientID},
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"redirect_uri":  {defaultRedirectURI},
		"code_verifier": {verifier},
		"scope":         {tm.scope + " offline_access"},
	}

	tokenReq, err := http.NewRequest("POST", tm.tokenURL, strings.NewReader(tokenData.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: failed to create token request: %v", ErrRefreshFailed, err)
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	tokenReq.Header.Set("Origin", "https://m365.cloud.microsoft")
	tokenReq.Header.Set("Referer", "https://m365.cloud.microsoft/")
	tokenReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
	tokenReq.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	tokenResp, err := client.Do(tokenReq)
	if err != nil {
		return "", fmt.Errorf("%w: token exchange failed: %v", ErrRefreshFailed, err)
	}
	defer func() { _ = tokenResp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(tokenResp.Body, tokenResponseMax+1))
	if err != nil {
		return "", fmt.Errorf("%w: failed to read token response: %v", ErrRefreshFailed, err)
	}
	if len(body) > tokenResponseMax {
		return "", fmt.Errorf("%w: token response exceeds %d bytes", ErrRefreshFailed, tokenResponseMax)
	}

	if tokenResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: token exchange status %d: %s", ErrRefreshFailed, tokenResp.StatusCode, string(body))
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("%w: failed to parse token response: %v", ErrRefreshFailed, err)
	}

	// Save new refresh token if provided
	if result.RefreshToken != "" {
		if err := tm.writeRefreshToken(result.RefreshToken); err != nil {
			return "", fmt.Errorf("%w: failed to save refresh token: %v", ErrRefreshFailed, err)
		}
	}

	// Cache access token
	expiresAt := time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	cache := TokenCache{
		AccessToken: result.AccessToken,
		ExpiresAt:   expiresAt.Unix(),
	}

	if err := tm.writeCache(cache); err != nil {
		return "", fmt.Errorf("%w: failed to write cache: %v", ErrRefreshFailed, err)
	}

	logging.Infof("exchangeAuthCode: success, expires_in=%d", result.ExpiresIn)
	return result.AccessToken, nil
}

// designerTokenCacheFile stores the designerapp access token cache. The value
// is where a credential is stored, not a credential.
// #nosec G101
const designerTokenCacheFile = "data/tokens/designer_token_cache.json"

// designerBrokerRefreshFile stores the broker-compatible refresh token.
// This refresh token is obtained via SSO cookie broker authorize flow and
// is separate from the standard refresh token (rt_90day.txt) because the
// broker flow requires a refresh token issued for brk-multihub:// redirect URI.
const designerBrokerRefreshFile = "data/tokens/rt_broker.txt"

// designerClientID is the M365 web app client_id used for designerapp tokens.
// This is the "brokered" app that the broker (c0ab8ce9) acquires tokens on
// behalf of.
const designerClientID = "4765445b-32c6-49b0-83e6-1d93765276ca"

// designerBrokerScope is the OAuth2 scope for the broker token request.
// The .default scope for the designerappservice resource is what MSAL.js
// uses when acquiring tokens for image downloads.
const designerBrokerScope = "https://designerappservice.officeapps.live.com/.default openid profile offline_access"

// designerBrokerRedirectURI is the redirect URI used in the broker token
// request body. MSAL.js uses the brk-multihub scheme for brokered flows.
const designerBrokerRedirectURI = "brk-multihub://outlook.office.com"

// brokerClientID is the broker app client_id used in broker token requests.
// This is always c0ab8ce9 (the M365 Copilot broker app), regardless of the
// configured M365_CLIENT_ID, because the broker flow requires the broker
// app's client_id to acquire tokens on behalf of the brokered app (4765445b).
const brokerClientID = "c0ab8ce9-e9a0-42e7-b064-33d422df41f1"

// designerTokenCache is the on-disk cache for the designerapp access token.
type designerTokenCache struct {
	AccessToken string `json:"access_token"`
	ExpiresAt   int64  `json:"expires_at"`
}

// designerOAuthError represents an OAuth error returned by the broker token endpoint.
type designerOAuthError struct {
	Status      int
	Code        string
	Description string
}

// Error returns the broker OAuth error details.
func (e *designerOAuthError) Error() string {
	return fmt.Sprintf("designer broker token status %d: %s: %s", e.Status, e.Code, e.Description)
}

// isExpiredRefreshToken reports whether the broker refresh token must be replaced.
func (e *designerOAuthError) isExpiredRefreshToken() bool {
	return e.Code == "invalid_grant" && strings.Contains(e.Description, "AADSTS700084")
}

// GetDesignerToken returns a valid designerapp access token, acquiring a new
// one via the broker refresh token flow if the cached token is expired or
// missing. Falls back to SSO cookie broker authorize flow if no broker
// refresh token is available.
func (tm *TokenManager) GetDesignerToken() (string, error) {
	// Check cache first
	if token, ok := readDesignerTokenCache(); ok {
		logging.Debug("GetDesignerToken: cache hit")
		return token, nil
	}

	// The broker refresh token is single-use like the primary one, so only one
	// goroutine may acquire at a time.
	tm.designerMu.Lock()
	defer tm.designerMu.Unlock()

	// Re-check under the lock; a concurrent caller may have just filled it.
	if token, ok := readDesignerTokenCache(); ok {
		logging.Debug("GetDesignerToken: cache filled while waiting for designer lock")
		return token, nil
	}

	logging.Info("GetDesignerToken: cache miss, acquiring new token")
	// Acquire new token via broker refresh token flow
	token, expiresIn, err := tm.acquireDesignerToken()
	if err != nil {
		logging.Errorf("GetDesignerToken: failed to acquire token: %v", err)
		return "", err
	}

	// Cache it
	cache := designerTokenCache{
		AccessToken: token,
		ExpiresAt:   time.Now().Add(time.Duration(expiresIn) * time.Second).Unix(),
	}
	// The cache holds the designer access token by design; caching it is the
	// whole point of the file. It lives under the gitignored data/ tree and is
	// written 0600.
	// #nosec G117
	cacheData, _ := json.Marshal(cache)
	if err := atomicWriteFile(designerTokenCacheFile, cacheData, 0600); err != nil {
		logging.Errorf("GetDesignerToken: failed to write cache: %v", err)
	}

	logging.Infof("GetDesignerToken: success, expires_in=%d", expiresIn)
	return token, nil
}

// readDesignerTokenCache returns the cached designerapp token when it is still
// valid for at least 60 more seconds.
func readDesignerTokenCache() (string, bool) {
	data, err := os.ReadFile(designerTokenCacheFile)
	if err != nil {
		return "", false
	}
	var cache designerTokenCache
	if json.Unmarshal(data, &cache) != nil {
		return "", false
	}
	if time.Now().Unix() >= cache.ExpiresAt-60 {
		return "", false
	}
	return cache.AccessToken, true
}

// acquireDesignerToken performs a broker refresh token request to obtain a
// JWE access token for designerapp.officeapps.live.com image downloads.
// Uses a broker-compatible refresh token (stored in rt_broker.txt). If none
// exists, falls back to SSO cookie broker authorize flow to obtain one.
func (tm *TokenManager) acquireDesignerToken() (string, int, error) {
	requestToken := tm.requestDesignerToken
	if tm.designerTokenRequest != nil {
		requestToken = tm.designerTokenRequest
	}
	acquireBrokerToken := tm.acquireBrokerRefreshTokenViaSSO
	if tm.brokerTokenAcquisition != nil {
		acquireBrokerToken = tm.brokerTokenAcquisition
	}

	refreshToken, err := tm.readBrokerRefreshToken()
	if err != nil {
		logging.Info("acquireDesignerToken: no broker refresh token, acquiring via SSO cookies")
		refreshToken, err = acquireBrokerToken()
		if err != nil {
			logging.Errorf("acquireDesignerToken: failed to acquire broker refresh token: %v", err)
			return "", 0, fmt.Errorf("failed to acquire broker refresh token: %w", err)
		}
		return requestToken(refreshToken)
	}

	logging.Debug("acquireDesignerToken: using existing broker refresh token")
	token, expiresIn, err := requestToken(refreshToken)
	var oauthErr *designerOAuthError
	if !errors.As(err, &oauthErr) || !oauthErr.isExpiredRefreshToken() {
		return token, expiresIn, err
	}

	logging.Warn("acquireDesignerToken: broker refresh token expired, acquiring a new token via SSO cookies")
	refreshToken, err = acquireBrokerToken()
	if err != nil {
		return "", 0, fmt.Errorf("failed to reacquire expired broker refresh token: %w", err)
	}

	return requestToken(refreshToken)
}

// buildDesignerTokenRequest builds the broker token exchange, which is the
// MSAL.js broker flow rather than the plain refresh_token grant the standard
// token uses.
func (tm *TokenManager) buildDesignerTokenRequest(refreshToken string) (*http.Request, error) {
	// Build the broker token URL with query parameters
	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token?brk_client_id=%s&brk_redirect_uri=%s&client_id=%s&client-request-id=%s",
		tm.tenant,
		designerClientID,
		url.QueryEscape(defaultRedirectURI),
		brokerClientID,
		generateClientRequestID(),
	)

	// Build the request body matching MSAL.js broker flow
	body := url.Values{
		"client_id":                  {brokerClientID},
		"redirect_uri":               {designerBrokerRedirectURI},
		"scope":                      {designerBrokerScope},
		"grant_type":                 {"refresh_token"},
		"client_info":                {"1"},
		"x-client-SKU":               {"msal.js.browser"},
		"x-client-VER":               {"5.9.0"},
		"x-ms-lib-capability":        {"retry-after, h429"},
		"x-client-current-telemetry": {"5|61,0,,,|,"},
		"x-client-last-telemetry":    {"5|0|||0,0"},
		"refresh_token":              {refreshToken},
	}

	// X-AnchorMailbox helps AAD route the request to the correct token service
	if tm.userOID != "" {
		body.Set("X-AnchorMailbox", fmt.Sprintf("Oid:%s@%s", tm.userOID, tm.tenant))
	}

	// brk_ params go in both URL query and body (MSAL.js sends them in both)
	body.Set("brk_client_id", designerClientID)
	body.Set("brk_redirect_uri", defaultRedirectURI)

	req, err := http.NewRequest("POST", tokenURL, strings.NewReader(body.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create designer broker token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("Origin", "https://m365.cloud.microsoft")
	req.Header.Set("Referer", "https://m365.cloud.microsoft/")
	return req, nil
}

// designerTokenFailure reads a non-200 from the broker token endpoint. An OAuth
// error body becomes a designerOAuthError, which the caller acts on; anything
// else is reported as the status it was.
func designerTokenFailure(status int, respBody []byte) error {
	var oauthResult struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(respBody, &oauthResult); err == nil && oauthResult.Error != "" {
		return &designerOAuthError{
			Status:      status,
			Code:        oauthResult.Error,
			Description: oauthResult.ErrorDescription,
		}
	}
	return fmt.Errorf("designer broker token status %d: %s", status, string(respBody)[:min(300, len(respBody))])
}

// requestDesignerToken exchanges a broker refresh token for a designer access token.
func (tm *TokenManager) requestDesignerToken(refreshToken string) (string, int, error) {
	req, err := tm.buildDesignerTokenRequest(refreshToken)
	if err != nil {
		return "", 0, err
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("designer broker token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, tokenResponseMax+1))
	if err != nil {
		return "", 0, fmt.Errorf("failed to read designer broker token response: %w", err)
	}
	if len(respBody) > tokenResponseMax {
		return "", 0, fmt.Errorf("designer broker token response exceeds %d bytes", tokenResponseMax)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, designerTokenFailure(resp.StatusCode, respBody)
	}

	var result refreshResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", 0, fmt.Errorf("failed to parse designer broker token response: %w", err)
	}

	// Save rotated broker refresh token if returned
	if result.RefreshToken != "" {
		if err := tm.writeBrokerRefreshToken(result.RefreshToken); err != nil {
			return "", 0, fmt.Errorf("failed to save rotated broker refresh token: %w", err)
		}
	}

	return result.AccessToken, result.ExpiresIn, nil
}

// acquireBrokerRefreshTokenViaSSO performs a broker authorize flow using SSO
// cookies to obtain a broker-compatible refresh token. This is needed because
// the standard refresh token (issued for spalanding redirect URI) is not
// compatible with the broker flow (which requires brk-multihub:// redirect URI).
func (tm *TokenManager) acquireBrokerRefreshTokenViaSSO() (string, error) {
	logging.Info("acquireBrokerRefreshTokenViaSSO: starting broker authorize flow via SSO cookies")
	store, err := tm.loadSSOCookies()
	if err != nil {
		logging.Errorf("acquireBrokerRefreshTokenViaSSO: no SSO cookies: %v", err)
		return "", fmt.Errorf("no SSO cookies for broker authorize: %w", err)
	}
	cookieHeader := cookieHeaderFrom(store.Cookies)

	verifier, challenge, err := generatePKCE()
	if err != nil {
		return "", fmt.Errorf("PKCE failed: %w", err)
	}

	authReq, err := brokerBrowserRequest(tm.brokerAuthorizeTarget(challenge), "", cookieHeader)
	if err != nil {
		return "", fmt.Errorf("failed to create broker authorize request: %w", err)
	}

	httpClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 15 * time.Second,
	}

	currentResp, err := httpClient.Do(authReq)
	if err != nil && !strings.Contains(err.Error(), "ErrUseLastResponse") {
		return "", fmt.Errorf("broker authorize request failed: %w", err)
	}
	defer func() { _ = currentResp.Body.Close() }()

	authCode, err := followBrokerRedirects(httpClient, currentResp, cookieHeader)
	if err != nil {
		return "", err
	}

	logging.Info("acquireBrokerRefreshTokenViaSSO: obtained auth code, exchanging for broker tokens")
	return tm.exchangeBrokerAuthCode(authCode, verifier)
}

// brokerAuthorizeTarget builds the broker authorize URL with PKCE and the brk_
// parameters. The broker flow needs the brk-multihub:// redirect URI, which is
// why the standard token cannot be reused for it.
func (tm *TokenManager) brokerAuthorizeTarget(challenge string) string {
	params := url.Values{
		"client_id":             {brokerClientID},
		"response_type":         {"code"},
		"redirect_uri":          {designerBrokerRedirectURI},
		"scope":                 {designerBrokerScope},
		"response_mode":         {"fragment"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"brk_client_id":         {designerClientID},
		"brk_redirect_uri":      {defaultRedirectURI},
		"sso_reload":            {"True"},
	}
	return fmt.Sprintf(authorizeURLTemplate, tm.tenant) + "?" + params.Encode()
}

// brokerBrowserRequest builds a GET the broker sign-in flow accepts. referer is
// empty on the authorize request and names the sign-in host on each redirect,
// which is what the flow itself sends.
func brokerBrowserRequest(target, referer, cookieHeader string) (*http.Request, error) {
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	req.Header.Set("Cookie", cookieHeader)
	return req, nil
}

// brokerNextLocation reports where the broker sign-in flow goes next. AAD
// sometimes redirects with a meta refresh in the page rather than a header.
func brokerNextLocation(resp *http.Response, hop int) (string, error) {
	if location := resp.Header.Get("Location"); location != "" {
		return location, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, authPageMax+1))
	if len(body) > authPageMax {
		logging.Warnf("sign-in page exceeds %d bytes; a meta refresh past the cap is not followed", authPageMax)
		body = body[:authPageMax]
	}
	bodyStr := string(body)
	if metaURL := extractMetaRefreshURL(bodyStr); metaURL != "" {
		return metaURL, nil
	}
	return "", fmt.Errorf("no redirect from broker authorize (status %d, hop %d): %s",
		resp.StatusCode, hop, summarizeBrokerAuthorizeResponse(bodyStr))
}

// followBrokerRedirects walks the broker sign-in redirects until one carries
// the authorization code.
//
// Microsoft AAD may return intermediate redirects (e.g. /jsdisabled, /kmsi)
// before the final redirect to spalanding#code=..., especially in
// headless/Docker environments where JS is not available.
func followBrokerRedirects(client *http.Client, resp *http.Response, cookieHeader string) (string, error) {
	const maxRedirects = 10
	current := resp
	for i := range maxRedirects {
		location, err := brokerNextLocation(current, i)
		if err != nil {
			return "", err
		}
		logging.Debugf("acquireBrokerRefreshTokenViaSSO: redirect hop %d -> %s", i, location[:min(120, len(location))])

		// The final redirect is brk_redirect_uri, which is on the M365 host.
		if strings.Contains(location, "m365.cloud.microsoft") {
			authCode, codeErr := brokerAuthCodeFromRedirect(location)
			if codeErr != nil {
				return "", codeErr
			}
			if authCode != "" {
				return authCode, nil
			}
		}

		// Follow the redirect, forwarding SSO cookies
		redirectReq, err := brokerBrowserRequest(location, "https://login.microsoftonline.com/", cookieHeader)
		if err != nil {
			return "", fmt.Errorf("failed to create broker redirect request (hop %d): %w", i, err)
		}

		_ = current.Body.Close()
		current, err = client.Do(redirectReq)
		if err != nil && !strings.Contains(err.Error(), "ErrUseLastResponse") {
			return "", fmt.Errorf("broker redirect request failed (hop %d): %w", i, err)
		}
		defer func() { _ = current.Body.Close() }()
	}

	return "", fmt.Errorf("broker authorize: max redirects (%d) reached without obtaining auth code", maxRedirects)
}

// exchangeBrokerAuthCode exchanges an authorization code for a broker
// refresh token and designerapp access token.
func (tm *TokenManager) exchangeBrokerAuthCode(authCode, verifier string) (string, error) {
	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token?brk_client_id=%s&brk_redirect_uri=%s&client_id=%s&client-request-id=%s",
		tm.tenant,
		designerClientID,
		url.QueryEscape(defaultRedirectURI),
		brokerClientID,
		generateClientRequestID(),
	)

	body := url.Values{
		"client_id":        {brokerClientID},
		"redirect_uri":     {designerBrokerRedirectURI},
		"scope":            {designerBrokerScope},
		"grant_type":       {"authorization_code"},
		"code":             {authCode},
		"code_verifier":    {verifier},
		"client_info":      {"1"},
		"brk_client_id":    {designerClientID},
		"brk_redirect_uri": {defaultRedirectURI},
	}

	req, err := http.NewRequest("POST", tokenURL, strings.NewReader(body.Encode()))
	if err != nil {
		return "", fmt.Errorf("failed to create broker code exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("Origin", "https://m365.cloud.microsoft")
	req.Header.Set("Referer", "https://m365.cloud.microsoft/")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("broker code exchange failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, tokenResponseMax+1))
	if err != nil {
		return "", fmt.Errorf("failed to read broker code exchange response: %w", err)
	}
	if len(respBody) > tokenResponseMax {
		return "", fmt.Errorf("broker code exchange response exceeds %d bytes", tokenResponseMax)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("broker code exchange status %d: %s", resp.StatusCode, string(respBody)[:min(300, len(respBody))])
	}

	var result struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse broker code exchange response: %w", err)
	}

	if result.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token in broker code exchange response")
	}

	// Save broker refresh token
	if err := tm.writeBrokerRefreshToken(result.RefreshToken); err != nil {
		return "", fmt.Errorf("failed to save broker refresh token: %w", err)
	}

	logging.Info("exchangeBrokerAuthCode: success, broker refresh token saved")
	return result.RefreshToken, nil
}

// readBrokerRefreshToken reads and decrypts the broker refresh token from file.
func (tm *TokenManager) readBrokerRefreshToken() (string, error) {
	data, err := os.ReadFile(designerBrokerRefreshFile)
	if err != nil {
		return "", fmt.Errorf("broker refresh token not found: %w", err)
	}

	encrypted := string(data)
	if encrypted == "" {
		return "", fmt.Errorf("broker refresh token file is empty")
	}

	decrypted, err := crypto.Decrypt(encrypted)
	if err != nil {
		return encrypted, nil
	}

	return decrypted, nil
}

// writeBrokerRefreshToken encrypts and writes the broker refresh token to file.
func (tm *TokenManager) writeBrokerRefreshToken(token string) error {
	encrypted, err := crypto.Encrypt(token)
	if err != nil {
		return fmt.Errorf("failed to encrypt broker refresh token: %w", err)
	}

	dir := filepath.Dir(designerBrokerRefreshFile)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create directory for broker refresh token: %w", err)
	}

	return atomicWriteFile(designerBrokerRefreshFile, []byte(encrypted), 0600)
}

// generateClientRequestID generates a UUID for the client-request-id parameter.
func generateClientRequestID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
