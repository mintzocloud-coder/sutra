// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mintzocloud-coder/sutra/services/api/internal/config"
)

// testHandler builds the full serving stack with no database. Route-level
// assertions (which handler a method+path reaches) work without a DB: the
// status handler 500s on the nil pool via the Recover middleware, which
// still proves the request was routed to it (not 404/405).
func testHandler() http.Handler {
	cfg := config.Config{AuthBypass: true, BypassSubject: "route-test-user"}
	return newHandler(context.Background(), cfg, nil, nil, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(os.Stderr, nil)), nil, nil, nil, nil)
}

// TestOrderStatusRouteReachesHandler locks the F3 contract: PATCH
// /v1/orders/{id}/status must route to the status handler. A 405 here
// means the route is shadowed or mis-registered (CI smoke triage #26:
// the smoke once observed a 405 on this exact route).
func TestOrderStatusRouteReachesHandler(t *testing.T) {
	h := testHandler()
	req := httptest.NewRequest(http.MethodPatch,
		"/v1/orders/52fb52ed-8841-4ae2-a36c-1df35f3764b5/status",
		strings.NewReader(`{"status":"confirmed"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("PATCH /v1/orders/{id}/status -> %d %s; want the status handler (any non-404/405)",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	t.Logf("PATCH /v1/orders/{id}/status -> %d (handler reached)", rec.Code)
}

// TestOrderStatusRouteRejectsOtherMethods locks the method discipline:
// only PATCH is allowed, and the 405 carries Allow: PATCH.
func TestOrderStatusRouteRejectsOtherMethods(t *testing.T) {
	h := testHandler()
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(m,
			"/v1/orders/52fb52ed-8841-4ae2-a36c-1df35f3764b5/status", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /v1/orders/{id}/status -> %d; want 405", m, rec.Code)
			continue
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPatch {
			t.Errorf("%s /v1/orders/{id}/status: Allow=%q; want %q", m, allow, http.MethodPatch)
		}
	}
}
