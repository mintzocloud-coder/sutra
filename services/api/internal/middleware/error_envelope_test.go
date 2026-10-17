// SPDX-License-Identifier: AGPL-3.0-or-later
package middleware

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodeEnvelope parses the canonical {"error":{...}} body and returns
// the inner error object.
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response lacks error envelope: %q", rec.Body.String())
	}
	return errObj
}

// assertEnvelopeShape checks the full canonical shape:
// code + message + request_id present, JSON content type.
func assertEnvelopeShape(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d", rec.Code, wantStatus)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	errObj := decodeEnvelope(t, rec)
	if errObj["code"] != wantCode {
		t.Fatalf("error.code = %v, want %q", errObj["code"], wantCode)
	}
	if _, ok := errObj["message"].(string); !ok {
		t.Fatalf("error.message missing or not a string: %v", errObj)
	}
	rid, ok := errObj["request_id"].(string)
	if !ok || rid == "" {
		t.Fatalf("error.request_id missing or empty: %v", errObj)
	}
}

func TestWriteJSONErrorShape(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSONError(w, r, http.StatusTeapot, "custom_code", "custom message")
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "req-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	errObj := decodeEnvelope(t, rec)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", rec.Code)
	}
	if errObj["code"] != "custom_code" {
		t.Fatalf("code = %v, want custom_code", errObj["code"])
	}
	if errObj["message"] != "custom message" {
		t.Fatalf("message = %v, want custom message", errObj["message"])
	}
	if errObj["request_id"] != "req-123" {
		t.Fatalf("request_id = %v, want req-123 (inbound ID echoed)", errObj["request_id"])
	}
}

func TestErrorCodeMapping(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, "bad_request"},
		{http.StatusUnauthorized, "unauthorized"},
		{http.StatusForbidden, "forbidden"},
		{http.StatusNotFound, "not_found"},
		{http.StatusMethodNotAllowed, "method_not_allowed"},
		{http.StatusConflict, "conflict"},
		{http.StatusUnprocessableEntity, "unprocessable_entity"},
		{http.StatusTooManyRequests, "rate_limited"},
		{http.StatusInternalServerError, "internal"},
		{http.StatusServiceUnavailable, "service_unavailable"},
		{http.StatusTeapot, "error"},     // unmapped → stable fallback
		{http.StatusBadGateway, "error"}, // unmapped → stable fallback
		{http.StatusOK, "error"},         // unmapped → stable fallback
	}
	for _, tc := range cases {
		if got := ErrorCode(tc.status); got != tc.want {
			t.Errorf("ErrorCode(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestWriteStatusErrorDerivesCode(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusBadRequest, "bad_request"},
		{http.StatusUnauthorized, "unauthorized"},
		{http.StatusForbidden, "forbidden"},
		{http.StatusNotFound, "not_found"},
		{http.StatusConflict, "conflict"},
		{http.StatusUnprocessableEntity, "unprocessable_entity"},
		{http.StatusTooManyRequests, "rate_limited"},
		{http.StatusInternalServerError, "internal"},
		{http.StatusServiceUnavailable, "service_unavailable"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			WriteStatusError(w, r, tc.status, "msg")
		})).ServeHTTP(rec, req)
		assertEnvelopeShape(t, rec, tc.status, tc.code)
	}
}

func TestNotFoundHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	Chain(NotFoundHandler(), RequestID).ServeHTTP(rec, req)
	assertEnvelopeShape(t, rec, http.StatusNotFound, "not_found")
}

func TestMethodNotAllowedHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	rec := httptest.NewRecorder()
	Chain(MethodNotAllowedHandler(http.MethodGet, http.MethodHead), RequestID).ServeHTTP(rec, req)
	assertEnvelopeShape(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	allow := rec.Header().Get("Allow")
	if !strings.Contains(allow, "GET") {
		t.Fatalf("Allow header = %q, want it to mention GET", allow)
	}
}

// TestRecoverEnvelopeCarriesRequestID pins the middleware order contract:
// RequestID runs outside Recover, so a recovered panic still reports the
// request's ID for log correlation (a panic used to emit an empty
// request_id when Recover was outermost).
func TestRecoverEnvelopeCarriesRequestID(t *testing.T) {
	panicer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(errors.New("boom"))
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := Chain(panicer, RequestID, Recover(log))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "panic-req-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	errObj := decodeEnvelope(t, rec)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if errObj["code"] != "internal" {
		t.Fatalf("code = %v, want internal", errObj["code"])
	}
	if errObj["request_id"] != "panic-req-1" {
		t.Fatalf("request_id = %v, want panic-req-1", errObj["request_id"])
	}
	if rec.Header().Get("X-Request-ID") != "panic-req-1" {
		t.Fatalf("response X-Request-ID = %q, want panic-req-1",
			rec.Header().Get("X-Request-ID"))
	}
}
