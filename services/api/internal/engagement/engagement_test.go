// SPDX-License-Identifier: AGPL-3.0-or-later
package engagement

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/posts"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

// Integration tests: run when TEST_DATABASE_URL is set; otherwise skip
// cleanly (same convention as internal/posts, internal/social).

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping engagement integration test")
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
	return "engagement-test-" + hex.EncodeToString(b[:])
}

func provisionUser(t *testing.T, pool *db.Pool) pgtype.UUID {
	t.Helper()
	u, err := users.Provision(context.Background(), pool, uniqueSubject(t), "Engagement Tester")
	if err != nil {
		t.Fatalf("users.Provision: %v", err)
	}
	id, err := social.ParseUserID(u.ID)
	if err != nil {
		t.Fatalf("social.ParseUserID: %v", err)
	}
	return id
}

func createPost(t *testing.T, pool *db.Pool, authorID pgtype.UUID) pgtype.UUID {
	t.Helper()
	svc := posts.NewService(pool, 1000000) // velocity cap irrelevant here
	p, err := svc.Create(context.Background(), authorID, "like me", nil)
	if err != nil {
		t.Fatalf("posts.Create: %v", err)
	}
	var id pgtype.UUID
	if err := id.Scan(p.ID); err != nil || !id.Valid {
		t.Fatalf("bad post id: %v", p.ID)
	}
	return id
}

func TestLikeUnlikeIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	author, liker := provisionUser(t, pool), provisionUser(t, pool)
	postID := createPost(t, pool, author)

	created, count, err := svc.Like(ctx, postID, liker)
	if err != nil {
		t.Fatalf("Like: %v", err)
	}
	if !created || count != 1 {
		t.Fatalf("first like: created=%v count=%d, want true/1", created, count)
	}
	// Retry: idempotent, no double count.
	created, count, err = svc.Like(ctx, postID, liker)
	if err != nil {
		t.Fatalf("Like retry: %v", err)
	}
	if created || count != 1 {
		t.Fatalf("like retry: created=%v count=%d, want false/1", created, count)
	}

	removed, count, err := svc.Unlike(ctx, postID, liker)
	if err != nil {
		t.Fatalf("Unlike: %v", err)
	}
	if !removed || count != 0 {
		t.Fatalf("unlike: removed=%v count=%d, want true/0", removed, count)
	}
	// Retry: idempotent no-op.
	removed, count, err = svc.Unlike(ctx, postID, liker)
	if err != nil {
		t.Fatalf("Unlike retry: %v", err)
	}
	if removed || count != 0 {
		t.Fatalf("unlike retry: removed=%v count=%d, want false/0", removed, count)
	}
}

func TestLikeUnknownPost(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	liker := provisionUser(t, pool)
	var ghost pgtype.UUID
	if err := ghost.Scan("00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, _, err := svc.Like(ctx, ghost, liker); !errors.Is(err, ErrPostNotFound) {
		t.Fatalf("Like on unknown post: err=%v, want ErrPostNotFound", err)
	}
	// Unlike is "ensure absent": no error even for unknown posts.
	if _, _, err := svc.Unlike(ctx, ghost, liker); err != nil {
		t.Fatalf("Unlike on unknown post: %v", err)
	}
}

