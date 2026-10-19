// SPDX-License-Identifier: AGPL-3.0-or-later
// F3 (part 1): cart service — add / update / remove / clear / view.
//
// Design notes:
//   - One cart per user (carts.user_id is the PK). The cart row is created
//     lazily on the first add and deleted by Clear; items cascade.
//   - Prices are read LIVE from product_variants on every view: a cart
//     never shows a stale price. The immutable per-unit price snapshot
//     is taken at checkout when order_items are written (F3 part 2).
//   - Stock guard: every add/set runs in a transaction that upserts the
//     line and then checks the result against available stock
//     (inventory.quantity - inventory.reserved), rolling back on
//     overflow. Two racing tabs can never over-book.
//   - Listing state flips: a line whose variant or product is not
//     'active' cannot be added, but lines already in the cart survive
//     the flip (so reactivation restores the cart). Get filters to
//     buyable lines and reports the rest as UnavailableCount.
package cart

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

// maxLineQty caps a single cart line (accumulated quantity, not the
// request): a sanity ceiling far above any realistic order; the
// per-variant stock check is the real guard. Mirrored by the
// cart_items_qty_sane DB check (quantity <= 999) as a last resort.
const maxLineQty = 999

var (
	// ErrInvalidQuantity: qty < 1 on add, or qty < 0 / > 999 on set.
	ErrInvalidQuantity = errors.New("cart: invalid quantity")
	// ErrVariantNotFound: the variant id names no row.
	ErrVariantNotFound = errors.New("cart: variant not found")
	// ErrVariantUnavailable: variant or parent product is not 'active'
	// (draft or archived). Cannot be added; existing lines are
	// reported as unavailable, not deleted.
	ErrVariantUnavailable = errors.New("cart: variant not available")
	// ErrInsufficientStock: the resulting line quantity exceeds
	// available stock (quantity - reserved). Best-effort under
	// concurrency like the other velocity guards.
	ErrInsufficientStock = errors.New("cart: insufficient stock")
	// ErrItemNotFound: PATCH/GET of a line that is not in the cart.
	// (DELETE of an absent line is a 204 no-op — idempotent by
	// design, so a lost ack can be retried safely.)
	ErrItemNotFound = errors.New("cart: item not in cart")
)

// Line is one cart line: the variant reference plus live product data.
// PricePaise is read fresh at view time — never a stale snapshot.
type Line struct {
	VariantID  string    `json:"variant_id"`
	ProductID  string    `json:"product_id"`
	SellerID   string    `json:"seller_id"`
	Title      string    `json:"title"`
	SKU        string    `json:"sku"`
	PricePaise int64     `json:"price_paise"`
	MRPPaise   *int64    `json:"mrp_paise,omitempty"`
	Quantity   int       `json:"quantity"`
	LineTotal  int64     `json:"line_total_paise"`
	AddedAt    time.Time `json:"added_at"`
}

// Cart is the full view: buyable lines plus totals. UnavailableCount
// is the number of lines whose variant/product is not currently
// buyable — the client can explain those separately instead of
// silently dropping them.
type Cart struct {
	Items            []*Line `json:"items"`
	ItemCount        int     `json:"item_count"`
	SubtotalPaise    int64   `json:"subtotal_paise"`
	UnavailableCount int     `json:"unavailable_count"`
}

// Service owns the cart tables.
type Service struct{ pool *db.Pool }

// NewService wires the service to the shared pool.
func NewService(pool *db.Pool) *Service { return &Service{pool: pool} }

// variantStatus resolves a variant's buyability: both the variant and
// its parent product must be 'active'. Returns the available stock
// (quantity - reserved; a missing inventory row counts as zero).
func variantStatus(ctx context.Context, q dbQuerier, variantID pgtype.UUID) (buyable bool, available int, err error) {
	var vStatus, pStatus string
	var quantity, reserved int
	err = q.QueryRow(ctx, `
		SELECT v.status, p.status,
		       COALESCE(i.quantity, 0), COALESCE(i.reserved, 0)
		  FROM product_variants v
		  JOIN products p ON p.id = v.product_id
		  LEFT JOIN inventory i ON i.variant_id = v.id
		 WHERE v.id = $1`,
		variantID).Scan(&vStatus, &pStatus, &quantity, &reserved)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, 0, ErrVariantNotFound
		}
		return false, 0, err
	}
	avail := quantity - reserved
	if avail < 0 {
		avail = 0
	}
	return vStatus == "active" && pStatus == "active", avail, nil
}

