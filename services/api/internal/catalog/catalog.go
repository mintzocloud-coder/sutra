// SPDX-License-Identifier: AGPL-3.0-or-later
// Package catalog owns the Wave-3 commerce catalog: sellers, the
// curated category tree, and (in F1 part 2) products, variants and
// inventory.
//
// 0%-COMMISSION PRICE MODEL: sellers.commission_rate_bps defaults to 0
// — the marketplace takes no cut of the sale price (the Meesho model,
// strictly better than legacy 5-30% take-rates). The column is
// per-seller basis points so a future differential rate needs no schema
// change; the platform monetizes via ads and logistics, never by
// skimming the seller's price. Price lives on the VARIANT
// (price_paise), never on the product: SKUs in different sizes/colors
// legitimately differ in price. All money is INTEGER paise (bigint) —
// floats never touch money, so there is no rounding drift, ever.
//
// This file ships F1 part 1: seller onboarding + the category tree.
// Products/variants/inventory land in F1 part 2.
package catalog

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

// Field limits.
const (
	MinStoreNameRunes = 3
	MaxStoreNameRunes = 80 // DB CHECK char_length(store_name) <= 80
	MinSlugLen        = 3
	MaxSlugLen        = 30 // DB CHECK store_slug ~ '^[a-z0-9-]{3,30}$'
)

// slugRe is the storefront-handle grammar: lowercase, digits, hyphens.
// Go also rejects leading/trailing hyphens and double hyphens — slugs
// appear in public storefront URLs, so they must read cleanly.
var slugRe = regexp.MustCompile(`^[a-z0-9-]{3,30}$`)

// gstinRe is the 15-character GSTIN grammar (checksum letter positions
// simplified; the full checksum algorithm is a KYC-wave concern).
var gstinRe = regexp.MustCompile(`^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][A-Z0-9]Z[A-Z0-9]$`)

var (
	// ErrSellerExists is returned when the caller already owns a seller
	// account (one seller per user, Wave-1 rule).
	ErrSellerExists = errors.New("catalog: seller account already registered")
	// ErrSellerNotFound is returned for unknown seller ids.
	ErrSellerNotFound = errors.New("catalog: seller not found")
	// ErrInvalidStoreName is returned for blank/overlong store names.
	ErrInvalidStoreName = errors.New("catalog: store name must be 3-80 characters")
	// ErrInvalidSlug is returned for slugs outside the storefront grammar.
	ErrInvalidSlug = errors.New("catalog: store slug must be 3-30 chars of [a-z0-9-], no leading/trailing/double hyphens")
	// ErrSlugTaken is returned when another seller claimed the slug first.
	ErrSlugTaken = errors.New("catalog: store slug is already taken")
	// ErrInvalidGSTIN is returned for malformed GSTINs.
	ErrInvalidGSTIN = errors.New("catalog: GSTIN must be a 15-character GSTIN")
)

