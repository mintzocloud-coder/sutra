// SPDX-License-Identifier: AGPL-3.0-or-later
package db

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
)

// testLogger discards logs during tests; failures surface via t.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Pure unit tests: discovery + validation of the embedded migrations.
// These never need a database.

func TestDiscoverMigrationsOrdered(t *testing.T) {
	migs, err := discoverMigrations()
	if err != nil {
		t.Fatalf("discoverMigrations: %v", err)
	}
	if len(migs) == 0 {
		t.Fatal("expected at least one embedded migration")
	}
	for i := 1; i < len(migs); i++ {
		if migs[i].Version <= migs[i-1].Version {
			t.Fatalf("migrations not strictly increasing: %d then %d",
				migs[i-1].Version, migs[i].Version)
		}
	}
	if migs[0].Version != 1 {
		t.Fatalf("first migration version = %d, want 1", migs[0].Version)
	}
}

// Integration: full Migrate cycle against a real PostgreSQL.
// Runs when TEST_DATABASE_URL is set; otherwise skips cleanly.
func TestMigrateIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping migration integration test")
	}
	log := testLogger(t)
	ctx := context.Background()

	pool, err := Open(ctx, dsn, 5, log)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	// First run: applies everything.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate (first): %v", err)
	}
	// Second run: idempotent, nothing pending, no error.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate (second): %v", err)
	}

	// Ledger check: migration 0001 recorded.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version = 1`).Scan(&n); err != nil {
		t.Fatalf("ledger query: %v", err)
	}
	if n != 1 {
		t.Fatalf("schema_migrations has %d rows for version 1, want 1", n)
	}

	// Ping probe behaves.
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
