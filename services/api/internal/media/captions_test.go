// SPDX-License-Identifier: AGPL-3.0-or-later
package media

// Integration tests for the E3 caption request surface: RequestCaptions,
// EnqueueCaptionsIfRequested, and the caption fields on Get. Run when
// TEST_DATABASE_URL is set (same convention as internal/posts); skipped
// cleanly otherwise. Redis runs locally on 6399 for the enqueue
// assertions (the stream XADDs are best-effort, so the service also
// works with rdb == nil — these tests use a real client to prove the
// happy path).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping media captions integration test")
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

func testRdb(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6399"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("local redis on 6399 unavailable: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })
	// Isolate this test's stream view: read only entries added after
	// the test starts.
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), CaptionStream).Err()
	})
	return rdb
}

func uniqueSubject(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "media-cap-test-" + hex.EncodeToString(b[:])
}

func provisionUser(t *testing.T, pool *db.Pool) pgtype.UUID {
	t.Helper()
	u, err := users.Provision(context.Background(), pool, uniqueSubject(t), "Caption Tester")
	if err != nil {
		t.Fatalf("users.Provision: %v", err)
	}
	id, err := social.ParseUserID(u.ID)
	if err != nil {
		t.Fatalf("ParseUserID(provisioned): %v", err)
	}
	return id
}

// mintMedia inserts a media row directly (bytes live on SeaweedFS;
// the registry just needs the row for these tests).
func mintMedia(t *testing.T, pool *db.Pool, owner pgtype.UUID, kind, status string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		INSERT INTO media (owner_id, kind, fid, status)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		owner, kind, "3,captest"+uniqueSubject(t), status).Scan(&id)
	if err != nil {
		t.Fatalf("mint media: %v", err)
	}
	return id
}

func streamLen(t *testing.T, rdb *redis.Client) int64 {
	t.Helper()
	n, err := rdb.XLen(context.Background(), CaptionStream).Result()
	if err != nil {
		t.Fatalf("XLen: %v", err)
	}
	return n
}

func rowState(t *testing.T, pool *db.Pool, id pgtype.UUID) (bool, string) {
	t.Helper()
	var auto bool
	var status string
	err := pool.QueryRow(context.Background(),
		`SELECT auto_caption, caption_status FROM media WHERE id = $1`, id).
		Scan(&auto, &status)
	if err != nil {
		t.Fatalf("rowState: %v", err)
	}
	return auto, status
}

func TestRequestCaptionsReadyEnqueues(t *testing.T) {
	pool := testPool(t)
	rdb := testRdb(t)
	ctx := context.Background()
	svc := NewService(pool, rdb, "master:9333", "http://public:8080")
	owner := provisionUser(t, pool)
	mid := mintMedia(t, pool, owner, KindVideo, StatusReady)

	if err := svc.RequestCaptions(ctx, mid, owner); err != nil {
		t.Fatalf("RequestCaptions: %v", err)
	}
	auto, status := rowState(t, pool, mid)
	if !auto || status != CaptionRequested {
		t.Fatalf("after request: auto=%v status=%q, want true/requested", auto, status)
	}
	if n := streamLen(t, rdb); n != 1 {
		t.Fatalf("stream entries = %d, want 1", n)
	}

	// Idempotent retry: no second flip, no second stream entry.
	if err := svc.RequestCaptions(ctx, mid, owner); err != nil {
		t.Fatalf("RequestCaptions retry: %v", err)
	}
	if n := streamLen(t, rdb); n != 1 {
		t.Fatalf("after retry: stream entries = %d, want 1", n)
	}
}

