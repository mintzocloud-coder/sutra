// SPDX-License-Identifier: AGPL-3.0-or-later
// F2 (part 1): the search-indexing seam of the catalog.
//
// Product lifecycle events (create/update/archive, including
// variant writes — add/update/archive all rebuild the parent
// product's document) feed the Meilisearch product index through
// ProductHooks. The hooks fire AFTER the database write commits —
// indexing is best-effort and lives outside the request's error path,
// so a down search cluster can never turn a 201 into a 500.
package catalog

import (
	"context"
	"database/sql"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/search"
)

// ProductHooks carries post-write indexing callbacks. The zero value
// disables indexing (unit tests, or search unconfigured).
type ProductHooks struct {
	// AfterProductWrite fires after a successful product create,
	// update or archive. The closure (wired in main.go) builds the
	// search document and upserts or deletes it — in a detached
	// goroutine, logging failures, never failing the request.
	AfterProductWrite func(ctx context.Context, productID string)
}

// productWritten invokes the hook when set. Handlers call this right
// after the service method succeeds.
func (h ProductHooks) productWritten(ctx context.Context, productID string) {
	if h.AfterProductWrite != nil && productID != "" {
		h.AfterProductWrite(ctx, productID)
	}
}

// BuildProductDoc assembles the search document for one product: the
// product row, its category slug, the cheapest ACTIVE variant's
// price (the storefront's "from ₹X"), and search_seq — the numeric
// tie-breaker that makes /v1/search keyset paging total (Meilisearch
// range filters are numbers-only, so the uuid PK can't do it).
// ErrProductNotFound when the row is gone — the caller then deletes
// the document from the index.
func (s *Service) BuildProductDoc(ctx context.Context, productID pgtype.UUID) (*search.ProductDoc, error) {
	var (
		id       pgtype.UUID
		sellerID pgtype.UUID
		title    string
		desc     string
		status   string
		updated  pgtype.Timestamptz
		slug     sql.NullString
		seq      int64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT p.id, p.seller_id, p.title, p.description, p.status,
		       p.updated_at, c.slug, p.search_seq
		FROM products p
		LEFT JOIN categories c ON c.id = p.category_id
		WHERE p.id = $1`, productID).
		Scan(&id, &sellerID, &title, &desc, &status, &updated, &slug, &seq)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, ErrProductNotFound
		}
		return nil, err
	}
	var minPrice sql.NullInt64
	if err := s.pool.QueryRow(ctx, `
		SELECT MIN(price_paise) FROM product_variants
		WHERE product_id = $1 AND status = 'active'`, productID).Scan(&minPrice); err != nil {
		return nil, err
	}
	doc := &search.ProductDoc{
		ID:            id.String(),
		Title:         title,
		Description:   desc,
		SellerID:      sellerID.String(),
		Status:        status,
		UpdatedAtUnix: updated.Time.Unix(),
		SortSeq:       seq,
	}
	if slug.Valid {
		doc.CategorySlug = slug.String
	}
	if minPrice.Valid {
		m := minPrice.Int64
		doc.MinPricePaise = &m
	}
	return doc, nil
}
