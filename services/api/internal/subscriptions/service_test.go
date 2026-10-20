// SPDX-License-Identifier: AGPL-3.0-or-later
package subscriptions

// DB-backed service tests (H4 part 2), gated on TEST_DATABASE_URL —
// they skip cleanly when the var is unset (house convention).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

// fakeProvider is the test-double aggregator: it behaves like the
// post-investors Cashfree provider (succeeds) so the service's
// webhook-promotion transaction, ledger, and accrual logic can be
// verified without touching real money rails.
type fakeProvider struct {
	subSeq int
}

func (f *fakeProvider) CreateSubscription(_ context.Context, p CreateParams) (*CreateResult, error) {
	f.subSeq++
	return &CreateResult{
		ProviderSubID: fmt.Sprintf("cf-sub-test-%d", f.subSeq),
		Status:        "pending",
		AuthURL:       "https://pay.example/auth/cf-sub-test",
	}, nil
}

func (f *fakeProvider) CancelSubscription(_ context.Context, _ string, _ bool) error { return nil }

func (f *fakeProvider) VerifyWebhook(_ context.Context, _ []byte, _ string) (*WebhookEvent, error) {
	return nil, errors.New("fakeProvider: VerifyWebhook not implemented in tests")
}

func testSubPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — integration tests skip cleanly")
	}
	pool, err := db.Open(context.Background(), dsn, 5, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func testSubUser(t *testing.T, pool *db.Pool, tag string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (subject) VALUES ($1) RETURNING id`,
		fmt.Sprintf("subs-fixture-%s-%d", tag, time.Now().UnixNano())).Scan(&id)
	if err != nil {
		t.Fatalf("users fixture: %v", err)
	}
	return id
}

func testPlan(t *testing.T, plans *PlanService, creator pgtype.UUID) *Plan {
	t.Helper()
	p, err := plans.CreatePlan(context.Background(), creator,
		"Inner Circle", "The inner circle tier", 9900, BillingMonthly, CurrencyINR)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	return p
}

// TestSubscribeFailClosed proves the part-1 invariant end to end: with
// the stub provider, Subscribe fails BEFORE any row is written — the
// error is typed, and the table is untouched.
func TestSubscribeFailClosed(t *testing.T) {
	pool := testSubPool(t)
	ctx := context.Background()
	creator := testSubUser(t, pool, "creator")
	member := testSubUser(t, pool, "member")
	plans := NewPlanService(pool)
	subs := NewSubscriptionService(pool, StubProvider{}, plans)
	plan := testPlan(t, plans, creator)

	_, err := subs.Subscribe(ctx, member, plan.ID, "Test Member", "+919876543210", "m@example.com")
	var disabled *ErrSubscriptionsDisabled
	if !errors.As(err, &disabled) {
		t.Fatalf("Subscribe with stub: want *ErrSubscriptionsDisabled, got %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM subscriptions`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("fail-closed violated: %d subscription rows written by a stub subscribe", n)
	}
}

