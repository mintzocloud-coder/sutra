#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# End-to-end smoke test for the full Sutra compose stack (C1).
#
# Boots postgres + valkey + seaweedfs + api + chat, runs the chat
# migrations, then verifies:
#   - API:  /healthz 200 (+ X-Request-ID), /readyz 200, /v1/me 200
#   - Chat: websocket connect → join → send → broadcast + msg:ack →
#           reconnect → history contains the message (persistence)
#   - Chat load probe: short scripts/bench_ws.py run (C4) for
#           joins/sec + server-confirmed msgs/sec + ack-latency percentiles
#   - SeaweedFS: /cluster/status 200; Valkey: PING → PONG
#
# Identity is exercised two ways:
#   - AUTH_BYPASS=1 for the basic API checks and the chat websocket
#     round-trip — the services' documented dev-only escape hatch (both
#     log a loud WARN when it is set; it is never enabled in production).
#   - The D1 real-issuer loop: a Hydra-issued RS256 JWT (client_credentials
#     + jwt-bearer grant) is presented to GET /v1/me with the api recreated
#     WITHOUT the bypass, proving the A2 JWKS verification path against a
#     real OIDC issuer. The bypass skips verification entirely, so the
#     bypass-on checks alone would prove nothing about Hydra.
#   - The D2 social round-trip: two Hydra-issued identities (sutra-smoke,
#     sutra-smoke-b) claim handles via PATCH /v1/me/profile, A follows B
#     (201 then idempotent 200), self-follow 422s, followers/following
#     listings are verified, A unfollows (204) — all with the bypass off.
#   - The D3 part-1 posts round-trip: A creates a post (201), an
#     oversized body 422s, A's author timeline lists it, B's delete
#     attempt 403s, A deletes it (204), the timeline is empty again.
#   - The D3 part-2 home feed round-trip: A follows B, B's 3 posts fan
#     out into A's home feed (newest first), cursor pagination walks
#     cleanly (no dupes, terminates), deleting a post removes it from
#     the follower's feed.
#   - The D4 video round-trip: upload intent → PUT a sample clip to
#     SeaweedFS → complete → the transcode worker turns it into HLS →
#     poll to ready (poster + renditions + playback URL), then a post
#     referencing the ready media (the posts media gate).
#   - The E3 auto-caption round-trip (same clip): the post opts in with
#     auto_caption=true → the caption worker transcribes → poll the
#     media row to caption_status ready (VTT caption_url sidecar).
#   - The D5 realtime endpoint: no token → 401, wrong token → 401,
#     unknown event → 422, valid envelope → 202 accepted.
#   - The D6 velocity guards: the api is recreated with 1/hour caps and
#     a throwaway Hydra identity proves post 429 (post_velocity_limited
#     + Retry-After) and follow 429 (follow_velocity_limited +
#     Retry-After), while an idempotent follow retry still 200s.
#   - The E6 AG-UI abuse guard: the ai service is recreated with
#     AI_AGUI_RPM=1 and a throwaway Hydra identity proves the echo
#     endpoint streams first, then 429s (ai_rate_limited + Retry-After).
#   - The F1 commerce round-trip: a throwaway Hydra identity registers a
#     seller (201 + 0bps commission; second registration 409), public
#     storefront reads (GET seller, category tree), product CRUD (create
#     draft → list → activate), variant add (duplicate SKU 409), inventory
#     (0 → +100; −150 refuses 409 insufficient_stock), then archive
#     variant + product.
#   - The F3 cart + order round-trip: a fresh sellable fixture (product →
#     active, variant, stock 10); cart add (line_total math) → over-stock
#     add 409 insufficient_stock → set qty 3 → cart view subtotal;
#     COD-first checkout (201, created/cod, cart cleared) → empty-cart
#     checkout 422 cart_empty → buyer + seller list views → bad role 400 →
#     detail (price snapshot, 400 on bad id, 404 on ghost) → seller reprice
#     leaves the snapshot untouched → created→confirmed→(delivered 422
#     invalid_transition)→shipped→delivered → a second order
#     buyer-cancelled proves reservation release (quantity 7, reserved 0)
#     → status-patch error matrix (422 invalid_status, 400 unknown
#     field); fixture archived at the end.
#   - The F2 product-search round-trip: Meilisearch boots with an ephemeral
#     per-run master key, the api is recreated with SEARCH_MASTER_KEY set
#     (bypass explicitly off — the production auth path); a throwaway
#     Hydra identity lists 3 products (P1 with two variants, to prove
#     min_price_paise = cheapest ACTIVE variant) → activate → poll
#     /v1/search until the async index catches up → hit fields asserted
#     (status/seller/category/min_price/sort keys) → typo tolerance
#     (kurtha → kurta) → seller/category/price filters → keyset
#     pagination walk (limit=2, next_cursor terminates, no dupes) →
#     400 param matrix → archive all → hits vanish from the index. The
#     api is then restored searchless and /v1/search → 503
#     search_unavailable + Retry-After is proven.
# The chat release fails closed at boot without a real SECRET_KEY_BASE, so
# this script generates an ephemeral one (kept in the environment only,
# never written to disk or committed).
#
# Usage:
#   bash scripts/smoke.sh                # tears the stack down afterwards
#   SMOKE_KEEP_UP=1 bash scripts/smoke.sh # leaves the stack running
set -euo pipefail

cd "$(dirname "$0")/.."

API_URL="${SMOKE_API_URL:-http://localhost:8081}"
CHAT_HOST="${SMOKE_CHAT_HOST:-localhost}"
CHAT_PORT="${SMOKE_CHAT_PORT:-4000}"
EXPECTED_SUBJECT="${AUTH_BYPASS_SUBJECT:-dev-user}"

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "smoke: missing required tool: $1" >&2
    exit 2
  }
}
for tool in docker curl python3 openssl; do need "$tool"; done
docker compose version >/dev/null 2>&1 || {
  echo "smoke: the 'docker compose' plugin is required" >&2
  exit 2
}

export SECRET_KEY_BASE="${SECRET_KEY_BASE:-$(openssl rand -base64 48)}"
# D5: shared service-to-service secret for the API→chat realtime event
# endpoint (POST /internal/events). Ephemeral per smoke run, kept in the
# environment only, never written to disk or committed. Both services
# read it from the environment (compose ${CHAT_INTERNAL_TOKEN:-}).
export CHAT_INTERNAL_TOKEN="${CHAT_INTERNAL_TOKEN:-$(openssl rand -hex 32)}"
# E1: shared service-to-service secret for the API->AI proxy (/v1/ai/*).
# Ephemeral per smoke run, environment only, never written or committed.
# Both services read it from the environment (compose ${AI_SERVICE_TOKEN:-});
# the ai service refuses to boot without it (fail-closed).
export AI_SERVICE_TOKEN=$(openssl rand -hex 16)
export AUTH_BYPASS=1
# F2: Meilisearch product search. The meilisearch container and the API's
# search client share one ephemeral master key per smoke run (environment
# only, never written or committed); the api enables indexing only when
# SEARCH_MASTER_KEY is set (empty = searchless, loud WARN at boot).
export SMOKE_MEILI_KEY="$(openssl rand -hex 16)"

cleanup() {
  if [ "${SMOKE_KEEP_UP:-0}" != "1" ]; then
    echo "smoke: tearing the stack down"
    docker compose down -v >/dev/null 2>&1 || true
  else
    echo "smoke: SMOKE_KEEP_UP=1 — leaving the stack running"
  fi
}
trap cleanup EXIT

pass() { echo "smoke PASS: $1"; }
fail() {
  echo "smoke FAIL: $1" >&2
  exit 1
}

wait_for() { # wait_for <secs> <description> <command...>
  local secs="$1" desc="$2"
  shift 2
  local deadline=$((SECONDS + secs))
  while [ $SECONDS -lt "$deadline" ]; do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  fail "timed out waiting for $desc"
}

echo "smoke: building service images"
docker compose build --quiet api chat transcode-worker caption-worker ai 2>/dev/null \
  || docker compose build api chat transcode-worker caption-worker ai

echo "smoke: starting postgres, valkey, seaweedfs"
docker compose up -d postgres valkey seaweedfs
# Postgres readiness is the flakiest gate on CI runners (cold image pulls,
# slow initdb). Wait longer here, and dump the container logs on timeout so
# the next failure carries its own diagnosis instead of just a timeout line.
pg_deadline=$((SECONDS + 180))
while [ $SECONDS -lt "$pg_deadline" ]; do
  if docker compose exec -T postgres pg_isready -U sutra >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
if ! docker compose exec -T postgres pg_isready -U sutra >/dev/null 2>&1; then
  echo "smoke: postgres never became ready — dumping container logs" >&2
  docker compose logs --tail=80 postgres >&2 || true
  docker compose ps >&2 || true
  fail "timed out waiting for postgres to accept connections (logs above)"
fi
pass "postgres ready"

echo "smoke: starting hydra + kratos (migrate one-shots run first via depends_on)"
docker compose up -d hydra kratos
wait_for 120 "hydra openid-configuration" \
  curl -sf http://localhost:4444/.well-known/openid-configuration
wait_for 120 "kratos /health/alive" \
  curl -sf http://localhost:4433/health/alive
pass "hydra + kratos up (identity plane live)"

echo "smoke: running chat database migrations"
docker compose run --rm chat /app/bin/sutra_chat eval "SutraChat.Release.migrate()"

echo "smoke: starting api + chat"
docker compose up -d api chat
wait_for 90 "api /healthz" curl -sf "$API_URL/healthz"

# --- API checks ----------------------------------------------------------
rid="$(curl -sS -D - -o /dev/null "$API_URL/healthz" | grep -i '^x-request-id:' | tr -d '\r' | awk '{print $2}')"
[ -n "$rid" ] || fail "/healthz response has no X-Request-ID"
pass "api /healthz 200 + X-Request-ID ($rid)"

[ "$(curl -s -o /dev/null -w '%{http_code}' "$API_URL/readyz")" = "200" ] \
  || fail "/readyz did not return 200"
pass "api /readyz 200 (database reachable)"

me="$(curl -sf "$API_URL/v1/me")" || fail "/v1/me did not return 200"
subject="$(printf '%s' "$me" | python3 -c 'import json,sys; print(json.load(sys.stdin)["subject"])')"
[ "$subject" = "$EXPECTED_SUBJECT" ] \
  || fail "/v1/me subject '$subject' != expected '$EXPECTED_SUBJECT'"
pass "api /v1/me 200 (bypass identity '$subject', auto-provisioned)"

# --- Fail-closed payments seam (F4) -------------------------------------------
# Compose pins PAYMENTS_PROVIDER=stub with empty CASHFREE_*, so the api must
# boot with the loud "fail-closed stub" WARN — and must NOT log the
# set-but-ignored WARN (the creds are unset here). Assert on the boot log.
docker compose logs api 2>/dev/null | grep -q "payments: running with the fail-closed stub" \
  || fail "api boot log missing the fail-closed payments WARN"
pass "api boot log carries the fail-closed payments WARN"
docker compose logs api 2>/dev/null | grep -q "payments: CASHFREE_" \
  && fail "api boot log claims CASHFREE_* set-but-ignored (creds are unset in smoke)"
pass "api boot log has no set-but-ignored CASHFREE_* WARN"

