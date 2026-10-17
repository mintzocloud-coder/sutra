// SPDX-License-Identifier: AGPL-3.0-or-later
// Command api is the Sutra Go API service: core request-serving layer.
// Realtime state lives in the Elixir chat service; intelligence in Python.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mintzocloud-coder/sutra/services/api/internal/config"
	"github.com/mintzocloud-coder/sutra/services/api/internal/oidc"
)

func main() {
	cfg := config.Load()
	verifier := oidc.NewVerifier(cfg.HydraJWKSURL)

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		// TODO: check database + Valkey reachability.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/v1/me", func(w http.ResponseWriter, r *http.Request) {
		sub, err := verifier.Subject(r.Header.Get("Authorization"))
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sub":` + `"` + sub + `"}`))
	})

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: mux}

	go func() {
		log.Printf("api listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.ShutdownTimeout)*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
	log.Println("api stopped")
}
