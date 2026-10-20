// SPDX-License-Identifier: AGPL-3.0-or-later
// Serving (H3 part 3b): the decision path that turns an ad slot into a
// billed impression.
//
// Flow for one slot:
//  1. ListActiveBidEnabled — ACTIVE campaigns that carry a bid
//     (no bid = fail-closed, never enters an auction).
//  2. Eligible(now) per campaign — window open + spend < budget.
//  3. ActiveCreatives per surviving campaign — a campaign with no
//     active creative cannot serve (nothing to render).
//  4. ContextualQuality — Promovolve-pattern contextual scoring:
//     page/slot content matched against the campaign's targeting.
//     No cookies, no profiles, no lookalikes — contextual > identity.
//  5. Auction kernel (auction.go) — rank = bid × quality, GSP price.
//  6. One transaction: INSERT ad_events + spend_paise accrual + the
//     55/45 creator/platform split recorded on the row (ledger-only
//     until F4's Cashfree onboarding — no money movement rails today).
//
// Env: ADS_SERVING_ENABLED (default OFF — fail-closed like every other
// seam), ADS_RESERVE_PAISE (default 50).
package ads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

// Revenue split: the creator whose content carries the ad keeps 55%
// (YouTube parity — the MINTZO CREATOR ECONOMICS decision), the
// platform keeps 45%.
const (
	creatorSharePct     = 55
	defaultReservePaise = int64(50)
)

// ErrServingDisabled is returned when ADS_SERVING_ENABLED is not set.
// The serving seam is fail-closed: disabled by default, so a half-
// wired integration can never spend advertiser money.
var ErrServingDisabled = errors.New("ads serving is disabled (ADS_SERVING_ENABLED)")

// ErrBudgetRaced is returned when the budget drains between the
// eligibility read and the billing transaction (concurrent winners).
// The transaction is rolled back — no event row, no spend — so the
// caller can treat it as "no ad served" rather than a billing error.
var ErrBudgetRaced = errors.New("budget exhausted during billing transaction")

// ServingConfig is the serving seam's runtime config. Read once at
// startup via ServingConfigFromEnv; Serve refuses to run when
// Enabled is false.
type ServingConfig struct {
	Enabled      bool
	ReservePaise int64
}

// ServingConfigFromEnv reads ADS_SERVING_ENABLED / ADS_RESERVE_PAISE.
// Serving stays OFF unless explicitly enabled — the same fail-closed
// posture as the D1 OTP adapter and the F4 Cashfree seam.
func ServingConfigFromEnv() ServingConfig {
	cfg := ServingConfig{ReservePaise: defaultReservePaise}
	v := strings.TrimSpace(strings.ToLower(os.Getenv("ADS_SERVING_ENABLED")))
	cfg.Enabled = v == "1" || v == "true" || v == "yes" || v == "on"
	if r := strings.TrimSpace(os.Getenv("ADS_RESERVE_PAISE")); r != "" {
		if n, err := strconv.ParseInt(r, 10, 64); err == nil && n > 0 {
			cfg.ReservePaise = n
		}
	}
	return cfg
}

// SlotContext is the publisher-supplied context for one ad slot. It is
// CONTENT, never identity: page categories, content language, publisher
// country. No user IDs, no device fingerprints, no behavioral signals
// — the contextual>identity rule from docs/ads.md is enforced by this
// struct having nowhere to put them.
type SlotContext struct {
	// Slot names the placement (e.g. "feed_slot_1").
	Slot string
	// Categories are the page/content category slugs (real catalog tree).
	Categories []string
	// Language is the content's ISO-639-1 language.
	Language string
	// Country is the publisher's ISO-3166-1 country.
	Country string
}

// qualityBase is the floor score: a servable ad with no contextual
// signal still bids, it just never outranks a contextually matched one.
const qualityBase = 0.5

// qualityCategoryBoost rewards contextual fit: the slot's content
// categories intersect the campaign's targeting categories.
const qualityCategoryBoost = 0.35

