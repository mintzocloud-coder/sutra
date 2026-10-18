// SPDX-License-Identifier: AGPL-3.0-or-later
// Package payments defines the payment-aggregator seam for Sutra commerce (F4).
//
// Real payment-aggregator onboarding (Cashfree) is post-investors: until the
// company has a signed agreement, live API keys, and a bank settlement
// account, the only registered provider is the fail-closed stub.
// Constructing any other provider — or calling any method on the stub — is
// an explicit, typed error, never a silent no-op. This package therefore
// cannot charge, split, or pay out a single paise in any environment.
//
// Money is integer paise everywhere (same convention as internal/catalog),
// and currency is pinned to "INR" — India-first launch; international
// rails are a post-launch decision.
//
// Post-investors wiring: implement Provider against Cashfree's APIs (PG
// orders + webhooks, Easy Split transfers, payouts), register it in
// NewProvider under PAYMENTS_PROVIDER, and document the onboarding
// artefacts in docs/payments.md. The stub must keep failing closed even
// then, so "stub" stays a valid PAYMENTS_PROVIDER for tests and dev.
package payments

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CurrencyINR is the only accepted currency until the international
// launch. OrderParams carrying anything else are rejected by the real
// provider; the stub rejects nothing — it refuses the whole call.
const CurrencyINR = "INR"

// Provider is the payment-aggregator seam: order intake (UPI Intent-first),
// webhook verification, vendor split transfers, and vendor payouts.
//
// Implementations must be safe to call concurrently and must never log
// full credential values. A nil error means the aggregator accepted the
// operation — callers treat a nil error on CreateOrder as "the buyer can
// be sent to payment", exactly like otp.Sender's nil-means-delivery
// contract.
type Provider interface {
	// CreateOrder registers a payment with the aggregator and returns the
	// session the client pays through (UPI Intent deep link or web page).
	CreateOrder(ctx context.Context, p OrderParams) (*OrderResult, error)
	// VerifyWebhook authenticates an aggregator callback (signature check)
	// and translates it into a normalized event. Implementations MUST
	// reject bad signatures — a nil error means "the aggregator said this".
	VerifyWebhook(ctx context.Context, rawBody []byte, signature string) (*WebhookEvent, error)
	// SplitTransfer moves the vendor's share of a paid order via the
	// aggregator's split product (Cashfree Easy Split design).
	SplitTransfer(ctx context.Context, p SplitParams) (*SplitResult, error)
	// Payout settles accumulated vendor earnings to their bank/UPI.
	Payout(ctx context.Context, p PayoutParams) (*PayoutResult, error)
}

// OrderParams describes a payment the buyer is about to make.
type OrderParams struct {
	// OrderRef is our internal order id (uuid string) — the idempotency
	// key at the aggregator; retried checkouts reuse it.
	OrderRef string
	// AmountPaise is the chargeable total in integer paise (> 0).
	AmountPaise int64
	// Currency must be CurrencyINR until the international launch.
	Currency string
	// BuyerName, BuyerPhone, BuyerEmail identify the payer. Phone is
	// E.164; UPI Intent flows require it.
	BuyerName  string
	BuyerPhone string
	BuyerEmail string
	// UPIIntent requests a upi:// deep link (UPI Intent-first checkout)
	// instead of the hosted web payment page.
	UPIIntent bool
}

// OrderResult is the aggregator's answer to CreateOrder.
type OrderResult struct {
	// ProviderOrderID is the aggregator's order/token id.
	ProviderOrderID string
	// SessionID is the payment session id (Cashfree "payment_session_id").
	SessionID string
	// PaymentLink is the hosted web fallback URL.
	PaymentLink string
	// UPIIntentURL is the upi:// deep link, set only when UPIIntent was
	// requested and the aggregator supports it.
	UPIIntentURL string
}

// WebhookStatus is the normalized outcome of a payment callback.
type WebhookStatus string

const (
	// WebhookPaid means the aggregator confirms captured funds.
	WebhookPaid WebhookStatus = "paid"
	// WebhookFailed means the payment attempt failed or was cancelled.
	WebhookFailed WebhookStatus = "failed"
)

// WebhookEvent is a verified aggregator callback.
type WebhookEvent struct {
	ProviderOrderID string
	OrderRef        string
	AmountPaise     int64
	Status          WebhookStatus
	// PaidAt is the aggregator's capture timestamp (zero for failures).
	PaidAt time.Time
}

