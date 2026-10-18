// SPDX-License-Identifier: AGPL-3.0-or-later
package feed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/posts"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

// ---------------------------------------------------------------------------
// Pure unit tests (no infrastructure).
// ---------------------------------------------------------------------------

func TestScoreRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	if got := timeOf(scoreOf(now)); !got.Equal(now) {
		t.Fatalf("score round-trip: got %v want %v", got, now)
	}
	// Microsecond scores keep full ordering precision (unlike nanos,
	// which lose bits in a float64 mantissa).
	a := time.UnixMicro(1760793935000000)
	b := time.UnixMicro(1760793935000001)
	if scoreOf(a) >= scoreOf(b) {
		t.Fatalf("score ordering broken: %v >= %v", scoreOf(a), scoreOf(b))
	}
}

func TestFeedCursorRoundTrip(t *testing.T) {
	c := &FeedCursor{
		Pushed: &PushedCursor{Score: 1760793935000000, Member: "123e4567-e89b-12d3-a456-426614174000"},
		Pull:   &social.Cursor{T: time.UnixMicro(1760793934000000).UTC(), ID: "123e4567-e89b-12d3-a456-426614174001"},
	}
	s := EncodeFeedCursor(c)
	if s == "" {
		t.Fatal("encoded cursor is empty")
	}
	back, err := DecodeFeedCursor(s)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Pushed.Score != c.Pushed.Score || back.Pushed.Member != c.Pushed.Member {
		t.Fatalf("pushed cursor mismatch: %+v", back.Pushed)
	}
	if !back.Pull.T.Equal(c.Pull.T) || back.Pull.ID != c.Pull.ID {
		t.Fatalf("pull cursor mismatch: %+v", back.Pull)
	}
	// A nil cursor encodes as "" (end of feed).
	if EncodeFeedCursor(nil) != "" {
		t.Fatal("nil cursor must encode as empty string")
	}
}

func TestDecodeFeedCursorInvalid(t *testing.T) {
	for name, s := range map[string]string{
		"not base64":       "!!!not-base64!!!",
		"base64 not json":  "aGVsbG8=", // "hello"
		"json wrong shape": "e30=",     // "{}" — zero score/member
		"empty":            "",
		"bad member uuid": EncodeFeedCursor(&FeedCursor{
			Pushed: &PushedCursor{Score: 1, Member: "not-a-uuid"},
		}),
		"zero score": EncodeFeedCursor(&FeedCursor{
			Pushed: &PushedCursor{Score: 0, Member: "123e4567-e89b-12d3-a456-426614174000"},
		}),
		"pull zero time": EncodeFeedCursor(&FeedCursor{
			Pull: &social.Cursor{ID: "123e4567-e89b-12d3-a456-426614174000"},
		}),
	} {
		if _, err := DecodeFeedCursor(s); err == nil {
			t.Errorf("%s: expected ErrInvalidCursor, got nil", name)
		}
	}
}

func TestMergeEntries(t *testing.T) {
	mk := func(id string, micros int64) PushedEntry {
		return PushedEntry{ID: id, Score: float64(micros)}
	}
	mkp := func(id string, micros int64) posts.Post {
		return posts.Post{ID: id, CreatedAt: time.UnixMicro(micros).UTC()}
	}
	pushed := []PushedEntry{mk("p3", 300), mk("p2", 200), mk("p1", 100)}
	pulled := []posts.Post{mkp("q2", 250), mkp("q1", 150)}

	merged, cp, cq := mergeEntries(pushed, pulled, 4)
	want := []string{"p3", "q2", "p2", "q1"}
	if len(merged) != len(want) {
		t.Fatalf("merged %d entries, want %d", len(merged), len(want))
	}
	for i, id := range want {
		if merged[i].id != id {
			t.Fatalf("position %d: got %s want %s", i, merged[i].id, id)
		}
	}
	if cp != 2 || cq != 2 {
		t.Fatalf("consumed pushed=%d pulled=%d, want 2 and 2", cp, cq)
	}

	// Tie on time: larger id first (matches the (score DESC, member
	// DESC) stream order); dedupe across streams consumes from both
	// but returns the id once.
	pushed = []PushedEntry{mk("b", 100), mk("a", 100)}
	pulled = []posts.Post{mkp("a", 100)}
	merged, cp, cq = mergeEntries(pushed, pulled, 10)
	if len(merged) != 2 || merged[0].id != "b" || merged[1].id != "a" {
		t.Fatalf("tie/dedupe wrong: %+v", merged)
	}
	if cp != 2 || cq != 1 {
		t.Fatalf("tie/dedupe consumed pushed=%d pulled=%d, want 2 and 1", cp, cq)
	}

	// Limit respected.
	merged, _, _ = mergeEntries(pushed, pulled, 1)
	if len(merged) != 1 || merged[0].id != "b" {
		t.Fatalf("limit=1 wrong: %+v", merged)
	}
}

