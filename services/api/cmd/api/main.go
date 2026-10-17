// SPDX-License-Identifier: AGPL-3.0-or-later
// Command api is the Sutra Go API service: core request-serving layer.
// Realtime state lives in the Elixir chat service; intelligence in Python.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mintzocloud-coder/sutra/services/api/internal/config"
	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/oidc"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

// newHandler builds the full HTTP serving stack (routes + middleware) so
// integration tests can exercise the exact same wiring as production.
func newHandler(cfg config.Config, pool *db.Pool, verifier *oidc.Verifier, log *slog.Logger) http.Handler {
	auth := middleware.RequireAuth(verifier, log, middleware.AuthOptions{
		Bypass:        cfg.AuthBypass,
		BypassSubject: cfg.BypassSubject,
	})

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		// Liveness: the process is alive. Never depends on the DB.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		// Readiness: fail when the database is unreachable so the
		// orchestrator stops routing traffic until it recovers.
		if err := pool.Ping(r.Context()); err != nil {
			log.Warn("readyz: database unreachable", "err", err,
				"request_id", middleware.FromContext(r.Context()))
			middleware.WriteStatusError(w, r, http.StatusServiceUnavailable,
				"not ready: database unreachable")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Protected API routes: every /v1/* handler runs behind RequireAuth,
	// so the verified subject is always in the request context. GET /v1/me
	// returns the caller's identity record, provisioning it on first sight
	// (see internal/users).
	//
	// Method routing is explicit (not Go 1.22 method patterns) so wrong
	// methods get the standard 405 envelope with an Allow header instead
	// of the mux's plain-text reply; the inner catch-all keeps unknown
	// /v1/* paths in the envelope too.
	v1 := http.NewServeMux()
	meHandler := users.MeHandler(pool, log)
	v1.Handle("/v1/me", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			middleware.MethodNotAllowedHandler(http.MethodGet)(w, r)
			return
		}
		meHandler(w, r)
	}))
	v1.Handle("/", middleware.NotFoundHandler())
	mux.Handle("/v1/", auth(v1))

	// Unknown paths (outside /healthz, /readyz, /v1/): 404 in the standard
	// envelope instead of Go's plain-text default.
	mux.Handle("/", middleware.NotFoundHandler())

	// Middleware order (outermost first): RequestID → Recover →
	// SecurityHeaders → CORS → RateLimit → Logger.
	// RequestID is outermost so every response — including recovered
	// panics and limiter 429s — carries the request ID. Recover sits next
	// so it also catches panics in the remaining middleware; it must stay
	// outside them. SecurityHeaders is outside everything that can produce
	// a response so all replies carry the baseline headers; CORS runs
	// before RateLimit so preflights aren't counted against the budget.
	limiter := middleware.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
	cors := middleware.CORS(middleware.CORSConfig{
		Origins: cfg.CORSOrigins,
		MaxAge:  cfg.CORSMaxAge,
	})
	return middleware.Chain(mux,
		middleware.RequestID,
		middleware.Recover(log),
		middleware.SecurityHeaders(),
		cors,
		middleware.RateLimit(limiter, log),
		middleware.Logger(log),
	)
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	cfg := config.Load()
	if cfg.AuthBypass {
		log.Warn("AUTH_BYPASS=1: JWT verification DISABLED — every request is "+
			"authenticated as the bypass subject. INSECURE: local development only, "+
			"never enable in production.",
			"bypass_subject", cfg.BypassSubject)
	}
	verifier := oidc.NewVerifier(cfg.HydraJWKSURL, cfg.HydraIssuer, cfg.JWTAudience)

	// Database: connect (fail fast if unreachable) then apply pending
	// migrations before serving any traffic.
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

	handler := newHandler(cfg, pool, verifier, log)
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: handler}

	go func() {
		log.Info("api listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.ShutdownTimeout)*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("shutdown failed", "err", err)
		os.Exit(1)
	}
	log.Info("api stopped")
}
