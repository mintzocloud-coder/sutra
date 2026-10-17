# Sutra Backend — Production Build Plan

**Owner:** Dhanasvi (repo in-charge, per founder 2026-10-17)
**Goal:** Production-ready backend, pushed to GitHub every 30 min until complete.
**Quality bar (founder directive):** NOT "built for the sake of it". Every piece must
aim to be *better and more advanced* than what's in the market (WhatsApp/Telegram/
Signal for messaging; idiomatic production Go services for the API).

## How this plan is executed

- A worker runs every 30 minutes. Each run picks the **next incomplete task** (in phase
  order), implements it fully, verifies it, commits, and pushes to GitHub.
- **Never push broken code.** Each task lists its acceptance criteria; the worker must
  satisfy ALL of them before pushing. If verification fails, fix forward in the same run;
  if it cannot be fixed, leave the task incomplete and log the blocker.
- Verification available locally: `go build`, `go vet` (Go 1.22 via apt). Elixir full
  compile is CI-gated (repo CI compiles on push). Keep changes small and reviewable.
- Update this file: mark tasks `[x]` when done, add notes/learnings at the bottom.

## Better-than-market benchmarks (design targets)

- **Messaging:** idempotent sends (`client_msg_id` dedupe), server-side ordering,
  delivery receipts, presence via CRDT, multi-device-ready session model,
  E2E-ready architecture (Wave 2: libsignal 1:1, MLS groups) — match or beat
  Signal's protocol hygiene and Telegram's speed pragmatism.
- **API:** structured logging with request IDs, graceful shutdown, real readiness
  checks (dependency-aware), rate limiting, idempotency on writes, cursor
  pagination (never OFFSET for feeds), OpenAPI spec kept in sync.
- **Ops:** non-root containers, health/readiness/liveness separation, DB
  migrations versioned, secrets via env (never in repo), CI green on every push.

---

## Phase A — API service (Go) hardening

- [ ] **A1. Structured logging + request IDs + panic recovery**
      Add: `slog` JSON logging, `X-Request-ID` middleware (generate if absent,
      echo back), panic-recovery middleware returning 500 JSON.
      Accept: every response carries `X-Request-ID`; panics in handlers return
      JSON 500 (not crash); `go vet` clean.

- [ ] **A2. Real JWT verification middleware**
      Complete the OIDC/JWKS seam: fetch + cache JWKS from Hydra issuer URL,
      verify RS256 signature/expiry/issuer/audience; `Authorization: Bearer`
      middleware populating request context with subject + claims; test-mode
      bypass via env for local dev (explicitly logged as insecure).
      Accept: valid token → 200 with subject in context; expired/wrong-signature
      → 401 JSON; unit tests with locally-generated RSA keys.

- [ ] **A3. PostgreSQL connection + migrations**
      Add: `database/sql` + pgx pool, env-configured DSN, versioned SQL
      migrations (`migrations/0001_init.sql` …) run at startup (advisory-locked),
      `/readyz` fails when DB unreachable.
      Accept: fresh `docker compose up` → migrations applied; killing PG →
      `/readyz` 503, `/healthz` 200.

- [ ] **A4. Users table + real `/v1/me`**
      Migration: `users(id uuid pk, subject text unique, handle text unique,
      display_name text, created_at, updated_at)`. `GET /v1/me` returns the
      DB row for the JWT subject (auto-provision on first seen = "just-in-time
      provisioning", standard for OIDC).
      Accept: first call with new subject creates row; second call returns it;
      unknown subject without token → 401.

- [ ] **A5. Rate limiting + CORS + security headers**
      Add: per-IP token-bucket rate limiter (in-memory, configurable),
      CORS middleware (env-configured origins), security headers
      (nosniff, frame-deny, referrer-policy).
      Accept: 100 rapid requests → 429s after limit; preflight works for
      configured origin; headers present.

- [ ] **A6. JSON error envelope**
      Standard `{"error":{"code","message","request_id"}}` for all errors;
      map common cases (400/401/403/404/409/422/429/500).
      Accept: every non-2xx from the API matches the envelope (test).

- [ ] **A7. API tests**
      Unit tests for middleware (auth, rate limit, request ID); integration
      test for `/v1/me` against real PG (testcontainers or local PG via
      compose; skip gracefully if unavailable).
      Accept: `go test ./...` green.

- [ ] **A8. Dockerfile hardening**
      Multi-stage build, `non-root` user, minimal base, `HEALTHCHECK`,
      graceful shutdown already present — verify SIGTERM handling.
      Accept: `docker build` succeeds; container runs as non-root
      (`whoami` ≠ root).

## Phase B — Chat service (Elixir) production

- [ ] **B1. Message persistence (PostgreSQL)** *(GitHub issue #9)*
      Ecto + `messages(id uuid, room_id text, sender_subject text,
      client_msg_id text unique per sender, body text, inserted_at)`;
      `client_msg_id` dedupe on insert (idempotent sends); history query
      for channel join (last N, cursor-based).
      Accept: restart server → history survives; duplicate `client_msg_id`
      → single row, returns existing.

- [ ] **B2. Socket authentication**
      `UserSocket.connect/3`: verify JWT (shared secret/JWKS with API —
      document the contract), reject unauthenticated; assign subject.
      Accept: no/invalid token → connect refused; valid → `socket.assigns.subject`.

- [ ] **B3. Channel authorization**
      `ChatChannel.join`: only allow `room:*` for authenticated subjects;
      document the room membership model (Wave 1: open rooms by ID;
      Wave 2: membership lists). Payload validation (max body length,
      e.g. 4096 chars).
      Accept: unauthenticated join → error; oversized body → error.

- [ ] **B4. Delivery receipts**
      `msg:ack` event: sender gets confirmation when message persisted +
      broadcast; document at-least-once semantics.
      Accept: send → ack received by sender; persisted before broadcast.

- [ ] **B5. Presence hardening**
      Keep Phoenix.Presence (CRDT); add `presence_diff` broadcast already
      there — verify multi-node via `libcluster` config (gossip, k8s-ready
      commented).
      Accept: config present, documented; single-node tests green.

- [ ] **B6. Chat tests**
      Channel tests: join/auth, send/persist, dedupe, history.
      Accept: `mix test` green in CI.

- [ ] **B7. Dockerfile hardening**
      Non-root, releases (`mix release`) instead of `mix phx.server`
      for production image.
      Accept: release boots; `mix phx.server` kept for dev.

## Phase C — Integration, docs, proof

- [ ] **C1. Full compose stack boots**
      `docker compose up` → pg, valkey, seaweedfs, api, chat all healthy;
      smoke script hits `/healthz`, `/v1/me` (test token), websocket
      send/receive round-trip.
      Accept: `scripts/smoke.sh` passes end-to-end.

- [ ] **C2. OpenAPI spec**
      `docs/openapi.yaml` for the REST API; kept in sync (CI lint if cheap).
      Accept: spec validates (spectral or redocly lint if available; else
      manual review).

- [ ] **C3. Production runbook**
      `docs/production.md`: env vars, secrets, migrations, backups,
      scaling notes (when to add chat nodes, PG read replicas),
      incident basics.
      Accept: a new contributor can deploy from this doc.

- [ ] **C4. Benchmarks**
      Basic load numbers: ws connections/sec, msg throughput single node
      (document hardware + method; honest numbers, no hype).
      Accept: `docs/benchmarks.md` with reproducible method.

## Done when

All boxes ticked, CI green, `scripts/smoke.sh` passes, docs complete.
Then report to founder: production-ready backend live on main.

## Log

- 2026-10-17: Plan created. Starting Phase A.
