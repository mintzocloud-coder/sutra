// SPDX-License-Identifier: AGPL-3.0-or-later
// F1b (part 1): integration tests for product CRUD, variant ops and
// inventory. Gated on TEST_DATABASE_URL like the F1a integration tests;
// pure-validation tests run unconditionally.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Pure unit tests: product/variant/inventory validation rules.

func TestValidateProductFields(t *testing.T) {
	if err := validateTitle("Cotton Kurta"); err != nil {
		t.Errorf("valid title rejected: %v", err)
	}
	if err := validateTitle("कपास कुर्ता"); err != nil {
		t.Errorf("Devanagari title rejected: %v", err)
	}
	for _, bad := range []string{"", "   ", string(make([]rune, 201))} {
		if err := validateTitle(bad); !errors.Is(err, ErrInvalidTitle) {
			t.Errorf("validateTitle(%q) = %v, want ErrInvalidTitle", bad, err)
		}
	}
	if err := validateDescription(string(make([]rune, 5001))); !errors.Is(err, ErrInvalidDescription) {
		t.Errorf("overlong description = %v, want ErrInvalidDescription", err)
	}
	if err := validatePrice(1); err != nil {
		t.Errorf("1 paise rejected: %v", err)
	}
	for _, bad := range []int64{0, -5, MaxPricePaise + 1} {
		if err := validatePrice(bad); !errors.Is(err, ErrInvalidPrice) {
			t.Errorf("validatePrice(%d) = %v, want ErrInvalidPrice", bad, err)
		}
	}
	fifty := int64(5000)
	if err := validateMRP(&fifty, 4999); err != nil {
		t.Errorf("mrp >= price rejected: %v", err)
	}
	if err := validateMRP(&fifty, 5001); !errors.Is(err, ErrInvalidMRP) {
		t.Errorf("mrp < price = %v, want ErrInvalidMRP", err)
	}
	big := int64(MaxPricePaise + 1)
	if err := validateMRP(&big, 1); !errors.Is(err, ErrInvalidMRP) {
		t.Errorf("absurd mrp = %v, want ErrInvalidMRP", err)
	}
	bigAttrs := map[string]string{}
	for i := 0; i < 17; i++ {
		bigAttrs[fmt.Sprintf("k%d", i)] = "v"
	}
	if err := validateAttributes(bigAttrs); !errors.Is(err, ErrInvalidAttributes) {
		t.Errorf("17 attrs = %v, want ErrInvalidAttributes", err)
	}
}

// --- integration tests (TEST_DATABASE_URL-gated) ---

func TestProductLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)

	userID := provisionUser(t, ctx, pool, uniqueSubject(t))
	seller, err := svc.Register(ctx, userID, "Test Store "+uniqueSubject(t)[:8], uniqueSlug(t, "f1b-store"), "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	_ = seller

	// Non-seller cannot create a product.
	userNoSeller := provisionUser(t, ctx, pool, uniqueSubject(t))
	if _, err := svc.CreateProduct(ctx, userNoSeller, "Kurta", "", "", ""); !errors.Is(err, ErrNotSeller) {
		t.Errorf("create without seller = %v, want ErrNotSeller", err)
	}

	// Create with a category slug; default status is draft.
	title := fmt.Sprintf("Cotton Kurta %d", time.Now().UnixNano()%100000)
	p, err := svc.CreateProduct(ctx, userID, title, "Handloom cotton kurta.", "mens-fashion", "")
	if err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	if p.Status != "draft" {
		t.Errorf("status = %q, want draft", p.Status)
	}
	if p.CategoryID == nil {
		t.Error("category_id = nil, want the mens-fashion id")
	}

	// Unknown category slug → ErrCategoryNotFound.
	if _, err := svc.CreateProduct(ctx, userID, "X", "", "no-such-category", ""); !errors.Is(err, ErrCategoryNotFound) {
		t.Errorf("bad category = %v, want ErrCategoryNotFound", err)
	}
	// Bad title → ErrInvalidTitle; bad status → ErrInvalidStatus.
	if _, err := svc.CreateProduct(ctx, userID, "  ", "", "", ""); !errors.Is(err, ErrInvalidTitle) {
		t.Errorf("blank title = %v, want ErrInvalidTitle", err)
	}
	if _, err := svc.CreateProduct(ctx, userID, "Valid", "", "", "bogus"); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("bad status = %v, want ErrInvalidStatus", err)
	}

	// Get: public read.
	got, err := svc.GetProduct(ctx, mustUUID(t, p.ID))
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if got.Title != title {
		t.Errorf("title = %q, want %q", got.Title, title)
	}
	var missing pgtype.UUID
	if err := missing.Scan("00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetProduct(ctx, missing); !errors.Is(err, ErrProductNotFound) {
		t.Errorf("GetProduct(missing) = %v, want ErrProductNotFound", err)
	}

	// Update: activate + retitle.
	active := "active"
	newTitle := title + " v2"
	p2, err := svc.UpdateProduct(ctx, userID, mustUUID(t, p.ID), ProductPatch{Title: &newTitle, Status: &active})
	if err != nil {
		t.Fatalf("UpdateProduct: %v", err)
	}
	if p2.Title != newTitle || p2.Status != "active" {
		t.Errorf("updated = %+v", p2)
	}
	// Clear the category.
	empty := ""
	p3, err := svc.UpdateProduct(ctx, userID, mustUUID(t, p.ID), ProductPatch{CategorySlug: &empty})
	if err != nil {
		t.Fatalf("clear category: %v", err)
	}
	if p3.CategoryID != nil {
		t.Errorf("category not cleared: %v", p3.CategoryID)
	}
	// Bad status in patch.
	bogus := "bogus"
	if _, err := svc.UpdateProduct(ctx, userID, mustUUID(t, p.ID), ProductPatch{Status: &bogus}); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("bad status patch = %v, want ErrInvalidStatus", err)
	}

	// Another seller cannot touch it → ErrNotOwner.
	user2 := provisionUser(t, ctx, pool, uniqueSubject(t))
	if _, err := svc.Register(ctx, user2, "Rival Store", uniqueSlug(t, "f1b-rival"), ""); err != nil {
		t.Fatalf("Register rival: %v", err)
	}
	if _, err := svc.UpdateProduct(ctx, user2, mustUUID(t, p.ID), ProductPatch{Title: &newTitle}); !errors.Is(err, ErrNotOwner) {
		t.Errorf("cross-owner update = %v, want ErrNotOwner", err)
	}
	if _, err := svc.ArchiveProduct(ctx, user2, mustUUID(t, p.ID)); !errors.Is(err, ErrNotOwner) {
		t.Errorf("cross-owner archive = %v, want ErrNotOwner", err)
	}

	// Archive: owner only.
	arch, err := svc.ArchiveProduct(ctx, userID, mustUUID(t, p.ID))
	if err != nil {
		t.Fatalf("ArchiveProduct: %v", err)
	}
	if arch.Status != "archived" {
		t.Errorf("status = %q, want archived", arch.Status)
	}
}

