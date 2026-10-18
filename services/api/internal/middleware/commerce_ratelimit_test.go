// SPDX-License-Identifier: AGPL-3.0-or-later
package middleware

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// isCommercePath matrix: the surface boundary is prefix-exact, never
// substring matching.
func TestIsCommercePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/sellers", true},
		{"/v1/sellers/abc", true},
		{"/v1/products", true},
		{"/v1/products/123/variants", true},
		{"/v1/variants/x", true},
		{"/v1/variants/x/inventory/adjust", true},
		{"/v1/categories", true},
		{"/v1/search", true},
		{"/v1/cart", true},
		{"/v1/cart/items", true},
		{"/v1/orders", true},
		{"/v1/orders/123/status", true},
		// Non-commerce surface.
		{"/v1/me", false},
		{"/v1/users/123", false},
		{"/v1/users/123/follow", false},
		{"/v1/posts", false},
		{"/v1/posts/123/like", false},
		{"/v1/feed/home", false},
		{"/v1/media/upload", false},
		{"/v1/ai/chat", false},
		{"/healthz", false},
		{"/readyz", false},
		// Prefix-boundary traps: no substring matching.
		{"/v1/productivity", false},
		{"/v1/sellers2", false},
		{"/v1/carts", false},
		{"/v1/order", false},
		{"/v1/ordersx", false},
		{"/v1/searches", false},
	}
	for _, tc := range cases {
		if got := IsCommercePath(tc.path); got != tc.want {
			t.Errorf("IsCommercePath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestCommerceRateLimit_AllowsThenDenies(t *testing.T) {
	log := slog.New(slog.NewTextHandler(&testDiscard{}, nil))
	l := NewRateLimiter(1, 1) // 1 rps, burst 1
	h := CommerceRateLimit(l, log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/categories", nil)
	req.RemoteAddr = "198.51.100.9:1234"

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", w.Code)
	}

	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", w2.Code)
	}
	body := w2.Body.String()
	if !strings.Contains(body, `"code":"commerce_rate_limited"`) {
		t.Errorf("429 body lacks commerce_rate_limited code: %s", body)
	}
	if !strings.Contains(body, `"request_id"`) {
		t.Errorf("429 body lacks request_id: %s", body)
	}
	if w2.Header().Get("Retry-After") == "" {
		t.Error("429 missing Retry-After header")
	}
	if w2.Header().Get("X-RateLimit-Limit") != "1" {
		t.Errorf("X-RateLimit-Limit = %q, want %q", w2.Header().Get("X-RateLimit-Limit"), "1")
	}
}

// Non-commerce traffic is never counted against the commerce budget:
// more than the burst allowance of /v1/me requests pass while the
// commerce bucket is already empty.
func TestCommerceRateLimit_NonCommercePassesThrough(t *testing.T) {
	log := slog.New(slog.NewTextHandler(&testDiscard{}, nil))
	l := NewRateLimiter(1, 1)
	h := CommerceRateLimit(l, log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Exhaust the commerce budget on the shared client IP.
	comReq := httptest.NewRequest(http.MethodGet, "/v1/orders", nil)
	comReq.RemoteAddr = "198.51.100.9:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, comReq)
	if w.Code != http.StatusOK {
		t.Fatalf("commerce request = %d, want 200", w.Code)
	}
	// Non-commerce requests on the same IP still pass.
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
		req.RemoteAddr = "198.51.100.9:1234"
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("non-commerce request %d = %d, want 200", i, w.Code)
		}
	}
}

// A disabled limiter (rps <= 0) lets commerce traffic through, so
// setting COMMERCE_LIMIT_RPS=0 removes the budget entirely.
func TestCommerceRateLimit_DisabledPassesThrough(t *testing.T) {
	log := slog.New(slog.NewTextHandler(&testDiscard{}, nil))
	l := NewRateLimiter(0, 0)
	h := CommerceRateLimit(l, log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
		req.RemoteAddr = "198.51.100.9:1234"
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200 (limiter disabled)", i, w.Code)
		}
	}
}

// testDiscard sinks slog output in these tests.
type testDiscard struct{}

func (testDiscard) Write(p []byte) (int, error) { return len(p), nil }
