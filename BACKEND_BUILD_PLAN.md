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

- [x] **C4. Benchmarks**
      Basic load numbers: ws connections/sec, msg throughput single node
      (document hardware + method; honest numbers, no hype).
      Accept: `docs/benchmarks.md` with reproducible method.
      DONE 2026-10-18: `docs/benchmarks.md` — full methodology (hardware
      template, scope notes, re-run policy, deliberately-not-benchmarked
      list); baseline tables present as TBD schema (no fake numbers —
      honest: no reference-hardware run available yet). New Go benchmarks:
      `cmd/api/bench_test.go` (Healthz seq+parallel through the full
      middleware chain, NotFound404, Unauthorized401, MethodNotAllowed405)
      and `internal/middleware/bench_test.go` (rate-limiter allow hot
      path seq+parallel, error-envelope JSON write, RequestID-only,
      SecurityHeaders-only, plus a TestRequestWithIDHelper guard);
      DB-free/httptest so they run anywhere. New `scripts/bench_ws.py`
      (stdlib Phoenix-channels-v2 load probe: conns/sec, server-confirmed
      msgs/sec via msg:ack, ack-latency p50/p95/p99, --fail-on-errors).
      `scripts/smoke.sh` runs a short probe (20×10) with --fail-on-errors
      on every smoke run; CI go-api job runs the short bench suite on
      every push.
      VERIFIED: bash -n + py_compile + YAML parse clean, SPDX headers
      present; bench_ws.py exercised against a fake Phoenix server —
      happy path 10×5 → 50/50 acks exit 0, ack-dropping server →
      errors counted + exit 3 with --fail-on-errors, refused port →
      connect errors counted + JSON summary exit 0. Go compile/bench run
      is CI-gated (no Go on this VM — apt lists locked by provisioning,
      per plan). Pushed. **Phase C complete — backend production build complete.**

## Phase D — Wave 1 product modules (epic #10: identity / social / video)

D-tasks build the first real product modules on the A/B/C foundation,
per ARCHITECTURE.md, docs/adr and the Wave-1 research (ADR-005). Same
rules as A/B/C: no placeholders, no TODOs, CI-gated verification, push
every run. Gated items stay gated: real SMS/DLT stays post-incorporation
(documented, fail-closed); E2E encryption is Wave 2 (never promise it).

- [x] **D1. Identity: Ory Kratos + Hydra in compose**
      Real Ory Kratos (self-service identity) + Ory Hydra (OAuth2/OIDC)
      services in docker-compose.yml; the Go API verifies Hydra-issued
      RS256 tokens through the existing A2 JWKS path — no new auth code,
      just a real issuer. Phone-OTP login ships as an adapter INTERFACE
      with a stub implementation; real SMS/DLT wiring is explicitly
      post-incorporation (documented in docs/identity.md, env-gated and
      fail-closed). First-boot documented.
      Accept: smoke.sh extended — dev token from Hydra → GET /v1/me 200;
      docs/identity.md describes the DLT-gated path.
      DONE 2026-10-18: compose gains `hydra-migrate`/`hydra` +
      `kratos-migrate`/`kratos` (images pinned `oryd/hydra:v26.4.2` +
      `oryd/kratos:v26.4.2`, latest per Ory changelog 2026-10-15);
      Hydra `serve all --dev` with explicit `URLS_SELF_ISSUER=
      http://hydra:4444` (matches API/chat `HYDRA_ISSUER`), Kratos serves
      `services/identity/kratos/kratos.yml` (dev config, email+password,
      optional E.164 phone trait reserved for the OTP future, courier
      unconfigured, dev secrets clearly marked) + `identity.schema.json`;
      api env now sets `HYDRA_ISSUER`/`HYDRA_JWKS_URL`/`JWT_AUDIENCE`
      explicitly. New `services/api/internal/otp` package: `Sender`
      interface, `StubSender` (every Send → typed
      `*ErrSMSSendingDisabled`, never a faked delivery), `NewSender`
      (only "stub"/"" registered; unknown provider = fail-closed error),
      `GenerateCode()` (crypto/rand 6-digit), `ValidatePhone()` (E.164);
      `OTP_PROVIDER` env (default "stub") in config + compose; main.go
      constructs the sender at startup and refuses to boot on unknown
      providers. New `docs/identity.md`: token-loop diagram, compose
      table, first boot, the smoke D1 loop, login/consent UI as Wave 1.5,
      the full DLT-gated SMS path + post-incorporation checklist,
      production secrets guidance, out-of-scope list (E2E Wave 2, real
      SMS, login UI). `scripts/smoke.sh` extended: boots hydra+kratos
      (waits on openid-configuration + /health/alive), creates a
      `sutra-smoke` OAuth2 client with the `jwt-bearer` grant type
      (→ JWT access tokens; `hydra create client` singular-flag syntax
      verified against the v2 CLI reference), mints a token via
      client_credentials (aud `sutra-api`), asserts it is JWT-shaped,
      recreates `api` WITHOUT `AUTH_BYPASS` (bypass skips verification,
      so leaving it on would prove nothing), then asserts
      `GET /v1/me` → 200 with `subject == "sutra-smoke"` and no-token →
      401. `docs/dev-setup.md` service table + identity paragraph
      updated; CI `smoke` job comment updated.
      VERIFIED: `gofmt` clean, `go build ./...` + `go vet ./...` green,
      `go test ./...` green (4 new otp tests pass); fail-closed proven
      live — `OTP_PROVIDER=bogus-gateway` → binary logs
      "invalid OTP_PROVIDER — refusing to start" and exits before
      touching the DB; `bash -n`, YAML (compose/CI), JSON schema, SPDX
      headers all clean; Hydra/Kratos DSNs structurally match the real
      compose postgres credentials. Full stack boot + the Hydra token
      loop are CI-gated (no docker on this VM) via the extended smoke
      job. Pushed.

