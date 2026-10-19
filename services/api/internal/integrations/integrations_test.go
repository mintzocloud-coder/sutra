// SPDX-License-Identifier: AGPL-3.0-or-later
package integrations

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// The whole seam fails closed: every stub method returns the same typed
// error, so callers can distinguish "deliberately unwired" from
// transport failures — the exact contract the real providers inherit.
func assertStubErr(t *testing.T, err error, integration string) {
	t.Helper()
	var target *ErrIntegrationDisabled
	if !errors.As(err, &target) {
		t.Fatalf("want *ErrIntegrationDisabled, got %T (%v)", err, err)
	}
	if target.Integration != integration {
		t.Fatalf("want integration %q, got %q", integration, target.Integration)
	}
}

func TestWhatsAppStubFailsClosed(t *testing.T) {
	wa, err := NewWhatsApp("stub", WhatsAppConfig{})
	if err != nil {
		t.Fatalf("NewWhatsApp(stub) failed: %v", err)
	}
	if _, err := wa.SendText(context.Background(), WhatsAppText{To: "+919999999999", Body: "hi"}); true {
		assertStubErr(t, err, "whatsapp")
	}
	if _, err := wa.SendTemplate(context.Background(), WhatsAppTemplate{To: "+919999999999", Name: "order_update", Language: "en"}); true {
		assertStubErr(t, err, "whatsapp")
	}
}

func TestTallyStubFailsClosed(t *testing.T) {
	br, err := NewTally("stub", TallyConfig{})
	if err != nil {
		t.Fatalf("NewTally(stub) failed: %v", err)
	}
	_, err = br.PostVoucher(context.Background(), TallyVoucher{Kind: TallyVoucherSales, Date: "20261020", ReferenceNo: "ord-1"})
	assertStubErr(t, err, "tally")
}

func TestZohoStubFailsClosed(t *testing.T) {
	z, err := NewZoho("stub", ZohoConfig{})
	if err != nil {
		t.Fatalf("NewZoho(stub) failed: %v", err)
	}
	if _, err := z.CreateContact(context.Background(), ZohoContact{DisplayName: "A Buyer"}); true {
		assertStubErr(t, err, "zoho-books")
	}
	if _, err := z.CreateInvoice(context.Background(), ZohoInvoice{ReferenceNo: "ord-1", Currency: "INR"}); true {
		assertStubErr(t, err, "zoho-books")
	}
}

// Real provider names refuse to boot until the post-investors
// onboarding — a misconfigured provider must shout, never silently
// degrade into the stub.
func TestRealProviderNamesRefuseBoot(t *testing.T) {
	if _, err := NewWhatsApp("cloud-api", WhatsAppConfig{}); err == nil {
		t.Fatal("NewWhatsApp(cloud-api) should fail closed until onboarding")
	} else if !errors.Is(err, errUnknownProvider) {
		t.Fatalf("want errUnknownProvider, got %v", err)
	}
	if _, err := NewTally("bridge", TallyConfig{}); err == nil {
		t.Fatal("NewTally(bridge) should fail closed until onboarding")
	}
	if _, err := NewZoho("books", ZohoConfig{}); err == nil {
		t.Fatal("NewZoho(books) should fail closed until onboarding")
	}
	// Empty = dev default, maps to the stub.
	if _, err := NewWhatsApp("", WhatsAppConfig{}); err != nil {
		t.Fatalf("NewWhatsApp(\"\") should map to stub, got %v", err)
	}
	if _, err := NewTally("", TallyConfig{}); err != nil {
		t.Fatalf("NewTally(\"\") should map to stub, got %v", err)
	}
	if _, err := NewZoho("", ZohoConfig{}); err != nil {
		t.Fatalf("NewZoho(\"\") should map to stub, got %v", err)
	}
}

// VerifyWebhookSignature is real crypto, so it gets a real vector test.
func TestVerifyWebhookSignature(t *testing.T) {
	body := []byte(`{"object":"whatsapp_business_account"}`)
	secret := "test-app-secret"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	header := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if err := VerifyWebhookSignature(body, secret, header); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// Tampered body must not verify.
	if err := VerifyWebhookSignature([]byte(`{"object":"x"}`), secret, header); err == nil {
		t.Fatal("tampered body must not verify")
	}
	// Missing prefix.
	if err := VerifyWebhookSignature(body, secret, strings.TrimPrefix(header, "sha256=")); err == nil {
		t.Fatal("missing sha256= prefix must be rejected")
	}
	// Unset secret fails closed — no callback is ever accepted.
	if err := VerifyWebhookSignature(body, "", header); err == nil {
		t.Fatal("unset secret must fail closed")
	}
	// Non-hex signature.
	if err := VerifyWebhookSignature(body, secret, "sha256=zzzz"); err == nil {
		t.Fatal("non-hex signature must be rejected")
	}
}

func TestValidatePhone(t *testing.T) {
	for _, good := range []string{"+919999999999", "+14155552671", "+441234567890"} {
		if err := ValidatePhone(good); err != nil {
			t.Errorf("ValidatePhone(%q) = %v, want nil", good, err)
		}
	}
	for _, bad := range []string{"", "9999999999", "+91", "+91999999999999999", "+91-99999-99999", "+91abc"} {
		if err := ValidatePhone(bad); err == nil {
			t.Errorf("ValidatePhone(%q) = nil, want error", bad)
		}
	}
}
