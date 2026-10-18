// SPDX-License-Identifier: AGPL-3.0-or-later
package transcode

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/media"
)

// ---------------------------------------------------------------------------
// Pure unit tests (no DB, no Redis)
// ---------------------------------------------------------------------------

func TestBackoffFor(t *testing.T) {
	base, max := 30*time.Second, 30*time.Minute
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 30 * time.Second}, // clamped to 1
		{1, 30 * time.Second},
		{2, 60 * time.Second},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{10, 30 * time.Minute}, // capped
		{100, 30 * time.Minute},
	}
	for _, c := range cases {
		if got := BackoffFor(c.attempt, base, max); got != c.want {
			t.Errorf("BackoffFor(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

func TestSelectLadder(t *testing.T) {
	names := func(ss []RenditionSpec) string {
		var n []string
		for _, s := range ss {
			n = append(n, s.Name)
		}
		return strings.Join(n, ",")
	}
	if got := names(SelectLadder(1920, DefaultLadder)); got != "720p,480p,360p" {
		t.Errorf("1920 → %s", got)
	}
	if got := names(SelectLadder(1280, DefaultLadder)); got != "720p,480p,360p" {
		t.Errorf("1280 → %s", got)
	}
	if got := names(SelectLadder(900, DefaultLadder)); got != "480p,360p" {
		t.Errorf("900 → %s", got)
	}
	if got := names(SelectLadder(100, DefaultLadder)); got != "360p" {
		t.Errorf("100 → %s (must keep lowest rung, never upscale)", got)
	}
	if got := SelectLadder(1920, nil); got != nil {
		t.Errorf("empty ladder → %v, want nil", got)
	}
}

func TestFidFromPlaybackURL(t *testing.T) {
	base := "http://public.test"
	if got := fidFromPlaybackURL(base+"/7,deadbeef", base); got != "7,deadbeef" {
		t.Errorf("valid → %q", got)
	}
	if got := fidFromPlaybackURL("http://other.test/7,deadbeef", base); got != "" {
		t.Errorf("wrong base → %q, want empty", got)
	}
	if got := fidFromPlaybackURL(base+"/", base); got != "" {
		t.Errorf("empty fid → %q, want empty", got)
	}
	if got := fidFromPlaybackURL(base+"/7,deadbeef?x=1", base); got != "" {
		t.Errorf("query string → %q, want empty", got)
	}
	if got := fidFromPlaybackURL(base+"/a/b", base); got != "" {
		t.Errorf("slash in fid → %q, want empty", got)
	}
}

// fakeOps is an in-memory streamOps for unit + integration tests.
type fakeOps struct {
	attempts map[string]int
	acked    []string
	added    []struct {
		stream string
		values map[string]string
	}
	heartbeats int
}

func newFakeOps() *fakeOps { return &fakeOps{attempts: map[string]int{}} }

func (f *fakeOps) EnsureGroup(ctx context.Context, stream, group string) error { return nil }
func (f *fakeOps) ReadGroup(ctx context.Context, stream, group, consumer string, block time.Duration) ([]XMessage, error) {
	return nil, nil
}
func (f *fakeOps) Ack(ctx context.Context, stream, group string, ids ...string) error {
	f.acked = append(f.acked, ids...)
	return nil
}
func (f *fakeOps) Add(ctx context.Context, stream string, values map[string]string) (string, error) {
	f.added = append(f.added, struct {
		stream string
		values map[string]string
	}{stream, values})
	return "0-1", nil
}
func (f *fakeOps) GetAttempts(ctx context.Context, mediaID string) (int, error) {
	return f.attempts[mediaID], nil
}
func (f *fakeOps) SetAttempts(ctx context.Context, mediaID string, n int) error {
	f.attempts[mediaID] = n
	return nil
}
func (f *fakeOps) ClearAttempts(ctx context.Context, mediaID string) error {
	delete(f.attempts, mediaID)
	return nil
}
func (f *fakeOps) Heartbeat(ctx context.Context, ttl time.Duration) error {
	f.heartbeats++
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testWorkerCfg() WorkerConfig {
	return WorkerConfig{
		Group: "test-group", Consumer: "test-consumer",
		MaxAttempts: 3, BackoffBase: time.Millisecond, BackoffMax: time.Millisecond,
		SweepInterval: time.Hour, HeartbeatTTL: time.Minute,
		PublicBaseURL: "http://public.test",
	}
}

func TestHandleMessageBadInputAcks(t *testing.T) {
	ctx := context.Background()
	fops := newFakeOps()
	w := &Worker{ops: fops, cfg: testWorkerCfg(), log: testLogger()}

	// Missing media_id → ack, no requeue.
	w.handleMessage(ctx, XMessage{ID: "1-0", Values: map[string]string{}})
	if len(fops.acked) != 1 || fops.acked[0] != "1-0" {
		t.Fatalf("missing media_id: acked=%v", fops.acked)
	}
	if len(fops.added) != 0 {
		t.Fatalf("missing media_id: must not requeue, added=%v", fops.added)
	}

	// Invalid UUID → ack, no requeue, no DB touch (mediaSvc is nil here).
	w.handleMessage(ctx, XMessage{ID: "2-0", Values: map[string]string{"media_id": "not-a-uuid"}})
	if len(fops.acked) != 2 {
		t.Fatalf("invalid uuid: acked=%v", fops.acked)
	}
}

// ---------------------------------------------------------------------------
// Integration tests (real PostgreSQL; stream layer faked)
// ---------------------------------------------------------------------------

type fakeTranscoder struct {
	probe ProbeInfo
	err   error
}

func (f *fakeTranscoder) Probe(ctx context.Context, srcPath string) (ProbeInfo, error) {
	if f.err != nil {
		return ProbeInfo{}, f.err
	}
	return f.probe, nil
}

func (f *fakeTranscoder) TranscodeHLS(ctx context.Context, srcPath, outDir, posterPath string, specs []RenditionSpec) error {
	if f.err != nil {
		return f.err
	}
	for _, s := range specs {
		if err := os.WriteFile(filepath.Join(outDir, s.Name+".m3u8"),
			[]byte("#EXTM3U\n#EXT-X-VERSION:3\n"), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outDir, s.Name+"_000.ts"),
			[]byte("fake-segment"), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(posterPath, []byte("fake-jpeg"), 0o644)
}

type fakeStorage struct {
	uploaded map[string]string // filename → fid
}

func newFakeStorage() *fakeStorage { return &fakeStorage{uploaded: map[string]string{}} }

func (f *fakeStorage) Download(ctx context.Context, fid string, w io.Writer) error {
	_, err := w.Write([]byte("fake-video-bytes"))
	return err
}

func (f *fakeStorage) Upload(ctx context.Context, filename, mime string, r io.Reader) (string, error) {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return "", err
	}
	fid := "9,up-" + filename
	f.uploaded[filename] = fid
	return fid, nil
}

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping transcode integration test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn, 5, testLogger())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedUploadedMedia inserts a user + an uploaded video row; returns its id.
func seedUploadedMedia(t *testing.T, ctx context.Context, pool *db.Pool) pgtype.UUID {
	t.Helper()
	var userID pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (subject) VALUES ('transcode-test-subject') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var id pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO media (owner_id, kind, status, fid, filename, mime_type)
		 VALUES ($1, 'video', 'uploaded', '7,deadbeef', 'clip.mp4', 'video/mp4')
		 RETURNING id`, userID).Scan(&id); err != nil {
		t.Fatalf("seed media: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})
	return id
}

func TestHandleMessageDeadLetter(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	id := seedUploadedMedia(t, ctx, pool)

	fops := newFakeOps()
	fops.attempts[id.String()] = 2 // next failure is the 3rd = MaxAttempts
	w := &Worker{
		ops:        fops,
		mediaSvc:   media.NewService(pool, nil, "seaweedfs:9333", "http://public.test"),
		storage:    newFakeStorage(),
		transcoder: &fakeTranscoder{err: errors.New("boom")},
		cfg:        testWorkerCfg(),
		log:        testLogger(),
	}

	w.handleMessage(ctx, XMessage{ID: "5-0", Values: map[string]string{"media_id": id.String()}})

	// Row parked failed with the reason.
	m, err := w.mediaSvc.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Status != media.StatusFailed {
		t.Errorf("status = %s, want failed", m.Status)
	}
	if !strings.Contains(m.Error, "3 attempts") {
		t.Errorf("error = %q, want attempt count", m.Error)
	}

	// Dead-letter stream got the job.
	found := false
	for _, a := range fops.added {
		if a.stream == DeadLetterStream && a.values["media_id"] == id.String() {
			found = true
			if a.values["attempts"] != "3" {
				t.Errorf("dead-letter attempts = %s, want 3", a.values["attempts"])
			}
		}
	}
	if !found {
		t.Errorf("no dead-letter entry; added=%v", fops.added)
	}

	// Original acked, attempts counter cleared.
	if len(fops.acked) != 1 || fops.acked[0] != "5-0" {
		t.Errorf("acked = %v", fops.acked)
	}
	if _, ok := fops.attempts[id.String()]; ok {
		t.Errorf("attempts counter not cleared: %v", fops.attempts)
	}
}

func TestHandleMessageSuccess(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	id := seedUploadedMedia(t, ctx, pool)

	fops := newFakeOps()
	fstor := newFakeStorage()
	w := &Worker{
		ops:      fops,
		mediaSvc: media.NewService(pool, nil, "seaweedfs:9333", "http://public.test"),
		storage:  fstor,
		transcoder: &fakeTranscoder{
			probe: ProbeInfo{Width: 1280, Height: 720, DurationMs: 60000},
		},
		cfg: testWorkerCfg(),
		log: testLogger(),
	}

	w.handleMessage(ctx, XMessage{ID: "6-0", Values: map[string]string{"media_id": id.String()}})

	m, err := w.mediaSvc.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Status != media.StatusReady {
		t.Fatalf("status = %s (%s), want ready", m.Status, m.Error)
	}
	if len(m.Renditions) != 3 {
		t.Fatalf("renditions = %d, want 3 (1280px source)", len(m.Renditions))
	}
	if m.Renditions[0].Name != "720p" || m.Renditions[2].Name != "360p" {
		t.Errorf("rendition order/names = %v", m.Renditions)
	}
	for _, r := range m.Renditions {
		if !strings.HasPrefix(r.URL, "http://public.test/") || !strings.HasSuffix(r.URL, ".m3u8") {
			t.Errorf("rendition URL = %q", r.URL)
		}
	}
	if m.PosterURL == "" {
		t.Error("poster URL empty")
	}
	if m.Width == nil || *m.Width != 1280 || m.DurationMs == nil || *m.DurationMs != 60000 {
		t.Errorf("dimensions/duration not recorded: %+v", m)
	}
	// Poster + 3 playlists + 3 segments uploaded.
	if len(fstor.uploaded) != 1+3+3 {
		t.Errorf("uploaded files = %d, want 7 (poster + 3 playlists + 3 segments)", len(fstor.uploaded))
	}
	if len(fops.acked) != 1 {
		t.Errorf("acked = %v, want the original message", fops.acked)
	}
}

func TestHandleMessageAlreadyDoneSkips(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	id := seedUploadedMedia(t, ctx, pool)
	// Flip to ready: a duplicate delivery must be a no-op.
	if _, err := pool.Exec(ctx, `UPDATE media SET status='ready' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}

	fops := newFakeOps()
	w := &Worker{
		ops:        fops,
		mediaSvc:   media.NewService(pool, nil, "seaweedfs:9333", "http://public.test"),
		storage:    newFakeStorage(),
		transcoder: &fakeTranscoder{err: errors.New("must not run")},
		cfg:        testWorkerCfg(),
		log:        testLogger(),
	}
	w.handleMessage(ctx, XMessage{ID: "7-0", Values: map[string]string{"media_id": id.String()}})

	if len(fops.acked) != 1 {
		t.Errorf("duplicate delivery must be acked: %v", fops.acked)
	}
	if len(fops.added) != 0 {
		t.Errorf("duplicate delivery must not requeue: %v", fops.added)
	}
}

var _ = redis.Nil // keep go-redis import if the fake ever drops it
