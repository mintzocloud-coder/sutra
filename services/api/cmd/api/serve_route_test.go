// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestServeRouteReachesHandler locks the H3-part-3b contract: POST
// /v1/ads/serve must route to the serve handler (any non-404/405).
// The seam is disabled in the test process, so the handler 503s on
// serving_disabled without touching the nil pool — routing is still
// proved (not 404/405).
func TestServeRouteReachesHandler(t *testing.T) {
	h := testHandler()
	req := httptest.NewRequest(http.MethodPost, "/v1/ads/serve",
		strings.NewReader(`{"slot":"feed_slot_1","categories":["audio"],"language":"hi","country":"IN"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/ads/serve -> %d %s; want the serve handler (any non-404/405)",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	t.Logf("POST /v1/ads/serve -> %d (handler reached)", rec.Code)
}

// TestServeRouteRejectsOtherMethods locks the method discipline:
// only POST is allowed, and the 405 carries Allow: POST.
func TestServeRouteRejectsOtherMethods(t *testing.T) {
	h := testHandler()
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(m, "/v1/ads/serve", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /v1/ads/serve -> %d; want 405", m, rec.Code)
			continue
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Errorf("%s /v1/ads/serve: Allow=%q; want %q", m, allow, http.MethodPost)
		}
	}
}
