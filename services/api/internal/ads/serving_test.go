// SPDX-License-Identifier: AGPL-3.0-or-later
// Tests for the H3 part 3b serving path: contextual quality, the pure
// decision, env config, and the billed-impression round trip.
package ads

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func int64ptr(n int64) *int64 { return &n }

// bidPtrPtr builds the **int64 UpdateInput.BidPaise needs: non-nil outer
// sets the bid (nil inner clears it).
func bidPtrPtr(n *int64) **int64 { return &n }

func testServeCandidate(t *testing.T, tag string, bid int64, q float64) ServeCandidate {
	t.Helper()
	c := &Campaign{Targeting: Targeting{}}
	c.BidPaise = int64ptr(bid)
	var id pgtype.UUID
	if err := id.Scan("11111111-1111-1111-1111-1111111111" + tag); err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return ServeCandidate{Campaign: c, Creative: &Creative{ID: id}, Quality: q}
}

func TestServingConfigFromEnv(t *testing.T) {
	os.Unsetenv("ADS_SERVING_ENABLED")
	os.Unsetenv("ADS_RESERVE_PAISE")
	cfg := ServingConfigFromEnv()
	if cfg.Enabled {
		t.Fatal("serving must be OFF by default (fail-closed)")
	}
	if cfg.ReservePaise != 50 {
		t.Fatalf("default reserve = %d, want 50", cfg.ReservePaise)
	}
	for _, v := range []string{"1", "true", "yes", "on", "TRUE"} {
		os.Setenv("ADS_SERVING_ENABLED", v)
		if !ServingConfigFromEnv().Enabled {
			t.Fatalf("ADS_SERVING_ENABLED=%q should enable serving", v)
		}
	}
	os.Setenv("ADS_SERVING_ENABLED", "0")
	if ServingConfigFromEnv().Enabled {
		t.Fatal("ADS_SERVING_ENABLED=0 must not enable serving")
	}
	os.Setenv("ADS_SERVING_ENABLED", "1")
	os.Setenv("ADS_RESERVE_PAISE", "200")
	if got := ServingConfigFromEnv().ReservePaise; got != 200 {
		t.Fatalf("reserve = %d, want 200", got)
	}
	os.Setenv("ADS_RESERVE_PAISE", "junk")
	if got := ServingConfigFromEnv().ReservePaise; got != 50 {
		t.Fatalf("bad reserve should fall back to 50, got %d", got)
	}
	os.Setenv("ADS_RESERVE_PAISE", "-5")
	if got := ServingConfigFromEnv().ReservePaise; got != 50 {
		t.Fatalf("negative reserve should fall back to 50, got %d", got)
	}
	os.Unsetenv("ADS_SERVING_ENABLED")
	os.Unsetenv("ADS_RESERVE_PAISE")
}