# --- Real-issuer identity loop (D1) -----------------------------------------
# The checks above ran with AUTH_BYPASS=1, which skips JWT verification
# entirely. Now prove the real path: a Hydra-issued RS256 JWT verified
# through the A2 JWKS flow reaches GET /v1/me.
echo "smoke: creating hydra oauth2 client 'sutra-smoke'"
SMOKE_HYDRA_CLIENT_SECRET="${SMOKE_HYDRA_CLIENT_SECRET:-$(openssl rand -hex 24)}"
# The jwt-bearer grant type is what makes Hydra mint JWT (not opaque)
# access tokens. Tolerate "already exists" for SMOKE_KEEP_UP=1 re-runs.
if ! docker compose run --rm hydra hydra create client \
    --endpoint http://hydra:4445 \
    --id sutra-smoke \
    --secret "$SMOKE_HYDRA_CLIENT_SECRET" \
    --grant-type client_credentials \
    --grant-type urn:ietf:params:oauth:grant-type:jwt-bearer \
    --response-type token \
    --scope openid \
    --audience sutra-api \
    --token-endpoint-auth-method client_secret_basic \
    >/tmp/sutra-hydra-client.out 2>&1; then
  if grep -qiE "already exists|conflict" /tmp/sutra-hydra-client.out; then
    echo "smoke: hydra client 'sutra-smoke' already exists — reusing"
  else
    cat /tmp/sutra-hydra-client.out >&2
    fail "hydra oauth2 client creation failed"
  fi
else
  pass "hydra oauth2 client 'sutra-smoke' created"
fi

token_json="$(curl -sf -u "sutra-smoke:$SMOKE_HYDRA_CLIENT_SECRET" \
  --data-urlencode 'grant_type=client_credentials' \
  --data-urlencode 'audience=sutra-api' \
  --data-urlencode 'scope=openid' \
  http://localhost:4444/oauth2/token)" || fail "hydra token request failed"
access_token="$(printf '%s' "$token_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')"
case "$access_token" in
  *.*.*) pass "hydra issued a JWT access token (jwt-bearer grant)" ;;
  *) fail "hydra returned an opaque token, not a JWT — the jwt-bearer grant type is required on the client" ;;
esac

echo "smoke: recreating api WITHOUT auth bypass to exercise real JWT verification"
AUTH_BYPASS= docker compose up -d --no-deps api
wait_for 90 "api /healthz (bypass off)" curl -sf "$API_URL/healthz"

me_jwt="$(curl -sf -H "Authorization: Bearer $access_token" "$API_URL/v1/me")" \
  || fail "/v1/me with hydra JWT did not return 200"
jwt_subject="$(printf '%s' "$me_jwt" | python3 -c 'import json,sys; print(json.load(sys.stdin)["subject"])')"
[ "$jwt_subject" = "sutra-smoke" ] \
  || fail "/v1/me subject '$jwt_subject' != hydra client 'sutra-smoke' — token was not really verified"
pass "api /v1/me 200 with hydra-issued JWT (subject 'sutra-smoke', RS256 verified via JWKS)"

[ "$(curl -s -o /dev/null -w '%{http_code}' "$API_URL/v1/me")" = "401" ] \
  || fail "/v1/me without token did not 401 — bypass still on?"
pass "api /v1/me 401 without token (bypass confirmed off)"

# --- AI core round-trip (E1) -------------------------------------------------
# The Python AI service boots behind its fail-closed service token; the Go
# API proxies /v1/ai/* to it with the same token plus the verified JWT
# subject (X-Sutra-Subject). Ollama is NOT booted here — E1's echo graph
# never calls a model (the pull command is documented in docs/ai-core.md).
echo "smoke: starting ai service (E1)"
docker compose up -d ai
wait_for 120 "ai /healthz" curl -sf http://localhost:8000/healthz
pass "ai service up (fail-closed boot with ephemeral AI_SERVICE_TOKEN)"

ai_health="$(curl -sf -H "Authorization: Bearer $access_token" "$API_URL/v1/ai/healthz")" \
  || fail "/v1/ai/healthz through the Go proxy did not return 200"
printf '%s' "$ai_health" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["service"]=="sutra-ai", d' \
  || fail "/v1/ai/healthz did not return the ai service payload — proxy miswired?"
pass "api /v1/ai/healthz 200 through the proxy (hydra JWT, subject forwarded)"

[ "$(curl -s -o /dev/null -w '%{http_code}' "$API_URL/v1/ai/healthz")" = "401" ] \
  || fail "/v1/ai/healthz without token did not 401 — auth bypass leaked?"
pass "api /v1/ai/* 401 without token (Go auth still gates the proxy)"

ag_ui_out="$(curl -sf -N -H "Authorization: Bearer $access_token" -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"smoke ping"}]}' "$API_URL/v1/ai/ag-ui/run")" \
  || fail "POST /v1/ai/ag-ui/run through the proxy failed"
printf '%s' "$ag_ui_out" | grep -q '"type":"RUN_STARTED"' \
  || fail "AG-UI stream missing RUN_STARTED"
printf '%s' "$ag_ui_out" | grep -q '"type":"RUN_FINISHED"' \
  || fail "AG-UI stream missing RUN_FINISHED"
printf '%s' "$ag_ui_out" | grep -q 'smoke ping' \
  || fail "AG-UI echo graph did not reflect the message"
pass "AG-UI skeleton streams RUN_STARTED->echo->RUN_FINISHED through the proxy"

# --- Personal AI chat round-trip (E2) ---------------------------------------
# The stub model stands in for sarvam-30B (CI has no GPU/model to pull);
# DATABASE_URL comes from the compose file itself. Exercises: the full
# AG-UI turn (recall->generate->persist), thread memory across two turns,
# PG persistence, and both guard 429 paths (budget + rate limit).
echo "smoke: personal AI chat round-trip (E2)"
AI_MODEL_STUB=1 docker compose up -d --no-deps ai
wait_for 120 "ai /healthz (stub model)" curl -sf http://localhost:8000/healthz

chat_out="$(curl -sf -N -H "Authorization: Bearer $access_token" -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"smoke chat ping"}]}' "$API_URL/v1/ai/chat")" \
  || fail "POST /v1/ai/chat through the proxy failed"
printf '%s' "$chat_out" | grep -q '"type":"RUN_STARTED"' \
  || fail "chat stream missing RUN_STARTED"
printf '%s' "$chat_out" | grep -q '"type":"TEXT_MESSAGE_START"' \
  || fail "chat stream missing TEXT_MESSAGE_START"
printf '%s' "$chat_out" | grep -q 'AI_MODEL_STUB=1' \
  || fail "chat stream missing the stub-model marker"
printf '%s' "$chat_out" | grep -q '"type":"RUN_FINISHED"' \
  || fail "chat stream missing RUN_FINISHED"
thread_id="$(printf '%s' "$chat_out" | grep -o '"threadId":"[^"]*"' | head -1 | cut -d'"' -f4)"
[ -n "$thread_id" ] || fail "chat RUN_STARTED carried no threadId"
# Second turn on the same thread: proves per-user conversation memory.
curl -sf -N -H "Authorization: Bearer $access_token" -H 'Content-Type: application/json' \
  -d "{\"thread_id\":\"$thread_id\",\"messages\":[{\"role\":\"user\",\"content\":\"smoke chat pong\"}]}" \
  "$API_URL/v1/ai/chat" | grep -q '"type":"RUN_FINISHED"' \
  || fail "second chat turn on thread $thread_id failed"
mem="$(docker compose exec -T postgres psql -U sutra -d sutra -tAc \
  "SELECT count(*) FROM ai_messages m JOIN ai_threads t ON t.id = m.thread_id WHERE t.subject = 'sutra-smoke' AND t.client_thread_id = '$thread_id'")"
[ "$mem" = "4" ] || fail "expected 4 persisted chat messages, found '$mem'"
pass "personal AI chat streams a full AG-UI turn; thread memory persists (4 rows)"

# Daily token-budget 429 path (budget=1 token: any real turn exceeds it).
AI_MODEL_STUB=1 AI_DAILY_TOKEN_BUDGET=1 docker compose up -d --no-deps ai
wait_for 120 "ai /healthz (budget=1)" curl -sf http://localhost:8000/healthz
budget_code="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $access_token" \
  -H 'Content-Type: application/json' -d '{"messages":[{"role":"user","content":"x"}]}' \
  "$API_URL/v1/ai/chat")"
[ "$budget_code" = "429" ] || fail "budget-exhausted chat did not 429 (got $budget_code)"
pass "daily token budget 429 path (ai_budget_exhausted)"

# Per-user rate-limit 429 path (rpm=1; loop until a 429 to survive a
# minute-boundary rollover between the two requests).
AI_MODEL_STUB=1 AI_CHAT_RPM=1 docker compose up -d --no-deps ai
wait_for 120 "ai /healthz (rpm=1)" curl -sf http://localhost:8000/healthz
rate_code="200"
for _ in 1 2 3 4; do
  rate_code="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $access_token" \
    -H 'Content-Type: application/json' -d '{"messages":[{"role":"user","content":"ping"}]}' \
    "$API_URL/v1/ai/chat")"
  [ "$rate_code" = "429" ] && break
done
[ "$rate_code" = "429" ] || fail "rate-limited chat never 429d (last got $rate_code)"
pass "per-user rate limit 429 path (ai_rate_limited)"

# Restore the default ai configuration for the rest of the run.
docker compose up -d --no-deps ai
wait_for 120 "ai /healthz (defaults restored)" curl -sf http://localhost:8000/healthz
# NOTE: the api is intentionally left running WITHOUT the bypass from here
# on — the more production-faithful state. Nothing later in this script
# needs the bypass (chat keeps its own for the websocket round-trip).

# --- AG-UI rate-limit guard (E6) -------------------------------------------
# The /ag-ui/run echo endpoint costs no model tokens, but before E6 it was
# the one AI endpoint with no per-user limit — an unbounded SSE stream is
# an abuse vector. E6 closed it with the same PG-backed fixed-window
# limiter as /chat (AI_AGUI_RPM, default 60/min; the token budget does not
# apply because an echo turn spends zero tokens).
echo "smoke: AG-UI rate-limit guard (E6)"

echo "smoke: recreating ai with AI_AGUI_RPM=1"
AI_AGUI_RPM=1 docker compose up -d --no-deps ai
wait_for 120 "ai /healthz (agui rpm=1)" curl -sf http://localhost:8000/healthz

agui_client="sutra-smoke-g-$(date +%s)"
echo "smoke: creating hydra oauth2 client '$agui_client'"
SMOKE_HYDRA_CLIENT_G_SECRET="${SMOKE_HYDRA_CLIENT_G_SECRET:-$(openssl rand -hex 16)}"
if ! docker compose run --rm hydra hydra create client \
    --endpoint http://hydra:4445 \
    --id "$agui_client" \
    --secret "$SMOKE_HYDRA_CLIENT_G_SECRET" \
    --grant-type client_credentials \
    --grant-type urn:ietf:params:oauth:grant-type:jwt-bearer \
    --response-type token \
    --scope openid \
    --audience sutra-api \
    --token-endpoint-auth-method client_secret_basic \
    >/tmp/sutra-hydra-client-g.out 2>&1; then
  cat /tmp/sutra-hydra-client-g.out >&2
  fail "hydra oauth2 client '$agui_client' creation failed"
else
  pass "hydra oauth2 client '$agui_client' created"
fi

token_g_json="$(curl -sf -u "$agui_client:$SMOKE_HYDRA_CLIENT_G_SECRET" \
  --data-urlencode 'grant_type=client_credentials' \
  --data-urlencode 'audience=sutra-api' \
  --data-urlencode 'scope=openid' \
  http://localhost:4444/oauth2/token)" || fail "hydra token request for '$agui_client' failed"
