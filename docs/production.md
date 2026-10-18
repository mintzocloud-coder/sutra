# Production runbook

Deploy Sutra's backend from zero to healthy on your own infrastructure.
If you can read this file and run the commands below, you can run the
backend. For local laptop setup, see `docs/dev-setup.md` instead.

**Scope:** `services/api` (Go REST) + `services/chat` (Elixir Phoenix
release) + their dependencies (PostgreSQL 18 + pgvector, Valkey,
SeaweedFS). Identity is Ory Hydra + Ory Kratos (D1, in
docker-compose.yml): the API and chat verify Hydra-issued RS256 JWTs
through the JWKS path, and compose ships both services with migrate
one-shots. Phone-OTP SMS is stub-only until DLT registration
post-incorporation — any real provider value fails the API boot
fail-closed (see `docs/identity.md`). The dev-only `AUTH_BYPASS=1`
hatch exists for smoke tests and is **never** enabled in production.

---

## 1. Before you start

- A host (or two) with Docker + Compose, or a Kubernetes cluster.
  Compose instructions are first; Kubernetes notes are at the bottom.
- A PostgreSQL 18 server with `pgvector` available (the compose file
  ships `pgvector/pgvector:pg18` if you have no managed DB).
- A real domain name, e.g. `sutra.example.com`, with a reverse proxy
  (Caddy / Traefik / nginx) terminating TLS in front of the API and
  chat ports.
- No secrets checked into git — ever. Every secret below is injected
  through the environment.

## 2. Secrets (generate once, store safely)

All secrets live **outside the repo** — in your orchestrator's secret
store, a `.env` file that is gitignored, or a secrets manager (Vault,
Doppler, cloud provider). Never in the image, never in a commit.

