// SPDX-License-Identifier: AGPL-3.0-or-later
// F3 (part 1): cart service tests.
//
// Integration tests are gated on TEST_DATABASE_URL (same convention as
// the catalog tests): they run the full service against real PG16 and
// skip cleanly otherwise. The error-code mapping test is pure and
// always runs.
package cart

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/catalog"
	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping cart integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, dsn, 5, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// uniqueSubject mints a fresh OIDC subject per test so runs stay
// isolated against a shared test database.
func uniqueSubject(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("cart-test-%d-%s", time.Now().UnixNano(), t.Name())
}

func provisionUser(t *testing.T, ctx context.Context, pool *db.Pool, subject string) pgtype.UUID {
	t.Helper()
	u, err := users.Provision(ctx, pool, subject, "")
	if err != nil {
		t.Fatalf("users.Provision: %v", err)
	}
	var id pgtype.UUID
	if err := id.Scan(u.ID); err != nil {
		t.Fatalf("scan user id: %v", err)
	}
	return id
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(s); err != nil {
		t.Fatalf("bad uuid %q: %v", s, err)
	}
	return id
}

// sellableVariant builds the full seller → active product → active
// variant → stocked chain and returns the variant id. stock is the
// available quantity after the adjustment.
func sellableVariant(t *testing.T, ctx context.Context, pool *db.Pool, cat *catalog.Service, userID pgtype.UUID, stock int) pgtype.UUID {
	t.Helper()
	// Slugs allow only [a-z0-9-]: derive the unique suffix from the
	// nanosecond timestamp (digits only), never from the test name.
	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "cart-store-" + tag[len(tag)-8:]
	if _, err := cat.Register(ctx, userID, "Cart Test Store "+tag[len(tag)-6:], slug, ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	p, err := cat.CreateProduct(ctx, userID, "Cart Widget "+tag[len(tag)-6:], "A widget for cart tests.", "", "active")
	if err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	v, err := cat.AddVariant(ctx, userID, mustUUID(t, p.ID), "CSKU-"+tag[len(tag)-10:], 49900, nil, nil, "")
	if err != nil {
		t.Fatalf("AddVariant: %v", err)
	}
	if _, err := cat.AdjustInventory(ctx, userID, mustUUID(t, v.ID), stock); err != nil {
		t.Fatalf("AdjustInventory: %v", err)
	}
	return mustUUID(t, v.ID)
}

func TestCartAddAndGet(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cat := catalog.NewService(pool)
	svc := NewService(pool)

	buyer := provisionUser(t, ctx, pool, uniqueSubject(t))
	variantID := sellableVariant(t, ctx, pool, cat, provisionUser(t, ctx, pool, uniqueSubject(t)), 10)

	ln, err := svc.AddItem(ctx, buyer, variantID, 2)
	if err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	if ln.Quantity != 2 {
		t.Errorf("quantity = %d, want 2", ln.Quantity)
	}
	if ln.PricePaise != 49900 {
		t.Errorf("price = %d, want live price 49900", ln.PricePaise)
	}
	if ln.LineTotal != 99800 {
		t.Errorf("line total = %d, want 99800", ln.LineTotal)
	}
	if ln.Title == "" || ln.SKU == "" || ln.ProductID == "" || ln.SellerID == "" {
		t.Errorf("line missing joined product data: %+v", ln)
	}

	c, err := svc.Get(ctx, buyer)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(c.Items) != 1 || c.ItemCount != 2 || c.SubtotalPaise != 99800 {
		t.Errorf("cart = %+v, want 1 line / 2 units / 99800", c)
	}
	if c.UnavailableCount != 0 {
		t.Errorf("unavailable = %d, want 0", c.UnavailableCount)
	}
}

func TestCartAddAccumulates(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cat := catalog.NewService(pool)
	svc := NewService(pool)

	buyer := provisionUser(t, ctx, pool, uniqueSubject(t))
	variantID := sellableVariant(t, ctx, pool, cat, provisionUser(t, ctx, pool, uniqueSubject(t)), 10)

	if _, err := svc.AddItem(ctx, buyer, variantID, 2); err != nil {
		t.Fatalf("AddItem 1: %v", err)
	}
	ln, err := svc.AddItem(ctx, buyer, variantID, 3)
	if err != nil {
		t.Fatalf("AddItem 2: %v", err)
	}
	if ln.Quantity != 5 {
		t.Errorf("accumulated quantity = %d, want 5", ln.Quantity)
	}
}

func TestCartAddValidation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cat := catalog.NewService(pool)
	svc := NewService(pool)

	buyer := provisionUser(t, ctx, pool, uniqueSubject(t))
	sellerUser := provisionUser(t, ctx, pool, uniqueSubject(t))
	variantID := sellableVariant(t, ctx, pool, cat, sellerUser, 5)

	// Quantity grammar.
	if _, err := svc.AddItem(ctx, buyer, variantID, 0); !errors.Is(err, ErrInvalidQuantity) {
		t.Errorf("qty 0 = %v, want ErrInvalidQuantity", err)
	}
	if _, err := svc.AddItem(ctx, buyer, variantID, -2); !errors.Is(err, ErrInvalidQuantity) {
		t.Errorf("qty -2 = %v, want ErrInvalidQuantity", err)
	}
	if _, err := svc.AddItem(ctx, buyer, variantID, 1000); !errors.Is(err, ErrInvalidQuantity) {
		t.Errorf("qty 1000 = %v, want ErrInvalidQuantity", err)
	}

	// Unknown variant.
	var ghost pgtype.UUID
	if err := ghost.Scan("00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddItem(ctx, buyer, ghost, 1); !errors.Is(err, ErrVariantNotFound) {
		t.Errorf("ghost variant = %v, want ErrVariantNotFound", err)
	}

	// Stock cap: 5 available, adding 6 must fail and leave no line.
	if _, err := svc.AddItem(ctx, buyer, variantID, 6); !errors.Is(err, ErrInsufficientStock) {
		t.Errorf("over-stock add = %v, want ErrInsufficientStock", err)
	}
	c, err := svc.Get(ctx, buyer)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(c.Items) != 0 {
		t.Errorf("failed add left %d lines, want 0 (rollback)", len(c.Items))
	}

	// Accumulating past stock must also fail: 5 available, 4 + 2.
	if _, err := svc.AddItem(ctx, buyer, variantID, 4); err != nil {
		t.Fatalf("AddItem 4: %v", err)
	}
	if _, err := svc.AddItem(ctx, buyer, variantID, 2); !errors.Is(err, ErrInsufficientStock) {
		t.Errorf("accumulate past stock = %v, want ErrInsufficientStock", err)
	}
	// The original 4-unit line must be intact (rollback, not partial).
	ln, err := svc.lineFor(ctx, buyer, variantID)
	if err != nil {
		t.Fatalf("lineFor: %v", err)
	}
	if ln.Quantity != 4 {
		t.Errorf("line after failed accumulate = %d, want 4", ln.Quantity)
	}

	// Draft product: not buyable.
	draft, err := cat.CreateProduct(ctx, sellerUser, "Draft Gadget", "", "", "")
	if err != nil {
		t.Fatalf("CreateProduct draft: %v", err)
	}
	dv, err := cat.AddVariant(ctx, sellerUser, mustUUID(t, draft.ID), "DRAFTSKU", 1000, nil, nil, "")
	if err != nil {
		t.Fatalf("AddVariant draft: %v", err)
	}
	if _, err := svc.AddItem(ctx, buyer, mustUUID(t, dv.ID), 1); !errors.Is(err, ErrVariantUnavailable) {
		t.Errorf("draft product add = %v, want ErrVariantUnavailable", err)
	}

	// Archived variant: not buyable.
	if _, err := cat.ArchiveVariant(ctx, sellerUser, variantID); err != nil {
		t.Fatalf("ArchiveVariant: %v", err)
	}
	if _, err := svc.AddItem(ctx, buyer, variantID, 1); !errors.Is(err, ErrVariantUnavailable) {
		t.Errorf("archived variant add = %v, want ErrVariantUnavailable", err)
	}
}