token_g="$(printf '%s' "$token_g_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')"

# A fresh subject gets a fresh fixed window: with rpm=1 the first run
# streams normally and the second must 429. Loop (like the E2 chat guard)
# to survive a minute-boundary rollover between attempts; each attempt
# captures status + body + headers so the final 429's envelope and
# Retry-After header can be asserted (the D6 velocity pattern).
agui_body="$(mktemp)"
agui_hdr="$(mktemp)"
agui_code="200"
for _ in 1 2 3 4 5; do
  agui_code="$(curl -s -o "$agui_body" -D "$agui_hdr" -w '%{http_code}' \
    -H "Authorization: Bearer $token_g" \
    -H 'Content-Type: application/json' \
    -d '{"messages":[{"role":"user","content":"agui guard ping"}]}' \
    "$API_URL/v1/ai/ag-ui/run" || true)"
  [ "$agui_code" = "429" ] && break
done
[ "$agui_code" = "429" ] || { rm -f "$agui_body" "$agui_hdr"; fail "ag-ui rate limit never triggered a 429 (last got $agui_code)"; }
grep -qi '^Retry-After: [1-9]' "$agui_hdr" \
  || { rm -f "$agui_body" "$agui_hdr"; fail "ag-ui 429 missing Retry-After header"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$agui_body"'"))["error"]["code"])')" = "ai_rate_limited" ] \
  || { rm -f "$agui_body" "$agui_hdr"; fail "ag-ui 429 envelope code is not ai_rate_limited"; }
rm -f "$agui_body" "$agui_hdr"
pass "AG-UI per-user rate limit: first call streams, over-cap 429 ai_rate_limited + Retry-After"

# Restore the default ai configuration for the rest of the run.
docker compose up -d --no-deps ai
wait_for 120 "ai /healthz (defaults restored, post-E6)" curl -sf http://localhost:8000/healthz

# --- Social graph round-trip (D2) --------------------------------------------
# Two real Hydra-issued identities exercise profiles + follows through the
# production auth path (the bypass is OFF from here on): A claims a handle,
# B claims a handle, A follows B, both listings are verified, A unfollows.
# Handles are fixed ("smoke_a"/"smoke_b") because the subjects are fixed
# (hydra client ids) — re-claiming your own handle is idempotent, so
# SMOKE_KEEP_UP=1 re-runs pass.
echo "smoke: social follow round-trip (D2)"

echo "smoke: creating hydra oauth2 client 'sutra-smoke-b'"
SMOKE_HYDRA_CLIENT_B_SECRET="${SMOKE_HYDRA_CLIENT_B_SECRET:-$(openssl rand -hex 24)}"
if ! docker compose run --rm hydra hydra create client \
    --endpoint http://hydra:4445 \
    --id sutra-smoke-b \
    --secret "$SMOKE_HYDRA_CLIENT_B_SECRET" \
    --grant-type client_credentials \
    --grant-type urn:ietf:params:oauth:grant-type:jwt-bearer \
    --response-type token \
    --scope openid \
    --audience sutra-api \
    --token-endpoint-auth-method client_secret_basic \
    >/tmp/sutra-hydra-client-b.out 2>&1; then
  if grep -qiE "already exists|conflict" /tmp/sutra-hydra-client-b.out; then
    echo "smoke: hydra client 'sutra-smoke-b' already exists — reusing"
  else
    cat /tmp/sutra-hydra-client-b.out >&2
    fail "hydra oauth2 client 'sutra-smoke-b' creation failed"
  fi
else
  pass "hydra oauth2 client 'sutra-smoke-b' created"
fi

token_b_json="$(curl -sf -u "sutra-smoke-b:$SMOKE_HYDRA_CLIENT_B_SECRET" \
  --data-urlencode 'grant_type=client_credentials' \
  --data-urlencode 'audience=sutra-api' \
  --data-urlencode 'scope=openid' \
  http://localhost:4444/oauth2/token)" || fail "hydra token request for 'sutra-smoke-b' failed"
token_b="$(printf '%s' "$token_b_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')"

jget() { # jget <json> <key> — extract a top-level JSON field
  printf '%s' "$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)[sys.argv[1]])' "$2"
}

# A = sutra-smoke ($access_token), B = sutra-smoke-b ($token_b).
profile_a="$(curl -sf -X PATCH \
  -H "Authorization: Bearer $access_token" -H 'Content-Type: application/json' \
  -d '{"handle":"smoke_a","display_name":"Smoke A"}' \
  "$API_URL/v1/me/profile")" || fail "PATCH /v1/me/profile for A failed"
[ "$(jget "$profile_a" handle)" = "smoke_a" ] || fail "A handle not claimed"
id_a="$(jget "$profile_a" user_id)"

profile_b="$(curl -sf -X PATCH \
  -H "Authorization: Bearer $token_b" -H 'Content-Type: application/json' \
  -d '{"handle":"smoke_b","display_name":"Smoke B"}' \
  "$API_URL/v1/me/profile")" || fail "PATCH /v1/me/profile for B failed"
[ "$(jget "$profile_b" handle)" = "smoke_b" ] || fail "B handle not claimed"
id_b="$(jget "$profile_b" user_id)"
pass "profiles claimed: smoke_a ($id_a), smoke_b ($id_b)"

# A follows B: 201 first, 200 on idempotent retry.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $access_token" "$API_URL/v1/users/$id_b/follow")" = "201" ] \
  || fail "POST follow A→B did not return 201"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $access_token" "$API_URL/v1/users/$id_b/follow")" = "200" ] \
  || fail "POST follow retry did not return 200 (idempotent)"
pass "follow A→B: 201 then idempotent 200"

# Self-follow is rejected.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $access_token" "$API_URL/v1/users/$id_a/follow")" = "422" ] \
  || fail "self-follow did not return 422"
pass "self-follow rejected with 422"

# B's followers contain A; A's following contains B.
followers="$(curl -sf -H "Authorization: Bearer $access_token" \
  "$API_URL/v1/users/$id_b/followers?limit=10")" || fail "GET followers failed"
[ "$(printf '%s' "$followers" | python3 -c 'import json,sys; print(json.load(sys.stdin)["items"][0]["id"])')" = "$id_a" ] \
  || fail "A missing from B's followers"
following="$(curl -sf -H "Authorization: Bearer $access_token" \
  "$API_URL/v1/users/$id_a/following?limit=10")" || fail "GET following failed"
[ "$(printf '%s' "$following" | python3 -c 'import json,sys; print(json.load(sys.stdin)["items"][0]["id"])')" = "$id_b" ] \
  || fail "B missing from A's following"
pass "followers/following listings verified (cursor-paged)"

# A unfollows B: 204, then B's followers are empty.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $access_token" "$API_URL/v1/users/$id_b/follow")" = "204" ] \
  || fail "DELETE follow did not return 204"
followers_after="$(curl -sf -H "Authorization: Bearer $access_token" \
  "$API_URL/v1/users/$id_b/followers")" || fail "GET followers after unfollow failed"
[ "$(printf '%s' "$followers_after" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["items"]))')" = "0" ] \
  || fail "B's followers not empty after unfollow"
pass "unfollow 204; followers empty"

# --- Posts round-trip (D3 part 1) -------------------------------------------
# A creates a post (201), an oversized body 422s, A's author timeline
# lists it, B's delete attempt 403s (author-only), A deletes it (204),
# and the timeline is empty again — all with the bypass off.
echo "smoke: posts round-trip (D3 part 1)"

post_json="$(curl -sf -X POST \
  -H "Authorization: Bearer $access_token" -H 'Content-Type: application/json' \
  -d '{"text":"smoke post one"}' \
  "$API_URL/v1/posts")" || fail "POST /v1/posts failed"
post_id="$(jget "$post_json" id)"
[ -n "$post_id" ] || fail "post id missing from create response"
pass "post created ($post_id)"

# Oversized body (501 runes) is rejected with 422.
oversize_body="$(python3 -c 'import json; print(json.dumps({"text": "x"*501}))')"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $access_token" -H 'Content-Type: application/json' \
  -d "$oversize_body" \
  "$API_URL/v1/posts")" = "422" ] || fail "oversized post did not return 422"
pass "oversized post rejected with 422"

# A's author timeline lists the new post newest-first.
timeline="$(curl -sf -H "Authorization: Bearer $access_token" \
  "$API_URL/v1/users/$id_a/posts?limit=10")" || fail "GET author timeline failed"
[ "$(printf '%s' "$timeline" | python3 -c 'import json,sys; print(json.load(sys.stdin)["items"][0]["id"])')" = "$post_id" ] \
  || fail "post missing from author timeline"
pass "author timeline lists the post"

# B cannot delete A's post (403); A deletes it (204).
[ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $token_b" "$API_URL/v1/posts/$post_id")" = "403" ] \
  || fail "non-author delete did not return 403"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $access_token" "$API_URL/v1/posts/$post_id")" = "204" ] \
  || fail "author delete did not return 204"
pass "non-author delete 403; author delete 204"

timeline_after="$(curl -sf -H "Authorization: Bearer $access_token" \
  "$API_URL/v1/users/$id_a/posts")" || fail "GET author timeline after delete failed"
[ "$(printf '%s' "$timeline_after" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["items"]))')" = "0" ] \
  || fail "author timeline not empty after delete"
pass "author timeline empty after delete"

# --- Home feed round-trip (D3 part 2) ---------------------------------------
# A follows B again; B posts; A's home feed carries B's post (fan-out on
# write). Pagination walks cleanly (no dupes, terminates), and deleting a
# post removes it from the follower's feed (delete propagation).
echo "smoke: home feed round-trip (D3 part 2)"

[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $access_token" "$API_URL/v1/users/$id_b/follow")" = "201" ] \
  || fail "POST follow A→B (feed setup) did not return 201"

feed_post() {
  curl -sf -X POST \
    -H "Authorization: Bearer $token_b" -H 'Content-Type: application/json' \
    -d "{\"text\":\"$1\"}" \
    "$API_URL/v1/posts" || fail "POST /v1/posts (feed setup) failed"
}
fp1="$(feed_post "feed post one")"; fpid1="$(jget "$fp1" id)"
fp2="$(feed_post "feed post two")"; fpid2="$(jget "$fp2" id)"
fp3="$(feed_post "feed post three")"; fpid3="$(jget "$fp3" id)"
[ -n "$fpid1" ] && [ -n "$fpid2" ] && [ -n "$fpid3" ] || fail "feed post ids missing"
pass "B published 3 posts (fan-out on write)"

# A's home feed lists all three, newest first.
feed1="$(curl -sf -H "Authorization: Bearer $access_token" \
  "$API_URL/v1/feed/home?limit=10")" || fail "GET /v1/feed/home failed"
[ "$(printf '%s' "$feed1" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["items"]))')" = "3" ] \
  || fail "home feed does not list all 3 posts"
[ "$(printf '%s' "$feed1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["items"][0]["id"])')" = "$fpid3" ] \
  || fail "home feed not newest-first"
pass "home feed lists B's 3 posts, newest first"

# Pagination: limit=2 → pages of 2 and 1, no dupes, terminates.
p1="$(curl -sf -H "Authorization: Bearer $access_token" \
  "$API_URL/v1/feed/home?limit=2")" || fail "feed page 1 failed"
