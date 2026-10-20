// SPDX-License-Identifier: AGPL-3.0-or-later
// Tests for the creative registry (H3 part 3a): validation matrices,
// transition matrix, and DB-backed service tests (gated on
// TEST_DATABASE_URL, house convention).
package ads

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func strptr(s string) *string { return &s }

func validCreativeInput() CreativeInput {
	return CreativeInput{
		Name:        "Diwali Hero",
		Kind:        CreativeKindImage,
		Format:      "image_1200x628",
		MediaURL:    "https://cdn.example.com/hero.jpg",
		LandingURL:  "https://shop.example.com/kurta",
		Headline:    "Festive kurtas from ₹999",
		Description: "Handloom festive wear, free shipping.",
		CTA:         "shop_now",
	}
}

func TestValidateCreativeInput(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*CreativeInput)
		wantErr bool
	}{
		{"valid", func(*CreativeInput) {}, false},
		{"empty name", func(i *CreativeInput) { i.Name = "" }, true},
		{"name 129 runes", func(i *CreativeInput) {
			i.Name = string(make([]rune, 129))
		}, true},
		{"emoji name counts as runes", func(i *CreativeInput) {
			i.Name = string(make([]rune, 128)) + "🪔"
		}, true},
		{"unknown kind", func(i *CreativeInput) { i.Kind = "hologram" }, true},
		{"image format on video kind", func(i *CreativeInput) {
			i.Kind = CreativeKindVideo
		}, true},
		{"native format on image kind", func(i *CreativeInput) {
			i.Format = "native"
		}, true},
		{"native valid", func(i *CreativeInput) {
			i.Kind, i.Format = CreativeKindNative, "native"
		}, false},
		{"javascript media_url", func(i *CreativeInput) {
			i.MediaURL = "javascript:alert(1)"
		}, true},
		{"data landing_url", func(i *CreativeInput) {
			i.LandingURL = "data:text/html,<h1>x</h1>"
		}, true},
		{"ftp media_url", func(i *CreativeInput) { i.MediaURL = "ftp://x/y" }, true},
		{"protocol-relative landing_url", func(i *CreativeInput) {
			i.LandingURL = "//evil.example/x"
		}, true},
		{"headline 81 runes", func(i *CreativeInput) {
			i.Headline = string(make([]rune, 81))
		}, true},
		{"headline exactly 80", func(i *CreativeInput) {
			i.Headline = string(make([]rune, 80))
		}, false},
		{"description 141 runes", func(i *CreativeInput) {
			i.Description = string(make([]rune, 141))
		}, true},
		{"unknown cta", func(i *CreativeInput) { i.CTA = "buy_buy_buy" }, true},
		{"empty cta ok", func(i *CreativeInput) { i.CTA = "" }, false},
		{"empty headline ok", func(i *CreativeInput) { i.Headline = "" }, false},
	}
	for _, tc := range cases {
		in := validCreativeInput()
		tc.mutate(&in)
		err := ValidateCreativeInput(in)
		if tc.wantErr && err == nil {
			t.Fatalf("%s: want error, got nil", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("%s: want nil, got %v", tc.name, err)
		}
		if tc.wantErr && !errors.Is(err, ErrInvalidCreativeField) {
			t.Fatalf("%s: err = %v, want ErrInvalidCreativeField", tc.name, err)
		}
	}
}

func TestCreativeTransitionMatrix(t *testing.T) {
	matrix := map[string]map[string]bool{
		CreativeDraft:    {CreativeActive: true, CreativeArchived: true},
		CreativeActive:   {CreativeArchived: true},
		CreativeArchived: {},
	}
	for from, tos := range CreativeTransitions {
		for _, to := range []string{CreativeDraft, CreativeActive, CreativeArchived} {
			got := false
			for _, allowed := range tos {
				if allowed == to {
					got = true
				}
			}
			if want := matrix[from][to]; got != want {
				t.Fatalf("transition %s→%s = %v, want %v", from, to, got, want)
			}
		}
	}
	// Draft → draft and active → active are not edges (no self-loops).
	for _, s := range []string{CreativeDraft, CreativeActive} {
		for _, allowed := range CreativeTransitions[s] {
			if allowed == s {
				t.Fatalf("self-loop on %s", s)
			}
		}
	}
}