| Secret | What | Generate |
|---|---|---|
| `SECRET_KEY_BASE` | Chat: Phoenix endpoint signing/encryption key (boot fails closed without it — see `services/chat/config/runtime.exs`) | `openssl rand -base64 48` (≥ 64 bytes) |
| `POSTGRES_PASSWORD` | Database password (only needed if you run your own PG) | `openssl rand -base64 24` |
| `RELEASE_COOKIE` | Chat clustering: Erlang distribution cookie, shared by all chat nodes (B5) | `openssl rand -base64 24` |
| `CLUSTER_SECRET` | Chat clustering: signs gossip payloads (B5) | `openssl rand -base64 24` |
| `HYDRA_*` | OIDC issuer URL + client creds (issue #4); until Hydra lands, `JWT_AUDIENCE` just needs to be consistent across api + chat | — |

Create a gitignored `.env` (compose reads it automatically):

```bash
# .env — gitignored, chmod 600
SECRET_KEY_BASE="$(openssl rand -base64 48)"
POSTGRES_PASSWORD="$(openssl rand -base64 24)"
RELEASE_COOKIE="$(openssl rand -base64 24)"
CLUSTER_SECRET="$(openssl rand -base64 24)"
PHX_HOST=api.sutra.example.com
HYDRA_ISSUER=https://hydra.sutra.example.com   # placeholder until issue #4
JWT_AUDIENCE=sutra-api
CORS_ORIGINS=https://app.sutra.example.com
```

> Never set `AUTH_BYPASS=1` in production. Both services log a loud
> startup warning when it is set; treat that warning as a deployment
> failure and roll back.

## 3. Environment reference

### API (`services/api` — `internal/config.Load`)

| Var | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | Container listen port |
| `DATABASE_URL` | `postgres://sutra:sutra@postgres:5432/sutra?sslmode=disable` | PG DSN (pool in `internal/db`) |
| `VALKEY_ADDR` | `valkey:6379` | Valkey endpoint |
| `DB_MAX_CONNS` | `25` | pgx pool size |
| `HYDRA_JWKS_URL` | `http://hydra:4444/.well-known/jwks.json` | JWKS for RS256 verification (A2) |
| `HYDRA_ISSUER` | `http://hydra:4444` | `iss` claim check |
| `JWT_AUDIENCE` | `sutra-api` | `aud` claim check |
| `RATE_LIMIT_RPS` | `100` | Per-IP sustained rate; `<=0` disables (A5) |
| `RATE_LIMIT_BURST` | `200` | Per-IP burst (A5) |
| `CORS_ORIGINS` | *(empty = disabled)* | Comma-separated allowlist or `*` (A5) |
| `CORS_MAX_AGE` | `86400` | Preflight cache seconds |
| `POST_VELOCITY_PER_HOUR` | `30` | Rolling per-author post cap; over the cap → 429 `post_velocity_limited` + Retry-After (D3) |
| `FOLLOW_VELOCITY_PER_HOUR` | `200` | Rolling per-user new-follow cap; over the cap → 429 `follow_velocity_limited` + Retry-After (D6) |
| `OTP_PROVIDER` | `stub` | Phone-OTP SMS gateway: only `stub` exists until DLT registration post-incorporation; any other value makes the API refuse to boot (fail-closed) (D1) |
| `FANOUT_THRESHOLD` | `10000` | Authors above this follower count merge on read instead of fan-out-on-write (D3b) |
| `FEED_TIMELINE_MAX` | `2000` | Home-timeline sorted-set trim: each user's `feed:home:{id}` keeps at most this many entries (D3b) |
| `SEAWEEDFS_MASTER_ADDR` | `seaweedfs:9333` | SeaweedFS master (dir/assign) for the media upload path (D4) |
| `MEDIA_PUBLIC_BASE_URL` | `http://seaweedfs:8080` | Volume-server base used for presigned PUT URLs and playback/poster URLs (D4) |
| `TRANSCODE_GROUP` | `transcode-workers` | Valkey consumer-group for the `media:transcode` stream; replicas share it so jobs distribute (D4b) |
| `TRANSCODE_CONSUMER` | *(container hostname)* | Consumer name within the group; set explicitly when you want pinned identity, else hostname (D4b) |
| `TRANSCODE_MAX_ATTEMPTS` | `5` | Job retries before the row is marked failed and the job parked on the `media:transcode:dead` stream (D4b) |
| `TRANSCODE_BACKOFF_BASE` | `30s` | Exponential-backoff base between retries (D4b) |
| `TRANSCODE_BACKOFF_MAX` | `30m` | Backoff cap (D4b) |
| `TRANSCODE_SWEEP_INTERVAL` | `5m` | Interval at which the worker sweeps stuck `uploaded` rows back onto the stream (D4b) |
| `CHAT_INTERNAL_URL` | `http://chat:4000` | Base URL of the chat service's internal event endpoint (D5) |
| `CHAT_INTERNAL_TOKEN` | *(unset)* | Service-to-service Bearer token for `POST /internal/events`; empty **disables** the realtime publisher with a loud WARN (D5) |
| `SHUTDOWN_TIMEOUT` | `10` | Graceful-shutdown seconds |
| `AUTH_BYPASS` | *(unset)* | Dev-only identity hatch — **never in prod** |

### Chat (`services/chat` — `config/runtime.exs`, read at boot)

| Var | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | *(required — boot fails without it)* | Same PG as the API |
| `POOL_SIZE` | `10` | Ecto pool size |
| `SECRET_KEY_BASE` | *(required — fails closed, empty rejected)* | Endpoint signing key |
| `PHX_HOST` | `localhost` | Host used in endpoint URLs |
| `HYDRA_ISSUER` / `JWT_AUDIENCE` | `http://hydra:4444` / `sutra-api` | Must match the API's values (`docs/auth-contract.md`) |
| `CLUSTER_ENABLED` | *(unset)* | `1` = enable libcluster (B5) |
| `CLUSTER_SECRET` | `dev-only-cluster-secret` | Gossip signing (set in prod!) |
| `RELEASE_NODE` / `RELEASE_COOKIE` | — | BEAM node name + distribution cookie (B7); unique node name per replica |
| `CHAT_INTERNAL_TOKEN` | *(required in prod)* | Shared secret for `POST /internal/events` (D5); fail-closed — every request 401s when unset |
| `MIX_ENV` | `dev` | Set `prod` for the release image |
| `AUTH_BYPASS` / `AUTH_BYPASS_SUBJECT` | *(unset)* / `dev-user` | Dev-only hatch — **never in prod** |

## 4. Build the images

```bash
docker build -t sutra-api:prod ./services/api     # multi-stage, non-root app user (A8)
docker build -t sutra-chat:prod ./services/chat   # mix release, non-root app user (B7)
```

CI builds both images on every push (the `compose` job) — if CI is
green, the images are buildable. The release image runs
`bin/sutra_chat start` in the foreground so SIGTERM reaches the BEAM
directly (graceful shutdown on both services).

## 5. Database + migrations

**Before you migrate, snapshot the database** (see §8).

The API and chat share one database but keep separate migration
ledgers (deliberately — a single shared `schema_migrations` table
would corrupt both migrators; see `docs/dev-setup.md`):

| Service | Ledger | Migrations |
|---|---|---|
| api | `schema_migrations` (integer version) | `services/api/internal/db/migrations/` — run at startup, advisory-locked |
| chat | `chat_schema_migrations` (bigint, Ecto) | `services/chat/priv/repo/migrations/` — run manually from the release |

**API:** migrations run automatically at boot inside an advisory lock,
so multiple api replicas starting together migrate exactly once.

**Chat:** the release has no `mix`. Run migrations explicitly before
starting the new chat image:

```bash
docker compose run --rm chat /app/bin/sutra_chat eval "SutraChat.Release.migrate()"
```

Roll back with `SutraChat.Release.rollback(repo, steps)` — know the
steps before you run it.

Migration discipline: forward-only, transactional, never edit a
migration that has run in production — add a new one.

## 6. First production boot (compose)

1. `chmod 600 .env` and fill in the secrets (§2). Double-check
   `AUTH_BYPASS` is **unset** — `./services/api` and the chat release
   both log a WARN at startup if it is present.
2. Build images (§4).
3. Migrate chat (§5). The API self-migrates at boot.
4. Boot: `docker compose up -d` (with prod overrides — see note below).
5. Verify (§7).

Compose note: the repo's `docker-compose.yml` is dev-flavored (bypass
passthroughs, dev defaults). For production, add a small
`docker-compose.prod.yml` override rather than editing the base file:

