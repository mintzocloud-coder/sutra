// SPDX-License-Identifier: AGPL-3.0-or-later
// Package transcode is the background video pipeline worker (D4 part 2).
//
// The API's media registry (internal/media) enqueues finished video uploads
// onto the Valkey stream media:transcode; this worker drains them and turns
// each source file into a multi-rendition HLS package + poster frame:
//
//  1. XREADGROUP on the stream (consumer group: exactly-once-ish routing to
//     N worker replicas; pending-message redelivery on crash).
//  2. Download source bytes from SeaweedFS.
//  3. ffprobe → pick the rendition ladder at or below source resolution
//     (never upscale).
//  4. ffmpeg 8.1 → per-rendition HLS playlists + segments + poster frame.
//  5. Upload outputs back to SeaweedFS; media.MarkReady (or MarkFailed on
//     give-up) flips the registry row.
//
// Failure handling: attempts are tracked in a Valkey hash. A failed job is
// re-queued with exponential backoff (sleep + re-XADD, attempts+1); after
// MaxAttempts it lands on the media:transcode:dead stream AND the row is
// parked failed via media.MarkFailed, so nothing fails silently. The worker
// also sweeps media rows stuck in status='uploaded' on startup and every
// SweepInterval (covers a lost XADD — the enqueue is best-effort).
//
// Liveness: the worker SETs worker:transcode:heartbeat (TTL'd) each loop;
// compose/k8s probe it. Graceful shutdown: Run returns when ctx is done,
// finishing the in-flight job first.
package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/media"
)

// Stream / key names. The input stream constant lives with the producer
// (media.TranscodeStream); the dead-letter stream and auxiliary keys are
// owned here.
const (
	// DeadLetterStream gets jobs that exhausted MaxAttempts.
	DeadLetterStream = "media:transcode:dead"
	// AttemptsHash tracks per-media-id attempt counts: H media:transcode:attempts <id> <n>.
	AttemptsHash = "media:transcode:attempts"
	// HeartbeatKey is TTL-refreshed by every worker loop; liveness probes read it.
	HeartbeatKey = "worker:transcode:heartbeat"
)

// RenditionSpec is one output rung of the HLS ladder.
type RenditionSpec struct {
	Name         string // e.g. "720p"
	Width        int
	Height       int
	VideoBitrate string // e.g. "2500k"
	AudioBitrate string // e.g. "128k"
	Bandwidth    int64  // bits/sec, advertised in the master playlist
}

// DefaultLadder is the transcode ladder: 720p / 480p / 360p. Rungs above
// the source resolution are skipped per job (never upscale); the lowest
// rung is always kept so even tiny sources get an HLS package.
var DefaultLadder = []RenditionSpec{
	{Name: "720p", Width: 1280, Height: 720, VideoBitrate: "2500k", AudioBitrate: "128k", Bandwidth: 2_800_000},
	{Name: "480p", Width: 854, Height: 480, VideoBitrate: "1200k", AudioBitrate: "128k", Bandwidth: 1_400_000},
	{Name: "360p", Width: 640, Height: 360, VideoBitrate: "800k", AudioBitrate: "96k", Bandwidth: 900_000},
}

