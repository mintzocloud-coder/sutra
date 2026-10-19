// SPDX-License-Identifier: AGPL-3.0-or-later
package miniapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// Pure unit tests (no database).

func TestRegisterValidation(t *testing.T) {
	str := func(s string) *string { return &s }
	badManifest := func() json.RawMessage { return json.RawMessage(`{"permissions":["telepathy"]}`) }
	dupManifest := func() json.RawMessage { return json.RawMessage(`{"permissions":["camera","camera"]}`) }
	okManifest := func() json.RawMessage { return json.RawMessage(`{"permissions":["camera","storage"]}`) }
	cases := []struct {
		name string
		in   RegisterInput
		want error
	}{
		{"empty name", RegisterInput{Slug: "ok-app", EntryURL: "https://a.example/"}, ErrInvalidField},
		{"bad slug uppercase", RegisterInput{Name: "A", Slug: "Bad", EntryURL: "https://a.example/"}, ErrInvalidSlug},
		{"bad slug short", RegisterInput{Name: "A", Slug: "ab", EntryURL: "https://a.example/"}, ErrInvalidSlug},
		{"bad entry url", RegisterInput{Name: "A", Slug: "ok-app", EntryURL: "ftp://a.example/"}, ErrInvalidField},
		{"javascript entry url", RegisterInput{Name: "A", Slug: "ok-app", EntryURL: "javascript:alert(1)"}, ErrInvalidField},
		{"unknown permission", RegisterInput{Name: "A", Slug: "ok-app", EntryURL: "https://a.example/", Manifest: badManifest()}, ErrInvalidManifest},
		{"duplicate permission", RegisterInput{Name: "A", Slug: "ok-app", EntryURL: "https://a.example/", Manifest: dupManifest()}, ErrInvalidManifest},
		{"non-object manifest", RegisterInput{Name: "A", Slug: "ok-app", EntryURL: "https://a.example/", Manifest: json.RawMessage(`[1]`)}, ErrInvalidManifest},
		{"bad icon url", RegisterInput{Name: "A", Slug: "ok-app", EntryURL: "https://a.example/", IconURL: str("javascript:x")}, ErrInvalidField},
		{"ok minimal", RegisterInput{Name: "Groceries", Slug: "ok-app-1", EntryURL: "https://a.example/app"}, nil},
		{"ok full", RegisterInput{Name: "Groceries", Slug: "ok-app-2", EntryURL: "https://a.example/app", IconURL: str("https://a.example/i.png"), Manifest: okManifest(), Description: str("fresh food")}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateRegister(tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("validateRegister = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTransitionsMatrix(t *testing.T) {
	cases := []struct {
		from, to string
		allowed  bool
	}{
		{StatusDraft, StatusPublished, true},
		{StatusDraft, StatusSuspended, false},
		{StatusDraft, StatusArchived, true},
		{StatusPublished, StatusDraft, false},
		{StatusPublished, StatusSuspended, true},
		{StatusPublished, StatusArchived, true},
		{StatusSuspended, StatusPublished, true},
		{StatusSuspended, StatusDraft, false},
		{StatusArchived, StatusPublished, false},
	}
	for _, tc := range cases {
		got := false
		for _, n := range Transitions[tc.from] {
			if n == tc.to {
				got = true
			}
		}
		if got != tc.allowed {
			t.Errorf("transition %s->%s = %v, want %v", tc.from, tc.to, got, tc.allowed)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	var id pgtype.UUID
	if err := id.Scan("123e4567-e89b-12d3-a456-426614174000"); err != nil {
		t.Fatal(err)
	}
	c := Cursor{CreatedAt: time.Date(2026, 10, 19, 8, 0, 0, 123456789, time.UTC), ID: id}
	s := c.Encode()
	if strings.Contains(s, "123e4567") {
		t.Fatalf("cursor leaks the id in the clear: %q", s)
	}
	back, err := DecodeCursor(s)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if back.ID != id || !back.CreatedAt.Equal(c.CreatedAt) {
		t.Fatalf("round trip mismatch: %+v", back)
	}
	for _, bad := range []string{"", "!!!", "bm90LWEtY3Vyc29y", c.Encode()[:4] + "!!!"} {
		if _, err := DecodeCursor(bad); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("DecodeCursor(%q) = %v, want ErrInvalidCursor", bad, err)
		}
	}
}

// Integration tests against a real PostgreSQL (migrations applied).
// Run when TEST_DATABASE_URL is set; otherwise skip cleanly.

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping miniapp integration test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn, 5, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func uniqueStr(t *testing.T, prefix string) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

func testUser(t *testing.T, pool *db.Pool) pgtype.UUID {
	t.Helper()
	u, err := users.Provision(context.Background(), pool, uniqueStr(t, "sub"), "Dev")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	var id pgtype.UUID
	if err := id.Scan(u.ID); err != nil {
		t.Fatal(err)
	}
	return id
}

func registerApp(t *testing.T, svc *Service, dev pgtype.UUID, slug string) *MiniApp {
	t.Helper()
	app, err := svc.Register(context.Background(), dev, RegisterInput{
		Name:     "Test App",
		Slug:     slug,
		EntryURL: "https://apps.example/" + slug,
		Manifest: json.RawMessage(`{"permissions":["camera"]}`),
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if app.Status != StatusDraft {
		t.Fatalf("new app status = %q, want draft", app.Status)
	}
	return app
}

func TestRegistryLifecycleIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	dev := testUser(t, pool)
	other := testUser(t, pool)
	slug := uniqueStr(t, "shop")

	app := registerApp(t, svc, dev, slug)
	if app.DeveloperID != dev {
		t.Fatalf("developer mismatch")
	}
	if len(app.Manifest.Permissions) != 1 || app.Manifest.Permissions[0] != "camera" {
		t.Fatalf("manifest permissions = %v", app.Manifest.Permissions)
	}

	// Slug race: second registration with the same slug loses.
	if _, err := svc.Register(ctx, other, RegisterInput{Name: "Copy", Slug: slug, EntryURL: "https://x.example/"}); !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("duplicate slug = %v, want ErrSlugTaken", err)
	}

	// Non-owner cannot update or transition.
	if _, err := svc.Update(ctx, app.ID, other, UpdateInput{Name: ptr("Hacked")}); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("non-owner update = %v, want ErrNotOwner", err)
	}
	if _, err := svc.Transition(ctx, app.ID, other, StatusPublished); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("non-owner publish = %v, want ErrNotOwner", err)
	}

	// Invalid transition: draft -> suspended.
	if _, err := svc.Transition(ctx, app.ID, dev, StatusSuspended); !errors.Is(err, ErrBadStatusTransition) {
		t.Fatalf("draft->suspended = %v, want ErrBadStatusTransition", err)
	}

	// Draft is not installable and not in the directory.
	if _, err := svc.Install(ctx, other, app.ID); !errors.Is(err, ErrInstallNotAllowed) {
		t.Fatalf("install draft = %v, want ErrInstallNotAllowed", err)
	}
	dir, err := svc.Directory(ctx, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range dir.Apps {
		if a.ID == app.ID {
			t.Fatalf("draft app leaked into the directory")
		}
	}

	// Publish, then update name + manifest, then install.
	pub, err := svc.Transition(ctx, app.ID, dev, StatusPublished)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if pub.Status != StatusPublished {
		t.Fatalf("status = %q", pub.Status)
	}
	updated, err := svc.Update(ctx, app.ID, dev, UpdateInput{
		Name:     ptr("Test App v2"),
		Manifest: rawPtr(`{"permissions":["camera","payments"],"display":{"theme":"dark"}}`),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "Test App v2" || len(updated.Manifest.Permissions) != 2 {
		t.Fatalf("update not applied: %+v", updated)
	}
	if _, ok := updated.Manifest.Raw["display"]; !ok {
		t.Fatalf("manifest metadata not preserved")
	}

	isNew, err := svc.Install(ctx, other, app.ID)
	if err != nil || !isNew {
		t.Fatalf("install = %v,%v", isNew, err)
	}
	isNew2, err := svc.Install(ctx, other, app.ID)
	if err != nil || isNew2 {
		t.Fatalf("re-install = %v,%v, want (false,nil)", isNew2, err)
	}
	shelf, err := svc.Installed(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if len(shelf) != 1 || shelf[0].MiniApp.ID != app.ID {
		t.Fatalf("shelf = %+v", shelf)
	}
	if shelf[0].MiniApp.EntryURL != "https://apps.example/"+slug {
		t.Fatalf("entry_url not returned to the client")
	}
	if err := svc.Uninstall(ctx, other, app.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Uninstall(ctx, other, app.ID); err != nil { // idempotent
		t.Fatal(err)
	}

	// Suspend -> not installable again; resume -> installable.
	if _, err := svc.Transition(ctx, app.ID, dev, StatusSuspended); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := svc.Install(ctx, other, app.ID); !errors.Is(err, ErrInstallNotAllowed) {
		t.Fatalf("install suspended = %v, want ErrInstallNotAllowed", err)
	}
	if _, err := svc.Transition(ctx, app.ID, dev, StatusPublished); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// Archive is terminal: Get 404s, no further transitions.
	if _, err := svc.Transition(ctx, app.ID, dev, StatusArchived); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := svc.Get(ctx, app.ID); !errors.Is(err, ErrMiniAppNotFound) {
		t.Fatalf("get archived = %v, want ErrMiniAppNotFound", err)
	}
	if _, err := svc.Transition(ctx, app.ID, dev, StatusPublished); !errors.Is(err, ErrMiniAppNotFound) {
		t.Fatalf("transition archived = %v, want ErrMiniAppNotFound", err)
	}
	if _, err := svc.Update(ctx, app.ID, dev, UpdateInput{}); !errors.Is(err, ErrMiniAppNotFound) {
		t.Fatalf("update archived = %v, want ErrMiniAppNotFound", err)
	}
}

func TestDirectoryPaginationIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	dev := testUser(t, pool)

	// Register+publish 3 apps; one stays draft (must not leak).
	ids := []pgtype.UUID{}
	for i := 0; i < 3; i++ {
		app := registerApp(t, svc, dev, uniqueStr(t, "dir"))
		if _, err := svc.Transition(ctx, app.ID, dev, StatusPublished); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, app.ID)
	}
	registerApp(t, svc, dev, uniqueStr(t, "draft")) // stays draft

	page1, err := svc.Directory(ctx, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Apps) != 2 || page1.NextCursor == "" {
		t.Fatalf("page1 = %d apps, cursor %q", len(page1.Apps), page1.NextCursor)
	}
	after, err := DecodeCursor(page1.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	page2, err := svc.Directory(ctx, 2, &after)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Apps) != 1 || page2.NextCursor != "" {
		t.Fatalf("page2 = %d apps, cursor %q", len(page2.Apps), page2.NextCursor)
	}
	seen := map[string]bool{}
	for _, a := range append(page1.Apps, page2.Apps...) {
		if seen[a.ID.String()] {
			t.Fatalf("duplicate app %s across pages", a.ID.String())
		}
		seen[a.ID.String()] = true
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 published apps, got %d", len(seen))
	}
}

func ptr(s string) *string { return &s }

func rawPtr(s string) *json.RawMessage { r := json.RawMessage(s); return &r }
