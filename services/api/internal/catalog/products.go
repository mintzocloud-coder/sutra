// SPDX-License-Identifier: AGPL-3.0-or-later
// F1b (part 1): the sellable layer of the Wave-3 catalog — products,
// variants and inventory.
//
// OWNERSHIP MODEL: every product belongs to exactly one seller, and
// every mutation is owner-only. The service resolves the caller's
// seller id from their user id (SellerIDForUser) and refuses anything
// else with ErrNotOwner. Reads (Get/List/GetInventory) are PUBLIC —
// storefronts have no login wall. The OIDC subject never leaks: the
// Product/Variant JSON carries seller_id, never user_id.
//
// MONEY: price_paise and mrp_paise are INTEGER paise (bigint). Floats
// never touch money. Price lives on the VARIANT, never the product —
// SKUs in different sizes/colors legitimately differ in price.
// mrp_paise is the optional reference price; when set it must be >=
// price (DB CHECK backstop + Go validation).
//
// INVENTORY: quantity = stock on hand, reserved = held by pending
// (unconfirmed) orders, available = quantity - reserved. Sellers move
// quantity only via AdjustInventory (a signed delta, atomically
// guarded so stock never goes negative). The F6 order saga is the
// ONLY writer of reserved — this package refuses to touch it.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Product limits mirror the DB CHECKs in migration 0008.
const (
	MinTitleRunes    = 1
	MaxTitleRunes    = 200       // DB CHECK char_length(title) <= 200
	MaxDescRunes     = 5000      // DB CHECK char_length(description) <= 5000
	MaxSKULen        = 64        // DB CHECK char_length(sku) <= 64
	MaxPricePaise    = 100000000 // 1e8 paise = ₹10 lakh; DB CHECK backstop
	MaxAttrCount     = 16
	MaxAttrKeyRunes  = 32
	MaxAttrValRunes  = 128
	MaxListLimit     = 50
	DefaultListLimit = 20
)

// Valid statuses for products and variants.
var (
	validProductStatuses = map[string]bool{"draft": true, "active": true, "archived": true}
	validVariantStatuses = map[string]bool{"active": true, "archived": true}
)

var (
	// ErrNotSeller is returned when the caller has no seller account.
	ErrNotSeller = errors.New("catalog: caller has no seller account")
	// ErrNotOwner is returned when the caller does not own the product.
	ErrNotOwner = errors.New("catalog: product does not belong to the caller's seller account")
	// ErrProductNotFound is returned for unknown product ids.
	ErrProductNotFound = errors.New("catalog: product not found")
	// ErrVariantNotFound is returned for unknown variant ids.
	ErrVariantNotFound = errors.New("catalog: variant not found")
	// ErrCategoryNotFound is returned for unknown category slugs.
	ErrCategoryNotFound = errors.New("catalog: category not found")
	// ErrInvalidTitle is returned for blank/overlong product titles.
	ErrInvalidTitle = errors.New("catalog: title must be 1-200 characters")
	// ErrInvalidDescription is returned for overlong descriptions.
	ErrInvalidDescription = errors.New("catalog: description must be at most 5000 characters")
	// ErrInvalidStatus is returned for unknown statuses.
	ErrInvalidStatus = errors.New("catalog: invalid status")
	// ErrInvalidPrice is returned for non-positive or absurd prices.
	ErrInvalidPrice = errors.New("catalog: price_paise must be between 1 and 100000000 paise")
	// ErrInvalidMRP is returned for MRP below price or otherwise invalid.
	ErrInvalidMRP = errors.New("catalog: mrp_paise must be null or >= price_paise")
	// ErrSKUExists is returned when the product already has the SKU.
	ErrSKUExists = errors.New("catalog: sku already exists for this product")
	// ErrInvalidSKU is returned for blank/overlong SKUs.
	ErrInvalidSKU = errors.New("catalog: sku must be 1-64 characters")
	// ErrInvalidAttributes is returned for oversized attribute bags.
	ErrInvalidAttributes = errors.New("catalog: attributes must have at most 16 entries, keys 1-32 chars, values 1-128 chars")
	// ErrInsufficientStock is returned when an inventory delta would
	// drive quantity negative.
	ErrInsufficientStock = errors.New("catalog: inventory delta would drive stock below zero")
)

