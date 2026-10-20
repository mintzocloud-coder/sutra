// SPDX-License-Identifier: AGPL-3.0-or-later
package ads

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// testReportCampaign builds a campaign fixture at a fixed "now".
func testReportCampaign(status string, budget, spend int64, createdAt time.Time) *Campaign {
	return &Campaign{
		ID:          pgtype.UUID{Bytes: [16]byte{1, 2, 3, 4}, Valid: true},
		Name:        "report fixture",
		Status:      status,
		BudgetPaise: budget,
		SpendPaise:  spend,
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
	}
}

// TestEligibilityReasonLocksToEligible pins eligibilityReason to Eligible
// check-for-check: the two must never drift (one returning a reason
// while the other says eligible, or vice versa).
func TestEligibilityReasonLocksToEligible(t *testing.T) {
	now := time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)
	past := now.Add(-48 * time.Hour)
	future := now.Add(48 * time.Hour)
	cases := []struct {
		name string
		c    *Campaign
	}{
		{"active", testReportCampaign(CampaignActive, 10000, 0, now.Add(-24*time.Hour))},
		{"draft", testReportCampaign(CampaignDraft, 10000, 0, now.Add(-24*time.Hour))},
		{"paused", testReportCampaign(CampaignPaused, 10000, 0, now.Add(-24*time.Hour))},
		{"archived", testReportCampaign(CampaignArchived, 10000, 0, now.Add(-24*time.Hour))},
		{"exhausted", testReportCampaign(CampaignActive, 10000, 10000, now.Add(-24*time.Hour))},
		{"not started", func() *Campaign {
			c := testReportCampaign(CampaignActive, 10000, 0, now.Add(-24*time.Hour))
			c.StartAt = &future
			return c
		}()},
		{"window closed", func() *Campaign {
			c := testReportCampaign(CampaignActive, 10000, 0, now.Add(-72*time.Hour))
			c.EndAt = &past
			return c
		}()},
	}
	for _, tc := range cases {
		got := eligibilityReason(tc.c, now)
		eligible := Eligible(tc.c, now)
		if (got == "") != eligible {
			t.Errorf("%s: reason=%q but Eligible=%v (drift!)", tc.name, got, eligible)
		}
	}
}

// TestBuildReportFreshCampaign covers the zero-spend edge: no rate, no
// projection, eligible when active.
func TestBuildReportFreshCampaign(t *testing.T) {
	now := time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)
	c := testReportCampaign(CampaignActive, 500000, 0, now.Add(-6*time.Hour))
	r := BuildReport(c, now)
	if !r.EligibleNow || r.EligibilityReason != "" {
		t.Errorf("fresh active campaign must be eligible, got %q", r.EligibilityReason)
	}
	if r.RemainingPaise != 500000 {
		t.Errorf("remaining = %d, want 500000", r.RemainingPaise)
	}
	if r.SpendPerDayPaise != nil || r.ProjectedExhaustionAt != nil {
		t.Error("zero spend must project nothing, not guesses")
	}
	if r.Delivery.Present {
		t.Error("v1 delivery must report present=false")
	}
}

// TestBuildReportPacingMath hand-checks the projection: 10 days live,
// 40000 paise spent of 100000 → 4000/day → 60000 remaining → 15 days →
// exhaustion at now+15d.
func TestBuildReportPacingMath(t *testing.T) {
	now := time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)
	c := testReportCampaign(CampaignActive, 100000, 40000, now.Add(-240*time.Hour))
	r := BuildReport(c, now)
	if r.SpendPerDayPaise == nil || *r.SpendPerDayPaise != 4000 {
		t.Errorf("spend/day = %v, want 4000", r.SpendPerDayPaise)
	}
	want := now.Add(15 * 24 * time.Hour)
	if r.ProjectedExhaustionAt == nil || r.ProjectedExhaustionAt.Sub(want).Abs() > time.Second {
		t.Errorf("exhaustion = %v, want ~%v", r.ProjectedExhaustionAt, want)
	}
}

// TestBuildReportExhausted covers the terminal edge: remaining 0,
// no projection (nothing left to project), not eligible.
func TestBuildReportExhausted(t *testing.T) {
	now := time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)
	c := testReportCampaign(CampaignActive, 100000, 100000, now.Add(-240*time.Hour))
	r := BuildReport(c, now)
	if r.EligibleNow || r.EligibilityReason != "budget_exhausted" {
		t.Errorf("exhausted campaign: eligible=%v reason=%q", r.EligibleNow, r.EligibilityReason)
	}
	if r.RemainingPaise != 0 {
		t.Errorf("remaining = %d, want 0", r.RemainingPaise)
	}
	if r.ProjectedExhaustionAt != nil {
		t.Error("exhausted campaign must not project an exhaustion date")
	}
}

// TestBuildReportPausedNotProjecting: a paused campaign still reports
// its spend rate (the money DID move) but is not eligible.
func TestBuildReportPausedNotProjecting(t *testing.T) {
	now := time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)
	c := testReportCampaign(CampaignPaused, 100000, 20000, now.Add(-120*time.Hour))
	r := BuildReport(c, now)
	if r.EligibleNow || r.EligibilityReason != "not_active" {
		t.Errorf("paused: eligible=%v reason=%q", r.EligibleNow, r.EligibilityReason)
	}
	if r.SpendPerDayPaise == nil || *r.SpendPerDayPaise != 4000 {
		t.Errorf("spend/day = %v, want 4000", r.SpendPerDayPaise)
	}
}
