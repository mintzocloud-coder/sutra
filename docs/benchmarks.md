# Sutra backend benchmarks (C4)

Reproducible load numbers for the backend: Go API HTTP overhead and
chat websocket end-to-end throughput. Honest numbers, no hype —
every figure here says exactly what was measured, on what hardware,
and what it does NOT cover.

**Status:** the harnesses exist and run in CI (`go test -bench` on every
push; a short `bench_ws.py` probe inside `scripts/smoke.sh`). Baseline
numbers on reference hardware are not captured yet — the tables below
are the schema to fill in, not results.

## Hardware & environment template

Every baseline entry must record these. Numbers without hardware are
marketing.

| Field | Example |
|---|---|
| CPU | 8 vCPU (model) |
| RAM | 32 GB |
| Disk | NVMe, PG data dir on local SSD |
| Network | loopback (same host) vs LAN |
| OS / kernel | Ubuntu 24.04 |
| Runtime | Go 1.22.x / Elixir 1.18 + OTP 27 |
| Commit | full SHA of the measured build |
| Command | exact command run |
| Date | YYYY-MM-DD |

CI runner numbers (`ubuntu-latest`, shared, variable) are **regression
indicators only** — they prove the harness works and catch big
regressions; they are not publishable baselines. Publishable baselines
come from a dedicated machine with the template filled.

## 1. Go API — HTTP overhead benchmarks

Location: `services/api/cmd/api/bench_test.go`,
`services/api/internal/middleware/bench_test.go`.

Run:

```bash
cd services/api
go test -run='^$' -bench=. -benchtime=10s -count=3 ./cmd/api/... ./internal/middleware/... ./internal/social/
```

Scope: **pure HTTP-layer overhead** — mux routing, the full middleware
chain (RequestID → Recover → SecurityHeaders → CORS → RateLimit →
Logger), handler JSON encoding. `httptest`, DB-free, network-free, rate
limiter effectively unlimited.

| Benchmark | Measures | ns/op | req/s | allocs/op |
|---|---|---|---|---|
| `BenchmarkHealthz` | GET /healthz through full chain — the latency floor of any API request | TBD | TBD | TBD |
| `BenchmarkHealthzParallel` | same, across all cores — healthy = scales ~linearly with GOMAXPROCS | TBD | TBD | TBD |
| `BenchmarkNotFound404` | mux miss + 404 JSON envelope (the path every client-visible error shares) | TBD | TBD | TBD |
| `BenchmarkUnauthorized401` | /v1/me with no token → 401 envelope (RequireAuth fast-reject; no JWKS fetch, no DB) | TBD | TBD | TBD |
| `BenchmarkMethodNotAllowed405` | POST /v1/me → 405 envelope + Allow header | TBD | TBD | TBD |
| `BenchmarkRateLimiterAllow` | token-bucket allow hot path, single key | TBD | TBD | TBD |
| `BenchmarkRateLimiterAllowParallel` | same, 64 client keys — mutex contention under many IPs | TBD | TBD | TBD |
| `BenchmarkErrorEnvelopeWrite` | JSON marshal of the canonical error envelope | TBD | TBD | TBD |
| `BenchmarkRequestIDOnly` | request-ID generation + header echo in isolation | TBD | TBD | TBD |
| `BenchmarkSecurityHeadersOnly` | baseline header writes in isolation | TBD | TBD | TBD |

### Social validation benchmarks (D6)

Location: `services/api/internal/social/bench_test.go`. Pure,
DB-free hot paths that sit on every profile/follow read and write —
run with the same command above.

| Benchmark | Measures | ns/op | req/s | allocs/op |
|---|---|---|---|---|
| `BenchmarkValidateHandle` | compiled-regex match on every handle claim and profile PATCH | TBD | TBD | TBD |
| `BenchmarkValidateHandleInvalid` | fast-reject path for an illegal handle | TBD | TBD | TBD |
| `BenchmarkEncodeCursor` | `next_cursor` generation per paginated page (JSON marshal + base64) | TBD | TBD | TBD |
| `BenchmarkDecodeCursor` | client-cursor parsing on every `?cursor=` request (base64 + JSON + UUID validation) | TBD | TBD | TBD |
| `BenchmarkParseUserID` | `{id}` path-parameter parse on every `/v1/users/{id}` route | TBD | TBD | TBD |