// TestPlanCRUD exercises creator plan lifecycle: create → get (owner)
// → list → update (name/active) → deactivate → 404 for strangers.
func TestPlanCRUD(t *testing.T) {
	pool := testSubPool(t)
	ctx := context.Background()
	creator := testSubUser(t, pool, "creator")
	stranger := testSubUser(t, pool, "stranger")
	plans := NewPlanService(pool)

	p := testPlan(t, plans, creator)
	if p.PricePaise != 9900 || p.BillingPeriod != BillingMonthly || !p.IsActive {
		t.Fatalf("CreatePlan returned wrong row: %+v", p)
	}

	got, err := plans.GetPlan(ctx, creator, p.ID)
	if err != nil || got.ID != p.ID {
		t.Fatalf("GetPlan owner: %v", err)
	}
	if _, err := plans.GetPlan(ctx, stranger, p.ID); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("GetPlan stranger: want ErrPlanNotFound, got %v", err)
	}

	list, err := plans.ListPlans(ctx, creator, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListPlans: %v (n=%d)", err, len(list))
	}

	name := "Inner Circle v2"
	updated, err := plans.UpdatePlan(ctx, creator, p.ID, UpdatePlanInput{Name: &name})
	if err != nil || updated.Name != name {
		t.Fatalf("UpdatePlan name: %v", err)
	}
	if updated.PricePaise != 9900 {
		t.Fatalf("UpdatePlan must never touch price: %+v", updated)
	}

	deactivated, err := plans.DeactivatePlan(ctx, creator, p.ID)
	if err != nil || deactivated.IsActive {
		t.Fatalf("DeactivatePlan: %v", err)
	}
	if _, err := plans.GetPlanPublic(ctx, p.ID); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("GetPlanPublic on inactive plan: want ErrPlanNotFound, got %v", err)
	}

	// Validation still enforced at the service layer.
	if _, err := plans.CreatePlan(ctx, creator, "", "x", 100, BillingMonthly, CurrencyINR); !errors.Is(err, ErrInvalidPlanInput) {
		t.Fatalf("empty name: want ErrInvalidPlanInput, got %v", err)
	}
	if _, err := plans.CreatePlan(ctx, creator, "x", "x", 0, BillingMonthly, CurrencyINR); !errors.Is(err, ErrInvalidPlanInput) {
		t.Fatalf("zero price: want ErrInvalidPlanInput, got %v", err)
	}
}