c1="$(printf '%s' "$p1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["next_cursor"] or "")')"
[ -n "$c1" ] || fail "feed page 1 missing next_cursor"
p2="$(curl -sf -H "Authorization: Bearer $access_token" \
  "$API_URL/v1/feed/home?limit=2&cursor=$c1")" || fail "feed page 2 failed"
c2="$(printf '%s' "$p2" | python3 -c 'import json,sys; print(json.load(sys.stdin)["next_cursor"] or "")')"
[ -z "$c2" ] || fail "feed page 2 should be the last page"
all_ids="$(printf '%s\n%s' "$p1" "$p2" | python3 -c '
import json,sys
ids=[it["id"] for pg in sys.stdin for it in json.loads(pg)["items"]]
print(len(ids), len(set(ids)))')"
[ "$all_ids" = "3 3" ] || fail "feed pagination lost/duplicated posts ($all_ids)"
pass "feed pagination: 2+1 pages, no dupes, terminates"

# Delete propagation: B deletes post two → it leaves A's feed.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $token_b" "$API_URL/v1/posts/$fpid2")" = "204" ] \
  || fail "delete of feed post did not return 204"
feed_after="$(curl -sf -H "Authorization: Bearer $access_token" \
  "$API_URL/v1/feed/home?limit=10")" || fail "GET /v1/feed/home after delete failed"
[ "$(printf '%s' "$feed_after" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["items"]))')" = "2" ] \
  || fail "deleted post still in home feed"
printf '%s' "$feed_after" | python3 -c "
import json,sys
ids=[it['id'] for it in json.load(sys.stdin)['items']]
assert '$fpid2' not in ids, 'deleted post id still present'
assert '$fpid1' in ids and '$fpid3' in ids, 'wrong posts remain'
" || fail "delete propagation left wrong state"
pass "delete propagation: post removed from follower feed"

# A unfollows B so later runs start clean.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $access_token" "$API_URL/v1/users/$id_b/follow")" = "204" ] \
  || fail "DELETE follow (feed teardown) did not return 204"

# --- Recommendations v1 ranking (E5) ---------------------------------------
# Engagement moves posts: A posts X, then Y seconds later. The baseline
# feed is newest-first (Y above X); after B likes X, X outranks Y. One
# like contributes 0.3*0.1 = +0.03 while the ~1s recency gap costs
# ~0.000005, so the flip is deterministic, not timing-luck.
echo "smoke: recommendations v1 ranking (E5)"

rx="$(curl -sf -X POST -H "Authorization: Bearer $access_token" \
  -H 'Content-Type: application/json' -d '{"text":"rank candidate older"}' \
  "$API_URL/v1/posts")" || fail "POST /v1/posts (rank X) failed"
rxid="$(jget "$rx" id)"
ry="$(curl -sf -X POST -H "Authorization: Bearer $access_token" \
  -H 'Content-Type: application/json' -d '{"text":"rank candidate newer"}' \
  "$API_URL/v1/posts")" || fail "POST /v1/posts (rank Y) failed"
ryid="$(jget "$ry" id)"
[ -n "$rxid" ] && [ -n "$ryid" ] || fail "rank post ids missing"

rank_pos() {
  printf '%s' "$1" | python3 -c "
import json,sys
ids=[it['id'] for it in json.load(sys.stdin)['items']]
print(ids.index('$2'), ids.index('$3'))"
}

feed_rank0="$(curl -sf -H "Authorization: Bearer $access_token" \
  "$API_URL/v1/feed/home?limit=10")" || fail "GET /v1/feed/home (rank baseline) failed"
pos0="$(rank_pos "$feed_rank0" "$rxid" "$ryid")"
[ "${pos0% *}" -gt "${pos0#* }" ] \
  || fail "baseline feed not newest-first (X@${pos0% *}, Y@${pos0#* })"
pass "rank baseline: newer post Y above older post X"

[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_b" "$API_URL/v1/posts/$rxid/like")" = "201" ] \
  || fail "POST /v1/posts/{id}/like (rank setup) did not return 201"

feed_rank1="$(curl -sf -H "Authorization: Bearer $access_token" \
  "$API_URL/v1/feed/home?limit=10")" || fail "GET /v1/feed/home (rank after like) failed"
pos1="$(rank_pos "$feed_rank1" "$rxid" "$ryid")"
[ "${pos1% *}" -lt "${pos1#* }" ] \
  || fail "like did not move older post above newer (X@${pos1% *}, Y@${pos1#* })"
pass "ranking: liked older post X outranks newer post Y"

# Cleanup: B unlikes X; A deletes both posts.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $token_b" "$API_URL/v1/posts/$rxid/like")" = "204" ] \
  || fail "DELETE /v1/posts/{id}/like (rank cleanup) did not return 204"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $access_token" "$API_URL/v1/posts/$rxid")" = "204" ] \
  || fail "DELETE rank post X did not return 204"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $access_token" "$API_URL/v1/posts/$ryid")" = "204" ] \
  || fail "DELETE rank post Y did not return 204"
pass "rank cleanup done"

# --- Language bridge round-trip (E4) ----------------------------------------
# The stub backend stands in for IndicTrans2 (CI has no model to pull);
# it returns a visibly-marked "[tgt] text" so the round-trip proves the
# plumbing without pretending to translate. Exercises: direct
# POST /v1/ai/translate through the Go proxy, then the ?translate=xx
# feed convenience (best-effort decoration of the home timeline).
echo "smoke: language bridge round-trip (E4)"
TRANSLATE_BACKEND=stub docker compose up -d --no-deps ai
wait_for 120 "ai /healthz (translate stub)" curl -sf http://localhost:8000/healthz

tr_out="$(curl -sf -X POST -H "Authorization: Bearer $access_token" \
  -H 'Content-Type: application/json' \
  -d '{"text":"namaste duniya","target":"hi"}' "$API_URL/v1/ai/translate")" \
  || fail "POST /v1/ai/translate through the proxy failed"
printf '%s' "$tr_out" | python3 -c '
import json,sys
d=json.load(sys.stdin)
assert d["text"]=="[hi] namaste duniya", d
assert d["target"]=="hi", d
assert d["source"]=="en", d
' || fail "/v1/ai/translate did not return the stub translation"
pass "POST /v1/ai/translate 200 through the proxy (stub backend, subject forwarded)"

# Feed convenience: A follows B; B posts; A's feed with ?translate=gu
# carries the translated rendering alongside the original.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $access_token" "$API_URL/v1/users/$id_b/follow")" = "201" ] \
  || fail "POST follow A→B (translate setup) did not return 201"
tr_post="$(curl -sf -X POST -H "Authorization: Bearer $token_b" \
  -H 'Content-Type: application/json' -d '{"text":"translate me please"}' \
  "$API_URL/v1/posts")" || fail "POST /v1/posts (translate setup) failed"
tr_pid="$(jget "$tr_post" id)"
[ -n "$tr_pid" ] || fail "translate post id missing"

feed_tr="$(curl -sf -H "Authorization: Bearer $access_token" \
  "$API_URL/v1/feed/home?limit=10&translate=gu")" \
  || fail "GET /v1/feed/home?translate=gu failed"
printf '%s' "$feed_tr" | python3 -c '
import json,sys
items=json.load(sys.stdin)["items"]
it=[x for x in items if x["id"]=="'"$tr_pid"'"]
assert it, "translated post missing from feed"
assert it[0]["text"]=="translate me please", "original text mutated"
assert it[0].get("translated_text")=="[gu] translate me please", it[0]
assert it[0].get("translated_from")=="en", it[0]
' || fail "?translate=gu did not decorate the feed item"
pass "feed ?translate=gu decorates items best-effort (translated_text + translated_from)"

# Unsupported target is a client bug: 400, not a feed failure.
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $access_token" \
  "$API_URL/v1/feed/home?translate=xx")" = "400" ] \
  || fail "?translate=xx did not 400 invalid_translate_target"
pass "feed ?translate=xx 400s on unsupported targets"

# Teardown: B deletes the post, A unfollows.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $token_b" "$API_URL/v1/posts/$tr_pid")" = "204" ] \
  || fail "delete of translate post did not return 204"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $access_token" "$API_URL/v1/users/$id_b/follow")" = "204" ] \
  || fail "DELETE follow (translate teardown) did not return 204"

# Restore the ai service to its compose defaults for later sections.
docker compose up -d --no-deps ai
wait_for 120 "ai /healthz (translate defaults restored)" curl -sf http://localhost:8000/healthz

# --- Video upload + transcode round-trip (D4) --------------------------------
# The full clip journey: upload intent → PUT bytes to SeaweedFS →
# complete → the transcode worker builds HLS + poster → poll the media
# row to ready. The worker is started here (not with api/chat above) so
# its liveness heartbeat gates only this section.
echo "smoke: video upload + transcode round-trip (D4)"

if ! command -v ffmpeg >/dev/null 2>&1; then
  # The worker itself always runs ffmpeg (inside its image); only the
  # sample-clip minting below needs a host ffmpeg. CI runners ship one;
  # without it the section is skipped, not failed.
  echo "smoke: SKIP — no host ffmpeg, cannot mint a sample clip"
else
  echo "smoke: starting transcode-worker"
  docker compose up -d transcode-worker
  worker_heartbeat() { # true once the worker's TTL heartbeat key exists
    [ "$(docker compose exec -T valkey valkey-cli get worker:transcode:heartbeat \
      2>/dev/null | tr -d '\r')" != "(nil)" ]
  }
  wait_for 90 "transcode-worker heartbeat" worker_heartbeat
  pass "transcode-worker alive (heartbeat key present)"

  # E3: boot the caption worker alongside the transcode worker and gate
  # on its TTL heartbeat — it drains media:captions once we opt in below.
  echo "smoke: starting caption-worker"
  docker compose up -d caption-worker
  caption_heartbeat() { # true once the caption worker's TTL heartbeat key exists
    [ "$(docker compose exec -T valkey valkey-cli get worker:captions:heartbeat \
      2>/dev/null | tr -d '\r')" != "(nil)" ]
  }
  wait_for 90 "caption-worker heartbeat" caption_heartbeat
  pass "caption-worker alive (heartbeat key present)"

  # Mint a 2s 640x360 test clip on the host with a sine-wave audio
  # track. 360p matches the lowest ladder rung exactly, so the worker
  # never has to upscale a clip smaller than its rung widths; the audio
  # track (even a plain tone) gives the caption worker something to
  # decode — faster-whisper needs an audio stream present.
  clip="/tmp/sutra-smoke-clip.mp4"
  ffmpeg -v error -y -f lavfi -i "testsrc=duration=2:size=640x360:rate=15" \
    -f lavfi -i "sine=frequency=440:duration=2" \
    -c:v libx264 -pix_fmt yuv420p -c:a aac -shortest "$clip" \
    || fail "ffmpeg could not mint the sample clip"

  intent="$(curl -sf -X POST \
    -H "Authorization: Bearer $access_token" -H 'Content-Type: application/json' \
    -d '{"kind":"video","filename":"smoke-clip.mp4","mime_type":"video/mp4"}' \
    "$API_URL/v1/media/upload")" || fail "POST /v1/media/upload failed"
  media_id="$(jget "$intent" media_id)"
  upload_url="$(jget "$intent" upload_url)"
  [ -n "$media_id" ] && [ -n "$upload_url" ] || fail "upload intent response missing fields"
  pass "upload intent minted ($media_id)"

  # upload_url points at the compose-internal volume hostname; the host
  # reaches the same server via localhost:8080 (seaweedfs publishes it).
  host_upload_url="$(printf '%s' "$upload_url" | sed 's#^http://seaweedfs:8080#http://localhost:8080#')"
  curl -sf -X PUT -H 'Content-Type: video/mp4' \
    --data-binary "@$clip" "$host_upload_url" \
    || fail "PUT of clip bytes to SeaweedFS failed"
  pass "clip bytes PUT to SeaweedFS"

  completed="$(curl -sf -X POST \
    -H "Authorization: Bearer $access_token" \
    "$API_URL/v1/media/$media_id/complete")" || fail "POST /v1/media/{id}/complete failed"
  [ "$(jget "$completed" status)" = "uploaded" ] \
    || fail "complete did not leave the video in 'uploaded' status"
  pass "complete → video 'uploaded', transcode job enqueued"

  # Poll for the worker to finish: ready carries poster + HLS renditions.
  deadline=$((SECONDS + 180))
  status=""
  while [ $SECONDS -lt "$deadline" ]; do
    status="$(jget "$(curl -sf -H "Authorization: Bearer $access_token" \
      "$API_URL/v1/media/$media_id")" status)"
    case "$status" in
      ready|failed) break ;;
    esac
    sleep 3
  done
  [ "$status" = "ready" ] || fail "video never reached 'ready' (last status: '$status')"
  final="$(curl -sf -H "Authorization: Bearer $access_token" \
    "$API_URL/v1/media/$media_id")"
  printf '%s' "$final" | python3 -c "
