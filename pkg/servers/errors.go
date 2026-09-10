package servers

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/client"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
)

// OpenAI's error body carries a category in "type" and a machine-readable
// string in "code". This service had the two reversed: "type" held the specific
// code and "code" held the HTTP status as a number. Clients written against the
// OpenAI contract read "type" to decide whether to retry, re-authenticate or
// give up, so the reversal made every error look the same to them.
//
// sendError keeps its signature, because eighty call sites pass only a status
// and a message. Both fields are derived from that status instead.

// openAIErrorType maps an HTTP status onto the OpenAI error category.
func openAIErrorType(status int) string {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "server_error"
	}
}

// openAIErrorCode derives the default machine-readable code from an HTTP
// status, for example "bad_request" or "method_not_allowed". Callers that have
// a more specific code pass it to sendErrorCode instead.
func openAIErrorCode(status int) string {
	text := http.StatusText(status)
	if text == "" {
		return "error"
	}
	return strings.ToLower(strings.ReplaceAll(text, " ", "_"))
}

// Every upstream failure used to be reported as 500 with the transport error
// pasted into the message. That told a client nothing about whether to retry,
// re-authenticate or stop, and it leaked request URLs and token handling
// details from the M365 backend into a public response body.
//
// The failures are classified instead, and the raw error stays in the server
// log.

// upstream error codes. Each names one recoverable situation, so a client can
// branch on it without parsing the message.
const (
	upstreamAuthFailedCode     = "upstream_auth_failed"
	upstreamForbiddenCode      = "insufficient_permissions"
	upstreamRateLimitCode      = "rate_limit_exceeded"
	upstreamTimeoutCode        = "upstream_timeout"
	upstreamUnavailableCode    = "upstream_unavailable"
	upstreamRejectedCode       = "upstream_error"
	upstreamTurnFailedCode     = "upstream_turn_failed"
	modelNotFoundCode          = "model_not_found"
	internalProcessingCode     = "internal_error"
	requestTooLargeCode        = "request_too_large"
	rateLimitRetryAfterSeconds = 60
)

// sendRequestBodyError answers a request body this service could not read.
//
// A body past the cap gets 413 under its own code, so a client can tell a
// request it must shrink from one that was simply malformed. Both used to
// arrive as the same 400.
func (api *APIServer) sendRequestBodyError(w http.ResponseWriter, err error) {
	if tooLarge, ok := errors.AsType[*http.MaxBytesError](err); ok {
		logging.Warnf("request body above the limit of %d bytes", tooLarge.Limit)
		api.sendErrorCode(w, http.StatusRequestEntityTooLarge, requestTooLargeCode,
			"request body exceeds "+strconv.FormatInt(tooLarge.Limit, 10)+" bytes")
		return
	}
	api.sendError(w, http.StatusBadRequest, "Failed to read request body: "+err.Error())
}

// classifyUpstreamError maps a failed backend request onto the HTTP status and
// code the client should see.
//
// An error that carries no evidence of an upstream failure stays 500: the tool
// loop and the payload builders fail through this same path, and reporting our
// own bug as a backend outage would send the client into a pointless retry.
func classifyUpstreamError(err error) (int, string) {
	switch {
	case errors.Is(err, auth.ErrTokenNotFound), errors.Is(err, auth.ErrRefreshFailed):
		return http.StatusUnauthorized, upstreamAuthFailedCode
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, upstreamTimeoutCode
	case errors.Is(err, client.ErrHandshakeFailed), errors.Is(err, client.ErrConnectionClosed):
		return http.StatusBadGateway, upstreamUnavailableCode
	}

	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return http.StatusGatewayTimeout, upstreamTimeoutCode
	}

	// Checked before the status lookup below: a failed turn reached the backend
	// and carries no HTTP status, so it would otherwise fall through to the
	// generic internal error and read as a bug on this side.
	if turnFailed(err) {
		return http.StatusBadGateway, upstreamTurnFailedCode
	}

	if status, ok := client.UpstreamStatus(err); ok {
		return classifyUpstreamStatus(status)
	}

	return http.StatusInternalServerError, internalProcessingCode
}

// turnFailed reports a turn that reached the backend and produced no answer. A
// turn that ended with neither an answer nor a verdict counts too, because the
// client can act on both the same way.
func turnFailed(err error) bool {
	if _, ok := client.TurnFailure(err); ok {
		return true
	}
	return errors.Is(err, client.ErrEmptyTurn)
}

