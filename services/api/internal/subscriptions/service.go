// SPDX-License-Identifier: AGPL-3.0-or-later
// Package subscriptions — DB service layer (H4 part 2).
//
// The PlanService owns creator membership plans (CRUD over
// subscription_plans, migration 0020). The SubscriptionService owns the
// member-plan billing relationship: subscribing (provider mandate first,
// local row second), member cancellation, and the webhook promotion
// transaction that turns a verified aggregator event into a lifecycle
// transition + an append-only membership_events ledger row + a
// creator_earnings accrual — all three in one database transaction.
//
// Fail-closed discipline (unchanged from part 1): while the stub is the
// only wired provider, Subscribe and CancelMember cannot touch the DB —
// the provider refuses first, so a row can never exist in a state that
// implies money moved. The ONLY way a row leaves "pending" is a
// verified billing_succeeded webhook, and the stub can never emit one.
//
// Money is integer paise; currency is pinned to INR (ValidatePlanInput
// enforces both before any write). The 70/30 split (locked founder
// economics) is SplitShares: creator = floor(amount*70/100), platform =
// the remainder, so the two always sum to the billed amount — the same
// invariant the membership_events CHECK enforces.
//
// Accrual vs. movement: billing_succeeded is the only event that grows
// creator_earnings.pending_paise (an honest record of what the platform
// owes). Payouts ride payments.Provider.Payout once the aggregator is
// live (F4) — nothing here moves money.
package subscriptions

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

// Sentinel errors; handlers map them to the error envelope.
var (
	ErrPlanNotFound         = errors.New("subscription plan not found")
	ErrSubscriptionNotFound = errors.New("subscription not found")
	ErrNotOwner             = errors.New("not the owner of this subscription resource")
	ErrAlreadySubscribed    = errors.New("an active or past-due subscription on this plan already exists")
	ErrBadTransition        = errors.New("subscription status transition not allowed")
	ErrWebhookNoRow         = errors.New("no subscription matches this provider reference")
)

// Plan is a creator's membership tier (subscription_plans row).
type Plan struct {
	ID            pgtype.UUID   `json:"id"`
	CreatorID     pgtype.UUID   `json:"creator_id"`
	Name          string        `json:"name"`
	Description   string        `json:"description"`
	PricePaise    int64         `json:"price_paise"`
	BillingPeriod BillingPeriod `json:"billing_period"`
	Currency      string        `json:"currency"`
	IsActive      bool          `json:"is_active"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

// Subscription is a member-plan billing relationship
// (subscriptions row).
type Subscription struct {
	ID                 pgtype.UUID        `json:"id"`
	SubscriberID       pgtype.UUID        `json:"subscriber_id"`
	PlanID             pgtype.UUID        `json:"plan_id"`
	Status             SubscriptionStatus `json:"status"`
	ProviderRef        *string            `json:"provider_ref,omitempty"`
	CurrentPeriodStart *time.Time         `json:"current_period_start,omitempty"`
	CurrentPeriodEnd   *time.Time         `json:"current_period_end,omitempty"`
	CancelAtPeriodEnd  bool               `json:"cancel_at_period_end"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
}

// CreateParams validation for the subscribe call lives in the handler;
// the service validates the plan side here.

// PlanService is the DB store for membership plans.
type PlanService struct{ pool *db.Pool }

// NewPlanService builds the plan store on an existing pool.
func NewPlanService(pool *db.Pool) *PlanService { return &PlanService{pool: pool} }