func TestContextualQualityMatrix(t *testing.T) {
	camp := func(cats, langs []string) *Campaign {
		return &Campaign{Targeting: Targeting{Categories: cats, Languages: langs}}
	}
	slot := SlotContext{Slot: "feed_1", Categories: []string{"mens-fashion"}, Language: "hi", Country: "in"}
	cases := []struct {
		name string
		c    *Campaign
		want float64
	}{
		{"no targeting at all", camp(nil, nil), 0.5},
		{"category match", camp([]string{"mens-fashion"}, nil), 0.85},
		{"language match only", camp(nil, []string{"hi"}), 0.65},
		{"category + language match clamps at 1", camp([]string{"mens-fashion"}, []string{"hi"}), 1.0},
		{"unrelated category", camp([]string{"womens-fashion"}, nil), 0.5},
		{"unrelated language", camp(nil, []string{"ta"}), 0.5},
	}
	for _, tc := range cases {
		if got := ContextualQuality(tc.c, slot); got != tc.want {
			t.Fatalf("%s: quality = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Never zero (the kernel's junk exclusion keys on quality == 0).
	if q := ContextualQuality(camp(nil, nil), slot); q <= 0 || q > 1 {
		t.Fatalf("quality out of (0,1]: %v", q)
	}
	// Case-insensitive category match.
	if q := ContextualQuality(camp([]string{"Mens-Fashion"}, nil), slot); q != 0.85 {
		t.Fatalf("case-insensitive category match: %v, want 0.85", q)
	}
}

func TestDecideMatrix(t *testing.T) {
	// Classic: higher bid × quality wins, pays second price.
	d, err := Decide(50, []ServeCandidate{testServeCandidate(t, "01", 100, 1.0), testServeCandidate(t, "02", 80, 1.0)})
	if err != nil || d == nil {
		t.Fatalf("decide err=%v d=%v", err, d)
	}
	if d.PricePaise != 81 {
		t.Fatalf("GSP price = %d, want 81", d.PricePaise)
	}
	if d.Creative.ID != testServeCandidate(t, "01", 0, 0).Creative.ID {
		t.Fatal("wrong winner creative")
	}
	// Quality beats bid.
	d2, err := Decide(50, []ServeCandidate{testServeCandidate(t, "03", 60, 1.0), testServeCandidate(t, "04", 100, 0.5)})
	if err != nil || d2 == nil {
		t.Fatalf("decide err=%v d=%v", err, d2)
	}
	if d2.Creative.ID != testServeCandidate(t, "03", 0, 0).Creative.ID {
		t.Fatal("higher-quality lower bid should win")
	}
	// No candidates → nil, not an error.
	if d3, err := Decide(50, nil); err != nil || d3 != nil {
		t.Fatalf("empty: err=%v d=%v, want nil,nil", err, d3)
	}
	// All below reserve → nil.
	if d4, err := Decide(500, []ServeCandidate{testServeCandidate(t, "05", 100, 1.0)}); err != nil || d4 != nil {
		t.Fatalf("below reserve: err=%v d=%v, want nil,nil", err, d4)
	}
	// Nil-bid campaign is excluded (fail-closed).
	noBid := ServeCandidate{Campaign: &Campaign{}, Creative: testServeCandidate(t, "06", 0, 0).Creative, Quality: 1.0}
	if d5, err := Decide(50, []ServeCandidate{noBid}); err != nil || d5 != nil {
		t.Fatalf("nil bid: err=%v d=%v, want nil,nil", err, d5)
	}
	// Zero quality excluded.
	zeroQ := testServeCandidate(t, "07", 100, 0)
	if d6, err := Decide(50, []ServeCandidate{zeroQ}); err != nil || d6 != nil {
		t.Fatalf("zero quality: err=%v d=%v, want nil,nil", err, d6)
	}
	// Price never exceeds the winner's own bid.
	for i := 0; i < 20; i++ {
		dd, err := Decide(50, []ServeCandidate{testServeCandidate(t, "08", 120, 0.9), testServeCandidate(t, "09", 115, 0.85)})
		if err != nil || dd == nil {
			t.Fatalf("iter %d: err=%v", i, err)
		}
		if dd.PricePaise > 120 {
			t.Fatalf("price %d exceeds own bid", dd.PricePaise)
		}
	}
}

func TestValidateBidMatrix(t *testing.T) {
	if err := validateBid(nil, 1000); err != nil {
		t.Fatalf("nil bid (not bid-enabled) must validate: %v", err)
	}
	if err := validateBid(int64ptr(500), 1000); err != nil {
		t.Fatalf("bid 500/1000: %v", err)
	}
	if err := validateBid(int64ptr(0), 1000); !errors.Is(err, ErrInvalidCampaignField) {
		t.Fatalf("zero bid err = %v, want ErrInvalidCampaignField", err)
	}
	if err := validateBid(int64ptr(-10), 1000); !errors.Is(err, ErrInvalidCampaignField) {
		t.Fatalf("negative bid err = %v, want ErrInvalidCampaignField", err)
	}
	if err := validateBid(int64ptr(1001), 1000); !errors.Is(err, ErrInvalidCampaignField) {
		t.Fatalf("bid above budget err = %v, want ErrInvalidCampaignField", err)
	}
}

// --- Integration: the billed-impression round trip on real PG. ---

func TestServeRoundTrip(t *testing.T) {
	pool := testAdsPool(t)
	ctx := context.Background()
	// Hermeticity: Serve reads ALL active bid-enabled campaigns, so wipe
	// the ad tables first — a previous (failed) run's fixtures must not
	// leak into this run's auctions. Tests in this package run
	// sequentially and every other test rebuilds its own fixtures.
	for _, q := range []string{
		`DELETE FROM ad_events`,
		`DELETE FROM ad_creatives`,
		`DELETE FROM ad_campaigns`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("test cleanup: %v", err)
		}
	}
	campSvc := NewCampaignService(pool)
	creativeSvc := NewCreativeService(pool, campSvc)
	deps := ServingDeps{Campaigns: campSvc, Creatives: creativeSvc}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	adv := testSellerFixture(t, pool, "serve-rt")
	cfg := ServingConfig{Enabled: true, ReservePaise: 50}
	now := time.Now()
	slot := SlotContext{Slot: "feed_1", Categories: []string{"mens-fashion"}, Language: "hi", Country: "in"}

	// Disabled seam is fail-closed: no serve, no rows.
	if _, err := Serve(ctx, pool, log, deps, ServingConfig{Enabled: false}, now, slot); !errors.Is(err, ErrServingDisabled) {
		t.Fatalf("disabled serve err = %v, want ErrServingDisabled", err)
	}

	// Campaign with a bid but no active creative: nothing servable.
	camp, err := campSvc.Create(ctx, adv, CreateInput{
		Name:        "Serve RT Campaign",
		BudgetPaise: 100000,
		BidPaise:    int64ptr(100),
		Targeting:   json.RawMessage(`{"categories":["mens-fashion"],"languages":["hi"]}`),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := campSvc.Transition(ctx, adv, camp.ID, CampaignActive); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if got, err := Serve(ctx, pool, log, deps, cfg, now, slot); err != nil || got != nil {
		t.Fatalf("no creative: got=%v err=%v, want nil,nil", got, err)
	}

	// Register + activate a creative: the auction now has one bidder.
	cr, err := creativeSvc.Create(ctx, adv, camp.ID, validCreativeInput())
	if err != nil {
		t.Fatalf("creative create: %v", err)
	}
	if _, err := creativeSvc.Transition(ctx, adv, cr.ID, CreativeActive); err != nil {
		t.Fatalf("creative activate: %v", err)
	}
	served, err := Serve(ctx, pool, log, deps, cfg, now, slot)
	if err != nil || served == nil {
		t.Fatalf("serve err=%v served=%v, want a billed impression", err, served)
	}
	// Single bidder, reserve 50: thin-competition floor — the winner
	// pays the reserve, never less.
	if served.PricePaise != 50 {
		t.Fatalf("single-bidder price = %d, want 50 (reserve floor)", served.PricePaise)
	}
	if served.CreatorSharePaise+served.PlatformSharePaise != served.PricePaise {
		t.Fatalf("55/45 split does not sum to price: %d + %d != %d",
			served.CreatorSharePaise, served.PlatformSharePaise, served.PricePaise)
	}
	if served.CreatorSharePaise != 50*55/100 {
		t.Fatalf("creator share = %d, want %d", served.CreatorSharePaise, 50*55/100)
	}
	if served.Slot != "feed_1" || served.CreativeID != cr.ID || served.CampaignID != camp.ID {
		t.Fatalf("served ad identity mismatch: %+v", served)
	}

	// The ledger row exists and spend accrued in the same transaction.
	var (
		price, cShare, pShare int64
		spend                 int64
	)
	if err := pool.QueryRow(ctx, `SELECT price_paise, creator_share_paise, platform_share_paise FROM ad_events WHERE impression_id = $1`,
		served.ImpressionID).Scan(&price, &cShare, &pShare); err != nil {
		t.Fatalf("ad_events row: %v", err)
	}
	if price != served.PricePaise || cShare != served.CreatorSharePaise || pShare != served.PlatformSharePaise {
		t.Fatalf("ledger row mismatch: %d/%d/%d", price, cShare, pShare)
	}
	if err := pool.QueryRow(ctx, `SELECT spend_paise FROM ad_campaigns WHERE id = $1`, camp.ID).Scan(&spend); err != nil {
		t.Fatalf("spend read: %v", err)
	}
	if spend != served.PricePaise {
		t.Fatalf("spend_paise = %d, want %d", spend, served.PricePaise)
	}

	// Clearing the bid drops the campaign out of auctions (fail-closed).
	cleared, err := campSvc.Update(ctx, adv, camp.ID, UpdateInput{BidPaise: bidPtrPtr(nil)})
	if err != nil {
		t.Fatalf("clear bid: %v", err)
	}
	if cleared.BidPaise != nil {
		t.Fatalf("bid not cleared: %v", *cleared.BidPaise)
	}
	if got, err := Serve(ctx, pool, log, deps, cfg, now, slot); err != nil || got != nil {
		t.Fatalf("cleared bid: got=%v err=%v, want nil,nil", got, err)
	}

	// Setting an invalid bid is rejected before any write.
	if _, err := campSvc.Update(ctx, adv, camp.ID, UpdateInput{BidPaise: bidPtrPtr(int64ptr(-5))}); !errors.Is(err, ErrInvalidCampaignField) {
		t.Fatalf("negative bid err = %v, want ErrInvalidCampaignField", err)
	}
	if _, err := campSvc.Update(ctx, adv, camp.ID, UpdateInput{BidPaise: bidPtrPtr(int64ptr(200000))}); !errors.Is(err, ErrInvalidCampaignField) {
		t.Fatalf("bid above budget err = %v, want ErrInvalidCampaignField", err)
	}

	// Re-enable the bid and pause the campaign: paused campaigns never serve.
	if _, err := campSvc.Update(ctx, adv, camp.ID, UpdateInput{BidPaise: bidPtrPtr(int64ptr(100))}); err != nil {
		t.Fatalf("re-set bid: %v", err)
	}
	if _, err := campSvc.Transition(ctx, adv, camp.ID, CampaignPaused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got, err := Serve(ctx, pool, log, deps, cfg, now, slot); err != nil || got != nil {
		t.Fatalf("paused campaign: got=%v err=%v, want nil,nil", got, err)
	}
}
