# SPDX-License-Identifier: AGPL-3.0-or-later
"""PostgreSQL access for the Sutra AI service (E2).

The service owns its schema: forward-only migrations in
``services/ai/migrations/`` run at startup under a PostgreSQL advisory
lock (so N replicas racing to boot apply each migration exactly once),
tracked in the ``ai_schema_migrations`` ledger. This mirrors the Go API's
migration runner but is deliberately a separate ledger/table set — each
service migrates only its own tables (lesson from C1's ledger collision).
"""
from __future__ import annotations

import logging
from pathlib import Path

import psycopg
from psycopg_pool import ConnectionPool

log = logging.getLogger("sutra-ai.db")

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"
# Advisory-lock key namespace for this service's migrations. hashtext()
# keeps it in bigint range; the string is service-specific so it can never
# collide with the Go API's or the chat service's migration locks.
_LOCK_KEY = "sutra-ai-migrations"


def _discover_migrations() -> list[tuple[int, Path]]:
    """Return (version, path) sorted by version. Versions are the leading
    integer of the filename: 0001_foo.sql -> 1."""
    found: list[tuple[int, Path]] = []
    for path in sorted(MIGRATIONS_DIR.glob("*.sql")):
        stem = path.stem  # "0001_ai_memory"
        version = int(stem.split("_", 1)[0])
        found.append((version, path))
    return found


def run_migrations(dsn: str) -> None:
    """Apply pending migrations. Idempotent; safe to call on every boot."""
    migrations = _discover_migrations()
    if not migrations:
        log.warning("no migrations discovered in %s", MIGRATIONS_DIR)
        return
    with psycopg.connect(dsn) as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT pg_advisory_lock(hashtext(%s))", (_LOCK_KEY,))
            try:
                cur.execute(
                    """
                    CREATE TABLE IF NOT EXISTS ai_schema_migrations (
                        version    INTEGER PRIMARY KEY,
                        applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
                    )
                    """
                )
                cur.execute("SELECT version FROM ai_schema_migrations")
                applied = {row[0] for row in cur.fetchall()}
                for version, path in migrations:
                    if version in applied:
                        continue
                    sql = path.read_text(encoding="utf-8")
                    log.info("applying AI migration %s", path.name)
                    # One transaction per migration: a failed migration
                    # leaves no partial state and is retried next boot.
                    with conn.transaction():
                        cur.execute(sql)
                        cur.execute(
                            "INSERT INTO ai_schema_migrations (version) VALUES (%s)",
                            (version,),
                        )
                    log.info("applied AI migration %s", path.name)
            finally:
                cur.execute("SELECT pg_advisory_unlock(hashtext(%s))", (_LOCK_KEY,))


def open_pool(dsn: str) -> ConnectionPool:
    """Connection pool for request handling. Fail-fast: a bad DSN raises
    here, at startup, not on the first user request."""
    pool = ConnectionPool(dsn, min_size=1, max_size=10, open=True)
    with pool.connection() as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT 1")
    return pool
