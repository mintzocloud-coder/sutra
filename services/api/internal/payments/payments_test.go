// SPDX-License-Identifier: AGPL-3.0-or-later
package payments

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var testCfg = CashfreeConfig{}

func TestNewProviderDefaultsToStub(t *testing.T) {
	for _, name := range []string{"", "stub"} {
		p, err := NewProvider(name, testCfg)
		if err != nil {
			t.Fatalf("NewProvider(%q): unexpected error: %v", name, err)
		}
		if _, ok := p.(StubProvider); !ok {
			t.Fatalf("NewProvider(%q): got %T, want StubProvider", name, p)
		}
	}
}

func TestStubProviderFailsClosedOnEveryMethod(t *testing.T) {
	p, err := NewProvider("stub", testCfg)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	ctx := context.Background()

	_, err = p.CreateOrder(ctx, OrderParams{OrderRef: "o1", AmountPaise: 19900, Currency: CurrencyINR})
	assertDisabled(t, "CreateOrder", err)
	_, err = p.VerifyWebhook(ctx, []byte(`{"x":1}`), "sig")
	assertDisabled(t, "VerifyWebhook", err)
	_, err = p.SplitTransfer(ctx, SplitParams{OrderRef: "o1", VendorAccount: "v1", AmountPaise: 19900})
	assertDisabled(t, "SplitTransfer", err)
	_, err = p.Payout(ctx, PayoutParams{BeneficiaryID: "v1", AmountPaise: 19900, Mode: "upi"})
	assertDisabled(t, "Payout", err)
}

func assertDisabled(t *testing.T, method string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected ErrPaymentsDisabled, got nil — a nil error must mean the aggregator accepted the operation", method)
	}
	var perr *ErrPaymentsDisabled
	if !errors.As(err, &perr) {
		t.Fatalf("%s: expected *ErrPaymentsDisabled, got %T (%v)", method, err, err)
	}
	if !strings.Contains(perr.Error(), "disabled") {
		t.Fatalf("%s: error message should say disabled: %q", method, perr.Error())
	}
}

// TestNewProviderRejectsCashfreeBeforeOnboarding is the heart of the
// fail-closed contract: asking for the real aggregator before the
// post-investors onboarding refuses to boot — even when credentials
// are present — instead of silently running without payments.
func TestNewProviderRejectsCashfreeBeforeOnboarding(t *testing.T) {
	withCreds := CashfreeConfig{AppID: "live-app-id", Secret: "live-secret", Env: "prod"}
	for _, cfg := range []CashfreeConfig{testCfg, withCreds} {
		p, err := NewProvider("cashfree", cfg)
		if !errors.Is(err, errUnknownProvider) {
			t.Fatalf("NewProvider(cashfree, %+v): expected errUnknownProvider, got provider=%v err=%v", cfg, p, err)
		}
		if p != nil {
			t.Fatalf("NewProvider(cashfree): expected nil provider, got %v", p)
		}
		if !strings.Contains(err.Error(), "post-investors") {
			t.Fatalf("error should name the post-investors gate: %q", err.Error())
		}
	}
}

func TestNewProviderRejectsUnknownNames(t *testing.T) {
	for _, name := range []string{"CashFree", "razorpay", "stripe", " cashfree"} {
		if _, err := NewProvider(name, testCfg); !errors.Is(err, errUnknownProvider) {
			t.Fatalf("NewProvider(%q): expected errUnknownProvider, got %v", name, err)
		}
	}
}

func TestNewProviderRejectsBadCashfreeEnv(t *testing.T) {
	_, err := NewProvider("stub", CashfreeConfig{Env: "production"})
	if err == nil || !strings.Contains(err.Error(), "CASHFREE_ENV") {
		t.Fatalf("expected CASHFREE_ENV validation error, got %v", err)
	}
	for _, env := range []string{"", "sandbox", "prod"} {
		if _, err := NewProvider("stub", CashfreeConfig{Env: env}); err != nil {
			t.Fatalf("CASHFREE_ENV=%q should be valid: %v", env, err)
		}
	}
}

// TestStubErrorNeverLeaksSecrets guards the logging contract: the
// disabled error must be safe to log verbatim even when credentials
// were supplied in the config.
func TestStubErrorNeverLeaksSecrets(t *testing.T) {
	p, err := NewProvider("stub", CashfreeConfig{AppID: "app-123", Secret: "s3cr3t"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	_, serr := p.CreateOrder(context.Background(), OrderParams{})
	msg := serr.Error()
	for _, secret := range []string{"app-123", "s3cr3t"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("disabled error must not leak credentials: %q", msg)
		}
	}
}
