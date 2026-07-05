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
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/crypto"
	"golang.org/x/net/publicsuffix"
)

const (
	// ssoCookiesFile is the encrypted SSO cookie store.
	ssoCookiesFile = "data/tokens/sso_cookies.json"
	// authorizeURLTemplate is the OAuth2 authorize endpoint for silent re-auth.
	authorizeURLTemplate = "https://login.microsoftonline.com/%s/oauth2/v2.0/authorize"
	// defaultRedirectURI is the redirect URI registered for the M365 Copilot SPA app.
	defaultRedirectURI = "https://m365.cloud.microsoft/landingv2"
)

// SSOCookie represents a single SSO cookie from login.microsoftonline.com.
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
	Cookies   []SSOCookie `json:"cookies"`
	CapturedAt time.Time  `json:"capturedAt"`
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
		Cookies:   cookies,
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

	return os.WriteFile(ssoCookiesFile, []byte(encrypted), 0600)
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

// reauthWithSSO performs silent re-authentication using stored SSO cookies.
// It uses the OAuth2 authorize endpoint with prompt=none and PKCE.
// If the SSO session is still valid, it returns new access and refresh tokens.
func (tm *TokenManager) reauthWithSSO() (string, error) {
	store, err := tm.loadSSOCookies()
	if err != nil {
		return "", fmt.Errorf("%w: no SSO cookies available: %v", ErrRefreshFailed, err)
	}

	// Build cookie jar with SSO cookies for login.microsoftonline.com
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return "", fmt.Errorf("%w: failed to create cookie jar: %v", ErrRefreshFailed, err)
	}

	loginURL, _ := url.Parse("https://login.microsoftonline.com")
	var httpCookies []*http.Cookie
	for _, c := range store.Cookies {
		domain := c.Domain
		if domain == "" {
			domain = "login.microsoftonline.com"
		}
		httpCookies = append(httpCookies, &http.Cookie{
			Name:     c.Name,
			Value:    c.Value,
			Path:     c.Path,
			Domain:   domain,
			Secure:   c.Secure,
			HttpOnly: c.HttpOnly,
		})
	}
	jar.SetCookies(loginURL, httpCookies)

	client := &http.Client{
		Jar: jar,
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

	// Build authorize URL with prompt=none for silent auth
	authorizeURL := fmt.Sprintf(authorizeURLTemplate, tm.tenant)
	params := url.Values{
		"client_id":             {tm.clientID},
		"response_type":         {"code"},
		"redirect_uri":          {defaultRedirectURI},
		"scope":                 {tm.scope},
		"prompt":                {"none"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"m365bridge-sso"},
	}

	authReq, err := http.NewRequest("GET", authorizeURL+"?"+params.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("%w: failed to create authorize request: %v", ErrRefreshFailed, err)
	}
	authReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	authResp, err := client.Do(authReq)
	if err != nil {
		return "", fmt.Errorf("%w: authorize request failed: %v", ErrRefreshFailed, err)
	}
	defer authResp.Body.Close()

	// Check for redirect with auth code
	location := authResp.Header.Get("Location")
	if location == "" {
		body, _ := io.ReadAll(authResp.Body)
		return "", fmt.Errorf("%w: no redirect from authorize (status %d): %s", ErrRefreshFailed, authResp.StatusCode, string(body))
	}

	// Parse auth code from redirect URL
	locURL, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("%w: failed to parse redirect URL: %v", ErrRefreshFailed, err)
	}

	authCode := locURL.Query().Get("code")
	if authCode == "" {
		// Check for error in fragment (response_mode=fragment is default for SPA)
		fragment := locURL.Fragment
		if fragment != "" {
			fragParams, _ := url.ParseQuery(fragment)
			authCode = fragParams.Get("code")
			if authCode == "" {
				errCode := fragParams.Get("error")
				errDesc := fragParams.Get("error_description")
				return "", fmt.Errorf("%w: authorize returned error: %s: %s", ErrRefreshFailed, errCode, errDesc)
			}
		}
		if authCode == "" {
			errCode := locURL.Query().Get("error")
			errDesc := locURL.Query().Get("error_description")
			if errCode != "" {
				return "", fmt.Errorf("%w: authorize returned error: %s: %s", ErrRefreshFailed, errCode, errDesc)
			}
			return "", fmt.Errorf("%w: no auth code in redirect URL: %s", ErrRefreshFailed, location)
		}
	}

	// Exchange auth code for tokens
	tokenData := url.Values{
		"client_id":     {tm.clientID},
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"redirect_uri":  {defaultRedirectURI},
		"code_verifier": {verifier},
		"scope":         {tm.scope},
	}

	tokenReq, err := http.NewRequest("POST", tm.tokenURL, strings.NewReader(tokenData.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: failed to create token request: %v", ErrRefreshFailed, err)
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenReq.Header.Set("Origin", "https://m365.cloud.microsoft")
	tokenReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	tokenResp, err := client.Do(tokenReq)
	if err != nil {
		return "", fmt.Errorf("%w: token exchange failed: %v", ErrRefreshFailed, err)
	}
	defer tokenResp.Body.Close()

	body, err := io.ReadAll(tokenResp.Body)
	if err != nil {
		return "", fmt.Errorf("%w: failed to read token response: %v", ErrRefreshFailed, err)
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

	return result.AccessToken, nil
}
