// SPDX-License-Identifier: AGPL-3.0-or-later
package integrations

// Tests for the WhatsApp send route and the inbound webhook
// (H2 part 2a). All tests are DB-free: the handler's sellerID seam is
// stubbed, and the stub provider fails closed without any network.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
)

// testHandler builds a handler with a stubbed seller identity seam.
// wa may be nil (send route then answers 503); fakeOK makes the fake
// provider succeed so the 202 path is exercised.
func testWhatsAppHandler(wa WhatsAppProvider, secret string, sellerOK bool) *WhatsAppHandler {
	h := NewWhatsAppHandler(nil, wa, secret, slog.Default())
	h.sellerID = func(_ context.Context, _ string) (string, error) {
		if !sellerOK {
			return "", errNoSellerRow
		}
		return "seller-123", nil
	}
	return h
}

// authed wraps a handler with the REAL RequireAuth middleware in
// bypass mode (the test identity "sub-1"), so the handler sees the
// subject exactly as it would in production.
func authed(h http.Handler) http.Handler {
	return middleware.RequireAuth(nil, slog.Default(), middleware.AuthOptions{
		Bypass: true, BypassSubject: "sub-1",
	})(h)
}

func sendReq(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/integrations/whatsapp/send",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	h := testWhatsAppHandler(WhatsAppStub{}, "secret", true)
	authed(h.Send()).ServeHTTP(w, r)
	return w
}

func envelopeCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not the error envelope: %v (body %q)", err, w.Body.String())
	}
	return env.Error.Code
}

func TestWhatsAppSendNoSubject(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/integrations/whatsapp/send",
		strings.NewReader(`{"to":"+919876543210","text":"hi"}`))
	w := httptest.NewRecorder()
	// No auth middleware: the handler itself must refuse.
	testWhatsAppHandler(WhatsAppStub{}, "secret", true).Send().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
	if got := envelopeCode(t, w); got != "unauthorized" {
		t.Fatalf("code = %q, want unauthorized", got)
	}
}

func TestWhatsAppSendNotSeller(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/integrations/whatsapp/send",
		strings.NewReader(`{"to":"+919876543210","text":"hi"}`))
	w := httptest.NewRecorder()
	authed(testWhatsAppHandler(WhatsAppStub{}, "secret", false).Send()).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
	if got := envelopeCode(t, w); got != "not_a_seller" {
		t.Fatalf("code = %q, want not_a_seller", got)
	}
}

