// SPDX-License-Identifier: AGPL-3.0-or-later
package otp

import (
	"context"
	"errors"
	"testing"
)

func TestNewSender(t *testing.T) {
	for _, p := range []string{"", "stub"} {
		s, err := NewSender(p)
		if err != nil {
			t.Fatalf("NewSender(%q) unexpected error: %v", p, err)
		}
		if _, ok := s.(StubSender); !ok {
			t.Fatalf("NewSender(%q) = %T, want StubSender", p, s)
		}
	}
	if _, err := NewSender("twilio"); err == nil {
		t.Fatal("NewSender(twilio) succeeded: unknown providers must fail closed")
	} else if !errors.Is(err, errUnknownProvider) {
		t.Fatalf("NewSender(twilio) error = %v, want it to wrap errUnknownProvider", err)
	}
}

func TestStubSenderFailsClosed(t *testing.T) {
	s, err := NewSender("stub")
	if err != nil {
		t.Fatalf("NewSender(stub): %v", err)
	}
	err = s.Send(context.Background(), "+919876543210", "123456")
	var disabled *ErrSMSSendingDisabled
	if !errors.As(err, &disabled) {
		t.Fatalf("StubSender.Send error = %v (%T), want *ErrSMSSendingDisabled", err, err)
	}
	if disabled.Reason == "" {
		t.Fatal("ErrSMSSendingDisabled.Reason is empty: operators need to know why")
	}
}

func TestGenerateCode(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code, err := GenerateCode()
		if err != nil {
			t.Fatalf("GenerateCode: %v", err)
		}
		if len(code) != 6 {
			t.Fatalf("GenerateCode = %q, want 6 digits", code)
		}
		for _, r := range code {
			if r < '0' || r > '9' {
				t.Fatalf("GenerateCode = %q, want digits only", code)
			}
		}
		seen[code] = true
	}
	// 200 draws from a 1M space must not all collide (sanity: real randomness).
	if len(seen) < 190 {
		t.Fatalf("GenerateCode produced only %d distinct codes in 200 draws", len(seen))
	}
}

func TestValidatePhone(t *testing.T) {
	valid := []string{"+919876543210", "+14155552671", "+442071234567"}
	for _, p := range valid {
		if !ValidatePhone(p) {
			t.Errorf("ValidatePhone(%q) = false, want true", p)
		}
	}
	invalid := []string{
		"", "9876543210", "+91 98765 43210", "+0123456789", "+1",
		"+1234567890123456", // 16 digits: over E.164 max
		"++919876543210", "tel:+919876543210",
	}
	for _, p := range invalid {
		if ValidatePhone(p) {
			t.Errorf("ValidatePhone(%q) = true, want false", p)
		}
	}
}
