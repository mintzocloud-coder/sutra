// SPDX-License-Identifier: AGPL-3.0-or-later
// Package otp defines the phone-OTP seam for Sutra identity (D1).
//
// Sending a real SMS in India requires DLT registration (principal entity,
// sender headers, message templates) which happens post-incorporation.
// Until then the only registered provider is the fail-closed stub:
// constructing any other provider — or calling Send on the stub — is an
// explicit, typed error, never a silent no-op. This package therefore
// cannot accidentally send (or pretend to send) an SMS in any environment.
//
// Post-incorporation wiring: implement Sender against the DLT-registered
// SMS gateway, register it in NewSender under OTP_PROVIDER, and document
// the DLT artefacts in docs/identity.md. The stub must keep failing closed
// even then, so "stub" stays a valid OTP_PROVIDER for tests and dev.
package otp

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"regexp"
)

// Sender transmits a one-time passcode to a phone number.
//
// Implementations must be safe to call concurrently, must not log the
// code, and must return a descriptive error (never nil) when delivery did
// not happen — callers treat nil as "the user can expect an SMS".
type Sender interface {
	Send(ctx context.Context, phone, code string) error
}

// ErrSMSSendingDisabled is returned by StubSender.Send. It is a distinct
// type (not a sentinel) so callers and logs can distinguish "deliberately
// unwired" from transport failures.
type ErrSMSSendingDisabled struct {
	// Reason explains why sending is disabled in this environment.
	Reason string
}

func (e *ErrSMSSendingDisabled) Error() string {
	return "otp: SMS sending disabled: " + e.Reason
}

// StubSender is the fail-closed Sender: every Send returns
// ErrSMSSendingDisabled. It exists so the OTP seam is fully wired
// (config → constructor → call sites) long before a real gateway is,
// without any code path that could fake a delivery.
type StubSender struct{}

// Send always fails closed — it never pretends the SMS went out.
func (StubSender) Send(_ context.Context, _, _ string) error {
	return &ErrSMSSendingDisabled{
		Reason: "no SMS provider is wired — DLT registration is " +
			"post-incorporation (see docs/identity.md)",
	}
}

var errUnknownProvider = errors.New("otp: unknown SMS provider")

// NewSender builds the Sender for provider. Only "stub" (and the empty
// default, which maps to it) exists until DLT registration; anything else
// is a fail-closed error so a misconfigured OTP_PROVIDER refuses to boot
// instead of silently dropping OTPs.
func NewSender(provider string) (Sender, error) {
	switch provider {
	case "", "stub":
		return StubSender{}, nil
	default:
		return nil, fmt.Errorf("%w: %q (real SMS providers are post-incorporation; see docs/identity.md)",
			errUnknownProvider, provider)
	}
}

// codeDigits is the length of generated OTP codes. Six digits is the
// industry norm for SMS OTPs (balances memorability against the ~1M space
// combined with rate limiting and short TTLs enforced by callers).
const codeDigits = 6

// GenerateCode returns a cryptographically random numeric OTP of
// codeDigits digits, zero-padded (e.g. "042917"). Callers own the TTL,
// attempt limits, and storage — this package only mints the secret.
func GenerateCode() (string, error) {
	upper := new(big.Int).Exp(big.NewInt(10), big.NewInt(codeDigits), nil)
	n, err := rand.Int(rand.Reader, upper)
	if err != nil {
		return "", fmt.Errorf("otp: code generation failed: %w", err)
	}
	return fmt.Sprintf("%0*d", codeDigits, n.Int64()), nil
}

// e164 matches plausible E.164 phone numbers: leading +, country code
// 1-9, then 6-14 more digits (total ≤ 15 digits per ITU-T E.164).
var e164 = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// ValidatePhone reports whether phone is a plausible E.164 number.
// It is a syntax check, not proof the number exists or can receive SMS.
func ValidatePhone(phone string) bool {
	return e164.MatchString(phone)
}
