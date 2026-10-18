// SPDX-License-Identifier: AGPL-3.0-or-later
package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

// Pure unit tests: slug grammar, slugify, GSTIN, store-name rules.

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Sharma Saree House", "sharma-saree-house"},
		{"  Mumbai   Electronics  ", "mumbai-electronics"},
		{"Café & Co. — 24/7", "caf-co-24-7"},
		{"सूरत Textiles", "textiles"},
		{"A", "a"},
		{"---", ""},
		{"", ""},
		{"UPPER CASE", "upper-case"},
		{"a--b", "a-b"},
	}
	for _, c := range cases {
		if got := Slugify(c.in); got != c.want {
			t.Errorf("Slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidateSlug(t *testing.T) {
	valid := []string{"abc", "my-store", "store-123", "a-b-c-1-2-3", "sharma-saree-house"}
	for _, s := range valid {
		if err := ValidateSlug(s); err != nil {
			t.Errorf("ValidateSlug(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{
		"", "ab", // too short
		"this-slug-is-way-too-long-for-the-grammar",
		"UPPER", "has space", "under_score", "dot.name",
		"-leading", "trailing-", "double--hyphen",
	}
	for _, s := range invalid {
		if err := ValidateSlug(s); err != ErrInvalidSlug {
			t.Errorf("ValidateSlug(%q) = %v, want ErrInvalidSlug", s, err)
		}
	}
}

func TestValidateGSTIN(t *testing.T) {
	if err := ValidateGSTIN(""); err != nil {
		t.Errorf("empty GSTIN must be allowed, got %v", err)
	}
	if err := ValidateGSTIN("24ABCDE1234F1Z5"); err != nil {
		t.Errorf("valid GSTIN rejected: %v", err)
	}
	for _, bad := range []string{"short", "24ABCDE1234F1Z", "24abcde1234f1z5", "24ABCDE1234F1Z55", "not-a-gstin!!"} {
		if err := ValidateGSTIN(bad); err != ErrInvalidGSTIN {
			t.Errorf("ValidateGSTIN(%q) = %v, want ErrInvalidGSTIN", bad, err)
		}
	}
}

func TestValidateStoreName(t *testing.T) {
	if err := validateStoreName("Sharma Sarees"); err != nil {
		t.Errorf("valid name rejected: %v", err)
	}
	// Rune-counted: 3 emoji are 3 runes (not 12 bytes) — fair for
	// Indic-script and emoji names.
	if err := validateStoreName("🛍️🛍️🛍️"); err != nil {
		t.Errorf("3-rune emoji name rejected: %v", err)
	}
	for _, bad := range []string{"", "  ", "ab", string(make([]byte, 0))} {
		if err := validateStoreName(bad); err != ErrInvalidStoreName {
			t.Errorf("validateStoreName(%q) = %v, want ErrInvalidStoreName", bad, err)
		}
	}
	long := ""
	for i := 0; i < 81; i++ {
		long += "a"
	}
	if err := validateStoreName(long); err != ErrInvalidStoreName {
		t.Errorf("81-char name = %v, want ErrInvalidStoreName", err)
	}
}

// Integration tests: run when TEST_DATABASE_URL is set; otherwise skip.

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping catalog integration test")
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

// uniqueSubject mints a fresh OIDC subject per test so runs are
// isolated even against a shared test database.
func uniqueSubject(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("catalog-test-%d-%s", time.Now().UnixNano(), t.Name())
}

// uniqueSlug mints a per-run slug inside the 30-char grammar so tests
// stay green against a reused test database (leftover rows from an
// earlier run must never collide with a new one).
func uniqueSlug(t *testing.T, base string) string {
	t.Helper()
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	slug := fmt.Sprintf("%s-%s", base, suffix)
	if len(slug) > MaxSlugLen {
		// Truncate the base on a hyphen boundary so the join never
		// produces a double hyphen.
		trunc := strings.TrimRight(base[:MaxSlugLen-len(suffix)-1], "-")
		slug = trunc + "-" + suffix
	}
	if err := ValidateSlug(slug); err != nil {
		t.Fatalf("uniqueSlug(%q) invalid: %v", slug, err)
	}
	return slug
}

// provisionUser creates a user row and returns its UUID.
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

func TestRegisterSeller(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	uid := provisionUser(t, ctx, pool, uniqueSubject(t))

	slug := uniqueSlug(t, "sharma-saree-house")
	seller, err := svc.Register(ctx, uid, "Sharma Saree House", slug, "24ABCDE1234F1Z5")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if seller.StoreName != "Sharma Saree House" || seller.StoreSlug != slug {
		t.Errorf("unexpected seller record: %+v", seller)
	}
	if seller.Status != "active" {
		t.Errorf("status = %q, want active", seller.Status)
	}
	if seller.CommissionRateBps != 0 {
		t.Errorf("commission_rate_bps = %d, want 0 (the 0%%-commission model)", seller.CommissionRateBps)
	}
	if seller.GSTIN == nil || *seller.GSTIN != "24ABCDE1234F1Z5" {
		t.Errorf("gstin = %v, want 24ABCDE1234F1Z5", seller.GSTIN)
	}

	// Second registration for the same user: one seller per user.
	_, err = svc.Register(ctx, uid, "Another Store", "another-store", "")
	if !errors.Is(err, ErrSellerExists) {
		t.Errorf("second register = %v, want ErrSellerExists", err)
	}

	// Slug race: another user claims the same slug → ErrSlugTaken.
	uid2 := provisionUser(t, ctx, pool, uniqueSubject(t))
	_, err = svc.Register(ctx, uid2, "Copycat Store", slug, "")
	if !errors.Is(err, ErrSlugTaken) {
		t.Errorf("slug collision = %v, want ErrSlugTaken", err)
	}

	// Validation matrix.
	for name, args := range map[string][3]string{
		"blank name":    {"  ", "slug-ok", ""},
		"overlong name": {string(make([]rune, 81)), "slug-ok", ""},
		"bad slug":      {"Valid Name", "UPPER", ""},
		"bad gstin":     {"Valid Name", "valid-slug-9", "nope"},
	} {
		uidN := provisionUser(t, ctx, pool, uniqueSubject(t))
		if _, err := svc.Register(ctx, uidN, args[0], args[1], args[2]); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}

	// Get: the registered seller is publicly readable.
	got, err := svc.Get(ctx, mustUUID(t, seller.ID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.StoreSlug != slug {
		t.Errorf("Get returned %+v", got)
	}

	// Get unknown id → ErrSellerNotFound.
	var missing pgtype.UUID
	if err := missing.Scan("00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, missing); !errors.Is(err, ErrSellerNotFound) {
		t.Errorf("Get(missing) = %v, want ErrSellerNotFound", err)
	}
}

func TestRegisterSlugDerived(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	uid := provisionUser(t, ctx, pool, uniqueSubject(t))

	// Empty slug → derived from the store name.
	name := fmt.Sprintf("Mumbai Electronics %d", time.Now().Unix()%100000)
	seller, err := svc.Register(ctx, uid, name, "", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if want := Slugify(name); seller.StoreSlug != want {
		t.Errorf("derived slug = %q, want %q", seller.StoreSlug, want)
	}

	// Name with no usable characters → generated fallback slug.
	uid2 := provisionUser(t, ctx, pool, uniqueSubject(t))
	seller2, err := svc.Register(ctx, uid2, "🛍️🛍️🛍️", "", "")
	if err != nil {
		t.Fatalf("Register emoji name: %v", err)
	}
	if err := ValidateSlug(seller2.StoreSlug); err != nil {
		t.Errorf("fallback slug %q invalid: %v", seller2.StoreSlug, err)
	}
}

func TestListCategories(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)

	tree, err := svc.ListCategories(ctx)
	if err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	if len(tree) == 0 {
		t.Fatal("category tree is empty: migration 0008 seed did not run")
	}

	// Find well-known seeded roots and assert the children shape.
	bySlug := map[string]*CategoryNode{}
	var walk func(nodes []*CategoryNode)
	walk = func(nodes []*CategoryNode) {
		for _, n := range nodes {
			bySlug[n.Slug] = n
			walk(n.Children)
		}
	}
	walk(tree)

	for _, want := range []string{"electronics", "fashion", "grocery"} {
		root, ok := bySlug[want]
		if !ok {
			t.Errorf("seeded category %q missing", want)
			continue
		}
		if root.ParentID != nil {
			t.Errorf("root %q has a parent", want)
		}
		if len(root.Children) == 0 {
			t.Errorf("root %q has no children", want)
		}
	}
	// Depth-1 child resolves under its parent: mobiles → electronics.
	mobiles, ok := bySlug["mobiles"]
	if !ok {
		t.Fatal("seeded category mobiles missing")
	}
	if mobiles.ParentID == nil || *mobiles.ParentID != bySlug["electronics"].ID {
		t.Errorf("mobiles parent = %v, want electronics", mobiles.ParentID)
	}
	// No duplicate slugs anywhere in the tree.
	seen := map[string]int{}
	var count func(nodes []*CategoryNode)
	count = func(nodes []*CategoryNode) {
		for _, n := range nodes {
			seen[n.Slug]++
			count(n.Children)
		}
	}
	count(tree)
	for slug, n := range seen {
		if n > 1 {
			t.Errorf("slug %q appears %d times in tree", slug, n)
		}
	}
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(s); err != nil {
		t.Fatalf("bad uuid %q: %v", s, err)
	}
	return id
}