The D3/D6 **velocity caps are deliberately not benchmarked**: they are
DB-backed (a `COUNT(*)` over the trailing-hour window), and these
harnesses are DB-free by design. Their contract — 429 +
`post_velocity_limited` / `follow_velocity_limited` envelope codes +
computed `Retry-After` — is proven by `scripts/smoke.sh` (the D6
section asserts all three live against the real stack).

The E6 **AI guards are deliberately not benchmarked** either: they are
PG-backed fixed-window counters (`ai_rate_windows`, `ai_usage_daily`) —
DB round-trip cost, not hot-path cost — and a benchmark that couldn't
touch the database would measure nothing real. Their contract
(429 + `ai_rate_limited` / `ai_budget_exhausted` + `Retry-After`) is
proven per-endpoint: `/v1/ai/chat` and `/v1/ai/translate` by
`scripts/smoke.sh` (the E2/E4 sections assert both 429 paths live),
`/v1/ai/ag-ui/run` by `services/ai/tests/test_agui_guards.py` (unit)
plus the same live path as chat. Model inference itself is
hardware-bound (sarvam-30B via Ollama dev / vLLM prod); token
throughput benchmarks are Wave-2 work and belong in docs/ai-core.md,
not here.

**Reading them:** `BenchmarkHealthz` ns/op is the per-request framework
tax every route pays; compare `BenchmarkUnauthorized401` minus
`BenchmarkHealthz` to price the auth-reject path; compare
`BenchmarkNotFound404` minus `BenchmarkHealthz` to price envelope
encoding. These numbers do NOT include PostgreSQL, JWKS fetch, TLS, or
network time — real `/v1/me` latency = framework tax + PG round-trip
(+ JWKS fetch on cache miss).

## 2. Chat — websocket end-to-end throughput

Harness: `scripts/bench_ws.py` (pure stdlib Phoenix channels v2 client).

Run:

```bash
# 1. boot the stack and keep it up (AUTH_BYPASS=1: dev-only identity hatch)
SMOKE_KEEP_UP=1 AUTH_BYPASS=1 bash scripts/smoke.sh
# 2. drive load at the chat endpoint
python3 scripts/bench_ws.py localhost 4000 --conns 200 --msgs 50 --body-bytes 256
```

Metrics (all in the JSON summary it prints):

| Metric | Definition |
|---|---|
| `conns_per_s` | successful socket joins per second |
| `msgs_per_s` | **server-confirmed** messages per second (counts `msg:ack`, not sends) |
| `ack_latency_ms` p50/p95/p99 | push → `msg:ack` round-trip per message |
| `join_latency_ms` p50/p95/p99 | socket connect + channel join |
| `errors` | failed connects / failed sends+acks (any > 0 = investigate, don't average away) |

**What the number includes:** every message is persisted to PostgreSQL
before broadcast (persist-before-broadcast), so `msgs_per_s` includes
the PG write; presence join/leave diffs; the Python client runs one
thread per connection (client-side overhead is real — this measures the
service, not a theoretical protocol ceiling).

**What it does NOT include:** multi-node clustering (single node only),
TLS termination, real WAN latency, or reads (history pagination is a
separate path — measure with `history` joins if you need it).

| Run | Hardware | conns | msgs/conn | body | msgs/s | ack p50 | ack p95 | ack p99 | errors |
|---|---|---|---|---|---|---|---|---|---|
| baseline | TBD | 200 | 50 | 256 B | TBD | TBD | TBD | TBD | TBD |

## 3. Re-run policy

- Re-run the Go benchmarks on any change to the middleware chain,
  mux wiring, or the envelope (CI runs the short version on every
  push; record the full `-benchtime=10s -count=3` numbers here).
- Re-run the ws bench on any change to the send path (persistence,
  broadcast, ack, presence) or deps (Phoenix, Ecto, postgrex).
- Always record the commit SHA. A number that moves without a code
  change is noise — say so.

## 4. Deliberately not benchmarked (yet)

- Multi-node chat throughput (needs CLUSTER_ENABLED + 2+ nodes; see
  docs/presence.md) — single-node first, then publish the scaling curve.
- TLS + real-network latency — terminate TLS in front and measure at
  the edge when the deployment target is known.
- History-pagination read throughput under load.
- Valkey/SeaweedFS paths (not on any request hot path today).