func scanPlan(row pgx.Row) (*Plan, error) {
	var p Plan
	if err := row.Scan(&p.ID, &p.CreatorID, &p.Name, &p.Description,
		&p.PricePaise, &p.BillingPeriod, &p.Currency, &p.IsActive,
		&p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

const planCols = `id, creator_id, name, description, price_paise, billing_period, currency, is_active, created_at, updated_at`

// CreatePlan inserts a creator's new membership tier. Price and period
// are immutable after creation (see UpdatePlan) so an existing
// member's terms can never be rewritten under them — a new tier is a
// new plan row.
func (s *PlanService) CreatePlan(ctx context.Context, creatorID pgtype.UUID, name, description string, pricePaise int64, period BillingPeriod, currency string) (*Plan, error) {
	if err := ValidatePlanInput(name, description, pricePaise, period, currency); err != nil {
		return nil, err
	}
	return scanPlan(s.pool.QueryRow(ctx,
		`INSERT INTO subscription_plans (creator_id, name, description, price_paise, billing_period, currency)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+planCols,
		creatorID, name, description, pricePaise, string(period), currency))
}

// GetPlan returns the plan only to its creator — plans of other
// creators are 404 (never leaks existence).
func (s *PlanService) GetPlan(ctx context.Context, creatorID, planID pgtype.UUID) (*Plan, error) {
	p, err := scanPlan(s.pool.QueryRow(ctx,
		`SELECT `+planCols+` FROM subscription_plans WHERE id = $1 AND creator_id = $2`,
		planID, creatorID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPlanNotFound
	}
	return p, err
}

// GetPlanPublic returns an ACTIVE plan to anyone (a member needs the
// plan's price/period before subscribing). Inactive or missing plans
// are 404 — an inactive plan is not subscribable, full stop.
func (s *PlanService) GetPlanPublic(ctx context.Context, planID pgtype.UUID) (*Plan, error) {
	p, err := scanPlan(s.pool.QueryRow(ctx,
		`SELECT `+planCols+` FROM subscription_plans WHERE id = $1 AND is_active`,
		planID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPlanNotFound
	}
	return p, err
}

// ListPlans returns the creator's plans, newest first. Limit is
// clamped (cost guard); the count stays small in practice.
func (s *PlanService) ListPlans(ctx context.Context, creatorID pgtype.UUID, limit int) ([]*Plan, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+planCols+` FROM subscription_plans WHERE creator_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`,
		creatorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePlanInput carries the mutable plan fields. Price and period are
// deliberately absent: they are the contract the member signed, and
// changing them under an existing subscription would rewrite billing
// terms retroactively. Rename/reword/deactivate is the allowed surface.
type UpdatePlanInput struct {
	Name        *string
	Description *string
	IsActive    *bool
}

// UpdatePlan applies the mutable fields, owner-only.
func (s *PlanService) UpdatePlan(ctx context.Context, creatorID, planID pgtype.UUID, in UpdatePlanInput) (*Plan, error) {
	p, err := s.GetPlan(ctx, creatorID, planID)
	if err != nil {
		return nil, err
	}
	name, desc, active := p.Name, p.Description, p.IsActive
	if in.Name != nil {
		name = *in.Name
	}
	if in.Description != nil {
		desc = *in.Description
	}
	if in.IsActive != nil {
		active = *in.IsActive
	}
	// Re-validate against the same limits as creation (price/period
	// unchanged, so ValidatePlanInput with the existing values).
	if err := ValidatePlanInput(name, desc, p.PricePaise, p.BillingPeriod, p.Currency); err != nil {
		return nil, err
	}
	return scanPlan(s.pool.QueryRow(ctx,
		`UPDATE subscription_plans SET name = $1, description = $2, is_active = $3
		 WHERE id = $4 AND creator_id = $5 RETURNING `+planCols,
		name, desc, active, planID, creatorID))
}

// DeactivatePlan is the DELETE semantic: plans are never hard-deleted
// (subscriptions references them ON DELETE RESTRICT) — the row is
// flipped inactive so no new member can subscribe while history and
// accruals stay intact.
func (s *PlanService) DeactivatePlan(ctx context.Context, creatorID, planID pgtype.UUID) (*Plan, error) {
	inactive := false
	return s.UpdatePlan(ctx, creatorID, planID, UpdatePlanInput{IsActive: &inactive})
}

// SubscriptionService is the DB store + provider seam for member-plan
// billing relationships. The provider is consulted before any local
// write that implies money movement (fail-closed).
type SubscriptionService struct {
	pool     *db.Pool
	provider Provider
	plans    *PlanService
}

// NewSubscriptionService builds the subscription store.
func NewSubscriptionService(pool *db.Pool, provider Provider, plans *PlanService) *SubscriptionService {
	return &SubscriptionService{pool: pool, provider: provider, plans: plans}
}

func scanSubscription(row pgx.Row) (*Subscription, error) {
	var s Subscription
	var status string
	var providerRef pgtype.Text
	var start, end pgtype.Timestamptz
	if err := row.Scan(&s.ID, &s.SubscriberID, &s.PlanID, &status, &providerRef,
		&start, &end, &s.CancelAtPeriodEnd, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	s.Status = SubscriptionStatus(status)
	if providerRef.Valid {
		v := providerRef.String
		s.ProviderRef = &v
	}
	if start.Valid {
		v := start.Time
		s.CurrentPeriodStart = &v
	}
	if end.Valid {
		v := end.Time
		s.CurrentPeriodEnd = &v
	}
	return &s, nil
}

const subCols = `id, subscriber_id, plan_id, status, provider_ref, current_period_start, current_period_end, cancel_at_period_end, created_at, updated_at`

// SubscribeResult is what a successful subscribe returns: the local row
// (pending until the mandate's first billing event) plus the
// authorization session the member must complete (UPI AutoPay
// e-mandate) when the provider is real.
type SubscribeResult struct {
	Subscription *Subscription
	AuthURL      string
}

// Subscribe starts a member's subscription to an active plan. The
// provider mandate is created FIRST; only its success inserts the
// local pending row. With the stub provider this fails closed BEFORE
// any row exists — the 503 the handler answers is the honest state:
// no mandate exists anywhere, so nothing was subscribed.
func (s *SubscriptionService) Subscribe(ctx context.Context, memberID pgtype.UUID, planID pgtype.UUID, customerName, customerPhone, customerEmail string) (*SubscribeResult, error) {
	plan, err := s.plans.GetPlanPublic(ctx, planID)
	if err != nil {
		return nil, err
	}
	res, err := s.provider.CreateSubscription(ctx, CreateParams{
		MemberID:      uuidString(memberID),
		PlanID:        uuidString(planID),
		AmountPaise:   plan.PricePaise,
		Currency:      plan.Currency,
		Period:        plan.BillingPeriod,
		CustomerName:  customerName,
		CustomerPhone: customerPhone,
		CustomerEmail: customerEmail,
	})
	if err != nil {
		return nil, err
	}
	sub, err := scanSubscription(s.pool.QueryRow(ctx,
		`INSERT INTO subscriptions (subscriber_id, plan_id, status, provider_ref)
		 VALUES ($1, $2, 'pending', $3) RETURNING `+subCols,
		memberID, planID, res.ProviderSubID))
	if err != nil {
		if isUniqueViolation(err) {
			// A live row raced us (the partial unique index is the
			// source of truth for double-subscribe).
			return nil, ErrAlreadySubscribed
		}
		return nil, err
	}
	return &SubscribeResult{Subscription: sub, AuthURL: res.AuthURL}, nil
}

// GetSubscription returns the member's own subscription row (404 for
// anyone else's — never leaks existence).
func (s *SubscriptionService) GetSubscription(ctx context.Context, memberID, subID pgtype.UUID) (*Subscription, error) {
	sub, err := scanSubscription(s.pool.QueryRow(ctx,
		`SELECT `+subCols+` FROM subscriptions WHERE id = $1 AND subscriber_id = $2`,
		subID, memberID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSubscriptionNotFound
	}
	return sub, err
}

// ListMine returns the member's subscriptions, newest first.
func (s *SubscriptionService) ListMine(ctx context.Context, memberID pgtype.UUID, limit int) ([]*Subscription, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+subCols+` FROM subscriptions WHERE subscriber_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`,
		memberID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Subscription
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// ActiveCreatorIDs returns the creator ids the member holds an ACTIVE
// subscription with — the raw material for the member_of:<creator_id>
// grants (see EntitlementsFor). Gating layers call this, never the
// provider.
func (s *SubscriptionService) ActiveCreatorIDs(ctx context.Context, memberID pgtype.UUID) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT sp.creator_id FROM subscriptions s
		 JOIN subscription_plans sp ON sp.id = s.plan_id
		 WHERE s.subscriber_id = $1 AND s.status = 'active'`,
		memberID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, uuidString(id))
	}
	return out, rows.Err()
}

// CancelMember cancels the member's subscription at the provider, then
// records the outcome locally. atPeriodEnd=true keeps the member's
// access until current_period_end (the row stays active and flips on
// the provider's cancelled webhook); false cancels now. The provider
// is cancelled FIRST — with the stub this fails closed before any
// local write, so a "cancelled with no mandate" row can never exist.
func (s *SubscriptionService) CancelMember(ctx context.Context, memberID, subID pgtype.UUID, atPeriodEnd bool) (*Subscription, error) {
	sub, err := s.GetSubscription(ctx, memberID, subID)
	if err != nil {
		return nil, err
	}
	if sub.Status != StatusActive && sub.Status != StatusPastDue {
		// pending has no mandate to cancel; cancelled is terminal.
		return nil, ErrBadTransition
	}
	if sub.ProviderRef == nil || *sub.ProviderRef == "" {
		// No aggregator mandate exists (fail-closed invariant) — there
		// is nothing to cancel at the provider, and flipping the row
		// locally would claim a cancellation that never happened.
		return nil, &ErrSubscriptionsDisabled{Reason: "no aggregator mandate exists for this subscription"}
	}
	if err := s.provider.CancelSubscription(ctx, *sub.ProviderRef, atPeriodEnd); err != nil {
		return nil, err
	}
	// The plan's price is the honest ledger amount for a cancellation
	// (no money was captured; the row is memo-only, never accrued).
	pricePaise, err := s.planPricePaise(ctx, sub.PlanID)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if atPeriodEnd {
		sub, err = scanSubscription(tx.QueryRow(ctx,
			`UPDATE subscriptions SET cancel_at_period_end = true WHERE id = $1 RETURNING `+subCols,
			subID))
	} else {
		sub, err = s.transitionLocked(ctx, tx, sub, StatusCancelled, pricePaise, sub.ProviderRef)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return sub, nil
}

// planPricePaise returns a plan's price regardless of its active flag
// (a member cancelling an inactive plan's subscription still needs the
// honest ledger amount). Price is not a secret: members already saw it
// when they subscribed.
func (s *SubscriptionService) planPricePaise(ctx context.Context, planID pgtype.UUID) (int64, error) {
	var price int64
	err := s.pool.QueryRow(ctx,
		`SELECT price_paise FROM subscription_plans WHERE id = $1`, planID).Scan(&price)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrPlanNotFound
	}
	return price, err
}

// HandleWebhook applies a VERIFIED aggregator event (the handler runs
// Provider.VerifyWebhook first — this method must never see an
// unverified payload) to the subscription identified by ev.ProviderSubID,
// in a single transaction: lifecycle transition + ledger row +
// creator_earnings accrual. Duplicate deliveries of the same event are
// idempotent (matched on subscription + event type + amount +
// provider ref); the first delivery wins and redeliveries no-op.
func (s *SubscriptionService) HandleWebhook(ctx context.Context, ev *WebhookEvent) (*Subscription, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the subscription row by provider reference, joining the plan
	// for its price, period, and creator.
	var sub Subscription
	var status string
	var providerRef pgtype.Text
	var start, end pgtype.Timestamptz
	var planPrice int64
	var planPeriod string
	var creatorID pgtype.UUID
	err = tx.QueryRow(ctx,
		`SELECT `+prefixedSubCols("s")+`, sp.price_paise, sp.billing_period, sp.creator_id
		 FROM subscriptions s JOIN subscription_plans sp ON sp.id = s.plan_id
		 WHERE s.provider_ref = $1 FOR UPDATE OF s`,
		ev.ProviderSubID).Scan(
		&sub.ID, &sub.SubscriberID, &sub.PlanID, &status, &providerRef,
		&start, &end, &sub.CancelAtPeriodEnd, &sub.CreatedAt, &sub.UpdatedAt,
		&planPrice, &planPeriod, &creatorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWebhookNoRow
	}
	if err != nil {
		return nil, err
	}
	sub.Status = SubscriptionStatus(status)
	if providerRef.Valid {
		v := providerRef.String
		sub.ProviderRef = &v
	}
	if start.Valid {
		v := start.Time
		sub.CurrentPeriodStart = &v
	}
	if end.Valid {
		v := end.Time
		sub.CurrentPeriodEnd = &v
	}

	amount := ev.AmountPaise
	if amount <= 0 {
		// Cancellations carry no captured money; the ledger still
		// requires amount > 0, so the plan price is the honest memo.
		amount = planPrice
	}

	// Idempotency: the same provider delivery twice must not double the
	// ledger or the accrual.
	var dup bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM membership_events
		 WHERE subscription_id = $1 AND event_type = $2 AND amount_paise = $3
		   AND provider_ref IS NOT DISTINCT FROM $4)`,
		sub.ID, string(ev.Status), amount, nullableText(ev.ProviderSubID)).Scan(&dup); err != nil {
		return nil, err
	}

	var target SubscriptionStatus
	switch ev.Status {
	case WebhookBillingSucceeded:
		// The only event that promotes to active and the only event
		// that accrues creator earnings.
		if !CanTransition(sub.Status, StatusActive) {
			if sub.Status == StatusActive && dup {
				// Redelivered success after an already-active row:
				// nothing to do.
				return &sub, tx.Commit(ctx)
			}
			return nil, ErrBadTransition
		}
		target = StatusActive
	case WebhookBillingFailed:
		// A failed charge on a pending mandate is not past-due — the
		// mandate was never established. The ledger still records the
		// attempt; the row stays pending.
		if sub.Status == StatusPending {
			if err := s.insertLedgerRow(ctx, tx, &sub, sub.PlanID, string(ev.Status), amount, nullableText(ev.ProviderSubID)); err != nil {
				return nil, err
			}
			return &sub, tx.Commit(ctx)
		}
		if !CanTransition(sub.Status, StatusPastDue) {
			return nil, ErrBadTransition
		}
		target = StatusPastDue
	case WebhookSubscriptionCancelled:
		if !CanTransition(sub.Status, StatusCancelled) {
			return nil, ErrBadTransition
		}
		target = StatusCancelled
	default:
		return nil, errors.New("subscriptions: unknown webhook event status")
	}

	if dup {
		// Same event already fully applied — acknowledge without
		// touching the ledger or accruals.
		return &sub, tx.Commit(ctx)
	}

	subPtr, err := s.transitionLocked(ctx, tx, &sub, target, amount, nullableText(ev.ProviderSubID))
	if err != nil {
		return nil, err
	}

	// billing_succeeded: move the period window forward and accrue the
	// creator's 70% (the accrual is what the platform owes — payout
	// rides the payments seam post-investors).
	if ev.Status == WebhookBillingSucceeded {
		creatorShare, _, err := SplitShares(ev.AmountPaise)
		if err != nil {
			return nil, err
		}
		startAt := ev.PaidAt
		if startAt.IsZero() {
			startAt = time.Now().UTC()
		}
		periodEnd := startAt
		if BillingPeriod(planPeriod) == BillingYearly {
			periodEnd = periodEnd.AddDate(1, 0, 0)
		} else {
			periodEnd = periodEnd.AddDate(0, 1, 0)
		}
		subPtr, err = scanSubscription(tx.QueryRow(ctx,
			`UPDATE subscriptions SET current_period_start = $1, current_period_end = $2, cancel_at_period_end = false
			 WHERE id = $3 RETURNING `+subCols,
			startAt, periodEnd, sub.ID))
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO creator_earnings (creator_id, pending_paise, paid_out_paise)
			 VALUES ($1, $2, 0)
			 ON CONFLICT (creator_id) DO UPDATE SET pending_paise = creator_earnings.pending_paise + EXCLUDED.pending_paise`,
			creatorID, creatorShare); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return subPtr, nil
}

