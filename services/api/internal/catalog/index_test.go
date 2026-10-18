// SPDX-License-Identifier: AGPL-3.0-or-later
// F2 (part 1): integration tests for the search document builder.
// Gated on TEST_DATABASE_URL like the other catalog tests; the hook
// no-op behavior is covered by the zero-value ProductHooks in the
// handler tests (nil AfterProductWrite must never panic).
package catalog

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestBuildProductDoc(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)

	userID := provisionUser(t, ctx, pool, uniqueSubject(t))
	if _, err := svc.Register(ctx, userID, "Search Store "+uniqueSubject(t)[:8], uniqueSlug(t, "f2-store"), ""); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Unknown product → ErrProductNotFound (the caller deletes the doc).
	var missing pgtype.UUID
	if err := missing.Scan("00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BuildProductDoc(ctx, missing); !errors.Is(err, ErrProductNotFound) {
		t.Errorf("BuildProductDoc(missing) = %v, want ErrProductNotFound", err)
	}

	p, err := svc.CreateProduct(ctx, userID, "Handloom Cotton Kurta", "Woven in Surat.", "mens-fashion", "active")
	if err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	pid := mustUUID(t, p.ID)

	doc, err := svc.BuildProductDoc(ctx, pid)
	if err != nil {
		t.Fatalf("BuildProductDoc: %v", err)
	}
	if doc.ID != p.ID || doc.Title != "Handloom Cotton Kurta" || doc.Description != "Woven in Surat." {
		t.Errorf("doc identity = %+v", doc)
	}
	if doc.SellerID == "" {
		t.Error("doc.SellerID empty")
	}
	if doc.CategorySlug != "mens-fashion" {
		t.Errorf("CategorySlug = %q, want mens-fashion", doc.CategorySlug)
	}
	if doc.Status != "active" {
		t.Errorf("Status = %q, want active", doc.Status)
	}
	if doc.MinPricePaise != nil {
		t.Errorf("MinPricePaise = %v with no variants, want nil", *doc.MinPricePaise)
	}
	if doc.UpdatedAtUnix <= 0 {
		t.Errorf("UpdatedAtUnix = %d, want positive", doc.UpdatedAtUnix)
	}

	// Price is the cheapest ACTIVE variant: an archived cheaper
	// variant must not win.
	if _, err := svc.AddVariant(ctx, userID, pid, "KURTA-M", 49900, nil, map[string]string{"size": "M"}, "active"); err != nil {
		t.Fatalf("AddVariant: %v", err)
	}
	cheap, err := svc.AddVariant(ctx, userID, pid, "KURTA-S", 39900, nil, map[string]string{"size": "S"}, "active")
	if err != nil {
		t.Fatalf("AddVariant: %v", err)
	}
	if _, err := svc.ArchiveVariant(ctx, userID, mustUUID(t, cheap.ID)); err != nil {
		t.Fatalf("ArchiveVariant: %v", err)
	}
	doc, err = svc.BuildProductDoc(ctx, pid)
	if err != nil {
		t.Fatalf("BuildProductDoc: %v", err)
	}
	if doc.MinPricePaise == nil || *doc.MinPricePaise != 49900 {
		t.Errorf("MinPricePaise = %v, want 49900 (archived 39900 excluded)", doc.MinPricePaise)
	}

	// Archived products stay buildable (the hook deletes them from the
	// index instead of upserting).
	if _, err := svc.ArchiveProduct(ctx, userID, pid); err != nil {
		t.Fatalf("ArchiveProduct: %v", err)
	}
	doc, err = svc.BuildProductDoc(ctx, pid)
	if err != nil {
		t.Fatalf("BuildProductDoc: %v", err)
	}
	if doc.Status != "archived" {
		t.Errorf("Status = %q, want archived", doc.Status)
	}

	// Cleared category → empty slug (omitempty keeps it out of the doc).
	empty := ""
	if _, err := svc.UpdateProduct(ctx, userID, pid, ProductPatch{CategorySlug: &empty}); err != nil {
		t.Fatalf("clear category: %v", err)
	}
	doc, err = svc.BuildProductDoc(ctx, pid)
	if err != nil {
		t.Fatalf("BuildProductDoc: %v", err)
	}
	if doc.CategorySlug != "" {
		t.Errorf("CategorySlug = %q, want empty after clear", doc.CategorySlug)
	}
}
