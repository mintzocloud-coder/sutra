// SPDX-License-Identifier: AGPL-3.0-or-later
// Tests for the advertiser API (H3 part 2): pure validation/transition
// matrices plus DB-backed service tests (gated on TEST_DATABASE_URL,
// following the house convention — they skip cleanly without a DB and
// run in CI's go-api job).
package ads

import (
	"context"
	"encoding/json"
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

func TestValidateTargetingMatrix(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"empty", "", false},
		{"null", "null", false},
		{"empty object", `{}`, false},
		{"countries ok", `{"countries":["in","us"]}`, false},
		{"languages ok", `{"languages":["hi","en"]}`, false},
		{"devices ok", `{"devices":["mobile","desktop"]}`, false},
		{"categories grammar ok", `{"categories":["mens-fashion","electronics"]}`, false},
		{"all axes", `{"countries":["in"],"languages":["hi"],"categories":["books"],"devices":["tablet"]}`, false},
		{"unknown key is loud error", `{"regions":["in"]}`, true},
		{"typo key is loud error", `{"country":["in"]}`, true},
		{"bad country code", `{"countries":["IN"]}`, true},
		{"empty country", `{"countries":[""]}`, true},
		{"bad language", `{"languages":["hindi"]}`, true},
		{"bad device", `{"devices":["watch"]}`, true},
		{"bad category shape", `{"categories":["Mens Fashion"]}`, true},
		{"duplicate values", `{"countries":["in","in"]}`, true},
		{"non-object", `[1]`, true},
		{"non-array value", `{"countries":"in"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateTargeting(json.RawMessage(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateTargeting(%q) err=%v, wantErr=%v", tc.raw, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidTargeting) {
				t.Fatalf("err %v is not ErrInvalidTargeting", err)
			}
		})
	}
}

func TestCampaignTransitionMatrix(t *testing.T) {
	cases := []struct {
		from    string
		to      string
		allowed bool
	}{
		{CampaignDraft, CampaignActive, true},
		{CampaignDraft, CampaignPaused, false},
		{CampaignDraft, CampaignArchived, true},
		{CampaignActive, CampaignPaused, true},
		{CampaignActive, CampaignDraft, false},
		{CampaignActive, CampaignArchived, true},
		{CampaignPaused, CampaignActive, true},
		{CampaignPaused, CampaignDraft, false},
		{CampaignPaused, CampaignArchived, true},
		{CampaignArchived, CampaignActive, false},
		{CampaignArchived, CampaignDraft, false},
	}
	for _, tc := range cases {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			allowed := false
			for _, n := range CampaignTransitions[tc.from] {
				if n == tc.to {
					allowed = true
				}
			}
			if allowed != tc.allowed {
				t.Fatalf("matrix(%s->%s) = %v, want %v", tc.from, tc.to, allowed, tc.allowed)
			}
		})
	}
}

func TestEligibleMatrix(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	newCamp := func() *Campaign {
		return &Campaign{Status: CampaignActive, BudgetPaise: 10000, SpendPaise: 0}
	}
	cases := []struct {
		name  string
		apply func(*Campaign)
		want  bool
	}{
		{"plain active eligible", func(c *Campaign) {}, true},
		{"draft never eligible", func(c *Campaign) { c.Status = CampaignDraft }, false},
		{"paused never eligible", func(c *Campaign) { c.Status = CampaignPaused }, false},
		{"archived never eligible", func(c *Campaign) { c.Status = CampaignArchived }, false},
		{"window open", func(c *Campaign) { c.StartAt = &past; c.EndAt = &future }, true},
		{"not started", func(c *Campaign) { c.StartAt = &future }, false},
		{"ended", func(c *Campaign) { c.EndAt = &past }, false},
		{"budget exhausted", func(c *Campaign) { c.SpendPaise = 10000 }, false},
		{"spend over budget (should not happen, still ineligible)", func(c *Campaign) { c.SpendPaise = 10001 }, false},
		{"one paise left", func(c *Campaign) { c.SpendPaise = 9999 }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCamp()
			tc.apply(c)
			if got := Eligible(c, now); got != tc.want {
				t.Fatalf("Eligible() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRemainingBudget(t *testing.T) {
	c := &Campaign{BudgetPaise: 5000, SpendPaise: 4999}
	if got := RemainingBudget(c); got != 1 {
		t.Fatalf("RemainingBudget = %d, want 1", got)
	}
	c.SpendPaise = 5000
	if got := RemainingBudget(c); got != 0 {
		t.Fatalf("exhausted RemainingBudget = %d, want 0", got)
	}
	c.SpendPaise = 6000
	if got := RemainingBudget(c); got != 0 {
		t.Fatalf("overspend RemainingBudget = %d, want 0 (never negative)", got)
	}
}

func TestValidateMoneyMatrix(t *testing.T) {
	zero := int64(0)
	ok := int64(1000)
	tooBig := maxBudgetPaise + 1
	bigger := int64(2000)
	cases := []struct {
		name    string
		budget  int64
		daily   *int64
		wantErr bool
	}{
		{"valid", 1000, nil, false},
		{"valid with daily", 1000, &ok, false},
		{"daily equals budget", 1000, &[]int64{1000}[0], false},
		{"zero budget", 0, nil, true},
		{"negative budget", -5, nil, true},
		{"over cap", tooBig, nil, true},
		{"zero daily", 1000, &zero, true},
		{"daily over budget", 1000, &bigger, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMoney(tc.budget, tc.daily)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateMoney(%d, %v) err=%v, wantErr=%v", tc.budget, tc.daily, err, tc.wantErr)
			}
		})
	}
}

func TestAdsCursorRoundTrip(t *testing.T) {
	var id pgtype.UUID
	if err := id.Scan("123e4567-e89b-12d3-a456-426614174000"); err != nil {
		t.Fatal(err)
	}
	c := &AdsCursor{CreatedAt: time.Now().UTC().Truncate(time.Nanosecond), ID: id}
	back, err := DecodeAdsCursor(c.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != id || !back.CreatedAt.Equal(c.CreatedAt) {
		t.Fatalf("round-trip mismatch: %+v vs %+v", back, c)
	}
	for _, bad := range []string{"", "!!!", "aGk=", "bm90YXBpc2V8bm90LXV1aWQ"} {
		if _, err := DecodeAdsCursor(bad); !errors.Is(err, ErrInvalidCampaignCursor) {
			t.Fatalf("DecodeAdsCursor(%q) = %v, want ErrInvalidCampaignCursor", bad, err)
		}
	}
}

// testAdsPool returns a real DB pool for integration tests, skipping
// cleanly when TEST_DATABASE_URL is unset (house convention).
func testAdsPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — integration tests skip cleanly")
	}
	pool, err := db.Open(context.Background(), dsn, 5, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	// House convention (users/orders/…): the CI test database is a
	// fresh `postgres` database with no schema — run the migrations
	// before touching tables. Without this the fixture INSERTs fail
	// with "relation does not exist" (seen in CI on the ads suite).
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// testSellerFixture creates a users row + sellers row and returns the
// seller id. Subjects/slugs are unique per invocation (time nanos +
// a per-process counter) so tests can re-run against a reused DB.
var fixtureCounter int64

func testSellerFixture(t *testing.T, pool *db.Pool, tag string) pgtype.UUID {
	t.Helper()
	fixtureCounter++
	// Slug must fit the sellers ^[a-z0-9-]{3,30}$ CHECK: last 8 nano
	// digits + counter keep it well under the cap.
	uniq := fmt.Sprintf("adsf-%d-%d", time.Now().UnixNano()%100000000, fixtureCounter)
	ctx := context.Background()
	var userID pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (subject) VALUES ($1) RETURNING id`,
		"ads-fixture-"+uniq).Scan(&userID); err != nil {
		t.Fatalf("users fixture: %v", err)
	}
	var sellerID pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO sellers (user_id, store_name, store_slug) VALUES ($1, $2, $3) RETURNING id`,
		userID, "Ads Fixture Store "+uniq, uniq).Scan(&sellerID); err != nil {
		t.Fatalf("sellers fixture: %v", err)
	}
	return sellerID
}

func TestCampaignLifecycle(t *testing.T) {
	pool := testAdsPool(t)
	ctx := context.Background()
	svc := NewCampaignService(pool)
	adv := testSellerFixture(t, pool, "lifecycle")

	start := time.Now().UTC().Add(time.Hour)
	end := start.Add(24 * time.Hour)
	c, err := svc.Create(ctx, adv, CreateInput{
		Name:        "Diwali Kurta Push",
		BudgetPaise: 100000,
		Targeting:   json.RawMessage(`{"countries":["in"],"categories":["mens-fashion"]}`),
		StartAt:     &start,
		EndAt:       &end,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.Status != CampaignDraft {
		t.Fatalf("new campaign status = %q, want draft", c.Status)
	}
	if c.SpendPaise != 0 {
		t.Fatalf("new campaign spend = %d, want 0", c.SpendPaise)
	}
	if len(c.Targeting.Countries) != 1 || c.Targeting.Countries[0] != "in" {
		t.Fatalf("targeting not round-tripped: %+v", c.Targeting)
	}

	// Draft is not eligible (window is in the future anyway).
	if Eligible(c, time.Now().UTC()) {
		t.Fatal("draft campaign must not be eligible")
	}

	// Unknown category slug is a loud 422-class error.
	_, err = svc.Create(ctx, adv, CreateInput{
		Name: "Bad", BudgetPaise: 1000,
		Targeting: json.RawMessage(`{"categories":["not-a-real-category"]}`),
	})
	if !errors.Is(err, ErrUnknownCategorySlug) {
		t.Fatalf("unknown category err = %v, want ErrUnknownCategorySlug", err)
	}

	// Bad money shapes are 400-class before any write.
	for _, in := range []CreateInput{
		{Name: "", BudgetPaise: 1000},
		{Name: "x", BudgetPaise: 0},
		{Name: "x", BudgetPaise: -5},
	} {
		if _, err := svc.Create(ctx, adv, in); !errors.Is(err, ErrInvalidCampaignField) {
			t.Fatalf("Create(%+v) err = %v, want ErrInvalidCampaignField", in, err)
		}
	}

	// end before start rejected.
	_, err = svc.Create(ctx, adv, CreateInput{
		Name: "x", BudgetPaise: 1000, StartAt: &end, EndAt: &start,
	})
	if !errors.Is(err, ErrInvalidCampaignField) {
		t.Fatalf("bad window err = %v, want ErrInvalidCampaignField", err)
	}

	// Activate: draft -> active.
	active, err := svc.Transition(ctx, adv, c.ID, CampaignActive)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if active.Status != CampaignActive {
		t.Fatalf("status = %q, want active", active.Status)
	}

	// Pause and resume.
	paused, err := svc.Transition(ctx, adv, c.ID, CampaignPaused)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if paused.Status != CampaignPaused {
		t.Fatalf("status = %q, want paused", paused.Status)
	}
	resumed, err := svc.Transition(ctx, adv, c.ID, CampaignActive)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Status != CampaignActive {
		t.Fatalf("status = %q, want active", resumed.Status)
	}

	// Illegal edge: active -> draft.
	if _, err := svc.Transition(ctx, adv, c.ID, CampaignDraft); !errors.Is(err, ErrBadCampaignTransition) {
		t.Fatalf("active->draft err = %v, want ErrBadCampaignTransition", err)
	}

	// Patch: name + daily budget; null clears daily; status/spend
	// fields are not in the body (strict decode handles that).
	daily := int64(5000)
	dailyPtr := &daily
	updated, err := svc.Update(ctx, adv, c.ID, UpdateInput{Name: strPtr("Diwali Kurta Push v2"), DailyBudgetPaise: &dailyPtr})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "Diwali Kurta Push v2" || updated.DailyBudgetPaise == nil || *updated.DailyBudgetPaise != 5000 {
		t.Fatalf("update not applied: %+v", updated)
	}
	clearDaily := UpdateInput{DailyBudgetPaise: &[]*int64{nil}[0]}
	updated, err = svc.Update(ctx, adv, c.ID, clearDaily)
	if err != nil {
		t.Fatalf("clear daily: %v", err)
	}
	if updated.DailyBudgetPaise != nil {
		t.Fatalf("daily not cleared: %+v", updated)
	}

	// Budget cannot be lowered below spend (spend is 0 here, so any
	// positive value works; lowering below zero is impossible).
	low := int64(1)
	if _, err := svc.Update(ctx, adv, c.ID, UpdateInput{BudgetPaise: &low}); err != nil {
		t.Fatalf("lower budget below spend(=0) err = %v", err)
	}

	// Archive: terminal; behaves as 404 after.
	archived, err := svc.Transition(ctx, adv, c.ID, CampaignArchived)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if archived.Status != CampaignArchived {
		t.Fatalf("status = %q, want archived", archived.Status)
	}
	if _, err := svc.Get(ctx, adv, c.ID); !errors.Is(err, ErrCampaignNotFound) {
		t.Fatalf("get after archive err = %v, want ErrCampaignNotFound", err)
	}
	// Archived is invisible at Get, so a post-archive transition reads
	// as not-found (not a transition error) — the terminal state still
	// cannot be left.
	if _, err := svc.Transition(ctx, adv, c.ID, CampaignActive); !errors.Is(err, ErrCampaignNotFound) {
		t.Fatalf("archive->active err = %v, want ErrCampaignNotFound", err)
	}
}

func TestCampaignOwnerIsolation(t *testing.T) {
	pool := testAdsPool(t)
	ctx := context.Background()
	svc := NewCampaignService(pool)
	a := testSellerFixture(t, pool, "owner-a")
	b := testSellerFixture(t, pool, "owner-b")

	c, err := svc.Create(ctx, a, CreateInput{Name: "A's campaign", BudgetPaise: 1000})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Get(ctx, b, c.ID); !errors.Is(err, ErrCampaignNotOwner) {
		t.Fatalf("cross-owner get err = %v, want ErrCampaignNotOwner", err)
	}
	if _, err := svc.Update(ctx, b, c.ID, UpdateInput{Name: strPtr("hijack")}); !errors.Is(err, ErrCampaignNotOwner) {
		t.Fatalf("cross-owner update err = %v, want ErrCampaignNotOwner", err)
	}
	if _, err := svc.Transition(ctx, b, c.ID, CampaignActive); !errors.Is(err, ErrCampaignNotOwner) {
		t.Fatalf("cross-owner transition err = %v, want ErrCampaignNotOwner", err)
	}
	// B's list must be empty (no cross-advertiser visibility).
	page, err := svc.List(ctx, b, 10, nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Campaigns) != 0 {
		t.Fatalf("B sees %d campaigns, want 0", len(page.Campaigns))
	}
}

func TestCampaignListPagination(t *testing.T) {
	pool := testAdsPool(t)
	ctx := context.Background()
	svc := NewCampaignService(pool)
	adv := testSellerFixture(t, pool, "paged")

	for i := 0; i < 3; i++ {
		if _, err := svc.Create(ctx, adv, CreateInput{
			Name: "camp", BudgetPaise: 1000,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	p1, err := svc.List(ctx, adv, 2, nil)
	if err != nil {
		t.Fatalf("List p1: %v", err)
	}
	if len(p1.Campaigns) != 2 || p1.NextCursor == "" {
		t.Fatalf("p1 = %d campaigns, cursor %q; want 2 + cursor", len(p1.Campaigns), p1.NextCursor)
	}
	after, err := DecodeAdsCursor(p1.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	p2, err := svc.List(ctx, adv, 2, &after)
	if err != nil {
		t.Fatalf("List p2: %v", err)
	}
	if len(p2.Campaigns) != 1 || p2.NextCursor != "" {
		t.Fatalf("p2 = %d campaigns, cursor %q; want 1 + no cursor", len(p2.Campaigns), p2.NextCursor)
	}
	// No duplicates across pages.
	seen := map[string]bool{}
	for _, c := range append(p1.Campaigns, p2.Campaigns...) {
		id := c.ID.String()
		if seen[id] {
			t.Fatalf("duplicate campaign %s across pages", id)
		}
		seen[id] = true
	}
}

func strPtr(s string) *string { return &s }