// TestWebhookLifecycle drives the full billing flow against the fake
// provider: subscribe → pending → billing_succeeded webhook → active +
// ledger + accrual → duplicate webhook idempotent → failed webhook on a
// second subscription → past_due → cancelled webhook → cancelled
// terminal. It also verifies the fail-closed subscribe-while-stub
// guarantee is NOT tripped by the fake: the stub path is covered by
// TestSubscribeFailClosed above.
func TestWebhookLifecycle(t *testing.T) {
	pool := testSubPool(t)
	ctx := context.Background()
	creator := testSubUser(t, pool, "creator")
	member := testSubUser(t, pool, "member")
	plans := NewPlanService(pool)
	subs := NewSubscriptionService(pool, &fakeProvider{}, plans)
	plan := testPlan(t, plans, creator) // 9900 paise monthly

	res, err := subs.Subscribe(ctx, member, plan.ID, "Test Member", "+919876543210", "m@example.com")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if res.Subscription.Status != StatusPending || res.AuthURL == "" {
		t.Fatalf("new subscription must be pending with an auth URL: %+v", res.Subscription)
	}
	ref := *res.Subscription.ProviderRef

	// Double subscribe on the same plan: the partial unique index is
	// the source of truth — but at pending status the index does not
	// apply, so a second pending row is allowed (each carries its own
	// mandate); a second subscribe while the first is ACTIVE is the
	// double-billing surface that must 409. Promote first, then try.
	paidAt := time.Now().UTC().Truncate(time.Second)
	promoted, err := subs.HandleWebhook(ctx, &WebhookEvent{
		ProviderSubID: ref, AmountPaise: 9900,
		Status: WebhookBillingSucceeded, PaidAt: paidAt,
	})
	if err != nil {
		t.Fatalf("HandleWebhook billing_succeeded: %v", err)
	}
	if promoted.Status != StatusActive {
		t.Fatalf("webhook did not promote: %s", promoted.Status)
	}
	if promoted.CurrentPeriodEnd == nil || !promoted.CurrentPeriodEnd.After(paidAt) {
		t.Fatalf("period window not moved forward: %+v", promoted)
	}

	if _, err := subs.Subscribe(ctx, member, plan.ID, "Test Member", "+919876543210", "m@example.com"); !errors.Is(err, ErrAlreadySubscribed) {
		t.Fatalf("second live subscription: want ErrAlreadySubscribed, got %v", err)
	}

	// Ledger: exactly one billing_succeeded row with the 70/30 split
	// (6930/2970), and the accrual grew by the creator's 6930.
	var creatorShare, platformShare int64
	var eventType string
	var evRef *string
	if err := pool.QueryRow(ctx,
		`SELECT event_type, creator_share_paise, platform_share_paise, provider_ref
		 FROM membership_events WHERE subscription_id = $1`, promoted.ID).
		Scan(&eventType, &creatorShare, &platformShare, &evRef); err != nil {
		t.Fatalf("ledger row: %v", err)
	}
	if eventType != "billing_succeeded" || creatorShare != 6930 || platformShare != 2970 {
		t.Fatalf("ledger split wrong: %s %d/%d", eventType, creatorShare, platformShare)
	}
	if evRef == nil || *evRef != ref {
		t.Fatalf("ledger must carry the provider ref (migration CHECK): %v", evRef)
	}
	var pending, paidOut int64
	if err := pool.QueryRow(ctx,
		`SELECT pending_paise, paid_out_paise FROM creator_earnings WHERE creator_id = $1`, creator).
		Scan(&pending, &paidOut); err != nil {
		t.Fatalf("creator_earnings: %v", err)
	}
	if pending != 6930 || paidOut != 0 {
		t.Fatalf("accrual wrong: pending=%d paid_out=%d", pending, paidOut)
	}

	// Entitlements: the member now holds member_of:<creator>.
	ids, err := subs.ActiveCreatorIDs(ctx, member)
	if err != nil {
		t.Fatalf("ActiveCreatorIDs: %v", err)
	}
	grants := EntitlementsFor(ids)
	if len(grants) != 1 || grants[0] != MemberGrantFor(creator.String()) {
		t.Fatalf("grants wrong: %v", grants)
	}

	// Duplicate delivery: same event twice is a no-op (no double
	// ledger, no double accrual).
	if _, err := subs.HandleWebhook(ctx, &WebhookEvent{
		ProviderSubID: ref, AmountPaise: 9900,
		Status: WebhookBillingSucceeded, PaidAt: paidAt,
	}); err != nil {
		t.Fatalf("duplicate webhook: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM membership_events WHERE subscription_id = $1`, promoted.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("duplicate webhook wrote a second ledger row (n=%d, err=%v)", n, err)
	}

	// A failed charge on the active row: past_due + ledger row, but the
	// accrual must NOT move (no money was captured).
	if _, err := subs.HandleWebhook(ctx, &WebhookEvent{
		ProviderSubID: ref, AmountPaise: 9900, Status: WebhookBillingFailed,
	}); err != nil {
		t.Fatalf("billing_failed: %v", err)
	}
	got, err := subs.GetSubscription(ctx, member, promoted.ID)
	if err != nil || got.Status != StatusPastDue {
		t.Fatalf("row not past_due: %+v %v", got, err)
	}
	if err := pool.QueryRow(ctx, `SELECT pending_paise FROM creator_earnings WHERE creator_id = $1`, creator).Scan(&pending); err != nil || pending != 6930 {
		t.Fatalf("failed billing must not accrue: pending=%d err=%v", pending, err)
	}

	// Failed charge on a PENDING mandate: stays pending (the mandate
	// was never established), ledger records the attempt.
	plan2 := func() *Plan {
		p, err := plans.CreatePlan(ctx, creator, "Tier Two", "", 4900, BillingYearly, CurrencyINR)
		if err != nil {
			t.Fatalf("CreatePlan tier2: %v", err)
		}
		return p
	}()
	res2, err := subs.Subscribe(ctx, member, plan2.ID, "Test Member", "+919876543210", "m@example.com")
	if err != nil {
		t.Fatalf("Subscribe tier2: %v", err)
	}
	if _, err := subs.HandleWebhook(ctx, &WebhookEvent{
		ProviderSubID: *res2.Subscription.ProviderRef, AmountPaise: 4900, Status: WebhookBillingFailed,
	}); err != nil {
		t.Fatalf("failed billing on pending: %v", err)
	}
	got2, _ := subs.GetSubscription(ctx, member, res2.Subscription.ID)
	if got2.Status != StatusPending {
		t.Fatalf("pending mandate must stay pending on failed billing: %s", got2.Status)
	}

	// Mandate cancelled at the aggregator: cancelled is terminal, the
	// ledger carries the plan price as memo (no accrual).
	if _, err := subs.HandleWebhook(ctx, &WebhookEvent{
		ProviderSubID: ref, Status: WebhookSubscriptionCancelled,
	}); err != nil {
		t.Fatalf("cancelled webhook: %v", err)
	}
	got, _ = subs.GetSubscription(ctx, member, promoted.ID)
	if got.Status != StatusCancelled {
		t.Fatalf("row not cancelled: %s", got.Status)
	}
	if _, err := subs.HandleWebhook(ctx, &WebhookEvent{
		ProviderSubID: ref, AmountPaise: 9900, Status: WebhookBillingSucceeded,
	}); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("resurrecting a cancelled row: want ErrBadTransition, got %v", err)
	}

	// Unknown provider reference: loud 404, never a silent accept.
	if _, err := subs.HandleWebhook(ctx, &WebhookEvent{
		ProviderSubID: "cf-sub-nope", Status: WebhookBillingSucceeded, AmountPaise: 100,
	}); !errors.Is(err, ErrWebhookNoRow) {
		t.Fatalf("unknown ref: want ErrWebhookNoRow, got %v", err)
	}
}

