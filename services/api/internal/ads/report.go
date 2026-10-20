// SPDX-License-Identifier: AGPL-3.0-or-later
// Campaign reporting v1 (H3 part 3).
//
// Reporting v1 is honest accounting, not vanity analytics. Everything in
// CampaignReport comes from the campaign row itself (budget, spend,
// window, status) plus arithmetic against the wall clock. Delivery
// numbers — impressions, clicks, CTR — do NOT exist in v1: no delivery
// pipeline writes them yet, and this endpoint refuses to invent them.
// The serving integration (docs/ads.md) adds an ad_events ledger and a
// delivery section to this report; until then Delivery is a stub that
// says so in plain language.
//
// The pacing projection (spend_per_day, projected_exhaustion_at) exists
// so an advertiser can see a runaway budget early. It degrades to nulls
// instead of guesses when there is no spend history to project from.
package ads

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// ReportDelivery is the honest placeholder for the delivery analytics
// that the serving integration will add. present=false until the
// ad_events ledger lands (docs/ads.md: the exact schema is specified
// there so the extension is a real contract, not a TODO).
type ReportDelivery struct {
	Present bool `json:"present"`
}

// CampaignReport is the v1 report payload. Money is integer paise,
// times are RFC3339.
type CampaignReport struct {
	CampaignID            pgtype.UUID    `json:"campaign_id"`
	Name                  string         `json:"name"`
	Status                string         `json:"status"`
	BudgetPaise           int64          `json:"budget_paise"`
	SpendPaise            int64          `json:"spend_paise"`
	RemainingPaise        int64          `json:"remaining_paise"`
	DailyBudgetPaise      *int64         `json:"daily_budget_paise,omitempty"`
	EligibleNow           bool           `json:"eligible_now"`
	EligibilityReason     string         `json:"eligibility_reason,omitempty"`
	DaysLive              float64        `json:"days_live"`
	SpendPerDayPaise      *float64       `json:"spend_per_day_paise,omitempty"`
	ProjectedExhaustionAt *time.Time     `json:"projected_exhaustion_at,omitempty"`
	Delivery              ReportDelivery `json:"delivery"`
}

// eligibilityReason mirrors Eligible(c, t) check-for-check but names the
// blocking reason. "" means eligible. The checks are duplicated (not
// delegated) so the reason is specific; report_test.go pins the two in
// lockstep so they cannot drift.
func eligibilityReason(c *Campaign, t time.Time) string {
	switch {
	case c.Status == CampaignArchived:
		return "archived"
	case c.Status != CampaignActive:
		return "not_active"
	case c.StartAt != nil && t.Before(*c.StartAt):
		return "window_not_started"
	case c.EndAt != nil && !t.Before(*c.EndAt):
		return "window_closed"
	case c.SpendPaise >= c.BudgetPaise:
		return "budget_exhausted"
	default:
		return ""
	}
}

// Report builds the v1 report for a campaign at time now. Pure — no
// database, no clock — so every pacing edge is unit-testable.
func BuildReport(c *Campaign, now time.Time) *CampaignReport {
	reason := eligibilityReason(c, now)
	r := &CampaignReport{
		CampaignID:       c.ID,
		Name:             c.Name,
		Status:           c.Status,
		BudgetPaise:      c.BudgetPaise,
		SpendPaise:       c.SpendPaise,
		RemainingPaise:   RemainingBudget(c),
		DailyBudgetPaise: c.DailyBudgetPaise,
		EligibleNow:      reason == "",
		Delivery:         ReportDelivery{Present: false},
	}
	if reason != "" {
		r.EligibilityReason = reason
	}
	// Pacing: only meaningful once the campaign has lived a while and
	// has actually spent. A fresh or zero-spend campaign projects
	// nothing — nulls, not guesses.
	days := now.Sub(c.CreatedAt).Hours() / 24
	if days < 0 {
		days = 0
	}
	r.DaysLive = days
	if c.SpendPaise > 0 && days >= 0.25 {
		rate := float64(c.SpendPaise) / days
		r.SpendPerDayPaise = &rate
		if remaining := RemainingBudget(c); remaining > 0 && rate > 0 {
			exhaust := now.Add(time.Duration(float64(remaining)/rate*24) * time.Hour)
			r.ProjectedExhaustionAt = &exhaust
		}
	}
	return r
}

// report serves GET /v1/ads/campaigns/{id}/report: the v1 accounting
// report. Owner-scoped exactly like GET /v1/ads/campaigns/{id} (403
// not_owner, never a cross-advertiser leak).
func (h *CampaignHandler) Report(w http.ResponseWriter, r *http.Request) {
	id, ok := targetCampaignUUID(w, r)
	if !ok {
		return
	}
	advertiser, ok := h.advertiseScope(w, r)
	if !ok {
		return
	}
	c, err := h.svc.Get(r.Context(), advertiser, id)
	if err != nil {
		h.adsError(w, r, "report campaign", err)
		return
	}
	writeAdsJSON(w, http.StatusOK, BuildReport(c, time.Now()))
}
