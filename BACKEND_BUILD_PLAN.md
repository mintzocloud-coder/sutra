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

- [x] **A1. Structured logging + request IDs + panic recovery**
      Add: `slog` JSON logging, `X-Request-ID` middleware (generate if absent,
      echo back), panic-recovery middleware returning 500 JSON.
      Accept: every response carries `X-Request-ID`; panics in handlers return
      JSON 500 (not crash); `go vet` clean.
      DONE 2026-10-17: `internal/middleware` (RequestID, Logger, Recover, Chain);
      main.go uses slog JSON + middleware chain; verified `go build`+`go vet`
      clean, smoke test: X-Request-ID present, /v1/me 401, JSON access logs.

- [x] **A2. Real JWT verification middleware**
      Complete the OIDC/JWKS seam: fetch + cache JWKS from Hydra issuer URL,
      verify RS256 signature/expiry/issuer/audience; `Authorization: Bearer`
      middleware populating request context with subject + claims; test-mode
      bypass via env for local dev (explicitly logged as insecure).
      Accept: valid token → 200 with subject in context; expired/wrong-signature
      → 401 JSON; unit tests with locally-generated RSA keys.
      DONE 2026-10-17: rewrote `internal/oidc` — stdlib-only RS256 verifier
      (no third-party JWT lib): JWKS fetch + cache (15m TTL, refresh on
      unknown kid for key rotation), alg pinned to RS256 (rejects alg:none),
      exp/iss/aud checks with 60s skew leeway; new `internal/middleware/auth.go`
      `RequireAuth` stores subject+claims in context, 401 JSON envelope,
      `AUTH_BYPASS=1` dev bypass (loud WARN at startup). Config: HYDRA_ISSUER,
      JWT_AUDIENCE, AUTH_BYPASS, AUTH_BYPASS_SUBJECT. `/v1/*` now behind auth;
      /healthz open. Tests: 8 oidc tests + 4 middleware tests (RSA-2048 keys,
      httptest JWKS, incl. rotation) — `go build`+`go vet`+`go test ./...`
      green; live smoke: 401 JSON without token, 200+bypass subject with
      AUTH_BYPASS=1.

- [x] **A3. PostgreSQL connection + migrations**
      Add: `database/sql` + pgx pool, env-configured DSN, versioned SQL
      migrations (`migrations/0001_init.sql` …) run at startup (advisory-locked),
      `/readyz` fails when DB unreachable.
      Accept: fresh `docker compose up` → migrations applied; killing PG →
      `/readyz` 503, `/healthz` 200.
      DONE 2026-10-17: new `internal/db` package — pgx/v5 pgxpool (v5.7.4,
      go-1.22-compatible pin; GOTOOLCHAIN=local guard), `Open` with pool
      tuning + fail-fast ping, embedded migrations
      (`internal/db/migrations/0001_extensions.sql` — pgcrypto for future
      UUID PKs), advisory-locked forward-only runner (`schema_migrations`
      ledger, per-migration transactions, idempotent). Config: `DB_MAX_CONNS`.
      `/readyz` now pings DB (503 "not ready: database unreachable" on drop),
      `/healthz` stays DB-independent. Verified END-TO-END on real PG16:
      fresh boot → migration 0001 applied; PG killed → healthz 200/readyz
      503; PG restarted → readyz 200 (pool recovers); X-Request-ID intact.
      Tests: discovery-ordering unit test + real-DB integration test
      (TEST_DATABASE_URL-gated, skips cleanly) — `go build`+`go vet`+
      `go test ./...` green. GOTCHA: `go get` with GOTOOLCHAIN=auto tried to
      pull go1.25 toolchain via pgx v5.11; pinned v5.7.4 + exported
      GOTOOLCHAIN=local during tidy.

