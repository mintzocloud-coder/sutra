// SPDX-License-Identifier: AGPL-3.0-or-later
// Creatives (H3 part 3a): the creative registry over the GSP auction
// kernel.
//
// A creative is the servable unit of an ad: media + copy + landing.
// Serving (part 3b) reads ACTIVE creatives on ACTIVE campaigns through
// Eligible(); nothing here computes prices — the auction kernel
// (auction.go) is the only place money changes hands, and campaigns.go
// owns the money. This file owns the creative registry: formats, copy
// validation, status lifecycle, and ownership.
//
// Rules (all enforced here, not left to clients):
//   - kind/format are a closed registry: image/video/native kinds with
//     per-kind format allowlists. The migration CHECKs the same
//     compatibility — the Go validation fails first with a clean 400.
//   - media_url/landing_url are http(s) only. javascript:/data: URLs
//     can never enter the registry (XSS-by-creative is a real attack);
//     both the Go validator and the migration CHECK enforce it.
//   - every creative belongs to one campaign; reads and writes are
//     owner-checked through the campaign's advertiser_id. Cross-owner
//     access is 403 (never leaks existence), archived is 404.
//   - status lifecycle: draft → active → archived (terminal). A
//     creative activates only under a non-archived campaign; kind and
//     format are immutable once active (a served placement must never
//     change shape mid-flight).
package ads

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

// Creative kinds. A closed set: serving paths branch on these.
const (
	CreativeKindImage  = "image"
	CreativeKindVideo  = "video"
	CreativeKindNative = "native"
)

// Creative statuses. See migration 0018 for the DB-level rules.
const (
	CreativeDraft    = "draft"
	CreativeActive   = "active"
	CreativeArchived = "archived"
)

// CreativeTransitions is the allowed status state machine. Archive is
// terminal; activation is the only forward edge (a paused campaign's
// creatives stay active — the campaign's Eligible() gates serving,
// not the creative).
var CreativeTransitions = map[string][]string{
	CreativeDraft:    {CreativeActive, CreativeArchived},
	CreativeActive:   {CreativeArchived},
	CreativeArchived: {},
}

// creativeFormats is the per-kind format registry. A format is the
// exact served shape (IAB-style); the registry is closed so a serving
// path never meets a dimension it doesn't know.
var creativeFormats = map[string][]string{
	CreativeKindImage:  {"image_1200x628", "image_1080x1080", "image_1080x1920"},
	CreativeKindVideo:  {"video_1920x1080", "video_1080x1920"},
	CreativeKindNative: {"native"},
}

// creativeCTAs is the call-to-action registry. Closed: a typo'd CTA
// silently rendering nothing would waste real impressions.
var creativeCTAs = map[string]struct{}{
	"":           {},
	"shop_now":   {},
	"learn_more": {},
	"install":    {},
	"sign_up":    {},
	"watch_more": {},
}

// Sentinel errors; handlers map them to the error envelope.
var (
	ErrCreativeNotFound      = errors.New("creative not found")
	ErrCreativeNotOwner      = errors.New("creative belongs to another advertiser")
	ErrInvalidCreativeField  = errors.New("invalid creative field")
	ErrBadCreativeTransition = errors.New("invalid creative status transition")
	ErrInvalidCreativeCursor = errors.New("invalid creative cursor")
	ErrCreativeKindLocked    = errors.New("kind and format are immutable once active")
)