```yaml
# docker-compose.prod.yml
services:
  api:
    image: sutra-api:prod
    environment:
      CORS_ORIGINS: "https://app.sutra.example.com"
      RATE_LIMIT_RPS: "100"
      RATE_LIMIT_BURST: "200"
      HYDRA_JWKS_URL: "https://hydra.sutra.example.com/.well-known/jwks.json"
      HYDRA_ISSUER: "https://hydra.sutra.example.com"
      JWT_AUDIENCE: "sutra-api"
      # AUTH_BYPASS deliberately absent
    ports: ["8080:8080"]
  chat:
    image: sutra-chat:prod
    environment:
      PHX_HOST: "chat.sutra.example.com"
      POOL_SIZE: "10"
      HYDRA_ISSUER: "https://hydra.sutra.example.com"
      JWT_AUDIENCE: "sutra-api"
      # CLUSTER_ENABLED/RELEASE_*: only when running >1 chat node (§9)
    # ports stay internal; the reverse proxy routes /socket/websocket
```

`docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d`

## 7. Verify health

| Probe | Meaning | Expect |
|---|---|---|
| `GET /healthz` | Liveness — process is up, DB-independent | `200` always |
| `GET /readyz` | Readiness — DB ping + migrations applied | `200` healthy, `503` with `service_unavailable` envelope when PG unreachable |
| `GET /v1/me` (Bearer JWT) | Identity | `200` with user row (JIT-provisioned), `401` without/invalid token |

Both services log startup loudly. Expected-good checklist:

```bash
# api behind your reverse proxy (or host port 8080/8081 in compose)
curl -s -o /dev/null -w '%{http_code}\n' https://api.sutra.example.com/healthz   # 200
curl -s -o /dev/null -w '%{http_code}\n' https://api.sutra.example.com/readyz    # 200
curl -s -o /dev/null -w '%{http_code}\n' https://api.sutra.example.com/v1/me     # 401 (no token)

# request IDs on every response
curl -sI https://api.sutra.example.com/healthz | grep -i x-request-id

# chat: websocket at /socket/websocket (Phoenix channels)
# a valid token (or bypass in dev) joins room:<id> and sends message:new;
# smoke.sh does this end-to-end — see scripts/smoke.sh for the exact flow
```

`/readyz` is the readiness probe; `/healthz` is the liveness probe.
Point your orchestrator's readiness check at `/readyz` (DB-aware) and
liveness at `/healthz` (DB-free) — a DB outage must take pods out of
rotation (503) without killing them (200).

## 8. Backups

**What to back up:**

- **PostgreSQL (critical):** nightly `pg_dump` at minimum; WAL
  archiving (PITR) once you have real users. Test restores —
  an untested backup is a rumor.
- **SeaweedFS volume (`weeddata`):** blobs are the system of
  record for files; snapshot the volume on the same schedule as PG.
- **Chat presence / Valkey:** ephemeral by design (presence state
  converges again via CRDT on reconnect) — no backup needed.
- **Secrets:** the secret *store* is the backup; keep the generation
  commands (§2), not the values, in your runbook copy.

**Before any migration** (especially the first chat migration on a
live database): `pg_dump -Fc` to a timestamped file and verify the
dump restores on a scratch database. Forward-only migrations don't
protect you from a bad SQL statement.

## 9. Scaling

- **API (Go):** stateless. Scale replicas freely; the rate limiter is
  per-instance in memory (A5) — effective per-IP budget = `RPS ×
  replicas`, so tighten `RATE_LIMIT_RPS` as you add replicas or move
  to a shared limiter (Valkey is wired and waiting: `VALKEY_ADDR`).
  `DB_MAX_CONNS` (default 25) × replicas must stay below PG
  `max_connections`.
