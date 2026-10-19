// SPDX-License-Identifier: AGPL-3.0-or-later
package integrations

// Tests for the Zoho Books routes (H2 final piece). All tests are
// DB-free: the handler's sellerID seam is stubbed, and the stub
// provider fails closed without any network or OAuth.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testZohoHandler builds a handler with a stubbed seller identity seam.
// provider may be nil (routes then answer 503).
func testZohoHandler(provider ZohoBooksProvider, sellerOK bool) *ZohoHandler {
	h := NewZohoHandler(nil, provider, slog.Default())
	h.sellerID = func(_ context.Context, _ string) (string, error) {
		if !sellerOK {
			return "", errNoSellerRow
		}
		return "seller-123", nil
	}
	return h
}

func contactReq(t *testing.T, body string, provider ZohoBooksProvider, sellerOK bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/integrations/zoho/contacts",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	authed(testZohoHandler(provider, sellerOK).CreateContact()).ServeHTTP(w, r)
	return w
}

func invoiceReq(t *testing.T, body string, provider ZohoBooksProvider, sellerOK bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/integrations/zoho/invoices",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	authed(testZohoHandler(provider, sellerOK).CreateInvoice()).ServeHTTP(w, r)
	return w
}

func TestZohoContactNoSubject401(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/integrations/zoho/contacts",
		strings.NewReader(`{"display_name":"A Buyer"}`))
	w := httptest.NewRecorder()
	testZohoHandler(ZohoStub{}, true).CreateContact().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestZohoInvoiceNoSubject401(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/integrations/zoho/invoices",
		strings.NewReader(`{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20261020","lines":[{"name":"Widget","quantity":1,"amount_paise":129900}]}`))
	w := httptest.NewRecorder()
	testZohoHandler(ZohoStub{}, true).CreateInvoice().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestZohoNonSeller403(t *testing.T) {
	if w := contactReq(t, `{"display_name":"A Buyer"}`, ZohoStub{}, false); w.Code != http.StatusForbidden {
		t.Fatalf("contact: want 403, got %d", w.Code)
	} else if c := envelopeCode(t, w); c != "not_a_seller" {
		t.Fatalf("contact: want not_a_seller, got %q", c)
	}
	good := `{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20261020","lines":[{"name":"Widget","quantity":1,"amount_paise":129900}]}`
	if w := invoiceReq(t, good, ZohoStub{}, false); w.Code != http.StatusForbidden {
		t.Fatalf("invoice: want 403, got %d", w.Code)
	} else if c := envelopeCode(t, w); c != "not_a_seller" {
		t.Fatalf("invoice: want not_a_seller, got %q", c)
	}
}

func TestZohoStubFailsClosed503(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    *httptest.ResponseRecorder
	}{
		{"contact/stub", contactReq(t, `{"display_name":"A Buyer"}`, ZohoStub{}, true)},
		{"contact/nil", contactReq(t, `{"display_name":"A Buyer"}`, nil, true)},
		{"invoice/stub", invoiceReq(t, `{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20261020","lines":[{"name":"Widget","quantity":1,"amount_paise":129900}]}`, ZohoStub{}, true)},
		{"invoice/nil", invoiceReq(t, `{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20261020","lines":[{"name":"Widget","quantity":1,"amount_paise":129900}]}`, nil, true)},
	} {
		if tc.w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: want 503, got %d", tc.name, tc.w.Code)
		}
		if c := envelopeCode(t, tc.w); c != "zoho_unavailable" {
			t.Fatalf("%s: want zoho_unavailable, got %q", tc.name, c)
		}
		if ra := tc.w.Header().Get("Retry-After"); ra == "" {
			t.Fatalf("%s: 503 missing Retry-After", tc.name)
		}
	}
}

func TestZohoContactValidationMatrix(t *testing.T) {
	good := `{"display_name":"A Buyer","email":"a@example.com","phone":"+919876543210"}`
	for _, tc := range []struct {
		name, body string
		wantCode   string
		wantStatus int
	}{
		{"missing-name", `{}`, "invalid_request", 400},
		{"blank-name", `{"display_name":"  "}`, "invalid_request", 400},
		{"bad-email", `{"display_name":"A","email":"no-at-sign"}`, "invalid_request", 400},
		{"bad-email-domain", `{"display_name":"A","email":"a@b"}`, "invalid_request", 400},
		{"bad-phone", `{"display_name":"A","phone":"9876543210"}`, "invalid_phone", 422},
		{"unknown-field", `{"display_name":"A","bogus":1}`, "invalid_request", 400},
		{"bad-json", `{`, "invalid_request", 400},
	} {
		w := contactReq(t, tc.body, ZohoStub{}, true)
		if w.Code != tc.wantStatus {
			t.Fatalf("%s: want %d, got %d", tc.name, tc.wantStatus, w.Code)
		}
		if c := envelopeCode(t, w); c != tc.wantCode {
			t.Fatalf("%s: want %q, got %q", tc.name, tc.wantCode, c)
		}
	}
	if w := contactReq(t, good, ZohoStub{}, true); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("valid contact with stub: want 503, got %d", w.Code)
	}
}