// The sanity cap applies to the ACCUMULATED line: adding onto an
// existing line must refuse with ErrInvalidQuantity (422) instead of
// tripping the cart_items_qty_sane DB check (quantity <= 999), which
// used to surface as an unmapped 500 from the upsert.
func TestCartAddAccumulatedCap(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cat := catalog.NewService(pool)
	svc := NewService(pool)

	buyer := provisionUser(t, ctx, pool, uniqueSubject(t))
	// Stock far above the cap so the stock guard cannot fire first.
	variantID := sellableVariant(t, ctx, pool, cat, provisionUser(t, ctx, pool, uniqueSubject(t)), 2000)

	if _, err := svc.AddItem(ctx, buyer, variantID, 999); err != nil {
		t.Fatalf("AddItem 999: %v", err)
	}
	if _, err := svc.AddItem(ctx, buyer, variantID, 1); !errors.Is(err, ErrInvalidQuantity) {
		t.Errorf("accumulate past cap = %v, want ErrInvalidQuantity", err)
	}
	// The original 999-unit line must be intact (rollback, not partial).
	ln, err := svc.lineFor(ctx, buyer, variantID)
	if err != nil {
		t.Fatalf("lineFor: %v", err)
	}
	if ln.Quantity != 999 {
		t.Errorf("line after failed accumulate = %d, want 999", ln.Quantity)
	}
}