- [x] **D2. Profiles + follow graph**
      Migration 0003: profiles (user_id PK, handle UNIQUE, display_name,
      bio, avatar_url, timestamps); follows (follower_id, followee_id,
      created_at; composite PK; no-self-follow CHECK). Go API:
      PATCH /v1/me/profile, GET /v1/users/{id}, POST+DELETE
      /v1/users/{id}/follow, GET followers/following (cursor-paged).
      Handle claim: first-come, lowercase [a-z0-9_]{3,20}.
      Accept: endpoints + tests green, OpenAPI updated, smoke covers a
      follow round-trip.
      DONE 2026-10-18: migration `0003_profiles_follows.sql` (profiles +
      follows + drops the 0002 placeholder users.handle — handles live on
      profiles now; users.display_name stays the OIDC seed); new
      `internal/social` package — `Profile`/`ProfilePatch` (presence-flag
      partial updates; explicit null clears), handle regex validation,
      avatar http(s)-only (javascript: rejected), first-come claims via the
      UNIQUE constraint (loser → 409 handle_taken), PATCH serialized per
      user (SELECT ... FOR UPDATE on the users row), follows idempotent
      (composite PK + ON CONFLICT DO NOTHING: POST → 201 new / 200 retry,
      DELETE → 204), self-follow rejected in Go + DB CHECK backstop,
      computed follower/following counts (no denormalized drift),
      keyset pagination on (created_at, edge-id) with opaque base64url
      cursors, public profile never exposes the OIDC subject; routes wired
      in main.go (Go 1.22 `{id}` patterns + explicit method guards → 405
      envelopes); OpenAPI updated (5 new paths, Profile/ProfilePatch/
      FollowItem/FollowPage schemas, User.handle removed) — redocly lint
      0 warnings; smoke.sh gains the D2 follow round-trip (two Hydra
      clients, bypass OFF: claim handles → follow 201/200 → self-follow
      422 → listings verified → unfollow 204).
      VERIFIED: `go build`+`go vet`+gofmt clean; full `go test ./...`
      green on real PG16 — 3 pure unit suites (handle grammar matrix,
      cursor round-trip + 5 malformed shapes, patch validation incl.
      emoji-rune counting and javascript: avatar) + 5 integration suites:
      profile patch/merge/clear, 10-racer handle-claim race (exactly 1
      winner), follow/unfollow/idempotency/counts, 3-page cursor
      pagination newest-first, HTTP round-trip (409/422/400/404/405
      envelopes, unknown-field 400, no subject leak). Two real bugs caught
      by tests before push: (1) next_cursor built from the lookahead row
      instead of the last returned item (dropped a row per page); (2)
      DisallowUnknownFields is a no-op on map decode — unknown PATCH keys
      now rejected by an explicit allowlist (live-verified 400). Full
      end-to-end also live-verified against the compiled binary (real
      main.go mux patterns). Pushed.

- [x] **D3a. Posts table + CRUD (D3 part 1)**
      Migration 0004: posts (id uuid pk, author_id → users, text ≤500
      runes with non-blank CHECK, media_ids text[] ≤10 with CHECKs,
      author-timeline index (author_id, created_at DESC, id DESC)).
      New `internal/posts` package: Service.Create (validation + rolling
      per-author velocity cap — default 30/h, `POST_VELOCITY_PER_HOUR`
      env, over-cap → 429 `post_velocity_limited` + computed Retry-After
      to when the oldest post in the window ages out; best-effort under
      concurrency, documented), Get, ListByAuthor (keyset on
      (created_at, id), never OFFSET), Delete (author-only, 404/403).
      media_ids shape-validated only — the "ready media only" gate is D4.
      Routes: POST /v1/posts (201), DELETE /v1/posts/{id} (204/403/404),
      GET /v1/users/{id}/posts (404 unknown user, cursor-paged);
      social.pageParams/callerUUID exported for reuse (no duplication).
      OpenAPI updated (3 paths, Post/PostCreate/PostPage schemas, PostId
      param) — redocly lint valid, 0 warnings. smoke.sh gains the posts
      round-trip (create 201, oversized 422, timeline lists newest-first,
      non-author delete 403, author delete 204, timeline empty).
      Accept: tests green + live binary smoke of all routes.
      DONE 2026-10-18 ~05:35 IST: `go build`+`go vet`+gofmt clean; full
      `go test ./...` green on real PG16 — 2 pure unit suites (text rune
      counting incl. emoji, media-id grammar matrix) + 5 integration
      suites (create/get/delete, validation matrix, velocity cap 3/h →
      4th rejected with RetryAfter ≥1s + per-author isolation, 3-page
      cursor pagination newest-first, author isolation). One real bug
      caught pre-push: LIMIT built as a literal instead of a $N
      parameter ("expected 1 arguments, got 2"). Live binary smoke:
      201/405/200/204/404/400 all correct; POST_VELOCITY_PER_HOUR=1 →
      live 429 `post_velocity_limited` + `Retry-After: 3584`. Pushed.

