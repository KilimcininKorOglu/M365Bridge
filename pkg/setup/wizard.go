// Package setup provides the browser-based setup wizard for M365 Copilot authentication.
// It extracts OID, tenant ID, and refresh token from browser console output,
// verifies the token, and saves the environment configuration.
package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/crypto"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
)

const (
	// defaultRefreshTokenFile is the default path for the refresh token.
	defaultRefreshTokenFile = "data/tokens/rt_90day.txt"
	// defaultCacheFile is the default path for the token cache.
	defaultCacheFile = "data/tokens/token_cache.json"
	// defaultEnvFile is the default path for the environment file.
	defaultEnvFile = "data/.env"
	// defaultSetupFile is the default file to read setup JSON from.
	defaultSetupFile = "data/setup.json"
)

// Run executes the setup wizard with the given file path.
// If filePath is empty, defaults to data/setup.json.
func Run(filePath string) error {
	if filePath == "" {
		filePath = defaultSetupFile
	}

	fmt.Println("=" + strings.Repeat("=", 58))
	fmt.Printf("  M365 Copilot Setup Wizard v%s\n", models.Version)
	fmt.Println("=" + strings.Repeat("=", 58))
	fmt.Println()

	// Show the JS snippet so the user knows what to run in the browser
	printBrowserInstructions()

	// Step 1: Read configuration from file
	fmt.Printf("Reading JSON from file: %s\n\n", filePath)
	tenant, oid, refreshToken, ssoCookies, err := getConfigFromFile(filePath)
	if err != nil {
		return fmt.Errorf("%w\n\nSave the browser console JSON output to %s and try again", err, filePath)
	}

	// Step 2: Verify token
	if err := verifyToken(tenant, oid, refreshToken); err != nil {
		return err
	}

	// Step 3: Save SSO cookies if provided
	if len(ssoCookies) > 0 {
		if err := auth.SaveSSOCookies(ssoCookies); err != nil {
			fmt.Printf("  Warning: failed to save SSO cookies: %v\n", err)
		} else {
			fmt.Println("  SSO cookies encrypted and saved")
		}
	}

	// Step 4: Save environment configuration
	if err := saveEnv(tenant, oid); err != nil {
		return err
	}

	// Success message
	fmt.Println("=" + strings.Repeat("=", 58))
	fmt.Println("Setup Complete!")
	fmt.Println("=" + strings.Repeat("=", 58))
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  ./bin/m365-bridge \"your question\"         # CLI query")
	fmt.Println("  ./bin/m365-bridge -i                       # Interactive mode")
	fmt.Println("  ./bin/m365-bridge --list-models            # List models")
	fmt.Println("  ./bin/m365-bridge serve --port 8000        # Start API server")
	fmt.Println()
	fmt.Printf("Token storage: %s\n", filepath.Dir(defaultRefreshTokenFile))
	fmt.Printf("Config file:   %s\n", defaultEnvFile)

	return nil
}