// SplitParams describes a vendor's share of one paid order. In the
// v1 0%-commission model (see migration 0008) the platform keeps 0 and
// the vendor receives the full order total; CommissionBps is recorded
// for reconciliation and future pricing changes.
type SplitParams struct {
	// OrderRef is the paid order this transfer settles.
	OrderRef string
	// VendorAccount is the aggregator's beneficiary id for the seller.
	VendorAccount string
	// AmountPaise is the vendor's share in integer paise (> 0).
	AmountPaise int64
	// CommissionBps is the platform take in basis points (0 in v1).
	CommissionBps int64
}

// SplitResult is the aggregator's answer to SplitTransfer.
type SplitResult struct {
	TransferID string
	Status     string
}

// PayoutParams describes a vendor earnings settlement.
type PayoutParams struct {
	// BeneficiaryID is the aggregator's beneficiary id for the seller.
	BeneficiaryID string
	// AmountPaise is the payout total in integer paise (> 0).
	AmountPaise int64
	// Mode is "upi", "imps", or "neft".
	Mode string
}

// PayoutResult is the aggregator's answer to Payout.
type PayoutResult struct {
	PayoutID string
	Status   string
}

// ErrPaymentsDisabled is returned by every StubProvider method. It is a
// distinct type (not a sentinel) so callers and logs can distinguish
// "deliberately unwired" from transport failures — exactly like
// otp.ErrSMSSendingDisabled.
type ErrPaymentsDisabled struct {
	// Reason explains why payments are disabled in this environment.
	Reason string
}

func (e *ErrPaymentsDisabled) Error() string {
	return "payments: payment processing disabled: " + e.Reason
}

// StubProvider is the fail-closed Provider: every method returns
// ErrPaymentsDisabled. It exists so the payments seam is fully wired
// (config → constructor → call sites) long before the Cashfree
// onboarding, without any code path that could charge a buyer, move a
// vendor's money, or fake a payment.
type StubProvider struct{}

var errStubDisabled = &ErrPaymentsDisabled{
	Reason: "no payment aggregator is wired — Cashfree onboarding is " +
		"post-investors (see docs/payments.md)",
}

// CreateOrder always fails closed — it never returns a usable session.
func (StubProvider) CreateOrder(_ context.Context, _ OrderParams) (*OrderResult, error) {
	return nil, errStubDisabled
}

// VerifyWebhook always fails closed — it never authenticates a callback.
func (StubProvider) VerifyWebhook(_ context.Context, _ []byte, _ string) (*WebhookEvent, error) {
	return nil, errStubDisabled
}

// SplitTransfer always fails closed — it never moves vendor money.
func (StubProvider) SplitTransfer(_ context.Context, _ SplitParams) (*SplitResult, error) {
	return nil, errStubDisabled
}

// Payout always fails closed — it never settles earnings.
func (StubProvider) Payout(_ context.Context, _ PayoutParams) (*PayoutResult, error) {
	return nil, errStubDisabled
}

// CashfreeConfig carries the future Cashfree credentials. Until the
// aggregator onboarding completes post-investors the values are loaded,
// never logged (not even the App ID), and ignored by the stub — a real
// provider name refuses to boot regardless of whether keys are present.
type CashfreeConfig struct {
	AppID  string
	Secret string
	// Env is "sandbox" or "prod"; "" means unset. The real adapter will
	// refuse to boot without AppID+Secret; sandbox-vs-prod selects the
	// endpoint it talks to.
	Env string
}

var errUnknownProvider = errors.New("payments: unknown payment provider")

// NewProvider builds the Provider for name with the Cashfree credentials.
// Only "stub" (and the empty default, which maps to it) exists until the
// Cashfree onboarding completes post-investors; anything else — including
// "cashfree" — is a fail-closed error so a misconfigured
// PAYMENTS_PROVIDER refuses to boot instead of silently taking no
// payments. A malformed CASHFREE_ENV is likewise a boot-time error: bad
// config must shout, never be silently ignored.
func NewProvider(name string, cf CashfreeConfig) (Provider, error) {
	switch cf.Env {
	case "", "sandbox", "prod":
		// Valid: unset (dev default) or an explicit environment.
	default:
		return nil, fmt.Errorf("payments: invalid CASHFREE_ENV %q: must be \"sandbox\" or \"prod\"", cf.Env)
	}
	switch name {
	case "", "stub":
		return StubProvider{}, nil
	default:
		return nil, fmt.Errorf("%w: %q (Cashfree onboarding is post-investors; until then only \"stub\" boots — see docs/payments.md)",
			errUnknownProvider, name)
	}
}
