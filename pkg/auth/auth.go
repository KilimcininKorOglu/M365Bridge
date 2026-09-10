// Package auth provides token management and OAuth2 authentication for M365 Copilot.
// It handles access token caching, refresh token storage, and token refresh logic.
package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/crypto"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
)

var (
	// ErrTokenNotFound is returned when the refresh token file is empty or missing.
	ErrTokenNotFound = errors.New("refresh token not found")
	// ErrRefreshFailed is returned when token refresh fails.
	ErrRefreshFailed = errors.New("token refresh failed")
)

const (
	// tokenURLTemplate is the OAuth2 token endpoint URL template. It is a public
	// Microsoft endpoint and carries no secret.
	// #nosec G101
	tokenURLTemplate = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"
	// cacheExpiryBuffer is the time buffer before token expiry to trigger refresh.
	cacheExpiryBuffer = 60 * time.Second
	// tokenResponseMax caps a token endpoint response. The body is a small JSON
	// object, so anything near this size is a redirected or hostile endpoint
	// rather than a token, and reading it whole would hold it all in memory.
	tokenResponseMax = 1 << 20
	// authPageMax caps a sign-in page read while following redirects. Those
	// pages are HTML and much larger than a token response.
	authPageMax = 4 << 20
)

// TokenCache represents the cached access token data.
type TokenCache struct {
	AccessToken string `json:"access_token"`
	ExpiresAt   int64  `json:"expires_at"`
}

// TokenManager handles OAuth2 token lifecycle management.
//
// Refresh tokens are single-use: Entra rotates them on every redemption and
// invalidates the previous value. The background refresher and every request
// path can both reach a refresh, so all redemption paths are serialized by
// mutexes. refreshMu guards the primary refresh token and access-token cache;
// designerMu guards the separate designerapp broker refresh token and its
// cache. Both are held across the network exchange so two callers can never
// redeem the same refresh token concurrently.
type TokenManager struct {
	tenant                 string
	clientID               string
	scope                  string
	refreshFile            string
	cacheFile              string
	tokenURL               string
	userOID                string
	designerTokenRequest   func(string) (string, int, error)
	brokerTokenAcquisition func() (string, error)
	refreshMu              sync.Mutex
	designerMu             sync.Mutex
}

// NewTokenManager creates a new TokenManager instance.
func NewTokenManager(tenant, clientID, scope, refreshFile, cacheFile string) *TokenManager {
	return &TokenManager{
		tenant:      tenant,
		clientID:    clientID,
		scope:       scope,
		refreshFile: refreshFile,
		cacheFile:   cacheFile,
		tokenURL:    fmt.Sprintf(tokenURLTemplate, tenant),
	}
}

// SetUserOID sets the user object ID for broker token requests.
func (tm *TokenManager) SetUserOID(oid string) {
	tm.userOID = oid
}

// Get returns a valid access token, refreshing if necessary.
// Returns cached token if valid, otherwise performs token refresh.
func (tm *TokenManager) Get() (string, error) {
	// Try to load from cache first
	if token, err := tm.loadFromCache(); err == nil {
		logging.Debug("TokenManager.Get: cache hit")
		return token, nil
	}

	logging.Debug("TokenManager.Get: cache miss, refreshing")
	tm.refreshMu.Lock()
	defer tm.refreshMu.Unlock()

	// Re-check the cache under the lock. A concurrent caller may have completed
	// a refresh while this goroutine waited, and redeeming again would burn the
	// rotated refresh token for nothing.
	if token, err := tm.loadFromCache(); err == nil {
		logging.Debug("TokenManager.Get: cache filled while waiting for refresh lock")
		return token, nil
	}

	return tm.refreshLocked()
}

// Refresh exchanges the refresh token for a new access token.
// Updates both the refresh token file and cache file.
func (tm *TokenManager) Refresh() (string, error) {
	tm.refreshMu.Lock()
	defer tm.refreshMu.Unlock()
	return tm.refreshLocked()
}

// refreshTokenRejected reports whether a token endpoint error says the stored
// refresh token itself cannot be redeemed, which is the case the SSO cookies
// recover from. Microsoft answers `invalid_grant` for every one of them: an
// expired token, a revoked one, a superseded one, and a value that is not a
// token at all. An error of another class, such as a throttle or an
// interaction requirement, is not something a cookie exchange would fix.
func refreshTokenRejected(body string) bool {
	return strings.Contains(body, "invalid_grant")
}

// aadstsCode picks the AADSTS number out of a token endpoint error body, so a
// log line names the reason without carrying the whole response.
func aadstsCode(body string) string {
	_, digits, found := strings.Cut(body, "AADSTS")
	if !found {
		return "no AADSTS code"
	}
	end := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(digits)
	}
	return "AADSTS" + digits[:end]
}

// refreshResponse is the token endpoint's answer to a refresh_token grant.
type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// refreshLocked performs the refresh token exchange. Callers must hold
// refreshMu, which keeps the single-use refresh token from being redeemed by
// two goroutines at once.
func (tm *TokenManager) refreshLocked() (string, error) {
	logging.Info("TokenManager.Refresh: starting token refresh")
	refreshToken, err := tm.readRefreshToken()
	if err != nil {
		logging.Errorf("TokenManager.Refresh: failed to read refresh token: %v", err)
		return "", err
	}

	body, status, err := tm.redeemRefreshToken(refreshToken)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return tm.handleRefreshFailure(status, body)
	}

	var result refreshResponse
	if err := json.Unmarshal(body, &result); err != nil {
		logging.Errorf("TokenManager.Refresh: failed to parse response: %v", err)
		return "", fmt.Errorf("%w: failed to parse response", ErrRefreshFailed)
	}
	return tm.storeRefreshResult(result)
}

