// SPDX-License-Identifier: AGPL-3.0-or-later
// Command transcode-worker is the Sutra background video pipeline (D4b).
//
// It drains the Valkey stream media:transcode (fed by the API's media
// registry), downloads each source clip from SeaweedFS, produces a
// multi-rendition HLS package + poster frame with FFmpeg, uploads the
// outputs back, and flips the media row to ready (or failed after
// MaxAttempts, with the job parked on the media:transcode:dead stream).
//
// Like the API it refuses to boot without PostgreSQL and Valkey; unlike
// the API, SeaweedFS reachability is warn-only at startup (a dead volume
// server fails individual jobs, which is exactly what the retry and
// dead-letter machinery exists for).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mintzocloud-coder/sutra/services/api/internal/config"
	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/media"
	"github.com/mintzocloud-coder/sutra/services/api/internal/transcode"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	cfg := config.Load()

	startupCtx, startupCancel := context.WithTimeout(context.Background(), 60*time.Second)
	pool, err := db.Open(startupCtx, cfg.DatabaseURL, int32(cfg.DBMaxConns), log)
	if err != nil {
		startupCancel()
		log.Error("database unavailable at startup", "err", err)
		os.Exit(1)
	}
	if err := db.Migrate(startupCtx, pool); err != nil {
		startupCancel()
		log.Error("migrations failed", "err", err)
		os.Exit(1)
	}
	startupCancel()
	defer pool.Close()

	rdb := redis.NewClient(&redis.Options{Addr: cfg.ValkeyAddr})
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		pingCancel()
		log.Error("valkey unavailable at startup — refusing to start",
			"addr", cfg.ValkeyAddr, "err", err)
		os.Exit(1)
	}
	pingCancel()
	defer rdb.Close()
	log.Info("valkey connected", "addr", cfg.ValkeyAddr)

	mediaSvc := media.NewService(pool, rdb, cfg.SeaweedMasterAddr, cfg.MediaPublicBaseURL)
	if err := media.PingMaster(context.Background(), cfg.SeaweedMasterAddr); err != nil {
		log.Warn("seaweedfs master unreachable at startup — jobs will fail and retry until it recovers",
			"master", cfg.SeaweedMasterAddr, "err", err)
	} else {
		log.Info("seaweedfs master reachable", "master", cfg.SeaweedMasterAddr)
	}

	w := transcode.NewWorker(
		pool,
		rdb,
		mediaSvc,
		transcode.NewSeaweedStorage(cfg.SeaweedMasterAddr, cfg.MediaPublicBaseURL),
		&transcode.FFmpegTranscoder{},
		transcode.WorkerConfig{
			Group:         cfg.TranscodeGroup,
			Consumer:      cfg.TranscodeConsumer,
			MaxAttempts:   cfg.TranscodeMaxAttempts,
			BackoffBase:   cfg.TranscodeBackoffBase,
			BackoffMax:    cfg.TranscodeBackoffMax,
			SweepInterval: cfg.TranscodeSweepInterval,
			HeartbeatTTL:  20 * time.Second,
			PublicBaseURL: cfg.MediaPublicBaseURL,
		},
		log,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := w.Run(ctx); err != nil {
		log.Error("transcode worker exited with error", "err", err)
		os.Exit(1)
	}
	log.Info("transcode worker stopped")
}