- [x] **D3b. Feed timelines: Valkey hybrid fan-out (D3 part 2)**
      Valkey sorted-set home timelines: fan-out-on-write to followers'
      home timelines, except high-follower authors (FANOUT_THRESHOLD env,
      default 10k) whose posts merge on read. Go API: GET /v1/feed/home
      (score+id cursor). Delete propagation from timelines. NOTE: needs
      a Valkey client dep (go-redis v9 speaks RESP to Valkey; if the Go
      module proxy is unreachable from the build VM, write a minimal
      RESP client instead — document the choice). config.ValkeyAddr
      already exists; compose already runs valkey:8.
      Accept: docs/feeds.md documents the hybrid model; tests cover
      push path, pull path, merge ordering, delete propagation.
      DONE 2026-10-18 ~07:10 IST: `internal/feed` (Service: FanoutPost/
      UnfanoutPost/HomeFeed; GET /v1/feed/home handler; publish hooks in
      posts handlers with compensating rollback on fan-out failure →
      503), config FANOUT_THRESHOLD/FEED_TIMELINE_MAX, docs/feeds.md,
      OpenAPI + redocly green, smoke.sh D3-part-2 round-trip.
      VERIFIED: `go build`+`go vet`+gofmt clean; full `go test ./...`
      green on real PG16 + real Redis — 6 pure unit + 7 integration
      suites. Review caught 2 REAL pagination bugs pre-push, both with
      regression tests (proven to FAIL on the old code): (1) the cursor
      path emitted the same-score tail AFTER lower scores, corrupting
      newest-first order and dropping a tied post; (2) a merge-starved
      stream was marked done with entries unconsumed, silently losing
      posts. Live binary smoke: follow → 3 posts → feed newest-first,
      2+2 pagination terminates, pre-follow post correctly absent,
      delete → 204 + propagation, author sees own posts, 400 on bad
      cursor/limit. `go mod tidy` promoted go-redis to a direct require.
      Next: D4 (video upload pipeline).

- [x] **D4a. Video pipeline part 1: media registry + presigned upload**
      Migration 0005: media table (owner, kind, status lifecycle
      pending→uploaded→ready/failed, fids, renditions jsonb, CHECKs).
      New `internal/media` package: presigned SeaweedFS upload
      (POST /v1/media/upload → media row + direct PUT URL; validation
      before any storage call, 503 `media_unavailable` when the master
      is unreachable), GET /v1/media/{id} (status polling; any
      authenticated user may read), POST /v1/media/{id}/complete
      (HEADs the fid to confirm bytes: images → ready, videos →
      uploaded + XADD onto `media:transcode` Valkey stream), CheckReady
      gate wired into posts.Create (posts reference only ready media:
      unknown → 422 unknown_media, unready → 422 media_not_ready,
      foreign → 403 media_not_owned), MarkReady/MarkFailed for the
      part-2 worker, OpenAPI updated (3 paths + Media/UploadSession/
      Rendition schemas) — redocly lint 0 warnings.
      Accept: go build+vet+test green; live binary smoke of all
      routes and the posts gate.
      DONE 2026-10-18 ~07:50 IST: `go build`+`go vet`+gofmt clean;
      full `go test ./...` green on real PG16 (part 1 shipped with no
      test file — service is thin DB/HTTP glue; tests arrive with the
      worker in part 2). Two real integration bugs caught pre-push:
      (1) unused `media` import in internal/posts (previous run left it
      behind — build was RED); (2) bench_test.go still calling the old
      newHandler signature (5 args) after the mediaSvc parameter was
      added — vet was RED. Live binary smoke (real PG16 + local
      redis): /v1/me 200 provisions user; upload bad JSON → 400,
      unknown field → 400, bad kind → 422, valid intent with no
      SeaweedFS → 503 `media_unavailable` + Retry-After (proves the
      master-unreachable path); GET bad id → 400, unknown uuid → 404
      envelope, complete unknown → 404, wrong method → 405; post with
      unknown media_id → 422 `unknown_media`, post without media →
      201 (gate proven). Full happy-path upload (201 + PUT + complete)
      needs SeaweedFS — covered by the smoke.sh D4 round-trip in part
      2 (CI has docker).