func TestRequestCaptionsUploadedWaitsForReady(t *testing.T) {
	pool := testPool(t)
	rdb := testRdb(t)
	ctx := context.Background()
	svc := NewService(pool, rdb, "master:9333", "http://public:8080")
	owner := provisionUser(t, pool)
	mid := mintMedia(t, pool, owner, KindVideo, StatusUploaded)

	if err := svc.RequestCaptions(ctx, mid, owner); err != nil {
		t.Fatalf("RequestCaptions: %v", err)
	}
	auto, status := rowState(t, pool, mid)
	if !auto || status != CaptionRequested {
		t.Fatalf("after request: auto=%v status=%q, want true/requested", auto, status)
	}
	// Not ready yet → no stream entry; the transcode worker enqueues
	// at MarkReady time instead.
	if n := streamLen(t, rdb); n != 0 {
		t.Fatalf("stream entries = %d, want 0 (not ready yet)", n)
	}
	// The worker's handoff is a no-op while still uploaded...
	if err := svc.EnqueueCaptionsIfRequested(ctx, mid); err != nil {
		t.Fatalf("EnqueueCaptionsIfRequested (uploaded): %v", err)
	}
	if n := streamLen(t, rdb); n != 0 {
		t.Fatalf("stream entries = %d, want 0", n)
	}
	// ...and enqueues once the video is ready.
	if _, err := pool.Exec(ctx, `UPDATE media SET status='ready' WHERE id=$1`, mid); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if err := svc.EnqueueCaptionsIfRequested(ctx, mid); err != nil {
		t.Fatalf("EnqueueCaptionsIfRequested (ready): %v", err)
	}
	if n := streamLen(t, rdb); n != 1 {
		t.Fatalf("stream entries = %d, want 1", n)
	}
}

func TestEnqueueCaptionsIfRequestedNoOptIn(t *testing.T) {
	pool := testPool(t)
	rdb := testRdb(t)
	ctx := context.Background()
	svc := NewService(pool, rdb, "master:9333", "http://public:8080")
	owner := provisionUser(t, pool)
	mid := mintMedia(t, pool, owner, KindVideo, StatusReady)

	if err := svc.EnqueueCaptionsIfRequested(ctx, mid); err != nil {
		t.Fatalf("EnqueueCaptionsIfRequested: %v", err)
	}
	if n := streamLen(t, rdb); n != 0 {
		t.Fatalf("stream entries = %d, want 0 (no opt-in)", n)
	}
	auto, status := rowState(t, pool, mid)
	if auto || status != CaptionNone {
		t.Fatalf("row changed without opt-in: auto=%v status=%q", auto, status)
	}
}

func TestRequestCaptionsRejects(t *testing.T) {
	pool := testPool(t)
	rdb := testRdb(t)
	ctx := context.Background()
	svc := NewService(pool, rdb, "master:9333", "http://public:8080")
	owner := provisionUser(t, pool)
	other := provisionUser(t, pool)

	img := mintMedia(t, pool, owner, KindImage, StatusReady)
	if err := svc.RequestCaptions(ctx, img, owner); err != ErrCaptionsVideoOnly {
		t.Fatalf("image: err = %v, want ErrCaptionsVideoOnly", err)
	}
	vid := mintMedia(t, pool, owner, KindVideo, StatusReady)
	if err := svc.RequestCaptions(ctx, vid, other); err != ErrMediaNotOwned {
		t.Fatalf("foreign: err = %v, want ErrMediaNotOwned", err)
	}
	var ghost pgtype.UUID
	if err := ghost.Scan("00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatalf("uuid scan: %v", err)
	}
	if err := svc.RequestCaptions(ctx, ghost, owner); err != ErrMediaNotFound {
		t.Fatalf("unknown: err = %v, want ErrMediaNotFound", err)
	}
}

func TestGetExposesCaptionFields(t *testing.T) {
	pool := testPool(t)
	rdb := testRdb(t)
	ctx := context.Background()
	svc := NewService(pool, rdb, "master:9333", "http://public:8080")
	owner := provisionUser(t, pool)
	mid := mintMedia(t, pool, owner, KindVideo, StatusReady)

	m, err := svc.Get(ctx, mid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.CaptionStatus != CaptionNone || m.AutoCaption || m.CaptionURL != "" {
		t.Fatalf("fresh row captions = %+v, want none/false/empty", m)
	}
	if err := svc.RequestCaptions(ctx, mid, owner); err != nil {
		t.Fatalf("RequestCaptions: %v", err)
	}
	m, err = svc.Get(ctx, mid)
	if err != nil {
		t.Fatalf("Get after request: %v", err)
	}
	if m.CaptionStatus != CaptionRequested || !m.AutoCaption {
		t.Fatalf("after request: status=%q auto=%v", m.CaptionStatus, m.AutoCaption)
	}
}