// TestCancelMember verifies the member-initiated cancel against the
// fake provider (at_period_end=true flips the flag only; immediate
// cancel writes the cancelled ledger row with the plan price memo).
func TestCancelMember(t *testing.T) {
	pool := testSubPool(t)
	ctx := context.Background()
	creator := testSubUser(t, pool, "creator")
	member := testSubUser(t, pool, "member")
	plans := NewPlanService(pool)
	subs := NewSubscriptionService(pool, &fakeProvider{}, plans)
	plan := testPlan(t, plans, creator)

	res, err := subs.Subscribe(ctx, member, plan.ID, "Test Member", "+919876543210", "m@example.com")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	ref := *res.Subscription.ProviderRef
	if _, err := subs.HandleWebhook(ctx, &WebhookEvent{
		ProviderSubID: ref, AmountPaise: 9900, Status: WebhookBillingSucceeded, PaidAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("promote: %v", err)
	}

	flagged, err := subs.CancelMember(ctx, member, res.Subscription.ID, true)
	if err != nil {
		t.Fatalf("cancel at_period_end: %v", err)
	}
	if !flagged.CancelAtPeriodEnd || flagged.Status != StatusActive {
		t.Fatalf("at_period_end cancel must keep access: %+v", flagged)
	}

	now, err := subs.CancelMember(ctx, member, res.Subscription.ID, false)
	if err != nil {
		t.Fatalf("immediate cancel: %v", err)
	}
	if now.Status != StatusCancelled {
		t.Fatalf("immediate cancel must terminate: %s", now.Status)
	}
	var amount, cShare, pShare int64
	var evType string
	if err := pool.QueryRow(ctx,
		`SELECT event_type, amount_paise, creator_share_paise, platform_share_paise
		 FROM membership_events WHERE subscription_id = $1 AND event_type = 'cancelled'`, now.ID).
		Scan(&evType, &amount, &cShare, &pShare); err != nil {
		t.Fatalf("cancelled ledger row: %v", err)
	}
	if amount != 9900 || cShare+pShare != 9900 {
		t.Fatalf("cancelled memo split wrong: %d %d/%d", amount, cShare, pShare)
	}

	// Cancelling someone else's row is 404 (never leaks existence).
	other := testSubUser(t, pool, "other")
	if _, err := subs.CancelMember(ctx, other, now.ID, false); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("cancel stranger row: want ErrSubscriptionNotFound, got %v", err)
	}
}
