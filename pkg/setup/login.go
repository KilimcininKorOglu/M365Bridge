package setup

// login.go carries the browser login flow: `m365-bridge login` launches the
// user's own Chrome through CDP, waits for the interactive M365 sign-in, then
// captures in one pass everything the gateway needs — the standard refresh
// token, the designer broker refresh token, and the cookies of every domain
// the browser picked up. The captures ride CDP network events, so no page
// script and no bundled browser are involved: the image stays small and the
// flows run against Microsoft's own pages with the user watching.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
)

const (
	chatHomeURL       = "https://m365.cloud.microsoft/chat"
	libraryURL        = "https://m365.cloud.microsoft/library"
	searchURL         = "https://m365.cloud.microsoft/search"
	tokenURLPath      = "/oauth2/v2.0/token"
	defaultLoginWait  = 5 * time.Minute
	defaultRouteWait  = 8 * time.Second
	defaultBrokerWait = 45 * time.Second
)

// LoginOptions carries the knobs of the browser login flow.
type LoginOptions struct {
	// BrowserPath is the executable to drive. Empty discovers the user's
	// installed Chrome automatically.
	BrowserPath string
}

// RunLogin runs the full browser login: open the browser, wait for the
// sign-in, capture tokens and cookies, verify what was captured by
// redeeming it, and store everything the gateway's credential files hold.
func RunLogin(opts LoginOptions) error {
	printLoginBanner()

	profileDir, err := os.MkdirTemp("", "m365-bridge-login-")
	if err != nil {
		return fmt.Errorf("failed to create a browser profile: %w", err)
	}
	defer os.RemoveAll(profileDir)

	taskCtx, cancel, err := startBrowser(opts.BrowserPath, profileDir)
	if err != nil {
		return err
	}
	defer cancel()

	capture := newTokenCapture(taskCtx)
	chromedp.ListenTarget(taskCtx, capture.onEvent)
	if err := chromedp.Run(taskCtx, network.Enable()); err != nil {
		return fmt.Errorf("failed to attach the network listener: %w", err)
	}

	if err := chromedp.Run(taskCtx, chromedp.Navigate(chatHomeURL)); err != nil {
		return fmt.Errorf("failed to open the M365 page: %w", err)
	}
	fmt.Println("  A Chrome window is open. Sign in to your M365 account there.")
	fmt.Println("  The capture starts by itself once the sign-in lands on the chat page.")
	if err := waitForSignIn(taskCtx, defaultLoginWait); err != nil {
		return err
	}

	fmt.Println("  Signed in. Capturing the standard refresh token ...")
	standardRT, err := captureStandard(taskCtx, capture)
	if err != nil {
		return err
	}
	fmt.Println("  Standard refresh token captured. Requesting one image so the designer broker fires ...")
	brokerRT := captureBroker(taskCtx, capture)

	cookies, err := collectCookies(taskCtx)
	if err != nil {
		return err
	}
	oid, tenant, err := captureIdentity(taskCtx)
	if err != nil {
		return err
	}

	return saveLogin(saveLoginInput{
		Tenant: tenant, OID: oid, StandardRT: standardRT, BrokerRT: brokerRT,
		Cookies: cookies,
	})
}

// printLoginBanner prints the wizard-style header of the login flow.
func printLoginBanner() {
	fmt.Println("=" + strings.Repeat("=", 58))
	fmt.Printf("  M365 Copilot Browser Login v%s\n", models.Version)
	fmt.Println("=" + strings.Repeat("=", 58))
	fmt.Println()
	fmt.Println("  This captures, in one pass:")
	fmt.Println("    - the standard refresh token (chat)")
	fmt.Println("    - the designer broker refresh token (image generation)")
	fmt.Println("    - every cookie the browser holds (login and M365 web)")
	fmt.Println()
}