// SelectLadder returns the rungs at or below the source resolution,
// always keeping the lowest rung as a fallback.
func SelectLadder(srcWidth int, ladder []RenditionSpec) []RenditionSpec {
	if len(ladder) == 0 {
		return nil
	}
	var out []RenditionSpec
	for _, r := range ladder {
		if r.Width <= srcWidth {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		out = append(out, ladder[len(ladder)-1])
	}
	return out
}

// ProbeInfo describes a source file.
type ProbeInfo struct {
	Width      int
	Height     int
	DurationMs int64
}

// Transcoder probes and transcodes. The FFmpeg implementation shells out
// to ffprobe/ffmpeg; tests substitute a fake.
type Transcoder interface {
	Probe(ctx context.Context, srcPath string) (ProbeInfo, error)
	// TranscodeHLS writes per-rendition HLS output into outDir
	// (<name>.m3u8 + <name>_NNN.ts) and a poster JPEG to posterPath.
	TranscodeHLS(ctx context.Context, srcPath, outDir, posterPath string, specs []RenditionSpec) error
}

// Storage moves bytes between the worker and SeaweedFS.
type Storage interface {
	Download(ctx context.Context, fid string, w io.Writer) error
	// Upload stores one file and returns its fid.
	Upload(ctx context.Context, filename, mime string, r io.Reader) (string, error)
}

// XMessage is one stream entry the worker reads.
type XMessage struct {
	ID     string
	Values map[string]string
}

// streamOps is the minimal Valkey surface the worker needs; the go-redis
// implementation lives below, fakes in tests above it.
type streamOps interface {
	EnsureGroup(ctx context.Context, stream, group string) error
	ReadGroup(ctx context.Context, stream, group, consumer string, block time.Duration) ([]XMessage, error)
	Ack(ctx context.Context, stream, group string, ids ...string) error
	Add(ctx context.Context, stream string, values map[string]string) (string, error)
	GetAttempts(ctx context.Context, mediaID string) (int, error)
	SetAttempts(ctx context.Context, mediaID string, n int) error
	ClearAttempts(ctx context.Context, mediaID string) error
	Heartbeat(ctx context.Context, ttl time.Duration) error
}

// redisStream implements streamOps with go-redis v9 (RESP — works against
// Valkey 8, which the compose stack runs).
type redisStream struct{ client *redis.Client }

func (r *redisStream) EnsureGroup(ctx context.Context, stream, group string) error {
	err := r.client.XGroupCreateMkStream(ctx, stream, group, "0").Err()
	if err != nil && strings.Contains(err.Error(), "BUSYGROUP") {
		return nil
	}
	return err
}

func (r *redisStream) ReadGroup(ctx context.Context, stream, group, consumer string, block time.Duration) ([]XMessage, error) {
	res, err := r.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{stream, ">"},
		Count:    1,
		Block:    block,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	var out []XMessage
	for _, s := range res {
		for _, m := range s.Messages {
			vals := make(map[string]string, len(m.Values))
			for k, v := range m.Values {
				vals[k] = fmt.Sprint(v)
			}
			out = append(out, XMessage{ID: m.ID, Values: vals})
		}
	}
	return out, nil
}

func (r *redisStream) Ack(ctx context.Context, stream, group string, ids ...string) error {
	return r.client.XAck(ctx, stream, group, ids...).Err()
}

func (r *redisStream) Add(ctx context.Context, stream string, values map[string]string) (string, error) {
	return r.client.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: values}).Result()
}

func (r *redisStream) GetAttempts(ctx context.Context, mediaID string) (int, error) {
	s, err := r.client.HGet(ctx, AttemptsHash, mediaID).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, nil // corrupt counter: treat as fresh, don't wedge the job
	}
	return n, nil
}

func (r *redisStream) SetAttempts(ctx context.Context, mediaID string, n int) error {
	return r.client.HSet(ctx, AttemptsHash, mediaID, n).Err()
}

func (r *redisStream) ClearAttempts(ctx context.Context, mediaID string) error {
	return r.client.HDel(ctx, AttemptsHash, mediaID).Err()
}

func (r *redisStream) Heartbeat(ctx context.Context, ttl time.Duration) error {
	return r.client.Set(ctx, HeartbeatKey, time.Now().UTC().Format(time.RFC3339), ttl).Err()
}

// WorkerConfig tunes the worker; all fields have sane defaults from env
// (see internal/config).
type WorkerConfig struct {
	Group         string
	Consumer      string
	MaxAttempts   int
	BackoffBase   time.Duration
	BackoffMax    time.Duration
	SweepInterval time.Duration
	HeartbeatTTL  time.Duration
	// PublicBaseURL rewrites playlist URLs (same value the API hands out).
	PublicBaseURL string
}

// Worker drains the transcode stream.
type Worker struct {
	pool       *db.Pool
	ops        streamOps
	mediaSvc   *media.Service
	storage    Storage
	transcoder Transcoder
	cfg        WorkerConfig
	log        *slog.Logger
}

// NewWorker wires a worker; ops is the Valkey stream layer.
func NewWorker(pool *db.Pool, rdb *redis.Client, mediaSvc *media.Service, storage Storage, transcoder Transcoder, cfg WorkerConfig, log *slog.Logger) *Worker {
	return &Worker{
		pool:       pool,
		ops:        &redisStream{client: rdb},
		mediaSvc:   mediaSvc,
		storage:    storage,
		transcoder: transcoder,
		cfg:        cfg,
		log:        log,
	}
}