import json,sys
m=json.load(sys.stdin)
assert m['poster_url'], 'poster_url missing'
assert m['playback_url'], 'playback_url missing'
assert len(m['renditions']) >= 1, 'no renditions'
print('renditions:', ','.join(r['name'] for r in m['renditions']))
" || fail "ready video missing poster/playback/renditions"
  pass "video ready: poster + playback URL + HLS renditions published"

  # The D4a posts gate + the E3 auto-caption round-trip: a post may
  # reference the ready media, and auto_caption=true asks the caption
  # worker to transcribe it. PublishHooks.AfterCreateCaptions →
  # media.RequestCaptions (the row is ready, so the job is enqueued onto
  # media:captions immediately; idempotent on retry) → the caption
  # worker transcribes → caption_status flips to ready with a VTT
  # caption_url sidecar.
  media_post="$(curl -sf -X POST \
    -H "Authorization: Bearer $access_token" -H 'Content-Type: application/json' \
    -d "{\"text\":\"smoke clip post\",\"media_ids\":[\"$media_id\"],\"auto_caption\":true}" \
    "$API_URL/v1/posts")" || fail "POST /v1/posts with ready media failed"
  media_post_id="$(jget "$media_post" id)"
  pass "post with ready media attached (201) — posts gate honored; auto_caption=true opted in"

  # Poll for the caption worker: ready carries the VTT sidecar.
  deadline=$((SECONDS + 300))
  cap_status=""
  while [ $SECONDS -lt "$deadline" ]; do
    cap_status="$(jget "$(curl -sf -H "Authorization: Bearer $access_token" \
      "$API_URL/v1/media/$media_id")" caption_status)"
    case "$cap_status" in
      ready|failed) break ;;
    esac
    sleep 5
  done
  [ "$cap_status" = "ready" ] || fail "captions never reached 'ready' (last status: '$cap_status')"
  printf '%s' "$(curl -sf -H "Authorization: Bearer $access_token" \
    "$API_URL/v1/media/$media_id")" | python3 -c "
import json,sys
m=json.load(sys.stdin)
assert m['caption_url'], 'caption_url missing'
assert m['auto_caption'] is True, 'auto_caption opt-in not sticky'
print('caption_url:', m['caption_url'])
" || fail "ready caption missing caption_url"
  pass "auto-captions ready: VTT caption_url sidecar published"
  [ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
    -H "Authorization: Bearer $access_token" "$API_URL/v1/posts/$media_post_id")" = "204" ] \
    || fail "cleanup delete of media post failed"
  pass "caption post deleted (cleanup)"
  rm -f "$clip"
fi

# --- Realtime events round-trip (D5) ----------------------------------------
# The API publishes post:new / follower:new to the chat's internal
# endpoint; the chat fans them out on Phoenix `user:<subject>` topics.
# Both services share CHAT_INTERNAL_TOKEN (generated at the top of this
# script; the API's publisher is therefore enabled for this run, so the
# follows/posts above already exercised the real API→chat publish path
# best-effort). Prove the endpoint is live and auth is enforced:
# no token → 401, wrong token → 401, unknown event → 422, and a valid
# envelope → 202 accepted for broadcast. Topic join-auth (users get only
# their own topic) is covered by the chat channel tests in CI.
echo "smoke: realtime events endpoint (D5)"

CHAT_URL="http://$CHAT_HOST:$CHAT_PORT"
internal_status() { # internal_status [token] <json> — POST /internal/events, print the HTTP status
  local token="$1" body="$2" args=(-s -o /dev/null -w '%{http_code}' -X POST)
  [ -n "$token" ] && args+=(-H "Authorization: Bearer $token")
  args+=(-H 'Content-Type: application/json' -d "$body" "$CHAT_URL/internal/events")
  curl "${args[@]}"
}

evt='{"topics":["user:sutra-smoke-b"],"event":"follower:new","dedupe_key":"follow:smoke-d5","payload":{"follower_id":"smoke-d5","follower_handle":"smoke_a","created_at":"2026-10-18T10:00:00Z"}}'
[ "$(internal_status "" "$evt")" = "401" ] \
  || fail "/internal/events without token did not 401"
pass "internal/events 401 without token"

[ "$(internal_status "wrong-token" "$evt")" = "401" ] \
  || fail "/internal/events with wrong token did not 401"
pass "internal/events 401 with wrong token"

[ "$(internal_status "$CHAT_INTERNAL_TOKEN" '{"topics":["user:x"],"event":"nope","dedupe_key":"k","payload":{}}')" = "422" ] \
  || fail "/internal/events with unknown event did not 422"
pass "internal/events 422 on unknown event"

[ "$(internal_status "$CHAT_INTERNAL_TOKEN" "$evt")" = "202" ] \
  || fail "/internal/events valid envelope did not 202"
pass "internal/events 202: follower:new accepted for user:sutra-smoke-b"

# --- Velocity abuse guards (D6) -------------------------------------------
# Tighten the rolling abuse caps to 1/hour and prove the 429 contract:
# posts: 1st create → 201, 2nd within the hour → 429 post_velocity_limited
# + Retry-After; follows: 1st new edge → 201, a second NEW edge → 429
# follow_velocity_limited + Retry-After, while an idempotent retry of an
# EXISTING edge still 200s (retries must never consume budget — the D6
# part-1 design fix). A throwaway Hydra identity unique to this run keeps
# the trailing-hour counters at zero (SMOKE_KEEP_UP=1 re-runs stay green);
# id_a/id_b from the D2 section are the follow targets.
echo "smoke: velocity abuse guards (D6)"

echo "smoke: recreating api with tight velocity caps (1/hour)"
POST_VELOCITY_PER_HOUR=1 FOLLOW_VELOCITY_PER_HOUR=1 docker compose up -d --no-deps api
wait_for 90 "api /healthz (velocity caps on)" curl -sf "$API_URL/healthz"

vel_client="sutra-smoke-v-$(date +%s)"
echo "smoke: creating hydra oauth2 client '$vel_client'"
SMOKE_HYDRA_CLIENT_V_SECRET="${SMOKE_HYDRA_CLIENT_V_SECRET:-$(openssl rand -hex 16)}"
if ! docker compose run --rm hydra hydra create client \
    --endpoint http://hydra:4445 \
    --id "$vel_client" \
    --secret "$SMOKE_HYDRA_CLIENT_V_SECRET" \
    --grant-type client_credentials \
    --grant-type urn:ietf:params:oauth:grant-type:jwt-bearer \
    --response-type token \
    --scope openid \
    --audience sutra-api \
    --token-endpoint-auth-method client_secret_basic \
    >/tmp/sutra-hydra-client-v.out 2>&1; then
  cat /tmp/sutra-hydra-client-v.out >&2
  fail "hydra oauth2 client '$vel_client' creation failed"
else
  pass "hydra oauth2 client '$vel_client' created"
fi

token_v_json="$(curl -sf -u "$vel_client:$SMOKE_HYDRA_CLIENT_V_SECRET" \
  --data-urlencode 'grant_type=client_credentials' \
  --data-urlencode 'audience=sutra-api' \
  --data-urlencode 'scope=openid' \
  http://localhost:4444/oauth2/token)" || fail "hydra token request for '$vel_client' failed"
token_v="$(printf '%s' "$token_v_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')"

# Provision V (JIT via /v1/me) so posts/follows have an author/follower.
curl -sf -H "Authorization: Bearer $token_v" "$API_URL/v1/me" >/dev/null \
  || fail "GET /v1/me for '$vel_client' failed"

# Post velocity: 1st create → 201, 2nd within the hour → 429
# post_velocity_limited with a Retry-After header.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_v" -H 'Content-Type: application/json' \
  -d '{"text":"velocity post one"}' \
  "$API_URL/v1/posts")" = "201" ] || fail "velocity: first post did not return 201"
vel_hdr="/tmp/sutra-vel-hdr.txt"; vel_body="/tmp/sutra-vel-body.json"
vel_code="$(curl -s -o "$vel_body" -D "$vel_hdr" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_v" -H 'Content-Type: application/json' \
  -d '{"text":"velocity post two"}' \
  "$API_URL/v1/posts")"
[ "$vel_code" = "429" ] || fail "velocity: second post did not return 429 (got $vel_code)"
grep -qi '^Retry-After: [1-9]' "$vel_hdr" \
  || fail "velocity: post 429 missing Retry-After header"
[ "$(python3 -c 'import json; print(json.load(open("'"$vel_body"'"))["error"]["code"])')" = "post_velocity_limited" ] \
  || fail "velocity: post 429 envelope code is not post_velocity_limited"
pass "post velocity: 201 then 429 post_velocity_limited + Retry-After"

# Follow velocity: V→A → 201; V→B (a NEW edge) → 429 follow_velocity_limited
# with a Retry-After header; retrying the EXISTING V→A edge → 200.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_v" "$API_URL/v1/users/$id_a/follow")" = "201" ] \
  || fail "velocity: follow V→A did not return 201"
fvel_code="$(curl -s -o "$vel_body" -D "$vel_hdr" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_v" "$API_URL/v1/users/$id_b/follow")"
[ "$fvel_code" = "429" ] || fail "velocity: second new follow did not return 429 (got $fvel_code)"
grep -qi '^Retry-After: [1-9]' "$vel_hdr" \
  || fail "velocity: follow 429 missing Retry-After header"
[ "$(python3 -c 'import json; print(json.load(open("'"$vel_body"'"))["error"]["code"])')" = "follow_velocity_limited" ] \
  || fail "velocity: follow 429 envelope code is not follow_velocity_limited"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_v" "$API_URL/v1/users/$id_a/follow")" = "200" ] \
  || fail "velocity: idempotent retry of existing follow did not return 200"
pass "follow velocity: 201 then 429 follow_velocity_limited + Retry-After; retry → 200"

# Restore the default caps so later sections use production values.
echo "smoke: restoring api to default velocity caps"
docker compose up -d --no-deps api
wait_for 90 "api /healthz (defaults restored)" curl -sf "$API_URL/healthz"