// transitionLocked moves a locked subscription row to the target status
// (checking the CanTransition matrix) and appends the ledger row with
// the 70/30 split. Only billing_succeeded callers accrue — this method
// never touches creator_earnings.
func (s *SubscriptionService) transitionLocked(ctx context.Context, tx pgx.Tx, sub *Subscription, target SubscriptionStatus, amountPaise int64, providerRef *string) (*Subscription, error) {
	if !CanTransition(sub.Status, target) {
		return nil, ErrBadTransition
	}
	if err := s.insertLedgerRow(ctx, tx, sub, sub.PlanID, string(webhookEventFor(target)), amountPaise, providerRef); err != nil {
		return nil, err
	}
	return scanSubscription(tx.QueryRow(ctx,
		`UPDATE subscriptions SET status = $1 WHERE id = $2 RETURNING `+subCols,
		string(target), sub.ID))
}

// insertLedgerRow appends one membership_events row. The 70/30 split is
// SplitShares (floor-then-remainder, always sums to amount) — the same
// invariant the DB CHECK enforces, so a split that doesn't sum is a
// loud error here instead of a confusing constraint violation there.
func (s *SubscriptionService) insertLedgerRow(ctx context.Context, tx pgx.Tx, sub *Subscription, planID pgtype.UUID, eventType string, amountPaise int64, providerRef *string) error {
	creatorShare, platformShare, err := SplitShares(amountPaise)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO membership_events (subscription_id, plan_id, event_type, amount_paise, creator_share_paise, platform_share_paise, provider_ref)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		sub.ID, planID, eventType, amountPaise, creatorShare, platformShare, providerRef)
	return err
}

// webhookEventFor maps a lifecycle target to the ledger event type.
func webhookEventFor(target SubscriptionStatus) WebhookStatus {
	switch target {
	case StatusActive:
		return WebhookBillingSucceeded
	case StatusPastDue:
		return WebhookBillingFailed
	default:
		return WebhookSubscriptionCancelled
	}
}

func prefixedSubCols(alias string) string {
	return alias + `.id, ` + alias + `.subscriber_id, ` + alias + `.plan_id, ` + alias + `.status, ` +
		alias + `.provider_ref, ` + alias + `.current_period_start, ` + alias + `.current_period_end, ` +
		alias + `.cancel_at_period_end, ` + alias + `.created_at, ` + alias + `.updated_at`
}

func nullableText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// uuidString renders a pgtype.UUID in canonical form.
func uuidString(u pgtype.UUID) string { return u.String() }

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