func TestZohoInvoiceValidationMatrix(t *testing.T) {
	good := `{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20261020","lines":[{"name":"Widget","quantity":1,"amount_paise":129900}]}`
	for _, tc := range []struct {
		name, body string
	}{
		{"missing-contact", `{"reference_no":"R1","currency":"INR","due_date":"20261020","lines":[{"name":"Widget","quantity":1,"amount_paise":100}]}`},
		{"missing-ref", `{"contact_id":"c1","currency":"INR","due_date":"20261020","lines":[{"name":"Widget","quantity":1,"amount_paise":100}]}`},
		{"non-inr", `{"contact_id":"c1","reference_no":"R1","currency":"USD","due_date":"20261020","lines":[{"name":"Widget","quantity":1,"amount_paise":100}]}`},
		{"missing-currency", `{"contact_id":"c1","reference_no":"R1","due_date":"20261020","lines":[{"name":"Widget","quantity":1,"amount_paise":100}]}`},
		{"bad-date-shape", `{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20-10-2026","lines":[{"name":"Widget","quantity":1,"amount_paise":100}]}`},
		{"bad-date-calendar", `{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20261345","lines":[{"name":"Widget","quantity":1,"amount_paise":100}]}`},
		{"no-lines", `{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20261020","lines":[]}`},
		{"missing-name", `{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20261020","lines":[{"quantity":1,"amount_paise":100}]}`},
		{"zero-qty", `{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20261020","lines":[{"name":"W","quantity":0,"amount_paise":100}]}`},
		{"negative-amount", `{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20261020","lines":[{"name":"W","quantity":1,"amount_paise":-100}]}`},
		{"zero-amount", `{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20261020","lines":[{"name":"W","quantity":1,"amount_paise":0}]}`},
		{"unknown-field", `{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20261020","bogus":1,"lines":[{"name":"W","quantity":1,"amount_paise":100}]}`},
		{"bad-json", `{`},
	} {
		w := invoiceReq(t, tc.body, ZohoStub{}, true)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", tc.name, w.Code)
		}
		if c := envelopeCode(t, w); c != "invalid_request" {
			t.Fatalf("%s: want invalid_request, got %q", tc.name, c)
		}
	}
	if w := invoiceReq(t, good, ZohoStub{}, true); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("valid invoice with stub: want 503, got %d", w.Code)
	}
}

// fakeZoho is the test provider that succeeds — it exercises the 202
// path for both routes.
type fakeZoho struct{}

func (fakeZoho) CreateContact(_ context.Context, c ZohoContact) (string, error) {
	if c.DisplayName == "" {
		panic("fakeZoho: empty display name")
	}
	return "z-contact-1", nil
}

func (fakeZoho) CreateInvoice(_ context.Context, inv ZohoInvoice) (string, error) {
	if inv.Currency != "INR" {
		panic("fakeZoho: non-INR invoice")
	}
	return "z-invoice-1", nil
}

func TestZohoRealProvider202(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/integrations/zoho/contacts",
		strings.NewReader(`{"display_name":"A Buyer"}`))
	w := httptest.NewRecorder()
	authed(testZohoHandler(fakeZoho{}, true).CreateContact()).ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("contact: want 202, got %d", w.Code)
	}

	r = httptest.NewRequest(http.MethodPost, "/v1/integrations/zoho/invoices",
		strings.NewReader(`{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20261020","lines":[{"name":"Widget","quantity":1,"amount_paise":129900}]}`))
	w = httptest.NewRecorder()
	authed(testZohoHandler(fakeZoho{}, true).CreateInvoice()).ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("invoice: want 202, got %d", w.Code)
	}
}

// fakeZohoUpstream errors like a real provider hitting Zoho's API —
// the route must answer an honest 502, not a fail-closed 503.
type fakeZohoUpstream struct{}

func (fakeZohoUpstream) CreateContact(_ context.Context, _ ZohoContact) (string, error) {
	return "", context.DeadlineExceeded
}

func (fakeZohoUpstream) CreateInvoice(_ context.Context, _ ZohoInvoice) (string, error) {
	return "", context.DeadlineExceeded
}

func TestZohoUpstreamError502(t *testing.T) {
	w := contactReq(t, `{"display_name":"A Buyer"}`, fakeZohoUpstream{}, true)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("contact: want 502, got %d", w.Code)
	}
	if c := envelopeCode(t, w); c != "zoho_upstream" {
		t.Fatalf("contact: want zoho_upstream, got %q", c)
	}
	good := `{"contact_id":"c1","reference_no":"R1","currency":"INR","due_date":"20261020","lines":[{"name":"Widget","quantity":1,"amount_paise":129900}]}`
	w = invoiceReq(t, good, fakeZohoUpstream{}, true)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("invoice: want 502, got %d", w.Code)
	}
	if c := envelopeCode(t, w); c != "zoho_upstream" {
		t.Fatalf("invoice: want zoho_upstream, got %q", c)
	}
}