// ---------------------------------------------------------------------------
// Integration tests (real PostgreSQL + real Valkey/Redis).
// ---------------------------------------------------------------------------

var feedTestSeq atomic.Int64

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping feed integration test")
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

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR not set; skipping feed integration test")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping %s: %v", addr, err)
	}
	t.Cleanup(func() {
		_ = rdb.FlushDB(context.Background()).Err()
		_ = rdb.Close()
	})
	return rdb
}

func feedUser(t *testing.T, pool *db.Pool, name string) (pgtype.UUID, string) {
	t.Helper()
	subject := fmt.Sprintf("feed-test-%d-%d", time.Now().UnixNano(), feedTestSeq.Add(1))
	u, err := users.Provision(context.Background(), pool, subject, name)
	if err != nil {
		t.Fatalf("provision %s: %v", name, err)
	}
	var id pgtype.UUID
	if err := id.Scan(u.ID); err != nil || !id.Valid {
		t.Fatalf("bad user id %q: %v", u.ID, err)
	}
	return id, u.ID
}

func feedFollow(t *testing.T, pool *db.Pool, follower, followee pgtype.UUID) {
	t.Helper()
	if _, err := social.Follow(context.Background(), pool, follower, followee); err != nil {
		t.Fatalf("follow: %v", err)
	}
}

func feedPost(t *testing.T, pool *db.Pool, author pgtype.UUID, text string) *posts.Post {
	t.Helper()
	svc := posts.NewService(pool, 100000) // velocity cap irrelevant here
	p, err := svc.Create(context.Background(), author, text, nil)
	if err != nil {
		t.Fatalf("create post: %v", err)
	}
	return p
}