func TestListProductsKeyset(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	userID := provisionUser(t, ctx, pool, uniqueSubject(t))
	seller, err := svc.Register(ctx, userID, "List Store", uniqueSlug(t, "f1b-list"), "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	sellerID := mustUUID(t, seller.ID)

	// Five products, created in order.
	for i := 0; i < 5; i++ {
		time.Sleep(2 * time.Millisecond) // distinct created_at for keyset
		if _, err := svc.CreateProduct(ctx, userID, fmt.Sprintf("List item %d %d", i, time.Now().UnixNano()%1000000), "", "", ""); err != nil {
			t.Fatalf("CreateProduct %d: %v", i, err)
		}
	}
	page1, err := svc.ListProducts(ctx, sellerID, 3, "")
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	if len(page1) != 3 {
		t.Fatalf("page1 len = %d, want 3", len(page1))
	}
	// Newest first.
	if !(page1[0].CreatedAt.After(page1[1].CreatedAt) || page1[0].CreatedAt.Equal(page1[1].CreatedAt)) {
		t.Error("page1 not newest-first")
	}
	page2, err := svc.ListProducts(ctx, sellerID, 3, page1[2].ID)
	if err != nil {
		t.Fatalf("ListProducts page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2))
	}
	// No overlap between pages.
	for _, a := range page1 {
		for _, b := range page2 {
			if a.ID == b.ID {
				t.Errorf("product %s on both pages", a.ID)
			}
		}
	}
	// Limit clamps to the sane default instead of erroring.
	all, err := svc.ListProducts(ctx, sellerID, 5000, "")
	if err != nil {
		t.Fatalf("ListProducts clamped: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("clamped list len = %d, want 5", len(all))
	}
}

func TestVariantAndInventory(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	userID := provisionUser(t, ctx, pool, uniqueSubject(t))
	if _, err := svc.Register(ctx, userID, "Variant Store", uniqueSlug(t, "f1b-variant"), ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	p, err := svc.CreateProduct(ctx, userID, "Test Kurta", "", "", "")
	if err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	prodID := mustUUID(t, p.ID)

	// Add variant: price in paise, MRP, attributes.
	mrp := int64(299900)
	v, err := svc.AddVariant(ctx, userID, prodID, "KURTA-M-RED", 199900, &mrp,
		map[string]string{"size": "M", "color": "red"}, "")
	if err != nil {
		t.Fatalf("AddVariant: %v", err)
	}
	if v.PricePaise != 199900 || v.MrpPaise == nil || *v.MrpPaise != 299900 {
		t.Errorf("variant money = %+v", v)
	}
	if v.Attributes["size"] != "M" || v.Attributes["color"] != "red" {
		t.Errorf("attributes = %v", v.Attributes)
	}
	if v.Status != "active" {
		t.Errorf("status = %q, want active", v.Status)
	}

	// Inventory row exists with 0 stock, available 0.
	inv, err := svc.GetInventory(ctx, mustUUID(t, v.ID))
	if err != nil {
		t.Fatalf("GetInventory: %v", err)
	}
	if inv.Quantity != 0 || inv.Reserved != 0 || inv.Available != 0 {
		t.Errorf("fresh inventory = %+v, want all zero", inv)
	}

	// SKU is unique per product.
	if _, err := svc.AddVariant(ctx, userID, prodID, "KURTA-M-RED", 100000, nil, nil, ""); !errors.Is(err, ErrSKUExists) {
		t.Errorf("duplicate sku = %v, want ErrSKUExists", err)
	}
	// MRP below price rejected; zero price rejected; oversized attrs rejected.
	low := int64(100)
	if _, err := svc.AddVariant(ctx, userID, prodID, "KURTA-L", 200000, &low, nil, ""); !errors.Is(err, ErrInvalidMRP) {
		t.Errorf("mrp<price = %v, want ErrInvalidMRP", err)
	}
	if _, err := svc.AddVariant(ctx, userID, prodID, "KURTA-XL", 0, nil, nil, ""); !errors.Is(err, ErrInvalidPrice) {
		t.Errorf("zero price = %v, want ErrInvalidPrice", err)
	}

	// Stock in: +50.
	inv, err = svc.AdjustInventory(ctx, userID, mustUUID(t, v.ID), 50)
	if err != nil {
		t.Fatalf("AdjustInventory +50: %v", err)
	}
	if inv.Quantity != 50 || inv.Available != 50 {
		t.Errorf("after +50: %+v", inv)
	}
	// Stock out: -20 → 30.
	inv, err = svc.AdjustInventory(ctx, userID, mustUUID(t, v.ID), -20)
	if err != nil {
		t.Fatalf("AdjustInventory -20: %v", err)
	}
	if inv.Quantity != 30 {
		t.Errorf("after -20: quantity = %d, want 30", inv.Quantity)
	}
	// Cannot drive stock negative.
	if _, err := svc.AdjustInventory(ctx, userID, mustUUID(t, v.ID), -31); !errors.Is(err, ErrInsufficientStock) {
		t.Errorf("overdraw = %v, want ErrInsufficientStock", err)
	}

	// Variant price update; MRP clear via SetMRP.
	newPrice := int64(179900)
	upd, err := svc.UpdateVariant(ctx, userID, mustUUID(t, v.ID),
		VariantPatch{PricePaise: &newPrice, SetMRP: true})
	if err != nil {
		t.Fatalf("UpdateVariant: %v", err)
	}
	if upd.PricePaise != 179900 || upd.MrpPaise != nil {
		t.Errorf("updated variant = %+v", upd)
	}

	// Cross-owner writes refused.
	user2 := provisionUser(t, ctx, pool, uniqueSubject(t))
	if _, err := svc.Register(ctx, user2, "Rival 2", uniqueSlug(t, "f1b-rival2"), ""); err != nil {
		t.Fatalf("Register rival: %v", err)
	}
	if _, err := svc.AddVariant(ctx, user2, prodID, "X", 100, nil, nil, ""); !errors.Is(err, ErrNotOwner) {
		t.Errorf("cross-owner add variant = %v, want ErrNotOwner", err)
	}
	if _, err := svc.AdjustInventory(ctx, user2, mustUUID(t, v.ID), 5); !errors.Is(err, ErrNotOwner) {
		t.Errorf("cross-owner adjust = %v, want ErrNotOwner", err)
	}

	// List variants of the product.
	vs, err := svc.ListVariants(ctx, prodID)
	if err != nil {
		t.Fatalf("ListVariants: %v", err)
	}
	if len(vs) != 1 || vs[0].SKU != "KURTA-M-RED" {
		t.Errorf("variants = %+v", vs)
	}

	// Archive variant.
	arch, err := svc.ArchiveVariant(ctx, userID, mustUUID(t, v.ID))
	if err != nil {
		t.Fatalf("ArchiveVariant: %v", err)
	}
	if arch.Status != "archived" {
		t.Errorf("status = %q, want archived", arch.Status)
	}
}