func TestWhatsAppSendStubFailsClosed(t *testing.T) {
	w := sendReq(t, `{"to":"+919876543210","text":"order shipped"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", w.Code)
	}
	if got := envelopeCode(t, w); got != "whatsapp_unavailable" {
		t.Fatalf("code = %q, want whatsapp_unavailable", got)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("503 without Retry-After")
	}
}

func TestWhatsAppSendNilProviderFailsClosed(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/integrations/whatsapp/send",
		strings.NewReader(`{"to":"+919876543210","text":"hi"}`))
	w := httptest.NewRecorder()
	authed(testWhatsAppHandler(nil, "secret", true).Send()).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || envelopeCode(t, w) != "whatsapp_unavailable" {
		t.Fatalf("got %d/%s, want 503/whatsapp_unavailable", w.Code, envelopeCode(t, w))
	}
}

func TestWhatsAppSendValidationMatrix(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		code  int
		ecode string
	}{
		{"bad phone", `{"to":"9876543210","text":"hi"}`, 422, "invalid_phone"},
		{"short phone", `{"to":"+123","text":"hi"}`, 422, "invalid_phone"},
		{"both text and template", `{"to":"+919876543210","text":"hi","template":{"name":"t","language":"en"}}`, 400, "invalid_request"},
		{"neither", `{"to":"+919876543210"}`, 400, "invalid_request"},
		{"empty text", `{"to":"+919876543210","text":"  "}`, 400, "invalid_request"},
		{"oversized text", `{"to":"+919876543210","text":"` + strings.Repeat("a", 4097) + `"}`, 400, "invalid_request"},
		{"template no name", `{"to":"+919876543210","template":{"name":"","language":"en"}}`, 400, "invalid_request"},
		{"template bad language", `{"to":"+919876543210","template":{"name":"t","language":"xx"}}`, 400, "invalid_request"},
		{"template bad component type", `{"to":"+919876543210","template":{"name":"t","language":"hi","components":[{"type":"image","parameters":[]}]}}`, 400, "invalid_request"},
		{"template oversized param", `{"to":"+919876543210","template":{"name":"t","language":"en","components":[{"type":"body","parameters":["` + strings.Repeat("a", 1001) + `"]}]}}`, 400, "invalid_request"},
		{"unknown field", `{"to":"+919876543210","text":"hi","extra":1}`, 400, "invalid_request"},
		{"bad json", `{"to":`, 400, "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := sendReq(t, tc.body)
			if w.Code != tc.code {
				t.Fatalf("got %d, want %d (body %s)", w.Code, tc.code, w.Body.String())
			}
			if got := envelopeCode(t, w); got != tc.ecode {
				t.Fatalf("code = %q, want %q", got, tc.ecode)
			}
		})
	}
}

// fakeWhatsApp is the test provider that succeeds — it exercises the
// 202 path the fail-closed stub can never reach.
type fakeWhatsApp struct{}

func (fakeWhatsApp) SendText(_ context.Context, m WhatsAppText) (string, error) {
	return "wamid.text-" + m.To, nil
}

func (fakeWhatsApp) SendTemplate(_ context.Context, m WhatsAppTemplate) (string, error) {
	return "wamid.tmpl-" + m.Name, nil
}

func TestWhatsAppSendSuccess(t *testing.T) {
	for _, body := range []string{
		`{"to":"+919876543210","text":"order shipped"}`,
		`{"to":"+919876543210","template":{"name":"order_update","language":"hi","components":[{"type":"body","parameters":["123"]}]}}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/integrations/whatsapp/send",
			strings.NewReader(body))
		w := httptest.NewRecorder()
		authed(testWhatsAppHandler(fakeWhatsApp{}, "secret", true).Send()).ServeHTTP(w, r)
		if w.Code != http.StatusAccepted {
			t.Fatalf("got %d, want 202 (body %s)", w.Code, w.Body.String())
		}
		var resp struct {
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.MessageID == "" {
			t.Fatalf("bad 202 body: %v (body %q)", err, w.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// Inbound webhook
// ---------------------------------------------------------------------------

func signBody(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestHubChallenge(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		target string
		code   int
		want   string // expected body on 200, or envelope code
	}{
		{"happy path", "tok123", "/internal/whatsapp/webhook?hub.mode=subscribe&hub.verify_token=tok123&hub.challenge=abc", 200, "abc"},
		{"wrong token", "tok123", "/internal/whatsapp/webhook?hub.mode=subscribe&hub.verify_token=nope&hub.challenge=abc", 403, "webhook_verify_failed"},
		{"bad mode", "tok123", "/internal/whatsapp/webhook?hub.mode=unsubscribe&hub.verify_token=tok123&hub.challenge=abc", 400, "invalid_request"},
		{"missing challenge", "tok123", "/internal/whatsapp/webhook?hub.mode=subscribe&hub.verify_token=tok123", 400, "invalid_request"},
		{"unset secret fails closed", "", "/internal/whatsapp/webhook?hub.mode=subscribe&hub.verify_token=tok123&hub.challenge=abc", 401, "whatsapp_webhook_unconfigured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.target, nil)
			w := httptest.NewRecorder()
			testWhatsAppHandler(nil, tc.secret, true).Webhook().ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatalf("got %d, want %d (body %s)", w.Code, tc.code, w.Body.String())
			}
			if tc.code == 200 {
				if got := strings.TrimSpace(w.Body.String()); got != tc.want {
					t.Fatalf("challenge = %q, want %q", got, tc.want)
				}
			} else if got := envelopeCode(t, w); got != tc.want {
				t.Fatalf("code = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInboundCallback(t *testing.T) {
	body := []byte(`{"object":"whatsapp_business_account","entry":[{"changes":[{"value":{"messaging_product":"whatsapp","messages":[{}],"statuses":[{},{}]}}]}]}`)
	cases := []struct {
		name    string
		secret  string
		headers map[string]string
		body    []byte
		code    int
	}{
		{"happy path", "s3cret", map[string]string{"X-Hub-Signature-256": signBody(t, "s3cret", body)}, body, 200},
		{"bad signature", "s3cret", map[string]string{"X-Hub-Signature-256": signBody(t, "wrong", body)}, body, 401},
		{"missing header", "s3cret", nil, body, 401},
		{"unset secret fails closed", "", map[string]string{"X-Hub-Signature-256": signBody(t, "", body)}, body, 401},
		{"bad json", "s3cret", map[string]string{"X-Hub-Signature-256": "x"}, []byte("{nope"), 401}, // sig check runs first
		{"foreign object", "s3cret", map[string]string{"X-Hub-Signature-256": "x"}, []byte(`{"object":"page"}`), 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/internal/whatsapp/webhook", bytes.NewReader(tc.body))
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			testWhatsAppHandler(nil, tc.secret, true).Webhook().ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatalf("got %d, want %d (body %s)", w.Code, tc.code, w.Body.String())
			}
		})
	}
}

func TestInboundCallbackValidSigBadJSON(t *testing.T) {
	body := []byte(`{not json`)
	r := httptest.NewRequest(http.MethodPost, "/internal/whatsapp/webhook", bytes.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", signBody(t, "s3cret", body))
	w := httptest.NewRecorder()
	testWhatsAppHandler(nil, "s3cret", true).Webhook().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || envelopeCode(t, w) != "invalid_request" {
		t.Fatalf("got %d/%s, want 400/invalid_request", w.Code, envelopeCode(t, w))
	}
}

func TestInboundCallbackValidSigForeignObject(t *testing.T) {
	body := []byte(`{"object":"instagram"}`)
	r := httptest.NewRequest(http.MethodPost, "/internal/whatsapp/webhook", bytes.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", signBody(t, "s3cret", body))
	w := httptest.NewRecorder()
	testWhatsAppHandler(nil, "s3cret", true).Webhook().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || envelopeCode(t, w) != "invalid_request" {
		t.Fatalf("got %d/%s, want 400/invalid_request", w.Code, envelopeCode(t, w))
	}
}

func TestValidateSendRequestUnit(t *testing.T) {
	ok := &WhatsAppSendRequest{To: "+919876543210", Text: "hi"}
	if code, _ := validateSendRequest(ok); code != "" {
		t.Fatalf("valid text request rejected: %s", code)
	}
	tmpl := &WhatsAppSendRequest{To: "+14155552671"}
	tmpl.Template = &struct {
		Name       string `json:"name"`
		Language   string `json:"language"`
		Components []struct {
			Type       string   `json:"type"`
			Parameters []string `json:"parameters"`
		} `json:"components,omitempty"`
	}{Name: "order_update", Language: "gu"}
	if code, _ := validateSendRequest(tmpl); code != "" {
		t.Fatalf("valid template request rejected: %s", code)
	}
}