func TestValidateCreativePatch(t *testing.T) {
	// kind/format change on an active creative is locked.
	if err := ValidateCreativePatch(CreativeActive, CreativePatch{
		Kind: strptr(CreativeKindVideo), Format: strptr("video_1920x1080"),
	}); !errors.Is(err, ErrCreativeKindLocked) {
		t.Fatalf("active kind change err = %v, want ErrCreativeKindLocked", err)
	}
	// On a draft, kind without format is rejected.
	if err := ValidateCreativePatch(CreativeDraft, CreativePatch{
		Kind: strptr(CreativeKindVideo),
	}); !errors.Is(err, ErrInvalidCreativeField) {
		t.Fatalf("kind-only err = %v, want ErrInvalidCreativeField", err)
	}
	// On a draft, incompatible kind+format is rejected.
	if err := ValidateCreativePatch(CreativeDraft, CreativePatch{
		Kind: strptr(CreativeKindVideo), Format: strptr("image_1080x1080"),
	}); !errors.Is(err, ErrInvalidCreativeField) {
		t.Fatalf("mismatched pair err = %v, want ErrInvalidCreativeField", err)
	}
	// On a draft, compatible kind+format is accepted.
	if err := ValidateCreativePatch(CreativeDraft, CreativePatch{
		Kind: strptr(CreativeKindVideo), Format: strptr("video_1920x1080"),
	}); err != nil {
		t.Fatalf("compatible pair err = %v, want nil", err)
	}
	// javascript: URL in a patch is rejected.
	if err := ValidateCreativePatch(CreativeDraft, CreativePatch{
		MediaURL: strptr("javascript:alert(1)"),
	}); !errors.Is(err, ErrInvalidCreativeField) {
		t.Fatalf("patch url err = %v, want ErrInvalidCreativeField", err)
	}
	// Empty patch is a no-op, never an error.
	if err := ValidateCreativePatch(CreativeDraft, CreativePatch{}); err != nil {
		t.Fatalf("empty patch err = %v, want nil", err)
	}
}

// testCreativeCampaign is a scratch helper name kept for continuity
// with the campaigns_test.go fixture style (see testSellerFixture).
func ghostCreativeCampaignID() pgtype.UUID {
	var id pgtype.UUID
	// A fixed UUID that can never collide with a real row.
	if err := id.Scan("00000000-0000-0000-0000-000000000000"); err != nil {
		panic(err)
	}
	return id
}

