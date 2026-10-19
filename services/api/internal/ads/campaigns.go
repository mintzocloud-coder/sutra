// SPDX-License-Identifier: AGPL-3.0-or-later
// Campaigns (H3 part 2): the advertiser API over the GSP auction kernel.
//
// Every route is seller-scoped: the advertiser is the caller's seller
// row (the same seam the WhatsApp/Tally/Zoho handlers use), so there is
// no ad-account concept to phish and no cross-seller visibility. Money
// is integer paise. Budget guardrails are enforced here AND by the
// migration-0017 CHECKs — the API never trusts the client with money.
//
// Rules (all enforced here, not left to clients):
//   - campaigns start as draft; auctions only see campaigns where
//     Eligible() is true (status active, window open, spend < budget).
//   - status follows the transition matrix (draft->active, active->paused,
//     paused->active, *->archived); archive is terminal, no un-archive.
//   - spend is NEVER settable through this API — part 3's reporting
//     path (backed by auction billing) owns it; the 0017 CHECK
//     (spend <= budget) is a hard stop.
//   - targeting keys are a closed set (unknown keys are loud errors,
//     never silent no-ops — a typo'd targeting key that silently
//     matches everything would spend real money);
//     countries = ISO 3166-1 alpha-2 lowercase; languages = ISO 639-1
//     lowercase; categories = real category slugs from the catalog
//     seed tree; devices = mobile/tablet/desktop.
//   - update writes are owner-only; GET/PATCH/DELETE on another
//     advertiser's campaign is 403/404 (never leaks existence).
package ads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

// Campaign statuses. See migration 0017 for the DB-level rules;
// Transitions documents the allowed matrix.
const (
	CampaignDraft    = "draft"
	CampaignActive   = "active"
	CampaignPaused   = "paused"
	CampaignArchived = "archived"
)

// CampaignTransitions is the allowed status state machine. Archiving is
// terminal and reachable from everywhere; activate covers both first
// activation and resume-from-pause so the HTTP surface stays small.
var CampaignTransitions = map[string][]string{
	CampaignDraft:    {CampaignActive, CampaignArchived},
	CampaignActive:   {CampaignPaused, CampaignArchived},
	CampaignPaused:   {CampaignActive, CampaignArchived},
	CampaignArchived: {},
}

// Device allowlist for targeting.devices. A closed set keeps the
// targeting semantics exact — anything else is a new platform
// capability, never an ad-hoc string.
var deviceAllowlist = map[string]struct{}{
	"mobile":  {},
	"tablet":  {},
	"desktop": {},
}

var (
	countryRe  = regexp.MustCompile(`^[a-z]{2}$`)
	languageRe = regexp.MustCompile(`^[a-z]{2}$`)
	// Category slugs come from the catalog seed tree ([a-z0-9-] shape).
	categoryRe = regexp.MustCompile(`^[a-z0-9-]{2,64}$`)
)

// Sentinel errors; handlers map them to the error envelope.
var (
	ErrCampaignNotFound      = errors.New("campaign not found")
	ErrCampaignNotOwner      = errors.New("not the campaign owner")
	ErrInvalidCampaignField  = errors.New("invalid campaign field")
	ErrInvalidTargeting      = errors.New("invalid targeting")
	ErrBadCampaignTransition = errors.New("status transition not allowed")
	ErrUnknownCategorySlug   = errors.New("unknown category slug")
	ErrInvalidCampaignCursor = errors.New("invalid campaign cursor")
)

const (
	maxCampaignNameLen = 128
	// maxRupeesPaise bounds a single campaign's budget: ₹10 crore in
	// paise. Beyond that a campaign needs a human conversation (and the
	// platform's spend-velocity guards), not an API field.
	maxBudgetPaise = int64(10_00_00_000) * 100
)

// Targeting is the advertiser's audience filter. All slices are
// inclusion lists (match ANY element); empty/nil = no constraint on
// that axis. JSON keys outside this struct are rejected at write time.
type Targeting struct {
	Countries  []string `json:"countries,omitempty"`
	Languages  []string `json:"languages,omitempty"`
	Categories []string `json:"categories,omitempty"`
	Devices    []string `json:"devices,omitempty"`
}