// BackoffFor returns the delay before attempt n (1-based): base * 2^(n-1),
// capped at max. Pure, unit-tested.
func BackoffFor(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}

// Run is the worker main loop. It sweeps stale rows, then blocks on the
// stream until ctx is cancelled; the in-flight job finishes first.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.ops.EnsureGroup(ctx, media.TranscodeStream, w.cfg.Group); err != nil {
		return fmt.Errorf("transcode: ensure consumer group: %w", err)
	}
	w.log.Info("transcode worker started",
		"group", w.cfg.Group, "consumer", w.cfg.Consumer, "max_attempts", w.cfg.MaxAttempts)

	if err := w.sweep(ctx); err != nil {
		w.log.Warn("transcode sweep failed (will retry on next interval)", "err", err)
	}
	sweepTick := time.NewTicker(w.cfg.SweepInterval)
	defer sweepTick.Stop()

	for {
		// Liveness heartbeat every loop; TTL 3x the block window so a
		// stalled worker's key expires and the probe notices.
		if err := w.ops.Heartbeat(ctx, w.cfg.HeartbeatTTL); err != nil {
			w.log.Warn("transcode heartbeat failed", "err", err)
		}
		select {
		case <-ctx.Done():
			w.log.Info("transcode worker stopping")
			return nil
		case <-sweepTick.C:
			if err := w.sweep(ctx); err != nil {
				w.log.Warn("transcode sweep failed", "err", err)
			}
		default:
		}

		msgs, err := w.ops.ReadGroup(ctx, media.TranscodeStream, w.cfg.Group, w.cfg.Consumer, 5*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.log.Warn("transcode read failed", "err", err)
			continue
		}
		for _, m := range msgs {
			w.handleMessage(ctx, m)
		}
	}
}

// handleMessage processes one job: download → probe → transcode → upload
// → MarkReady, with backoff requeue or dead-letter on failure.
func (w *Worker) handleMessage(ctx context.Context, m XMessage) {
	mediaID := m.Values["media_id"]
	if mediaID == "" {
		w.log.Warn("transcode job missing media_id; acking", "stream_id", m.ID)
		_ = w.ops.Ack(ctx, media.TranscodeStream, w.cfg.Group, m.ID)
		return
	}
	log := w.log.With("media_id", mediaID)

	var id pgtype.UUID
	if err := id.Scan(mediaID); err != nil {
		log.Warn("transcode job has invalid media_id; acking", "err", err)
		_ = w.ops.Ack(ctx, media.TranscodeStream, w.cfg.Group, m.ID)
		return
	}

	cur, err := w.mediaSvc.Get(ctx, id)
	if err != nil {
		// Row gone (or DB blip): ack + move on. A transient DB error
		// surfaces again on the next sweep, so nothing is lost.
		log.Warn("transcode: media lookup failed; acking", "err", err)
		_ = w.ops.Ack(ctx, media.TranscodeStream, w.cfg.Group, m.ID)
		return
	}
	if cur.Status != media.StatusUploaded {
		// Already ready/failed (duplicate delivery): ack, nothing to do.
		_ = w.ops.Ack(ctx, media.TranscodeStream, w.cfg.Group, m.ID)
		_ = w.ops.ClearAttempts(ctx, mediaID)
		return
	}

	attempts, err := w.ops.GetAttempts(ctx, mediaID)
	if err != nil {
		log.Warn("transcode: attempts lookup failed; treating as fresh", "err", err)
		attempts = 0
	}

	if jerr := w.transcodeOne(ctx, log, id, mediaID, cur.Filename); jerr != nil {
		attempts++
		if attempts >= w.cfg.MaxAttempts {
			w.deadLetter(ctx, log, m, id, mediaID, attempts, jerr)
			return
		}
		_ = w.ops.SetAttempts(ctx, mediaID, attempts)
		backoff := BackoffFor(attempts, w.cfg.BackoffBase, w.cfg.BackoffMax)
		log.Warn("transcode job failed; requeueing with backoff",
			"attempt", attempts, "backoff", backoff, "err", jerr)
		// Requeue first, ack the original, then sleep: the job waits
		// out its backoff even if this worker is restarted (the next
		// read picks it up after the sleep elapses... note the sleep
		// below keeps THIS worker from hot-looping it; a replica could
		// pick it up early — acceptable, attempts are shared).
		_, _ = w.ops.Add(ctx, media.TranscodeStream, map[string]string{"media_id": mediaID})
		_ = w.ops.Ack(ctx, media.TranscodeStream, w.cfg.Group, m.ID)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		return
	}

	_ = w.ops.ClearAttempts(ctx, mediaID)
	_ = w.ops.Ack(ctx, media.TranscodeStream, w.cfg.Group, m.ID)
	log.Info("transcode job complete")
}