// Creative is one servable ad unit.
type Creative struct {
	ID          pgtype.UUID `json:"id"`
	CampaignID  pgtype.UUID `json:"campaign_id"`
	Name        string      `json:"name"`
	Kind        string      `json:"kind"`
	Format      string      `json:"format"`
	MediaURL    string      `json:"media_url"`
	LandingURL  string      `json:"landing_url"`
	Headline    string      `json:"headline"`
	Description string      `json:"description"`
	CTA         string      `json:"cta"`
	Status      string      `json:"status"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// CreativeInput is the create payload. Nil/zero values get sane
// defaults (headline/description/cta optional); everything else is
// validated strictly.
type CreativeInput struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Format      string `json:"format"`
	MediaURL    string `json:"media_url"`
	LandingURL  string `json:"landing_url"`
	Headline    string `json:"headline"`
	Description string `json:"description"`
	CTA         string `json:"cta"`
}

// CreativePatch is the update payload. Pointer fields distinguish
// "absent" from "set": nil = no change; non-nil = set (empty string
// allowed where the grammar permits it). Status transitions go
// through the explicit action routes, never PATCH.
type CreativePatch struct {
	Name        *string `json:"name"`
	MediaURL    *string `json:"media_url"`
	LandingURL  *string `json:"landing_url"`
	Headline    *string `json:"headline"`
	Description *string `json:"description"`
	CTA         *string `json:"cta"`
	Kind        *string `json:"kind"`
	Format      *string `json:"format"`
}

// CreativePage is a keyset-paginated creative listing (reuses the
// shared AdsCursor: (created_at, id)).
type CreativePage struct {
	Items      []*Creative `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// CreativeService is the registry CRUD over ad_creatives.
type CreativeService struct {
	pool *db.Pool
	camp *CampaignService
}

// NewCreativeService builds the registry service. It takes the
// campaign service (never a second pool) so ownership checks go
// through one code path.
func NewCreativeService(pool *db.Pool, camp *CampaignService) *CreativeService {
	return &CreativeService{pool: pool, camp: camp}
}

// isValidCreativeURL allows http(s) only. Everything else —
// javascript:, data:, protocol-relative — is rejected before it can
// be stored, let alone served.
func isValidCreativeURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// ValidateCreativeInput checks the create grammar. It returns
// ErrInvalidCreativeField naming the offending field so the client
// gets a precise 400.
func ValidateCreativeInput(in CreativeInput) error {
	if n := len([]rune(in.Name)); n < 1 || n > 128 {
		return fmt.Errorf("%w: name must be 1..128 characters", ErrInvalidCreativeField)
	}
	formats, ok := creativeFormats[in.Kind]
	if !ok {
		return fmt.Errorf("%w: kind must be image, video or native", ErrInvalidCreativeField)
	}
	known := false
	for _, f := range formats {
		if f == in.Format {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("%w: format %q is not valid for kind %q",
			ErrInvalidCreativeField, in.Format, in.Kind)
	}
	if !isValidCreativeURL(in.MediaURL) {
		return fmt.Errorf("%w: media_url must be an http(s) URL", ErrInvalidCreativeField)
	}
	if !isValidCreativeURL(in.LandingURL) {
		return fmt.Errorf("%w: landing_url must be an http(s) URL", ErrInvalidCreativeField)
	}
	if len([]rune(in.Headline)) > 80 {
		return fmt.Errorf("%w: headline must be at most 80 characters", ErrInvalidCreativeField)
	}
	if len([]rune(in.Description)) > 140 {
		return fmt.Errorf("%w: description must be at most 140 characters", ErrInvalidCreativeField)
	}
	if _, ok := creativeCTAs[in.CTA]; !ok {
		return fmt.Errorf("%w: cta %q is not a known call to action", ErrInvalidCreativeField, in.CTA)
	}
	return nil
}

// ValidateCreativePatch checks a partial update. kind/format may only
// change on drafts — ValidateCreativePatch takes the creative's
// current status to enforce that.
func ValidateCreativePatch(status string, in CreativePatch) error {
	if in.Name != nil {
		if n := len([]rune(*in.Name)); n < 1 || n > 128 {
			return fmt.Errorf("%w: name must be 1..128 characters", ErrInvalidCreativeField)
		}
	}
	if in.MediaURL != nil && !isValidCreativeURL(*in.MediaURL) {
		return fmt.Errorf("%w: media_url must be an http(s) URL", ErrInvalidCreativeField)
	}
	if in.LandingURL != nil && !isValidCreativeURL(*in.LandingURL) {
		return fmt.Errorf("%w: landing_url must be an http(s) URL", ErrInvalidCreativeField)
	}
	if in.Headline != nil && len([]rune(*in.Headline)) > 80 {
		return fmt.Errorf("%w: headline must be at most 80 characters", ErrInvalidCreativeField)
	}
	if in.Description != nil && len([]rune(*in.Description)) > 140 {
		return fmt.Errorf("%w: description must be at most 140 characters", ErrInvalidCreativeField)
	}
	if in.CTA != nil {
		if _, ok := creativeCTAs[*in.CTA]; !ok {
			return fmt.Errorf("%w: cta %q is not a known call to action", ErrInvalidCreativeField, *in.CTA)
		}
	}
	if in.Kind != nil || in.Format != nil {
		if status != CreativeDraft {
			return fmt.Errorf("%w: only drafts can change kind/format", ErrCreativeKindLocked)
		}
		// kind and format must change together on drafts: a kind change
		// with the old format kept would break the registry's
		// compatibility invariant (the migration CHECK would 500).
		kind := in.Kind
		format := in.Format
		if kind == nil {
			return fmt.Errorf("%w: changing format requires kind", ErrInvalidCreativeField)
		}
		if format == nil {
			return fmt.Errorf("%w: changing kind requires format", ErrInvalidCreativeField)
		}
		formats, ok := creativeFormats[*kind]
		if !ok {
			return fmt.Errorf("%w: kind must be image, video or native", ErrInvalidCreativeField)
		}
		if format != nil {
			known := false
			for _, f := range formats {
				if f == *format {
					known = true
					break
				}
			}
			if !known {
				return fmt.Errorf("%w: format %q is not valid for kind %q",
					ErrInvalidCreativeField, *format, *kind)
			}
		}
	}
	return nil
}

func scanCreative(row pgx.Row, c *Creative) error {
	return row.Scan(&c.ID, &c.CampaignID, &c.Name, &c.Kind, &c.Format,
		&c.MediaURL, &c.LandingURL, &c.Headline, &c.Description,
		&c.CTA, &c.Status, &c.CreatedAt, &c.UpdatedAt)
}

// resolveCreativeCampaign returns the campaign for a create/list
// request: it must exist, not be archived, and belong to the caller.
func (s *CreativeService) resolveCreativeCampaign(ctx context.Context, advertiser, campaignID pgtype.UUID) error {
	c, err := s.camp.Get(ctx, advertiser, campaignID)
	if err != nil {
		return err // ErrCampaignNotFound / ErrCampaignNotOwner as-is
	}
	_ = c
	return nil
}

// Create registers a draft creative under the caller's campaign.
func (s *CreativeService) Create(ctx context.Context, advertiser, campaignID pgtype.UUID, in CreativeInput) (*Creative, error) {
	if err := ValidateCreativeInput(in); err != nil {
		return nil, err
	}
	if err := s.resolveCreativeCampaign(ctx, advertiser, campaignID); err != nil {
		return nil, err
	}
	var c Creative
	row := s.pool.QueryRow(ctx, `
		INSERT INTO ad_creatives
			(campaign_id, name, kind, format, media_url, landing_url, headline, description, cta)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id, campaign_id, name, kind, format, media_url, landing_url,
		          headline, description, cta, status, created_at, updated_at`,
		campaignID, in.Name, in.Kind, in.Format, in.MediaURL,
		in.LandingURL, in.Headline, in.Description, in.CTA)
	if err := scanCreative(row, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Get returns one creative, owner-checked through its campaign.
// Archived creatives behave as 404 (registry tombstones).
func (s *CreativeService) Get(ctx context.Context, advertiser, id pgtype.UUID) (*Creative, error) {
	var c Creative
	row := s.pool.QueryRow(ctx, `
		SELECT id, campaign_id, name, kind, format, media_url, landing_url,
		       headline, description, cta, status, created_at, updated_at
		FROM ad_creatives WHERE id = $1 AND status <> 'archived'`, id)
	if err := scanCreative(row, &c); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCreativeNotFound
		}
		return nil, err
	}
	// Ownership goes through the campaign (single source of truth);
	// a creative can never outlive its campaign's advertiser binding.
	camp, err := s.camp.Get(ctx, advertiser, c.CampaignID)
	if err != nil {
		if errors.Is(err, ErrCampaignNotFound) || errors.Is(err, ErrCampaignNotOwner) {
			return nil, ErrCreativeNotOwner
		}
		return nil, err
	}
	_ = camp
	return &c, nil
}

// Update applies a presence-flag partial to a draft or active
// creative. status is never patchable (explicit action routes own it).
func (s *CreativeService) Update(ctx context.Context, advertiser, id pgtype.UUID, in CreativePatch) (*Creative, error) {
	c, err := s.Get(ctx, advertiser, id)
	if err != nil {
		return nil, err
	}
	if err := ValidateCreativePatch(c.Status, in); err != nil {
		return nil, err
	}
	// COALESCE keeps absent fields untouched; empty string is a legal
	// explicit value for headline/description/cta.
	var out Creative
	row := s.pool.QueryRow(ctx, `
		UPDATE ad_creatives SET
			name        = COALESCE($2, name),
			media_url   = COALESCE($3, media_url),
			landing_url = COALESCE($4, landing_url),
			headline    = COALESCE($5, headline),
			description = COALESCE($6, description),
			cta         = COALESCE($7, cta),
			kind        = COALESCE($8, kind),
			format      = COALESCE($9, format)
		WHERE id = $1
		RETURNING id, campaign_id, name, kind, format, media_url, landing_url,
		          headline, description, cta, status, created_at, updated_at`,
		id, in.Name, in.MediaURL, in.LandingURL, in.Headline,
		in.Description, in.CTA, in.Kind, in.Format)
	if err := scanCreative(row, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Transition moves a creative through the status matrix. Activation
// refuses archived campaigns (serving reads Eligible() anyway, but
// activating under a dead campaign is a client bug worth failing
// loudly on).
func (s *CreativeService) Transition(ctx context.Context, advertiser, id pgtype.UUID, to string) (*Creative, error) {
	c, err := s.Get(ctx, advertiser, id)
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, next := range CreativeTransitions[c.Status] {
		if next == to {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, ErrBadCreativeTransition
	}
	if to == CreativeActive {
		if err := s.resolveCreativeCampaign(ctx, advertiser, c.CampaignID); err != nil {
			return nil, err
		}
	}
	var out Creative
	row := s.pool.QueryRow(ctx, `
		UPDATE ad_creatives SET status = $2
		WHERE id = $1 AND status = $3
		RETURNING id, campaign_id, name, kind, format, media_url, landing_url,
		          headline, description, cta, status, created_at, updated_at`,
		id, to, c.Status)
	if err := scanCreative(row, &out); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Lost the race to a concurrent transition: the state
			// the client read is gone, and inventing an outcome
			// would be dishonest.
			return nil, ErrBadCreativeTransition
		}
		return nil, err
	}
	return &out, nil
}

// List returns the caller's creatives for one campaign, newest first,
// keyset-paginated on (created_at, id).
func (s *CreativeService) List(ctx context.Context, advertiser, campaignID pgtype.UUID, limit int, after *AdsCursor) (*CreativePage, error) {
	if err := s.resolveCreativeCampaign(ctx, advertiser, campaignID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxAdsPageLimit {
		return nil, ErrInvalidCreativeCursor
	}
	var rows pgx.Rows
	var err error
	if after == nil {
		rows, err = s.pool.Query(ctx, `
			SELECT id, campaign_id, name, kind, format, media_url, landing_url,
			       headline, description, cta, status, created_at, updated_at
			FROM ad_creatives
			WHERE campaign_id = $1 AND status <> 'archived'
			ORDER BY created_at DESC, id DESC LIMIT $2`, campaignID, limit+1)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT id, campaign_id, name, kind, format, media_url, landing_url,
			       headline, description, cta, status, created_at, updated_at
			FROM ad_creatives
			WHERE campaign_id = $1 AND status <> 'archived'
			  AND (created_at, id) < ($3, $4)
			ORDER BY created_at DESC, id DESC LIMIT $2`,
			campaignID, limit+1, after.CreatedAt, after.ID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &CreativePage{Items: []*Creative{}}
	for rows.Next() {
		var c Creative
		if err := scanCreative(rows, &c); err != nil {
			return nil, err
		}
		page.Items = append(page.Items, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Items) > limit {
		last := page.Items[limit-1]
		page.NextCursor = (&AdsCursor{CreatedAt: last.CreatedAt, ID: last.ID}).Encode()
		page.Items = page.Items[:limit]
	}
	return page, nil
}

// ActiveCreatives is the part-3b serving-path reader: active
// creatives on one campaign, newest first. It is defined here (not on
// the auction) because the registry owns what "active" means.
func (s *CreativeService) ActiveCreatives(ctx context.Context, campaignID pgtype.UUID) ([]*Creative, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, campaign_id, name, kind, format, media_url, landing_url,
		       headline, description, cta, status, created_at, updated_at
		FROM ad_creatives
		WHERE campaign_id = $1 AND status = 'active'
		ORDER BY created_at DESC, id DESC`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Creative
	for rows.Next() {
		var c Creative
		if err := scanCreative(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}
