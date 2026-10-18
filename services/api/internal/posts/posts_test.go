// SPDX-License-Identifier: AGPL-3.0-or-later
package posts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

// Pure unit tests: no database needed.

func TestValidateText(t *testing.T) {
	if err := validateText("hello world"); err != nil {
		t.Fatalf("validateText(valid) = %v", err)
	}
	if err := validateText(strings.Repeat("a", MaxPostLenRunes)); err != nil {
		t.Fatalf("validateText(500 runes) = %v", err)
	}
	// Emoji are measured in runes, not bytes: 500 emoji are fine.
	if err := validateText(strings.Repeat("🎉", MaxPostLenRunes)); err != nil {
		t.Fatalf("validateText(500 emoji) = %v", err)
	}
	for _, blank := range []string{"", "   ", "\n\t "} {
		if err := validateText(blank); !errors.Is(err, ErrEmptyText) {
			t.Fatalf("validateText(%q) = %v, want ErrEmptyText", blank, err)
		}
	}
	if err := validateText(strings.Repeat("a", MaxPostLenRunes+1)); !errors.Is(err, ErrTextTooLong) {
		t.Fatalf("validateText(501 runes) = %v, want ErrTextTooLong", err)
	}
	if err := validateText(strings.Repeat("🎉", MaxPostLenRunes+1)); !errors.Is(err, ErrTextTooLong) {
		t.Fatalf("validateText(501 emoji) = %v, want ErrTextTooLong", err)
	}
}

func TestValidateMediaIDs(t *testing.T) {
	if err := ValidateMediaIDs(nil); err != nil {
		t.Fatalf("ValidateMediaIDs(nil) = %v", err)
	}
	if err := ValidateMediaIDs([]string{"abc_123-XYZ", strings.Repeat("m", MaxMediaIDLen)}); err != nil {
		t.Fatalf("ValidateMediaIDs(valid) = %v", err)
	}
	tooMany := make([]string, MaxMediaIDsPerPost+1)
	for i := range tooMany {
		tooMany[i] = "media-ok"
	}
	if err := ValidateMediaIDs(tooMany); !errors.Is(err, ErrTooManyMedia) {
		t.Fatalf("ValidateMediaIDs(11) = %v, want ErrTooManyMedia", err)
	}
	for _, bad := range []string{"", "has space", "with/slash", "semi;colon",
		"java:script", strings.Repeat("m", MaxMediaIDLen+1), "café"} {
		if err := ValidateMediaIDs([]string{bad}); !errors.Is(err, ErrBadMediaID) {
			t.Fatalf("ValidateMediaIDs(%q) = %v, want ErrBadMediaID", bad, err)
		}
	}
}

func TestNewServiceDefaultCap(t *testing.T) {
	svc := NewService(nil, 0)
	if svc.velocityCapPerHour != DefaultVelocityCap {
		t.Fatalf("default cap = %d, want %d", svc.velocityCapPerHour, DefaultVelocityCap)
	}
	svc = NewService(nil, -5)
	if svc.velocityCapPerHour != DefaultVelocityCap {
		t.Fatalf("negative cap = %d, want default %d", svc.velocityCapPerHour, DefaultVelocityCap)
	}
}

// Integration tests: run when TEST_DATABASE_URL is set; otherwise skip
// cleanly (same convention as internal/social).

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping posts integration test")
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

func uniqueSubject(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "posts-test-" + hex.EncodeToString(b[:])
}

func provisionUser(t *testing.T, pool *db.Pool) pgtype.UUID {
	t.Helper()
	u, err := users.Provision(context.Background(), pool, uniqueSubject(t), "Posts Tester")
	if err != nil {
		t.Fatalf("users.Provision: %v", err)
	}
	id, err := social.ParseUserID(u.ID)
	if err != nil {
		t.Fatalf("ParseUserID(provisioned): %v", err)
	}
	return id
}

