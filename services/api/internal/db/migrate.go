// SPDX-License-Identifier: AGPL-3.0-or-later
// Migration runner: discovers versioned *.sql files embedded in the
// binary, applies pending ones in order, each in its own transaction,
// guarded by a session-level advisory lock.
//
// Naming: NNNN_name.sql (e.g. 0001_extensions.sql). Versions must be
// unique positive integers; gaps are fine (later tasks append higher
// numbers). Migrations are FORWARD-ONLY — never edit an applied file;
// add a new one.
package db

import (
	"context"
	"embed"
	"fmt"
	"hash/crc32"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one versioned schema change.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// migrationLockKey serializes concurrent migrators (N API replicas racing
// at startup) via pg_advisory_lock. Stable across deploys: crc32 of the
// migration namespace.
var migrationLockKey = int64(crc32.ChecksumIEEE([]byte("sutra/api/migrations")))

// discoverMigrations reads the embedded SQL files, validates naming, and
// returns them ordered by version. Validation failures are hard errors:
// a misnamed migration must never silently skip.
func discoverMigrations() ([]Migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".sql")
		// Split "<version>_<name>": version is the leading digit run.
		i := 0
		for i < len(base) && base[i] >= '0' && base[i] <= '9' {
			i++
		}
		if i == 0 || i >= len(base) || base[i] != '_' {
			return nil, fmt.Errorf("migration %q: name must be <version>_<name>.sql", e.Name())
		}
		version, err := strconv.Atoi(base[:i])
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %q: version must be a positive integer", e.Name())
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d (%q and %q)", version, prev, e.Name())
		}
		body, err := migrationFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), err)
		}
		if strings.TrimSpace(string(body)) == "" {
			return nil, fmt.Errorf("migration %q: empty file", e.Name())
		}
		seen[version] = e.Name()
		out = append(out, Migration{Version: version, Name: base[i+1:], SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Migrate applies every pending migration in version order. It is safe to
// run concurrently from multiple replicas: a session-level advisory lock
// makes all but one waiter, and already-applied versions are skipped via
// the schema_migrations ledger.
func Migrate(ctx context.Context, pool *Pool) error {
	migrations, err := discoverMigrations()
	if err != nil {
		return err
	}

	// Dedicated connection: the advisory lock is session-scoped and must
	// live as long as the migration run.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	// Unlock with a fresh context: ctx may be cancelled during shutdown.
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockKey) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate schema_migrations: %w", err)
	}

	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		if err := applyMigration(ctx, conn, m); err != nil {
			return fmt.Errorf("migration %04d (%s): %w", m.Version, m.Name, err)
		}
		pool.log.Info("migration applied", "version", m.Version, "name", m.Name)
	}
	return nil
}

// applyMigration runs one migration inside its own transaction, then
// records it in schema_migrations. Native pgx Exec (simple protocol)
// handles multi-statement SQL files. Migrations must be written
// idempotently (IF NOT EXISTS) so a crash between statement and ledger
// write is recoverable by re-running Migrate.
func applyMigration(ctx context.Context, conn *pgxpool.Conn, m Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1)`, m.Version); err != nil {
		return fmt.Errorf("record: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