// startBrowser launches the user's own Chrome headed on a throwaway profile
// and returns the tab context with its cancel function.
func startBrowser(browserPath, profileDir string) (context.Context, context.CancelFunc, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", false),
		chromedp.UserDataDir(profileDir),
	)
	if browserPath != "" {
		opts = append(opts, chromedp.ExecPath(browserPath))
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	return taskCtx, func() { taskCancel(); allocCancel() }, nil
}

// waitForSignIn polls the tab location until the SPA has landed back on the
// chat surface, which happens only after a completed sign-in.
func waitForSignIn(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("login did not complete: %w", err)
		}
		var loc string
		if err := chromedp.Run(ctx, chromedp.Location(&loc)); err == nil && onAppSurface(loc) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("login was not completed within %s", timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// onAppSurface reports whether the location is an M365 web app page rather
// than a login or landing hop.
func onAppSurface(loc string) bool {
	u, err := url.Parse(loc)
	if err != nil || u.Host != "m365.cloud.microsoft" {
		return false
	}
	path := u.Path
	return path != "" && path != "/landingv2" && path != "/spalanding"
}

// captureStandard walks the app's routes until a silent renew hands over the
// standard refresh token.
func captureStandard(ctx context.Context, capture *tokenCapture) (string, error) {
	for _, page := range []string{chatHomeURL, libraryURL, searchURL, chatHomeURL} {
		if rt := capture.Standard(); rt != "" {
			return rt, nil
		}
		if err := chromedp.Run(ctx, chromedp.Navigate(page)); err != nil {
			return "", fmt.Errorf("failed to navigate to %s: %w", page, err)
		}
		if err := waitRoute(ctx, defaultRouteWait, capture.standardCh); err == nil {
			return capture.Standard(), nil
		}
	}
	return "", fmt.Errorf("no standard refresh token was captured while walking the app routes")
}

// captureBroker asks the app for one image so the designer broker grant
// fires, and waits for its refresh token. A failed capture is not fatal: the
// standard flow still lands, and the image routes report their own state.
func captureBroker(ctx context.Context, capture *tokenCapture) string {
	for range 2 {
		if rt := capture.Broker(); rt != "" {
			return rt
		}
		if err := sendImagePrompt(ctx); err != nil {
			fmt.Printf("  Warning: could not reach the chat input: %v\n", err)
			return ""
		}
		if err := waitRoute(ctx, defaultBrokerWait, capture.brokerCh); err == nil {
			return capture.Broker()
		}
	}
	return ""
}

// sendImagePrompt types the broker trigger prompt into the app's chat box
// and submits it with an Enter keypress.
func sendImagePrompt(ctx context.Context) error {
	// The value-setter dance mirrors what the app's own framework does, so
	// the SPA accepts the input as if a person had typed it.
	const js = `(() => {
  const els = document.querySelectorAll('textarea, [contenteditable="true"], input[type="text"]');
  const el = els[els.length - 1];
  if (!el) return 'no-input';
  el.focus();
  const text = 'draw a simple cat';
  if (el.tagName === 'TEXTAREA' || el.tagName === 'INPUT') {
    const setter = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(el), 'value').set;
    setter.call(el, text);
    el.dispatchEvent(new Event('input', { bubbles: true }));
  } else {
    el.textContent = text;
    el.dispatchEvent(new InputEvent('input', { bubbles: true }));
  }
  el.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', code: 'Enter', bubbles: true }));
  el.dispatchEvent(new KeyboardEvent('keyup', { key: 'Enter', code: 'Enter', bubbles: true }));
  return 'sent';
})()`
	var reply string
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &reply)); err != nil {
		return err
	}
	if reply != "sent" {
		return fmt.Errorf("chat input not found")
	}
	return nil
}

// waitRoute blocks until the capture channel fires or the wait elapses.
func waitRoute(ctx context.Context, wait time.Duration, ch <-chan string) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ch:
		return nil
	case <-timer.C:
		return fmt.Errorf("nothing captured within %s", wait)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// collectCookies reads every cookie the browser holds through the CDP