# --- Commerce catalog round-trip (F1b part 2) --------------------------------
# A throwaway Hydra identity (fresh every run) registers a seller, lists a
# product, adds a variant, moves inventory, then archives everything —
# the full F1a/F1b HTTP surface through the production auth path (bypass
# off). The identity is per-run because seller registration is first-come
# (one seller per user; a second registration is 409).
echo "smoke: commerce catalog round-trip (F1b)"

com_client="sutra-smoke-c-$(date +%s)"
echo "smoke: creating hydra oauth2 client '$com_client'"
SMOKE_HYDRA_CLIENT_C_SECRET=${SMOKE_HYDRA_CLIENT_C_SECRET:-$(openssl rand -hex 16)}
if ! docker compose run --rm hydra hydra create client \
    --endpoint http://hydra:4445 \
    --id "$com_client" \
    --secret "$SMOKE_HYDRA_CLIENT_C_SECRET" \
    --grant-type client_credentials \
    --grant-type urn:ietf:params:oauth:grant-type:jwt-bearer \
    --response-type token \
    --scope openid \
    --audience sutra-api \
    --token-endpoint-auth-method client_secret_basic \
    >/tmp/sutra-hydra-client-c.out 2>&1; then
  cat /tmp/sutra-hydra-client-c.out >&2
  fail "hydra oauth2 client '$com_client' creation failed"
else
  pass "hydra oauth2 client '$com_client' created"
fi

token_c_json="$(curl -sf -u "$com_client:$SMOKE_HYDRA_CLIENT_C_SECRET" \
  --data-urlencode 'grant_type=client_credentials' \
  --data-urlencode 'audience=sutra-api' \
  --data-urlencode 'scope=openid' \
  http://localhost:4444/oauth2/token)" || fail "hydra token request for '$com_client' failed"
token_c="$(printf '%s' "$token_c_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')"

com_slug="smoke-store-c-$(date +%s)"
seller_json="$(curl -sf -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d "{\"store_name\":\"Smoke Store\",\"store_slug\":\"$com_slug\"}" \
  "$API_URL/v1/sellers")" || fail "POST /v1/sellers failed"
seller_id="$(printf '%s' "$seller_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
[ "$(printf '%s' "$seller_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["store_slug"])')" = "$com_slug" ] \
  || fail "seller slug mismatch"
[ "$(printf '%s' "$seller_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["commission_rate_bps"])')" = "0" ] \
  || fail "seller commission_rate_bps is not 0 (0%-commission model)"
pass "seller registered: $com_slug ($seller_id), commission 0bps"

# Second registration for the same user → 409 seller_already_registered.
com_body="/tmp/sutra-com-body.json"
com_code="$(curl -s -o "$com_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"store_name":"Smoke Store Two"}' \
  "$API_URL/v1/sellers")"
[ "$com_code" = "409" ] || fail "second seller registration did not return 409 (got $com_code)"
[ "$(python3 -c 'import json; print(json.load(open("'"$com_body"'"))["error"]["code"])')" = "seller_already_registered" ] \
  || fail "second registration envelope code is not seller_already_registered"
pass "duplicate registration → 409 seller_already_registered"

# Public storefront reads.
seller_get="$(curl -sf -H "Authorization: Bearer $token_c" "$API_URL/v1/sellers/$seller_id")" \
  || fail "GET /v1/sellers/{id} failed"
[ "$(printf '%s' "$seller_get" | python3 -c 'import json,sys; print(json.load(sys.stdin)["store_name"])')" = "Smoke Store" ] \
  || fail "GET seller returned wrong store_name"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/sellers/not-a-uuid")" = "400" ] || fail "GET seller with bad id did not return 400"
pass "GET /v1/sellers/{id}: 200 storefront read, 400 on bad id"

cats="$(curl -sf -H "Authorization: Bearer $token_c" "$API_URL/v1/categories")" \
  || fail "GET /v1/categories failed"
python3 -c '
import json, sys
tree = json.load(sys.stdin)["categories"]
assert len(tree) >= 10, "fewer than 10 root categories"
elec = next(c for c in tree if c["slug"] == "electronics")
child_slugs = {c["slug"] for c in elec["children"]}
assert {"audio", "mobiles"} <= child_slugs, "electronics missing audio/mobiles children"
' <<<"$cats" || fail "category tree shape wrong"
pass "GET /v1/categories: tree with electronics → audio/mobiles"

# Product: create (draft) → list → activate → get.
prod_json="$(curl -sf -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"title":"Smoke Catalog Item","description":"smoke-test listing","category_slug":"audio"}' \
  "$API_URL/v1/products")" || fail "POST /v1/products failed"
prod_id="$(printf '%s' "$prod_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
[ "$(printf '%s' "$prod_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')" = "draft" ] \
  || fail "new product is not draft"
pass "product created as draft: $prod_id"

plist="$(curl -sf -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/products?seller_id=$seller_id")" || fail "GET /v1/products failed"
[ "$(printf '%s' "$plist" | python3 -c 'import json,sys; print(json.load(sys.stdin)["products"][0]["id"])')" = "$prod_id" ] \
  || fail "product missing from seller listing"
pass "GET /v1/products: newest-first storefront listing"

curl -sf -X PATCH -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"status":"active"}' "$API_URL/v1/products/$prod_id" >/dev/null \
  || fail "PATCH /v1/products/{id} (activate) failed"
[ "$(curl -sf -H "Authorization: Bearer $token_c" "$API_URL/v1/products/$prod_id" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')" = "active" ] \
  || fail "product status not active after PATCH"
pass "PATCH /v1/products/{id}: draft → active"

# Variant: add SKU → inventory starts at 0 → +100 → −150 refuses 409.
var_json="$(curl -sf -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"sku":"SMOKE-C-001","price_paise":149900,"mrp_paise":299900,"attributes":{"color":"black"}}' \
  "$API_URL/v1/products/$prod_id/variants")" || fail "POST /v1/products/{id}/variants failed"
var_id="$(printf '%s' "$var_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"sku":"SMOKE-C-001","price_paise":149900}' \
  "$API_URL/v1/products/$prod_id/variants")" = "409" ] \
  || fail "duplicate SKU did not return 409"
pass "variant created: $var_id; duplicate SKU → 409 sku_exists"

inv0="$(curl -sf -H "Authorization: Bearer $token_c" "$API_URL/v1/variants/$var_id/inventory")" \
  || fail "GET inventory failed"
[ "$(printf '%s' "$inv0" | python3 -c 'import json,sys; print(json.load(sys.stdin)["quantity"])')" = "0" ] \
  || fail "new variant inventory is not 0"
inv1="$(curl -sf -X POST -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"delta":100}' "$API_URL/v1/variants/$var_id/inventory/adjust")" \
  || fail "inventory adjust +100 failed"
[ "$(printf '%s' "$inv1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["quantity"])')" = "100" ] \
  || fail "quantity not 100 after +100"
com_code="$(curl -s -o "$com_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"delta":-150}' "$API_URL/v1/variants/$var_id/inventory/adjust")"
[ "$com_code" = "409" ] || fail "adjust below zero did not return 409 (got $com_code)"
[ "$(python3 -c 'import json; print(json.load(open("'"$com_body"'"))["error"]["code"])')" = "insufficient_stock" ] \
  || fail "insufficient_stock envelope code missing"
pass "inventory: 0 → +100 = 100; −150 → 409 insufficient_stock"

# Cleanup: archive variant, archive product (nothing is hard-deleted).
[ "$(curl -sf -X DELETE -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/variants/$var_id" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')" = "archived" ] \
  || fail "DELETE variant did not archive"
[ "$(curl -sf -X DELETE -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/products/$prod_id" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')" = "archived" ] \
  || fail "DELETE product did not archive"
pass "variant + product archived (cleanup)"

# --- Commerce cart + order round-trip (F3 part 2b) ---------------------------
# A throwaway Hydra identity doubles as buyer and seller here: the cart
# round-trip (add → view → set → over-stock refusal) flows into COD
# checkout, the full status-machine walk (created → confirmed → shipped →
# delivered, with illegal edges rejected), a buyer-cancel proving the
# reservation release, and the error matrix. Everything asserts on the
# real compose stack, not mocks.
echo "smoke: commerce cart + order round-trip (F3)"

# Fresh sellable listing: product → active, one variant, stock 10.
ordp_json="$(curl -sf -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"title":"Smoke Order Item","description":"smoke-test checkout","category_slug":"audio"}' \
  "$API_URL/v1/products")" || fail "POST /v1/products (order item) failed"
ordp_id="$(printf '%s' "$ordp_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
curl -sf -X PATCH -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"status":"active"}' "$API_URL/v1/products/$ordp_id" >/dev/null \
  || fail "PATCH /v1/products/{id} (activate order item) failed"
ordv_json="$(curl -sf -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"sku":"SMOKE-O-001","price_paise":149900,"mrp_paise":299900}' \
  "$API_URL/v1/products/$ordp_id/variants")" || fail "POST variant (order item) failed"
ordv_id="$(printf '%s' "$ordv_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
curl -sf -X POST -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"delta":10}' "$API_URL/v1/variants/$ordv_id/inventory/adjust" >/dev/null \
  || fail "inventory adjust +10 (order item) failed"
pass "order fixture: product $ordp_id active, variant $ordv_id stocked 10"

# Cart: add 2 → 201, line_total = 2 × 149900 paise.
ord_line="$(curl -sf -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d "{\"variant_id\":\"$ordv_id\",\"quantity\":2}" \
  "$API_URL/v1/cart/items")" || fail "POST /v1/cart/items failed"
[ "$(printf '%s' "$ord_line" | python3 -c 'import json,sys; print(json.load(sys.stdin)["line_total_paise"])')" = "299800" ] \
  || fail "cart line_total wrong (want 2 × 149900)"
pass "cart add: 2 × SMOKE-O-001 → 201, line_total 299800 paise"

# Over-stock add refuses: 2 + 999 > 10 available → 409 insufficient_stock.
com_code="$(curl -s -o "$com_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d "{\"variant_id\":\"$ordv_id\",\"quantity\":999}" \
  "$API_URL/v1/cart/items")"
[ "$com_code" = "409" ] || fail "cart over-stock add did not return 409 (got $com_code)"
[ "$(python3 -c 'import json; print(json.load(open("'"$com_body"'"))["error"]["code"])')" = "insufficient_stock" ] \
  || fail "cart over-stock envelope code is not insufficient_stock"
pass "cart over-stock add → 409 insufficient_stock"

# PATCH quantity to 3 → 200; cart view shows the live-price subtotal.
ord_line="$(curl -sf -X PATCH \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"quantity":3}' "$API_URL/v1/cart/items/$ordv_id")" \
  || fail "PATCH /v1/cart/items/{variant_id} failed"
[ "$(printf '%s' "$ord_line" | python3 -c 'import json,sys; print(json.load(sys.stdin)["line_total_paise"])')" = "449700" ] \
  || fail "cart line_total wrong after set to 3"
ord_cart="$(curl -sf -H "Authorization: Bearer $token_c" "$API_URL/v1/cart")" \
  || fail "GET /v1/cart failed"
[ "$(printf '%s' "$ord_cart" | python3 -c 'import json,sys; print(json.load(sys.stdin)["subtotal_paise"])')" = "449700" ] \
  || fail "cart subtotal wrong"