// qualityLanguageBoost rewards language fit.
const qualityLanguageBoost = 0.15

// ContextualQuality scores one creative for one slot from page content
// only (Promovolve pattern). Pure function — deterministic, replayable,
// unit-testable. Quality is CONSUMED by the auction kernel, never
// computed there; and the kernel's junk exclusion (quality == 0) still
// applies, so this function clamps to a small positive floor instead of
// ever returning 0.
func ContextualQuality(c *Campaign, slot SlotContext) float64 {
	q := qualityBase
	cats := map[string]bool{}
	for _, cat := range c.Targeting.Categories {
		cats[strings.ToLower(strings.TrimSpace(cat))] = true
	}
	for _, cat := range slot.Categories {
		if cats[strings.ToLower(strings.TrimSpace(cat))] {
			q += qualityCategoryBoost
			break
		}
	}
	if slot.Language != "" {
		for _, lang := range c.Targeting.Languages {
			if strings.EqualFold(strings.TrimSpace(lang), strings.TrimSpace(slot.Language)) {
				q += qualityLanguageBoost
				break
			}
		}
	}
	// The kernel requires quality in (0,1]: clamp, never zero.
	if q <= 0 {
		q = 0.05
	}
	if q > 1 {
		q = 1
	}
	return q
}

// ServeCandidate is one auction entrant: an eligible campaign, the
// creative that will render if it wins, and its contextual quality.
type ServeCandidate struct {
	Campaign *Campaign
	Creative *Creative
	Quality  float64
}

// AuctionDecision is the serving path's answer: the winning creative
// and the GSP price actually charged.
type AuctionDecision struct {
	Campaign   *Campaign
	Creative   *Creative
	PricePaise int64
}

// Decide runs the pure decision: candidates → auction kernel → winner.
// Returns nil when nothing is servable (no candidates, all bids below
// reserve, or kernel error — the kernel's junk exclusion applies before
// ranking, so junk can never set prices). Pure — no DB, replayable for
// billing disputes.
func Decide(reservePaise int64, cands []ServeCandidate) (*AuctionDecision, error) {
	bids := make([]Bid, 0, len(cands))
	byBidder := map[string]ServeCandidate{}
	for _, cand := range cands {
		if cand.Campaign == nil || cand.Creative == nil {
			continue
		}
		if cand.Campaign.BidPaise == nil || *cand.Campaign.BidPaise <= 0 {
			continue // no bid = fail-closed, never enters the auction
		}
		if cand.Quality <= 0 {
			continue
		}
		bidderID := creativeBidderID(cand.Creative)
		bids = append(bids, Bid{
			BidderID: bidderID,
			BidPaise: *cand.Campaign.BidPaise,
			Quality:  cand.Quality,
		})
		byBidder[bidderID] = cand
	}
	res, ok, err := Winner(bids, reservePaise)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil // nothing servable — not an error
	}
	cand := byBidder[res.BidderID]
	return &AuctionDecision{
		Campaign:   cand.Campaign,
		Creative:   cand.Creative,
		PricePaise: res.PricePaise,
	}, nil
}

// creativeBidderID is the auction's bidder identity: one creative, one
// bid. Campaign-level bid money × per-creative contextual quality —
// the winning CREATIVE renders.
func creativeBidderID(cr *Creative) string {
	return "creative:" + uuidString(cr.ID)
}

// uuidString renders a pgtype.UUID without pulling in another dep.
func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ServedAd is the billable outcome of Serve.
type ServedAd struct {
	ImpressionID       pgtype.UUID `json:"impression_id"`
	CampaignID         pgtype.UUID `json:"campaign_id"`
	CreativeID         pgtype.UUID `json:"creative_id"`
	Slot               string      `json:"slot"`
	PricePaise         int64       `json:"price_paise"`
	CreatorSharePaise  int64       `json:"creator_share_paise"`
	PlatformSharePaise int64       `json:"platform_share_paise"`
	Creative           *Creative   `json:"creative"`
}