// Campaign is the advertiser record.
type Campaign struct {
	ID               pgtype.UUID `json:"id"`
	AdvertiserID     pgtype.UUID `json:"advertiser_id"`
	Name             string      `json:"name"`
	Status           string      `json:"status"`
	BudgetPaise      int64       `json:"budget_paise"`
	SpendPaise       int64       `json:"spend_paise"`
	DailyBudgetPaise *int64      `json:"daily_budget_paise,omitempty"`
	Targeting        Targeting   `json:"targeting"`
	StartAt          *time.Time  `json:"start_at,omitempty"`
	EndAt            *time.Time  `json:"end_at,omitempty"`
	CreatedAt        time.Time   `json:"created_at"`
	UpdatedAt        time.Time   `json:"updated_at"`
}

// Eligible reports whether the campaign may enter auctions at time t.
// Pure function — part 3's serving path calls this before building a
// Bid from the campaign, so auction eligibility is inspectable and
// unit-testable without a database. The budget and window rules here
// mirror the 0017 CHECKs (defense in depth, not duplication of trust).
func Eligible(c *Campaign, t time.Time) bool {
	if c.Status != CampaignActive {
		return false
	}
	if c.StartAt != nil && t.Before(*c.StartAt) {
		return false
	}
	if c.EndAt != nil && !t.Before(*c.EndAt) {
		return false
	}
	// An exhausted budget stops serving even when the status is still
	// active (status flips to paused in part 3's billing path; the
	// kernel must not depend on that flip having landed).
	if c.SpendPaise >= c.BudgetPaise {
		return false
	}
	return true
}

// RemainingBudget is the spendable remainder (budget - spend), never
// negative. The auction's per-impression price must be <= this.
func RemainingBudget(c *Campaign) int64 {
	if r := c.BudgetPaise - c.SpendPaise; r > 0 {
		return r
	}
	return 0
}

// validateTargeting checks the closed shape: unknown keys are loud
// errors (a typo'd key that silently matched everything would spend
// real money), value grammars are exact, and category slugs must
// exist in the catalog tree (verified in Create/Update, not here —
// pure so the grammar matrix is unit-testable without a DB).
func validateTargeting(raw json.RawMessage) (Targeting, error) {
	if len(raw) == 0 {
		return Targeting{}, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Targeting{}, fmt.Errorf("%w: targeting must be a JSON object", ErrInvalidTargeting)
	}
	var t Targeting
	for k, v := range obj {
		var vals []string
		if err := json.Unmarshal(v, &vals); err != nil {
			return Targeting{}, fmt.Errorf("%w: %q must be an array of strings", ErrInvalidTargeting, k)
		}
		seen := map[string]struct{}{}
		for _, s := range vals {
			s = strings.TrimSpace(s)
			if s == "" {
				return Targeting{}, fmt.Errorf("%w: %q contains an empty value", ErrInvalidTargeting, k)
			}
			if _, dup := seen[s]; dup {
				return Targeting{}, fmt.Errorf("%w: %q contains duplicate %q", ErrInvalidTargeting, k, s)
			}
			seen[s] = struct{}{}
		}
		switch k {
		case "countries":
			for _, s := range vals {
				if !countryRe.MatchString(s) {
					return Targeting{}, fmt.Errorf("%w: %q is not an ISO 3166-1 alpha-2 code", ErrInvalidTargeting, s)
				}
			}
			t.Countries = vals
		case "languages":
			for _, s := range vals {
				if !languageRe.MatchString(s) {
					return Targeting{}, fmt.Errorf("%w: %q is not an ISO 639-1 code", ErrInvalidTargeting, s)
				}
			}
			t.Languages = vals
		case "categories":
			for _, s := range vals {
				if !categoryRe.MatchString(s) {
					return Targeting{}, fmt.Errorf("%w: %q is not a category slug", ErrInvalidTargeting, s)
				}
			}
			t.Categories = vals
		case "devices":
			for _, s := range vals {
				if _, ok := deviceAllowlist[s]; !ok {
					return Targeting{}, fmt.Errorf("%w: %q is not a known device (mobile/tablet/desktop)", ErrInvalidTargeting, s)
				}
			}
			t.Devices = vals
		default:
			return Targeting{}, fmt.Errorf("%w: unknown targeting key %q (allowed: countries, languages, categories, devices)", ErrInvalidTargeting, k)
		}
	}
	return t, nil
}