// printBrowserInstructions shows the JS snippet and steps for extracting config from the browser.
func printBrowserInstructions() {
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("  Step 1: Get configuration from browser")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()
	fmt.Println("Please complete the following steps in your browser:")
	fmt.Println("  1. Open https://m365.cloud.microsoft and login")
	fmt.Println("  2. Press F12 to open DevTools -> Console")
	fmt.Println("  3. Paste and run the following code:")
	fmt.Println()
	fmt.Println("  (Copy the complete line below)")
	fmt.Println()
	fmt.Println(strings.Repeat("-", 60))

	jsSnippet := `(() => {
const k = Object.keys(localStorage).find(k => k.startsWith('msal.') && k.includes('|'));
if (!k) return 'NOT_FOUND';
const p = k.split('|')[1].split('.');
const oid = p[0], tenant = p[1];

const origFetch = window.fetch;
window.fetch = async function(...args) {
  const resp = await origFetch.apply(this, args);
  const url = typeof args[0] === 'string' ? args[0] : (args[0] && args[0].url) || '';
  if (url.includes('login.microsoftonline.com') && url.includes('oauth2/v2.0/token')) {
    try {
      const clone = resp.clone();
      const data = await clone.json();
      if (data.refresh_token) {
        console.log('===== COPY THE COMPLETE JSON LINE BELOW =====');
        console.log(JSON.stringify({oid, tenant, refresh_token: data.refresh_token}));
      }
    } catch(e) {}
  }
  return resp;
};

const origXHROpen = XMLHttpRequest.prototype.open;
const origXHRSend = XMLHttpRequest.prototype.send;
XMLHttpRequest.prototype.open = function(method, url) {
  this._url = url;
  return origXHROpen.apply(this, arguments);
};
XMLHttpRequest.prototype.send = function(body) {
  this.addEventListener('load', function() {
    if (this._url && this._url.includes('oauth2/v2.0/token')) {
      try {
        const data = JSON.parse(this.responseText);
        if (data.refresh_token) {
          console.log('===== COPY THE COMPLETE JSON LINE BELOW =====');
          console.log(JSON.stringify({oid, tenant, refresh_token: data.refresh_token}));
        }
      } catch(e) {}
    }
  });
  return origXHRSend.apply(this, arguments);
};

// Force MSAL to refresh by clearing all access tokens from localStorage
// MSAL will then silently refresh them, triggering our interceptors
const keys = Object.keys(localStorage);
let cleared = 0;
for (const key of keys) {
  if (key.includes('accesstoken') || key.includes('idtoken')) {
    localStorage.removeItem(key);
    cleared++;
  }
}

// Trigger silent token acquisition by dispatching events MSAL listens to
window.dispatchEvent(new Event('load'));
if (window.msal) {
  try {
    const accounts = window.msal.getAllAccounts();
    if (accounts.length > 0) {
      window.msal.acquireTokenSilent({
        account: accounts[0],
        scopes: ['https://substrate.office.com/sydney/.default']
      }).catch(() => {});
    }
  } catch(e) {}
}

return 'Interceptors installed and ' + cleared + ' access tokens cleared. MSAL should refresh automatically. Watch the console for the JSON output.';
})()`

	fmt.Println(jsSnippet)
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()
	fmt.Println("  After running the code, MSAL will auto-refresh tokens")
	fmt.Println("  Watch the console for: ===== COPY THE COMPLETE JSON LINE BELOW =====")
	fmt.Println("  (If nothing appears, interact with the page to trigger token refresh)")
	fmt.Println()
	fmt.Println("  Save the JSON output to data/setup.json (or pass --file <path>)")
	fmt.Println()

	// SSO cookie instructions for auto-renewal
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("  Step 2 (Optional): Capture SSO cookies for auto-renewal")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()
	fmt.Println("  SSO cookies allow the server to auto-renew tokens after the")
	fmt.Println("  24-hour refresh token expiry, without running setup-wizard again.")
	fmt.Println("  SSO cookies last weeks/months.")
	fmt.Println()
	fmt.Println("  In DevTools:")
	fmt.Println("    1. Go to Application -> Cookies -> https://login.microsoftonline.com")
	fmt.Println("    2. Find these cookies and copy their values:")
	fmt.Println("       - ESTSAUTH")
	fmt.Println("       - ESTSAUTHPERSISTENT")
	fmt.Println("       -SignInStateCookie (if present)")
	fmt.Println("    3. Add them to data/setup.json under \"sso_cookies\" array:")
	fmt.Println()
	fmt.Println("  Example setup.json with SSO cookies:")
	fmt.Println("  {")
	fmt.Println("    \"oid\": \"...\", \"tenant\": \"...\", \"refresh_token\": \"...\",")
	fmt.Println("    \"sso_cookies\": [")
	fmt.Println("      {\"name\": \"ESTSAUTH\", \"value\": \"...\"},")
	fmt.Println("      {\"name\": \"ESTSAUTHPERSISTENT\", \"value\": \"...\"}")
	fmt.Println("    ]")
	fmt.Println("  }")
	fmt.Println()
}

