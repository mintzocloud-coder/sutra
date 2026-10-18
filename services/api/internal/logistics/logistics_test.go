// SPDX-License-Identifier: AGPL-3.0-or-later
package logistics

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var testCfg = ProviderConfig{}

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

	_, err = p.CreateShipment(ctx, ShipmentParams{
		OrderRef:       "ord-1",
		SellerID:       "seller-1",
		Service:        ServiceStandard,
		WeightGrams:    500,
		COD:            true,
		CODAmountPaise: 49900,
		RecipientName:  "Ravi",
		RecipientPhone: "+919876543210",
		AddressLine1:   "42 MG Road",
		City:           "Surat",
		State:          "Gujarat",
		Pincode:        "395001",
	})
	assertDisabled(t, "CreateShipment", err)
	_, err = p.Track(ctx, "TRACK123")
	assertDisabled(t, "Track", err)
	err = p.Cancel(ctx, "ship-1")
	assertDisabled(t, "Cancel", err)
}

func assertDisabled(t *testing.T, method string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected ErrLogisticsDisabled, got nil — a nil error must mean the partner accepted the operation", method)
	}
	var perr *ErrLogisticsDisabled
	if !errors.As(err, &perr) {
		t.Fatalf("%s: expected *ErrLogisticsDisabled, got %T (%v)", method, err, err)
	}
	if !strings.Contains(perr.Error(), "disabled") {
		t.Fatalf("%s: error message should say disabled: %q", method, perr.Error())
	}
}

// TestNewProviderRejectsPartnersBeforeOnboarding is the heart of the
// fail-closed contract: asking for a real logistics partner before the
// post-investors onboarding refuses to boot — even when credentials are
// present — instead of silently booking nothing.
func TestNewProviderRejectsPartnersBeforeOnboarding(t *testing.T) {
	withCreds := ProviderConfig{APIKey: "live-api-key", Secret: "live-secret", Env: "prod"}
	for _, cfg := range []ProviderConfig{testCfg, withCreds} {
		for _, name := range []string{"delhivery", "shiprocket"} {
			p, err := NewProvider(name, cfg)
			if !errors.Is(err, errUnknownProvider) {
				t.Fatalf("NewProvider(%q, %+v): expected errUnknownProvider, got provider=%v err=%v", name, cfg, p, err)
			}
			if p != nil {
				t.Fatalf("NewProvider(%q): expected nil provider", name)
			}
			if !strings.Contains(err.Error(), "post-investors") {
				t.Fatalf("error should name the post-investors gate: %q", err.Error())
			}
		}
	}
}

func TestNewProviderRejectsUnknownNames(t *testing.T) {
	for _, name := range []string{"Delhivery", "dtdc", "bluedart", " delhivery"} {
		if _, err := NewProvider(name, testCfg); !errors.Is(err, errUnknownProvider) {
			t.Fatalf("NewProvider(%q): expected errUnknownProvider, got %v", name, err)
		}
	}
}

func TestNewProviderRejectsBadLogisticsEnv(t *testing.T) {
	_, err := NewProvider("stub", ProviderConfig{Env: "production"})
	if err == nil || !strings.Contains(err.Error(), "LOGISTICS_ENV") {
		t.Fatalf("expected LOGISTICS_ENV validation error, got %v", err)
	}
	for _, env := range []string{"", "sandbox", "prod"} {
		if _, err := NewProvider("stub", ProviderConfig{Env: env}); err != nil {
			t.Fatalf("LOGISTICS_ENV=%q should be valid: %v", env, err)
		}
	}
}

// TestStubErrorNeverLeaksSecrets guards the logging contract: the
// disabled error must be safe to log verbatim even when credentials
// were supplied in the config.
func TestStubErrorNeverLeaksSecrets(t *testing.T) {
	p, err := NewProvider("stub", ProviderConfig{APIKey: "api-123", Secret: "s3cr3t"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	_, serr := p.CreateShipment(context.Background(), ShipmentParams{})
	msg := serr.Error()
	for _, secret := range []string{"api-123", "s3cr3t"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("disabled error must not leak credentials: %q", msg)
		}
	}
}

// TestTrackingStatusEnumStability pins the wire values of the tracking
// lifecycle — the Elixir realtime consumer (F5 part 2) and the mobile
// clients switch on these strings, so they must not drift silently.
func TestTrackingStatusEnumStability(t *testing.T) {
	want := map[TrackingStatus]string{
		TrackingCreated:        "created",
		TrackingPickedUp:       "picked_up",
		TrackingInTransit:      "in_transit",
		TrackingOutForDelivery: "out_for_delivery",
		TrackingDelivered:      "delivered",
		TrackingFailedDelivery: "failed_delivery",
		TrackingCancelled:      "cancelled",
		TrackingReturned:       "returned",
	}
	for status, s := range want {
		if string(status) != s {
			t.Fatalf("TrackingStatus %v = %q, want %q", status, string(status), s)
		}
	}
}
