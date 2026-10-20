// SPDX-License-Identifier: AGPL-3.0-or-later
package ads

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveTestHandler builds the serve handler with the seam DISABLED —
// the fail-closed path returns before any DB or service is touched,
// so these tests are fully hermetic (no pool, no fixtures).
func serveTestHandler() *ServeHandler {
	return NewServeHandler(nil, ServingDeps{}, ServingConfig{Enabled: false, ReservePaise: 50}, nil)
}

func doServe(t *testing.T, h *ServeHandler, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/ads/serve", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func serveErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("serve error body is not the envelope: %v (%q)", err, rec.Body.String())
	}
	return env.Error.Code
}

// TestServeDisabledIsLoud: the fail-closed seam answers 503
// serving_disabled, never silent "no demand".
func TestServeDisabledIsLoud(t *testing.T) {
	rec := doServe(t, serveTestHandler(), http.MethodPost,
		`{"slot":"feed_slot_1","categories":["audio"],"language":"hi","country":"IN"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled serve -> %d; want 503", rec.Code)
	}
	if got := serveErrCode(t, rec); got != "serving_disabled" {
		t.Fatalf("disabled serve code = %q; want serving_disabled", got)
	}
}

// TestServeRejectsBadShapes: slot is required; the bounds on
// categories/language/country fail loud, before the seam check.
func TestServeRejectsBadShapes(t *testing.T) {
	disabled := serveTestHandler()
	cases := []struct {
		name, body string
		want       int
		code       string
	}{
		{"empty slot", `{"slot":""}`, 400, "invalid_serve_request"},
		{"missing slot", `{"categories":["audio"]}`, 400, "invalid_serve_request"},
		{"slot too long", `{"slot":"` + strings.Repeat("s", 65) + `"}`, 400, "invalid_serve_request"},
		{"too many categories", `{"slot":"a","categories":["a","a","a","a","a","a","a","a","a","a","a"]}`, 400, "invalid_serve_request"},
		{"category too long", `{"slot":"a","categories":["` + strings.Repeat("c", 65) + `"]}`, 400, "invalid_serve_request"},
		{"language too long", `{"slot":"a","language":"hindi6"}`, 400, "invalid_serve_request"},
		{"country too long", `{"slot":"a","country":"IND"}`, 400, "invalid_serve_request"},
		{"unknown field", `{"slot":"a","user_id":"u1"}`, 400, "invalid_json"},
		{"not json", `{"slot":`, 400, "invalid_json"},
	}
	for _, tc := range cases {
		rec := doServe(t, disabled, http.MethodPost, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s -> %d; want %d", tc.name, rec.Code, tc.want)
			continue
		}
		if got := serveErrCode(t, rec); got != tc.code {
			t.Errorf("%s: code = %q; want %q", tc.name, got, tc.code)
		}
	}
}

// TestServeRequestShapeToSlot pins the request → SlotContext mapping
// (country uppercased, whitespace trimmed).
func TestServeRequestShapeToSlot(t *testing.T) {
	r := ServeRequest{Slot: " feed_1 ", Categories: []string{" Audio "}, Language: " hi ", Country: "in"}
	slot, err := r.toSlot()
	if err != nil {
		t.Fatalf("toSlot: %v", err)
	}
	if slot.Slot != "feed_1" || len(slot.Categories) != 1 || slot.Categories[0] != "Audio" {
		t.Fatalf("toSlot trimmed wrong: %+v", slot)
	}
	if slot.Language != "hi" || slot.Country != "IN" {
		t.Fatalf("toSlot language/country wrong: %+v", slot)
	}
}

// TestServeHandlerRejectsNonPost defends the method discipline inside
// the handler (registration also guards via methodGuard).
func TestServeHandlerRejectsNonPost(t *testing.T) {
	rec := doServe(t, serveTestHandler(), http.MethodGet, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/ads/serve -> %d; want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow = %q; want %q", allow, http.MethodPost)
	}
}
