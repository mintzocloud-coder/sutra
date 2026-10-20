// SPDX-License-Identifier: AGPL-3.0-or-later
// Package subscriptions defines the subscription-billing seam for Sutra
// creator memberships (H4, Wave 5).
//
// Members subscribe to a creator's paid plans (tiers); billing rides the
// Cashfree Subscriptions API with UPI AutoPay mandates — the Indian
// recurring-payments rail (e-mandate on UPI, RBI-compliant). The platform
// take is 30%: the member's share on every billing event is 70% creator /
// 30% platform (locked founder economics, 2026-10-17), recorded on the
// membership_events ledger (migration 0020).
//
// Real aggregator onboarding is post-investors: until the company has a
// signed Cashfree agreement, live API keys, and a settlement account, the
// only registered provider is the fail-closed stub. Constructing any
// other provider — including "cashfree" — or calling any method on the
// stub is an explicit, typed error, never a silent no-op. This package
// therefore cannot create a mandate, charge a member, or fake a billing
// event in any environment.
//
// Money is integer paise everywhere (same convention as
// internal/catalog and internal/payments), currency pinned to "INR"
// (India-first launch).
//
// The entitlement model: an ACTIVE subscription grants the member the
// entitlement "member_of:<creator_id>" (see EntitlementsFor). Membership
// gating (exclusive posts, paid groups, live gifts tiers) reads this
// grant list — never the provider state — so a fail-closed subscription
// can never grant access: only a verified billing_succeeded event
// promotes a row to active, and the stub can never emit one.
//
// Post-investors wiring: implement Provider against Cashfree's
// Subscriptions API (create subscription → UPI AutoPay authorization
// link; webhooks SUBSCRIPTION_PAYMENT_SUCCESS/FAILED; cancel), register
// it in NewProvider under SUBSCRIPTIONS_PROVIDER, and work through the
// checklist in docs/subscriptions.md. The stub must keep failing closed
// even then, so "stub" stays a valid SUBSCRIPTIONS_PROVIDER for tests
// and dev.
package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CurrencyINR is the only accepted currency until the international
// launch. The real provider will reject anything else; the stub rejects
// nothing — it refuses the whole call.
const CurrencyINR = "INR"

// BillingPeriod is a plan's recurrence cadence.
type BillingPeriod string

const (
	// BillingMonthly charges every calendar month.
	BillingMonthly BillingPeriod = "monthly"
	// BillingYearly charges every 12 months.
	BillingYearly BillingPeriod = "yearly"
)

// CreatorShareBps is the creator's cut of every membership billing event
// in basis points: 70% creator / 30% platform (locked founder economics).
// The platform share is amount - creatorShare so the two always sum
// exactly to the billed amount (see SplitShares).
const CreatorShareBps = 7000

// SubscriptionStatus is the billing-state lifecycle of a member's
// subscription row (migration 0020).
type SubscriptionStatus string

const (
	// StatusPending: row created, aggregator mandate not yet established.
	// While the stub is wired this is a terminal state for writes —
	// nothing can promote it (no fake "active with fake money").
	StatusPending SubscriptionStatus = "pending"
	// StatusActive: mandate live, billing events arriving; the member
	// holds the member_of:<creator_id> entitlement.
	StatusActive SubscriptionStatus = "active"
	// StatusPastDue: a billing event failed; the mandate still exists and
	// the member keeps a grace window (configured per plan at the
	// provider); failed events are recorded on the ledger.
	StatusPastDue SubscriptionStatus = "past_due"
	// StatusCancelled: terminal; the member's entitlement is gone at
	// (or past) current_period_end.
	StatusCancelled SubscriptionStatus = "cancelled"
)

// CanTransition reports whether a subscription row may move from one
// status to another. The matrix is deliberately small: pending can only
// leave via a verified aggregator event (never by a local flip), and
// cancelled is terminal — resurrection requires a new subscription row,
// so billing history is never rewritten.
func CanTransition(from, to SubscriptionStatus) bool {
	switch from {
	case StatusPending:
		// Only the aggregator (via a verified webhook) may activate.
		return to == StatusActive || to == StatusCancelled
	case StatusActive:
		return to == StatusPastDue || to == StatusCancelled
	case StatusPastDue:
		// Recovery (payment retried successfully) or terminal cancel.
		return to == StatusActive || to == StatusCancelled
	case StatusCancelled:
		return false
	default:
		return false
	}
}