- **Chat:** single node first. Add nodes when a single node's
  CPU/memory or websocket count saturates:
  1. Set `CLUSTER_ENABLED=1`, a **unique** `RELEASE_NODE` per
     replica (e.g. `sutra-chat@10.0.0.2`), and the **same**
     `RELEASE_COOKIE` everywhere (B5).
  2. Prefer the Kubernetes DNS discovery strategy over gossip
     (multicast doesn't cross most cloud VPCs) — commented example
     in `config/runtime.exs`.
  3. Presence is a CRDT (`docs/presence.md`) — state converges once
     nodes connect; no external store needed. Message delivery is
     at-least-once with idempotent sends (`client_msg_id`), so
     replicas don't need sticky routing for correctness.
- **PostgreSQL:** the write path is single-primary today. Add read
  replicas only for heavy read endpoints; the Go API currently opens
  one pool (`DATABASE_URL`) — a read-split would need a second DSN
  (not built yet). Grow the primary's disk/CPU first; the bottleneck
  for this workload is `messages` insert throughput.
- **Benchmarks:** measured load numbers and the exact method live in
  `docs/benchmarks.md` (C4) — read them before sizing hardware, and
  re-run after any scaling change.

## 10. Incident basics

| Symptom | Likely cause | Check |
|---|---|---|
| `/readyz` 503, `/healthz` 200 | PG unreachable / migrations pending | `pg_isready`; PG logs; did the last deploy's chat migration run? |
| `/v1/me` 401 for valid users | JWKS fetch failing, issuer/aud mismatch, or Hydra down | api logs for JWKS fetch errors; `HYDRA_ISSUER`/`JWT_AUDIENCE` match api ↔ chat |
| Chat socket connect refused | Chat boots fail-closed without Hydra (issue #4) or wrong token | chat boot logs; `HYDRA_ISSUER` reachable from chat |
| API 429 spikes for one client | Per-IP limiter (A5) | `RATE_LIMIT_RPS`/`RATE_LIMIT_BURST`; raise, or add replicas and tighten |
| Chat nodes show divergent presence | BEAM nodes not connected | Same `RELEASE_COOKIE`? `RELEASE_NODE` unique? gossip multicast blocked (use DNS strategy) |
| Panic in api | — | `Recover` middleware returns JSON 500 and logs it — check structured logs for the request_id |
| Container restarts | OOM or failed healthcheck | Container logs; `/healthz` vs `/readyz` behavior tells you liveness vs dependency |

Every API error response carries the envelope
`{"error":{"code","message","request_id"}}` — give the user their
`request_id` and grep it in the JSON logs; `slog` lines carry the same
`request_id` for end-to-end correlation.

## 11. Pre-flight checklist (before any prod deploy)

- [ ] `AUTH_BYPASS` unset on **both** services (check the WARN lines in logs)
- [ ] Real `SECRET_KEY_BASE` set for chat (release fails closed otherwise)
- [ ] `HYDRA_ISSUER`/`JWT_AUDIENCE` consistent between api and chat
- [ ] `CORS_ORIGINS` set to the real frontend origin(s), never `*`
      with credentials (the middleware never sends `Allow-Credentials` —
      keep it that way)
- [ ] PG backup taken and restore-tested before migrating
- [ ] `scripts/smoke.sh` passes against a staging copy of the stack
- [ ] CI green on the commit you're deploying
- [ ] Secrets injected via the secret store — nothing secret in env
      files committed to git
- [ ] `CLUSTER_SECRET`/`RELEASE_COOKIE` changed from dev defaults
      before enabling clustering

## 12. Rollback

Images are immutable per release; keep the previous image tagged.
Compose:

```bash
# api
docker compose up -d --no-deps api   # after re-tagging :prod to the previous image
# chat: same, then re-run the migration check
docker compose run --rm chat /app/bin/sutra_chat eval "SutraChat.Release.migrate()"
```

Migrations are forward-only — rolling back the image does not roll
back the schema. A schema change that breaks the old code is a
deployment-plan bug: schema changes must stay backward-compatible for
at least one release, or use expand/migrate/contract.

## Kubernetes notes (if you skip compose)

- Deployment: 2+ replicas api, 1+ chat; readiness `/readyz`,
  liveness `/healthz`.
- Secrets via `Secret` objects (or external-secrets); never ConfigMaps.
- Chat clustering: use the DNS discovery strategy in
  `config/runtime.exs`, one `RELEASE_NODE` per pod (downward API IP),
  shared `RELEASE_COOKIE` secret.
- TLS at the ingress; both services run non-root (`app` user) and
  speak plain HTTP behind the proxy.
- PodDisruptionBudgets on the chat StatefulSet — websocket
  connections are long-lived; drain gracefully on rollout.