// Seller is the API representation of one seller account.
type Seller struct {
	ID                string    `json:"id"`
	StoreName         string    `json:"store_name"`
	StoreSlug         string    `json:"store_slug"`
	Status            string    `json:"status"`
	CommissionRateBps int       `json:"commission_rate_bps"`
	GSTIN             *string   `json:"gstin,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// CategoryNode is one node of the category tree.
type CategoryNode struct {
	ID       string          `json:"id"`
	Slug     string          `json:"slug"`
	Name     string          `json:"name"`
	ParentID *string         `json:"parent_id,omitempty"`
	Children []*CategoryNode `json:"children,omitempty"`
}

// Service is the catalog store.
type Service struct {
	pool *db.Pool
}

// NewService builds the catalog store.
func NewService(pool *db.Pool) *Service {
	return &Service{pool: pool}
}

// validateStoreName checks the 3-80 rune store-name rule (rune-counted
// so Indic-script names are measured fairly).
func validateStoreName(name string) error {
	if n := utf8.RuneCountInString(strings.TrimSpace(name)); n < MinStoreNameRunes || n > MaxStoreNameRunes {
		return ErrInvalidStoreName
	}
	return nil
}

// ValidateSlug checks the storefront-handle grammar.
func ValidateSlug(slug string) error {
	if !slugRe.MatchString(slug) ||
		strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") ||
		strings.Contains(slug, "--") {
		return ErrInvalidSlug
	}
	return nil
}

// ValidateGSTIN checks the optional GSTIN (empty = not provided yet;
// real KYC verification is a post-incorporation wave).
func ValidateGSTIN(gstin string) error {
	if gstin == "" {
		return nil
	}
	if !gstinRe.MatchString(gstin) {
		return ErrInvalidGSTIN
	}
	return nil
}

// Slugify derives a storefront handle from a store name: lowercase,
// runs of non-alphanumerics collapse to one hyphen, edges trimmed.
// It may return "" for names with no usable characters — callers fall
// back to a generated slug.
func Slugify(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevHyphen := true // trims a leading hyphen without a flag
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	slug := b.String()
	return strings.Trim(slug, "-")
}

// Register creates the caller's seller account. One seller per user:
// a second call for the same user is ErrSellerExists, not a second
// account. The slug is first-come: a racing claim on the same slug
// resolves in the UNIQUE constraint, loser gets ErrSlugTaken.
// GSTIN is optional at registration; the DB + Go shapes are the only
// checks — real KYC verification ships post-incorporation.
func (s *Service) Register(ctx context.Context, userID pgtype.UUID, storeName, slug, gstin string) (*Seller, error) {
	if err := validateStoreName(storeName); err != nil {
		return nil, err
	}
	if err := ValidateGSTIN(gstin); err != nil {
		return nil, err
	}
	if slug == "" {
		slug = Slugify(storeName)
		if len(slug) < MinSlugLen {
			// The name had no usable characters (e.g. all emoji):
			// fall back to a stable generated handle.
			uid := userID.String()
			if len(uid) < 8 { // cannot happen: ids come from our users table
				return nil, ErrInvalidSlug
			}
			slug = "store-" + uid[:8]
		}
	}
	if err := ValidateSlug(slug); err != nil {
		return nil, err
	}

	var seller Seller
	var id pgtype.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO sellers (user_id, store_name, store_slug, gstin)
		VALUES ($1, $2, $3, NULLIF($4, ''))
		RETURNING id, store_name, store_slug, status,
		          commission_rate_bps, gstin, created_at, updated_at`,
		userID, strings.TrimSpace(storeName), slug, strings.TrimSpace(gstin)).
		Scan(&id, &seller.StoreName, &seller.StoreSlug, &seller.Status,
			&seller.CommissionRateBps, &seller.GSTIN, &seller.CreatedAt, &seller.UpdatedAt)
	if err == nil {
		seller.ID = id.String()
		return &seller, nil
	}
	// Plain INSERT (no ON CONFLICT) so the constraint name tells us
	// WHICH race the caller lost: their own account, or the slug.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "sellers_user_id_key":
			return nil, ErrSellerExists
		case "sellers_store_slug_key":
			return nil, ErrSlugTaken
		}
	}
	return nil, err
}

// scanSeller maps one seller row into a Seller.
func scanSeller(row pgx.Row) (*Seller, error) {
	var (
		seller Seller
		id     pgtype.UUID
		gstin  pgtype.Text
	)
	if err := row.Scan(&id, &seller.StoreName, &seller.StoreSlug,
		&seller.Status, &seller.CommissionRateBps, &gstin,
		&seller.CreatedAt, &seller.UpdatedAt); err != nil {
		return nil, err
	}
	seller.ID = id.String()
	if gstin.Valid {
		g := gstin.String
		seller.GSTIN = &g
	}
	return &seller, nil
}

// Get returns one seller by id. Storefronts are public: any caller may
// read any seller's record (the OIDC subject never leaks — there is no
// user_id in the Seller JSON).
func (s *Service) Get(ctx context.Context, id pgtype.UUID) (*Seller, error) {
	seller, err := scanSeller(s.pool.QueryRow(ctx, `
		SELECT id, store_name, store_slug, status,
		       commission_rate_bps, gstin, created_at, updated_at
		FROM sellers WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSellerNotFound
	}
	return seller, err
}

// ListCategories returns the seeded category tree: roots first, each
// with its children recursively. Slugs are the stable API identifiers;
// names are display labels. Depth-0 roots order by name; children order
// by name under their parent.
func (s *Service) ListCategories(ctx context.Context) ([]*CategoryNode, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, slug, name, parent_id
		FROM categories
		ORDER BY depth ASC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodes := map[string]*CategoryNode{}
	var order []string // row order = depth asc, name asc: parents before children
	for rows.Next() {
		var (
			node     CategoryNode
			id       pgtype.UUID
			parentID pgtype.UUID
		)
		if err := rows.Scan(&id, &node.Slug, &node.Name, &parentID); err != nil {
			return nil, err
		}
		node.ID = id.String()
		if parentID.Valid {
			p := parentID.String()
			node.ParentID = &p
		}
		nodes[node.ID] = &node
		order = append(order, node.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var roots []*CategoryNode
	for _, id := range order {
		node := nodes[id]
		if node.ParentID == nil {
			roots = append(roots, node)
			continue
		}
		// Defensive: a dangling parent (should never happen — FK) is
		// surfaced as a root rather than dropped silently.
		parent, ok := nodes[*node.ParentID]
		if !ok {
			roots = append(roots, node)
			continue
		}
		parent.Children = append(parent.Children, node)
	}
	if roots == nil {
		roots = []*CategoryNode{}
	}
	return roots, nil
}
