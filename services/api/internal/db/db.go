// SPDX-License-Identifier: AGPL-3.0-or-later
// Package db owns the API service's PostgreSQL connection pool and the
// versioned migration runner.
//
// Migrations live in migrations/*.sql and are EMBEDDED in the binary
// (go:embed), so the container image is self-contained: a fresh
// `docker compose up` applies every pending migration before the service
// starts accepting traffic. Concurrent replicas serialize schema changes
// through a session-level advisory lock.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is the service-wide PostgreSQL connection pool with a bounded
// health probe for readiness checks.
type Pool struct {
	*pgxpool.Pool
	log *slog.Logger
}

// Open parses dsn, applies production pool tuning, and verifies
// connectivity with a bounded ping.
//
// It FAILS FAST when the database is unreachable: orchestrators (compose
// restart policies, Kubernetes) retry the container until Postgres is up,
// which is safer than serving traffic with a half-open dependency. The
// /readyz handler keeps returning 503 if the database drops later.
func Open(ctx context.Context, dsn string, maxConns int32, log *slog.Logger) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	// Production pool tuning. Connections are cheap to keep but expensive
	// to churn; these bounds keep the pool warm without starving Postgres.
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	// Log the host/database but NEVER the DSN: it carries the password.
	host, database := cfg.ConnConfig.Host, cfg.ConnConfig.Database

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database ping: %w", err)
	}

	log.Info("database connected", "host", host, "database", database,
		"max_conns", cfg.MaxConns)
	return &Pool{Pool: pool, log: log}, nil
}

// Ping is a bounded (2s) health probe, used by /readyz to fail fast when
// the database drops mid-run.
func (p *Pool) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return p.Pool.Ping(ctx)
}