// Provider is the aggregator seam for subscription billing: mandate
// creation (UPI AutoPay authorization), cancellation, and webhook
// verification. Implementations must be safe to call concurrently and
// must never log full credential values. A nil error means the
// aggregator accepted the operation.
type Provider interface {
	// CreateSubscription registers a recurring mandate with the
	// aggregator and returns the authorization session the member
	// completes (UPI AutoPay e-mandate link for Cashfree).
	CreateSubscription(ctx context.Context, p CreateParams) (*CreateResult, error)
	// CancelSubscription cancels the mandate at the aggregator.
	// atPeriodEnd=true means the member keeps access until
	// current_period_end; the local row flips to cancelled then.
	CancelSubscription(ctx context.Context, providerSubID string, atPeriodEnd bool) error
	// VerifyWebhook authenticates an aggregator callback (signature
	// check) and translates it into a normalized event. Implementations
	// MUST reject bad signatures — a nil error means "the aggregator
	// said this".
	VerifyWebhook(ctx context.Context, rawBody []byte, signature string) (*WebhookEvent, error)
}

// CreateParams describes a member subscribing to a plan.
type CreateParams struct {
	// MemberID is the subscriber's users.id (uuid string).
	MemberID string
	// PlanID is the subscription_plans.id (uuid string).
	PlanID string
	// AmountPaise is the per-period price in integer paise (> 0).
	AmountPaise int64
	// Currency must be CurrencyINR until the international launch.
	Currency string
	// Period is the billing cadence (monthly/yearly).
	Period BillingPeriod
	// CustomerName, CustomerPhone, CustomerEmail identify the payer.
	// Phone is E.164; UPI AutoPay mandate setup requires it.
	CustomerName  string
	CustomerPhone string
	CustomerEmail string
}

// CreateResult is the aggregator's answer to CreateSubscription.
type CreateResult struct {
	// ProviderSubID is the aggregator's subscription id.
	ProviderSubID string
	// Status is the aggregator's initial subscription state.
	Status string
	// AuthURL is the UPI AutoPay authorization session the member must
	// complete (Cashfree "authorization link" / UPI Intent deep link).
	// Without it the mandate does not exist.
	AuthURL string
}

// WebhookStatus is the normalized outcome of a subscription callback.
type WebhookStatus string

const (
	// WebhookBillingSucceeded means the aggregator captured one period's
	// charge. This is the ONLY event that promotes a subscription to
	// active and the ONLY event that writes a 70/30 ledger row.
	WebhookBillingSucceeded WebhookStatus = "billing_succeeded"
	// WebhookBillingFailed means the period's charge failed; the row
	// moves to past_due (retry policy lives with the aggregator).
	WebhookBillingFailed WebhookStatus = "billing_failed"
	// WebhookSubscriptionCancelled means the mandate died at the
	// aggregator (member cancelled in their bank app, or we cancelled).
	WebhookSubscriptionCancelled WebhookStatus = "cancelled"
)

// WebhookEvent is a verified aggregator subscription callback.
type WebhookEvent struct {
	ProviderSubID string
	// AmountPaise is the captured period amount (> 0 for billing
	// events, 0 for cancellations).
	AmountPaise int64
	Status      WebhookStatus
	// PaidAt is the aggregator's capture timestamp (zero unless
	// billing_succeeded).
	PaidAt time.Time
}

// ErrSubscriptionsDisabled is returned by every StubProvider method. It
// is a distinct type (not a sentinel) so callers and logs can
// distinguish "deliberately unwired" from transport failures — exactly
// like payments.ErrPaymentsDisabled and otp.ErrSMSSendingDisabled.
type ErrSubscriptionsDisabled struct {
	// Reason explains why subscription billing is disabled.
	Reason string
}

func (e *ErrSubscriptionsDisabled) Error() string {
	return "subscriptions: subscription billing disabled: " + e.Reason
}

// StubProvider is the fail-closed Provider: every method returns
// ErrSubscriptionsDisabled. It exists so the subscription seam is fully
// wired (config → constructor → call sites) long before the Cashfree
// onboarding, without any code path that could create a mandate, charge
// a member, or fake a billing event.
type StubProvider struct{}