[ "$(printf '%s' "$ord_cart" | python3 -c 'import json,sys; print(json.load(sys.stdin)["unavailable_count"])')" = "0" ] \
  || fail "cart unavailable_count wrong"
pass "cart set qty 3 → 200; GET /v1/cart subtotal 449700 paise, 0 unavailable"

# Checkout (COD-first): 201, status created, payment_method cod; cart cleared.
ord_co="$(curl -sf -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  "$API_URL/v1/orders")" || fail "POST /v1/orders (checkout) failed"
ord_id="$(printf '%s' "$ord_co" | python3 -c 'import json,sys; print(json.load(sys.stdin)["orders"][0]["id"])')"
[ "$(printf '%s' "$ord_co" | python3 -c 'import json,sys; print(json.load(sys.stdin)["orders"][0]["status"])')" = "created" ] \
  || fail "checkout order not created"
[ "$(printf '%s' "$ord_co" | python3 -c 'import json,sys; print(json.load(sys.stdin)["orders"][0]["payment_method"])')" = "cod" ] \
  || fail "checkout payment_method not cod"
[ "$(printf '%s' "$ord_co" | python3 -c 'import json,sys; print(json.load(sys.stdin)["orders"][0]["total_paise"])')" = "449700" ] \
  || fail "checkout total wrong"
[ "$(curl -sf -H "Authorization: Bearer $token_c" "$API_URL/v1/cart" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["item_count"])')" = "0" ] \
  || fail "cart not cleared after checkout"
pass "checkout → 201, order $ord_id created/cod, total 449700 paise, cart cleared"

# Second checkout on the empty cart → 422 cart_empty.
com_code="$(curl -s -o "$com_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  "$API_URL/v1/orders")"
[ "$com_code" = "422" ] || fail "empty-cart checkout did not return 422 (got $com_code)"
[ "$(python3 -c 'import json; print(json.load(open("'"$com_body"'"))["error"]["code"])')" = "cart_empty" ] \
  || fail "empty-cart envelope code is not cart_empty"
pass "empty-cart checkout → 422 cart_empty"

# List as buyer (default) and seller; bad role → 400.
[ "$(curl -sf -H "Authorization: Bearer $token_c" "$API_URL/v1/orders" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["orders"][0]["id"])')" = "$ord_id" ] \
  || fail "GET /v1/orders (buyer) missing the order"
[ "$(curl -sf -H "Authorization: Bearer $token_c" "$API_URL/v1/orders?role=seller" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["orders"][0]["id"])')" = "$ord_id" ] \
  || fail "GET /v1/orders?role=seller missing the order"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/orders?role=bogus")" = "400" ] || fail "bad role did not return 400"
pass "order lists: buyer + seller views, bad role → 400"

# Detail: price snapshot; bad id → 400, ghost id → 404.
ord_det="$(curl -sf -H "Authorization: Bearer $token_c" "$API_URL/v1/orders/$ord_id")" \
  || fail "GET /v1/orders/{id} failed"
[ "$(printf '%s' "$ord_det" | python3 -c 'import json,sys; print(json.load(sys.stdin)["items"][0]["price_paise"])')" = "149900" ] \
  || fail "order line price snapshot wrong"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/orders/not-a-uuid")" = "400" ] || fail "bad order id did not return 400"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/orders/00000000-0000-0000-0000-000000000000")" = "404" ] \
  || fail "ghost order id did not return 404"
pass "order detail: snapshot 149900 paise, 400 on bad id, 404 on ghost"

# Price immutability: seller reprices the variant; the order keeps its snapshot.
curl -sf -X PATCH -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"price_paise":159900}' "$API_URL/v1/variants/$ordv_id" >/dev/null \
  || fail "PATCH variant (reprice) failed"
[ "$(curl -sf -H "Authorization: Bearer $token_c" "$API_URL/v1/orders/$ord_id" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["items"][0]["price_paise"])')" = "149900" ] \
  || fail "order line price changed after seller reprice"
pass "price snapshot immutable after seller reprice to 159900"

# State machine: confirmed → (illegal delivered) → shipped → delivered.
ord_st() { curl -sf -X PATCH -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d "{\"status\":\"$2\"}" "$API_URL/v1/orders/$1" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])'; }
[ "$(ord_st "$ord_id" confirmed)" = "confirmed" ] || fail "created → confirmed failed"
com_code="$(curl -s -o "$com_body" -w '%{http_code}' -X PATCH \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"status":"delivered"}' "$API_URL/v1/orders/$ord_id")"
[ "$com_code" = "422" ] || fail "confirmed → delivered did not return 422 (got $com_code)"
[ "$(python3 -c 'import json; print(json.load(open("'"$com_body"'"))["error"]["code"])')" = "invalid_transition" ] \
  || fail "invalid_transition envelope code missing"
[ "$(ord_st "$ord_id" shipped)" = "shipped" ] || fail "confirmed → shipped failed"
[ "$(ord_st "$ord_id" delivered)" = "delivered" ] || fail "shipped → delivered failed"
pass "state machine: confirmed → (delivered 422 invalid_transition) → shipped → delivered"

# Cancel a fresh order → reservation released (quantity 7, reserved 0).
curl -sf -X POST -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d "{\"variant_id\":\"$ordv_id\",\"quantity\":2}" "$API_URL/v1/cart/items" >/dev/null \
  || fail "cart add (second order) failed"
ord_id2="$(curl -sf -X POST -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  "$API_URL/v1/orders" | python3 -c 'import json,sys; print(json.load(sys.stdin)["orders"][0]["id"])')"
[ "$(ord_st "$ord_id2" cancelled)" = "cancelled" ] || fail "created → cancelled failed"
ord_inv="$(curl -sf -H "Authorization: Bearer $token_c" "$API_URL/v1/variants/$ordv_id/inventory")" \
  || fail "GET inventory (after cancel) failed"
[ "$(printf '%s' "$ord_inv" | python3 -c 'import json,sys; print(json.load(sys.stdin)["quantity"])')" = "7" ] \
  || fail "inventory quantity wrong after cancel (want 7)"
[ "$(printf '%s' "$ord_inv" | python3 -c 'import json,sys; print(json.load(sys.stdin)["reserved"])')" = "0" ] \
  || fail "inventory reserved not released after cancel"
pass "buyer cancel → reservation released (quantity 7, reserved 0)"

# Status-patch error matrix: unknown name → 422 invalid_status, missing → 400.
com_code="$(curl -s -o "$com_body" -w '%{http_code}' -X PATCH \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"status":"teleported"}' "$API_URL/v1/orders/$ord_id")"
[ "$com_code" = "422" ] || fail "unknown status did not return 422 (got $com_code)"
[ "$(python3 -c 'import json; print(json.load(open("'"$com_body"'"))["error"]["code"])')" = "invalid_status" ] \
  || fail "invalid_status envelope code missing"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X PATCH \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"nope":1}' "$API_URL/v1/orders/$ord_id")" = "400" ] \
  || fail "unknown field on status patch did not return 400"
pass "status-patch error matrix: 422 invalid_status, 400 on unknown field"

# Cleanup: archive variant + product (nothing is hard-deleted).
[ "$(curl -sf -X DELETE -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/variants/$ordv_id" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')" = "archived" ] \
  || fail "DELETE order variant did not archive"
[ "$(curl -sf -X DELETE -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/products/$ordp_id" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')" = "archived" ] \
  || fail "DELETE order product did not archive"
pass "order fixture archived (cleanup)"

# --- Product search round-trip (F2 part 2b) --------------------------------
# Meilisearch boots with the ephemeral per-run master key; the api is
# recreated with SEARCH_MASTER_KEY set and AUTH_BYPASS explicitly off (the
# production auth path). A throwaway Hydra identity lists 3 products —
# P1 gets two variants so the hit's min_price_paise = cheapest ACTIVE
# variant is asserted. Indexing is async (Meilisearch 202s then indexes in
# the background), so every visibility assertion polls with a deadline.
echo "smoke: product search round-trip (F2)"
echo "smoke: starting meilisearch"
MEILI_MASTER_KEY="$SMOKE_MEILI_KEY" docker compose up -d meilisearch
wait_for 60 "meilisearch /health" curl -sf http://localhost:7700/health
pass "meilisearch up (CE v1.38.0, /health 200)"

echo "smoke: recreating api with SEARCH_MASTER_KEY set (bypass off)"
AUTH_BYPASS= SEARCH_MASTER_KEY="$SMOKE_MEILI_KEY" docker compose up -d --no-deps api
wait_for 90 "api /healthz (search enabled)" curl -sf "$API_URL/healthz"

search_client="sutra-smoke-s-$(date +%s)"
echo "smoke: creating hydra oauth2 client '$search_client'"
SMOKE_HYDRA_CLIENT_S_SECRET="$(openssl rand -hex 16)"
if ! docker compose run --rm hydra hydra create client \
    --endpoint http://hydra:4445 \
    --id "$search_client" \
    --secret "$SMOKE_HYDRA_CLIENT_S_SECRET" \
    --grant-type client_credentials \
    --grant-type urn:ietf:params:oauth:grant-type:jwt-bearer \
    --response-type token \
    --scope openid \
    --audience sutra-api \
    --token-endpoint-auth-method client_secret_basic \
    >/tmp/sutra-hydra-client-s.out 2>&1; then
  cat /tmp/sutra-hydra-client-s.out >&2
  fail "hydra oauth2 client '$search_client' creation failed"
else
  pass "hydra oauth2 client '$search_client' created"
fi

token_s_json="$(curl -sf -u "$search_client:$SMOKE_HYDRA_CLIENT_S_SECRET" \
  --data-urlencode 'grant_type=client_credentials' \
  --data-urlencode 'audience=sutra-api' \
  --data-urlencode 'scope=openid' \
  http://localhost:4444/oauth2/token)" || fail "hydra token request for '$search_client' failed"
token_s="$(printf '%s' "$token_s_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')"

s_slug="smoke-search-s-$(date +%s)"
seller_s="$(curl -sf -X POST \
  -H "Authorization: Bearer $token_s" -H 'Content-Type: application/json' \
  -d "{\"store_name\":\"Smoke Search Store\",\"store_slug\":\"$s_slug\"}" \
  "$API_URL/v1/sellers")" || fail "search: seller registration failed"
seller_s_id="$(printf '%s' "$seller_s" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"

mkproduct() { # mkproduct <title> <description> → prints product id
  curl -sf -X POST -H "Authorization: Bearer $token_s" -H 'Content-Type: application/json' \
    -d "{\"title\":\"$1\",\"description\":\"$2\",\"category_slug\":\"audio\"}" \
    "$API_URL/v1/products" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])'
}
p1="$(mkproduct "Searchable Kurta Red" "handwoven cotton kurta, crimson")" \
  || fail "search: product 1 create failed"
p2="$(mkproduct "Searchable Kurta Blue" "handwoven cotton kurta, indigo")" \
  || fail "search: product 2 create failed"
p3="$(mkproduct "Searchable Salwar" "soft cotton salwar pants")" \
  || fail "search: product 3 create failed"

addvar() { # addvar <product_id> <sku> <price_paise>
  curl -sf -X POST -H "Authorization: Bearer $token_s" -H 'Content-Type: application/json' \
    -d "{\"sku\":\"$2\",\"price_paise\":$3}" \
    "$API_URL/v1/products/$1/variants" >/dev/null
}
addvar "$p1" "SMOKE-S-001" 249900 || fail "search: P1 variant 1 failed"
addvar "$p1" "SMOKE-S-002" 149900 || fail "search: P1 variant 2 failed"
addvar "$p2" "SMOKE-S-003" 199900 || fail "search: P2 variant failed"
addvar "$p3" "SMOKE-S-004" 99900  || fail "search: P3 variant failed"