func TestCartSetQuantity(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cat := catalog.NewService(pool)
	svc := NewService(pool)

	buyer := provisionUser(t, ctx, pool, uniqueSubject(t))
	variantID := sellableVariant(t, ctx, pool, cat, provisionUser(t, ctx, pool, uniqueSubject(t)), 10)

	if _, err := svc.AddItem(ctx, buyer, variantID, 2); err != nil {
		t.Fatalf("AddItem: %v", err)
	}

	ln, err := svc.SetQuantity(ctx, buyer, variantID, 7)
	if err != nil {
		t.Fatalf("SetQuantity 7: %v", err)
	}
	if ln.Quantity != 7 || ln.LineTotal != 7*49900 {
		t.Errorf("set line = %+v, want qty 7 total %d", ln, 7*49900)
	}

	// Above stock must fail and leave the line intact.
	if _, err := svc.SetQuantity(ctx, buyer, variantID, 11); !errors.Is(err, ErrInsufficientStock) {
		t.Errorf("set past stock = %v, want ErrInsufficientStock", err)
	}
	ln, err = svc.lineFor(ctx, buyer, variantID)
	if err != nil {
		t.Fatalf("lineFor: %v", err)
	}
	if ln.Quantity != 7 {
		t.Errorf("line after failed set = %d, want 7", ln.Quantity)
	}

	// Setting a line for an existing-but-unadded variant is a 404 on
	// the line; setting on a nonexistent variant is a 404 on the
	// variant itself (more accurate than blaming the cart line).
	otherVariant := sellableVariant(t, ctx, pool, cat, provisionUser(t, ctx, pool, uniqueSubject(t)), 10)
	if _, err := svc.SetQuantity(ctx, buyer, otherVariant, 1); !errors.Is(err, ErrItemNotFound) {
		t.Errorf("set absent line = %v, want ErrItemNotFound", err)
	}
	var ghost pgtype.UUID
	if err := ghost.Scan("00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetQuantity(ctx, buyer, ghost, 1); !errors.Is(err, ErrVariantNotFound) {
		t.Errorf("set ghost variant = %v, want ErrVariantNotFound", err)
	}

	// qty 0 removes the line.
	if _, err := svc.SetQuantity(ctx, buyer, variantID, 0); err != nil {
		t.Fatalf("SetQuantity 0: %v", err)
	}
	c, err := svc.Get(ctx, buyer)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(c.Items) != 0 {
		t.Errorf("after set-0, lines = %d, want 0", len(c.Items))
	}
}