// redeemRefreshToken posts the refresh_token grant and returns the bounded
// response body with its status, so the caller decides what the status means.
func (tm *TokenManager) redeemRefreshToken(refreshToken string) ([]byte, int, error) {
	data := url.Values{}
	data.Set("client_id", tm.clientID)
	data.Set("refresh_token", refreshToken)
	data.Set("grant_type", "refresh_token")
	data.Set("scope", tm.scope)

	req, err := http.NewRequest("POST", tm.tokenURL, bytes.NewBufferString(data.Encode()))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: failed to create request", ErrRefreshFailed)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://m365.cloud.microsoft")
	req.Header.Set("User-Agent", "Mozilla/5.0")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrRefreshFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// One extra byte distinguishes "exactly at the limit" from "truncated".
	body, err := io.ReadAll(io.LimitReader(resp.Body, tokenResponseMax+1))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: failed to read response", ErrRefreshFailed)
	}
	if len(body) > tokenResponseMax {
		return nil, 0, fmt.Errorf("%w: token response exceeds %d bytes", ErrRefreshFailed, tokenResponseMax)
	}
	return body, resp.StatusCode, nil
}

// handleRefreshFailure answers a non-200 from the token endpoint, and falls
// back to the SSO cookies when the stored refresh token is the thing that was
// rejected.
//
// Microsoft answers `invalid_grant` whenever the stored refresh token cannot be
// redeemed: AADSTS700084 for an expired one, AADSTS9002313 for a value that is
// not a token at all, and several more for a revoked or superseded one. The SSO
// cookies re-authenticate in every one of those cases, so the fallback keys on
// the OAuth error rather than on one AADSTS number, which left a recoverable
// install reporting a hard authentication failure.
func (tm *TokenManager) handleRefreshFailure(status int, body []byte) (string, error) {
	errMsg := string(body)
	if refreshTokenRejected(errMsg) && hasSSOCookies() {
		logging.Warnf("TokenManager.Refresh: refresh token rejected (%s), falling back to SSO cookie re-auth", aadstsCode(errMsg))
		return tm.reauthWithSSO()
	}
	logging.Errorf("TokenManager.Refresh: token refresh failed status=%d: %s", status, errMsg[:min(200, len(errMsg))])
	return "", fmt.Errorf("%w: status %d: %s", ErrRefreshFailed, status, errMsg)
}

// storeRefreshResult saves the rotated refresh token and caches the access
// token the exchange returned.
func (tm *TokenManager) storeRefreshResult(result refreshResponse) (string, error) {
	// Save new refresh token if provided
	if result.RefreshToken != "" {
		if err := tm.writeRefreshToken(result.RefreshToken); err != nil {
			logging.Errorf("TokenManager.Refresh: failed to save refresh token: %v", err)
			return "", fmt.Errorf("%w: failed to save refresh token", ErrRefreshFailed)
		}
	}

	// Cache access token
	expiresAt := time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	cache := TokenCache{
		AccessToken: result.AccessToken,
		ExpiresAt:   expiresAt.Unix(),
	}

	if err := tm.writeCache(cache); err != nil {
		logging.Errorf("TokenManager.Refresh: failed to write cache: %v", err)
		return "", fmt.Errorf("%w: failed to write cache", ErrRefreshFailed)
	}

	logging.Infof("TokenManager.Refresh: success, expires_in=%d expires_at=%s", result.ExpiresIn, expiresAt.Format(time.RFC3339))
	return result.AccessToken, nil
}

// readRefreshToken reads and decrypts the refresh token from file.
func (tm *TokenManager) readRefreshToken() (string, error) {
	data, err := os.ReadFile(tm.refreshFile)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrTokenNotFound, tm.refreshFile)
	}

	encrypted := string(data)
	if encrypted == "" {
		return "", ErrTokenNotFound
	}

	// Try to decrypt
	decrypted, err := crypto.Decrypt(encrypted)
	if err != nil {
		// If decryption fails, assume it's plaintext (legacy support)
		return encrypted, nil
	}

	return decrypted, nil
}

// writeRefreshToken encrypts and writes the refresh token to file.
func (tm *TokenManager) writeRefreshToken(token string) error {
	encrypted, err := crypto.Encrypt(token)
	if err != nil {
		return fmt.Errorf("failed to encrypt token: %w", err)
	}

	// Ensure directory exists
	dir := filepath.Dir(tm.refreshFile)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("failed to create directory: %w", err)
		}
	}

	return atomicWriteFile(tm.refreshFile, []byte(encrypted), 0600)
}

// loadFromCache attempts to load a valid access token from cache.
func (tm *TokenManager) loadFromCache() (string, error) {
	data, err := os.ReadFile(tm.cacheFile)
	if err != nil {
		return "", err
	}

	var cache TokenCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return "", err
	}

	// Check if token is still valid
	if cache.ExpiresAt > time.Now().Add(cacheExpiryBuffer).Unix() {
		return cache.AccessToken, nil
	}

	return "", errors.New("token expired")
}

// writeCache writes the access token cache to file.
//
// The cache holds the access token by design; caching it is the whole point of
// the file. It lives under the gitignored data/ tree, its directory is 0700 and
// the file itself is 0600.
func (tm *TokenManager) writeCache(cache TokenCache) error {
	// #nosec G117
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}

	// Ensure directory exists
	dir := filepath.Dir(tm.cacheFile)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("failed to create directory: %w", err)
		}
	}

	return atomicWriteFile(tm.cacheFile, data, 0600)
}