for pid in "$p1" "$p2" "$p3"; do
  curl -sf -X PATCH -H "Authorization: Bearer $token_s" -H 'Content-Type: application/json' \
    -d '{"status":"active"}' "$API_URL/v1/products/$pid" >/dev/null \
    || fail "search: activating $pid failed"
done
pass "search: 3 products listed, variants added, all active"

echo "smoke: waiting for the 3 products to land in the index"
python3 - "$API_URL" "$token_s" "$p1,$p2,$p3" <<'PYEOF' || fail "search: products did not appear in the index"
import json, sys, time, urllib.request
api, token, want = sys.argv[1], sys.argv[2], set(sys.argv[3].split(","))
deadline = time.time() + 120
while True:
    req = urllib.request.Request(api + "/v1/search?q=searchable&limit=50",
                                 headers={"Authorization": "Bearer " + token})
    try:
        page = json.load(urllib.request.urlopen(req, timeout=10))
    except Exception:
        page = {"hits": []}
    got = {h["id"] for h in page.get("hits", [])}
    if want <= got:
        break
    if time.time() > deadline:
        print("smoke FAIL: indexed ids %s, wanted %s" % (got, want), file=sys.stderr)
        sys.exit(1)
    time.sleep(2)
PYEOF
pass "search: all 3 products indexed (async indexing caught up)"

# Hit shape: status/seller/category from the catalog row, min_price_paise =
# the cheapest ACTIVE variant (P1: 149900, not 249900), numeric sort keys.
hit1="$(curl -sf -G -H "Authorization: Bearer $token_s" \
  --data-urlencode 'q=searchable' --data-urlencode 'limit=50' \
  "$API_URL/v1/search")" || fail "search: q=searchable failed"
HIT1="$hit1" python3 - "$p1" "$p2" "$p3" "$seller_s_id" <<'PYEOF' || fail "search: hit fields wrong"
import json, os, sys
page = json.loads(os.environ["HIT1"])
by_id = {h["id"]: h for h in page["hits"]}
p1, p2, p3, sid = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
h1, h2, h3 = by_id[p1], by_id[p2], by_id[p3]
assert h1["title"] == "Searchable Kurta Red", h1
assert h1["status"] == "active" and h1["seller_id"] == sid and h1["category_slug"] == "audio", h1
assert h1["min_price_paise"] == 149900, ("min price is not the cheapest variant", h1)
assert h2["min_price_paise"] == 199900 and h3["min_price_paise"] == 99900, page
assert isinstance(h1["sort_seq"], int) and isinstance(h1["updated_at_unix"], int), h1
PYEOF
pass "search: hit shape (status/seller/category/min_price=cheapest variant/sort keys)"

# Typo tolerance: "kurtha" (one substitution, 6 chars ≥ oneTypo threshold).
typo_ids="$(curl -sf -G -H "Authorization: Bearer $token_s" \
  --data-urlencode 'q=kurtha' --data-urlencode 'limit=50' \
  "$API_URL/v1/search" \
  | python3 -c 'import json,sys; print(" ".join(h["id"] for h in json.load(sys.stdin)["hits"]))')"
case " $typo_ids " in
  *" $p1 "*|*" $p2 "*) pass "search: typo tolerance (kurtha → kurta hits P1/P2)" ;;
  *) fail "search: typo query kurtha matched none of P1/P2 (got: $typo_ids)" ;;
esac

# Facet filters: seller_id, category_slug, price band.
count_hits() { # count_hits <param>... → prints hit count
  local urlargs=() p
  for p in "$@"; do urlargs+=(--data-urlencode "$p"); done
  curl -sf -G -H "Authorization: Bearer $token_s" \
    --data-urlencode 'q=searchable' --data-urlencode "limit=50" \
    "${urlargs[@]}" "$API_URL/v1/search" \
    | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["hits"]))'
}
[ "$(count_hits "seller_id=$seller_s_id")" = "3" ] \
  || fail "search: seller_id filter did not return 3"
[ "$(count_hits "seller_id=00000000-0000-0000-0000-000000000000")" = "0" ] \
  || fail "search: unknown seller_id filter leaked hits"
[ "$(count_hits "category_slug=audio")" = "3" ] \
  || fail "search: category_slug=audio filter did not return 3"
[ "$(count_hits "category_slug=laptops")" = "0" ] \
  || fail "search: category_slug=laptops filter leaked hits"
[ "$(count_hits "max_price_paise=100000")" = "1" ] \
  || fail "search: price band max=100000 did not return exactly P3"
[ "$(count_hits "min_price_paise=150000" "max_price_paise=200000")" = "1" ] \
  || fail "search: price band 150000–200000 did not return exactly P2"
pass "search: seller/category/price filters"

# Keyset pagination: limit=2 walks 2 → 1, exact ids, no dupes, terminates.
pg1="$(curl -sf -G -H "Authorization: Bearer $token_s" \
  --data-urlencode 'q=searchable' --data-urlencode 'limit=2' \
  "$API_URL/v1/search")" || fail "search: page 1 failed"
c1="$(printf '%s' "$pg1" | python3 -c 'import json,sys; p=json.load(sys.stdin); print(" ".join(h["id"] for h in p["hits"]), p.get("next_cursor",""))')"
c1_ids="$(printf '%s' "$c1" | cut -d' ' -f1,2)"
c1_cur="$(printf '%s' "$c1" | cut -d' ' -f3)"
[ -n "$c1_cur" ] || fail "search: page 1 has no next_cursor"
[ "$c1_ids" = "$p3 $p2" ] || fail "search: page 1 order wrong (got: $c1_ids)"
pg2="$(curl -sf -G -H "Authorization: Bearer $token_s" \
  --data-urlencode 'q=searchable' --data-urlencode 'limit=2' \
  --data-urlencode "cursor=$c1_cur" \
  "$API_URL/v1/search")" || fail "search: page 2 failed"
PG2="$pg2" python3 - "$p1" <<'PYEOF' || fail "search: page 2 wrong"
import json, os, sys
page = json.loads(os.environ["PG2"])
ids = [h["id"] for h in page["hits"]]
assert ids == [sys.argv[1]], ids
assert not page.get("next_cursor"), "page 2 should be the last page"
PYEOF
pass "search: keyset pagination (2 + 1, no dupes, terminates)"

# 400 param matrix — malformed known params are 400 with stable codes.
s_body="/tmp/sutra-search-body.json"
expect400() { # expect400 <description> <expected_code> <query-string>
  code="$(curl -s -o "$s_body" -w '%{http_code}' -G -H "Authorization: Bearer $token_s" \
    --data-urlencode 'q=searchable' --data-urlencode "$3" "$API_URL/v1/search")"
  [ "$code" = "400" ] || fail "search: $1 did not return 400 (got $code)"
  got="$(python3 -c 'import json; print(json.load(open("'"$s_body"'"))["error"]["code"])')"
  [ "$got" = "$2" ] || fail "search: $1 envelope code is $got, want $2"
}
expect400 "limit=999" "invalid_limit" "limit=999"
expect400 "bad cursor" "invalid_cursor" "cursor=not-a-cursor"
expect400 "min>max price" "invalid_price_range" "min_price_paise=200&max_price_paise=100"
expect400 "bad status" "invalid_status" "status=hidden"
pass "search: 400 param matrix (invalid_limit/invalid_cursor/invalid_price_range/invalid_status)"

# Delete propagation: archive all three → hits vanish from the index.
for pid in "$p1" "$p2" "$p3"; do
  [ "$(curl -sf -X DELETE -H "Authorization: Bearer $token_s" \
    "$API_URL/v1/products/$pid" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')" = "archived" ] \
    || fail "search: archiving $pid failed"
done
echo "smoke: waiting for the archived products to leave the index"
python3 - "$API_URL" "$token_s" <<'PYEOF' || fail "search: archived products still in the index"
import json, sys, time, urllib.request
api, token = sys.argv[1], sys.argv[2]
deadline = time.time() + 120
while True:
    req = urllib.request.Request(api + "/v1/search?q=searchable&limit=50",
                                 headers={"Authorization": "Bearer " + token})
    try:
        page = json.load(urllib.request.urlopen(req, timeout=10))
    except Exception:
        page = {"hits": []}
    if not page.get("hits"):
        break
    if time.time() > deadline:
        print("smoke FAIL: still indexed: %s" % [h["id"] for h in page["hits"]], file=sys.stderr)
        sys.exit(1)
    time.sleep(2)
PYEOF
pass "search: archive → documents leave the index (cleanup)"

# Restore the api to searchless defaults; prove the 503 contract on the way:
# with no SEARCH_MASTER_KEY the proxy is disabled → 503 search_unavailable
# + Retry-After (never a 500 — search is a dependency, not a bug).
echo "smoke: restoring api to searchless defaults"
docker compose up -d --no-deps api
wait_for 90 "api /healthz (searchless)" curl -sf "$API_URL/healthz"
s503_hdr="/tmp/sutra-search-503-hdr.txt"
s503_code="$(curl -s -o "$s_body" -D "$s503_hdr" -w '%{http_code}' \
  -H "Authorization: Bearer $token_s" -G --data-urlencode 'q=searchable' \
  "$API_URL/v1/search")"
[ "$s503_code" = "503" ] || fail "search: unconfigured search did not return 503 (got $s503_code)"
grep -qi '^Retry-After: [1-9]' "$s503_hdr" \
  || fail "search: 503 missing Retry-After header"
[ "$(python3 -c 'import json; print(json.load(open("'"$s_body"'"))["error"]["code"])')" = "search_unavailable" ] \
  || fail "search: 503 envelope code is not search_unavailable"
pass "search: unconfigured → 503 search_unavailable + Retry-After"

# --- Chat websocket round-trip --------------------------------------------
wait_for 90 "chat websocket listener" \
  python3 -c "import socket; socket.create_connection(('$CHAT_HOST', $CHAT_PORT), timeout=3).close()"
python3 scripts/smoke_ws.py "$CHAT_HOST" "$CHAT_PORT" || fail "chat websocket round-trip"

# --- Chat load probe (short, C4) --------------------------------------------
# Exercises the bench_ws.py harness against the real stack on every run so
# CI proves it works; prints a JSON summary. Full benchmark numbers go to
# docs/benchmarks.md. Override with SMOKE_BENCH_ARGS, skip with
# SMOKE_SKIP_BENCH=1.
if [ "${SMOKE_SKIP_BENCH:-0}" != "1" ]; then
  # shellcheck disable=SC2086
  python3 scripts/bench_ws.py "$CHAT_HOST" "$CHAT_PORT" --fail-on-errors ${SMOKE_BENCH_ARGS:---conns 20 --msgs 10} \
    || fail "chat websocket load probe"
  pass "chat websocket load probe (JSON summary above)"
fi

# --- Remaining services -----------------------------------------------------
curl -sf http://localhost:9333/cluster/status >/dev/null \
  || fail "seaweedfs /cluster/status"
pass "seaweedfs /cluster/status 200"

[ "$(docker compose exec -T valkey valkey-cli ping 2>/dev/null | tr -d '\r')" = "PONG" ] \
  || fail "valkey did not answer PONG"
pass "valkey PING → PONG"

echo "smoke: ALL CHECKS PASSED"