// deadLetter parks the job permanently: dead-letter stream + failed row.
func (w *Worker) deadLetter(ctx context.Context, log *slog.Logger, m XMessage, id pgtype.UUID, mediaID string, attempts int, jerr error) {
	reason := fmt.Sprintf("transcode failed after %d attempts: %v", attempts, jerr)
	if len(reason) > 1000 {
		reason = reason[:1000]
	}
	log.Error("transcode job exhausted attempts; dead-lettering", "attempts", attempts, "err", jerr)
	_, _ = w.ops.Add(ctx, DeadLetterStream, map[string]string{
		"media_id": mediaID,
		"attempts": strconv.Itoa(attempts),
		"error":    reason,
	})
	if err := w.mediaSvc.MarkFailed(ctx, id, reason); err != nil {
		log.Error("transcode: MarkFailed failed", "err", err)
	}
	_ = w.ops.ClearAttempts(ctx, mediaID)
	_ = w.ops.Ack(ctx, media.TranscodeStream, w.cfg.Group, m.ID)
}

// transcodeOne does the actual work for one media id.
func (w *Worker) transcodeOne(ctx context.Context, log *slog.Logger, id pgtype.UUID, mediaID, filename string) error {
	cur, err := w.mediaSvc.Get(ctx, id)
	if err != nil {
		return err
	}
	// The playback URL is publicBase/fid; the fid is internal — recover it
	// from the URL the registry built. (Registry never exposes raw fids.)
	fid := fidFromPlaybackURL(cur.PlaybackURL, w.cfg.PublicBaseURL)
	if fid == "" {
		return errors.New("transcode: cannot derive storage fid")
	}

	workDir, err := os.MkdirTemp("", "transcode-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	srcPath := filepath.Join(workDir, "source")
	f, err := os.Create(srcPath)
	if err != nil {
		return err
	}
	if err := w.storage.Download(ctx, fid, f); err != nil {
		f.Close()
		return fmt.Errorf("download: %w", err)
	}
	f.Close()
	log.Info("transcode: downloaded source", "fid", fid)

	probe, err := w.transcoder.Probe(ctx, srcPath)
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	specs := SelectLadder(probe.Width, DefaultLadder)
	log.Info("transcode: ladder selected",
		"src", fmt.Sprintf("%dx%d", probe.Width, probe.Height),
		"rungs", rungNames(specs))

	outDir := filepath.Join(workDir, "hls")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	posterPath := filepath.Join(workDir, "poster.jpg")
	if err := w.transcoder.TranscodeHLS(ctx, srcPath, outDir, posterPath, specs); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}

	// Upload: poster first (it becomes poster_fid), then the HLS tree.
	posterFID, err := w.uploadFile(ctx, posterPath, "poster.jpg", "image/jpeg")
	if err != nil {
		return fmt.Errorf("upload poster: %w", err)
	}
	renditions, err := w.uploadRenditions(ctx, outDir, specs)
	if err != nil {
		return fmt.Errorf("upload renditions: %w", err)
	}

	return w.mediaSvc.MarkReady(ctx, id, probe.Width, probe.Height, probe.DurationMs, posterFID, renditions)
}

