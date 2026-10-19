// SPDX-License-Identifier: AGPL-3.0-or-later
package integrations

// Tests for the Tally voucher route (H2 part 2b). All tests are
// DB-free: the handler's sellerID and bridgeRegistered seams are
// stubbed, and the stub bridge fails closed without any network.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeTallyOK is a bridge that accepts every voucher — exercises the
// 202 path the stub can never reach.
type fakeTallyOK struct{}

func (fakeTallyOK) PostVoucher(_ context.Context, _ TallyVoucher) (string, error) {
	return "job-42", nil
}

// testTallyHandler builds a handler with stubbed identity and registry
// seams. bridge may be nil (route then answers 503).
func testTallyHandler(bridge TallyBridge, sellerOK, registered bool) *TallyHandler {
	h := NewTallyHandler(nil, bridge, slog.Default())
	h.sellerID = func(_ context.Context, _ string) (string, error) {
		if !sellerOK {
			return "", errNoSellerRow
		}
		return "seller-123", nil
	}
	h.bridgeRegistered = func(_ context.Context, _ string) (bool, error) {
		return registered, nil
	}
	return h
}

func tallyReq(t *testing.T, h *TallyHandler, body string, authedReq bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/integrations/tally/vouchers",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	if authedReq {
		authed(h.PostVoucher()).ServeHTTP(w, r)
	} else {
		h.PostVoucher().ServeHTTP(w, r)
	}
	return w
}

const tallyValidBody = `{"kind":"sales","date":"20261020","reference_no":"ORD-1",` +
	`"lines":[{"ledger_name":"Cash","amount_paise":129900},` +
	`{"ledger_name":"Sales","amount_paise":-129900}]}`

func TestTallyNoSubject(t *testing.T) {
	w := tallyReq(t, testTallyHandler(TallyStub{}, true, true), tallyValidBody, false)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
	if got := envelopeCode(t, w); got != "unauthorized" {
		t.Fatalf("code = %q, want unauthorized", got)
	}
}

func TestTallyNotASeller(t *testing.T) {
	w := tallyReq(t, testTallyHandler(TallyStub{}, false, true), tallyValidBody, true)
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
	if got := envelopeCode(t, w); got != "not_a_seller" {
		t.Fatalf("code = %q, want not_a_seller", got)
	}
}

func TestTallyValidationMatrix(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // fragment expected in the message
	}{
		{"bad JSON", `{"kind":`, "valid JSON"},
		{"unknown field", `{"kind":"sales","date":"20261020","reference_no":"R","bogus":1,"lines":[]}`, "known fields"},
		{"bad kind", `{"kind":"receipt","date":"20261020","reference_no":"R","lines":[]}`, "kind must be"},
		{"short date", `{"kind":"sales","date":"2026102","reference_no":"R","lines":[]}`, "YYYYMMDD"},
		{"impossible date", `{"kind":"sales","date":"20261340","reference_no":"R","lines":[]}`, "not a valid calendar date"},
		{"empty reference", `{"kind":"sales","date":"20261020","reference_no":"  ","lines":[]}`, "reference_no is required"},
		{"no lines", `{"kind":"sales","date":"20261020","reference_no":"R","lines":[]}`, "at least one ledger line"},
		{"unbalanced", `{"kind":"sales","date":"20261020","reference_no":"R","lines":[{"ledger_name":"Cash","amount_paise":100},{"ledger_name":"Sales","amount_paise":-99}]}`, "does not balance"},
		{"duplicate ledger", `{"kind":"sales","date":"20261020","reference_no":"R","lines":[{"ledger_name":"Cash","amount_paise":100},{"ledger_name":"Cash","amount_paise":-100}]}`, "duplicate ledger"},
		{"zero amount", `{"kind":"sales","date":"20261020","reference_no":"R","lines":[{"ledger_name":"Cash","amount_paise":0},{"ledger_name":"Sales","amount_paise":0}]}`, "non-zero"},
		{"over cap", `{"kind":"sales","date":"20261020","reference_no":"R","lines":[{"ledger_name":"Cash","amount_paise":1000000000000},{"ledger_name":"Sales","amount_paise":-1000000000000}]}`, "per-line ceiling"},
		{"blank ledger", `{"kind":"sales","date":"20261020","reference_no":"R","lines":[{"ledger_name":"  ","amount_paise":100},{"ledger_name":"Sales","amount_paise":-100}]}`, "ledger_name is required"},
		{"long narration", `{"kind":"sales","date":"20261020","reference_no":"R","narration":"` + strings.Repeat("n", 1001) + `","lines":[{"ledger_name":"Cash","amount_paise":100},{"ledger_name":"Sales","amount_paise":-100}]}`, "narration exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := tallyReq(t, testTallyHandler(TallyStub{}, true, true), tc.body, true)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400", w.Code)
			}
			if got := envelopeCode(t, w); got != "invalid_request" {
				t.Fatalf("code = %q, want invalid_request", got)
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("body %q lacks %q", w.Body.String(), tc.want)
			}
		})
	}
}

func TestTallyNotRegistered(t *testing.T) {
	w := tallyReq(t, testTallyHandler(TallyStub{}, true, false), tallyValidBody, true)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", w.Code)
	}
	if got := envelopeCode(t, w); got != "tally_not_registered" {
		t.Fatalf("code = %q, want tally_not_registered", got)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
}

func TestTallyRouteStubFailsClosed(t *testing.T) {
	w := tallyReq(t, testTallyHandler(TallyStub{}, true, true), tallyValidBody, true)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", w.Code)
	}
	if got := envelopeCode(t, w); got != "tally_unavailable" {
		t.Fatalf("code = %q, want tally_unavailable", got)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
}

func TestTallyNilBridgeFailsClosed(t *testing.T) {
	w := tallyReq(t, testTallyHandler(nil, true, true), tallyValidBody, true)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", w.Code)
	}
	if got := envelopeCode(t, w); got != "tally_unavailable" {
		t.Fatalf("code = %q, want tally_unavailable", got)
	}
}

func TestTallyAccepted(t *testing.T) {
	w := tallyReq(t, testTallyHandler(fakeTallyOK{}, true, true), tallyValidBody, true)
	if w.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202 (body %s)", w.Code, w.Body.String())
	}
	var res struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.JobID != "job-42" {
		t.Fatalf("bad 202 body: %v (%s)", err, w.Body.String())
	}
}
