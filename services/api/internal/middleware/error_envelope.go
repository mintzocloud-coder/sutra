// SPDX-License-Identifier: AGPL-3.0-or-later
// Shared JSON error envelope for the Sutra API service.
//
// Canonical shape: {"error":{"code","message","request_id"}}.
// Every non-2xx response the service emits uses this envelope, so clients
// can rely on a single machine-readable shape and operators can correlate
// any error with access logs via request_id.
package middleware

import (
	"encoding/json"
	"net/http"
)

// statusCodes maps HTTP statuses to stable machine-readable error codes.
// Codes are lowercase snake_case and part of the public API contract:
// do not rename an existing code.
var statusCodes = map[int]string{
	http.StatusBadRequest:          "bad_request",
	http.StatusUnauthorized:        "unauthorized",
	http.StatusForbidden:           "forbidden",
	http.StatusNotFound:            "not_found",
	http.StatusMethodNotAllowed:    "method_not_allowed",
	http.StatusConflict:            "conflict",
	http.StatusUnprocessableEntity: "unprocessable_entity",
	http.StatusTooManyRequests:     "rate_limited",
	http.StatusInternalServerError: "internal",
	http.StatusServiceUnavailable:  "service_unavailable",
}

// ErrorCode returns the machine-readable error code for an HTTP status.
// Unmapped statuses fall back to "error" so the envelope shape stays
// stable for any status a handler might use.
func ErrorCode(status int) string {
	if code, ok := statusCodes[status]; ok {
		return code
	}
	return "error"
}

// WriteJSONError sends status with a JSON error envelope containing a
// machine-readable code, a human-readable message, and the request ID so
// clients and operators can correlate with access logs.
func WriteJSONError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":       code,
			"message":    message,
			"request_id": FromContext(r.Context()),
		},
	})
	_, _ = w.Write(body)
}

// WriteStatusError sends status with the envelope, deriving the error
// code from the status (see ErrorCode). Prefer this for the standard
// cases (400/401/403/404/409/422/429/500); keep WriteJSONError for errors
// that need a bespoke code within one status (e.g. several 400 flavors).
func WriteStatusError(w http.ResponseWriter, r *http.Request, status int, message string) {
	WriteJSONError(w, r, status, ErrorCode(status), message)
}

// NotFoundHandler answers 404 in the standard envelope. Register it as
// the mux catch-all ("/") so unknown paths get the envelope instead of
// Go's plain-text "404 page not found".
func NotFoundHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		WriteStatusError(w, r, http.StatusNotFound, "not found")
	}
}

// MethodNotAllowedHandler answers 405 in the standard envelope and sets
// the Allow header so clients know which methods the route supports.
func MethodNotAllowedHandler(allowed ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for i, m := range allowed {
			if i == 0 {
				w.Header().Set("Allow", m)
			} else {
				w.Header().Add("Allow", m)
			}
		}
		WriteStatusError(w, r, http.StatusMethodNotAllowed, "method not allowed")
	}
}