// uploadRenditions uploads each variant playlist + its segments, then
// writes and uploads the master playlist referencing absolute URLs.
func (w *Worker) uploadRenditions(ctx context.Context, outDir string, specs []RenditionSpec) ([]media.Rendition, error) {
	var out []media.Rendition
	for _, spec := range specs {
		entries, err := filepath.Glob(filepath.Join(outDir, spec.Name+"*"))
		if err != nil {
			return nil, err
		}
		playlistURL := ""
		for _, e := range entries {
			base := filepath.Base(e)
			mime := "video/MP2T"
			if strings.HasSuffix(base, ".m3u8") {
				mime = "application/vnd.apple.mpegurl"
			}
			fid, err := w.uploadFile(ctx, e, base, mime)
			if err != nil {
				return nil, err
			}
			if strings.HasSuffix(base, ".m3u8") {
				playlistURL = w.cfg.PublicBaseURL + "/" + fid
			}
		}
		if playlistURL == "" {
			return nil, fmt.Errorf("no playlist produced for rung %s", spec.Name)
		}
		out = append(out, media.Rendition{
			Name:      spec.Name,
			URL:       playlistURL,
			Bandwidth: spec.Bandwidth,
			Width:     spec.Width,
			Height:    spec.Height,
		})
	}
	return out, nil
}

// uploadFile streams one local file to SeaweedFS via the Storage impl.
func (w *Worker) uploadFile(ctx context.Context, path, filename, mime string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return w.storage.Upload(ctx, filename, mime, f)
}

// sweep enqueues every row stuck in uploaded (missed XADD, crashed
// worker, deploy gap). Idempotent: handleMessage no-ops on rows that
// already left uploaded.
func (w *Worker) sweep(ctx context.Context) error {
	rows, err := w.pool.Query(ctx, `SELECT id FROM media WHERE status = 'uploaded'`)
	if err != nil {
		return err
	}
	var ids []pgtype.UUID
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		mid := id.String()
		if _, err := w.ops.Add(ctx, media.TranscodeStream, map[string]string{"media_id": mid}); err != nil {
			return err
		}
	}
	if len(ids) > 0 {
		w.log.Info("transcode sweep requeued rows", "count", len(ids))
	}
	return nil
}

// fidFromPlaybackURL recovers the storage fid from the registry-built
// playback URL (publicBase/fid). Empty string if it doesn't match.
func fidFromPlaybackURL(playbackURL, publicBase string) string {
	prefix := strings.TrimSuffix(publicBase, "/") + "/"
	if !strings.HasPrefix(playbackURL, prefix) {
		return ""
	}
	fid := strings.TrimPrefix(playbackURL, prefix)
	if fid == "" || strings.Contains(fid, "/") || strings.Contains(fid, "?") {
		return ""
	}
	return fid
}

func rungNames(specs []RenditionSpec) string {
	var names []string
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return strings.Join(names, ",")
}

// ---------------------------------------------------------------------------
// FFmpeg implementation
// ---------------------------------------------------------------------------

// FFmpegTranscoder shells out to ffprobe/ffmpeg binaries on PATH.
// Requires FFmpeg 8.1+ (h264/aac HLS output); compose pins the image.
type FFmpegTranscoder struct {
	FFprobePath string // default "ffprobe"
	FFmpegPath  string // default "ffmpeg"
}

func (t *FFmpegTranscoder) ffprobe() string {
	if t.FFprobePath != "" {
		return t.FFprobePath
	}
	return "ffprobe"
}

func (t *FFmpegTranscoder) ffmpeg() string {
	if t.FFmpegPath != "" {
		return t.FFmpegPath
	}
	return "ffmpeg"
}