// ServingDeps are Serve's service dependencies (stub-able seam).
type ServingDeps struct {
	Campaigns *CampaignService
	Creatives *CreativeService
}

// Serve fills one slot. It returns (nil, nil) when nothing is servable
// — disabled seam, no eligible campaigns, no winner — which is a normal
// outcome, not an error: the publisher renders organic content instead.
//
// When a winner exists, the billing is atomic: the ad_events row and
// the spend_paise accrual land in ONE transaction, guarded by a
// spend_paise + price <= budget_paise predicate (a concurrent winner
// that drains the budget makes the accrual update touch 0 rows →
// ErrBudgetRaced → full rollback, never a CHECK-violation partial).
func Serve(ctx context.Context, pool *db.Pool, log *slog.Logger, deps ServingDeps, cfg ServingConfig, now time.Time, slot SlotContext) (*ServedAd, error) {
	if !cfg.Enabled {
		return nil, ErrServingDisabled
	}
	campaigns, err := deps.Campaigns.ListActiveBidEnabled(ctx)
	if err != nil {
		return nil, err
	}
	cands := make([]ServeCandidate, 0, len(campaigns))
	for _, c := range campaigns {
		if !Eligible(c, now) {
			continue
		}
		creatives, err := deps.Creatives.ActiveCreatives(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		if len(creatives) == 0 {
			continue // nothing to render — fail-closed
		}
		// The first active creative represents the campaign in this
		// auction (multi-creative rotation is a later serving pass;
		// serving one creative per campaign keeps billing attributable).
		cr := creatives[0]
		cands = append(cands, ServeCandidate{
			Campaign: c,
			Creative: cr,
			Quality:  ContextualQuality(c, slot),
		})
	}
	decision, err := Decide(cfg.ReservePaise, cands)
	if err != nil {
		return nil, err
	}
	if decision == nil {
		return nil, nil // nothing servable
	}
	return billImpression(ctx, pool, log, decision, slot.Slot)
}

// billImpression writes the delivery-ledger row and accrues spend in
// one transaction. The 55/45 split is recorded on the row (ledger-only
// until F4's Cashfree onboarding — documented in docs/ads.md).
func billImpression(ctx context.Context, pool *db.Pool, log *slog.Logger, d *AuctionDecision, slot string) (*ServedAd, error) {
	creatorShare := d.PricePaise * creatorSharePct / 100
	platformShare := d.PricePaise - creatorShare
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var impressionID pgtype.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO ad_events (campaign_id, creative_id, slot, price_paise, creator_share_paise, platform_share_paise)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING impression_id`,
		d.Campaign.ID, d.Creative.ID, slot, d.PricePaise, creatorShare, platformShare).Scan(&impressionID); err != nil {
		return nil, err
	}
	// Atomic accrual with a budget predicate: if a concurrent winner
	// drained the budget between Eligible() and now, this touches 0
	// rows → rollback → ErrBudgetRaced (never a partial bill).
	res, err := tx.Exec(ctx, `
		UPDATE ad_campaigns
		SET spend_paise = spend_paise + $1, updated_at = now()
		WHERE id = $2 AND spend_paise + $1 <= budget_paise`,
		d.PricePaise, d.Campaign.ID)
	if err != nil {
		return nil, err
	}
	if res.RowsAffected() == 0 {
		return nil, ErrBudgetRaced
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if log != nil {
		log.Info("ad impression billed",
			"impression_id", uuidString(impressionID),
			"campaign_id", uuidString(d.Campaign.ID),
			"price_paise", d.PricePaise)
	}
	return &ServedAd{
		ImpressionID:       impressionID,
		CampaignID:         d.Campaign.ID,
		CreativeID:         d.Creative.ID,
		Slot:               slot,
		PricePaise:         d.PricePaise,
		CreatorSharePaise:  creatorShare,
		PlatformSharePaise: platformShare,
		Creative:           d.Creative,
	}, nil
}
