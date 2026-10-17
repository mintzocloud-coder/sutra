// SPDX-License-Identifier: AGPL-3.0-or-later
package middleware

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRateLimiterBurstThenDeny(t *testing.T) {
	l := NewRateLimiter(10, 5)
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("request %d: expected allow within burst", i+1)
		}
	}
	ok, wait := l.Allow("1.2.3.4")
	if ok {
		t.Fatal("expected denial after burst exhausted")
	}
	if wait <= 0 {
		t.Fatalf("expected positive retry-after, got %v", wait)
	}
	// A different IP has its own bucket.
	if ok, _ := l.Allow("5.6.7.8"); !ok {
		t.Fatal("different client should have a fresh bucket")
	}
}

func TestRateLimiterRefill(t *testing.T) {
	l := NewRateLimiter(1000, 1) // 1 token/ms refill
	if ok, _ := l.Allow("1.2.3.4"); !ok {
		t.Fatal("first request should be allowed")
	}
	if ok, _ := l.Allow("1.2.3.4"); ok {
		t.Fatal("second immediate request should be denied")
	}
	time.Sleep(5 * time.Millisecond) // ~5 tokens earned
	if ok, _ := l.Allow("1.2.3.4"); !ok {
		t.Fatal("request after refill should be allowed")
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	l := NewRateLimiter(0, 0)
	for i := 0; i < 1000; i++ {
		if ok, _ := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("request %d: disabled limiter must always allow", i+1)
		}
	}
}

func TestRateLimitMiddleware429(t *testing.T) {
	limiter := NewRateLimiter(1, 2) // 2-burst, then 1 rps
	h := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		RequestID,
		RateLimit(limiter, discardLogger()),
	)

	var ok200, denied int
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusOK:
			ok200++
		case http.StatusTooManyRequests:
			denied++
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("429 response missing Retry-After header")
			}
			if rec.Header().Get("X-Request-ID") == "" {
				t.Fatal("429 response missing X-Request-ID")
			}
			// Standard JSON error envelope.
			var env map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("429 body is not JSON: %v", err)
			}
			errObj, _ := env["error"].(map[string]any)
			if errObj["code"] != "rate_limited" {
				t.Fatalf("expected error.code=rate_limited, got %v", errObj["code"])
			}
			if _, ok := errObj["request_id"].(string); !ok {
				t.Fatal("envelope missing request_id")
			}
		default:
			t.Fatalf("unexpected status %d", rec.Code)
		}
	}
	if ok200 < 2 {
		t.Fatalf("expected at least the burst of 2 to pass, got %d", ok200)
	}
	if denied == 0 {
		t.Fatal("expected 429s after the burst, got none")
	}
	// Informational limit header is advertised on allowed responses.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-RateLimit-Limit") == "" {
		t.Fatal("missing X-RateLimit-Limit header")
	}
}

func TestRateLimitClientIPIsolation(t *testing.T) {
	// Exhaust one IP's bucket; another IP must be unaffected.
	limiter := NewRateLimiter(1, 1)
	h := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}),
		RateLimit(limiter, discardLogger()),
	)
	hit := func(ip string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = ip + ":9999"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := hit("10.0.0.1"); code != http.StatusOK {
		t.Fatalf("first request from ip1: want 200, got %d", code)
	}
	if code := hit("10.0.0.1"); code != http.StatusTooManyRequests {
		t.Fatalf("second request from ip1: want 429, got %d", code)
	}
	if code := hit("10.0.0.2"); code != http.StatusOK {
		t.Fatalf("first request from ip2: want 200, got %d", code)
	}
}

func TestRateLimitErrorEnvelopeOnLimit(t *testing.T) {
	// Sanity: a malformed RemoteAddr (no port) must not break keying.
	limiter := NewRateLimiter(1, 1)
	h := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}),
		RateLimit(limiter, discardLogger()),
	)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "weird-no-port"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "rate_limited") {
		t.Fatalf("want 429 envelope, got %d %q", rec.Code, rec.Body.String())
	}
}