func TestCreateGetDelete(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool, 1000) // velocity cap out of the way
	author := provisionUser(t, pool)

	p, err := svc.Create(ctx, author, "first post", []string{"media_1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.AuthorID != author.String() || p.Text != "first post" {
		t.Fatalf("Create returned %+v", p)
	}
	if len(p.MediaIDs) != 1 || p.MediaIDs[0] != "media_1" {
		t.Fatalf("Create media_ids = %v", p.MediaIDs)
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Fatalf("Create timestamps zero: %+v", p)
	}

	got, err := svc.Get(ctx, mustUUID(t, p.ID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != p.ID || got.Text != "first post" {
		t.Fatalf("Get = %+v, want id %s", got, p.ID)
	}

	// Nil media ids store as the empty list, never NULL.
	p2, err := svc.Create(ctx, author, "no media", nil)
	if err != nil {
		t.Fatalf("Create(nil media): %v", err)
	}
	if p2.MediaIDs == nil || len(p2.MediaIDs) != 0 {
		t.Fatalf("nil media_ids stored as %v", p2.MediaIDs)
	}

	// Non-author delete is forbidden; the post survives.
	other := provisionUser(t, pool)
	if err := svc.Delete(ctx, mustUUID(t, p.ID), other); !errors.Is(err, ErrNotAuthor) {
		t.Fatalf("Delete by non-author = %v, want ErrNotAuthor", err)
	}
	if _, err := svc.Get(ctx, mustUUID(t, p.ID)); err != nil {
		t.Fatalf("post gone after forbidden delete: %v", err)
	}

	// Author delete succeeds; second delete is a 404, not silent.
	if err := svc.Delete(ctx, mustUUID(t, p.ID), author); err != nil {
		t.Fatalf("Delete by author: %v", err)
	}
	if _, err := svc.Get(ctx, mustUUID(t, p.ID)); !errors.Is(err, ErrPostNotFound) {
		t.Fatalf("Get after delete = %v, want ErrPostNotFound", err)
	}
	if err := svc.Delete(ctx, mustUUID(t, p.ID), author); !errors.Is(err, ErrPostNotFound) {
		t.Fatalf("Delete twice = %v, want ErrPostNotFound", err)
	}

	// Deleting a never-existing id is also 404.
	var ghost pgtype.UUID
	if err := ghost.Scan("123e4567-e89b-12d3-a456-426614174000"); err != nil {
		t.Fatalf("scan ghost: %v", err)
	}
	if err := svc.Delete(ctx, ghost, author); !errors.Is(err, ErrPostNotFound) {
		t.Fatalf("Delete ghost = %v, want ErrPostNotFound", err)
	}
}

func TestCreateValidation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool, 1000)
	author := provisionUser(t, pool)

	for _, tc := range []struct {
		name     string
		text     string
		mediaIDs []string
		want     error
	}{
		{"blank", "   ", nil, ErrEmptyText},
		{"too long", strings.Repeat("x", MaxPostLenRunes+1), nil, ErrTextTooLong},
		{"too many media", "ok", []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}, ErrTooManyMedia},
		{"bad media id", "ok", []string{"not a media id!"}, ErrBadMediaID},
	} {
		if _, err := svc.Create(ctx, author, tc.text, tc.mediaIDs); !errors.Is(err, tc.want) {
			t.Fatalf("%s: Create = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestVelocityCap(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const cap = 3
	svc := NewService(pool, cap)
	author := provisionUser(t, pool)

	for i := 0; i < cap; i++ {
		if _, err := svc.Create(ctx, author, "post "+string(rune('a'+i)), nil); err != nil {
			t.Fatalf("Create %d/%d: %v", i+1, cap, err)
		}
	}
	_, err := svc.Create(ctx, author, "one too many", nil)
	var vel *VelocityLimitedError
	if !errors.As(err, &vel) {
		t.Fatalf("Create over cap = %v (%T), want *VelocityLimitedError", err, err)
	}
	if vel.Cap != cap {
		t.Fatalf("vel.Cap = %d, want %d", vel.Cap, cap)
	}
	if vel.RetryAfter < time.Second {
		t.Fatalf("vel.RetryAfter = %v, want >= 1s", vel.RetryAfter)
	}
	// Another author is unaffected: the cap is per-user.
	other := provisionUser(t, pool)
	if _, err := svc.Create(ctx, other, "other author post", nil); err != nil {
		t.Fatalf("Create by other author: %v", err)
	}
}

func TestListByAuthorPagination(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool, 1000)
	author := provisionUser(t, pool)

	const total = 5
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		p, err := svc.Create(ctx, author, "post #"+string(rune('0'+i)), nil)
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids = append(ids, p.ID)
		time.Sleep(2 * time.Millisecond) // distinct created_at per post
	}

	// Page through with limit 2: newest first, 3 pages (2+2+1).
	var seen []string
	var cursor *social.Cursor
	for page := 0; ; page++ {
		pg, err := svc.ListByAuthor(ctx, author, 2, cursor)
		if err != nil {
			t.Fatalf("ListByAuthor page %d: %v", page, err)
		}
		for _, item := range pg.Items {
			seen = append(seen, item.ID)
		}
		if pg.NextCursor == nil {
			break
		}
		c, err := social.DecodeCursor(*pg.NextCursor)
		if err != nil {
			t.Fatalf("DecodeCursor: %v", err)
		}
		cursor = &c
		if page > 5 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Fatalf("paginated %d posts, want %d", len(seen), total)
	}
	// Newest first: ids reversed.
	for i, id := range seen {
		if id != ids[total-1-i] {
			t.Fatalf("order[%d] = %s, want %s", i, id, ids[total-1-i])
		}
	}

	// Unknown author (valid UUID, no user): empty listing, no error.
	var ghost pgtype.UUID
	if err := ghost.Scan("123e4567-e89b-12d3-a456-426614174001"); err != nil {
		t.Fatalf("scan ghost: %v", err)
	}
	pg, err := svc.ListByAuthor(ctx, ghost, 10, nil)
	if err != nil {
		t.Fatalf("ListByAuthor(ghost): %v", err)
	}
	if len(pg.Items) != 0 || pg.NextCursor != nil {
		t.Fatalf("ghost listing = %+v", pg)
	}

	// Malformed cursor is a client error, not a DB error.
	bad := &social.Cursor{T: time.Now().UTC(), ID: "not-a-uuid"}
	if _, err := svc.ListByAuthor(ctx, author, 10, bad); !errors.Is(err, social.ErrInvalidCursor) {
		t.Fatalf("ListByAuthor(bad cursor) = %v, want ErrInvalidCursor", err)
	}
}

func TestListByAuthorExcludesOthers(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool, 1000)
	a := provisionUser(t, pool)
	b := provisionUser(t, pool)

	if _, err := svc.Create(ctx, a, "a's post", nil); err != nil {
		t.Fatalf("Create a: %v", err)
	}
	if _, err := svc.Create(ctx, b, "b's post", nil); err != nil {
		t.Fatalf("Create b: %v", err)
	}

	pg, err := svc.ListByAuthor(ctx, a, 10, nil)
	if err != nil {
		t.Fatalf("ListByAuthor(a): %v", err)
	}
	if len(pg.Items) != 1 || pg.Items[0].Text != "a's post" || pg.Items[0].AuthorID != a.String() {
		t.Fatalf("a's timeline = %+v", pg.Items)
	}
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil || !u.Valid {
		t.Fatalf("mustUUID(%q): %v", s, err)
	}
	return u
}