func TestCartRemoveAndClear(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cat := catalog.NewService(pool)
	svc := NewService(pool)

	buyer := provisionUser(t, ctx, pool, uniqueSubject(t))
	variantID := sellableVariant(t, ctx, pool, cat, provisionUser(t, ctx, pool, uniqueSubject(t)), 10)

	// Remove on an empty cart is a silent no-op (idempotent retry).
	if err := svc.RemoveItem(ctx, buyer, variantID); err != nil {
		t.Errorf("remove absent = %v, want nil", err)
	}

	if _, err := svc.AddItem(ctx, buyer, variantID, 3); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	if err := svc.RemoveItem(ctx, buyer, variantID); err != nil {
		t.Fatalf("RemoveItem: %v", err)
	}
	if err := svc.RemoveItem(ctx, buyer, variantID); err != nil {
		t.Errorf("second remove = %v, want nil (idempotent)", err)
	}

	if _, err := svc.AddItem(ctx, buyer, variantID, 1); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if err := svc.Clear(ctx, buyer); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	c, err := svc.Get(ctx, buyer)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(c.Items) != 0 || c.SubtotalPaise != 0 {
		t.Errorf("after clear: %+v, want empty cart", c)
	}
	// Clearing an empty cart also succeeds.
	if err := svc.Clear(ctx, buyer); err != nil {
		t.Errorf("clear empty = %v, want nil", err)
	}
}

func TestCartLivePriceAndUnavailableLines(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cat := catalog.NewService(pool)
	svc := NewService(pool)

	buyer := provisionUser(t, ctx, pool, uniqueSubject(t))
	sellerUser := provisionUser(t, ctx, pool, uniqueSubject(t))
	variantID := sellableVariant(t, ctx, pool, cat, sellerUser, 10)

	if _, err := svc.AddItem(ctx, buyer, variantID, 1); err != nil {
		t.Fatalf("AddItem: %v", err)
	}

	// Seller reprices; the cart view must show the NEW price (live
	// pricing — the cart never freezes a stale price).
	if _, err := cat.UpdateVariant(ctx, sellerUser, variantID,
		catalog.VariantPatch{PricePaise: func() *int64 { v := int64(59900); return &v }()}); err != nil {
		t.Fatalf("UpdateVariant price: %v", err)
	}
	c, err := svc.Get(ctx, buyer)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(c.Items) != 1 || c.Items[0].PricePaise != 59900 || c.SubtotalPaise != 59900 {
		t.Errorf("after reprice: %+v, want live price 59900", c.Items)
	}

	// Archiving hides the line from the view but keeps the row:
	// UnavailableCount reports it, reactivation restores it.
	if _, err := cat.ArchiveVariant(ctx, sellerUser, variantID); err != nil {
		t.Fatalf("ArchiveVariant: %v", err)
	}
	c, err = svc.Get(ctx, buyer)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(c.Items) != 0 || c.UnavailableCount != 1 {
		t.Errorf("after archive: items=%d unavailable=%d, want 0/1",
			len(c.Items), c.UnavailableCount)
	}
	if _, err := cat.UpdateVariant(ctx, sellerUser, variantID,
		catalog.VariantPatch{Status: func() *string { s := "active"; return &s }()}); err != nil {
		t.Fatalf("reactivate variant: %v", err)
	}
	c, err = svc.Get(ctx, buyer)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(c.Items) != 1 || c.UnavailableCount != 0 {
		t.Errorf("after reactivate: items=%d unavailable=%d, want 1/0",
			len(c.Items), c.UnavailableCount)
	}
}

func TestCartEmptyView(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	buyer := provisionUser(t, ctx, pool, uniqueSubject(t))

	c, err := svc.Get(ctx, buyer)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.Items == nil || len(c.Items) != 0 || c.SubtotalPaise != 0 || c.UnavailableCount != 0 {
		t.Errorf("empty cart view = %+v, want zeroed cart with non-nil items", c)
	}
}

// TestCartErrorCodes is pure: the handler error mapping must stay in
// sync with the service error set.
func TestCartErrorCodes(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{ErrInvalidQuantity, http.StatusUnprocessableEntity, "invalid_quantity"},
		{ErrVariantNotFound, http.StatusNotFound, "variant_not_found"},
		{ErrVariantUnavailable, http.StatusUnprocessableEntity, "variant_not_available"},
		{ErrInsufficientStock, http.StatusConflict, "insufficient_stock"},
		{ErrItemNotFound, http.StatusNotFound, "cart_item_not_found"},
		{errors.New("boom"), http.StatusInternalServerError, ""},
	}
	for _, tc := range cases {
		status, code := cartErrorCode(tc.err)
		if status != tc.status || code != tc.code {
			t.Errorf("cartErrorCode(%v) = (%d,%q), want (%d,%q)",
				tc.err, status, code, tc.status, tc.code)
		}
	}
}
