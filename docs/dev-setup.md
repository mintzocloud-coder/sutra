# Dev setup — laptop to first commit in 10 minutes

## Prereqs

- Docker + Docker Compose
- Git
- (per service, later) Elixir 1.17+, Go 1.23+, Python 3.12+, Node 20+, JDK 17+

## One command

```bash
docker compose up
```

This brings up:

| Service | Port | What |
|---|---|---|
| PostgreSQL 18 + pgvector | 5432 | system of record (user `sutra` / password `sutra`) |
| Valkey | 6379 | cache + ephemeral state |
| SeaweedFS | 9333 / 8080 | S3-compatible blob storage |
| API (Go) | 8081 → container 8080 | REST API: `/healthz`, `/readyz`, `/v1/me` |
| Chat (Elixir release) | 4000 | websocket `/socket/websocket` (Phoenix channels) |

The API and chat share one postgres database. Their migration ledgers are
kept separate on purpose: the Go API uses `schema_migrations`, the chat
service uses `chat_schema_migrations` (see `services/chat/config/config.exs`)
— otherwise the two migrators would corrupt each other's ledger.

First boot of the chat service needs two things (see `docker-compose.yml`
comments): a real `SECRET_KEY_BASE` (the release fails closed without one)
and its migrations, which live in the release itself:

```bash
export SECRET_KEY_BASE="$(openssl rand -base64 48)"
docker compose up -d postgres
docker compose run --rm chat /app/bin/sutra_chat eval "SutraChat.Release.migrate()"
docker compose up -d
```

## Smoke test

`scripts/smoke.sh` boots the whole stack and verifies it end to end —
API health/readiness/identity, a real websocket send → broadcast →
`msg:ack` → reconnect-history round-trip, SeaweedFS and Valkey. Identity
runs through the dev-only `AUTH_BYPASS=1` hatch (both services log a loud
warning; never enable it in production):

```bash
bash scripts/smoke.sh                # tears the stack down afterwards
SMOKE_KEEP_UP=1 bash scripts/smoke.sh # leaves the stack running
```

CI runs the same script on every push (the `smoke` job).

## Verify

```bash
pg_isready -h localhost -p 5432 -U sutra
valkey-cli -p 6379 ping
curl -s localhost:9333/cluster/status
```

## First commit checklist

1. Read `ARCHITECTURE.md` and the ADRs in `docs/architecture/`.
2. Pick an issue — `good first issue` if you're new.
3. Sign the CLA (comment "I agree to the CLA" on your first PR).
4. SPDX header on every source file: `// SPDX-License-Identifier: AGPL-3.0-or-later`
5. CI must pass: `bash scripts/check-headers.sh` locally first.
