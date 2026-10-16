// Package setup provides authentication setup and login commands for M365 Copilot.
package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/atomicfile"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/crypto"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
	"github.com/google/uuid"
)

// LoginOptions carries the flags of the zero-browser login command.
type LoginOptions struct {
	Quiet bool
	PKCE  bool
}

// RunLogin initiates zero-browser authentication using either Device Code Flow
// (default, RFC 8628) or terminal-assisted PKCE.
func RunLogin(opts LoginOptions) error {
	if opts.PKCE {
		return RunPKCELogin(opts.Quiet)
	}
	return RunDeviceLogin(opts.Quiet)
}

// RunDeviceLogin performs zero-browser sign-in using OAuth2 Device Code Flow (RFC 8628).
// The user authorizes on any separate device (phone, laptop) and the server acquires
// a 90-day rolling refresh token directly without any local browser.
func RunDeviceLogin(quiet bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	deviceCode, err := auth.StartDeviceCode(ctx, "", models.DefaultDeviceClientID, models.DefaultScope)
	if err != nil {
		return fmt.Errorf("failed to initiate device sign-in: %w", err)
	}

	printDeviceBanner(quiet, deviceCode)

	interval := time.Duration(deviceCode.Interval) * time.Second
	res, err := auth.PollDeviceCode(ctx, "", models.DefaultDeviceClientID, deviceCode.DeviceCode, interval)
	if err != nil {
		return fmt.Errorf("device authentication failed: %w", err)
	}

	return finishNativeLogin(res, models.DefaultDeviceClientID, quiet)
}

func printDeviceBanner(quiet bool, dc *auth.DeviceCode) {
	if quiet {
		return
	}
	fmt.Println("=" + strings.Repeat("=", 68))
	fmt.Printf("  Microsoft 365 Copilot Device Login v%s\n", models.Version)
	fmt.Println("=" + strings.Repeat("=", 68))
	fmt.Println()
	fmt.Println("  To sign in on any device (phone, laptop, tablet):")
	fmt.Println()
	fmt.Printf("    1. Open URL  : %s\n", dc.VerificationURI)
	fmt.Printf("    2. Enter Code: %s\n", dc.UserCode)
	fmt.Println()
	fmt.Println("  Waiting for authorization...")
	fmt.Println()
}

// RunPKCELogin performs terminal-assisted PKCE authorization.
// The user opens the official Microsoft sign-in page, and pastes the final callback URL or code.
func RunPKCELogin(quiet bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	verifier, challenge, err := auth.GeneratePKCE()
	if err != nil {
		return err
	}

	state := uuid.NewString()
	authURL := auth.AuthorizationURL("", models.DefaultClientID, models.NativeRedirectURI, models.DefaultScope, state, challenge)

	printPKCEBanner(quiet, authURL)

	code, err := readCallbackFromStdin()
	if err != nil {
		return err
	}

	res, err := auth.ExchangeCode(ctx, "", models.DefaultClientID, models.NativeRedirectURI, code, verifier, models.DefaultScope)
	if err != nil {
		return fmt.Errorf("code exchange failed: %w", err)
	}

	return finishNativeLogin(res, models.DefaultClientID, quiet)
}

func printPKCEBanner(quiet bool, authURL string) {
	if quiet {
		return
	}
	fmt.Println("=" + strings.Repeat("=", 68))
	fmt.Printf("  Microsoft 365 Copilot PKCE Login v%s\n", models.Version)
	fmt.Println("=" + strings.Repeat("=", 68))
	fmt.Println()
	fmt.Println("  1. Open this URL in any web browser:")
	fmt.Println()
	fmt.Println("     " + authURL)
	fmt.Println()
	fmt.Println("  2. Complete Microsoft sign-in.")
	fmt.Println("  3. After signing in, copy the full URL from the browser address bar")
	fmt.Println("     (e.g. https://.../nativeclient?code=...) and paste it below:")
	fmt.Println()
}

func readCallbackFromStdin() (string, error) {
	fmt.Print("  Paste callback URL or code: ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read from terminal: %w", err)
	}
	return auth.ExtractCodeFromCallback(line)
}

func finishNativeLogin(res *auth.TokenResult, clientID string, quiet bool) error {
	if res.TenantID == "" || res.UserOID == "" {
		return errors.New("token result missing tenant ID or user OID")
	}

	if err := saveCredentialStores(res, clientID); err != nil {
		return err
	}

	// Pre-mint designer token if possible
	tm := auth.NewTokenManager(res.TenantID, clientID, models.DefaultScope, defaultRefreshTokenFile, defaultCacheFile)
	if _, err := tm.GetDesignerToken(); err != nil {
		logging.Debugf("finishNativeLogin: designer pre-mint: %v", err)
	}

	if !quiet {
		printNativeSuccess(res)
	}
	return nil
}

func saveCredentialStores(res *auth.TokenResult, clientID string) error {
	return saveCredentialStoresTo(res, clientID, defaultRefreshTokenFile, defaultCacheFile, defaultEnvFile)
}

func saveCredentialStoresTo(res *auth.TokenResult, clientID, refreshFile, cacheFile, envFile string) error {
	dataDir := filepath.Dir(refreshFile)
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	encrypted, err := crypto.Encrypt(res.RefreshToken)
	if err != nil {
		return fmt.Errorf("failed to encrypt refresh token: %w", err)
	}
	if err := atomicfile.Write(refreshFile, []byte(encrypted), 0600); err != nil {
		return fmt.Errorf("failed to save refresh token: %w", err)
	}

	cache := auth.TokenCache{
		AccessToken: res.AccessToken,
		ExpiresAt:   res.ExpiresAt.Unix(),
	}
	if cacheBytes, err := json.Marshal(cache); err == nil {
		_ = atomicfile.Write(cacheFile, cacheBytes, 0600)
	}

	return saveEnvTo(res.TenantID, res.UserOID, clientID, envFile)
}

func printNativeSuccess(res *auth.TokenResult) {
	fmt.Println()
	fmt.Println("  ✓ Authentication successful!")
	if res.Email != "" {
		fmt.Printf("  Signed in as: %s\n", res.Email)
	}
	fmt.Printf("  Tenant ID: %s\n", res.TenantID)
	fmt.Printf("  User OID:  %s\n", res.UserOID)
	fmt.Println("  Refresh token saved with 90-day rolling expiration.")
	fmt.Println("  Zero browser required: automatic token refresh is now active.")
	fmt.Println()
}