func TestCreativeLifecycle(t *testing.T) {
	pool := testAdsPool(t)
	ctx := context.Background()
	campSvc := NewCampaignService(pool)
	svc := NewCreativeService(pool, campSvc)
	adv := testSellerFixture(t, pool, "creative-lifecycle")

	camp, err := campSvc.Create(ctx, adv, CreateInput{Name: "Creative Home", BudgetPaise: 50000})
	if err != nil {
		t.Fatalf("campaign fixture: %v", err)
	}

	c, err := svc.Create(ctx, adv, camp.ID, validCreativeInput())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.Status != CreativeDraft {
		t.Fatalf("new creative status = %q, want draft", c.Status)
	}
	if c.Kind != CreativeKindImage || c.Format != "image_1200x628" {
		t.Fatalf("registry mismatch: %+v", c)
	}

	// Validation happens before any write: bad input must not persist.
	if _, err := svc.Create(ctx, adv, camp.ID, func() CreativeInput {
		in := validCreativeInput()
		in.MediaURL = "javascript:alert(1)"
		return in
	}()); !errors.Is(err, ErrInvalidCreativeField) {
		t.Fatalf("bad URL err = %v, want ErrInvalidCreativeField", err)
	}

	// Activate: draft → active.
	active, err := svc.Transition(ctx, adv, c.ID, CreativeActive)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if active.Status != CreativeActive {
		t.Fatalf("status = %q, want active", active.Status)
	}

	// Active creatives lock kind/format.
	if _, err := svc.Update(ctx, adv, c.ID, CreativePatch{
		Kind: strptr(CreativeKindVideo), Format: strptr("video_1920x1080"),
	}); !errors.Is(err, ErrCreativeKindLocked) {
		t.Fatalf("active kind change err = %v, want ErrCreativeKindLocked", err)
	}

	// Copy fields still patchable while active.
	patched, err := svc.Update(ctx, adv, c.ID, CreativePatch{Headline: strptr("New headline")})
	if err != nil {
		t.Fatalf("patch headline: %v", err)
	}
	if patched.Headline != "New headline" {
		t.Fatalf("headline = %q", patched.Headline)
	}

	// Double-activate is not an edge.
	if _, err := svc.Transition(ctx, adv, c.ID, CreativeActive); !errors.Is(err, ErrBadCreativeTransition) {
		t.Fatalf("double activate err = %v, want ErrBadCreativeTransition", err)
	}

	// Archive is terminal: Get 404s afterwards (tombstone).
	archived, err := svc.Transition(ctx, adv, c.ID, CreativeArchived)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if archived.Status != CreativeArchived {
		t.Fatalf("status = %q, want archived", archived.Status)
	}
	if _, err := svc.Get(ctx, adv, c.ID); !errors.Is(err, ErrCreativeNotFound) {
		t.Fatalf("archived get err = %v, want ErrCreativeNotFound", err)
	}
	// Archived creatives never re-enter the matrix.
	if _, err := svc.Transition(ctx, adv, c.ID, CreativeActive); !errors.Is(err, ErrCreativeNotFound) {
		t.Fatalf("archived reactivate err = %v, want ErrCreativeNotFound", err)
	}
}

func TestCreativeOwnerIsolation(t *testing.T) {
	pool := testAdsPool(t)
	ctx := context.Background()
	campSvc := NewCampaignService(pool)
	svc := NewCreativeService(pool, campSvc)
	advA := testSellerFixture(t, pool, "creative-own-a")
	advB := testSellerFixture(t, pool, "creative-own-b")

	camp, err := campSvc.Create(ctx, advA, CreateInput{Name: "A Campaign", BudgetPaise: 50000})
	if err != nil {
		t.Fatalf("campaign fixture: %v", err)
	}
	c, err := svc.Create(ctx, advA, camp.ID, validCreativeInput())
	if err != nil {
		t.Fatalf("creative fixture: %v", err)
	}

	// B reads A's creative → 403-class, never the row.
	if _, err := svc.Get(ctx, advB, c.ID); !errors.Is(err, ErrCreativeNotOwner) {
		t.Fatalf("cross-owner get err = %v, want ErrCreativeNotOwner", err)
	}
	// B creates under A's campaign → 403-class.
	if _, err := svc.Create(ctx, advB, camp.ID, validCreativeInput()); !errors.Is(err, ErrCampaignNotOwner) {
		t.Fatalf("cross-owner create err = %v, want ErrCampaignNotOwner", err)
	}
	// B lists A's campaign → 403-class.
	if _, err := svc.List(ctx, advB, camp.ID, 10, nil); !errors.Is(err, ErrCampaignNotOwner) {
		t.Fatalf("cross-owner list err = %v, want ErrCampaignNotOwner", err)
	}
	// Ghost campaign → 404-class.
	if _, err := svc.Create(ctx, advA, ghostCreativeCampaignID(), validCreativeInput()); !errors.Is(err, ErrCampaignNotFound) {
		t.Fatalf("ghost campaign err = %v, want ErrCampaignNotFound", err)
	}
}