// classifyUpstreamStatus maps the backend's own HTTP status onto ours.
func classifyUpstreamStatus(status int) (int, string) {
	switch status {
	case http.StatusUnauthorized:
		return http.StatusUnauthorized, upstreamAuthFailedCode
	case http.StatusForbidden:
		return http.StatusForbidden, upstreamForbiddenCode
	case http.StatusPaymentRequired, http.StatusTooManyRequests:
		return http.StatusTooManyRequests, upstreamRateLimitCode
	case http.StatusServiceUnavailable:
		return http.StatusServiceUnavailable, upstreamUnavailableCode
	case http.StatusGatewayTimeout, http.StatusRequestTimeout:
		return http.StatusGatewayTimeout, upstreamTimeoutCode
	}
	// A dial that never reached a response reports status zero. The backend was
	// still unreachable, which is a gateway failure.
	return http.StatusBadGateway, upstreamRejectedCode
}

// upstreamErrorMessage states what failed without quoting the transport error.
func upstreamErrorMessage(op, code string) string {
	switch code {
	case upstreamAuthFailedCode:
		return "M365 authentication failed; the stored credentials could not be used for this " + op + " request"
	case upstreamForbiddenCode:
		return "M365 refused this " + op + " request for the configured account"
	case upstreamRateLimitCode:
		return "M365 rate limit reached for this " + op + " request; retry after the interval in the Retry-After header"
	case upstreamTimeoutCode:
		return "M365 did not answer the " + op + " request in time"
	case upstreamUnavailableCode:
		return "M365 is currently unreachable for this " + op + " request"
	case upstreamRejectedCode:
		return "M365 rejected the " + op + " request"
	case upstreamTurnFailedCode:
		return "M365 accepted the " + op + " request but ended the turn without producing an answer; a model whose backend tone is no longer served fails this way on every request"
	default:
		return "the " + op + " request failed before it could be completed"
	}
}

// resolveModel looks up the requested model and reports 404 when this service
// does not serve it.
//
// An unknown name used to fall back to the default entry, so the caller was
// answered by a tone it never asked for and a model removed from the registry
// kept working. The callers that guarded on an empty OpenAIID were dead code
// for the same reason: the fallback always carried one.
func (api *APIServer) resolveModel(w http.ResponseWriter, modelKey string) (models.ModelConfig, bool) {
	cfg, ok := models.FindModel(modelKey)
	if !ok {
		logging.Errorf("unknown model requested: %s", modelKey)
		api.sendErrorCode(w, http.StatusNotFound, modelNotFoundCode,
			"the model '"+modelKey+"' does not exist or is not served by this gateway; GET /v1/models lists the available ids")
		return models.ModelConfig{}, false
	}
	return cfg, true
}

// streamErrorFields classifies a failure that reached a stream already in
// progress, and returns the code and message the client may see.
//
// The HTTP status is gone by then, because the response header went out with
// the first frame. The body still has to carry the classification, and it still
// has to withhold the transport error, which names request URLs and credential
// file paths. The raw error goes to the log here, so every caller gets that for
// free.
// The status is returned too, because the OpenAI error object carries a
// category derived from it even when no header can be sent.
func streamErrorFields(op string, err error) (status int, code, message string) {
	status, code = classifyUpstreamError(err)
	logging.Errorf("%s stream failed: status=%d code=%s err=%v", op, status, code, err)
	return status, code, upstreamErrorMessage(op, code)
}

// sendUpstreamError reports a failed backend request. The raw error goes to the
// log; the client receives the classification and a fixed message.
func (api *APIServer) sendUpstreamError(w http.ResponseWriter, op string, err error) {
	status, code := classifyUpstreamError(err)
	logging.Errorf("%s failed: status=%d code=%s err=%v", op, status, code, err)

	// An exhausted conversation quota is the one empty-turn cause the client
	// can act on, so it keeps its own 429 instead of the generic gateway
	// failure. The Responses path already reported it that way before an empty
	// turn became an error here.
	if errors.Is(err, client.ErrEmptyTurn) && api.quotaExhausted() {
		api.sendThrottledError(w)
		return
	}

	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(rateLimitRetryAfterSeconds))
	}
	api.sendErrorCode(w, status, code, upstreamErrorMessage(op, code))
}