// CampaignService is the advertiser store; methods map 1:1 to the HTTP
// surface. The advertiser is always a seller id (seller-scoped routes).
type CampaignService struct{ pool *db.Pool }

// NewCampaignService builds the store on an existing pool.
func NewCampaignService(pool *db.Pool) *CampaignService { return &CampaignService{pool: pool} }

// CreateInput is the POST /v1/ads/campaigns body.
type CreateInput struct {
	Name             string          `json:"name"`
	BudgetPaise      int64           `json:"budget_paise"`
	DailyBudgetPaise *int64          `json:"daily_budget_paise"`
	Targeting        json.RawMessage `json:"targeting"`
	StartAt          *time.Time      `json:"start_at"`
	EndAt            *time.Time      `json:"end_at"`
}

// validateMoney checks the budget fields before any write. All values
// are integer paise; the platform never prices ads in fractional paise.
func validateMoney(budget int64, daily *int64) error {
	if budget <= 0 || budget > maxBudgetPaise {
		return fmt.Errorf("%w: budget_paise must be 1..%d paise", ErrInvalidCampaignField, maxBudgetPaise)
	}
	if daily != nil {
		if *daily <= 0 {
			return fmt.Errorf("%w: daily_budget_paise must be positive", ErrInvalidCampaignField)
		}
		if *daily > budget {
			return fmt.Errorf("%w: daily_budget_paise cannot exceed budget_paise", ErrInvalidCampaignField)
		}
	}
	return nil
}

// validateWindow checks the optional flight dates.
func validateWindow(start, end *time.Time) error {
	if start != nil && end != nil && !end.After(*start) {
		return fmt.Errorf("%w: end_at must be after start_at", ErrInvalidCampaignField)
	}
	return nil
}

