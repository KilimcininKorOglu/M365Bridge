package servers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/client"
)

func TestOpenAIErrorTypeMapsEveryStatusClass(t *testing.T) {
	cases := map[int]string{
		http.StatusBadRequest:          "invalid_request_error",
		http.StatusUnauthorized:        "authentication_error",
		http.StatusForbidden:           "authentication_error",
		http.StatusNotFound:            "invalid_request_error",
		http.StatusMethodNotAllowed:    "invalid_request_error",
		http.StatusConflict:            "invalid_request_error",
		http.StatusTooManyRequests:     "rate_limit_error",
		http.StatusInternalServerError: "server_error",
		http.StatusBadGateway:          "server_error",
		http.StatusServiceUnavailable:  "server_error",
		http.StatusGatewayTimeout:      "server_error",
	}
	for status, want := range cases {
		if got := openAIErrorType(status); got != want {
			t.Fatalf("openAIErrorType(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestOpenAIErrorCodeIsASlugNotANumber(t *testing.T) {
	cases := map[int]string{
		http.StatusBadRequest:          "bad_request",
		http.StatusMethodNotAllowed:    "method_not_allowed",
		http.StatusInternalServerError: "internal_server_error",
		http.StatusTooManyRequests:     "too_many_requests",
	}
	for status, want := range cases {
		if got := openAIErrorCode(status); got != want {
			t.Fatalf("openAIErrorCode(%d) = %q, want %q", status, got, want)
		}
	}
	if got := openAIErrorCode(799); got != "error" {
		t.Fatalf("an unnamed status produced %q", got)
	}
}

// decodeErrorBody reads the error envelope as the wire carries it, so a code
// that regressed to a number fails to decode into a string.
func decodeErrorBody(t *testing.T, body []byte) (message, errType, code string) {
	t.Helper()
	var decoded struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	return decoded.Error.Message, decoded.Error.Type, decoded.Error.Code
}

func TestSendErrorUsesTheOpenAIShape(t *testing.T) {
	api := &APIServer{}
	rec := httptest.NewRecorder()
	api.sendError(rec, http.StatusBadRequest, "Invalid model")

	message, errType, code := decodeErrorBody(t, rec.Body.Bytes())
	if message != "Invalid model" {
		t.Fatalf("message = %q", message)
	}
	if errType != "invalid_request_error" {
		t.Fatalf("type = %q, want the category", errType)
	}
	if code != "bad_request" {
		t.Fatalf("code = %q, want a string slug", code)
	}
}

func TestSendErrorCodeKeepsTheSpecificCode(t *testing.T) {
	api := &APIServer{}
	rec := httptest.NewRecorder()
	api.sendErrorCode(rec, http.StatusTooManyRequests, upstreamThrottledCode, "quota exhausted")

	_, errType, code := decodeErrorBody(t, rec.Body.Bytes())
	if errType != "rate_limit_error" {
		t.Fatalf("type = %q, want the category", errType)
	}
	if code != upstreamThrottledCode {
		t.Fatalf("code = %q, want %q", code, upstreamThrottledCode)
	}
}

func TestContentBlockedErrorCarriesItsCodeInTheCodeField(t *testing.T) {
	api := &APIServer{}
	rec := httptest.NewRecorder()
	api.sendContentBlockedError(rec, "I'm sorry, I can't respond to that.")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
	_, errType, code := decodeErrorBody(t, rec.Body.Bytes())
	if errType != "server_error" {
		t.Fatalf("type = %q, want the category", errType)
	}
	if code != upstreamContentBlockedCode {
		t.Fatalf("code = %q, want %q", code, upstreamContentBlockedCode)
	}
}

// timeoutError is a net.Error that reports a timeout, which is how a stalled
// dial reaches the HTTP layer.
type timeoutError struct{}

func (timeoutError) Error() string { return "i/o timeout" }
func (timeoutError) Timeout() bool { return true }
func (timeoutError) Temporary() bool {
	return true
}

func TestClassifyUpstreamErrorSeparatesTheRecoverableCases(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing refresh token",
			err:        fmt.Errorf("failed to get token: %w", auth.ErrTokenNotFound),
			wantStatus: http.StatusUnauthorized,
			wantCode:   upstreamAuthFailedCode,
		},
		{
			name:       "refresh rejected",
			err:        fmt.Errorf("chat: %w", auth.ErrRefreshFailed),
			wantStatus: http.StatusUnauthorized,
			wantCode:   upstreamAuthFailedCode,
		},
		{
			name:       "dial refused with 401",
			err:        &client.UpstreamError{Op: "dial", Status: 401, Err: errors.New("bad handshake")},
			wantStatus: http.StatusUnauthorized,
			wantCode:   upstreamAuthFailedCode,
		},
		{
			name:       "dial refused with 403",
			err:        &client.UpstreamError{Op: "dial", Status: 403, Err: errors.New("bad handshake")},
			wantStatus: http.StatusForbidden,
			wantCode:   upstreamForbiddenCode,
		},
		{
			name:       "dial throttled",
			err:        fmt.Errorf("chat failed: %w", &client.UpstreamError{Op: "dial", Status: 429, Err: errors.New("bad handshake")}),
			wantStatus: http.StatusTooManyRequests,
			wantCode:   upstreamRateLimitCode,
		},
		{
			name:       "quota exhausted reported as 402",
			err:        &client.UpstreamError{Op: "dial", Status: 402, Err: errors.New("payment required")},
			wantStatus: http.StatusTooManyRequests,
			wantCode:   upstreamRateLimitCode,
		},
		{
			name:       "backend down",
			err:        &client.UpstreamError{Op: "upload", Status: 503, Err: errors.New("unavailable")},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   upstreamUnavailableCode,
		},
		{
			name:       "backend gateway timeout",
			err:        &client.UpstreamError{Op: "dial", Status: 504, Err: errors.New("timeout")},
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   upstreamTimeoutCode,
		},
		{
			name:       "unreachable before any response",
			err:        &client.UpstreamError{Op: "dial", Err: errors.New("no route to host")},
			wantStatus: http.StatusBadGateway,
			wantCode:   upstreamRejectedCode,
		},
		{
			name:       "backend rejected with 500",
			err:        &client.UpstreamError{Op: "upload", Status: 500, Err: errors.New("boom")},
			wantStatus: http.StatusBadGateway,
			wantCode:   upstreamRejectedCode,
		},
		{
			name:       "handshake failed",
			err:        fmt.Errorf("%w: %v", client.ErrHandshakeFailed, errors.New("eof")),
			wantStatus: http.StatusBadGateway,
			wantCode:   upstreamUnavailableCode,
		},
		{
			name:       "connection closed mid-stream",
			err:        client.ErrConnectionClosed,
			wantStatus: http.StatusBadGateway,
			wantCode:   upstreamUnavailableCode,
		},
		{
			name:       "deadline exceeded",
			err:        fmt.Errorf("chat: %w", context.DeadlineExceeded),
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   upstreamTimeoutCode,
		},
		{
			name:       "network timeout",
			err:        fmt.Errorf("dial: %w", net.Error(timeoutError{})),
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   upstreamTimeoutCode,
		},
		{
			name:       "our own tool loop bug",
			err:        errors.New("duplicate coding tool call \"read_file\""),
			wantStatus: http.StatusInternalServerError,
			wantCode:   internalProcessingCode,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code := classifyUpstreamError(tc.err)
			if status != tc.wantStatus || code != tc.wantCode {
				t.Fatalf("got (%d, %q), want (%d, %q)", status, code, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

func TestSendUpstreamErrorHidesTheTransportDetail(t *testing.T) {
	// The raw error names the backend host and the failed handshake. A public
	// error body must not repeat it.
	api := &APIServer{}
	rec := httptest.NewRecorder()
	raw := &client.UpstreamError{
		Op:     "dial",
		Status: 403,
		Err:    errors.New("websocket: bad handshake for wss://substrate.office.com/?access_token=SECRET"),
	}
	api.sendUpstreamError(rec, "chat", raw)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	message, errType, code := decodeErrorBody(t, rec.Body.Bytes())
	for _, leaked := range []string{"substrate.office.com", "access_token", "SECRET", "bad handshake"} {
		if strings.Contains(message, leaked) {
			t.Fatalf("message leaked %q: %s", leaked, message)
		}
	}
	if errType != "authentication_error" {
		t.Fatalf("type = %q", errType)
	}
	if code != upstreamForbiddenCode {
		t.Fatalf("code = %q, want %q", code, upstreamForbiddenCode)
	}
	if !strings.Contains(message, "chat") {
		t.Fatalf("message %q does not name the failed operation", message)
	}
}

func TestSendUpstreamErrorSetsRetryAfterOnlyWhenThrottled(t *testing.T) {
	api := &APIServer{}

	rec := httptest.NewRecorder()
	api.sendUpstreamError(rec, "chat", &client.UpstreamError{Op: "dial", Status: 429, Err: errors.New("throttled")})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}

	rec = httptest.NewRecorder()
	api.sendUpstreamError(rec, "chat", client.ErrConnectionClosed)
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("a non-throttled failure set Retry-After to %q", got)
	}
}

// A turn the backend itself marked as failed reached M365 and carries no HTTP
// status, so it must not fall through to the generic internal error.
func TestClassifyUpstreamErrorReportsAFailedTurn(t *testing.T) {
	err := fmt.Errorf("chat: %w", &client.TurnFailedError{
		Value: "InternalError", TurnState: "Failed", Message: "Sorry, I wasn't able to respond to that."})

	status, code := classifyUpstreamError(err)
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
	if code != upstreamTurnFailedCode {
		t.Errorf("code = %q, want %q", code, upstreamTurnFailedCode)
	}
}

// The turn-failure branch has to be reached before the UpstreamStatus lookup,
// which would otherwise classify a wrapped pair by the HTTP status alone.
func TestClassifyUpstreamErrorPrefersTheTurnVerdictOverAStatus(t *testing.T) {
	err := &client.UpstreamError{
		Op:     "dial",
		Status: http.StatusServiceUnavailable,
		Err:    &client.TurnFailedError{Value: "InternalError"},
	}

	_, code := classifyUpstreamError(err)
	if code != upstreamTurnFailedCode {
		t.Errorf("code = %q, want %q; the turn verdict is the more specific signal", code, upstreamTurnFailedCode)
	}
}

func TestClassifyUpstreamErrorMapsOutOfCreditsToRateLimit(t *testing.T) {
	err := fmt.Errorf("chat: %w", &client.TurnFailedError{
		Value: "OutOfCredits", TurnState: "Failed", Message: "You have used your available priority access.",
	})

	status, code := classifyUpstreamError(err)
	if status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", status)
	}
	if code != upstreamRateLimitCode {
		t.Errorf("code = %q, want %q", code, upstreamRateLimitCode)
	}
}

func TestClassifyUpstreamErrorMapsForbiddenRequestToInsufficientPermissions(t *testing.T) {
	err := fmt.Errorf("chat: %w", &client.TurnFailedError{
		Value: "ForbiddenRequest", TurnState: "Failed", Message: "It looks like you don't have a valid licence.",
	})

	status, code := classifyUpstreamError(err)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
	if code != upstreamForbiddenCode {
		t.Errorf("code = %q, want %q", code, upstreamForbiddenCode)
	}

	api := &APIServer{}
	rec := httptest.NewRecorder()
	api.sendUpstreamError(rec, "chat", err)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	message, _, _ := decodeErrorBody(t, rec.Body.Bytes())
	want := "Your M365 Copilot license does not include premium models (Copilot Pro/Enterprise required)"
	if message != want {
		t.Errorf("message = %q, want %q", message, want)
	}
}

func TestUpstreamErrorMessageExplainsAFailedTurn(t *testing.T) {
	msg := upstreamErrorMessage("chat", upstreamTurnFailedCode)
	if !strings.Contains(msg, "without producing an answer") {
		t.Errorf("message does not say the turn produced nothing: %q", msg)
	}
}

// A refused tone ends the turn with no answer and no verdict frame, so the
// sentinel has to classify the same way the verdict does.
func TestClassifyUpstreamErrorReportsAnEmptyTurn(t *testing.T) {
	status, code := classifyUpstreamError(fmt.Errorf("chat: %w", client.ErrEmptyTurn))
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
	if code != upstreamTurnFailedCode {
		t.Errorf("code = %q, want %q", code, upstreamTurnFailedCode)
	}
}

// An unknown model used to be folded into the default entry, so the caller was
// answered by a tone it never asked for. It must be a 404 instead.
func TestResolveModelRejectsAnUnknownName(t *testing.T) {
	api := &APIServer{}
	rec := httptest.NewRecorder()

	if _, ok := api.resolveModel(rec, "gpt-9-imaginary"); ok {
		t.Fatal("an unknown model was accepted")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}

	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != modelNotFoundCode {
		t.Errorf("code = %q, want %q", body.Error.Code, modelNotFoundCode)
	}
	if !strings.Contains(body.Error.Message, "gpt-9-imaginary") {
		t.Errorf("message does not name the model: %q", body.Error.Message)
	}
}

// Both the registry key and the advertised OpenAI id must resolve, because
// clients send one or the other.
func TestResolveModelAcceptsAKeyAndAnAdvertisedID(t *testing.T) {
	api := &APIServer{}
	for _, name := range []string{"gpt5.5-reasoning", "gpt-5.5-reasoning"} {
		rec := httptest.NewRecorder()
		cfg, ok := api.resolveModel(rec, name)
		if !ok {
			t.Fatalf("%s was rejected", name)
		}
		if cfg.Tone == "" {
			t.Errorf("%s resolved to an entry with no tone", name)
		}
	}
}