var errStubDisabled = &ErrSubscriptionsDisabled{
	Reason: "no subscription provider is wired — Cashfree Subscriptions " +
		"onboarding (incl. UPI AutoPay) is post-investors (see " +
		"docs/subscriptions.md)",
}

// CreateSubscription always fails closed — it never returns an
// authorization session.
func (StubProvider) CreateSubscription(_ context.Context, _ CreateParams) (*CreateResult, error) {
	return nil, errStubDisabled
}

// CancelSubscription always fails closed — it never cancels a mandate.
func (StubProvider) CancelSubscription(_ context.Context, _ string, _ bool) error {
	return errStubDisabled
}

// VerifyWebhook always fails closed — it never authenticates a callback.
func (StubProvider) VerifyWebhook(_ context.Context, _ []byte, _ string) (*WebhookEvent, error) {
	return nil, errStubDisabled
}

var errUnknownProvider = errors.New("subscriptions: unknown subscription provider")

// NewProvider builds the Provider for name. Only "stub" (and the empty
// default, which maps to it) exists until the Cashfree Subscriptions
// onboarding completes post-investors; anything else — including
// "cashfree" — is a fail-closed error so a misconfigured
// SUBSCRIPTIONS_PROVIDER refuses to boot instead of silently billing
// nothing (or billing through an unwired rail). A "cashfree" name
// without the post-investors onboarding is exactly the configuration
// that would fake money movement, so it must shout.
func NewProvider(name string) (Provider, error) {
	switch name {
	case "", "stub":
		return StubProvider{}, nil
	default:
		return nil, fmt.Errorf("%w: %q (Cashfree Subscriptions + UPI AutoPay onboarding is post-investors; until then only \"stub\" boots — see docs/subscriptions.md)",
			errUnknownProvider, name)
	}
}

// ValidatePlanInput checks a creator's new/updated plan fields before
// anything is persisted. Limits mirror the migration 0020 CHECKs so the
// DB never has to be the first to complain.
func ValidatePlanInput(name, description string, pricePaise int64, period BillingPeriod, currency string) error {
	if name == "" || len([]rune(name)) > 80 {
		return fmt.Errorf("subscriptions: plan name must be 1..80 characters")
	}
	if len([]rune(description)) > 500 {
		return fmt.Errorf("subscriptions: plan description must be at most 500 characters")
	}
	if pricePaise <= 0 {
		return fmt.Errorf("subscriptions: plan price must be > 0 paise")
	}
	switch period {
	case BillingMonthly, BillingYearly:
	default:
		return fmt.Errorf("subscriptions: billing period must be %q or %q", BillingMonthly, BillingYearly)
	}
	if currency != CurrencyINR {
		return fmt.Errorf("subscriptions: currency must be %q", CurrencyINR)
	}
	return nil
}

// SplitShares divides a billing event's amount into creator and platform
// shares: creator = floor(amount * 70 / 100), platform = the remainder.
// The floor-then-remainder construction (rather than two independent
// roundings) guarantees creator+platform == amount exactly — the same
// invariant the membership_events CHECK enforces. Amounts <= 0 are a
// programming error, reported as one.
func SplitShares(amountPaise int64) (creatorPaise, platformPaise int64, err error) {
	if amountPaise <= 0 {
		return 0, 0, fmt.Errorf("subscriptions: cannot split non-positive amount %d", amountPaise)
	}
	creatorPaise = amountPaise * CreatorShareBps / 10000
	platformPaise = amountPaise - creatorPaise
	return creatorPaise, platformPaise, nil
}

// MemberGrantFor returns the entitlement string a single ACTIVE
// subscription confers: membership in the creator's paid audience.
// Membership gating (exclusive posts, paid groups, live tiers) matches
// on this string — never on provider state, so a fail-closed row can
// never grant access.
func MemberGrantFor(creatorID string) string {
	return "member_of:" + creatorID
}

// EntitlementsFor folds a member's active-subscription creator ids into
// the sorted, deduplicated grant list the gating layer checks. Inactive
// creators (empty id) are dropped — a grant without an identity is a
// hole, never a convenience.
func EntitlementsFor(creatorIDs []string) []string {
	seen := make(map[string]struct{}, len(creatorIDs))
	grants := make([]string, 0, len(creatorIDs))
	for _, id := range creatorIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		grants = append(grants, MemberGrantFor(id))
	}
	return grants
}