// storage domain, which sees HttpOnly values page scripts never can.
func collectCookies(ctx context.Context) ([]auth.SSOCookie, error) {
	var raw []*network.Cookie
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(actx context.Context) error {
		var err error
		raw, err = storage.GetCookies().Do(actx)
		return err
	}))
	if err != nil {
		return nil, fmt.Errorf("failed to read the browser cookies: %w", err)
	}
	out := make([]auth.SSOCookie, 0, len(raw))
	for _, c := range raw {
		out = append(out, auth.SSOCookie{
			Name:     c.Name,
			Value:    c.Value,
			Path:     c.Path,
			Domain:   c.Domain,
			Secure:   c.Secure,
			HttpOnly: c.HTTPOnly,
		})
	}
	return out, nil
}

// captureIdentity reads the signed-in account's oid and tenant from the
// page's localStorage, the same source the console snippet uses.
func captureIdentity(ctx context.Context) (string, string, error) {
	const js = `(() => {
  let oid, tenant;
  for (const key of Object.keys(localStorage)) {
    if (!key.includes('active-account-filters')) continue;
    try {
      const val = JSON.parse(localStorage.getItem(key));
      if (val?.homeAccountId?.includes('.')) { [oid, tenant] = val.homeAccountId.split('.'); break; }
    } catch (e) {}
  }
  if (!oid) {
    const mk = Object.keys(localStorage).find(k => k.startsWith('msal.') && k.includes('|'));
    if (mk) { const p = mk.split('|')[1]; if (p?.includes('.')) [oid, tenant] = p.split('.'); }
  }
  return { oid: oid || '', tenant: tenant || '' };
})()`
	var identity struct {
		OID    string `json:"oid"`
		Tenant string `json:"tenant"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &identity)); err != nil {
		return "", "", fmt.Errorf("failed to read the account identity: %w", err)
	}
	if identity.OID == "" || identity.Tenant == "" {
		return "", "", fmt.Errorf("no signed-in account found in the page storage")
	}
	return identity.OID, identity.Tenant, nil
}

// saveLoginInput is everything the write phase needs.
type saveLoginInput struct {
	Tenant     string
	OID        string
	StandardRT string
	BrokerRT   string
	Cookies    []auth.SSOCookie
}

// saveLogin verifies the captures by redeeming them, then writes every
// credential file the gateway keeps.
func saveLogin(in saveLoginInput) error {
	verifiedTenant, verifiedOID, err := verifyToken(in.Tenant, in.OID, in.StandardRT)
	if err != nil {
		return fmt.Errorf("the captured standard refresh token did not redeem: %w", err)
	}
	fmt.Println("  Standard refresh token redeemed, encrypted and saved")
	if err := saveEnv(verifiedTenant, verifiedOID); err != nil {
		return err
	}

	loginCookies, m365Cookies := splitCookiesByDomain(in.Cookies)
	if err := saveCookieSet("SSO", loginCookies, auth.SaveSSOCookies); err != nil {
		return err
	}
	if err := saveCookieSet("M365 web", m365Cookies, auth.SaveM365Cookies); err != nil {
		return err
	}

	if in.BrokerRT == "" {
		fmt.Println("  Warning: no designer broker refresh token was captured;")
		fmt.Println("  image generation stays on its current credential.")
		return nil
	}
	return saveBroker(verifiedTenant, in.BrokerRT)
}

// saveCookieSet stores one cookie group and reports the count.
func saveCookieSet(label string, cookies []auth.SSOCookie, save func([]auth.SSOCookie) error) error {
	if len(cookies) == 0 {
		fmt.Printf("  Warning: no %s cookies were captured\n", label)
		return nil
	}
	if err := save(cookies); err != nil {
		return fmt.Errorf("failed to save %s cookies: %w", label, err)
	}
	fmt.Printf("  %s cookies encrypted and saved (%d)\n", label, len(cookies))
	return nil
}

// saveBroker stores the broker refresh token and proves it by redeeming it
// through the real designerapp flow. The TokenManager arguments other than
// the tenant are placeholders: GetDesignerToken addresses the token endpoint
// by tenant and reads its constants and the broker store on its own.
func saveBroker(tenant, brokerRT string) error {
	if err := auth.SaveBrokerRefreshToken(brokerRT); err != nil {
		return err
	}
	tm := auth.NewTokenManager(tenant, "designer", "designer", "data/tokens/rt_90day.txt", "data/tokens/token_cache.json")
	token, err := tm.GetDesignerToken()
	if err != nil {
		fmt.Printf("  Warning: the broker refresh token was saved but did not redeem: %v\n", err)
		return nil
	}
	fmt.Printf("  Designer broker refresh token verified by redemption (access token: %d chars)\n", len(token))
	return nil
}

// tokenCapture classifies the token endpoint exchanges the browser fires and
// hands each captured refresh token to the waiting phase through a channel.
type tokenCapture struct {
	ctx        context.Context
	mu         sync.Mutex
	standardRT string
	brokerRT   string
	tokenReqs  map[network.RequestID]bool
	standardCh chan string
	brokerCh   chan string
}

func newTokenCapture(ctx context.Context) *tokenCapture {
	return &tokenCapture{
		ctx:        ctx,
		tokenReqs:  map[network.RequestID]bool{},
		standardCh: make(chan string, 4),
		brokerCh:   make(chan string, 4),
	}
}

// Standard returns the captured standard refresh token, if any.
func (c *tokenCapture) Standard() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.standardRT
}

// Broker returns the captured broker refresh token, if any.
func (c *tokenCapture) Broker() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.brokerRT
}

// onEvent is the CDP event handler. It only touches local state here: any
// protocol round-trip happens on the consumer side, because issuing protocol
// commands from inside the event goroutine deadlocks the connection.
func (c *tokenCapture) onEvent(ev any) {
	switch ev := ev.(type) {
	case *network.EventRequestWillBeSent:
		if strings.Contains(ev.Request.URL, tokenURLPath) {
			c.mu.Lock()
			c.tokenReqs[ev.RequestID] = true
			c.mu.Unlock()
		}
	case *network.EventResponseReceived:
		c.mu.Lock()
		_, known := c.tokenReqs[ev.RequestID]
		c.mu.Unlock()
		if known {
			go c.classify(ev.RequestID)
		}
	}
}

// classify reads the response body of one token exchange, decides which
// grant it answered, and records the refresh token. It runs on its own
// goroutine because reading the body is a protocol round-trip.
func (c *tokenCapture) classify(id network.RequestID) {
	body, err := network.GetResponseBody(id).Do(c.ctx)
	if err != nil {
		return
	}

	rt := refreshTokenFrom(body)
	if rt == "" {
		return
	}
	isBroker := clientIDFromPost(c, id) == auth.BrokerClientID

	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case isBroker && c.brokerRT == "":
		c.brokerRT = rt
		c.brokerCh <- rt
	case !isBroker && c.standardRT == "":
		c.standardRT = rt
		c.standardCh <- rt
	}
}

// clientIDFromPost reads the client_id of the exchange's request body. The
// protocol round-trip answers an error for requests whose data is gone, and
// an unclassified exchange is treated as the standard grant.
func clientIDFromPost(c *tokenCapture, id network.RequestID) string {
	post, err := network.GetRequestPostData(id).Do(c.ctx)
	if err != nil {
		return ""
	}
	values, err := url.ParseQuery(post)
	if err != nil {
		return ""
	}
	return values.Get("client_id")
}

// refreshTokenFrom extracts the refresh_token field of a token endpoint
// response body, and answers empty for failures, which carry no such field.
func refreshTokenFrom(body []byte) string {
	var parsed struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	return parsed.RefreshToken
}
