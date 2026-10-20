// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreativeRoutesReachHandler locks the H3-part-3a contract: the
// four creative routes must reach their handlers (any non-404/405).
// Route-level assertions work without a DB: the handlers fail on the
// nil pool via the Recover middleware, which still proves routing
// (the orders_status_route_test.go precedent).
func TestCreativeRoutesReachHandler(t *testing.T) {
	h := testHandler()
	const cid = "52fb52ed-8841-4ae2-a36c-1df35f3764b5"
	cases := []struct{ method, path string }{
		{http.MethodPost, "/v1/ads/campaigns/" + cid + "/creatives"},
		{http.MethodGet, "/v1/ads/campaigns/" + cid + "/creatives"},
		{http.MethodGet, "/v1/ads/creatives/" + cid},
		{http.MethodPatch, "/v1/ads/creatives/" + cid},
		{http.MethodDelete, "/v1/ads/creatives/" + cid},
		{http.MethodPost, "/v1/ads/creatives/" + cid + "/activate"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path,
			strings.NewReader(`{"name":"x"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Fatalf("%s %s -> %d %s; want the creative handler (any non-404/405)",
				tc.method, tc.path, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		t.Logf("%s %s -> %d (handler reached)", tc.method, tc.path, rec.Code)
	}
}

// TestCreativeRoutesRejectOtherMethods locks the method discipline on
// the creative routes: the 405 must carry the exact Allow set from the
// route registration in main.go.
func TestCreativeRoutesRejectOtherMethods(t *testing.T) {
	h := testHandler()
	const cid = "52fb52ed-8841-4ae2-a36c-1df35f3764b5"
	cases := []struct {
		path, allow string
		methods     []string
	}{
		{"/v1/ads/campaigns/" + cid + "/creatives", "POST, GET",
			[]string{http.MethodPut, http.MethodDelete, http.MethodPatch}},
		{"/v1/ads/creatives/" + cid, "GET, PATCH, DELETE",
			[]string{http.MethodPost, http.MethodPut}},
		{"/v1/ads/creatives/" + cid + "/activate", http.MethodPost,
			[]string{http.MethodGet, http.MethodPut, http.MethodDelete}},
	}
	for _, tc := range cases {
		for _, m := range tc.methods {
			req := httptest.NewRequest(m, tc.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s -> %d; want 405", m, tc.path, rec.Code)
				continue
			}
			if allow := rec.Header().Get("Allow"); allow != tc.allow {
				t.Errorf("%s %s: Allow=%q; want %q", m, tc.path, allow, tc.allow)
			}
		}
	}
}