// dbQuerier is the subset of pgx the status check needs, so the same
// logic runs on a plain pool or inside a transaction.
type dbQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AddItem adds qty units of the variant to the user's cart (accumulating
// with any existing line) and returns the resulting line. The upsert
// and the stock check run in one transaction: overflow rolls back.
func (s *Service) AddItem(ctx context.Context, userID, variantID pgtype.UUID, qty int) (*Line, error) {
	if qty < 1 || qty > maxLineQty {
		return nil, ErrInvalidQuantity
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO carts (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		return nil, err
	}
	buyable, _, err := variantStatus(ctx, tx, variantID)
	if err != nil {
		return nil, err
	}
	if !buyable {
		return nil, ErrVariantUnavailable
	}
	// The sanity cap applies to the ACCUMULATED line, not the request:
	// read the existing line first and refuse the add if the total would
	// exceed maxLineQty. Without this, an add of (say) 999 onto an
	// existing line of 2 trips the cart_items_qty_sane DB check
	// (quantity <= 999) on the upsert — a raw pgx error that maps to a
	// 500 instead of the 422 ErrInvalidQuantity below. The DB check
	// stays as a last-resort backstop.
	var existing int
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(
			(SELECT quantity FROM cart_items WHERE user_id = $1 AND variant_id = $2), 0)`,
		userID, variantID).Scan(&existing)
	if err != nil {
		return nil, err
	}
	if existing+qty > maxLineQty {
		return nil, ErrInvalidQuantity
	}
	var newQty int
	err = tx.QueryRow(ctx, `
		INSERT INTO cart_items (user_id, variant_id, quantity)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, variant_id)
		DO UPDATE SET quantity = cart_items.quantity + EXCLUDED.quantity
		RETURNING quantity`,
		userID, variantID, qty).Scan(&newQty)
	if err != nil {
		return nil, err
	}
	// The check runs AFTER the upsert inside the same tx, so the new
	// quantity is always compared against current availability; a
	// racing seller stock-down cannot slip between the two reads.
	_, available, err := variantStatus(ctx, tx, variantID)
	if err != nil {
		return nil, err
	}
	if newQty > available {
		return nil, ErrInsufficientStock
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.lineFor(ctx, userID, variantID)
}

// SetQuantity sets a line to exactly qty (0 removes the line). Setting
// an absent line is ErrItemNotFound; removing an absent line is the
// RemoveItem no-op. Overflow against available stock rolls back.
func (s *Service) SetQuantity(ctx context.Context, userID, variantID pgtype.UUID, qty int) (*Line, error) {
	if qty < 0 || qty > maxLineQty {
		return nil, ErrInvalidQuantity
	}
	if qty == 0 {
		return nil, s.RemoveItem(ctx, userID, variantID)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	buyable, _, err := variantStatus(ctx, tx, variantID)
	if err != nil {
		return nil, err
	}
	if !buyable {
		return nil, ErrVariantUnavailable
	}
	var newQty int
	err = tx.QueryRow(ctx,
		`UPDATE cart_items SET quantity = $3
		  WHERE user_id = $1 AND variant_id = $2
		  RETURNING quantity`,
		userID, variantID, qty).Scan(&newQty)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrItemNotFound
		}
		return nil, err
	}
	_, available, err := variantStatus(ctx, tx, variantID)
	if err != nil {
		return nil, err
	}
	if newQty > available {
		return nil, ErrInsufficientStock
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.lineFor(ctx, userID, variantID)
}

// RemoveItem deletes a line. Idempotent: removing an absent line (or
// from a nonexistent cart) succeeds silently so a lost ack can be
// retried without ever producing an error.
func (s *Service) RemoveItem(ctx context.Context, userID, variantID pgtype.UUID) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM cart_items WHERE user_id = $1 AND variant_id = $2`,
		userID, variantID)
	return err
}