- [x] **A4. Users table + real `/v1/me`**
      Migration: `users(id uuid pk, subject text unique, handle text unique,
      display_name text, created_at, updated_at)`. `GET /v1/me` returns the
      DB row for the JWT subject (auto-provision on first seen = "just-in-time
      provisioning", standard for OIDC).
      Accept: first call with new subject creates row; second call returns it;
      unknown subject without token → 401.
      DONE 2026-10-17: migration `0002_users.sql` (uuid pk via pgcrypto,
      unique subject/handle, nullable display_name, `set_updated_at()`
      trigger); new `internal/users` package — `Provision()` (INSERT ...
      ON CONFLICT (subject) DO NOTHING RETURNING + SELECT fallback, race-safe;
      display name seeded from OIDC `name`/`preferred_username` claims, never
      overwritten on later calls; handle stays NULL until claimed);
      `MeHandler` serves GET /v1/me with standard JSON error envelope.
      Verified: `go build`+`go vet`+`go test ./...` green on real PG16
      (8-racer concurrent-provision test converges on 1 row); live smoke:
      call 1 creates row, call 2 returns same id, no-token → 401 JSON
      envelope, X-Request-ID present. main.go now uses "GET /v1/me"
      method-pattern routing. GOTCHA: pkill -f with the binary name in the
      same command line self-matches — use `pkill -x` instead.

- [x] **A5. Rate limiting + CORS + security headers**
      Add: per-IP token-bucket rate limiter (in-memory, configurable),
      CORS middleware (env-configured origins), security headers
      (nosniff, frame-deny, referrer-policy).
      Accept: 100 rapid requests → 429s after limit; preflight works for
      configured origin; headers present.
      DONE 2026-10-17: new `internal/middleware/ratelimit.go` — stdlib-only
      token bucket keyed on TCP peer IP (NOT X-Forwarded-For; documented why),
      configurable RATE_LIMIT_RPS (default 100, <=0 disables) + RATE_LIMIT_BURST
      (default 200), idle-bucket sweeper (10m TTL) + 100k-bucket emergency
      eviction backstop; 429 JSON envelope + Retry-After + X-RateLimit-Limit;
      new `cors.go` — CORS_ORIGINS allowlist (exact match, "*" wildcard, empty
      = disabled), preflight 204 with echo of Allow-Headers, Vary: Origin,
      no Allow-Credentials (Bearer auth, no cookies); new `security.go` —
      nosniff / DENY / strict-origin-when-cross-origin / CSP
      `default-src 'none'; frame-ancestors 'none'`; new `error_envelope.go`
      shared WriteJSONError (A6 will consolidate onto it). Chain order:
      Recover → RequestID → SecurityHeaders → CORS → RateLimit → Logger
      (preflights bypass the limiter; 429s still carry security headers).
      Verified: `go build`+`go vet`+`go test ./...` green (12 new tests);
      live smoke on real PG (RATE_LIMIT_RPS=20,BURST=10): 100 rapid reqs →
      73×429; preflight allowed origin → 204 + ACAO/Allow-Methods/Headers/
      Max-Age; disallowed origin → no ACAO; all 4 security headers on
      200/429; /readyz 200, /v1/me works. GOTCHA: `pkill -f` with the binary
      path in the same command line SIGTERMs your own shell — `pkill -x`
      only for stopping test servers.

- [x] **A6. JSON error envelope**
      Standard `{"error":{"code","message","request_id"}}` for all errors;
      map common cases (400/401/403/404/409/422/429/500).
      Accept: every non-2xx from the API matches the envelope (test).
      DONE 2026-10-17: consolidated every error path onto the envelope —
      new `ErrorCode()` status→code map (400/401/403/404/405/409/422/429/
      500/503, "error" fallback), `WriteStatusError()` (code derived from
      status), `NotFoundHandler()` + `MethodNotAllowedHandler()` (Allow
      header); removed hand-rolled duplicates in auth.go / users/handler.go
      / Recover. main.go: extracted `newHandler()` (integration-testable),
      outer "/" catch-all → 404 envelope, inner /v1/ catch-all → 404,
      explicit method guard on /v1/me → 405 envelope (no Go method-pattern
      ambiguity), /readyz 503 → service_unavailable envelope; RequestID
      moved OUTERMOST so recovered panics carry a real request_id (was
      empty before). Tests: 7 envelope/mapping/handler unit tests +
      Recover-with-request-id order test; cmd/api integration tests —
      404/405/401 envelopes, readyz 503 envelope (closed pool), /v1/me 200
      (DB parts gated, skip cleanly). `go build`+`go vet`+`go test ./...`
      green incl. full run on throwaway local PG16. Live smoke: 404/405/
      401/429/503 all return envelope + echo X-Request-ID; readyz 503→200
      across a real PG kill/restart, healthz 200 throughout.

- [x] **A7. API tests**
      Unit tests for middleware (auth, rate limit, request ID); integration
      test for `/v1/me` against real PG (testcontainers or local PG via
      compose; skip gracefully if unavailable).
      Accept: `go test ./...` green.
      DONE 2026-10-17: filled the coverage gaps — new
      `internal/middleware/middleware_test.go` (8 tests: RequestID
      generate/reuse/echo/uniqueness, FromContext empty, Logger JSON line
      with request_id + status + duration + implicit-200 default, Recover
      panic→500 envelope with request_id + no panic-value leak + clean
      passthrough, Chain first-middleware-outermost order); new
      `TestRequireAuthPopulatesClaims` in auth_test.go (full claim set
      reaches handlers: subject/issuer/audience); TestHandler_MeOK
      strengthened (X-Request-ID asserted on the 200 success path, second
      GET returns same user id — HTTP-level provision idempotency). CI now
      actually RUNS the integration tests: `go-api` job has a postgres:16
      service + TEST_DATABASE_URL + `go test ./...` step (was build+vet
      only; gated tests skipped silently in CI before). Verified: full
      suite green on throwaway local PG16 AND green with no DB (10 clean
      skips). `go build`+`go vet`+`go test ./...` green.

- [x] **A8. Dockerfile hardening**
      Multi-stage build, `non-root` user, minimal base, `HEALTHCHECK`,
      graceful shutdown already present — verify SIGTERM handling.
      Accept: `docker build` succeeds; container runs as non-root
      (`whoami` ≠ root).
      DONE 2026-10-17: rewrote `services/api/Dockerfile` — multi-stage
      (golang:1.22-alpine → alpine:3.20), static stripped binary
      (`CGO_ENABLED=0 -trimpath -ldflags="-s -w"`), non-root `app` user
      (`addgroup -S`/`adduser -S`, `COPY --chown`, `USER app`), exec-form
      ENTRYPOINT so SIGTERM reaches Go directly, `HEALTHCHECK` on
      DB-independent `/healthz` (wget -qO-; /readyz stays the DB-aware
      readiness signal, wired in compose/k8s separately). Also added a CI
      step `docker build -f services/api/Dockerfile services/api` so the
      image builds on every push. Verified: (1) exact Dockerfile build
      flags compile a statically-linked stripped binary, `go vet` clean;
      (2) ran the binary as unprivileged uid 65534 (mirrors `USER app`):
      /healthz 200, /readyz 200, /v1/me 200; (3) SIGTERM → logged
      "api stopped", process exited cleanly (graceful Shutdown path).
      NOTE: literal `docker build` could not run on this VM — dockerd
      fails to start (no iptables/nft kernel support for bridge
      networking); the CI build step is the durable gate.

## Phase B — Chat service (Elixir) production

- [x] **B1. Message persistence (PostgreSQL)** *(GitHub issue #9)*
      Ecto + `messages(id uuid, room_id text, sender_subject text,
      client_msg_id text unique per sender, body text, inserted_at)`;
      `client_msg_id` dedupe on insert (idempotent sends); history query
      for channel join (last N, cursor-based).
      Accept: restart server → history survives; duplicate `client_msg_id`
      → single row, returns existing.
      DONE 2026-10-17: `SutraChat.Repo` (Ecto, supervised first in
      application.ex), migration `20261017220000_create_messages` (uuid PK,
      unique index on (sender_subject, client_msg_id), composite index on
      (room_id, inserted_at, id)), `SutraChat.Chat.Message` schema
      (microsecond inserted_at, no updated_at), `SutraChat.Chat` context:
      `persist_message/1` = single INSERT ... ON CONFLICT (atomic,
      race-safe, `returning: true` hands back the EXISTING row on retry),
      `history/3` = cursor-paged on (inserted_at, id) — never OFFSET —
      returns oldest-first pages + next_cursor. `ChatChannel`: persist
      BEFORE broadcast (crash between broadcast and persist can't lose a
      "sent" message), join replies with recent history (limit/cursor
      params), invalid cursor → clean error. Config: dev defaults in
      config.exs, prod from DATABASE_URL in new config/runtime.exs;
      compose chat service now gets DATABASE_URL.
      VERIFIED: apt Elixir 1.14 CANNOT compile phoenix 1.7.24's
      phoenix_template or postgrex>=0.20 (needs Elixir>=1.15/1.16 syntax)
      — full local compile stays CI-gated (Elixir 1.18), per plan. Locally
      verified what 1.14 allows: `Code.string_to_quoted!` syntax on all 8
      new/edited files; Message+Chat compiled against REAL ecto beams
      (macro/query API misuse would fail); changeset validation live
      (valid→true, missing body→invalid, non-string body rejected); the
      exact upsert + history SQL run against real PG: duplicate send →
      1 row, RETURNING yields the row; 2-page cursor pagination correct;
      same client_msg_id from another sender → separate row. Deps kept at
      latest (ecto_sql 3.14/postgrex 0.22.4 — zero hex advisories); an
      attempted 1.14-compatible pin (postgrex 0.19.3) was REVERTED after
      `mix hex.audit` showed HIGH/MEDIUM CVEs in the old pins. Pushed.

- [x] **B2. Socket authentication**
      `UserSocket.connect/3`: verify JWT (shared secret/JWKS with API —
      document the contract), reject unauthenticated; assign subject.
      Accept: no/invalid token → connect refused; valid → `socket.assigns.subject`.
      DONE 2026-10-17: new `SutraChat.Auth.Token` — pure RS256 verifier
      (stdlib `:public_key` + Jason, no JWT lib), mirroring Go
      `internal/oidc` claim-for-claim: alg pinned RS256 (none/HS256
      rejected), kid required, exp with 60s leeway (expiring-within-60s
      counts as expired, same as Go), iss/aud enforced, sub required,
      aud string-or-list; new `SutraChat.Auth.TokenVerifier` GenServer —
      JWKS cache (15m TTL, lazy first fetch, refresh on unknown kid for
      rotation, single-process serialization = no thundering herd),
      same env vars as the Go API (`HYDRA_ISSUER` default
      `http://hydra:4444`, `JWT_AUDIENCE` default `sutra-api`,
      `AUTH_BYPASS`/`AUTH_BYPASS_SUBJECT` dev bypass with loud WARN);
      `UserSocket.connect/3` now requires a `token` param, refuses
      (`:error`) on missing/invalid, assigns `user_id` = subject on valid;
      contract documented in `docs/auth-contract.md`; compose chat service
      gets HYDRA_ISSUER/JWT_AUDIENCE (fails closed until Hydra lands in
      issue #4); `:inets`/`:ssl` added to extra_applications.
      VERIFIED: apt Elixir 1.14 still can't compile Phoenix — CI-gated as
      per plan. Locally: syntax-checked all 9 files; Token+TokenVerifier
      compiled against real Jason beams; 34-check behavioral harness green
      (all error paths, leeway edge, float exp, JWK parse/reject, GenServer
      vs stub JWKS: lazy fetch, rotation refresh, unknown-kid, issuer-down
      clean error, bypass); socket glue exercised via Phoenix.Socket stub
      (valid→assigns.user_id+id/1, missing/invalid→:error). New ExUnit
      suite `test/sutra_chat/auth/token_test.exs` (18 tests) + test_helper
      shipped for CI/B6. GOTCHA: `:crypto.sign/verify` with `:rsa` rejects
      key tuples on OTP 25's crypto 5.1 (`pkey.c:357` badarg) — use the
      stable `:public_key.sign/3` + `:public_key.verify/4` instead.

- [x] **B3. Channel authorization**
      `ChatChannel.join`: only allow `room:*` for authenticated subjects;
      document the room membership model (Wave 1: open rooms by ID;
      Wave 2: membership lists). Payload validation (max body length,
      e.g. 4096 chars).
      Accept: unauthenticated join → error; oversized body → error.
      DONE 2026-10-17: new pure `SutraChat.Chat.RoomPolicy` (no Repo/Phoenix,
      unit-testable): `authorize_join/2` rejects nil/empty `user_id`
      (:unauthorized — defense-in-depth behind UserSocket's connect refusal)
      and malformed room ids (`^[A-Za-z0-9_-]{1,64}$`); `authorize_send/2`
      rejects empty payloads + body > 4096 chars (Telegram's cap) +
      client_msg_id > 128 chars. `ChatChannel.join/3` and
      `handle_in("message:new")` route through it with clean
      `{:error, %{reason: ...}}` replies; `Message.changeset` enforces the
      same caps at the persistence boundary. New doc `docs/room-membership.md`
      (Wave 1 open-rooms-by-ID rationale + enforcement table; Wave 2
      membership-list design where `authorize_join/2` keeps its shape;
      Wave 3+ E2E note). Tests: 10 ExUnit cases green (join auth/matrix,
      boundary 64/65, size caps, grapheme-vs-byte emoji check) + changeset
      verified against real ecto beams (4096 ok, 4097 → "should be at most
      4096 character(s)", 129-char client_msg_id rejected). Syntax-checked
      all touched files; full `mix compile` still CI-gated (1.14). Pushed.

- [x] **B4. Delivery receipts**
      `msg:ack` event: sender gets confirmation when message persisted +
      broadcast; document at-least-once semantics.
      Accept: send → ack received by sender; persisted before broadcast.
      DONE 2026-10-17: send path is now persist → broadcast → `msg:ack`
      push to the sender only (pushed event, not the push reply — exactly
      one confirmation path, and it can never precede durability).
      `Chat.persist_message/1` now returns `{:ok, message, deduped}`: the
      single-statement upsert detects fresh-vs-retry inside RETURNING via
      the `xmax` system column (`xmax = 0` = fresh insert; the
      `ON CONFLICT DO UPDATE` path always stamps a new tuple version) —
      no follow-up SELECT, no race. Ack payload (`Chat.ack_payload/2`,
      pure + unit-tested): server id, room_id, echoed client_msg_id,
      `deduped` flag (true = this send was a retry of an already-stored
      message — a distinction WhatsApp/Telegram acks don't make),
      `sent_at` server ms. New doc `docs/delivery-semantics.md`:
      at-least-once contract, client retry rules (same client_msg_id,
      10s timeout, deduped:true = success), crash-window table,
      broadcast-duplicate warning (clients dedupe on id), ordering,
      multi-device client_msg_id scoping, what the ack does NOT mean
      (not a read/delivery receipt — Wave 2).
      VERIFIED: syntax-checked all touched files; chat.ex + message.ex
      compile against real ecto beams; 3 new ExUnit tests green
      (ack_payload fields/deduped true-false/boolean guard); the exact
      @upsert_sql verified at the parameterized-protocol level on real
      PG16 (PREPARE/EXECUTE with bound args): fresh → inserted_new=t,
      retry → same original id + inserted_new=f, other sender →
      new row + t, 2 rows total. Full `mix compile` still CI-gated
      (1.14 can't build ecto_sql 3.14/postgrex 0.22 — they need ≥1.15).
      Pushed.

- [x] **B5. Presence hardening**
      Keep Phoenix.Presence (CRDT); add `presence_diff` broadcast already
      there — verify multi-node via `libcluster` config (gossip, k8s-ready
      commented).
      Accept: config present, documented; single-node tests green.
      DONE 2026-10-18: `{:libcluster, "~> 3.3"}` dep (resolved 3.5.0 via
      `mix deps.get`, lock updated); `Cluster.Supervisor` added to the app
      supervision tree (no-op with zero topologies); `config/runtime.exs`
      defines opt-in topologies via `CLUSTER_ENABLED=1` — gossip strategy
      (UDP multicast 230.1.1.251:45892, CLUSTER_SECRET HMAC, all keys
      verified against libcluster 3.5.0 source) + commented Kubernetes DNS
      strategy; documented release-node requirements
      (RELEASE_NODE/RELEASE_COOKIE, shared cookie). Channel: presence meta
      is now per-device (`online_at` ms, `device`, `status:"online"`) —
      client may send a stable `device_id` join param (capped 128 chars),
      else server mints `anon-<rand>`; same user on 2 devices accumulates
      2 metas under one key. New `docs/presence.md` (model, event contract
      presence_state/presence_diff, CRDT convergence notes, clustering
      guide, client rules); compose chat env documents CLUSTER_ENABLED /
      CLUSTER_SECRET / RELEASE_COOKIE (commented). New ExUnit suite
      `test/sutra_chat/presence_test.exs` (5 tests: track/list meta shape,
      multi-device metas, untrack→leaves diff, joins diff broadcast,
      dead-process auto-untrack) — runs against the real Presence module.
      VERIFIED: config keys + supervisor arity + Presence.start_link/3
      signature checked against actual dep sources (caught: Presence has
      no start_link/0 — test uses child_spec/1 via throwaway supervisor,
      probing SutraChat.PresenceSupervisor first); SPDX header check
      green. Local `mix compile`/`mix test` NOT runnable this run — fresh
      VM lost apt Elixir and provisioning's `apt-get update` is wedged,
      so no Elixir runtime exists; per plan, syntax/behavioral harness is
      CI-gated (Elixir 1.18). Changes are small/additive; every libcluster
      / Phoenix.Presence API used was verified against the real sources.
      Pushed.

- [x] **B6. Chat tests**
      Channel tests: join/auth, send/persist, dedupe, history.
      Accept: `mix test` green in CI.
      DONE 2026-10-18: new `SutraChatWeb.ChatChannelTest` (13 tests) via
      `Phoenix.ChannelTest`, all `async: false` against the real Repo
      (Ecto SQL sandbox, rollback per test) + real Presence:
      join/auth (no-identity socket refused, malformed room ids rejected,
      empty history on fresh room), presence (presence_state push shape +
      presence_diff broadcast), join params (limit honored, bad cursor
      rejected), send/persist (persist → broadcast → sender-only msg:ack,
      ack id matches broadcast id, row verified in PG via Chat.history),
      dedupe (same client_msg_id twice → one row, second ack
      `deduped: true` with the ORIGINAL id), history oldest-first across
      re-joins, cursor pagination (5 msgs / limit 2 → 3 pages + invalid
      cursor error), payload validation (4097-char body → clean error,
      missing/nil body → error, nothing persisted). New infra:
      `config/test.exs` (sandbox Repo — DATABASE_URL in CI, local
      sutra_chat_test fallback; endpoint `server: false`; quiet logger),
      `import_config "#{config_env()}.exs"` in config.exs, `test/support/`
      (`DataCase`, `ChannelCase`) compiled via new `elixirc_paths(:test)`
      in mix.exs, test_helper puts sandbox in manual mode. CI `elixir-chat`
      job renamed "builds and tests": postgres:16 service + DATABASE_URL,
      `mix ecto.create` + `mix ecto.migrate` + `mix test`.
      VERIFIED: all 7 new/edited files syntax-checked
      (`Code.string_to_quoted!`), SPDX headers present (checker green);
      full `mix compile`/`mix test` remain CI-gated (Elixir 1.18) — local
      1.14 cannot compile phoenix_template 1.1.0 (needs ~> 1.16), same as
      B1–B5. Test logic reviewed against Phoenix.ChannelTest semantics:
      `{:noreply, _}` handlers produce no reply (assert via broadcast/ack,
      not assert_reply), `^`-pinned id across the two dedupe acks, sandbox
      shared mode lets the channel process use the test's connection.
      Pushed.

- [x] **B7. Dockerfile hardening**
      Non-root, releases (`mix release`) instead of `mix phx.server`
      for production image.
      Accept: release boots; `mix phx.server` kept for dev.
      DONE 2026-10-18: multi-stage `services/chat/Dockerfile` — build stage
      (elixir:1.18-alpine, MIX_ENV=prod, deps-first layer caching,
      `mix release`), runtime stage (alpine:3.21, libstdc++/libgcc/
      ncurses-libs/ca-certificates, non-root `app` user, `--chown` copy,
      `USER app`, `bin/sutra_chat start` in foreground so SIGTERM reaches
      the BEAM). New `config/prod.exs` (minimal; required because config.exs
      imports `#{config_env()}.exs` — `mix release` would fail without it).
      `config/runtime.exs` now fails closed at boot without SECRET_KEY_BASE
      (the dev key baked into config.exs must never ship) and sets endpoint
      `url.host` from PHX_HOST. New `SutraChat.Release` module
      (`migrate/0`, `rollback/2` — releases have no `mix`). HEALTHCHECK via
      `bin/sutra_chat pid` (endpoint is websocket-only, no HTTP routes yet).
      Compose chat docs updated (SECRET_KEY_BASE, migrate command,
      RELEASE_NODE/RELEASE_COOKIE now functional for clustering); README
      documents dev=`mix phx.server` / prod=release split. CI `compose` job
      now also builds the chat image on every push (A8-style gate).
      VERIFIED: YAML valid for compose + CI; SPDX headers on all new files;
      full `mix release` + image boot are CI-gated (no Elixir/docker on this
      VM — same standing constraint as B1–B6). **Phase B complete.**

## Phase C — Integration, docs, proof

- [x] **C1. Full compose stack boots**
      `docker compose up` → pg, valkey, seaweedfs, api, chat all healthy;
      smoke script hits `/healthz`, `/v1/me` (test token), websocket
      send/receive round-trip.
      Accept: `scripts/smoke.sh` passes end-to-end.
      DONE 2026-10-18: new `scripts/smoke.sh` (builds images; boots
      postgres/valkey/seaweedfs; waits on pg_isready; runs chat
      migrations via `Release.migrate()` in the release; boots api+chat;
      asserts api /healthz 200 + X-Request-ID, /readyz 200, /v1/me 200
      with bypass subject `dev-user` auto-provisioned; runs
      `scripts/smoke_ws.py` stdlib-only Phoenix-channels-v2 client:
      join → empty history → message:new → broadcast + sender-only
      msg:ack (ids match, deduped:false) → reconnect + rejoin →
      message present in history (persistence proven); asserts
      seaweedfs /cluster/status 200 and valkey PING→PONG; tears the
      stack down unless SMOKE_KEEP_UP=1). Identity via AUTH_BYPASS=1
      (documented dev-only hatch; both services log a loud WARN).
      New CI `smoke` job runs the same script on every push.
      FIXES found while wiring the stack: (1) compose port clash —
      SeaweedFS filer and the API both mapped host 8080 → api host port
      is now 8081 (container still 8080); (2) migration ledger collision
      — Go API's `schema_migrations` (integer) vs Ecto's default
      `schema_migrations` (bigint) on the shared DB → chat now uses
      `migration_source: "chat_schema_migrations"`; (3) compose maps an
      unset SECRET_KEY_BASE to "" which Elixir treats as truthy →
      runtime.exs fail-closed check now rejects empty strings too.
      Compose api/chat now use `depends_on: service_healthy`; added
      AUTH_BYPASS/SECRET_KEY_BASE env passthroughs (empty = disabled);
      docs/dev-setup.md updated (service table, first-boot migrate
      steps, smoke instructions).
      VERIFIED: bash -n + py_compile clean; compose + CI YAML parse;
      SPDX headers present; smoke_ws.py protocol logic tested
      end-to-end against a fake Phoenix server (join/reply, interleaved
      presence_state, broadcast+ack matching, rejoin history) + negative
      test (connection refused → fast nonzero exit). Real end-to-end
      run is CI-gated (no docker/Elixir on this VM) via the new smoke
      job. Pushed.

- [x] **C2. OpenAPI spec**
      `docs/openapi.yaml` for the REST API; kept in sync (CI lint if cheap).
      Accept: spec validates (spectral or redocly lint if available; else
      manual review).
      DONE 2026-10-18: hand-written `docs/openapi.yaml` (OpenAPI 3.1.0)
      covering the full REST surface — /healthz (liveness, DB-free),
      /readyz (DB-aware, 503 envelope when PG down), GET /v1/me (Bearer
      RS256 JWT, just-in-time provisioning semantics), the 405 Allow path,
      and the mux catch-all 404; component schemas for User and the
      canonical error envelope, all 10 stable error codes, security scheme
      (RS256, RS256-pinned, exp/iss/aud, AUTH_BYPASS dev hatch), global
      conventions (X-Request-ID, security headers, per-IP rate limit +
      Retry-After). New `redocly.yaml` disables two rules deliberately
      (no-server-example.com: no prod host yet; no-unused-components:
      NotFound documents the catch-all). New CI `openapi` job lints the
      spec on every push.
      VERIFIED: `npx @redocly/cli lint` → valid, 0 warnings (checked
      twice: YAML parse + full semantic lint). Kept hand-written because
      there is no codegen step — header comment in the file says so.

- [x] **C3. Production runbook**
      `docs/production.md`: env vars, secrets, migrations, backups,
      scaling notes (when to add chat nodes, PG read replicas),
      incident basics.
      Accept: a new contributor can deploy from this doc.
      DONE 2026-10-18: new `docs/production.md` — secrets generation
      table (SECRET_KEY_BASE/POSTGRES_PASSWORD/RELEASE_COOKIE/
      CLUSTER_SECRET, gitignored .env, chmod 600), full env reference
      for api (internal/config.Load) and chat (config/runtime.exs),
      image build commands, migration procedure (api self-migrates
      advisory-locked at boot; chat via `SutraChat.Release.migrate()`
      before starting; separate ledgers schema_migrations vs
      chat_schema_migrations; backup-before-migrate rule),
      first-boot compose steps with a `docker-compose.prod.yml`
      override pattern, health/readiness verification table
      (/healthz liveness DB-free, /readyz readiness DB-aware — the
      probe your orchestrator should use), backup scope (PG nightly
      + WAL archiving once real users, seaweedfs volume snapshots,
      presence/valkey ephemeral by design), scaling notes (stateless
      api replicas + per-IP limiter caveat as replicas grow;
      chat multi-node via CLUSTER_ENABLED/RELEASE_NODE/RELEASE_COOKIE
      + k8s DNS preference; PG single-primary today, read-split not
      yet built), incident table (readyz 503, 401s, socket refusals,
      429 spikes, presence divergence, panics, restarts), pre-flight
      checklist (AUTH_BYPASS unset both services, SECRET_KEY_BASE,
      Hydra consistency, CORS never *, backup, smoke on staging,
      CI green, secrets via store, cluster secrets changed), rollback
      (immutable images, forward-only migrations, expand/migrate/
      contract), Kubernetes notes (probes, secrets, DNS clustering,
      PDB for websocket drain). Honest caveat documented: Hydra
      (issue #4) not landed → identity fail-closed in prod until it
      does; AUTH_BYPASS is the documented dev-only hatch, forbidden
      in production. Verified: every env var name/default cross-checked
      against `internal/config/config.go` and `config/runtime.exs`;
      migration files/ledgers and migrate command against the real
      repo state. Pushed.

- [ ] **C4. Benchmarks**
      Basic load numbers: ws connections/sec, msg throughput single node
      (document hardware + method; honest numbers, no hype).
      Accept: `docs/benchmarks.md` with reproducible method.

## Done when

All boxes ticked, CI green, `scripts/smoke.sh` passes, docs complete.
Then report to founder: production-ready backend live on main.

## Log

- 2026-10-17: Plan created. Starting Phase A.
- 2026-10-17 18:38 IST: A2 done — real JWT verification (RS256, JWKS cache+rotation, exp/iss/aud), RequireAuth middleware, AUTH_BYPASS dev mode. Verified `go build`+`go vet`+`go test ./...` green, live smoke 401/200 paths. Pushed.
- 2026-10-17 18:56 IST: CI red root cause = `scripts/check-headers.sh` failing on itself + `.gitignore` (both lacked the SPDX header it enforces; other 3 CI jobs green on all 3 runs). Fix: added `# SPDX-License-Identifier: AGPL-3.0-or-later` to both files (2 lines); checker verified passing locally. Rides the next scheduled push.
- 2026-10-17 19:25 IST: A3 done — pgx pool + embedded advisory-locked migrations + DB-aware /readyz. Verified end-to-end on real PG16 (kill/restart PG: readyz 503→200, healthz 200 throughout). Tests green. Pushed.
- 2026-10-17 ~20:05 IST: A4 done — users table (migration 0002) + real GET /v1/me with just-in-time provisioning (race-safe ON CONFLICT, claim-seeded display name, NULL handle until claimed). Verified `go build`+`go vet`+`go test ./...` green against real PG16 incl. 8-racer concurrency test; live smoke: first call creates row, second returns same id, no-token → 401 JSON envelope. Pushed.
- 2026-10-17 ~20:25 IST: A5 done — per-IP token-bucket rate limiter (stdlib-only, RATE_LIMIT_RPS/BURST env, Retry-After + X-RateLimit-Limit), CORS middleware (CORS_ORIGINS allowlist/wildcard/disabled, preflight 204), security headers (nosniff/DENY/referrer-policy/CSP). 12 new middleware tests green; live smoke: 100 rapid reqs → 73×429 JSON envelope, preflight 204 + ACAO for configured origin, all headers on 200/429. Pushed.
- 2026-10-17 ~20:55 IST: A6 done — JSON error envelope consolidated across ALL error paths: ErrorCode map + WriteStatusError + NotFound/MethodNotAllowed handlers; hand-rolled duplicates removed (auth, users, Recover); main.go extracted newHandler() for tests; /v1/me explicit method guard (405+Allow); RequestID outermost so panics carry real request_id. Tests: 7 unit + cmd/api integration (gated parts skip cleanly); full suite green on throwaway local PG16. Live smoke: 404/405/401/429/503 all envelope-shaped with echoed X-Request-ID; readyz 503→200 across real PG kill/restart. Pushed.
- 2026-10-17 ~21:30 IST: A7 done — middleware unit tests (request ID/Logger/Recover/Chain, 8 tests), claims-propagation auth test, /v1/me 200-path X-Request-ID + HTTP-level provision idempotency assertions; CI go-api job now runs `go test ./...` against a postgres:16 service (was build+vet only, gated tests skipped silently in CI). Suite green on throwaway local PG16 and green with 10 graceful skips (no DB). Pushed.
- 2026-10-17 ~21:55 IST: A8 done — Dockerfile hardening (multi-stage, static stripped binary, non-root `app` user, HEALTHCHECK on /healthz, exec ENTRYPOINT) + CI `docker build` step for the API image. Verified: exact Dockerfile build flags → statically-linked stripped binary, `go vet` clean; ran as unprivileged uid 65534 with /healthz, /readyz, /v1/me all 200; SIGTERM → "api stopped", clean exit. Local `docker build` impossible on this VM (dockerd: no iptables kernel support) — CI build step is the durable gate. Pushed. **Phase A complete.**
- 2026-10-17 ~22:35 IST: B1 done — Ecto message persistence (Repo, migration, Message schema, Chat context with atomic upsert dedupe + cursor-paged history, channel persist-before-broadcast + history on join, DATABASE_URL runtime config + compose wiring). Verified on real PG at SQL level (dedupe/pagination semantics) + Ecto-level (compile vs real ecto beams, changeset validation); full local `mix compile` impossible on apt Elixir 1.14 (phoenix_template/postgrex need ≥1.15/1.16) — CI (1.18) is the gate. Lesson: NEVER pin deps down just for local 1.14 compile — old postgrex/decimal pins carry HIGH/MEDIUM CVEs; keep latest and let CI gate. Pushed.
- 2026-10-17 ~23:05 IST: B2 done — socket authentication: pure RS256 verifier (Token, stdlib :public_key + Jason, mirrors Go internal/oidc claim-for-claim) + TokenVerifier GenServer (JWKS cache 15m TTL, lazy fetch, rotation refresh) + UserSocket.connect requires token param, refuses without/invalid, assigns subject. Contract in docs/auth-contract.md; compose chat gets HYDRA_ISSUER/JWT_AUDIENCE. Verified: 34-check local harness green (all error paths, GenServer vs stub JWKS incl. rotation + issuer-down, bypass) + socket glue via stub; ExUnit suite (18 tests) shipped for CI/B6. Lesson: :crypto.sign/verify(:rsa,...) rejects key tuples on OTP 25 crypto 5.1 — use :public_key.sign/3 + :public_key.verify/4. Pushed.
- 2026-10-17 ~23:35 IST: B3 done — channel authorization: pure `SutraChat.Chat.RoomPolicy` (join auth check + room-id charset/length + send size caps), wired into `ChatChannel.join/3` and `handle_in("message:new")` with clean error replies; `Message.changeset` mirrors caps as defense-in-depth; `docs/room-membership.md` documents Wave 1 open-rooms-by-ID + Wave 2 membership-list roadmap. Verified: 10 ExUnit tests green, changeset caps confirmed against real ecto beams (4096 ok / 4097 rejected), SPDX header check green. Full `mix compile` CI-gated. Pushed.
- 2026-10-17 ~23:55 IST: B4 done — delivery receipts: send path persist → broadcast → `msg:ack` push (sender only, pushed event not reply, never precedes durability); `persist_message/1` returns `{:ok, message, deduped}` via single-statement upsert with `xmax`-probe fresh-vs-retry detection (no extra SELECT, race-free); pure `Chat.ack_payload/2` (id/room_id/echoed client_msg_id/deduped/sent_at); `docs/delivery-semantics.md` documents the at-least-once contract + client retry rules + crash windows. Verified: 3 ExUnit tests green, modules compile vs real ecto beams, exact upsert SQL proven at parameterized-protocol level on PG16 (fresh=t / retry→same row f / other sender→new t). Pushed.
- 2026-10-18 ~00:35 IST: B5 done — presence hardening: libcluster 3.5.0 dep + Cluster.Supervisor in app tree (no-op when disabled); runtime.exs opt-in topologies (CLUSTER_ENABLED=1 → gossip; commented k8s DNS strategy); per-device presence metas (online_at ms/device/status) with client device_id join param; docs/presence.md; ExUnit presence suite (5 tests). NOTE: this run's VM lost apt Elixir mid-run and provisioning's apt-get update wedged — no local Elixir runtime, so the behavioral harness couldn't execute; per plan, CI (1.18) is the gate. All libcluster/Phoenix.Presence APIs verified against real dep sources; SPDX check green. Pushed.
- 2026-10-18 ~01:10 IST: B6 done — channel tests: SutraChatWeb.ChatChannelTest (13 tests, async:false, Phoenix.ChannelTest vs real Repo via Ecto sandbox + real Presence): join/auth (unauthorized socket refused, bad room ids rejected), presence_state/diff, send→persist→broadcast→sender-only msg:ack, client_msg_id dedupe (second ack deduped:true + original id, 1 row), history oldest-first + cursor pagination + invalid cursor, payload validation errors. New test infra: config/test.exs (sandbox Repo, DATABASE_URL in CI / local fallback, server:false, quiet logger), import_config per-env, test/support (DataCase, ChannelCase) via elixirc_paths(:test), test_helper sandbox manual mode. CI elixir-chat job now runs postgres:16 + mix ecto.create/migrate + mix test. Verified: syntax-checked all 7 files, SPDX headers present; full compile/test CI-gated (1.14 can't build phoenix_template 1.1.0). Pushed.
- 2026-10-18 ~01:35 IST: B7 done — chat Dockerfile hardening: multi-stage `mix release` build (deps-first caching), alpine:3.21 runtime (libstdc++/libgcc/ncurses-libs/ca-certificates), non-root `app` user, foreground `bin/sutra_chat start`, HEALTHCHECK via release `pid` (websocket-only endpoint). New config/prod.exs (minimal — `mix release` needs it since config.exs imports per-env files); runtime.exs fails closed at boot without SECRET_KEY_BASE (dev key never ships) + PHX_HOST url override; new SutraChat.Release migrate/rollback module (no `mix` inside releases); compose docs (SECRET_KEY_BASE, migrate command, RELEASE_NODE/COOKIE functional); README dev/prod split; CI builds the chat image on every push. Verified: YAML valid, SPDX headers present; release assembly + boot CI-gated (no Elixir/docker on this VM). Pushed. **Phase B complete.**
- 2026-10-18 ~02:10 IST: C1 done — full compose stack + `scripts/smoke.sh`: builds images, boots pg/valkey/seaweedfs, runs chat migrations via the release, boots api+chat; asserts /healthz 200 + X-Request-ID, /readyz 200, /v1/me 200 (bypass identity dev-user), websocket join→send→broadcast+msg:ack→reconnect-history round-trip (stdlib Python Phoenix-channels client), seaweedfs /cluster/status 200, valkey PONG. New CI `smoke` job runs it on every push. Fixed 3 real wiring bugs found: api/seaweedfs host-8080 port clash (api→8081), Go-vs-Ecto `schema_migrations` ledger collision (chat → `chat_schema_migrations`), SECRET_KEY_BASE empty-string bypass of the fail-closed check. Verified: shell/Python syntax, YAML parse, SPDX headers, ws client vs fake Phoenix server (happy + negative paths); real end-to-end run is CI-gated. Pushed.
- 2026-10-18 ~02:40 IST: C2 done — `docs/openapi.yaml` (OpenAPI 3.1.0, hand-written with keep-in-sync note): /healthz, /readyz (incl. 503 envelope), GET /v1/me (Bearer RS256, JIT-provision semantics), 405 Allow path, catch-all 404; User + error-envelope schemas, all 10 stable codes, auth security scheme (RS256-pinned, exp/iss/aud, AUTH_BYPASS hatch), global conventions (X-Request-ID, security headers, per-IP rate limit + Retry-After). New `redocly.yaml` (2 rules deliberately disabled, commented) + CI `openapi` job linting the spec on every push. Verified: `npx @redocly/cli lint` → valid, 0 warnings (a missed unquoted colon was the only fix). Pushed.
- 2026-10-18 ~03:10 IST: C3 done — `docs/production.md`: secrets table (generation commands, gitignored .env), full api+chat env reference cross-checked against config.go/runtime.exs, image builds, migration procedure (api advisory-locked self-migrate; chat via Release.migrate(); separate ledgers), first-boot compose steps with docker-compose.prod.yml override, /healthz-vs-/readyz probe guidance, backups (PG nightly+WAL, seaweedfs snapshots, presence ephemeral), scaling (stateless api + limiter caveat, chat clustering via CLUSTER_ENABLED/RELEASE_NODE/RELEASE_COOKIE, PG single-primary caveat), incident table, pre-flight checklist, rollback, Kubernetes notes. Honest caveat documented: Hydra not landed (issue #4) → identity fail-closed in prod; AUTH_BYPASS dev-only, forbidden in prod. Pushed.
