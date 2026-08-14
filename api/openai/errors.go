package openai

import (
	"encoding/json"
	"net/http"
)

// apiError is the OpenAI error object. Error responses always use this
// envelope: {"error": {"message", "type", "param", "code"}}.
type apiError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

type errorBody struct {
	Error apiError `json:"error"`
}

// Standard OpenAI error type strings.
const (
	errorTypeAuth      = "authentication_error"
	errorTypeInvalid   = "invalid_request_error"
	errorTypeServer    = "server_error"
	errorTypeRateLimit = "rate_limit_exceeded"
)

func apiErr(msg, typ, code string) apiError {
	return apiError{Message: msg, Type: typ, Code: code}
}

func authError(msg string) apiError {
	return apiErr(msg, errorTypeAuth, "invalid_api_key")
}

func invalidRequest(msg string) apiError {
	return apiErr(msg, errorTypeInvalid, "invalid_request_error")
}

func modelNotFound(model string) apiError {
	return apiErr("model not found: "+model, errorTypeInvalid, "model_not_found")
}

// sessionNotFound is the single generic error used for both unknown sessions
// and sessions owned by a different tenant. Collapsing them externally is
// deliberate: the distinct ErrSessionForbidden store error is an existence
// oracle and must never reach the wire.
func sessionNotFound() apiError {
	return apiErr("session not found or not accessible", errorTypeInvalid, "session_not_found")
}

func sessionBusy(msg string) apiError {
	if msg == "" {
		msg = "session already has an active turn"
	}
	return apiErr(msg, errorTypeInvalid, "session_busy")
}

func serverError(msg string) apiError {
	return apiErr(msg, errorTypeServer, "server_error")
}

// rateLimited builds a rate-limit-exceeded error for the given scope and
// reason (O-F10). The caller writes it via writeRateLimited which sets the
// Retry-After header.
func rateLimited(scope, reason string) apiError {
	return apiErr(
		"rate limit exceeded: "+scope+" "+reason,
		errorTypeRateLimit,
		scope+"_"+reason,
	)
}

// writeError writes the OpenAI error envelope with the given HTTP status.
func writeError(w http.ResponseWriter, status int, ae apiError) {
	writeJSON(w, status, errorBody{Error: ae})
}

// writeRateLimited writes a 429 with a Retry-After header (O-F10).
func writeRateLimited(w http.ResponseWriter, ae apiError) {
	w.Header().Set("Retry-After", "1")
	writeJSON(w, http.StatusTooManyRequests, errorBody{Error: ae})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A marshaling failure here is a programming error; the response is best
	// effort. encode and discard rather than panicking in a handler.
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = w.Write(b)
}