func TestCreativePagination(t *testing.T) {
	pool := testAdsPool(t)
	ctx := context.Background()
	campSvc := NewCampaignService(pool)
	svc := NewCreativeService(pool, campSvc)
	adv := testSellerFixture(t, pool, "creative-page")

	camp, err := campSvc.Create(ctx, adv, CreateInput{Name: "Page Campaign", BudgetPaise: 50000})
	if err != nil {
		t.Fatalf("campaign fixture: %v", err)
	}
	names := []string{"c-one", "c-two", "c-three"}
	for _, n := range names {
		in := validCreativeInput()
		in.Name = n
		if _, err := svc.Create(ctx, adv, camp.ID, in); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}

	page1, err := svc.List(ctx, adv, camp.ID, 2, nil)
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if len(page1.Items) != 2 || page1.NextCursor == "" {
		t.Fatalf("page 1 = %d items cursor %q, want 2 + cursor", len(page1.Items), page1.NextCursor)
	}
	if page1.Items[0].Name != "c-three" || page1.Items[1].Name != "c-two" {
		t.Fatalf("page 1 order = %q,%q, want newest-first", page1.Items[0].Name, page1.Items[1].Name)
	}
	after, err := DecodeAdsCursor(page1.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	page2, err := svc.List(ctx, adv, camp.ID, 2, &after)
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(page2.Items) != 1 || page2.NextCursor != "" {
		t.Fatalf("page 2 = %d items cursor %q, want 1 + no cursor", len(page2.Items), page2.NextCursor)
	}
	if page2.Items[0].Name != "c-one" {
		t.Fatalf("page 2 item = %q, want c-one", page2.Items[0].Name)
	}

	// A malformed cursor string fails loudly at the HTTP layer
	// (DecodeAdsCursor); the service takes a decoded cursor.
	if _, err := DecodeAdsCursor("not-a-cursor"); err == nil {
		t.Fatal("malformed cursor must fail")
	}
}

func TestActiveCreativesServingRead(t *testing.T) {
	pool := testAdsPool(t)
	ctx := context.Background()
	campSvc := NewCampaignService(pool)
	svc := NewCreativeService(pool, campSvc)
	adv := testSellerFixture(t, pool, "creative-serve")

	camp, err := campSvc.Create(ctx, adv, CreateInput{Name: "Serve Campaign", BudgetPaise: 50000})
	if err != nil {
		t.Fatalf("campaign fixture: %v", err)
	}
	draft, err := svc.Create(ctx, adv, camp.ID, validCreativeInput())
	if err != nil {
		t.Fatalf("draft fixture: %v", err)
	}
	_ = draft // drafts are invisible to the serving reader by design
	live, err := svc.Create(ctx, adv, camp.ID, func() CreativeInput {
		in := validCreativeInput()
		in.Name = "live one"
		return in
	}())
	if err != nil {
		t.Fatalf("live fixture: %v", err)
	}
	if _, err := svc.Transition(ctx, adv, live.ID, CreativeActive); err != nil {
		t.Fatalf("activate: %v", err)
	}
	dead, err := svc.Create(ctx, adv, camp.ID, func() CreativeInput {
		in := validCreativeInput()
		in.Name = "dead one"
		return in
	}())
	if err != nil {
		t.Fatalf("dead fixture: %v", err)
	}
	if _, err := svc.Transition(ctx, adv, dead.ID, CreativeArchived); err != nil {
		t.Fatalf("archive: %v", err)
	}

	got, err := svc.ActiveCreatives(ctx, camp.ID)
	if err != nil {
		t.Fatalf("ActiveCreatives: %v", err)
	}
	if len(got) != 1 || got[0].ID != live.ID {
		t.Fatalf("ActiveCreatives returned %d rows, want exactly the active one", len(got))
	}
}