// Clear empties the cart (the cart row itself is dropped; items
// cascade). Clearing an already-empty cart succeeds silently.
func (s *Service) Clear(ctx context.Context, userID pgtype.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM carts WHERE user_id = $1`, userID)
	return err
}

// Get returns the buyable lines (newest-added last) with live prices,
// plus the count of lines currently unavailable.
func (s *Service) Get(ctx context.Context, userID pgtype.UUID) (*Cart, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ci.variant_id, ci.quantity, ci.added_at,
		       v.product_id, v.sku, v.price_paise, v.mrp_paise,
		       p.title, p.seller_id
		  FROM cart_items ci
		  JOIN product_variants v ON v.id = ci.variant_id
		  JOIN products p ON p.id = v.product_id
		 WHERE ci.user_id = $1
		   AND v.status = 'active' AND p.status = 'active'
		 ORDER BY ci.added_at ASC, ci.variant_id ASC`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	c := &Cart{Items: []*Line{}}
	for rows.Next() {
		var ln Line
		var variantID, productID, sellerID pgtype.UUID
		if err := rows.Scan(&variantID, &ln.Quantity, &ln.AddedAt,
			&productID, &ln.SKU, &ln.PricePaise, &ln.MRPPaise,
			&ln.Title, &sellerID); err != nil {
			return nil, err
		}
		ln.VariantID = uuidString(variantID)
		ln.ProductID = uuidString(productID)
		ln.SellerID = uuidString(sellerID)
		ln.LineTotal = ln.PricePaise * int64(ln.Quantity)
		c.Items = append(c.Items, &ln)
		c.ItemCount += ln.Quantity
		c.SubtotalPaise += ln.LineTotal
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Lines that exist but are not currently buyable (archived
	// variant/product): counted, never silently dropped.
	var unavailable int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		  FROM cart_items ci
		  JOIN product_variants v ON v.id = ci.variant_id
		  JOIN products p ON p.id = v.product_id
		 WHERE ci.user_id = $1
		   AND NOT (v.status = 'active' AND p.status = 'active')`,
		userID).Scan(&unavailable); err != nil {
		return nil, err
	}
	c.UnavailableCount = unavailable
	return c, nil
}

// lineFor re-reads a single line after a write, so callers always see
// the committed state (live price included).
func (s *Service) lineFor(ctx context.Context, userID, variantID pgtype.UUID) (*Line, error) {
	var ln Line
	var variantIDOut, productID, sellerID pgtype.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT v.id, v.product_id, v.sku, v.price_paise, v.mrp_paise,
		       p.title, p.seller_id, ci.quantity, ci.added_at
		  FROM cart_items ci
		  JOIN product_variants v ON v.id = ci.variant_id
		  JOIN products p ON p.id = v.product_id
		 WHERE ci.user_id = $1 AND ci.variant_id = $2`,
		userID, variantID).Scan(
		&variantIDOut, &productID, &ln.SKU, &ln.PricePaise, &ln.MRPPaise,
		&ln.Title, &sellerID, &ln.Quantity, &ln.AddedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrItemNotFound
		}
		return nil, err
	}
	ln.VariantID = uuidString(variantIDOut)
	ln.ProductID = uuidString(productID)
	ln.SellerID = uuidString(sellerID)
	ln.LineTotal = ln.PricePaise * int64(ln.Quantity)
	return &ln, nil
}

// uuidString renders a pgtype.UUID in canonical form. Every UUID in
// this package comes from our own tables, so an invalid value is a
// programming error, not a client error.
func uuidString(id pgtype.UUID) string {
	if !id.Valid {
		panic("cart: invalid UUID from database")
	}
	return id.String()
}