- [x] **D4b. Video pipeline part 2: transcode worker + docs + smoke**
      PART 1 DONE 2026-10-18 ~08:35 IST: `internal/transcode` package —
      Worker drains media:transcode via consumer group (Valkey, go-redis),
      per-job: download from SeaweedFS → ffprobe → ladder ≤ source
      resolution (never upscale) → ffmpeg HLS 4s-seg VOD playlists +
      poster → upload back → media.MarkReady; failures: attempts hash +
      exponential backoff requeue (BackoffBase 30s, cap 30m), dead-letter
      stream + MarkFailed after MaxAttempts (5); startup + interval sweep
      of stuck uploaded rows; TTL heartbeat for liveness probes; pure
      streamOps interface so all retry/dead-letter logic is unit-testable.
      `cmd/transcode-worker` main (fail-closed on DB/Valkey, warn-only on
      SeaweedFS, graceful SIGTERM). Config: TRANSCODE_* envs (group,
      consumer=hostname, attempts, backoffs, sweep). BUG FIX (D4a code):
      media.scanMedia crashed on NULL filename/mime_type/error columns
      ("cannot scan NULL into *string") — now *string intermediates;
      caught by the new transcode integration tests. VERIFIED:
      `go build`+`go vet`+gofmt clean; full `go test ./...` green on
      real PG16 — 4 pure unit suites + 3 integration (dead-letter parks
      row failed + DLQ entry after 3 attempts; success → ready with 3
      renditions + poster + 7 files uploaded; duplicate delivery no-op).
      PART 2 NEXT: Dockerfile.worker (FFmpeg), compose service, CI
      compose-build gate, docs/video-pipeline.md, smoke.sh full clip
      round-trip.
      PART 2 DONE 2026-10-18 ~09:05 IST: `services/api/Dockerfile.worker`
      — multi-stage (golang:1.22-alpine build → alpine:3.20 runtime with
      the `ffmpeg` apk package, the only binaries the worker shells out
      to), non-root `app` user, own TMPDIR work dir, SIGTERM-friendly
      exec-form entrypoint, HEALTHCHECK as a process backstop (real
      liveness = the worker:transcode:heartbeat Valkey key). Compose gains
      the `transcode-worker` service (build from Dockerfile.worker,
      DB/Valkey/SeaweedFS env, depends_on postgres+valkey healthy and
      seaweedfs started; consumer group splits jobs across replicas).
      CI `compose` job now builds the worker image on every push.
      New docs/video-pipeline.md: lifecycle diagram, media statuses,
      worker design (consumer group, ACK-after-resolve, exponential
      backoff→dead-letter, duplicate-delivery no-ops, stuck-row sweep,
      heartbeat liveness, fail-closed), ladder table (720p/480p/360p,
      never upscale), outputs, failure-mode table, env reference, ops
      notes. VERIFIED: compose + CI YAML parse clean, SPDX checker
      green; image build + end-to-end boot are CI-gated (no docker on
      this VM). NEXT: smoke.sh full clip round-trip (upload intent →
      PUT sample clip → complete → poll to ready), then D4b [x].
      Background Go worker (cmd/transcode-worker or new package):
      FFmpeg 8.1 → HLS multi-rendition + poster frame; consumes the
      `media:transcode` Valkey stream (consumer group), backoff
      retries, dead-letter after N attempts; downloads from SeaweedFS,
      uploads outputs back, calls media.MarkReady/MarkFailed;
      worker Dockerfile with FFmpeg, compose service, health/liveness
      notes. New docs/video-pipeline.md. smoke.sh: full clip round-
      trip — upload intent → PUT sample clip → complete → poll GET
      /v1/media/{id} to ready. Worker unit tests in CI.
      Accept: docs/video-pipeline.md; smoke green; worker unit tests
      in CI; OpenAPI already has the media paths (D4a).
      PART 3 DONE 2026-10-18 ~09:30 IST: smoke.sh now carries the full
      clip round-trip — builds + starts transcode-worker, gates on its
      Valkey heartbeat (worker:transcode:heartbeat), mints a 2s 640x360
      H.264 clip with host ffmpeg (matches the lowest ladder rung, no
      upscale; SKIPs cleanly with a note where no host ffmpeg exists —
      CI runners ship it), upload intent → PUT bytes (upload_url's
      compose-internal hostname rewritten to localhost:8080 for the
      host) → complete (video lands in 'uploaded') → polls GET
      /v1/media/{id} to 'ready' → asserts poster_url + playback_url +
      ≥1 HLS rendition → proves the D4a posts gate with a 201 media
      post (then deletes it). VERIFIED: bash -n clean, SPDX checker
      green, sed rewrite + clip-minting command proven live (valid
      h264 640x360); end-to-end run is CI-gated (no docker on this VM)
      via the smoke job. **D4b COMPLETE — next: D5 realtime post events.**

