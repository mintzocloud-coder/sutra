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
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/oidc"
)

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
	auth := middleware.RequireAuth(verifier, log, middleware.AuthOptions{
		Bypass:        cfg.AuthBypass,
		BypassSubject: cfg.BypassSubject,
	})

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		// TODO(A3): check database + Valkey reachability.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Protected API routes: every /v1/* handler runs behind RequireAuth,
	// so the verified subject is always in the request context.
	v1 := http.NewServeMux()
	v1.HandleFunc("/v1/me", func(w http.ResponseWriter, r *http.Request) {
		sub := middleware.SubjectFromContext(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sub":` + `"` + sub + `"}`))
	})
	mux.Handle("/v1/", auth(v1))

	// Middleware order (outermost first): Recover → RequestID → Logger.
	// Recover must be outermost so it also catches panics in the other
	// middleware; RequestID must precede Logger so the ID is in the context.
	handler := middleware.Chain(mux,
		middleware.Recover(log),
		middleware.RequestID,
		middleware.Logger(log),
	)

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
