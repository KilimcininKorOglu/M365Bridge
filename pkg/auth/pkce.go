// Package auth provides token management and OAuth2 authentication for M365 Copilot.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
)

// GeneratePKCE creates a cryptographic code verifier and SHA-256 code challenge (RFC 7636).
func GeneratePKCE() (verifier, challenge string, err error) {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return "", "", fmt.Errorf("failed to generate code verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(verifierBytes)

	hash := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(hash[:])

	return verifier, challenge, nil
}

// AuthorizationURL constructs the Microsoft Entra ID authorization URL for PKCE authentication.
func AuthorizationURL(tenant, clientID, redirectURI, scope, state, challenge string) string {
	if tenant == "" {
		tenant = "common"
	}
	if clientID == "" {
		clientID = models.DefaultClientID
	}
	if redirectURI == "" {
		redirectURI = models.NativeRedirectURI
	}
	if scope == "" {
		scope = models.DefaultScope
	}

	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"response_mode":         {"query"},
		"scope":                 {scope},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}

	return fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/authorize?%s", tenant, q.Encode())
}

// ExchangeCode exchanges an authorization code for access and refresh tokens using PKCE verifier.
func ExchangeCode(ctx context.Context, tenant, clientID, redirectURI, code, verifier, scope string) (*TokenResult, error) {
	if tenant == "" {
		tenant = "common"
	}
	if clientID == "" {
		clientID = models.DefaultClientID
	}
	if redirectURI == "" {
		redirectURI = models.NativeRedirectURI
	}
	if scope == "" {
		scope = models.DefaultScope
	}

	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenant)
	form := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"scope":         {scope},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create code exchange request: %w", err)
	}
	// Native/public client PKCE exchange: do not send Origin header.
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("code exchange request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, tokenResponseMax+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read code exchange response: %w", err)
	}

	return parseExchangeResponse(body)
}

func parseExchangeResponse(body []byte) (*TokenResult, error) {
	var tr deviceTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}
	if tr.Error != "" {
		return nil, fmt.Errorf("token exchange error %s: %s", tr.Error, tr.ErrorDesc)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("token response missing access token")
	}

	return buildTokenResult(tr), nil
}

func normalizeCallbackURL(raw string) string {
	if strings.HasPrefix(raw, "?") || strings.HasPrefix(raw, "#") {
		return "http://dummy" + raw
	}
	if strings.HasPrefix(raw, "code=") {
		return "http://dummy?" + raw
	}
	return raw
}

func extractCodeFromParsed(u *url.URL) string {
	if code := u.Query().Get("code"); code != "" {
		return code
	}
	if u.Fragment != "" {
		fragValues, _ := url.ParseQuery(u.Fragment)
		return fragValues.Get("code")
	}
	return ""
}

// ExtractCodeFromCallback parses an authorization code from a raw user input,
// which could be a bare code, a full callback URL, or query/fragment parameters.
func ExtractCodeFromCallback(input string) (string, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return "", errors.New("empty callback input")
	}

	// If it's a bare code without URL syntax
	if !strings.Contains(raw, "://") && !strings.Contains(raw, "code=") && !strings.Contains(raw, "&") {
		return raw, nil
	}

	u, err := url.Parse(normalizeCallbackURL(raw))
	if err != nil {
		return "", fmt.Errorf("invalid callback URL: %w", err)
	}

	if code := extractCodeFromParsed(u); code != "" {
		return code, nil
	}

	return "", errors.New("no authorization code found in callback")
}