// Product is the API representation of a sellable listing. Price is NOT
// here — it lives on each variant (price_paise).
type Product struct {
	ID          string    `json:"id"`
	SellerID    string    `json:"seller_id"`
	CategoryID  *string   `json:"category_id,omitempty"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Variant is one purchasable SKU of a product.
type Variant struct {
	ID         string            `json:"id"`
	ProductID  string            `json:"product_id"`
	SKU        string            `json:"sku"`
	PricePaise int64             `json:"price_paise"`
	MrpPaise   *int64            `json:"mrp_paise,omitempty"`
	Attributes map[string]string `json:"attributes"`
	Status     string            `json:"status"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

// Inventory is the stock record for one variant.
type Inventory struct {
	VariantID string    `json:"variant_id"`
	Quantity  int       `json:"quantity"`
	Reserved  int       `json:"reserved"`
	Available int       `json:"available"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ProductPatch carries optional product edits. A nil pointer means
// "leave unchanged". CategorySlug "" (non-nil) clears the category.
type ProductPatch struct {
	Title        *string
	Description  *string
	CategorySlug *string
	Status       *string
}

// VariantPatch carries optional variant edits. SetMRP distinguishes
// "clear the MRP" (true + nil) from "leave it" (false).
type VariantPatch struct {
	SKU        *string
	PricePaise *int64
	SetMRP     bool
	MrpPaise   *int64
	Attributes *map[string]string
	Status     *string
}

// SellerIDForUser resolves the caller's seller account. Wave-1 rule:
// one seller per user. ErrNotSeller when they never registered.
func (s *Service) SellerIDForUser(ctx context.Context, userID pgtype.UUID) (pgtype.UUID, error) {
	var sellerID pgtype.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM sellers WHERE user_id = $1`, userID).
		Scan(&sellerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sellerID, ErrNotSeller
		}
		return sellerID, err
	}
	return sellerID, nil
}

// validateTitle checks the 1-200 rune title rule (rune-counted for
// Indic-script fairness, like store names).
func validateTitle(title string) error {
	if n := utf8.RuneCountInString(strings.TrimSpace(title)); n < MinTitleRunes || n > MaxTitleRunes {
		return ErrInvalidTitle
	}
	return nil
}

// validateDescription checks the 5000-rune description cap.
func validateDescription(desc string) error {
	if utf8.RuneCountInString(desc) > MaxDescRunes {
		return ErrInvalidDescription
	}
	return nil
}

// validatePrice checks the 1..1e8 paise price rule.
func validatePrice(paise int64) error {
	if paise < 1 || paise > MaxPricePaise {
		return ErrInvalidPrice
	}
	return nil
}

// validateMRP checks the MRP rule: nil = unset, else >= price and
// within the price band.
func validateMRP(mrp *int64, pricePaise int64) error {
	if mrp == nil {
		return nil
	}
	if err := validatePrice(*mrp); err != nil {
		return ErrInvalidMRP
	}
	if *mrp < pricePaise {
		return ErrInvalidMRP
	}
	return nil
}

// validateAttributes caps the attribute bag: at most 16 entries, small
// keys and values. Attributes drive variant pickers on the storefront,
// not free-form documents.
func validateAttributes(attrs map[string]string) error {
	if len(attrs) > MaxAttrCount {
		return ErrInvalidAttributes
	}
	for k, v := range attrs {
		if utf8.RuneCountInString(k) < 1 || utf8.RuneCountInString(k) > MaxAttrKeyRunes {
			return ErrInvalidAttributes
		}
		if utf8.RuneCountInString(v) < 1 || utf8.RuneCountInString(v) > MaxAttrValRunes {
			return ErrInvalidAttributes
		}
	}
	return nil
}

// encodeAttributes marshals the attribute bag to JSONB wire bytes. An
// empty/nil bag encodes as {} — never NULL — so storefronts always get
// an object, never a null surprise.
func encodeAttributes(attrs map[string]string) ([]byte, error) {
	if attrs == nil {
		attrs = map[string]string{}
	}
	return json.Marshal(attrs)
}

