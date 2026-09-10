// Package setup provides the browser-based setup wizard for M365 Copilot authentication.
// It extracts OID, tenant ID, and refresh token from browser console output,
// verifies the token, and saves the environment configuration.
package setup

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/atomicfile"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/crypto"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
)

const (
	// defaultRefreshTokenFile is the default path for the refresh token. The
	// value is where a credential is stored, not a credential.
	// #nosec G101
	defaultRefreshTokenFile = "data/tokens/rt_90day.txt"
	// defaultCacheFile is the default path for the token cache.
	defaultCacheFile = "data/tokens/token_cache.json"
	// defaultEnvFile is the default path for the environment file.
	defaultEnvFile = "data/.env"
	// defaultSetupFile is the default file to read setup JSON from.
	defaultSetupFile = "data/setup.json"
	// minRefreshTokenLength is the shortest value the wizard treats as a
	// refresh token. Microsoft issues one of several thousand characters, so
	// this only catches example text left in place of a real value.
	minRefreshTokenLength = 100
)

// guidPattern matches the form Entra writes a tenant id and an object id in.
var guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isFillerGUID reports whether every hex digit of a GUID is the same character,
// as in 22222222-2222-2222-2222-222222222222. Entra issues no such id, so a
// value of this shape is filler text someone typed in place of a real one.
//
// This is a function rather than a pattern because RE2, which is Go's regexp
// engine, has no backreference to say "the same digit again".
func isFillerGUID(value string) bool {
	var first rune
	for _, r := range value {
		if r == '-' {
			continue
		}
		if first == 0 {
			first = r
			continue
		}
		if r != first {
			return false
		}
	}
	return first != 0
}

// validateGUIDField reports why field cannot be the id it claims to be.
//
// Both ids are checked before anything is stored, the same way the refresh
// token's length is. A filler tenant reaches Microsoft and comes back as
// AADSTS90002, which reads like a broken install rather than a wrong value in
// a file; a filler oid never reaches Microsoft at all, so nothing else in the
// wizard can catch it and the install fails later, on a chat request.
func validateGUIDField(field, value string) error {
	if !guidPattern.MatchString(value) {
		return fmt.Errorf("%s is %q, which is not a GUID; copy the value the browser console printed", field, value)
	}
	if isFillerGUID(value) {
		return fmt.Errorf("%s is %q, which is filler text rather than a real id; copy the value the browser console printed", field, value)
	}
	return nil
}

