// Package auth provides token management and OAuth2 authentication for M365 Copilot.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
)

// DeviceCode represents an active OAuth2 device authorization grant session.
type DeviceCode struct {
	UserCode        string    `json:"user_code"`
	DeviceCode      string    `json:"device_code"`
	VerificationURI string    `json:"verification_uri"`
	Message         string    `json:"message"`
	ExpiresIn       int       `json:"expires_in"`
	Interval        int       `json:"interval"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type deviceCodeResponse struct {
	UserCode        string `json:"user_code"`
	DeviceCode      string `json:"device_code"`
	VerificationURI string `json:"verification_uri"`
	Message         string `json:"message"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Error           string `json:"error"`
	ErrorDesc       string `json:"error_description"`
}

// TokenResult holds the tokens and claims resulting from an OAuth exchange.
type TokenResult struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token,omitempty"`
	TokenType    string    `json:"token_type"`
	Scope        string    `json:"scope"`
	ExpiresIn    int       `json:"expires_in"`
	ExpiresAt    time.Time `json:"expires_at"`
	TenantID     string    `json:"tenant_id"`
	UserOID      string    `json:"user_oid"`
	Email        string    `json:"email,omitempty"`
	DisplayName  string    `json:"display_name,omitempty"`
}

type deviceTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
	Interval     int    `json:"interval"`
}

// StartDeviceCode initiates OAuth 2.0 Device Authorization Grant (RFC 8628).
// It returns the user verification URL and code to display to the user.
func StartDeviceCode(ctx context.Context, tenant, clientID, scope string) (*DeviceCode, error) {
	if tenant == "" {
		tenant = "common"
	}
	if clientID == "" {
		clientID = models.DefaultDeviceClientID
	}
	if scope == "" {
		scope = models.DefaultScope
	}

	endpoint := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/devicecode", tenant)
	form := url.Values{
		"client_id": {clientID},
		"scope":     {scope},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create device code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device code request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, tokenResponseMax+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read device code response: %w", err)
	}

	return parseDeviceCodeResponse(body)
}

func parseDeviceCodeResponse(body []byte) (*DeviceCode, error) {
	var dr deviceCodeResponse
	if err := json.Unmarshal(body, &dr); err != nil {
		return nil, fmt.Errorf("failed to parse device code response: %w", err)
	}
	if dr.Error != "" {
		return nil, fmt.Errorf("device code error %s: %s", dr.Error, dr.ErrorDesc)
	}
	if dr.DeviceCode == "" || dr.UserCode == "" {
		return nil, errors.New("device code response missing code fields")
	}

	interval := dr.Interval
	if interval <= 0 {
		interval = 5
	}
	return &DeviceCode{
		UserCode:        dr.UserCode,
		DeviceCode:      dr.DeviceCode,
		VerificationURI: dr.VerificationURI,
		Message:         dr.Message,
		ExpiresIn:       dr.ExpiresIn,
		Interval:        interval,
		ExpiresAt:       time.Now().Add(time.Duration(dr.ExpiresIn) * time.Second),
	}, nil
}

// PollDeviceCode polls the token endpoint until the user authenticates, the session expires,
// or the context is cancelled.
func PollDeviceCode(ctx context.Context, tenant, clientID, deviceCode string, interval time.Duration) (*TokenResult, error) {
	if tenant == "" {
		tenant = "common"
	}
	if clientID == "" {
		clientID = models.DefaultDeviceClientID
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}

	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenant)
	currentInterval := interval

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(currentInterval):
		}

		result, done, extraWait, err := pollDeviceStep(ctx, tokenURL, clientID, deviceCode)
		if err != nil {
			return nil, err
		}
		if done {
			return result, nil
		}
		if extraWait > 0 {
			currentInterval += extraWait
		}
	}
}

func pollDeviceStep(ctx context.Context, tokenURL, clientID, deviceCode string) (*TokenResult, bool, time.Duration, error) {
	form := url.Values{
		"client_id":   {clientID},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, false, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, tokenResponseMax+1))
	if err != nil {
		return nil, false, 0, err
	}

	return handleDeviceTokenBody(body)
}

func handleDeviceTokenBody(body []byte) (*TokenResult, bool, time.Duration, error) {
	var tr deviceTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, false, 0, fmt.Errorf("failed to parse token response: %w", err)
	}

	switch tr.Error {
	case "":
		// success path continues below
	case "authorization_pending":
		logging.Debug("Device code authorization pending...")
		return nil, false, 0, nil
	case "slow_down":
		extra := time.Duration(tr.Interval) * time.Second
		if extra <= 0 {
			extra = 5 * time.Second
		}
		return nil, false, extra, nil
	default:
		return nil, false, 0, fmt.Errorf("device authorization failed: %s: %s", tr.Error, tr.ErrorDesc)
	}

	if tr.AccessToken == "" {
		return nil, false, 0, errors.New("token response carries no access token")
	}

	result := buildTokenResult(tr)
	return result, true, 0, nil
}

func buildTokenResult(tr deviceTokenResponse) *TokenResult {
	expiresAt := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	tid, oid, email, name := parseJWTClaims(tr.AccessToken, tr.IDToken)

	return &TokenResult{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		IDToken:      tr.IDToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
		ExpiresIn:    tr.ExpiresIn,
		ExpiresAt:    expiresAt,
		TenantID:     tid,
		UserOID:      oid,
		Email:        email,
		DisplayName:  name,
	}
}

type jwtClaims struct {
	TID               string `json:"tid"`
	OID               string `json:"oid"`
	Sub               string `json:"sub"`
	TenantID          string `json:"tenant_id"`
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	UPN               string `json:"upn"`
	UniqueName        string `json:"unique_name"`
	Name              string `json:"name"`
}

func parseJWTClaims(accessToken, idToken string) (tid, oid, email, name string) {
	if c := decodeJWTPayload(accessToken); c != nil {
		tid = firstNonEmpty(c.TID, c.TenantID)
		oid = firstNonEmpty(c.OID, c.Sub)
		email = firstNonEmpty(c.PreferredUsername, c.Email, c.UPN, c.UniqueName)
		name = c.Name
	}
	if idToken != "" {
		if c := decodeJWTPayload(idToken); c != nil {
			if tid == "" {
				tid = firstNonEmpty(c.TID, c.TenantID)
			}
			if oid == "" {
				oid = firstNonEmpty(c.OID, c.Sub)
			}
			if email == "" {
				email = firstNonEmpty(c.PreferredUsername, c.Email, c.UPN, c.UniqueName)
			}
			if name == "" {
				name = c.Name
			}
		}
	}
	return tid, oid, email, name
}

func decodeJWTPayload(token string) *jwtClaims {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Fall back to standard URL encoding with padding
		pad := (4 - len(parts[1])%4) % 4
		padded := parts[1] + strings.Repeat("=", pad)
		payload, err = base64.URLEncoding.DecodeString(padded)
		if err != nil {
			return nil
		}
	}
	var claims jwtClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return &claims
}

func firstNonEmpty(candidates ...string) string {
	for _, c := range candidates {
		if strings.TrimSpace(c) != "" {
			return strings.TrimSpace(c)
		}
	}
	return ""
}
