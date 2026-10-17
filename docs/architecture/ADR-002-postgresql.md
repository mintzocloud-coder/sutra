# ADR-002: PostgreSQL 18 + pgvector (+ Valkey)

**Date:** 2026-10-17 · **Status:** Accepted · **Decider:** Harshal Vanpariya

## Decision

**PostgreSQL 18 + pgvector** is the primary database; **Valkey** is the cache/ephemeral store.

## Rationale

A fit-ranked fresh evaluation (Oct 2026, 18 candidates, popularity ignored) concluded:

- One engine covers all six workloads: ACID marketplace orders, JSONB feeds/video metadata, pgvector AI feature store, logical replication/CDC.
- License: PostgreSQL License / BSD-3-Clause — permissive, AGPLv3-project compatible.
- One `docker run` on a contributor laptop; runnable anywhere in India.
- **Killed on license:** ScyllaDB (source-available since Dec 2024), SurrealDB core (BSL 1.1), Dragonfly (BSL 1.1).
- **Watch list:** YugabyteDB (scale-out successor, same wire protocol), Turso, Neon, Gel, Doltgres, TigerBeetle (ledger sidecar), ClickHouse (analytics sidecar).

## Consequences

- Adopt YugabyteDB only when single-primary Postgres actually hurts — not before.
- Analytics outgrowing PG → ClickHouse/GreptimeDB sidecar, not a primary-DB change.