// setPostTime pins a post's timestamps so merge-order tests are
// deterministic (creation order would only give microsecond gaps).
func setPostTime(t *testing.T, pool *db.Pool, p *posts.Post, ts time.Time) {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(p.ID); err != nil {
		t.Fatalf("scan post id: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE posts SET created_at = $1, updated_at = $1 WHERE id = $2`, ts, id); err != nil {
		t.Fatalf("set post time: %v", err)
	}
	p.CreatedAt, p.UpdatedAt = ts, ts
}

func feedService(t *testing.T, pool *db.Pool, rdb *redis.Client, threshold, max int) *Service {
	t.Helper()
	return NewService(pool, rdb, threshold, max)
}

func TestFanoutPushPath(t *testing.T) {
	pool := testPool(t)
	rdb := testRedis(t)
	ctx := context.Background()
	svc := feedService(t, pool, rdb, 10000, 100)

	author, _ := feedUser(t, pool, "Author")
	follower, _ := feedUser(t, pool, "Follower")
	stranger, _ := feedUser(t, pool, "Stranger")
	feedFollow(t, pool, follower, author)

	p := feedPost(t, pool, author, "hello followers")
	if err := svc.FanoutPost(ctx, p); err != nil {
		t.Fatalf("FanoutPost: %v", err)
	}

	// The follower's home feed carries the post …
	page, err := svc.HomeFeed(ctx, follower, 20, nil)
	if err != nil {
		t.Fatalf("HomeFeed follower: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != p.ID || page.Items[0].Text != "hello followers" {
		t.Fatalf("follower feed wrong: %+v", page.Items)
	}
	if page.NextCursor != nil {
		t.Fatalf("expected end of feed, got cursor %v", *page.NextCursor)
	}

	// … the author sees their own post …
	apage, err := svc.HomeFeed(ctx, author, 20, nil)
	if err != nil || len(apage.Items) != 1 || apage.Items[0].ID != p.ID {
		t.Fatalf("author feed wrong: %+v err=%v", apage, err)
	}

	// … and a non-follower sees nothing.
	spage, err := svc.HomeFeed(ctx, stranger, 20, nil)
	if err != nil || len(spage.Items) != 0 {
		t.Fatalf("stranger feed wrong: %+v err=%v", spage, err)
	}
}

func TestFanoutCelebrityPullPath(t *testing.T) {
	pool := testPool(t)
	rdb := testRedis(t)
	ctx := context.Background()
	// Threshold 1: an author with >1 follower is a celebrity.
	svc := feedService(t, pool, rdb, 1, 100)

	celeb, _ := feedUser(t, pool, "Celeb")
	f1, _ := feedUser(t, pool, "Fan1")
	f2, _ := feedUser(t, pool, "Fan2")
	feedFollow(t, pool, f1, celeb)
	feedFollow(t, pool, f2, celeb)

	p := feedPost(t, pool, celeb, "celebrity post")
	if err := svc.FanoutPost(ctx, p); err != nil {
		t.Fatalf("FanoutPost: %v", err)
	}

	// Nothing was written to Valkey (the write amplification bound) …
	if n, err := rdb.ZCard(ctx, homeKey(f1.String())).Result(); err != nil || n != 0 {
		t.Fatalf("celebrity fanned out: zcard=%d err=%v", n, err)
	}
	// … but the follower still sees the post via the pull path.
	page, err := svc.HomeFeed(ctx, f1, 20, nil)
	if err != nil {
		t.Fatalf("HomeFeed: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != p.ID {
		t.Fatalf("pull path missing celebrity post: %+v", page.Items)
	}
}

func TestHomeFeedMergeOrdering(t *testing.T) {
	pool := testPool(t)
	rdb := testRedis(t)
	ctx := context.Background()
	svc := feedService(t, pool, rdb, 1, 100) // threshold 1: celeb is pull-path

	reader, _ := feedUser(t, pool, "Reader")
	pushedAuthor, _ := feedUser(t, pool, "PushedAuthor")
	celeb, _ := feedUser(t, pool, "MergeCeleb")
	feedFollow(t, pool, reader, pushedAuthor)
	feedFollow(t, pool, reader, celeb)
	// Celeb needs >1 follower to cross the threshold of 1.
	other, _ := feedUser(t, pool, "Other")
	feedFollow(t, pool, other, celeb)

	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	mk := func(author pgtype.UUID, text string, offset int) *posts.Post {
		p := feedPost(t, pool, author, text)
		ts := base.Add(time.Duration(offset) * time.Second)
		setPostTime(t, pool, p, ts)
		if err := svc.FanoutPost(ctx, p); err != nil {
			t.Fatalf("FanoutPost %s: %v", text, err)
		}
		return p
	}
	// Interleaved: pushed@0, celeb@1, pushed@2, celeb@3.
	mk(pushedAuthor, "p0", 0)
	mk(celeb, "c1", 1)
	mk(pushedAuthor, "p2", 2)
	want3 := mk(celeb, "c3", 3)

	page, err := svc.HomeFeed(ctx, reader, 20, nil)
	if err != nil {
		t.Fatalf("HomeFeed: %v", err)
	}
	want := []string{want3.ID} // newest first
	if len(page.Items) != 4 {
		t.Fatalf("got %d items, want 4", len(page.Items))
	}
	if page.Items[0].ID != want[0] {
		t.Fatalf("newest item %q, want %q", page.Items[0].ID, want[0])
	}
	texts := []string{page.Items[0].Text, page.Items[1].Text, page.Items[2].Text, page.Items[3].Text}
	for i, wantText := range []string{"c3", "p2", "c1", "p0"} {
		if texts[i] != wantText {
			t.Fatalf("position %d: got %q want %q", i, texts[i], wantText)
		}
	}
}

func TestUnfanoutDeletePropagation(t *testing.T) {
	pool := testPool(t)
	rdb := testRedis(t)
	ctx := context.Background()
	svc := feedService(t, pool, rdb, 10000, 100)

	author, _ := feedUser(t, pool, "DelAuthor")
	follower, _ := feedUser(t, pool, "DelFollower")
	feedFollow(t, pool, follower, author)
	p := feedPost(t, pool, author, "doomed post")
	if err := svc.FanoutPost(ctx, p); err != nil {
		t.Fatalf("FanoutPost: %v", err)
	}
	page, _ := svc.HomeFeed(ctx, follower, 20, nil)
	if len(page.Items) != 1 {
		t.Fatalf("precondition: follower feed should have 1 item, has %d", len(page.Items))
	}

	if err := svc.UnfanoutPost(ctx, p.ID); err != nil {
		t.Fatalf("UnfanoutPost: %v", err)
	}
	if n, err := rdb.ZCard(ctx, homeKey(follower.String())).Result(); err != nil || n != 0 {
		t.Fatalf("timeline not cleaned: zcard=%d err=%v", n, err)
	}
	page, err := svc.HomeFeed(ctx, follower, 20, nil)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("feed after unfan-out: %+v err=%v", page, err)
	}

	// A post deleted from PostgreSQL WITHOUT unfan-out (crash window)
	// is dropped by the rehydration step: a hole, never a ghost post.
	p2 := feedPost(t, pool, author, "ghost candidate")
	if err := svc.FanoutPost(ctx, p2); err != nil {
		t.Fatalf("FanoutPost: %v", err)
	}
	var pid pgtype.UUID
	if err := pid.Scan(p2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM posts WHERE id = $1`, pid); err != nil {
		t.Fatalf("direct delete: %v", err)
	}
	page, err = svc.HomeFeed(ctx, follower, 20, nil)
	if err != nil {
		t.Fatalf("HomeFeed: %v", err)
	}
	for _, item := range page.Items {
		if item.ID == p2.ID {
			t.Fatal("ghost post: deleted post still served from the feed")
		}
	}
}

func TestHomeFeedPagination(t *testing.T) {
	pool := testPool(t)
	rdb := testRedis(t)
	ctx := context.Background()
	svc := feedService(t, pool, rdb, 10000, 100)

	reader, _ := feedUser(t, pool, "PageReader")
	author, _ := feedUser(t, pool, "PageAuthor")
	feedFollow(t, pool, reader, author)

	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	var ids []string
	for i := 0; i < 5; i++ {
		p := feedPost(t, pool, author, fmt.Sprintf("page post %d", i))
		ts := base.Add(time.Duration(i) * time.Second)
		setPostTime(t, pool, p, ts)
		if err := svc.FanoutPost(ctx, p); err != nil {
			t.Fatalf("FanoutPost: %v", err)
		}
		ids = append([]string{p.ID}, ids...) // newest first
	}

	seen := map[string]bool{}
	var cursor *string
	pages := 0
	for {
		var fc *FeedCursor
		if cursor != nil {
			var err error
			fc, err = DecodeFeedCursor(*cursor)
			if err != nil {
				t.Fatalf("decode cursor: %v", err)
			}
		}
		page, err := svc.HomeFeed(ctx, reader, 2, fc)
		if err != nil {
			t.Fatalf("HomeFeed page %d: %v", pages, err)
		}
		pages++
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatalf("duplicate post %s across pages", item.ID)
			}
			seen[item.ID] = true
		}
		if page.NextCursor == nil {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 3 {
		t.Fatalf("expected 3 pages of 2/2/1, got %d", pages)
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d distinct posts, want 5", len(seen))
	}
	// Newest-first across the whole traversal.
	var order []string
	cursor = nil
	for {
		var fc *FeedCursor
		if cursor != nil {
			var err error
			fc, err = DecodeFeedCursor(*cursor)
			if err != nil {
				t.Fatal(err)
			}
		}
		page, err := svc.HomeFeed(ctx, reader, 2, fc)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			order = append(order, item.ID)
		}
		if page.NextCursor == nil {
			break
		}
		cursor = page.NextCursor
	}
	for i := range ids {
		if order[i] != ids[i] {
			t.Fatalf("order[%d] = %s, want %s", i, order[i], ids[i])
		}
	}
}

func TestHomeFeedValkeyDown(t *testing.T) {
	pool := testPool(t)
	// Point at a port where nothing listens: every Valkey op fails fast.
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })
	svc := NewService(pool, rdb, 10000, 100)

	reader, _ := feedUser(t, pool, "DownReader")
	if _, err := svc.HomeFeed(context.Background(), reader, 20, nil); !errors.Is(err, ErrFeedUnavailable) {
		t.Fatalf("expected ErrFeedUnavailable, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Regression tests for cursor-path bugs found in review (2026-10-18).
// ---------------------------------------------------------------------------

func TestHomeFeedTiePagination(t *testing.T) {
	pool := testPool(t)
	rdb := testRedis(t)
	ctx := context.Background()
	svc := feedService(t, pool, rdb, 10000, 100)

	reader, _ := feedUser(t, pool, "TieReader")
	author, _ := feedUser(t, pool, "TieAuthor")
	feedFollow(t, pool, reader, author)

	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	// Three posts share the exact same timestamp. Within one score the
	// page order is member (post id) DESCENDING; fan-out happens after
	// pinning so the timeline scores are the pinned times.
	var tiedIDs []string
	for i := 0; i < 3; i++ {
		p := feedPost(t, pool, author, fmt.Sprintf("tied %d", i))
		setPostTime(t, pool, p, base)
		if err := svc.FanoutPost(ctx, p); err != nil {
			t.Fatalf("FanoutPost: %v", err)
		}
		tiedIDs = append(tiedIDs, p.ID)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(tiedIDs)))
	older := feedPost(t, pool, author, "older")
	setPostTime(t, pool, older, base.Add(-time.Second))
	if err := svc.FanoutPost(ctx, older); err != nil {
		t.Fatalf("FanoutPost older: %v", err)
	}
	want := append(append([]string{}, tiedIDs...), older.ID)

	// limit=1 forces every page boundary through the tie: the old
	// implementation emitted the same-score tail AFTER lower scores
	// (corrupting newest-first order) and then marked the stream done
	// with a tied entry still unfetched (dropping it entirely).
	var got []string
	var cursor *string
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		var fc *FeedCursor
		if cursor != nil {
			var err error
			fc, err = DecodeFeedCursor(*cursor)
			if err != nil {
				t.Fatalf("decode cursor: %v", err)
			}
		}
		page, err := svc.HomeFeed(ctx, reader, 1, fc)
		if err != nil {
			t.Fatalf("HomeFeed: %v", err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("limit=1 page has %d items", len(page.Items))
		}
		got = append(got, page.Items[0].ID)
		if page.NextCursor == nil {
			break
		}
		cursor = page.NextCursor
	}
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d: got %s want %s (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestHomeFeedStarvedStreamNotLost(t *testing.T) {
	pool := testPool(t)
	rdb := testRedis(t)
	ctx := context.Background()
	// Threshold 1: celeb (2 followers) is pull-path; normal author is
	// push-path.
	svc := feedService(t, pool, rdb, 1, 100)

	reader, _ := feedUser(t, pool, "StarveReader")
	author, _ := feedUser(t, pool, "StarveAuthor")
	celeb, _ := feedUser(t, pool, "StarveCeleb")
	other, _ := feedUser(t, pool, "StarveOther")
	feedFollow(t, pool, reader, author)
	feedFollow(t, pool, reader, celeb)
	feedFollow(t, pool, other, celeb)

	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	p1 := feedPost(t, pool, author, "pushed post")
	setPostTime(t, pool, p1, base)
	if err := svc.FanoutPost(ctx, p1); err != nil {
		t.Fatalf("FanoutPost: %v", err)
	}
	q1 := feedPost(t, pool, celeb, "celebrity post")
	setPostTime(t, pool, q1, base.Add(time.Minute))
	if err := svc.FanoutPost(ctx, q1); err != nil {
		t.Fatalf("FanoutPost celeb: %v", err)
	}

	// limit=1: page 1 takes the newer celebrity post from the pull
	// stream; the pushed stream is exhausted-but-starved (the merge took
	// nothing from it). It must NOT be marked done — the old code lost
	// the pushed post here.
	page1, err := svc.HomeFeed(ctx, reader, 1, nil)
	if err != nil {
		t.Fatalf("HomeFeed page 1: %v", err)
	}
	if len(page1.Items) != 1 || page1.Items[0].ID != q1.ID {
		t.Fatalf("page 1 wrong: %+v", page1.Items)
	}
	if page1.NextCursor == nil {
		t.Fatal("page 1 should carry a cursor (pushed post remains)")
	}
	fc, err := DecodeFeedCursor(*page1.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	page2, err := svc.HomeFeed(ctx, reader, 1, fc)
	if err != nil {
		t.Fatalf("HomeFeed page 2: %v", err)
	}
	if len(page2.Items) != 1 || page2.Items[0].ID != p1.ID {
		t.Fatalf("page 2 wrong (starved pushed post lost?): %+v", page2.Items)
	}
	if page2.NextCursor != nil {
		t.Fatal("expected end of feed, got a cursor")
	}
}