// getConfigFromFile reads setup JSON from a file.
// Returns tenant, oid, refresh token, SSO cookies, and error.
func getConfigFromFile(path string) (string, string, string, []auth.SSOCookie, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("failed to read file %s: %w", path, err)
	}

	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return "", "", "", nil, fmt.Errorf("file %s is empty", path)
	}

	// Parse JSON directly
	var parsed struct {
		OID          string           `json:"oid"`
		Tenant       string           `json:"tenant"`
		RefreshToken string           `json:"refresh_token"`
		SSOCookies   []auth.SSOCookie `json:"sso_cookies"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		// Try extracting JSON from surrounding text
		jsonPattern := regexp.MustCompile(`\{.*\}`)
		if match := jsonPattern.FindString(raw); match != "" {
			if err2 := json.Unmarshal([]byte(match), &parsed); err2 != nil {
				return "", "", "", nil, fmt.Errorf("failed to parse JSON from file: %w", err)
			}
		} else {
			return "", "", "", nil, fmt.Errorf("failed to parse JSON from file: %w", err)
		}
	}

	if parsed.Tenant == "" || parsed.OID == "" {
		return "", "", "", nil, fmt.Errorf("missing tenant or oid in JSON")
	}
	if parsed.RefreshToken == "" || parsed.RefreshToken == "NOT_FOUND" {
		return "", "", "", nil, fmt.Errorf("missing or invalid refresh_token in JSON")
	}

	// If refresh_token is a JSON object, try extracting secret/value/data fields
	// If none found, use the entire JSON string as-is
	refreshToken := parsed.RefreshToken
	var rtObj map[string]interface{}
	if err := json.Unmarshal([]byte(refreshToken), &rtObj); err == nil {
		if secret, ok := rtObj["secret"].(string); ok && secret != "" {
			refreshToken = secret
		} else if value, ok := rtObj["value"].(string); ok && value != "" {
			refreshToken = value
		}
		// If no secret/value, keep the entire JSON string as refresh_token
	}

	fmt.Printf("  OID: %s\n", parsed.OID)
	fmt.Printf("  Tenant: %s\n", parsed.Tenant)
	fmt.Printf("  Refresh token: %d chars\n", len(refreshToken))
	if len(parsed.SSOCookies) > 0 {
		fmt.Printf("  SSO cookies: %d captured\n", len(parsed.SSOCookies))
	}

	return parsed.Tenant, parsed.OID, refreshToken, parsed.SSOCookies, nil
}

// verifyToken validates the refresh token by exchanging it for an access token.
func verifyToken(tenant, oid, refreshToken string) error {
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("  Step 2: Verify Token")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	// Ensure data directory exists
	dataDir := filepath.Dir(defaultRefreshTokenFile)
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	// Encrypt and save refresh token
	encryptedToken, err := crypto.Encrypt(refreshToken)
	if err != nil {
		return fmt.Errorf("failed to encrypt token: %w", err)
	}

	if err := os.WriteFile(defaultRefreshTokenFile, []byte(encryptedToken), 0600); err != nil {
		return fmt.Errorf("failed to save refresh token: %w", err)
	}
	fmt.Println("  Refresh token encrypted and saved")

	// Set environment variables for verification
	os.Setenv("M365_TENANT_ID", tenant)
	os.Setenv("M365_USER_OID", oid)

	// Create token manager and verify
	tokenManager := auth.NewTokenManager(tenant, models.DefaultClientID, models.DefaultScope, defaultRefreshTokenFile, defaultCacheFile)
	accessToken, err := tokenManager.Get()
	if err != nil {
		return fmt.Errorf("token verification failed: %w (refresh token may be expired)", err)
	}

	fmt.Printf("  Token verification successful (access_token length: %d)\n", len(accessToken))
	return nil
}

// saveEnv saves the environment configuration to .env file.
func saveEnv(tenant, oid string) error {
	envContent := fmt.Sprintf("# M365 Copilot Configuration\nM365_TENANT_ID=%s\nM365_USER_OID=%s\nM365_CLIENT_ID=%s\n",
		tenant, oid, models.DefaultClientID)

	if err := os.WriteFile(defaultEnvFile, []byte(envContent), 0600); err != nil {
		return fmt.Errorf("failed to save environment file: %w", err)
	}

	fmt.Printf("  Environment variables saved to %s\n", defaultEnvFile)
	return nil
}