// accessTokenClaims reads the tenant id and object id an access token was
// issued for.
//
// The claims are authoritative: Microsoft wrote them into the token it just
// returned, while the values in the setup file are whatever someone typed. No
// signature is verified and none needs to be, because the token arrived over
// TLS from the token endpoint in this same exchange and is never trusted as a
// credential here.
func accessTokenClaims(accessToken string) (tid, oid string, err error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return "", "", fmt.Errorf("access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", fmt.Errorf("failed to decode the access token claims: %w", err)
	}
	var claims struct {
		TID string `json:"tid"`
		OID string `json:"oid"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", fmt.Errorf("failed to parse the access token claims: %w", err)
	}
	if claims.TID == "" || claims.OID == "" {
		return "", "", fmt.Errorf("the access token carries no tid or oid claim")
	}
	return claims.TID, claims.OID, nil
}

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

	// Step 2: Save cookies by authentication surface before token verification.
	if len(ssoCookies) > 0 {
		loginCookies, m365Cookies := splitCookiesByDomain(ssoCookies)
		if len(loginCookies) > 0 {
			if err := auth.SaveSSOCookies(loginCookies); err != nil {
				fmt.Printf("  Warning: failed to save SSO cookies: %v\n", err)
			} else {
				fmt.Println("  SSO cookies encrypted and saved")
			}
		}
		if len(m365Cookies) > 0 {
			if err := auth.SaveM365Cookies(m365Cookies); err != nil {
				fmt.Printf("  Warning: failed to save M365 web cookies: %v\n", err)
			} else {
				fmt.Println("  M365 web cookies encrypted and saved")
			}
		}
	}

	// Step 3: Verify token (will fall back to SSO re-auth if refresh token expired)
	// The redeemed token names the identity it was issued for, and those are the
	// values saved, not the ones the file carried.
	verifiedTenant, verifiedOID, err := verifyToken(tenant, oid, refreshToken)
	if err != nil {
		return err
	}

	// Step 4: Save environment configuration
	if err := saveEnv(verifiedTenant, verifiedOID); err != nil {
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

	jsSnippet := `(async () => {
// 1. Get oid/tenant from the signed-in account
let oid, tenant;
for (const key of Object.keys(localStorage)) {
  if (!key.includes('active-account-filters')) continue;
  try {
    const val = JSON.parse(localStorage.getItem(key));
    if (val?.homeAccountId?.includes('.')) { [oid, tenant] = val.homeAccountId.split('.'); break; }
  } catch(e) {}
}
if (!oid) {
  const mk = Object.keys(localStorage).find(k => k.startsWith('msal.') && k.includes('|'));
  if (mk) { const p = mk.split('|')[1]; if (p?.includes('.')) [oid, tenant] = p.split('.'); }
}
if (!oid || !tenant) return 'ERROR: No signed-in account found. Log in to m365.cloud.microsoft and run this again.';

// 2. Watch every token exchange for the one this gateway uses
const targetClientID = '4765445b-32c6-49b0-83e6-1d93765276ca';
const origFetch = window.fetch;
let done;
const captured = new Promise(resolve => { done = resolve; });
window.fetch = async function(...args) {
  const resp = await origFetch.apply(this, args);
  const url = typeof args[0] === 'string' ? args[0] : args[0]?.url || '';
  if (url.includes('oauth2/v2.0/token')) {
    try {
      let bodyStr = '';
      const init = args[1];
      if (typeof init?.body === 'string') bodyStr = init.body;
      else if (init?.body instanceof URLSearchParams) bodyStr = init.body.toString();
      else if (init?.body instanceof ArrayBuffer || ArrayBuffer.isView(init?.body)) bodyStr = new TextDecoder().decode(init.body);
      else if (args[0] instanceof Request) bodyStr = await args[0].clone().text();
      // The sign-in exchange puts a broker id in client_id and carries the real
      // target in brk_client_id, so both are accepted.
      const params = new URLSearchParams(bodyStr);
      const isTarget = params.get('client_id') === targetClientID
                    || params.get('brk_client_id') === targetClientID;
      if (isTarget) {
        const data = await resp.clone().json();
        if (data.refresh_token) {
          console.log('===== COPY THE COMPLETE JSON BELOW =====');
          console.log(JSON.stringify({oid, tenant, refresh_token: data.refresh_token}, null, 2));
          done(true);
        }
      }
    } catch(e) {}
  }
  return resp;
};

// 3. Make the app ask for a token
// The page keeps its MSAL instance out of reach of the console, so the refresh
// cannot be requested directly. Moving to another route makes the app request
// one; the original page is restored afterwards.
const startPath = location.pathname;
let moved = false;
for (const href of ['/search', '/library', '/teach', '/chat/all', '/chat']) {
  if (href === startPath) continue;
  const link = document.querySelector('a[href="' + href + '"]');
  if (link) { link.click(); moved = true; break; }
}

// 4. Wait for the exchange, then put everything back
const ok = await Promise.race([captured, new Promise(r => setTimeout(() => r(false), 20000))]);
if (moved) history.back();
window.fetch = origFetch;

return ok
  ? 'Done. Copy the JSON printed above.'
  : 'No token exchange seen; the app is still using a token it refreshed a moment ago. Reload the page and run this again.';
})()`

	fmt.Println(jsSnippet)
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()
	fmt.Println("  The code moves to another page and back to make the app ask")
	fmt.Println("  for a token, then restores the page.")
	fmt.Println("  Watch the console for: ===== COPY THE COMPLETE JSON BELOW =====")
	fmt.Println("  (If it reports no token exchange, reload the page and run it again)")
	fmt.Println()
	fmt.Println("  Save the JSON output to data/setup.json (or pass --file <path>)")
	fmt.Println()

	// Browser cookie instructions for auto-renewal and conversation management.
	// This step is required rather than optional: the refresh token alone
	// expires after 24 hours, and without the login cookies the service cannot
	// sign itself back in.
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("  Step 2 (Required): Capture browser cookies")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()
	fmt.Println("  The refresh token expires after 24 hours. Login cookies let the")
	fmt.Println("  service renew it on its own; without them setup stops working")
	fmt.Println("  the next day. M365 web app cookies enable conversation management.")
	fmt.Println("  Capture cookies from both domains in DevTools -> Application -> Cookies:")
	fmt.Println("    - https://login.microsoftonline.com")
	fmt.Println("    - https://m365.cloud.microsoft")
	fmt.Println()
	fmt.Println("  Give every cookie its domain field. Each one is routed by that")
	fmt.Println("  field, and a cookie without it is discarded even though the count")
	fmt.Println("  below still reports it as read.")
	fmt.Println()
	fmt.Println("  Add each cookie to data/setup.json with its domain:")
	fmt.Println()
	fmt.Println("  {")
	fmt.Println("    \"oid\": \"...\", \"tenant\": \"...\", \"refresh_token\": \"...\",")
	fmt.Println("    \"sso_cookies\": [")
	fmt.Println("      {\"name\": \"ESTSAUTH\", \"value\": \"...\", \"domain\": \"login.microsoftonline.com\"},")
	fmt.Println("      {\"name\": \"<m365-cookie-name>\", \"value\": \"...\", \"domain\": \"m365.cloud.microsoft\"}")
	fmt.Println("    ]")
	fmt.Println("  }")
	fmt.Println()
}

// splitCookiesByDomain separates login cookies from M365 web cookies.
func splitCookiesByDomain(cookies []auth.SSOCookie) ([]auth.SSOCookie, []auth.SSOCookie) {
	var loginCookies []auth.SSOCookie
	var m365Cookies []auth.SSOCookie
	for _, cookie := range cookies {
		domain := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cookie.Domain)), ".")
		switch domain {
		case "login.microsoftonline.com":
			loginCookies = append(loginCookies, cookie)
		case "m365.cloud.microsoft", "microsoft.com":
			m365Cookies = append(m365Cookies, cookie)
		}
	}
	return loginCookies, m365Cookies
}

// setupFile is the shape of the JSON the operator hands the setup wizard.
type setupFile struct {
	OID          string           `json:"oid"`
	Tenant       string           `json:"tenant"`
	RefreshToken string           `json:"refresh_token"`
	SSOCookies   []auth.SSOCookie `json:"sso_cookies"`
}

// readSetupJSON reads and decodes the setup file. A file the operator pasted
// console output into carries the object inside surrounding text, so a failed
// decode is retried against the first braced span rather than refused.
func readSetupJSON(path string) (setupFile, error) {
	var parsed setupFile

	// The path is the -file argument the operator typed at the setup wizard.
	// #nosec G304
	data, err := os.ReadFile(path)
	if err != nil {
		return parsed, fmt.Errorf("failed to read file %s: %w", path, err)
	}

	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return parsed, fmt.Errorf("file %s is empty", path)
	}

	err = json.Unmarshal([]byte(raw), &parsed)
	if err == nil {
		return parsed, nil
	}

	match := regexp.MustCompile(`\{.*\}`).FindString(raw)
	if match == "" {
		return parsed, fmt.Errorf("failed to parse JSON from file: %w", err)
	}
	if err2 := json.Unmarshal([]byte(match), &parsed); err2 != nil {
		return parsed, fmt.Errorf("failed to parse JSON from file: %w", err)
	}
	return parsed, nil
}

// validateSetupFile refuses the values the wizard cannot tell apart from the
// example text, before anything is written or redeemed.
func validateSetupFile(parsed setupFile, path string) error {
	if parsed.Tenant == "" || parsed.OID == "" {
		return fmt.Errorf("missing tenant or oid in JSON")
	}
	if err := validateGUIDField("tenant", parsed.Tenant); err != nil {
		return err
	}
	if err := validateGUIDField("oid", parsed.OID); err != nil {
		return err
	}
	if parsed.RefreshToken == "" || parsed.RefreshToken == "NOT_FOUND" {
		return fmt.Errorf("missing or invalid refresh_token in JSON")
	}
	// A Microsoft refresh token runs to thousands of characters. A short value
	// is the example text left in place of a real one, and it is refused here
	// rather than stored, because the token endpoint answers a placeholder with
	// "request is malformed", which reads like a broken install.
	if len(parsed.RefreshToken) < minRefreshTokenLength {
		return fmt.Errorf(
			"refresh_token in %s is %d characters, which is too short to be a real token; copy the value the browser console printed",
			path, len(parsed.RefreshToken))
	}
	return nil
}

// refreshTokenSecret unwraps a refresh token the operator copied as a JSON
// object. With no secret or value field the whole string is the token.
func refreshTokenSecret(refreshToken string) string {
	var rtObj map[string]any
	if err := json.Unmarshal([]byte(refreshToken), &rtObj); err != nil {
		return refreshToken
	}
	if secret, ok := rtObj["secret"].(string); ok && secret != "" {
		return secret
	}
	if value, ok := rtObj["value"].(string); ok && value != "" {
		return value
	}
	return refreshToken
}

// getConfigFromFile reads setup JSON from a file.
// Returns tenant, oid, refresh token, SSO cookies, and error.
func getConfigFromFile(path string) (string, string, string, []auth.SSOCookie, error) {
	parsed, err := readSetupJSON(path)
	if err != nil {
		return "", "", "", nil, err
	}
	if err := validateSetupFile(parsed, path); err != nil {
		return "", "", "", nil, err
	}

	refreshToken := refreshTokenSecret(parsed.RefreshToken)

	fmt.Printf("  OID: %s\n", parsed.OID)
	fmt.Printf("  Tenant: %s\n", parsed.Tenant)
	fmt.Printf("  Refresh token: %d chars\n", len(refreshToken))
	if len(parsed.SSOCookies) > 0 {
		fmt.Printf("  SSO cookies: %d captured\n", len(parsed.SSOCookies))
	}

	return parsed.Tenant, parsed.OID, refreshToken, parsed.SSOCookies, nil
}

// verifyToken redeems the refresh token, stores it only once that succeeds, and
// returns the tenant id and object id the redeemed access token was issued for.
//
// The token is redeemed rather than merely loaded, and it is staged in a file
// of its own until the exchange returns. Verifying through TokenManager.Get()
// proved nothing, because Get returns a cached access token without ever
// reading the refresh token, so a placeholder passed the check whenever the
// previous run's cache was still warm. Writing the permanent file first was the
// other half of that failure: an unusable value replaced a working token before
// anything had tried it.
//
// The returned ids come from the token's own claims rather than from the setup
// file. The oid never takes part in the exchange, so nothing else in the wizard
// can tell a wrong one from a right one, and a wrong one breaks the install
// later on an ordinary chat request.
func verifyToken(tenant, oid, refreshToken string) (string, string, error) {
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("  Step 3: Verify Token")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	// Ensure data directory exists
	dataDir := filepath.Dir(defaultRefreshTokenFile)
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return "", "", fmt.Errorf("failed to create data directory: %w", err)
	}

	// Encrypt and stage the refresh token beside its permanent file.
	encryptedToken, err := crypto.Encrypt(refreshToken)
	if err != nil {
		return "", "", fmt.Errorf("failed to encrypt token: %w", err)
	}

	stagedFile := defaultRefreshTokenFile + ".verify"
	if err := atomicfile.Write(stagedFile, []byte(encryptedToken), 0600); err != nil {
		return "", "", fmt.Errorf("failed to stage refresh token: %w", err)
	}
	defer func() { _ = os.Remove(stagedFile) }()

	// Set environment variables for verification
	_ = os.Setenv("M365_TENANT_ID", tenant)
	_ = os.Setenv("M365_USER_OID", oid)

	// Refresh, never Get: the exchange is the only thing that proves the token
	// works, and it also rotates it, so the staged file ends up holding the
	// token the next run must use.
	tokenManager := auth.NewTokenManager(tenant, models.DefaultClientID, models.DefaultScope, stagedFile, defaultCacheFile)
	accessToken, err := tokenManager.Refresh()
	if err != nil {
		return "", "", fmt.Errorf("token verification failed: %w\n\nThe stored token was left untouched", err)
	}

	verified, err := os.ReadFile(stagedFile)
	if err != nil {
		return "", "", fmt.Errorf("failed to read the verified refresh token: %w", err)
	}
	if err := atomicfile.Write(defaultRefreshTokenFile, verified, 0600); err != nil {
		return "", "", fmt.Errorf("failed to save refresh token: %w", err)
	}
	fmt.Println("  Refresh token redeemed, encrypted and saved")

	fmt.Printf("  Token verification successful (access_token length: %d)\n", len(accessToken))

	claimTenant, claimOID, err := accessTokenClaims(accessToken)
	if err != nil {
		return "", "", fmt.Errorf("failed to read the identity the token was issued for: %w", err)
	}
	if claimTenant != tenant {
		fmt.Printf("  Tenant in the setup file was %s; the token was issued for %s, which is what gets saved\n", tenant, claimTenant)
	}
	if claimOID != oid {
		fmt.Printf("  OID in the setup file was %s; the token was issued for %s, which is what gets saved\n", oid, claimOID)
	}
	return claimTenant, claimOID, nil
}

// saveEnv saves the environment configuration to .env file.
func saveEnv(tenant, oid string) error {
	envContent := fmt.Sprintf("# M365 Copilot Configuration\nM365_TENANT_ID=%s\nM365_USER_OID=%s\nM365_CLIENT_ID=%s\n",
		tenant, oid, models.DefaultClientID)

	if err := atomicfile.Write(defaultEnvFile, []byte(envContent), 0600); err != nil {
		return fmt.Errorf("failed to save environment file: %w", err)
	}

	fmt.Printf("  Environment variables saved to %s\n", defaultEnvFile)
	return nil
}