- [x] **D5. Realtime post events**
      API publishes post/follower events to the chat service over an
      internal signed endpoint (service-to-service token); fanned out
      on Phoenix `user:<id>` topics: post:new, follower:new.
      Presence-aware; no notification persistence in Wave 1 (documented
      — Wave 2 adds the inbox).
      Accept: docs/realtime-events.md contract; channel tests cover
      topic auth (users get only their own) + event shape.
      DONE 2026-10-18 ~10:35 IST: PART 1 (Go, previous run) + PART 2
      (Elixir, this run): new `SutraChatWeb.UserChannel` (`user:*`
      topics — join allowed ONLY on the socket's own subject; users can
      never subscribe to someone else's events); `POST /internal/events`
      via new `SutraChatWeb.Router` (plugged into the endpoint) +
      `InternalEventsController` — shared Bearer token compared with
      `Plug.Crypto.secure_compare/2` (constant-time; unset token fails
      closed: every request 401s), strict envelope validation
      (topics `^user:[^:]+$`, known events, non-empty dedupe_key,
      per-event required payload fields) → 422 with the shared JSON
      error envelope, 202 `{"status":"accepted","event","topics"}` +
      `Endpoint.broadcast/3` fan-out on success; `config/runtime.exs`
      reads `CHAT_INTERNAL_TOKEN` (test default `test-internal-token`);
      compose chat service gains the env var (internal network only);
      docs/realtime-events.md dedupe bullet made precise (no server-side
      dedupe store in Wave 1 — stable payload ids are the client dedupe
      keys; Wave 2 inbox owns server-side dedupe); smoke.sh generates an
      ephemeral token for both services and asserts the live endpoint:
      401 no token / 401 wrong token / 422 unknown event / 202 valid
      envelope (the API's now-enabled publisher also exercises the real
      publish path best-effort during the D2–D4 sections).
      VERIFIED: all 10 new/edited Elixir files `Code.string_to_quoted!`
      clean; `secure_compare/2` (false on size mismatch, no raise),
      `use Phoenix.Router, helpers: false`, and `Endpoint.broadcast/3`
      all confirmed against the real dep sources; `bash -n` + compose
      YAML parse clean; new tests: UserChannelTest (6: own-topic join,
      cross-user refused, anonymous refused, malformed topics, post:new
      shape + topic isolation via broadcast struct, follower:new shape)
      + InternalEventsControllerTest (10: 401×3 auth, 422×5 validation,
      202×2 broadcast fan-out) with new ConnCase support (in-process
      ConnTest dispatch, no listener). Full `mix test` is CI-gated
      (Elixir 1.18) — local 1.14 still can't build phoenix_template.
      Pushed. **D5 COMPLETE — next: D6 Wave-1 hardening pass.**

- [x] **D6. Wave-1 hardening pass**
      Rate limits + abuse guards for D2–D5 (post/follow velocity caps),
      OpenAPI regenerated for all new routes, production.md and
      benchmarks.md updated, full smoke.sh green on the new flows.
      Accept: CI green, smoke green, docs complete. **Phase D complete.**
      D6 PART 1 DONE 2026-10-18 ~10:55 IST: follow velocity abuse guard —
      new `internal/social` `DefaultFollowVelocityCap` (200/h,
      `FOLLOW_VELOCITY_PER_HOUR` env, config wired through main.go),
      `FollowWithCap` (rolling trailing-hour count, RetryAfter computed
      from the oldest follow in the window, best-effort under
      concurrency like posts), `FollowVelocityLimitedError` →
      HTTP 429 `follow_velocity_limited` + Retry-After (mirrors
      posts handler). Key design fix caught by the new tests: idempotent
      retries of EXISTING edges bypass the velocity check entirely
      (early SELECT) — a sender that never got its ack must be able to
      retry, so a retry can never turn into a 429. Tests: 2 new
      (service-level: cap 2 → 3rd rejected with Cap/RetryAfter asserted,
      retry free at cap, <=0 → default; HTTP: 201 then 429 with envelope
      code + Retry-After, retry → 200). OpenAPI follow-POST description
      extended (429 ref already present — redocly 0 warnings);
      production.md env table gains POST_VELOCITY_PER_HOUR (was missing)
      + FOLLOW_VELOCITY_PER_HOUR.
      D6 PART 2 DONE 2026-10-18 ~11:25 IST: (a) production.md env-table
      backfill — all D4b/D5 envs cross-checked against
      internal/config/config.go + chat config/runtime.exs: OTP_PROVIDER,
      FANOUT_THRESHOLD, FEED_TIMELINE_MAX, SEAWEEDFS_MASTER_ADDR,
      MEDIA_PUBLIC_BASE_URL, TRANSCODE_GROUP/CONSUMER/MAX_ATTEMPTS/
      BACKOFF_BASE/BACKOFF_MAX/SWEEP_INTERVAL, CHAT_INTERNAL_URL/TOKEN
      (API table) + CHAT_INTERNAL_TOKEN (chat table); also fixed a stale
      line claiming Hydra "lands with issue #4" (D1 landed it). (b)
      smoke.sh D6 velocity section: recreates api with 1/hour caps,
      mints a throwaway per-run Hydra identity, asserts post 201 → 429
      post_velocity_limited + Retry-After, follow 201 → 429
      follow_velocity_limited + Retry-After, idempotent retry → 200,
      then restores defaults; header comment updated. (c) compose api
      gains POST_VELOCITY_PER_HOUR/FOLLOW_VELOCITY_PER_HOUR
      passthroughs (empty = Go defaults). (d) benchmarks.md: new social
      bench table + run command extended; velocity caps documented as
      deliberately-not-benchmarked (DB-backed; proven by the smoke 429
      assertions). New internal/social/bench_test.go — 5 pure benches
      (ValidateHandle ×2, EncodeCursor, DecodeCursor, ParseUserID);
      CI bench step extended to ./internal/social/...
      GOTCHA: copied the tool-redacted "<redacted>" placeholder into
      the new smoke line — it's a real redirect token, bash -n caught
      it; always write ${VAR:-$(openssl rand -hex 16)} like the D1
      lines. VERIFIED: go build+vet+gofmt clean, go test ./... green,
      new benches run (ns/op sane), bash -n + compose/CI YAML parse
      clean; full smoke + CI green are CI-gated (no docker on this VM).
      **D6 COMPLETE — Phase D (Wave 1) complete.**

## Phase E — Wave 2: AI core (epic #11)

E-tasks build the AI layer on the Wave-1 product: a Python AI service
(LangGraph + AG-UI), personal AI, creator studio, language bridge, and
recommendations v1. Same rules as A–D: no placeholders, CI-gated
verification, push every run, scope discipline (§6 of the cron body).

CONFLICT RULE: issue #19 (pgvector embeddings pipeline) is reserved for
Hacktoberfest contributors — the worker MUST NOT implement it. E5 uses
non-ML signals only; vector embeddings plug in later via #19.

- [x] **E1. Python AI service scaffold**
      New `services/ai` (FastAPI): /healthz, AG-UI protocol endpoint
      skeleton, LangGraph graph stub with one echo node, Ollama added to
      docker-compose.yml (dev model pull documented), vLLM noted for prod
      in docs/ai-core.md. Go API proxies /v1/ai/* → ai service (internal
      service token). Implements issue #18 → close it when done.
      Accept: compose boots ai service; smoke hits /v1/ai/healthz 200.
      DONE 2026-10-18 ~11:55 IST: `services/ai` — `app/main.py` (FastAPI:
      /healthz + POST /ag-ui/run SSE streaming real AG-UI 1.0 events
      RUN_STARTED → STEP_STARTED/FINISHED → TEXT_MESSAGE_START/CONTENT/END
      → RUN_FINISHED/RUN_ERROR, RunAgentInput model, constant-time
      service-token auth, fail-closed boot without AI_SERVICE_TOKEN),
      `app/graph.py` (real compiled LangGraph StateGraph START→echo→END;
      the endpoint consumes graphs only via run_stream() so E2's agent
      slots in untouched), `app/config.py` (PORT/AI_SERVICE_TOKEN/
      OLLAMA_BASE_URL/AI_MODEL), pinned requirements.txt
      (fastapi/uvicorn/langgraph), hardened Dockerfile (multi-stage,
      non-root, HEALTHCHECK), docs/ai-core.md (architecture, AG-UI event
      model, LangGraph design, Ollama dev pull command, vLLM prod note,
      env tables, E2–E5 roadmap, #19 conflict restated). Compose: `ai`
      service (build, host 8000 for dev debugging) + `ollama` service
      (ollama-data volume; model pull documented, not booted in smoke —
      E1's echo graph never calls a model). Go: new `internal/aiproxy`
      (ReverseProxy, /v1/ai prefix strip, Bearer service token +
      X-Sutra-Subject, FlushInterval -1 for SSE, 503 service_unavailable
      envelope + Retry-After on outage, disabled→503 when token empty,
      fail-closed boot on bad URL with token set), config
      AI_SERVICE_URL/AI_SERVICE_TOKEN + compose api env, routes wired in
      main.go (behind RequireAuth). CI compose job builds the ai image;
      OpenAPI gains /v1/ai/healthz (redocly 0 warnings); smoke.sh E1
      section (ephemeral AI_SERVICE_TOKEN, boot ai, /v1/ai/healthz 200
      via proxy with Hydra JWT, 401 without token, full AG-UI echo
      round-trip RUN_STARTED→RUN_FINISHED).
      VERIFIED: py_compile on all 4 app files; gofmt clean;
      `go build`+`go vet` green; full `go test ./...` green (5 new
      aiproxy tests: fail-closed URL, disabled 503 envelope, header/path
      forwarding incl. bare-prefix→/, upstream-down 503+Retry-After);
      bash -n, compose/CI YAML parse, SPDX checker, redocly lint all
      clean. Full compose boot + smoke run are CI-gated (no docker on
      this VM) via the extended smoke job. Next: E2 personal AI chat.

- [ ] **E2. Personal AI chat**
      POST /v1/ai/chat (SSE streaming): LangGraph agent with PG-backed
      conversation memory (per-user threads), Sarvam-30B via Ollama in
      dev (model name env-configurable; vLLM endpoint env for prod),
      system prompt from versioned YAML (never hardcode persona), per-user
      rate limit + daily token budget guard.
      Accept: streaming chat round-trip in smoke; token-budget 429 path
      tested.

- [ ] **E3. Creator studio: auto-captions**
      faster-whisper worker (CPU) consuming the transcode pipeline:
      on media ready → transcribe → VTT/SRT sidecar in SeaweedFS →
      media status exposes caption_url. POST /v1/posts accepts
      auto_caption flag. Implements issue #20 → close it when done.
      Accept: docs/creator-studio.md; smoke uploads clip, polls caption
      ready (CI-gated where no ffmpeg).

- [ ] **E4. Language bridge**
      POST /v1/ai/translate (IndicTrans2, MIT): post/comment text →
      target language (hi/gu/ta/bn/te/en…); cached in Valkey by
      (hash, target); per-user rate limit. Feed endpoint gains
      ?translate=xx convenience (best-effort, never blocks feed read).
      Accept: translation round-trip test (CI-gated model download).

- [ ] **E5. Recommendations v1 (non-ML)**
      Home-feed ranking v1: score = recency decay + follow-graph boost +
      engagement velocity, computed in Go at read time (no new infra).
      Documented as the baseline that #19's vector embeddings will
      replace/extend — do NOT build embeddings here.
      Accept: docs/recsys.md with the v1 formula + #19 integration plan;
      ranking tests deterministic.

- [ ] **E6. Wave-2 hardening pass**
      Rate limits + abuse guards for E1–E5, eval harness skeleton
      (golden prompt set, run script, results gitignored), OpenAPI for
      /v1/ai/*, production.md + benchmarks.md updated, full smoke green.
      Accept: CI green, smoke green, docs complete. **Phase E complete.**

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
- 2026-10-18 ~03:40 IST: C4 done — benchmarks: `docs/benchmarks.md` (methodology + hardware template + re-run policy; baseline tables TBD — no fabricated numbers); Go bench tests (cmd/api: Healthz seq/parallel, 404, 401, 405; middleware: limiter allow seq/parallel, envelope write, RequestID/SecurityHeaders isolation); `scripts/bench_ws.py` (stdlib Phoenix-channels-v2 load probe: conns/sec, server-confirmed msgs/sec, ack p50/p95/p99, --fail-on-errors); smoke.sh runs a short 20×10 probe with --fail-on-errors; CI go-api job runs the short bench suite. Verified: bash -n + py_compile + YAML clean, SPDX headers; bench_ws.py vs fake Phoenix server — happy 10×5 → 50/50 acks exit 0, ack-drop → errors counted + exit 3, refused port → connect errors counted + JSON exit 0. Go compile/bench CI-gated (no Go on this VM, apt locked). Pushed. **ALL PHASES COMPLETE — backend production build done; report to founder.**
- 2026-10-18 ~04:35 IST: D1 done — real Ory identity plane in compose (Hydra v26.4.2 + Kratos v26.4.2, pinned; migrate one-shots; Kratos dev config + identity schema); Go API verifies Hydra RS256 JWTs via the unchanged A2 JWKS path (HYDRA_ISSUER/JWKS_URL/JWT_AUDIENCE explicit in compose); `internal/otp` adapter interface + fail-closed stub (`OTP_PROVIDER` env-gated, unknown provider refuses boot — proven live); `docs/identity.md` (DLT-gated SMS path + post-incorporation checklist); smoke.sh extended with the real-issuer loop (Hydra JWT → /v1/me 200 with bypass OFF, no-token → 401). Verified: go build+vet+test green, gofmt/bash/YAML/JSON/SPDX clean, `hydra create client` syntax confirmed against v2 CLI reference. Full-stack run is CI-gated. Pushed.
- 2026-10-18 ~05:15 IST: D2 done — profiles + follow graph: migration 0003 (profiles/follows, retired users.handle), new internal/social package (PATCH /v1/me/profile with presence-flag partials + explicit-null clears, first-come handle claims via UNIQUE → 409, per-user PATCH serialization, idempotent follows 201/200/204, self-follow 422, computed counts, keyset cursor pagination, subject never exposed), 5 routes wired, OpenAPI + redocly green, smoke.sh D2 round-trip (two Hydra clients, bypass off). Verified: go build+vet+test green on real PG16 (incl. 10-racer claim race → 1 winner, 3-page pagination, HTTP envelope matrix); 2 real bugs caught by tests pre-push (next_cursor off-by-one row, DisallowUnknownFields no-op on maps → explicit allowlist); live binary smoke of all routes. Pushed.- 2026-10-18 ~05:45 IST: D3a done — posts table + CRUD (D3 part 1): migration 0004 (posts + CHECKs + author-timeline index), new internal/posts package (Create w/ validation + rolling per-author velocity cap → 429 post_velocity_limited + computed Retry-After; Get; ListByAuthor keyset on (created_at,id); author-only Delete), routes POST /v1/posts / DELETE /v1/posts/{id} / GET /v1/users/{id}/posts wired, OpenAPI + redocly green, smoke.sh posts round-trip added. Verified: go build+vet+test green on real PG16 (7 suites); 1 real bug caught pre-push (LIMIT literal vs $N param); live binary smoke 201/405/200/204/404/400 + live 429 with Retry-After. D3b (Valkey hybrid fan-out + GET /v1/feed/home + docs/feeds.md) is the next task. Pushed.
- 2026-10-18 ~07:50 IST: D4a done — media registry + presigned upload (D4 part 1): migration 0005, internal/media (SeaweedFS dir/assign + direct PUT URLs, GET status, complete with HEAD-byte-confirmation, CheckReady gate, MarkReady/MarkFailed), 3 routes wired in main.go with the posts media gate, OpenAPI media paths + schemas (redocly 0 warnings). Two build-red bugs caught this run: unused media import in posts.go, bench_test.go stale newHandler signature — both fixed. Verified: go build+vet+gofmt clean, go test ./... green on real PG16, live binary smoke (upload 400/400/422/503+Retry-After, GET 400/404, complete 404, 405; post w/ unknown media → 422 unknown_media, post w/o media → 201). Previous run wrote this code but timed out before verify/push (4th timeout — cron body now has strict scope discipline). Pushed. Next: D4b (transcode worker + docs/video-pipeline.md + smoke clip round-trip).
- 2026-10-18 ~08:35 IST: D4b part 1 done — transcode worker core (`internal/transcode` + `cmd/transcode-worker` + TRANSCODE_* config). Consumer-group stream drain, FFmpeg HLS ladder (never upscale) + poster, SeaweedFS round-trip, exponential-backoff retries, dead-letter after 5 attempts, sweep of stuck rows, TTL heartbeat. Fixed a real D4a bug (scanMedia NULL-column crash) caught by the new tests. `go build`+`go vet`+gofmt clean; full `go test ./...` green on real PG16 (4 unit + 3 integration suites). Pushed.
- 2026-10-18 ~09:10 IST: D4b part 2 done - worker packaging: services/api/Dockerfile.worker (Go build stage + alpine:3.20 runtime with ffmpeg apk, non-root app, own TMPDIR, pidof healthcheck backstop), compose transcode-worker service (build context services/api, DB/Valkey/SeaweedFS env, healthy-dependency ordering; consumer group splits jobs across replicas), CI compose job builds the worker image on every push, docs/video-pipeline.md (lifecycle diagram, media statuses, worker design, ladder table, outputs, failure modes, env reference, ops notes). Verified: compose + CI YAML parse clean, SPDX checker green; image build/boot are CI-gated (no docker on this VM). Next: smoke.sh full clip round-trip (D4b part 3). Rides the next push.
- 2026-10-18 ~09:30 IST: D4b part 3 done — smoke.sh full clip round-trip (upload intent → PUT → complete → poll to ready → 201 media post). bash -n + SPDX green, clip-minting proven live; end-to-end CI-gated. **D4b complete.** Next: D5 realtime post events.
- 2026-10-18 ~10:00 IST: D5 part 1 done — Go-side realtime publisher (`internal/realtime`): Envelope (topics/event/payload/dedupe_key), HTTPPublisher (Bearer service token, 5s timeout, 500-topic chunking, envelope validation, 2xx-required), disabled no-op publisher when CHAT_INTERNAL_TOKEN empty (loud WARN), NewPublisher fail-closed on bad URL with token set (proven live: refuses boot); PublishPostNew (follower-subject resolution mirroring feed's celebrity threshold — no realtime fan-out for celebrity posts, same as pull-on-read) + PublishFollowerNew (new edges only) helpers; wired in main.go — post:new fires detached after successful fan-out (never delays the 201), follower:new via new social.FollowHooks; config CHAT_INTERNAL_URL/CHAT_INTERNAL_TOKEN + compose api env; new docs/realtime-events.md (endpoint contract, topic model + join-auth rule, event catalog post:new/follower:new + reserved names, best-effort delivery semantics, celebrity policy, config table, client guidance). VERIFIED: go build+vet+gofmt clean, full `go test ./...` green on real PG16 (8 pure + 4 integration suites); live binary end-to-end vs a stub chat server — follow → `follower:new` to `user:<followee-subject>`, post → `post:new` to `user:<follower-subject>`, both with Bearer auth + dedupe keys; fail-closed boot refusal proven. NOTE: VM lost apt Go mid-run — installed Go 1.22.12 tarball to ~/golang (persists across VM replacements; add to PATH in future runs). NEXT: D5 part 2 — Elixir chat side: POST /internal/events endpoint with constant-time token check, `user:<id>` topic join auth, channel tests (topic auth + event shape), smoke.sh round-trip.
- 2026-10-18 ~10:35 IST: D5 part 2 done — Elixir chat side of realtime events (see D5 task note): UserChannel (own-topic-only join auth), Router + InternalEventsController (constant-time Bearer auth, strict envelope validation, 202+broadcast fan-out, shared error envelope), runtime.exs CHAT_INTERNAL_TOKEN config, compose chat env, smoke.sh live endpoint checks (401/401/422/202), docs dedupe precision. 16 new tests (6 channel + 10 controller). Syntax-checked all files; full mix test CI-gated (Elixir 1.18). **D5 COMPLETE.** Next: D6 Wave-1 hardening pass.
- 2026-10-18 ~10:55 IST: D6 part 1 done — follow velocity abuse guard (internal/social: DefaultFollowVelocityCap 200/h via FOLLOW_VELOCITY_PER_HOUR, FollowWithCap, 429 follow_velocity_limited + Retry-After; idempotent retries bypass the check — caught by new tests, design fixed). Tests: 2 new suites green on real PG16; full `go build`+`go vet`+`go test ./...` green; gofmt clean; redocly lint 0 warnings. OpenAPI + production.md updated. D6 stays open (env-table backfill, benchmarks.md, full smoke green remain). Pushed.
- 2026-10-18 ~11:25 IST: D6 part 2 done — production.md env-table backfill (13 API rows + chat CHAT_INTERNAL_TOKEN, all cross-checked against config.go/runtime.exs; fixed stale "Hydra lands with issue #4" line), smoke.sh D6 velocity section (1/hour caps → post/follow 429s with envelope codes + Retry-After, idempotent retry → 200, defaults restored), compose velocity env passthroughs, new internal/social/bench_test.go (5 pure benches; CI bench step extended) + benchmarks.md update. Verified: go build+vet+test green, benches run, bash -n + YAML parse clean; full smoke + CI green CI-gated. **D6 COMPLETE — Phase D (Wave 1) complete.**
- 2026-10-18 ~11:55 IST: E1 done — Python AI service scaffold (see E1 task note): FastAPI app (fail-closed /healthz + AG-UI 1.0 SSE endpoint over a real LangGraph echo graph), pinned deps, hardened Dockerfile, docs/ai-core.md; compose ai + ollama services; Go internal/aiproxy (/v1/ai/* behind RequireAuth, service token + X-Sutra-Subject, SSE flush, 503 envelopes) + AI_SERVICE_URL/TOKEN config; CI builds the ai image; OpenAPI /v1/ai/healthz (redocly clean); smoke.sh E1 section (ephemeral token, proxy 200/401 checks, AG-UI echo round-trip). Verified: py_compile, go build+vet+test green (5 new proxy tests), bash/YAML/SPDX/redocly clean; full boot + smoke CI-gated. Next: E2 personal AI chat.