// scanProduct maps one product row into a Product.
func scanProduct(row pgx.Row) (*Product, error) {
	var (
		p          Product
		id         pgtype.UUID
		sellerID   pgtype.UUID
		categoryID pgtype.UUID
	)
	if err := row.Scan(&id, &sellerID, &categoryID, &p.Title,
		&p.Description, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.ID = id.String()
	p.SellerID = sellerID.String()
	if categoryID.Valid {
		c := categoryID.String()
		p.CategoryID = &c
	}
	return &p, nil
}

// scanVariant maps one variant row into a Variant.
func scanVariant(row pgx.Row) (*Variant, error) {
	var (
		v        Variant
		id       pgtype.UUID
		prodID   pgtype.UUID
		mrp      pgtype.Int8
		attrsRaw []byte
	)
	if err := row.Scan(&id, &prodID, &v.SKU, &v.PricePaise, &mrp,
		&attrsRaw, &v.Status, &v.CreatedAt, &v.UpdatedAt); err != nil {
		return nil, err
	}
	v.ID = id.String()
	v.ProductID = prodID.String()
	if mrp.Valid {
		m := mrp.Int64
		v.MrpPaise = &m
	}
	v.Attributes = map[string]string{}
	if len(attrsRaw) > 0 {
		if err := json.Unmarshal(attrsRaw, &v.Attributes); err != nil {
			return nil, fmt.Errorf("catalog: corrupt variant attributes: %w", err)
		}
	}
	return &v, nil
}

// categoryIDBySlug resolves a category slug (the stable API
// identifier) to its id. ErrCategoryNotFound on unknown slugs.
func (s *Service) categoryIDBySlug(ctx context.Context, slug string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM categories WHERE slug = $1`, slug).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return id, ErrCategoryNotFound
		}
		return id, err
	}
	return id, nil
}

// CreateProduct creates a draft listing owned by the caller's seller
// account. categorySlug may be "" (uncategorized); status defaults to
// "draft" — nothing is listed until the seller explicitly activates it.
func (s *Service) CreateProduct(ctx context.Context, userID pgtype.UUID, title, description, categorySlug, status string) (*Product, error) {
	if err := validateTitle(title); err != nil {
		return nil, err
	}
	if err := validateDescription(description); err != nil {
		return nil, err
	}
	if status == "" {
		status = "draft"
	}
	if !validProductStatuses[status] {
		return nil, ErrInvalidStatus
	}

	sellerID, err := s.SellerIDForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	var categoryID *pgtype.UUID
	if categorySlug != "" {
		id, err := s.categoryIDBySlug(ctx, categorySlug)
		if err != nil {
			return nil, err
		}
		categoryID = &id
	}

	return scanProduct(s.pool.QueryRow(ctx, `
		INSERT INTO products (seller_id, category_id, title, description, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, seller_id, category_id, title, description, status,
		          created_at, updated_at`,
		sellerID, categoryID, strings.TrimSpace(title), description, status))
}

// GetProduct returns one product. Storefronts are public — any caller
// may read any product (no owner check, no user_id in the JSON).
func (s *Service) GetProduct(ctx context.Context, id pgtype.UUID) (*Product, error) {
	p, err := scanProduct(s.pool.QueryRow(ctx, `
		SELECT id, seller_id, category_id, title, description, status,
		       created_at, updated_at
		FROM products WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProductNotFound
	}
	return p, err
}

// ListProducts returns one seller's products, newest first, with
// keyset paging over (created_at DESC, id DESC). cursor is the last
// product id of the previous page ("" = first page). limit clamps to
// 1..50. Public: storefront browsing needs no login.
func (s *Service) ListProducts(ctx context.Context, sellerID pgtype.UUID, limit int, cursor string) ([]*Product, error) {
	if limit < 1 || limit > MaxListLimit {
		limit = DefaultListLimit
	}
	var rows pgx.Rows
	var err error
	if cursor == "" {
		rows, err = s.pool.Query(ctx, `
			SELECT id, seller_id, category_id, title, description, status,
			       created_at, updated_at
			FROM products
			WHERE seller_id = $1
			ORDER BY created_at DESC, id DESC
			LIMIT $2`, sellerID, limit)
	} else {
		var cursorID pgtype.UUID
		if err := cursorID.Scan(cursor); err != nil {
			return nil, fmt.Errorf("catalog: invalid cursor: %w", err)
		}
		rows, err = s.pool.Query(ctx, `
			SELECT id, seller_id, category_id, title, description, status,
			       created_at, updated_at
			FROM products
			WHERE seller_id = $1
			  AND (created_at, id) < (
			      SELECT created_at, id FROM products WHERE id = $2)
			ORDER BY created_at DESC, id DESC
			LIMIT $3`, sellerID, cursorID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	products := []*Product{}
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		products = append(products, p)
	}
	return products, rows.Err()
}

// ownProduct returns the product only if the caller's seller account
// owns it. Missing product → ErrProductNotFound; wrong owner →
// ErrNotOwner (403, so a seller cannot enumerate another seller's
// ids by error shape).
func (s *Service) ownProduct(ctx context.Context, userID, productID pgtype.UUID) (*Product, error) {
	sellerID, err := s.SellerIDForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	p, err := s.GetProduct(ctx, productID)
	if err != nil {
		return nil, err
	}
	var owner pgtype.UUID
	if err := owner.Scan(p.SellerID); err != nil {
		return nil, err
	}
	if owner != sellerID {
		return nil, ErrNotOwner
	}
	return p, nil
}

// UpdateProduct applies a partial edit to the caller's own product.
// Only provided fields change. ErrNotOwner when the product belongs to
// another seller.
func (s *Service) UpdateProduct(ctx context.Context, userID, productID pgtype.UUID, patch ProductPatch) (*Product, error) {
	p, err := s.ownProduct(ctx, userID, productID)
	if err != nil {
		return nil, err
	}
	title, desc, status := p.Title, p.Description, p.Status
	var categoryID *pgtype.UUID
	if patch.Title != nil {
		if err := validateTitle(*patch.Title); err != nil {
			return nil, err
		}
		title = strings.TrimSpace(*patch.Title)
	}
	if patch.Description != nil {
		if err := validateDescription(*patch.Description); err != nil {
			return nil, err
		}
		desc = *patch.Description
	}
	if patch.Status != nil {
		if !validProductStatuses[*patch.Status] {
			return nil, ErrInvalidStatus
		}
		status = *patch.Status
	}
	if patch.CategorySlug != nil {
		if *patch.CategorySlug == "" {
			categoryID = nil // clear
		} else {
			id, err := s.categoryIDBySlug(ctx, *patch.CategorySlug)
			if err != nil {
				return nil, err
			}
			categoryID = &id
		}
	} else if p.CategoryID != nil {
		var id pgtype.UUID
		if err := id.Scan(*p.CategoryID); err != nil {
			return nil, err
		}
		categoryID = &id
	}

	return scanProduct(s.pool.QueryRow(ctx, `
		UPDATE products
		SET title = $2, description = $3, status = $4, category_id = $5
		WHERE id = $1
		RETURNING id, seller_id, category_id, title, description, status,
		          created_at, updated_at`,
		productID, title, desc, status, categoryID))
}

// ArchiveProduct moves the caller's own product to "archived" — it
// leaves the storefront and search index (F2 only indexes active).
func (s *Service) ArchiveProduct(ctx context.Context, userID, productID pgtype.UUID) (*Product, error) {
	archived := "archived"
	return s.UpdateProduct(ctx, userID, productID, ProductPatch{Status: &archived})
}

// ownVariant resolves a variant only if the caller's seller account
// owns its product. Missing → ErrVariantNotFound; wrong owner →
// ErrNotOwner.
func (s *Service) ownVariant(ctx context.Context, userID, variantID pgtype.UUID) (*Variant, error) {
	sellerID, err := s.SellerIDForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	v, err := s.GetVariant(ctx, variantID)
	if err != nil {
		return nil, err
	}
	var prodID pgtype.UUID
	if err := prodID.Scan(v.ProductID); err != nil {
		return nil, err
	}
	p, err := s.GetProduct(ctx, prodID)
	if err != nil {
		return nil, err
	}
	var owner pgtype.UUID
	if err := owner.Scan(p.SellerID); err != nil {
		return nil, err
	}
	if owner != sellerID {
		return nil, ErrNotOwner
	}
	return v, nil
}

// GetVariant returns one variant. Public, like the product read.
func (s *Service) GetVariant(ctx context.Context, id pgtype.UUID) (*Variant, error) {
	v, err := scanVariant(s.pool.QueryRow(ctx, `
		SELECT id, product_id, sku, price_paise, mrp_paise, attributes,
		       status, created_at, updated_at
		FROM product_variants WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrVariantNotFound
	}
	return v, err
}

// ListVariants returns all variants of one product, SKU order.
// Public: variant pickers live on the storefront.
func (s *Service) ListVariants(ctx context.Context, productID pgtype.UUID) ([]*Variant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, product_id, sku, price_paise, mrp_paise, attributes,
		       status, created_at, updated_at
		FROM product_variants
		WHERE product_id = $1
		ORDER BY sku ASC`, productID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	variants := []*Variant{}
	for rows.Next() {
		v, err := scanVariant(rows)
		if err != nil {
			return nil, err
		}
		variants = append(variants, v)
	}
	return variants, rows.Err()
}

// AddVariant adds one purchasable SKU to the caller's own product and
// creates its inventory row (quantity 0) in the same transaction — a
// variant without an inventory row is a data-model violation, so both
// writes commit or neither does.
func (s *Service) AddVariant(ctx context.Context, userID, productID pgtype.UUID, sku string, pricePaise int64, mrpPaise *int64, attrs map[string]string, status string) (*Variant, error) {
	if _, err := s.ownProduct(ctx, userID, productID); err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(sku)) < 1 || utf8.RuneCountInString(sku) > MaxSKULen {
		return nil, ErrInvalidSKU
	}
	if err := validatePrice(pricePaise); err != nil {
		return nil, err
	}
	if err := validateMRP(mrpPaise, pricePaise); err != nil {
		return nil, err
	}
	if err := validateAttributes(attrs); err != nil {
		return nil, err
	}
	if status == "" {
		status = "active"
	}
	if !validVariantStatuses[status] {
		return nil, ErrInvalidStatus
	}
	attrsJSON, err := encodeAttributes(attrs)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	v, err := scanVariant(tx.QueryRow(ctx, `
		INSERT INTO product_variants (product_id, sku, price_paise, mrp_paise, attributes, status)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6)
		RETURNING id, product_id, sku, price_paise, mrp_paise, attributes,
		          status, created_at, updated_at`,
		productID, strings.TrimSpace(sku), pricePaise, mrpPaise, attrsJSON, status))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			pgErr.ConstraintName == "product_variants_sku_unique" {
			return nil, ErrSKUExists
		}
		return nil, err
	}
	var variantID pgtype.UUID
	if err := variantID.Scan(v.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO inventory (variant_id) VALUES ($1)`, variantID); err != nil {
		return nil, err
	}
	// A variant write can change the listing's "from ₹X" price, so the
	// product's updated_at moves with it: the search document's sort
	// key stays truthful and the reindex hook rebuilds a current doc.
	if _, err := tx.Exec(ctx,
		`UPDATE products SET updated_at = now() WHERE id = $1`, productID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

// UpdateVariant applies a partial edit to the caller's own variant.
// SetMRP=false leaves mrp_paise alone; SetMRP=true with a nil
// MrpPaise clears it.
func (s *Service) UpdateVariant(ctx context.Context, userID, variantID pgtype.UUID, patch VariantPatch) (*Variant, error) {
	v, err := s.ownVariant(ctx, userID, variantID)
	if err != nil {
		return nil, err
	}
	sku, price, status := v.SKU, v.PricePaise, v.Status
	mrp := v.MrpPaise
	attrs := v.Attributes
	if patch.SKU != nil {
		if len(strings.TrimSpace(*patch.SKU)) < 1 || utf8.RuneCountInString(*patch.SKU) > MaxSKULen {
			return nil, ErrInvalidSKU
		}
		sku = strings.TrimSpace(*patch.SKU)
	}
	if patch.PricePaise != nil {
		if err := validatePrice(*patch.PricePaise); err != nil {
			return nil, err
		}
		price = *patch.PricePaise
	}
	if patch.SetMRP {
		mrp = patch.MrpPaise
	}
	if err := validateMRP(mrp, price); err != nil {
		return nil, err
	}
	if patch.Attributes != nil {
		if err := validateAttributes(*patch.Attributes); err != nil {
			return nil, err
		}
		attrs = *patch.Attributes
	}
	if patch.Status != nil {
		if !validVariantStatuses[*patch.Status] {
			return nil, ErrInvalidStatus
		}
		status = *patch.Status
	}
	attrsJSON, err := encodeAttributes(attrs)
	if err != nil {
		return nil, err
	}

	updated, err := scanVariant(s.pool.QueryRow(ctx, `
		UPDATE product_variants
		SET sku = $2, price_paise = $3, mrp_paise = $4,
		    attributes = $5::jsonb, status = $6
		WHERE id = $1
		RETURNING id, product_id, sku, price_paise, mrp_paise, attributes,
		          status, created_at, updated_at`,
		variantID, sku, price, mrp, attrsJSON, status))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			pgErr.ConstraintName == "product_variants_sku_unique" {
			return nil, ErrSKUExists
		}
		return nil, err
	}
	// Same reasoning as AddVariant: the listing changed, so its
	// updated_at (and therefore its search sort key) moves.
	// ArchiveVariant funnels through here, so it is covered too.
	if _, err := s.pool.Exec(ctx,
		`UPDATE products SET updated_at = now() WHERE id = $1`, v.ProductID); err != nil {
		return nil, err
	}
	return updated, nil
}

// ArchiveVariant moves the caller's own variant to "archived".
func (s *Service) ArchiveVariant(ctx context.Context, userID, variantID pgtype.UUID) (*Variant, error) {
	archived := "archived"
	return s.UpdateVariant(ctx, userID, variantID, VariantPatch{Status: &archived})
}

// GetInventory returns one variant's stock record. Public — buyers see
// availability on the storefront.
func (s *Service) GetInventory(ctx context.Context, variantID pgtype.UUID) (*Inventory, error) {
	var inv Inventory
	var id pgtype.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT variant_id, quantity, reserved, updated_at
		FROM inventory WHERE variant_id = $1`, variantID).
		Scan(&id, &inv.Quantity, &inv.Reserved, &inv.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrVariantNotFound
	}
	if err != nil {
		return nil, err
	}
	inv.VariantID = id.String()
	inv.Available = inv.Quantity - inv.Reserved
	return &inv, nil
}

// AdjustInventory moves one variant's quantity by a signed delta in a
// single atomic UPDATE: the WHERE clause refuses the write when stock
// would go negative, so concurrent adjusts can never oversell into
// negative stock. Owner-only. reserved is NOT touched here — the F6
// order saga owns reserved.
func (s *Service) AdjustInventory(ctx context.Context, userID, variantID pgtype.UUID, delta int) (*Inventory, error) {
	if _, err := s.ownVariant(ctx, userID, variantID); err != nil {
		return nil, err
	}
	var inv Inventory
	var id pgtype.UUID
	err := s.pool.QueryRow(ctx, `
		UPDATE inventory
		SET quantity = quantity + $2, updated_at = now()
		WHERE variant_id = $1 AND quantity + $2 >= 0
		RETURNING variant_id, quantity, reserved, updated_at`,
		variantID, delta).Scan(&id, &inv.Quantity, &inv.Reserved, &inv.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Either the variant never existed (impossible — ownVariant
		// just read it) or the delta would drive stock negative.
		return nil, ErrInsufficientStock
	}
	if err != nil {
		return nil, err
	}
	inv.VariantID = id.String()
	inv.Available = inv.Quantity - inv.Reserved
	return &inv, nil
}