// resolveCategorySlugs verifies every targeting category slug exists in
// the catalog tree. The lookup runs in the caller's tx so the write is
// consistent: a category deleted between check and insert can only
// fail the write if we also lock it — overkill; the check rejects
// typos and stale slugs, which is the actual failure mode.
func resolveCategorySlugs(ctx context.Context, tx pgx.Tx, slugs []string) error {
	if len(slugs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT slug FROM categories WHERE slug = ANY($1)`, slugs)
	if err != nil {
		return err
	}
	found := map[string]struct{}{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return err
		}
		found[s] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, s := range slugs {
		if _, ok := found[s]; !ok {
			return fmt.Errorf("%w: %q", ErrUnknownCategorySlug, s)
		}
	}
	return nil
}

// Create registers a draft campaign for the advertiser (seller).
// Campaigns always start as draft — nothing enters auctions until the
// advertiser explicitly activates it.
func (s *CampaignService) Create(ctx context.Context, advertiser pgtype.UUID, in CreateInput) (*Campaign, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > maxCampaignNameLen {
		return nil, fmt.Errorf("%w: name must be 1-%d characters", ErrInvalidCampaignField, maxCampaignNameLen)
	}
	if err := validateMoney(in.BudgetPaise, in.DailyBudgetPaise); err != nil {
		return nil, err
	}
	if err := validateWindow(in.StartAt, in.EndAt); err != nil {
		return nil, err
	}
	targeting, err := validateTargeting(in.Targeting)
	if err != nil {
		return nil, err
	}
	targetingJSON, err := json.Marshal(targeting)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := resolveCategorySlugs(ctx, tx, targeting.Categories); err != nil {
		return nil, err
	}
	var c Campaign
	row := tx.QueryRow(ctx, `
		INSERT INTO ad_campaigns (advertiser_id, name, budget_paise, daily_budget_paise, targeting, start_at, end_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)
		RETURNING id, advertiser_id, name, status, budget_paise, spend_paise, daily_budget_paise, targeting, start_at, end_at, created_at, updated_at`,
		advertiser, name, in.BudgetPaise, in.DailyBudgetPaise, string(targetingJSON), in.StartAt, in.EndAt)
	if err := scanCampaign(row, &c); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &c, nil
}

// Get fetches one campaign. Another advertiser's campaign is
// indistinguishable from a missing one (404) — the owner check happens
// on write paths only, so reads never leak existence.
func (s *CampaignService) Get(ctx context.Context, advertiser, id pgtype.UUID) (*Campaign, error) {
	var c Campaign
	row := s.pool.QueryRow(ctx, `
		SELECT id, advertiser_id, name, status, budget_paise, spend_paise, daily_budget_paise, targeting, start_at, end_at, created_at, updated_at
		FROM ad_campaigns WHERE id = $1 AND status <> 'archived'`, id)
	if err := scanCampaign(row, &c); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCampaignNotFound
		}
		return nil, err
	}
	if c.AdvertiserID != advertiser {
		return nil, ErrCampaignNotOwner
	}
	return &c, nil
}

// UpdateInput is the PATCH /v1/ads/campaigns/{id} body. Every field is
// optional (nil = leave untouched). Status is NOT patchable here — it
// moves via the activate/pause/resume/archive routes so every state
// change is explicit. Spend is NEVER settable through this API.
type UpdateInput struct {
	Name             *string          `json:"name"`
	BudgetPaise      *int64           `json:"budget_paise"`
	DailyBudgetPaise **int64          `json:"daily_budget_paise"` // double pointer: explicit null clears
	Targeting        *json.RawMessage `json:"targeting"`
	StartAt          **time.Time      `json:"start_at"`
	EndAt            **time.Time      `json:"end_at"`
}

// Update applies an owner-only partial update. Archived campaigns are
// invisible (404), matching Get.
func (s *CampaignService) Update(ctx context.Context, advertiser, id pgtype.UUID, in UpdateInput) (*Campaign, error) {
	c, err := s.Get(ctx, advertiser, id)
	if err != nil {
		return nil, err
	}
	// Compute the effective post-update money/window and validate them
	// TOGETHER — a budget raise paired with an end-date change must be
	// checked as one state, not two partial ones.
	budget := c.BudgetPaise
	daily := c.DailyBudgetPaise
	start := c.StartAt
	end := c.EndAt
	if in.BudgetPaise != nil {
		budget = *in.BudgetPaise
	}
	if in.DailyBudgetPaise != nil {
		daily = *in.DailyBudgetPaise
	}
	if in.StartAt != nil {
		start = *in.StartAt
	}
	if in.EndAt != nil {
		end = *in.EndAt
	}
	if err := validateMoney(budget, daily); err != nil {
		return nil, err
	}
	if budget < c.SpendPaise {
		return nil, fmt.Errorf("%w: budget_paise cannot be lowered below current spend (%d paise)", ErrInvalidCampaignField, c.SpendPaise)
	}
	if err := validateWindow(start, end); err != nil {
		return nil, err
	}
	var targetingJSON []byte
	if in.Targeting != nil {
		t, err := validateTargeting(*in.Targeting)
		if err != nil {
			return nil, err
		}
		targetingJSON, err = json.Marshal(t)
		if err != nil {
			return nil, err
		}
	}
	set := []string{}
	args := []any{}
	n := 0
	add := func(col string, v any) {
		n++
		set = append(set, fmt.Sprintf("%s = $%d", col, n))
		args = append(args, v)
	}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len([]rune(name)) > maxCampaignNameLen {
			return nil, fmt.Errorf("%w: name must be 1-%d characters", ErrInvalidCampaignField, maxCampaignNameLen)
		}
		add("name", name)
	}
	if in.BudgetPaise != nil {
		add("budget_paise", budget)
	}
	if in.DailyBudgetPaise != nil {
		if *in.DailyBudgetPaise == nil {
			add("daily_budget_paise", nil)
		} else {
			add("daily_budget_paise", *in.DailyBudgetPaise)
		}
	}
	if in.Targeting != nil {
		add("targeting", string(targetingJSON))
	}
	if in.StartAt != nil {
		add("start_at", start)
	}
	if in.EndAt != nil {
		add("end_at", end)
	}
	if len(set) == 0 {
		return c, nil // nothing to change: return current row
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if in.Targeting != nil {
		var t Targeting
		if err := json.Unmarshal(targetingJSON, &t); err != nil {
			return nil, err
		}
		if err := resolveCategorySlugs(ctx, tx, t.Categories); err != nil {
			return nil, err
		}
	}
	n++
	args = append(args, id, advertiser)
	var updated Campaign
	row := tx.QueryRow(ctx, fmt.Sprintf(`
		UPDATE ad_campaigns SET %s, updated_at = now()
		WHERE id = $%d AND advertiser_id = $%d AND status <> 'archived'
		RETURNING id, advertiser_id, name, status, budget_paise, spend_paise, daily_budget_paise, targeting, start_at, end_at, created_at, updated_at`,
		strings.Join(set, ", "), n, n+1), args...)
	if err := scanCampaign(row, &updated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCampaignNotFound
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &updated, nil
}

// Transition moves a campaign through the status state machine
// (owner-only): activate (draft->active, paused->active), pause
// (active->paused), archive (*->archived, terminal). Activation
// additionally requires a live budget and a sane window — a campaign
// that cannot legally serve must not claim "active".
func (s *CampaignService) Transition(ctx context.Context, advertiser, id pgtype.UUID, to string) (*Campaign, error) {
	c, err := s.Get(ctx, advertiser, id)
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, next := range CampaignTransitions[c.Status] {
		if next == to {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("%w: %s -> %s", ErrBadCampaignTransition, c.Status, to)
	}
	if to == CampaignActive {
		if c.SpendPaise >= c.BudgetPaise {
			return nil, fmt.Errorf("%w: budget exhausted — raise budget_paise before activating", ErrInvalidCampaignField)
		}
		if c.EndAt != nil && !c.EndAt.After(time.Now()) {
			return nil, fmt.Errorf("%w: end_at is in the past", ErrInvalidCampaignField)
		}
	}
	var updated Campaign
	row := s.pool.QueryRow(ctx, `
		UPDATE ad_campaigns SET status = $1, updated_at = now()
		WHERE id = $2 AND advertiser_id = $3 AND status = $4
		RETURNING id, advertiser_id, name, status, budget_paise, spend_paise, daily_budget_paise, targeting, start_at, end_at, created_at, updated_at`,
		to, id, advertiser, c.Status)
	if err := scanCampaign(row, &updated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Lost a concurrent transition race — the read-modify-write
			// window is guarded by the status=$4 predicate.
			return nil, fmt.Errorf("%w: campaign changed state concurrently", ErrBadCampaignTransition)
		}
		return nil, err
	}
	return &updated, nil
}

// ListPage is the GET /v1/ads/campaigns response: the advertiser's own
// campaigns, newest first, keyset-paginated on (created_at, id).
type ListPage struct {
	Campaigns  []*Campaign `json:"campaigns"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// List returns the advertiser's campaigns (never anyone else's),
// newest first, keyset-paginated on (created_at, id) — never OFFSET.
func (s *CampaignService) List(ctx context.Context, advertiser pgtype.UUID, limit int, after *AdsCursor) (*ListPage, error) {
	args := []any{advertiser, limit + 1}
	where := "WHERE advertiser_id = $1 AND status <> 'archived'"
	if after != nil {
		where += " AND (created_at, id) < ($3, $4)"
		args = append(args, after.CreatedAt, after.ID)
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT id, advertiser_id, name, status, budget_paise, spend_paise, daily_budget_paise, targeting, start_at, end_at, created_at, updated_at
		FROM ad_campaigns %s
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Campaign{}
	for rows.Next() {
		var c Campaign
		if err := scanCampaign(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	page := &ListPage{}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		page.NextCursor = (&AdsCursor{CreatedAt: last.CreatedAt, ID: last.ID}).Encode()
	}
	page.Campaigns = out
	return page, nil
}

// scanCampaign scans the canonical 12-column row shape.
func scanCampaign(row pgx.Row, c *Campaign) error {
	var targetingJSON []byte
	if err := row.Scan(&c.ID, &c.AdvertiserID, &c.Name, &c.Status,
		&c.BudgetPaise, &c.SpendPaise, &c.DailyBudgetPaise, &targetingJSON,
		&c.StartAt, &c.EndAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return err
	}
	var t Targeting
	if err := json.Unmarshal(targetingJSON, &t); err != nil {
		return err
	}
	c.Targeting = t
	return nil
}