type probeJSON struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// Probe reads width/height/duration via ffprobe JSON.
func (t *FFmpegTranscoder) Probe(ctx context.Context, srcPath string) (ProbeInfo, error) {
	cmd := exec.CommandContext(ctx, t.ffprobe(),
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height:format=duration",
		"-of", "json", srcPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return ProbeInfo{}, fmt.Errorf("ffprobe: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var pj probeJSON
	if err := json.Unmarshal(stdout.Bytes(), &pj); err != nil {
		return ProbeInfo{}, fmt.Errorf("ffprobe: decode: %w", err)
	}
	if len(pj.Streams) == 0 || pj.Streams[0].Width == 0 {
		return ProbeInfo{}, errors.New("ffprobe: no video stream found")
	}
	dur, _ := strconv.ParseFloat(pj.Format.Duration, 64)
	return ProbeInfo{
		Width:      pj.Streams[0].Width,
		Height:     pj.Streams[0].Height,
		DurationMs: int64(dur * 1000),
	}, nil
}

// TranscodeHLS encodes each rung to HLS (4s segments, VOD playlists) and
// extracts a poster frame at t=1s (or t=0 for very short clips).
func (t *FFmpegTranscoder) TranscodeHLS(ctx context.Context, srcPath, outDir, posterPath string, specs []RenditionSpec) error {
	for _, spec := range specs {
		playlist := filepath.Join(outDir, spec.Name+".m3u8")
		segPat := filepath.Join(outDir, spec.Name+"_%03d.ts")
		cmd := exec.CommandContext(ctx, t.ffmpeg(), "-y",
			"-i", srcPath,
			"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2", spec.Width, spec.Height),
			"-c:v", "libx264", "-preset", "veryfast",
			"-b:v", spec.VideoBitrate, "-maxrate", spec.VideoBitrate, "-bufsize", spec.VideoBitrate,
			"-c:a", "aac", "-b:a", spec.AudioBitrate, "-ac", "2",
			"-hls_time", "4", "-hls_playlist_type", "vod",
			"-hls_segment_filename", segPat,
			playlist)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ffmpeg rung %s: %v: %s", spec.Name, err, tailLines(stderr.String(), 5))
		}
	}

	// Poster: 1s in, falling back to the first frame for sub-second clips.
	cmd := exec.CommandContext(ctx, t.ffmpeg(), "-y",
		"-ss", "1", "-i", srcPath,
		"-vframes", "1", "-q:v", "2", posterPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		cmd2 := exec.CommandContext(ctx, t.ffmpeg(), "-y",
			"-i", srcPath, "-vframes", "1", "-q:v", "2", posterPath)
		var stderr2 bytes.Buffer
		cmd2.Stderr = &stderr2
		if err2 := cmd2.Run(); err2 != nil {
			return fmt.Errorf("ffmpeg poster: %v: %s", err2, tailLines(stderr2.String(), 5))
		}
	}
	return nil
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// SeaweedFS storage implementation
// ---------------------------------------------------------------------------

// SeaweedStorage implements Storage against SeaweedFS: downloads via the
// public volume base URL, uploads via master dir/assign + direct PUT.
type SeaweedStorage struct {
	masterAddr    string // host:port, e.g. seaweedfs:9333
	publicBaseURL string // e.g. http://seaweedfs:8080
	http          *http.Client
}

// NewSeaweedStorage builds the storage client.
func NewSeaweedStorage(masterAddr, publicBaseURL string) *SeaweedStorage {
	return &SeaweedStorage{
		masterAddr:    strings.TrimSuffix(masterAddr, "/"),
		publicBaseURL: strings.TrimSuffix(publicBaseURL, "/"),
		http:          &http.Client{Timeout: 5 * time.Minute},
	}
}

// Download GETs the fid into w.
func (s *SeaweedStorage) Download(ctx context.Context, fid string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.publicBaseURL+"/"+fid, nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("seaweedfs download %s: status %d", fid, resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

type seaweedAssign struct {
	Fid string `json:"fid"`
	URL string `json:"url"`
}

// Upload assigns a fid from the master and PUTs the bytes to it.
func (s *SeaweedStorage) Upload(ctx context.Context, filename, mime string, r io.Reader) (string, error) {
	areq, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.masterAddr+"/dir/assign", nil)
	if err != nil {
		return "", err
	}
	aresp, err := s.http.Do(areq)
	if err != nil {
		return "", fmt.Errorf("seaweedfs dir/assign: %w", err)
	}
	defer aresp.Body.Close()
	if aresp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("seaweedfs dir/assign: status %d", aresp.StatusCode)
	}
	var ar seaweedAssign
	if err := json.NewDecoder(aresp.Body).Decode(&ar); err != nil {
		return "", fmt.Errorf("seaweedfs dir/assign decode: %w", err)
	}
	if ar.Fid == "" || ar.URL == "" {
		return "", errors.New("seaweedfs dir/assign: empty fid/url")
	}

	preq, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+ar.URL+"/"+ar.Fid, r)
	if err != nil {
		return "", err
	}
	preq.Header.Set("Content-Type", mime)
	presp, err := s.http.Do(preq)
	if err != nil {
		return "", fmt.Errorf("seaweedfs PUT %s: %w", ar.Fid, err)
	}
	defer presp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(presp.Body, 1024))
	if presp.StatusCode != http.StatusCreated && presp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("seaweedfs PUT %s: status %d", ar.Fid, presp.StatusCode)
	}
	return ar.Fid, nil
}