func TestLikeConcurrentRacers(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	author := provisionUser(t, pool)
	postID := createPost(t, pool, author)

	// 10 goroutines like the SAME post as the SAME user: the PK
	// guarantees exactly one row, no matter the interleaving.
	liker := provisionUser(t, pool)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.Like(ctx, postID, liker)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Like: %v", err)
		}
	}
	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM likes WHERE post_id = $1`,
		postID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("concurrent likes converged on %d rows, want 1", n)
	}
}

func TestSignalsBatch(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	author := provisionUser(t, pool)
	p1 := createPost(t, pool, author)
	p2 := createPost(t, pool, author)
	p3 := createPost(t, pool, author) // never liked: absent from the map

	// Two fresh likes on p1, one fresh + one aged like on p2.
	u1, u2, u3 := provisionUser(t, pool), provisionUser(t, pool), provisionUser(t, pool)
	for _, u := range []pgtype.UUID{u1, u2} {
		if _, _, err := svc.Like(ctx, p1, u); err != nil {
			t.Fatalf("Like p1: %v", err)
		}
	}
	if _, _, err := svc.Like(ctx, p2, u1); err != nil {
		t.Fatalf("Like p2: %v", err)
	}
	if _, _, err := svc.Like(ctx, p2, u3); err != nil {
		t.Fatalf("Like p2: %v", err)
	}
	// Age one of p2's likes beyond the 24h window.
	if _, err := pool.Exec(ctx,
		`UPDATE likes SET created_at = now() - interval '48 hours'
		 WHERE post_id = $1 AND user_id = $2`, p2, u3); err != nil {
		t.Fatalf("age like: %v", err)
	}

	sigs, err := svc.Signals(ctx, []pgtype.UUID{p1, p2, p3})
	if err != nil {
		t.Fatalf("Signals: %v", err)
	}
	s1, ok := sigs[p1.String()]
	if !ok || s1.LikesTotal != 2 || s1.LikesLast24h != 2 {
		t.Fatalf("p1 signals: %+v (ok=%v), want total=2 last24h=2", s1, ok)
	}
	s2, ok := sigs[p2.String()]
	if !ok || s2.LikesTotal != 2 || s2.LikesLast24h != 1 {
		t.Fatalf("p2 signals: %+v (ok=%v), want total=2 last24h=1", s2, ok)
	}
	if _, ok := sigs[p3.String()]; ok {
		t.Fatalf("p3 (unliked) present in signals map")
	}
	// Empty input: no query, empty map, no error.
	if out, err := svc.Signals(ctx, nil); err != nil || len(out) != 0 {
		t.Fatalf("Signals(nil): out=%v err=%v", out, err)
	}
}

func TestAuthorFollowerCounts(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	a1, a2 := provisionUser(t, pool), provisionUser(t, pool)
	a3 := provisionUser(t, pool) // no followers: absent from the map
	f1, f2 := provisionUser(t, pool), provisionUser(t, pool)

	for _, f := range []pgtype.UUID{f1, f2} {
		if _, err := social.Follow(ctx, pool, f, a1); err != nil {
			t.Fatalf("Follow a1: %v", err)
		}
	}
	if _, err := social.Follow(ctx, pool, f1, a2); err != nil {
		t.Fatalf("Follow a2: %v", err)
	}

	counts, err := svc.AuthorFollowerCounts(ctx, []pgtype.UUID{a1, a2, a3})
	if err != nil {
		t.Fatalf("AuthorFollowerCounts: %v", err)
	}
	if counts[a1.String()] != 2 {
		t.Fatalf("a1 followers: %d, want 2", counts[a1.String()])
	}
	if counts[a2.String()] != 1 {
		t.Fatalf("a2 followers: %d, want 1", counts[a2.String()])
	}
	if _, ok := counts[a3.String()]; ok {
		t.Fatalf("a3 (no followers) present in counts map")
	}
}

func TestLikeDeletedPostCascades(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	author, liker := provisionUser(t, pool), provisionUser(t, pool)
	postID := createPost(t, pool, author)
	if _, _, err := svc.Like(ctx, postID, liker); err != nil {
		t.Fatalf("Like: %v", err)
	}
	// Deleting the post cascades the like rows (migration 0007 FK).
	if _, err := pool.Exec(ctx, `DELETE FROM posts WHERE id = $1`, postID); err != nil {
		t.Fatalf("delete post: %v", err)
	}
	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM likes WHERE post_id = $1`,
		postID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("likes survived post deletion: %d rows", n)
	}
}
