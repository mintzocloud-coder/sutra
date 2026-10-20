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
#   - The G2 part-2 developer-webhooks round-trip: the api is recreated
#     with SVIX_API_TOKEN minted via `svix-server jwt generate`; an
#     endpoint is registered (secret shown once, never on list), a custom
#     event is published (202), the attempt log is polled until a
#     successful 200 delivery appears, then the endpoint is deleted
#     (idempotent 204).
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
#   - The F5 logistics round-trip: the api boot log carries the fail-closed
#     logistics stub WARN (and no set-but-ignored WARN while the creds are
#     unset); the shipment:update event round-trips through
#     POST /internal/events: valid envelope → 202, missing order_id → 422,
#     string or negative cod_expected_paise → 422.
#   - The G3 part-2 mini-app round-trip: the api is recreated with
#     MINIAPP_LAUNCH_SECRET; register draft (201) -> slug race 409 ->
#     bad permission 400 invalid_manifest -> owner launches the draft
#     (preview) and the signed v1 token's HMAC is verified offline ->
#     publish -> second user's uninstalled launch 403s (launch_not_allowed)
#     -> install (201 then idempotent 200) -> launch 200 -> the directory
#     lists the app -> the shelf shows entry_url+manifest -> uninstall ->
#     suspend -> new install 422s (install_not_allowed) -> archive -> GET
#     404s -> the api is restored secretless and launch answers 503
#     launch_unavailable (fail-closed).
#   - The H3 ads-platform round-trip (reuses the F1 commerce identity):
#     campaign create (draft) -> unknown targeting key 400s
#     (invalid_targeting — closed targeting) -> draft report: not
#     eligible (not_active), full budget remaining, no pacing
#     projections without spend history, delivery absent -> activate ->
#     eligible in report -> creative register + activate -> bid set ->
#     POST /v1/ads/serve: 200 with the winning creative at the reserve
#     floor, 55/45 split on the ledger row, spend accrued in report ->
#     no-demand serve 204, missing slot 400 -> archive -> report 404s
#     (archived campaigns never leak) -> serve 204.
#   - The H4 memberships round-trip (reuses the H1 identities):
#     plan create -> 201 -> zero-price plan 400s -> list carries the
#     plan, other user's read 404s (no existence leak) -> rename 200,
#     price change 400s (immutable_field) -> subscribe fails closed
#     503 (subscriptions_unavailable) + Retry-After -> empty
#     subscriptions + entitlements -> unknown cancel 404, bad id 400 ->
#     webhook ingress 503 -> deactivate (no hard delete) -> the api
#     boot log carries the subscriptions fail-closed WARN.
#   - The I1 E2E device-key round-trip: openssl-minted bundle -> Go
#     register (201; cross-key-signed bundle 422s e2e_invalid_bundle)
#     -> directory -> REST consume id 1 -> websocket user:<subject>
#     join -> e2e:prekey_fetch consumes id 2 (chat side) -> REST
#     consume id 3 -> drained-queue fetch -> one_time_prekey null
#     (signed-prekey-only X3DH fallback) -> not_found /
#     invalid_request negatives -> device deleted.
#   - The I2 X3DH session-init transport round-trip: join
#     room:smoke-e2e-room -> push e2e:session_init (openssl-minted
#     envelope) -> e2e:session_init broadcast (canonical, kind
#     e2e_session_init) + sender-only msg:ack (deduped false) ->
#     retry -> deduped:true ack with the original id -> bad key size /
#     missing recipient -> error replies -> reconnect + rejoin ->
#     the init replays from history (offline-recipient path).
#   - The I3 e2e message envelope transport round-trip: after the I2
#     init flips the room's E2E flag -> message:new + e2e_data
#     (openssl-minted ratchet key + random ciphertext) -> message:new
#     broadcast (kind e2e_message, canonical ciphertext-only envelope) +
#     sender-only msg:ack (deduped false) -> retry -> deduped:true with
#     the original id -> mixed body+e2e_data / bad key size / empty
#     ciphertext / missing recipient / missing client_msg_id -> error
#     replies -> reconnect + rejoin -> the e2e message replays from
#     history -> join reports e2e_enabled:true -> flag GET 200/404/400.
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
# H3: ad serving seam. The publisher endpoint (POST /v1/ads/serve) is
# fail-closed by default (503 serving_disabled); smoke enables it so
# the serving round-trip below exercises the real decision path →
# billing → ad_events ledger.
export ADS_SERVING_ENABLED=1
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
  # On identity-plane timeouts, dump the kratos/hydra container logs first:
  # a kratos that exits on config validation (smoke red 2026-10-19, triage
  # #5: hashers.argon2.memory must be "64MB", cipher exactly 32 chars)
  # leaves no trace in the probe loop otherwise.
  if [[ "$desc" == kratos* ]] || [[ "$desc" == hydra* ]]; then
    echo "smoke: identity-plane probe '$desc' timed out — container logs:" >&2
    docker compose logs --tail=60 kratos hydra 2>/dev/null || true
    docker compose ps kratos hydra 2>/dev/null || true
  fi
  fail "timed out waiting for $desc"
}

echo "smoke: building service images"
docker compose build --quiet api chat transcode-worker caption-worker ai 2>/dev/null \
  || docker compose build api chat transcode-worker caption-worker ai

echo "smoke: starting postgres, valkey, seaweedfs, nats"
docker compose up -d postgres valkey seaweedfs nats
# NATS (F6): the api's order event bus is fail-closed — with NATS_URL set
# in compose (the default), the api REFUSES TO BOOT when NATS is
# unreachable, so the bus must be up before the api. JetStream is enabled
# (-js); the monitoring port 8222 proves readiness.
wait_for 60 "nats /varz" curl -sf http://localhost:8222/varz
pass "nats up (JetStream enabled, monitoring on 8222)"
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
# A failed migrate one-shot (kratos-migrate/hydra-migrate) makes `up`
# exit non-zero with no useful output — dump the one-shots' logs first
# so the failure is diagnosable from the CI log alone (smoke red
# 2026-10-19: kratos-migrate exited 1 with zero visible output).
if ! docker compose up -d hydra kratos; then
  echo "smoke: identity plane failed to start — one-shot logs:"
  docker compose logs kratos-migrate hydra-migrate 2>/dev/null || true
  exit 1
fi
wait_for 120 "hydra openid-configuration" \
  curl -sf http://localhost:4444/.well-known/openid-configuration
# In Ory Kratos, /health/alive is served by the ADMIN API (4434), never the
# public API (4433) — probing 4433 always 404s and wedges the wait.
# (Fixed 2026-10-19: the 4433 probe timed out smoke CI since triage #3.)
wait_for 120 "kratos /health/alive" \
  curl -sf http://localhost:4434/health/alive
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
# The log is captured to a file FIRST: with `set -o pipefail`, a nonzero
# `docker compose logs` exit would otherwise masquerade as a missing WARN
# (smoke red 2026-10-19: the binary provably emits the WARN — it is logged
# before the HTTP server starts, and /v1/me returned 200 — so the failure
# was in log retrieval, not the seam).
api_boot_log="$(mktemp)"
docker compose logs --no-log-prefix api >"$api_boot_log" 2>/dev/null \
  || fail "could not read the api boot log (docker compose logs api failed)"
[ -s "$api_boot_log" ] || fail "api boot log is empty"
grep -q "payments: running with the fail-closed stub" "$api_boot_log" \
  || { echo "smoke: api boot log tail (payments WARN not found):" >&2; tail -30 "$api_boot_log" >&2; fail "api boot log missing the fail-closed payments WARN"; }
pass "api boot log carries the fail-closed payments WARN"
grep -q "payments: CASHFREE_" "$api_boot_log" \
  && fail "api boot log claims CASHFREE_* set-but-ignored (creds are unset in smoke)"
pass "api boot log has no set-but-ignored CASHFREE_* WARN"

# --- Fail-closed logistics seam (F5) -----------------------------------------
# Compose pins LOGISTICS_PROVIDER=stub with empty LOGISTICS_API_KEY /
# LOGISTICS_SECRET, so the api must boot with the loud "fail-closed stub"
# WARN — and must NOT log the set-but-ignored WARN (the creds are unset
# here). Assert on the boot log (same captured file as the F4 check above).
grep -q "logistics: running with the fail-closed stub" "$api_boot_log" \
  || { echo "smoke: api boot log tail (logistics WARN not found):" >&2; tail -30 "$api_boot_log" >&2; fail "api boot log missing the fail-closed logistics WARN"; }
pass "api boot log carries the fail-closed logistics WARN"
grep -q "LOGISTICS_\* credentials are set" "$api_boot_log" \
  && fail "api boot log claims LOGISTICS_* set-but-ignored (creds are unset in smoke)"
pass "api boot log has no set-but-ignored LOGISTICS_* WARN"
rm -f "$api_boot_log"

# --- Real-issuer identity loop (D1) -----------------------------------------
# The checks above ran with AUTH_BYPASS=1, which skips JWT verification
# entirely. Now prove the real path: a Hydra-issued RS256 JWT verified
# through the A2 JWKS flow reaches GET /v1/me.
echo "smoke: creating hydra oauth2 client 'sutra-smoke'"
SMOKE_HYDRA_CLIENT_SECRET="${SMOKE_HYDRA_CLIENT_SECRET:-$(openssl rand -hex 24)}"
# The per-client access_token_strategy "jwt" is what makes Hydra v2 mint JWT
# (not opaque) access tokens. The Admin REST API is used instead of
# `hydra create client`: the CLI's strategy flag name changed across Hydra
# versions, while the JSON field is stable per the swagger model. Tolerate
# "already exists" for SMOKE_KEEP_UP=1 re-runs (recreated so a stale opaque
# client can never survive a re-run).
client_body=$(cat <<EOF
{"client_id":"sutra-smoke","client_secret":"$SMOKE_HYDRA_CLIENT_SECRET",\
"grant_types":["client_credentials","urn:ietf:params:oauth:grant-type:jwt-bearer"],\
"response_types":["token"],"scope":"openid","audience":["sutra-api"],\
"token_endpoint_auth_method":"client_secret_basic","access_token_strategy":"jwt"}
EOF
)
create_client() {
  curl -sf -X POST http://localhost:4445/clients \
    -H 'Content-Type: application/json' \
    -d "$client_body" >/tmp/sutra-hydra-client.out 2>&1
}
if ! create_client; then
  if grep -qiE "already exists|conflict" /tmp/sutra-hydra-client.out; then
    echo "smoke: hydra client 'sutra-smoke' already exists — recreating with jwt strategy"
    curl -sf -X DELETE "http://localhost:4445/clients/sutra-smoke" >/dev/null \
      && create_client \
      || { cat /tmp/sutra-hydra-client.out >&2; fail "hydra oauth2 client re-creation failed"; }
  else
    cat /tmp/sutra-hydra-client.out >&2
    fail "hydra oauth2 client creation failed"
  fi
else
  pass "hydra oauth2 client 'sutra-smoke' created"
fi

# create_jwt_client <client-id> <client-secret> — Hydra OAuth2 client with
# access_token_strategy=jwt, via the Admin REST API (host-side curl to
# localhost:4445, the same path the D1 sutra-smoke block above proves).
# The version-fragile `hydra create client` CLI is NOT used: its strategy
# flag name changed across Hydra versions, and Hydra v2 mints OPAQUE tokens
# when no strategy is set — which the Go API's JWKS verifier rejects
# (triage #8: "hydra returned an opaque token, not a JWT"). On
# "already exists" (SMOKE_KEEP_UP=1 re-runs) the client is deleted and
# recreated, so a stale opaque-strategy client can never survive a re-run.
create_jwt_client() {
  local cid="$1" csecret="$2" out
  out="$(mktemp)"
  local body
  body=$(cat <<EOF
{"client_id":"$cid","client_secret":"$csecret",\
"grant_types":["client_credentials","urn:ietf:params:oauth:grant-type:jwt-bearer"],\
"response_types":["token"],"scope":"openid","audience":["sutra-api"],\
"token_endpoint_auth_method":"client_secret_basic","access_token_strategy":"jwt"}
EOF
)
  if curl -sf -X POST http://localhost:4445/clients \
      -H 'Content-Type: application/json' \
      -d "$body" >"$out" 2>&1; then
    rm -f "$out"
    pass "hydra oauth2 client '$cid' created (jwt strategy)"
    return 0
  fi
  if grep -qiE "already exists|conflict" "$out"; then
    echo "smoke: hydra client '$cid' already exists — recreating with jwt strategy"
    if curl -sf -X DELETE "http://localhost:4445/clients/$cid" >/dev/null \
      && curl -sf -X POST http://localhost:4445/clients \
          -H 'Content-Type: application/json' \
          -d "$body" >"$out" 2>&1; then
      rm -f "$out"
      pass "hydra oauth2 client '$cid' recreated (jwt strategy)"
      return 0
    fi
  fi
  cat "$out" >&2; rm -f "$out"
  fail "hydra oauth2 client '$cid' creation failed"
}

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

# --- Live media round-trip (G1) -------------------------------------------
# LiveKit's dev keypair is baked into docker-compose.yml defaults, so the
# Go live seam is enabled in smoke. Exercises: room open, host token
# mint (canPublish), viewer token mint (subscribe-only, no publish),
# on-air listing, room end (410 on post-end mint). The SFU itself is NOT
# exercised — media bytes never touch the API; the webhook ingress is
# covered below (G1 part 2): signed webhooks are verified, room_finished
# flips the registry, retries are idempotent.
echo "smoke: live room round-trip (G1)"
room_json="$(curl -sf -H "Authorization: Bearer $access_token" \
  -H "Content-Type: application/json" \
  -d '{"title":"smoke live","kind":"livestream"}' \
  "$API_URL/v1/live/rooms")" || fail "POST /v1/live/rooms did not return 201"
room_id="$(printf '%s' "$room_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
room_name="$(printf '%s' "$room_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["livekit_room_name"])')"
[ "$room_name" = "sutra:$room_id" ] \
  || fail "livekit_room_name '$room_name' != sutra:<room_id> — wire name must derive from the row id"
pass "live room opened (id $room_id, wire name $room_name)"

mint_token() { # role -> token JSON
  curl -sf -H "Authorization: Bearer $access_token" \
    -H "Content-Type: application/json" \
    -d "{\"room_id\":\"$room_id\",\"role\":\"$1\"}" \
    "$API_URL/v1/live/token"
}
host_tok="$(mint_token host | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')"
case "$host_tok" in
  *.*.*) ;;
  *) fail "host token is not a 3-part JWT" ;;
esac
host_grants="$(printf '%s' "$host_tok" | python3 -c '
import json,sys,base64
body = sys.stdin.read().strip().split(".")[1]
body += "=" * (-len(body) % 4)
c = json.loads(base64.urlsafe_b64decode(body))
print(json.dumps(c["video"]))')"
echo "$host_grants" | python3 -c 'import json,sys; v=json.load(sys.stdin); assert v["roomJoin"] and v["canPublish"], v' \
  || fail "host token missing roomJoin/canPublish: $host_grants"
[ "$(echo "$host_grants" | python3 -c 'import json,sys; print(json.load(sys.stdin)["room"])')" = "$room_name" ] \
  && pass "host token minted (room-scoped to $room_name, canPublish)"

viewer_tok="$(mint_token viewer | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')"
printf '%s' "$viewer_tok" | python3 -c '
import json,sys,base64
body = sys.stdin.read().strip().split(".")[1]
body += "=" * (-len(body) % 4)
v = json.loads(base64.urlsafe_b64decode(body))["video"]
assert v["roomJoin"] and v["canSubscribe"] and not v["canPublish"], v' \
  || fail "viewer token grants are wrong (must be subscribe-only, no publish)"
pass "viewer token minted (subscribe-only, no canPublish)"

[ "$(curl -sf -H "Authorization: Bearer $access_token" "$API_URL/v1/live/rooms" \
  | python3 -c 'import json,sys; print(sum(1 for r in json.load(sys.stdin) if r["status"]=="live"))')" -ge 1 ] \
  || fail "GET /v1/live/rooms did not list the smoke room"
pass "on-air listing shows the smoke room"

[ "$(curl -sf -X POST -H "Authorization: Bearer $access_token" \
  "$API_URL/v1/live/rooms/$room_id/end" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')" = "ended" ] \
  || fail "POST /v1/live/rooms/{id}/end did not flip status to ended"
pass "live room ended"

[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $access_token" \
  -H "Content-Type: application/json" \
  -d "{\"room_id\":\"$room_id\",\"role\":\"viewer\"}" \
  "$API_URL/v1/live/token")" = "410" ] \
  || fail "token mint for an ended room did not 410"
pass "token mint for ended room 410s"

# --- LiveKit webhook ingress (G1 part 2) ------------------------------------
# Signs webhooks the way livekit-server does (HS256, sha256 body bind)
# and exercises: forged signatures 401, room_finished flipping the
# room to ended, duplicate delivery being a no-op, ignored events
# acking 200, and foreign room names being ignored.
echo "smoke: livekit webhook ingress (G1 part 2)"
LK_KEY="${LIVEKIT_API_KEY:-sutra-dev-livekey}"
LK_SECRET="${LIVEKIT_API_SECRET:-sutra-dev-live-secret-change-me-in-production}"
cat > /tmp/livekit_webhook_smoke.py <<'PY'
import sys, json, hmac, hashlib, base64, time, urllib.request, urllib.error
url, key, secret, event, room_name = sys.argv[1:6]
body = json.dumps({"event": event, "id": "wh_smoke", "room": {"sid": "RM_x", "name": room_name}}).encode()
s = hashlib.sha256(body).digest()
claims = {"iss": key, "sub": key, "nbf": int(time.time()) - 60,
          "exp": int(time.time()) + 300, "sha256": base64.b64encode(s).decode()}
hdr = base64.urlsafe_b64encode(b'{"alg":"HS256","typ":"JWT"}').rstrip(b"=").decode()
pl = base64.urlsafe_b64encode(json.dumps(claims).encode()).rstrip(b"=").decode()
sig = base64.urlsafe_b64encode(hmac.new(secret.encode(), (hdr + "." + pl).encode(), hashlib.sha256).digest()).rstrip(b"=").decode()
req = urllib.request.Request(url, data=body, headers={
    "Content-Type": "application/json", "Authorization": key + " " + hdr + "." + pl + "." + sig})
try:
    resp = urllib.request.urlopen(req)
    print(resp.status)
    print(resp.read().decode())
except urllib.error.HTTPError as e:
    print(e.code)
    print(e.read().decode())
PY
post_webhook() { # event, room_name, secret -> "status|body"
  python3 /tmp/livekit_webhook_smoke.py "$API_URL/internal/livekit/webhook" "$LK_KEY" "$3" "$1" "$2" \
    | python3 -c 'import sys; parts = sys.stdin.read().splitlines(); print(parts[0] + "|" + (parts[1] if len(parts) > 1 else ""))'
}
wh_room_json="$(curl -sf -H "Authorization: Bearer $access_token" \
  -H "Content-Type: application/json" \
  -d '{"title":"smoke webhook","kind":"livestream"}' \
  "$API_URL/v1/live/rooms")" || fail "POST /v1/live/rooms (webhook room) did not return 201"
wh_room_id="$(printf '%s' "$wh_room_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
wh_room_name="$(printf '%s' "$wh_room_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["livekit_room_name"])')"

[ "$(post_webhook room_finished "$wh_room_name" wrong-secret | cut -d'|' -f1)" = "401" ] \
  || fail "webhook with forged signature did not 401"
pass "webhook rejects forged signature (401)"

res="$(post_webhook room_finished "$wh_room_name" "$LK_SECRET")"
[ "$(printf '%s' "$res" | cut -d'|' -f1)" = "200" ] \
  && printf '%s' "$res" | python3 -c 'import json,sys; b=json.loads(sys.stdin.read().split("|",1)[1]); assert b["handled"] and b["flipped"] and b["room_id"], b' \
  || fail "room_finished webhook did not flip the room: $res"
pass "room_finished webhook flipped the room to ended"

[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $access_token" \
  -H "Content-Type: application/json" \
  -d "{\"room_id\":\"$wh_room_id\",\"role\":\"viewer\"}" \
  "$API_URL/v1/live/token")" = "410" ] \
  || fail "token mint for webhook-ended room did not 410"
pass "webhook-ended room refuses new tokens (410)"

res="$(post_webhook room_finished "$wh_room_name" "$LK_SECRET")"
[ "$(printf '%s' "$res" | cut -d'|' -f1)" = "200" ] \
  && printf '%s' "$res" | python3 -c 'import json,sys; b=json.loads(sys.stdin.read().split("|",1)[1]); assert b["handled"] and not b["flipped"], b' \
  || fail "duplicate room_finished was not an idempotent no-op: $res"
pass "duplicate room_finished is an idempotent no-op"

res="$(post_webhook participant_joined "$wh_room_name" "$LK_SECRET")"
[ "$(printf '%s' "$res" | cut -d'|' -f1)" = "200" ] \
  && printf '%s' "$res" | python3 -c 'import json,sys; b=json.loads(sys.stdin.read().split("|",1)[1]); assert not b["handled"], b' \
  || fail "non-finish event was not acked-and-ignored: $res"
pass "non-finish events are acknowledged, ignored"

res="$(post_webhook room_finished someone-elses-room "$LK_SECRET")"
[ "$(printf '%s' "$res" | cut -d'|' -f1)" = "200" ] \
  && printf '%s' "$res" | python3 -c 'import json,sys; b=json.loads(sys.stdin.read().split("|",1)[1]); assert not b["handled"] and b["reason"]=="unknown_room", b' \
  || fail "room_finished for a foreign room was not ignored: $res"
pass "room_finished for a foreign room is ignored"

# --- Developer webhooks round-trip (G2 part 2) --------------------------------
# Boots the Svix server, mints the management token inside its container,
# recreates the api with SVIX_API_TOKEN (the dispatcher enabled), then:
# register an endpoint → publish an event → the attempt log shows a
# successful delivery → delete the endpoint (idempotent). The delivery
# canary is the api's own /healthz (tolerates any method, always 200);
# the dev compose whitelists the docker subnets for Svix's SSRF guard so
# this compose-internal delivery is allowed (production must NOT carry
# that whitelist — see docs/webhooks.md and docs/production.md).
echo "smoke: developer webhooks (G2 part 2)"
docker compose up -d svix-server
wait_for 90 "svix-server (jwt generate)" \
  docker compose exec -T svix-server svix-server jwt generate
# A JWT is three base64url segments — extract it regardless of any
# surrounding human-readable output from the generate command.
SVIX_TOKEN="$(docker compose exec -T svix-server svix-server jwt generate \
  | tr -d '\r' | grep -oE '[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+' | head -1)"
[ -n "$SVIX_TOKEN" ] || fail "svix-server jwt generate produced no JWT"
# Triage #15 (2026-10-19): `svix-server jwt generate` is an OFFLINE CLI — it
# succeeds long before the server's HTTP listener is up (migrations run at
# boot against postgres). Without this wait the round-trip below races
# container boot and the Go api's first Svix call fails at TCP connect
# (502 webhooks_upstream "webhook gateway is unreachable"). Probe the real
# management API (needs the DB, so a 200 means the server is truly ready).
wait_for 90 "svix-server management API (localhost:8071)" \
  curl -sf -o /dev/null -H "Authorization: Bearer $SVIX_TOKEN" \
  http://localhost:8071/api/v1/app
pass "svix-server up (management API answering, 8071)"
# Recreate the api with the dispatcher enabled. Triage #18 (2026-10-19):
# AUTH_BYPASS MUST stay off here (explicit AUTH_BYPASS= prefix): the
# exported AUTH_BYPASS=1 is still in the environment, and letting this
# recreate inherit it silently re-enabled the bypass — which is exactly
# what made E1's "/v1/ai/healthz without token did not 401" check fail
# (the bypass stamps dev-user, so every no-token request 200s instead of
# 401ing). The webhooks round-trip uses the hydra JWT ($access_token),
# so bypass-off exercises the real auth path — strictly stronger than
# the bypass for route-wiring purposes.
# Triage #19 (2026-10-19): the python round-trip below previously sent a
# hard-coded bogus Bearer value — it only ever passed because the bypass
# (triage #18's leak) stamped dev-user and the bad token was never
# checked. With bypass-off the api 401s it (run 39258431914). It now
# takes the real hydra JWT ($access_token) as argv[2], matching the
# comment above and the F2-search python heredoc pattern.
AUTH_BYPASS= SVIX_API_TOKEN="$SVIX_TOKEN" docker compose up -d --no-deps api
wait_for 90 "api /healthz (webhooks enabled)" curl -sf "$API_URL/healthz"
# Triage #16 (2026-10-19): the 2j run proved the host-side probe is NOT
# sufficient — it passed, yet the Go api's first Svix call still failed
# at TCP connect (502 webhooks_upstream "webhook gateway is
# unreachable") ~2s later. The host reaches svix-server through the
# published port; the Go client reaches it through the compose-internal
# name (http://svix-server:8071) — a different network path that can
# fail independently (fresh-container DNS/NAT races, or the svix
# listener flapping after the probe). Gate on the EXACT path the Go
# client uses: wget from INSIDE the api container (alpine ships wget;
# the HEALTHCHECK already relies on it).
wait_for 90 "svix-server reachable from inside the api container" \
  docker compose exec -T api wget -q -O /dev/null \
  --header="Authorization: Bearer $SVIX_TOKEN" \
  http://svix-server:8071/api/v1/app
pass "svix-server reachable from the api container (Go client's path)"
# Triage #16: if the round-trip still fails, dump the evidence the next
# triage needs — container state, the svix-server log tail, and both
# network paths re-probed — instead of the bare 502.
dump_webhooks_diag() {
  echo "smoke: webhooks round-trip failed — diagnostics:" >&2
  docker compose ps svix-server api >&2 || true
  echo "--- svix-server log tail:" >&2
  docker compose logs --tail=80 svix-server >&2 || true
  echo "--- api log tail (Triage #17: the Go api logs the exact Svix" >&2
  echo "--- error — 'webhooks: svix unreachable' with op/err — without" >&2
  echo "--- this, a 502 looks like a network problem when it isn't):" >&2
  docker compose logs --tail=60 api >&2 || true
  echo "--- host re-probe (localhost:8071):" >&2
  curl -s -o /dev/null -w 'host probe http_code=%{http_code}\n' \
    -H "Authorization: Bearer $SVIX_TOKEN" \
    http://localhost:8071/api/v1/app >&2 || true
  echo "--- container-path re-probe (api -> svix-server:8071):" >&2
  if docker compose exec -T api wget -q -O /dev/null \
    --header="Authorization: Bearer $SVIX_TOKEN" \
    http://svix-server:8071/api/v1/app >&2; then
    echo "container-path probe: OK" >&2
  else
    echo "container-path probe: FAIL" >&2
  fi
}
python3 - "$API_URL" "$access_token" <<'PYEOF' || { dump_webhooks_diag; fail "webhooks round-trip"; }
import json, sys, time, urllib.request, urllib.error
api = sys.argv[1]
hydra_jwt = sys.argv[2]  # the hydra JWT from the D1 token request
HDRS = {"Authorization": "Bearer " + hydra_jwt, "Content-Type": "application/json"}
def call(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(api + path, data=data, headers=HDRS, method=method)
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            return r.status, json.loads(r.read() or b"null")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"null")
# 1. register the endpoint (canary: the api's own /healthz)
st, ep = call("POST", "/v1/webhooks/endpoints",
              {"url": "http://api:8080/healthz", "description": "smoke canary",
               "filter_types": ["smoke.ping"]})
assert st == 201, (st, ep)
assert ep.get("secret", "").startswith("whsec_"), ep  # secret shown once, at creation
ep_id = ep["id"]
# 2. list shows it — and never the secret
st, lst = call("GET", "/v1/webhooks/endpoints")
assert st == 200, (st, lst)
mine = [e for e in lst["endpoints"] if e["id"] == ep_id]
assert len(mine) == 1, lst
assert not mine[0].get("secret"), lst
# 3. publish a custom event
st, msg = call("POST", "/v1/webhooks/events",
               {"event_type": "smoke.ping", "payload": {"n": 1}})
assert st == 202, (st, msg)
# 4. the attempt log shows a successful delivery (async — poll)
deadline = time.time() + 90
attempts = []
while True:
    st, log = call("GET", "/v1/webhooks/deliveries?message_id=" + msg["id"])
    assert st == 200, (st, log)
    attempts = log["attempts"]
    if any(a["status"] == "success" and a["responseStatusCode"] == 200 for a in attempts):
        break
    assert time.time() < deadline, "no successful delivery: %s" % json.dumps(attempts[:5])
    time.sleep(3)
# 5. delete the endpoint — and again (idempotent 204)
st, _ = call("DELETE", "/v1/webhooks/endpoints/" + ep_id)
assert st == 204, st
st, _ = call("DELETE", "/v1/webhooks/endpoints/" + ep_id)
assert st == 204, st
print("webhooks: endpoint %s registered, message %s delivered (200), endpoint removed"
      % (ep_id, msg["id"]))
PYEOF
pass "webhooks: register → publish → attempt log shows the delivery"

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
# The stub streams in 24-char chunks (services/ai/app/model.py), so the
# marker "AI_MODEL_STUB=1" is split across two TEXT_MESSAGE_CONTENT
# events ("AI_M" | "ODEL_STUB=1,") — a line-based grep can never match
# it. Reassemble the deltas exactly like a real AG-UI client does, then
# assert on the full text.
printf '%s' "$chat_out" | python3 -c '
import json, sys
text = ""
for line in sys.stdin:
    line = line.strip()
    if not line.startswith("data:"):
        continue
    try:
        ev = json.loads(line[5:])
    except Exception:
        continue
    if ev.get("type") == "TEXT_MESSAGE_CONTENT":
        text += ev.get("delta", "")
assert "AI_MODEL_STUB=1" in text, "reassembled text missing marker: %r" % text[:120]
' || fail "chat stream missing the stub-model marker (reassembled deltas)"
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
# INVARIANT (triage #18, 2026-10-19): every later
# `docker compose up -d --no-deps api` MUST carry an explicit AUTH_BYPASS=
# prefix — AUTH_BYPASS=1 is exported at the top of this script, and any
# recreate that inherits it silently re-enables the bypass, breaking all
# later no-token 401 checks (E1 proved it).

# --- Business AI copilot round-trip (H1) ---------------------------------------
# POST /v1/ai/business (proxied to the ai service's /business) answers
# catalog Q&A + order insights for the caller's OWN store, with the "never
# invent attributes" guardrail verified end to end over the wire. The stub
# model stands in for sarvam-30B (CI has no GPU/model to pull); it ECHOES
# the user's question, which proves the guard in both directions:
#   - a question asserting the TRUE price (₹1,299) streams it back unchanged
#     (the guard found it in the injected grounding block);
#   - a question asserting a WRONG price (₹999) gets the safe fallback —
#     the fabrication never reaches the client.
# Non-sellers are refused with 403 not_a_seller before anything runs, and
# the per-user rpm guard rejects with a JSON 429 (never a truncated SSE).
# NOTE: SSE payloads are json.dumps-encoded, so ₹ appears as \u20b9.
echo "smoke: business AI copilot round-trip (H1)"
AI_MODEL_STUB=1 docker compose up -d --no-deps ai
wait_for 120 "ai /healthz (stub model, H1)" curl -sf http://localhost:8000/healthz

biz_client="sutra-smoke-h-$(date +%s)"
echo "smoke: creating hydra oauth2 client '$biz_client'"
SMOKE_HYDRA_CLIENT_H_SECRET="$(openssl rand -hex 32)"
create_jwt_client "$biz_client" "$SMOKE_HYDRA_CLIENT_H_SECRET"

token_h_json="$(curl -sf -u "$biz_client:$SMOKE_HYDRA_CLIENT_H_SECRET" \
  --data-urlencode 'grant_type=client_credentials' \
  --data-urlencode 'audience=sutra-api' \
  --data-urlencode 'scope=openid' \
  http://localhost:4444/oauth2/token)" || fail "hydra token request for '$biz_client' failed"
token_h="$(printf '%s' "$token_h_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')"
[ -n "$token_h" ] || fail "hydra token for '$biz_client' was empty"

# Register a seller and seed the grounding facts: one active product, one
# active priced variant (SKU SMOKE-H-RED-M, price ₹1,299, stock 7) — the
# single source of truth the guard validates every reply against.
biz_slug="smoke-store-h-$(date +%s)"
curl -sf -X POST -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d "{\"store_name\":\"Smoke Biz Store\",\"store_slug\":\"$biz_slug\"}" \
  "$API_URL/v1/sellers" >/dev/null || fail "H1: POST /v1/sellers failed"
prod_h="$(curl -sf -X POST -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"title":"Smoke Business Kurta","description":"red handloom kurta","category_slug":"mens-fashion"}' \
  "$API_URL/v1/products")" || fail "H1: POST /v1/products failed"
prod_h_id="$(printf '%s' "$prod_h" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
var_h="$(curl -sf -X POST -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"sku":"SMOKE-H-RED-M","price_paise":129900,"mrp_paise":199900,"attributes":{"color":"red","size":"M"}}' \
  "$API_URL/v1/products/$prod_h_id/variants")" || fail "H1: POST variant failed"
var_h_id="$(printf '%s' "$var_h" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
curl -sf -X POST -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"delta":7}' "$API_URL/v1/variants/$var_h_id/inventory/adjust" >/dev/null \
  || fail "H1: inventory adjust failed"
curl -sf -X PATCH -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"status":"active"}' "$API_URL/v1/products/$prod_h_id" >/dev/null \
  || fail "H1: product activate failed"
pass "H1: seller + active product (SKU SMOKE-H-RED-M, price ₹1,299, stock 7)"

# Grounded question: the true price is in the grounding block, so the stub
# echo passes the guard and streams the real price back.
biz_out="$(curl -sf -N -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"is the red kurta price ₹1,299?"}]}' \
  "$API_URL/v1/ai/business")" || fail "H1: POST /v1/ai/business failed"
printf '%s' "$biz_out" | grep -q '"type":"RUN_STARTED"' \
  || fail "H1: business stream missing RUN_STARTED"
printf '%s' "$biz_out" | grep -q '"type":"TEXT_MESSAGE_START"' \
  || fail "H1: business stream missing TEXT_MESSAGE_START"
printf '%s' "$biz_out" | grep -q '\\u20b91,299' \
  || fail "H1: grounded question did not stream the true price (₹1,299)"
if printf '%s' "$biz_out" | grep -q "couldn't verify"; then
  fail "H1: grounded question wrongly triggered the fallback"
fi
printf '%s' "$biz_out" | grep -q '"type":"RUN_FINISHED"' \
  || fail "H1: business stream missing RUN_FINISHED"
pass "H1: grounded question streams the true price (₹1,299), no fallback"

# Fabricated question: the stub echoes ₹999, the guard must catch it and
# swap in the safe fallback — the wrong price must never reach the wire.
biz_bad="$(curl -sf -N -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"is the red kurta price ₹999?"}]}' \
  "$API_URL/v1/ai/business")" || fail "H1: fabricated-price call failed"
printf '%s' "$biz_bad" | grep -q "couldn't verify that detail" \
  || fail "H1: fabricated price did not get the safe fallback"
if printf '%s' "$biz_bad" | grep -q '\\u20b9999'; then
  fail "H1: fabricated price (₹999) leaked into the stream"
fi
printf '%s' "$biz_bad" | grep -q '"type":"RUN_FINISHED"' \
  || fail "H1: fabricated-price stream missing RUN_FINISHED"
pass "H1: fabricated price (₹999) blocked — safe fallback streamed, nothing leaked"

# Non-seller 403: a fresh Hydra identity with no seller store is refused
# before any guard, graph, or model call.
biz2_client="sutra-smoke-h2-$(date +%s)"
SMOKE_HYDRA_CLIENT_H2_SECRET="$(openssl rand -hex 32)"
create_jwt_client "$biz2_client" "$SMOKE_HYDRA_CLIENT_H2_SECRET"
token_h2="$(curl -sf -u "$biz2_client:$SMOKE_HYDRA_CLIENT_H2_SECRET" \
  --data-urlencode 'grant_type=client_credentials' \
  --data-urlencode 'audience=sutra-api' \
  --data-urlencode 'scope=openid' \
  http://localhost:4444/oauth2/token | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')" \
  || fail "hydra token request for '$biz2_client' failed"
[ -n "$token_h2" ] || fail "hydra token for '$biz2_client' was empty"
biz_body="$(mktemp)"
biz_code="$(curl -s -o "$biz_body" -w '%{http_code}' -N \
  -H "Authorization: Bearer $token_h2" -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"what is my catalog?"}]}' \
  "$API_URL/v1/ai/business")"
[ "$biz_code" = "403" ] || { rm -f "$biz_body"; fail "H1: non-seller did not get 403 (got $biz_code)"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$biz_body"'"))["error"]["code"])')" = "not_a_seller" ] \
  || { rm -f "$biz_body"; fail "H1: non-seller envelope code is not not_a_seller"; }
rm -f "$biz_body"
pass "H1: non-seller → 403 not_a_seller (eager, before any data access)"

# Per-user rate-limit 429 path (AI_BIZ_RPM=1; loop until a 429 to survive a
# minute-boundary rollover, same pattern as the E2 chat guard).
echo "smoke: recreating ai with AI_BIZ_RPM=1"
AI_MODEL_STUB=1 AI_BIZ_RPM=1 docker compose up -d --no-deps ai
wait_for 120 "ai /healthz (biz rpm=1)" curl -sf http://localhost:8000/healthz
biz429_body="$(mktemp)"
biz429_hdr="$(mktemp)"
biz429_code="200"
for _ in 1 2 3 4; do
  biz429_code="$(curl -s -o "$biz429_body" -D "$biz429_hdr" -w '%{http_code}' -N \
    -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
    -d '{"messages":[{"role":"user","content":"biz rate ping"}]}' \
    "$API_URL/v1/ai/business" || true)"
  [ "$biz429_code" = "429" ] && break
done
[ "$biz429_code" = "429" ] || { rm -f "$biz429_body" "$biz429_hdr"; fail "H1: business rate limit never triggered a 429 (last got $biz429_code)"; }
grep -qi '^Retry-After: [1-9]' "$biz429_hdr" \
  || { rm -f "$biz429_body" "$biz429_hdr"; fail "H1: business 429 missing Retry-After header"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$biz429_body"'"))["error"]["code"])')" = "ai_rate_limited" ] \
  || { rm -f "$biz429_body" "$biz429_hdr"; fail "H1: business 429 envelope code is not ai_rate_limited"; }
rm -f "$biz429_body" "$biz429_hdr"
pass "H1: per-user business rate limit 429 ai_rate_limited + Retry-After (JSON, not truncated SSE)"

# Restore the default ai configuration for the rest of the run.
docker compose up -d --no-deps ai
wait_for 120 "ai /healthz (defaults restored, post-H1)" curl -sf http://localhost:8000/healthz

# --- Business integrations fail-closed proof (H2) ---------------------------
# WhatsApp and Tally are post-investors seams: every call must fail
# CLOSED with an honest 503 + Retry-After (never imply delivery), and
# the webhook handshake must refuse without a configured verify token.
# Reuses the H1 identities: $token_h owns a seller store, $token_h2 owns
# nothing (non-seller).
echo "smoke: business integrations fail-closed (H2)"

# WhatsApp send: the stub fails closed → 503 whatsapp_unavailable +
# Retry-After, never a fake 202.
wa_body="$(mktemp)"; wa_hdr="$(mktemp)"
wa_code="$(curl -s -o "$wa_body" -D "$wa_hdr" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"to":"+919876543210","text":"your order has shipped"}' \
  "$API_URL/v1/integrations/whatsapp/send")"
[ "$wa_code" = "503" ] || { rm -f "$wa_body" "$wa_hdr"; fail "H2: WhatsApp send did not fail closed 503 (got $wa_code)"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$wa_body"'"))["error"]["code"])')" = "whatsapp_unavailable" ] \
  || { rm -f "$wa_body" "$wa_hdr"; fail "H2: WhatsApp send envelope code is not whatsapp_unavailable"; }
grep -qi '^Retry-After: [1-9]' "$wa_hdr" \
  || { rm -f "$wa_body" "$wa_hdr"; fail "H2: WhatsApp send 503 missing Retry-After header"; }
rm -f "$wa_body" "$wa_hdr"
pass "H2: WhatsApp send fails closed — 503 whatsapp_unavailable + Retry-After"

# WhatsApp send: no token → 401; non-seller → 403 not_a_seller.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"to":"+919876543210","text":"x"}' "$API_URL/v1/integrations/whatsapp/send")" = "401" ] \
  || fail "H2: unauthenticated WhatsApp send did not return 401"
wa_ns_body="$(mktemp)"
wa_ns_code="$(curl -s -o "$wa_ns_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_h2" -H 'Content-Type: application/json' \
  -d '{"to":"+919876543210","text":"x"}' "$API_URL/v1/integrations/whatsapp/send")"
[ "$wa_ns_code" = "403" ] || { rm -f "$wa_ns_body"; fail "H2: non-seller WhatsApp send did not return 403 (got $wa_ns_code)"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$wa_ns_body"'"))["error"]["code"])')" = "not_a_seller" ] \
  || { rm -f "$wa_ns_body"; fail "H2: non-seller WhatsApp envelope code is not not_a_seller"; }
rm -f "$wa_ns_body"
pass "H2: WhatsApp send — 401 without token, 403 not_a_seller for non-seller"

# Webhook hub-challenge: the verify token is unset in the default stack
# (compose passthrough expands to empty), so the handshake must fail
# closed 401 rather than echo.
[ "$(curl -s -o /dev/null -w '%{http_code}' \
  "$API_URL/internal/whatsapp/webhook?hub.mode=subscribe&hub.verify_token=anything&hub.challenge=abc")" = "401" ] \
  || fail "H2: webhook handshake with unset secret did not fail closed 401"

# Webhook hub-challenge: recreate the api with the verify token — the
# handshake must echo the challenge verbatim; a wrong token → 403.
echo "smoke: recreating api with WHATSAPP_WEBHOOK_VERIFY_TOKEN"
WHATSAPP_WEBHOOK_VERIFY_TOKEN=smoke-hook-secret AUTH_BYPASS= docker compose up -d --no-deps api
wait_for 120 "api /healthz (webhook secret set)" curl -sf "$API_URL/healthz"
challenge="$(curl -sf "$API_URL/internal/whatsapp/webhook?hub.mode=subscribe&hub.verify_token=smoke-hook-secret&hub.challenge=ch123")" \
  || fail "H2: webhook handshake failed with the correct verify token"
[ "$challenge" = "ch123" ] || fail "H2: webhook handshake did not echo the challenge verbatim"
[ "$(curl -s -o /dev/null -w '%{http_code}' \
  "$API_URL/internal/whatsapp/webhook?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=ch123")" = "403" ] \
  || fail "H2: webhook handshake with the wrong token did not return 403"
pass "H2: webhook handshake — 401 fail-closed unset, 200 verbatim echo with token, 403 wrong token"

# Webhook POST: the signature check is real HMAC — a forged signature
# must be refused 401 before any body is parsed.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'X-Hub-Signature-256: sha256=deadbeef' \
  -H 'Content-Type: application/json' -d '{"object":"whatsapp_business_account"}' \
  "$API_URL/internal/whatsapp/webhook")" = "401" ] \
  || fail "H2: webhook POST with forged signature did not return 401"
pass "H2: webhook POST with forged X-Hub-Signature-256 → 401"

# Restore the default (secretless) api configuration for the rest of the
# run — the secretless recreate re-proves the fail-closed contract.
# (triage #18 invariant: explicit AUTH_BYPASS= so the exported
# AUTH_BYPASS=1 cannot leak into the recreated container.)
AUTH_BYPASS= docker compose up -d --no-deps api
wait_for 120 "api /healthz (webhook secret unset, restored)" curl -sf "$API_URL/healthz"
[ "$(curl -s -o /dev/null -w '%{http_code}' \
  "$API_URL/internal/whatsapp/webhook?hub.mode=subscribe&hub.verify_token=anything&hub.challenge=abc")" = "401" ] \
  || fail "H2: webhook handshake after secretless restore did not fail closed 401"
pass "H2: secretless restore — handshake fails closed 401 again"

# Tally voucher: the seller has no bridge registered in
# seller_tally_bridges → 503 tally_not_registered + Retry-After.
tally_body="$(mktemp)"; tally_hdr="$(mktemp)"
tally_code="$(curl -s -o "$tally_body" -D "$tally_hdr" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"kind":"sales","date":"20261020","reference_no":"SMOKE-TALLY-1","lines":[{"ledger_name":"Cash","amount_paise":129900},{"ledger_name":"Sales","amount_paise":-129900}]}' \
  "$API_URL/v1/integrations/tally/vouchers")"
if [ "$tally_code" != "503" ]; then
  # Self-diagnosing failure: the 503 assertion has failed twice in CI
  # with 403 right after an api container recreate (the seller-scoped
  # WhatsApp call passes on the same token seconds earlier, and the
  # route is proven correct against a local stack). Dump everything
  # the next triage needs: the response body (envelope code), whether
  # the seller row actually exists for this subject, and the api log
  # tail — instead of a bare status code.
  echo "smoke DIAG: tally response body:"; cat "$tally_body"; echo
  echo "smoke DIAG: users/sellers rows for the H1 subject:"
  docker compose exec -T postgres psql -U sutra -d sutra -c \
    "SELECT u.subject, s.id AS seller_id, s.store_slug FROM users u LEFT JOIN sellers s ON s.user_id = u.id WHERE u.subject LIKE 'sutra-smoke-h-%';" 2>&1 | tail -8
  echo "smoke DIAG: api AUTH_BYPASS env (empty = real JWT verification; '1' means the bypass leaked and every request ran as dev-user):"
  docker compose exec -T api sh -c 'echo "AUTH_BYPASS=[$AUTH_BYPASS]"' 2>&1 | tail -2 || true
  echo "smoke DIAG: api log tail:"; docker compose logs --tail=30 api 2>&1 | tail -30
  rm -f "$tally_body" "$tally_hdr"
  fail "H2: Tally voucher did not fail closed 503 (got $tally_code)"
fi
[ "$(python3 -c 'import json; print(json.load(open("'"$tally_body"'"))["error"]["code"])')" = "tally_not_registered" ] \
  || { rm -f "$tally_body" "$tally_hdr"; fail "H2: Tally voucher envelope code is not tally_not_registered"; }
grep -qi '^Retry-After: [1-9]' "$tally_hdr" \
  || { rm -f "$tally_body" "$tally_hdr"; fail "H2: Tally voucher 503 missing Retry-After header"; }
rm -f "$tally_body" "$tally_hdr"
pass "H2: Tally voucher fails closed — 503 tally_not_registered + Retry-After"

# Tally voucher: no token → 401; non-seller → 403 not_a_seller;
# unbalanced voucher → 400 invalid_request (grammar validated before any
# bridge call).
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"kind":"sales","date":"20261020","reference_no":"R","lines":[{"ledger_name":"Cash","amount_paise":100},{"ledger_name":"Sales","amount_paise":-100}]}' \
  "$API_URL/v1/integrations/tally/vouchers")" = "401" ] \
  || fail "H2: unauthenticated Tally voucher did not return 401"
tally_ns_body="$(mktemp)"
tally_ns_code="$(curl -s -o "$tally_ns_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_h2" -H 'Content-Type: application/json' \
  -d '{"kind":"sales","date":"20261020","reference_no":"R","lines":[{"ledger_name":"Cash","amount_paise":100},{"ledger_name":"Sales","amount_paise":-100}]}' \
  "$API_URL/v1/integrations/tally/vouchers")"
[ "$tally_ns_code" = "403" ] || { rm -f "$tally_ns_body"; fail "H2: non-seller Tally voucher did not return 403 (got $tally_ns_code)"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$tally_ns_body"'"))["error"]["code"])')" = "not_a_seller" ] \
  || { rm -f "$tally_ns_body"; fail "H2: non-seller Tally envelope code is not not_a_seller"; }
rm -f "$tally_ns_body"
tally_bad_body="$(mktemp)"
tally_bad_code="$(curl -s -o "$tally_bad_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"kind":"sales","date":"20261020","reference_no":"R","lines":[{"ledger_name":"Cash","amount_paise":100},{"ledger_name":"Sales","amount_paise":-99}]}' \
  "$API_URL/v1/integrations/tally/vouchers")"
[ "$tally_bad_code" = "400" ] || { rm -f "$tally_bad_body"; fail "H2: unbalanced Tally voucher did not return 400 (got $tally_bad_code)"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$tally_bad_body"'"))["error"]["code"])')" = "invalid_request" ] \
  || { rm -f "$tally_bad_body"; fail "H2: unbalanced Tally envelope code is not invalid_request"; }
rm -f "$tally_bad_body"
pass "H2: Tally voucher — 401 without token, 403 not_a_seller, 400 invalid_request on unbalanced lines"

# Zoho contacts + invoices: the deployment wires only the fail-closed
# stub until the post-investors OAuth onboarding, so both routes must
# fail CLOSED with an honest 503 + Retry-After (never imply a contact
# was created or an invoice was booked). Seller-scoping: no token →
# 401; non-seller ($token_h2) → 403 not_a_seller; bad phone →
# 422 invalid_phone; non-INR currency → 400 invalid_request.
zoho_body="$(mktemp)"; zoho_hdr="$(mktemp)"
zoho_code="$(curl -s -o "$zoho_body" -D "$zoho_hdr" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"display_name":"Smoke Buyer","email":"smoke@example.com","phone":"+919876543210"}' \
  "$API_URL/v1/integrations/zoho/contacts")"
[ "$zoho_code" = "503" ] || { rm -f "$zoho_body" "$zoho_hdr"; fail "H2: Zoho contact did not fail closed 503 (got $zoho_code)"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$zoho_body"'"))["error"]["code"])')" = "zoho_unavailable" ] \
  || { rm -f "$zoho_body" "$zoho_hdr"; fail "H2: Zoho contact envelope code is not zoho_unavailable"; }
grep -qi '^Retry-After: [1-9]' "$zoho_hdr" \
  || { rm -f "$zoho_body" "$zoho_hdr"; fail "H2: Zoho contact 503 missing Retry-After header"; }
rm -f "$zoho_body" "$zoho_hdr"
pass "H2: Zoho contact fails closed — 503 zoho_unavailable + Retry-After"

zoho_inv_body="$(mktemp)"; zoho_inv_hdr="$(mktemp)"
zoho_inv_code="$(curl -s -o "$zoho_inv_body" -D "$zoho_inv_hdr" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"contact_id":"460000000026049","reference_no":"SMOKE-ZOHO-1","currency":"INR","due_date":"20261027","lines":[{"name":"Handloom Kurta","quantity":2,"amount_paise":129900}]}' \
  "$API_URL/v1/integrations/zoho/invoices")"
[ "$zoho_inv_code" = "503" ] || { rm -f "$zoho_inv_body" "$zoho_inv_hdr"; fail "H2: Zoho invoice did not fail closed 503 (got $zoho_inv_code)"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$zoho_inv_body"'"))["error"]["code"])')" = "zoho_unavailable" ] \
  || { rm -f "$zoho_inv_body" "$zoho_inv_hdr"; fail "H2: Zoho invoice envelope code is not zoho_unavailable"; }
grep -qi '^Retry-After: [1-9]' "$zoho_inv_hdr" \
  || { rm -f "$zoho_inv_body" "$zoho_inv_hdr"; fail "H2: Zoho invoice 503 missing Retry-After header"; }
rm -f "$zoho_inv_body" "$zoho_inv_hdr"
pass "H2: Zoho invoice fails closed — 503 zoho_unavailable + Retry-After"

[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"display_name":"X"}' "$API_URL/v1/integrations/zoho/contacts")" = "401" ] \
  || fail "H2: unauthenticated Zoho contact did not return 401"
zoho_ns_body="$(mktemp)"
zoho_ns_code="$(curl -s -o "$zoho_ns_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_h2" -H 'Content-Type: application/json' \
  -d '{"display_name":"X"}' "$API_URL/v1/integrations/zoho/contacts")"
[ "$zoho_ns_code" = "403" ] || { rm -f "$zoho_ns_body"; fail "H2: non-seller Zoho contact did not return 403 (got $zoho_ns_code)"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$zoho_ns_body"'"))["error"]["code"])')" = "not_a_seller" ] \
  || { rm -f "$zoho_ns_body"; fail "H2: non-seller Zoho envelope code is not not_a_seller"; }
rm -f "$zoho_ns_body"
zoho_phone_body="$(mktemp)"
[ "$(curl -s -o "$zoho_phone_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"display_name":"X","phone":"9876543210"}' "$API_URL/v1/integrations/zoho/contacts")" = "422" ] \
  || { rm -f "$zoho_phone_body"; fail "H2: bad-phone Zoho contact did not return 422"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$zoho_phone_body"'"))["error"]["code"])')" = "invalid_phone" ] \
  || { rm -f "$zoho_phone_body"; fail "H2: bad-phone Zoho envelope code is not invalid_phone"; }
rm -f "$zoho_phone_body"
zoho_cur_body="$(mktemp)"
[ "$(curl -s -o "$zoho_cur_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"contact_id":"c1","reference_no":"R","currency":"USD","due_date":"20261027","lines":[{"name":"W","quantity":1,"amount_paise":100}]}' \
  "$API_URL/v1/integrations/zoho/invoices")" = "400" ] \
  || { rm -f "$zoho_cur_body"; fail "H2: non-INR Zoho invoice did not return 400"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$zoho_cur_body"'"))["error"]["code"])')" = "invalid_request" ] \
  || { rm -f "$zoho_cur_body"; fail "H2: non-INR Zoho envelope code is not invalid_request"; }
rm -f "$zoho_cur_body"
pass "H2: Zoho — 401 without token, 403 not_a_seller, 422 invalid_phone, 400 on non-INR invoice"

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
create_jwt_client "$agui_client" "$SMOKE_HYDRA_CLIENT_G_SECRET"

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
create_jwt_client sutra-smoke-b "$SMOKE_HYDRA_CLIENT_B_SECRET"

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
# Unlike is 200 with the count (not 204): the OpenAPI contract documents
# idempotent "ensure absent" returning LikeResult so clients can update UI
# state without a refetch (internal/engagement/handler.go pins it too).
unlike_out="$(curl -sf -X DELETE \
  -H "Authorization: Bearer $token_b" "$API_URL/v1/posts/$rxid/like")" \
  || fail "DELETE /v1/posts/{id}/like (rank cleanup) failed"
printf '%s' "$unlike_out" | python3 -c "
import json,sys
d = json.load(sys.stdin)
assert d['liked'] is False, d
assert d['like_count'] == 0, d
" || fail "unlike did not return the 200 liked:false/count:0 contract"
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

# --- Shipment event round-trip (F5) ------------------------------------------
# The F5 part-2a shipment:update event rides the same POST /internal/events
# path: a fully-valid envelope 202s; a missing required field 422s; and
# cod_expected_paise must be a non-negative integer on the wire — a string
# or a negative value 422s (the field is effectively required: its absence
# is also a 422 on the chat side).
evt_shipment='{"topics":["user:sutra-smoke-b"],"event":"shipment:update","dedupe_key":"ship:smoke-f5","payload":{"shipment_id":"ship-smoke-f5","order_id":"ord-smoke-f5","status":"created","updated_at":"2026-10-18T10:00:00Z","cod_expected_paise":49900}}'
[ "$(internal_status "$CHAT_INTERNAL_TOKEN" "$evt_shipment")" = "202" ] \
  || fail "/internal/events shipment:update valid envelope did not 202"
pass "internal/events 202: shipment:update accepted for user:sutra-smoke-b"

evt_missing='{"topics":["user:x"],"event":"shipment:update","dedupe_key":"k","payload":{"shipment_id":"s","status":"created","updated_at":"2026-10-18T10:00:00Z"}}'
[ "$(internal_status "$CHAT_INTERNAL_TOKEN" "$evt_missing")" = "422" ] \
  || fail "/internal/events shipment:update missing order_id did not 422"
pass "internal/events 422: shipment:update missing required field"

evt_strmoney='{"topics":["user:x"],"event":"shipment:update","dedupe_key":"k","payload":{"shipment_id":"s","order_id":"o","status":"created","updated_at":"2026-10-18T10:00:00Z","cod_expected_paise":"49900"}}'
[ "$(internal_status "$CHAT_INTERNAL_TOKEN" "$evt_strmoney")" = "422" ] \
  || fail "/internal/events shipment:update string cod_expected_paise did not 422"
pass "internal/events 422: shipment:update string cod_expected_paise rejected"

evt_negmoney='{"topics":["user:x"],"event":"shipment:update","dedupe_key":"k","payload":{"shipment_id":"s","order_id":"o","status":"created","updated_at":"2026-10-18T10:00:00Z","cod_expected_paise":-1}}'
[ "$(internal_status "$CHAT_INTERNAL_TOKEN" "$evt_negmoney")" = "422" ] \
  || fail "/internal/events shipment:update negative cod_expected_paise did not 422"
pass "internal/events 422: shipment:update negative cod_expected_paise rejected"

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
AUTH_BYPASS= POST_VELOCITY_PER_HOUR=1 FOLLOW_VELOCITY_PER_HOUR=1 docker compose up -d --no-deps api
wait_for 90 "api /healthz (velocity caps on)" curl -sf "$API_URL/healthz"

vel_client="sutra-smoke-v-$(date +%s)"
echo "smoke: creating hydra oauth2 client '$vel_client'"
SMOKE_HYDRA_CLIENT_V_SECRET="${SMOKE_HYDRA_CLIENT_V_SECRET:-$(openssl rand -hex 16)}"
create_jwt_client "$vel_client" "$SMOKE_HYDRA_CLIENT_V_SECRET"

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
AUTH_BYPASS= docker compose up -d --no-deps api
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
create_jwt_client "$com_client" "$SMOKE_HYDRA_CLIENT_C_SECRET"

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

# Over-stock add refuses: 2 + 9 = 11 > 10 available → 409
# insufficient_stock. (Deliberately small: the add must trip the
# stock guard, not the 999 sanity cap on the accumulated line.)
com_code="$(curl -s -o "$com_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d "{\"variant_id\":\"$ordv_id\",\"quantity\":9}" \
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
# ord_st PATCHes the status and prints the new status on stdout. On any
# non-200 it dumps the HTTP status + response body to stderr before
# failing (triage pattern: a blind `curl -sf | json.load` hides the real
# error — the created→confirmed step must never fail without evidence).
ord_st() {
  local oid="$1" to="$2" st_body st_hdrs st_http
  st_body="$(mktemp)"
  st_hdrs="$(mktemp)"
  # `|| true`: a curl transport failure must reach the diag branch below,
  # not trip `set -e` inside the function. -D captures response headers:
  # on a 405 the Allow header identifies WHICH route guard rejected the
  # request (triage #26: PATCH /v1/orders/{id}/status 405'd in CI while
  # the same source routes correctly locally — Allow: PATCH means the
  # status route's own guard saw a non-PATCH method; anything else means
  # a different route shadowed it. TRIAGE #27 RESOLVED it: Allow: GET
  # meant the request had actually hit /v1/orders/{id} (GET-only detail
  # route) — the curl URL was missing the trailing /status, a typo in
  # the 2t ord_st rewrite (the Go router was innocent all along).
  st_http="$(curl -s -D "$st_hdrs" -o "$st_body" -w '%{http_code}' -X PATCH \
    -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
    -d "{\"status\":\"$to\"}" "$API_URL/v1/orders/$oid/status")" || true
  if [ "$st_http" != "200" ]; then
    echo "smoke DIAG: PATCH /v1/orders/$oid/status -> $to: HTTP $st_http" >&2
    grep -i '^allow:' "$st_hdrs" >&2 || echo "smoke DIAG: no Allow header" >&2
    cat "$st_body" >&2
    rm -f "$st_body" "$st_hdrs"
    return 1
  fi
  # `if !` keeps `set -e` from aborting the function before cleanup.
  if ! python3 -c 'import json; print(json.load(open("'"$st_body"'"))["status"])'; then
    rm -f "$st_body" "$st_hdrs"
    return 1
  fi
  rm -f "$st_body" "$st_hdrs"
  return 0
}
[ "$(ord_st "$ord_id" confirmed)" = "confirmed" ] || fail "created → confirmed failed"
com_code="$(curl -s -o "$com_body" -w '%{http_code}' -X PATCH \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"status":"delivered"}' "$API_URL/v1/orders/$ord_id/status")"
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
  -d '{"status":"teleported"}' "$API_URL/v1/orders/$ord_id/status")"
[ "$com_code" = "422" ] || fail "unknown status did not return 422 (got $com_code)"
[ "$(python3 -c 'import json; print(json.load(open("'"$com_body"'"))["error"]["code"])')" = "invalid_status" ] \
  || fail "invalid_status envelope code missing"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X PATCH \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"nope":1}' "$API_URL/v1/orders/$ord_id/status")" = "400" ] \
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

# --- Order event bus round-trip (F6 part 3) -----------------------------------
# The F3 checkout + status transitions above fired order.created and
# order.status_changed onto the sutra JetStream stream (the api boots with
# NATS_URL from compose). Publishing is detached and best-effort, so poll
# the NATS monitoring API until the stream holds at least one message —
# this proves the full path: Checkout/Transition → EventHook →
# PublishOrderEvent → JetStream, with no CLI dependency.
echo "smoke: order event bus round-trip (F6)"
msgs="$(python3 - <<'PYEOF' || echo "0"
import json, sys, time, urllib.request
# Triage #28 (2026-10-20): /jsz?streams=1 does NOT return a top-level
# "streams" list — "streams" is an INTEGER count (verified against the
# nats-server v2.11.8 source: JSInfo.Streams is `json:"streams"` on an
# int; streams=1 without acc flips opts.Accounts on). The per-stream
# detail lives at account_details[].stream_detail[] (StreamDetail.Name
# + StreamDetail.State.messages). The old poller iterated the int,
# raised TypeError inside the try, and the bare except swallowed it —
# 90s of silent polling, then a bogus "no events" FAIL even though the
# api had published order.created/order.status_changed just fine.
deadline = time.time() + 90
def sutra_msgs(info):
    for acc in info.get("account_details") or []:
        for s in acc.get("stream_detail") or []:
            if s.get("name") == "sutra":
                return s.get("state", {}).get("messages", 0) or 0
    return 0
while True:
    try:
        info = json.load(urllib.request.urlopen(
            "http://localhost:8222/jsz?streams=1", timeout=10))
        n = sutra_msgs(info)
        if n >= 1:
            print(n)
            break
    except Exception:
        pass
    if time.time() > deadline:
        print("0")
        break
    time.sleep(3)
PYEOF
)"
[ "${msgs:-0}" -ge 1 ] 2>/dev/null \
  || fail "no order events reached the sutra JetStream stream (F3 fired none)"
pass "order event bus: $msgs message(s) in the sutra stream (order.created/order.status_changed)"

# --- Commerce rate budget (F6 part 3) ----------------------------------------
# The api is recreated with a 1 rps / 1 burst commerce budget: the first
# /v1/categories read returns 200, an immediate second returns 429 with
# code commerce_rate_limited + Retry-After. A non-commerce path on the
# same client is unaffected by the exhausted commerce budget.
echo "smoke: commerce rate budget (F6)"
echo "smoke: recreating api with tight commerce caps (1 rps / 1 burst)"
AUTH_BYPASS= COMMERCE_LIMIT_RPS=1 COMMERCE_LIMIT_BURST=1 docker compose up -d --no-deps api
wait_for 90 "api /healthz (commerce caps)" curl -sf "$API_URL/healthz"

curl -sf -H "Authorization: Bearer $token_c" "$API_URL/v1/categories" >/dev/null \
  || fail "commerce budget: first /v1/categories did not return 200"
cl_code="$(curl -s -o "$com_body" -w '%{http_code}' \
  -H "Authorization: Bearer $token_c" "$API_URL/v1/categories")"
[ "$cl_code" = "429" ] || fail "commerce budget: second /v1/categories did not 429 (got $cl_code)"
[ "$(python3 -c 'import json; print(json.load(open("'"$com_body"'"))["error"]["code"])')" = "commerce_rate_limited" ] \
  || fail "commerce budget: 429 envelope code is not commerce_rate_limited"
[ -n "$(curl -sI -H "Authorization: Bearer $token_c" "$API_URL/v1/categories" | grep -i '^retry-after:')" ] \
  || fail "commerce budget: 429 missing Retry-After header"
pass "commerce budget: 200 → 429 commerce_rate_limited + Retry-After"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/me")" = "200" ] \
  || fail "commerce budget: non-commerce /v1/me was throttled by the commerce budget"
pass "commerce budget: non-commerce path unaffected"

echo "smoke: restoring api to default commerce caps"
AUTH_BYPASS= COMMERCE_LIMIT_RPS= COMMERCE_LIMIT_BURST= docker compose up -d --no-deps api
wait_for 90 "api /healthz (commerce caps restored)" curl -sf "$API_URL/healthz"

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
create_jwt_client "$search_client" "$SMOKE_HYDRA_CLIENT_S_SECRET"

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
expect400() { # expect400 <description> <expected_code> <query-string>...
  local urlargs=() qs
  for qs in "${@:3}"; do urlargs+=(--data-urlencode "$qs"); done
  code="$(curl -s -o "$s_body" -w '%{http_code}' -G -H "Authorization: Bearer $token_s" \
    --data-urlencode 'q=searchable' "${urlargs[@]}" "$API_URL/v1/search")"
  [ "$code" = "400" ] || fail "search: $1 did not return 400 (got $code)"
  got="$(python3 -c 'import json; print(json.load(open("'"$s_body"'"))["error"]["code"])')"
  [ "$got" = "$2" ] || fail "search: $1 envelope code is $got, want $2"
}
expect400 "limit=999" "invalid_limit" "limit=999"
expect400 "bad cursor" "invalid_cursor" "cursor=not-a-cursor"
expect400 "min>max price" "invalid_price_range" "min_price_paise=200" "max_price_paise=100"
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
AUTH_BYPASS= docker compose up -d --no-deps api
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

# --- Mini-app runtime round-trip (G3 part 2) -----------------------------------
# Registry + sandboxed-runtime contract through the production auth path
# (bypass off): register -> slug race -> bad manifest -> owner launches
# draft (preview, signed v1 token, HMAC verified offline) -> publish ->
# second user blocked until install -> install 201/200 -> launch 200 ->
# directory + shelf -> suspend blocks new installs -> archive -> 404 ->
# secretless api -> launch 503 fail-closed.
echo "smoke: mini-app runtime round-trip (G3 part 2)"

SMOKE_MINIAPP_SECRET="$(openssl rand -hex 32)"
echo "smoke: recreating api with MINIAPP_LAUNCH_SECRET"
AUTH_BYPASS= MINIAPP_LAUNCH_SECRET="$SMOKE_MINIAPP_SECRET" docker compose up -d --no-deps api
wait_for 90 "api /healthz (miniapp launch secret)" curl -sf "$API_URL/healthz"

# Two fresh identities: the app developer (owner) and a second user.
mdev_client="sutra-smoke-md-$(date +%s)"
muser_client="sutra-smoke-mu-$(date +%s)"
mdev_secret="$(openssl rand -hex 24)"
muser_secret="$(openssl rand -hex 24)"
for pair in "$mdev_client $mdev_secret" "$muser_client $muser_secret"; do
  set -- $pair
  create_jwt_client "$1" "$2"
done
pass "hydra oauth2 clients for mini-app smoke created"
hydra_token() { # hydra_token <client> <secret> — client_credentials token
  curl -sf -u "$1:$2" \
    --data-urlencode 'grant_type=client_credentials' \
    --data-urlencode 'audience=sutra-api' \
    --data-urlencode 'scope=openid' \
    http://localhost:4444/oauth2/token \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])'
}
mdev_token="$(hydra_token "$mdev_client" "$mdev_secret")" || fail "hydra token for dev client failed"
muser_token="$(hydra_token "$muser_client" "$muser_secret")" || fail "hydra token for user client failed"

mapp_slug="smoke-app-$(date +%s)"
mapp="$(curl -sf -X POST \
  -H "Authorization: Bearer $mdev_token" -H 'Content-Type: application/json' \
  -d "{\"name\":\"Smoke App\",\"slug\":\"$mapp_slug\",\"entry_url\":\"https://smoke.example/app\",\"manifest\":{\"permissions\":[\"storage\",\"share\"]}}" \
  "$API_URL/v1/mini-apps")" || fail "miniapp: register draft failed"
[ "$(printf '%s' "$mapp" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')" = "draft" ] \
  || fail "miniapp: new app is not draft"
mapp_id="$(jget "$mapp" id)"
pass "miniapp: registered draft '$mapp_slug' ($mapp_id)"

# Slug race: second registration with the same slug loses.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $mdev_token" -H 'Content-Type: application/json' \
  -d "{\"name\":\"Copy\",\"slug\":\"$mapp_slug\",\"entry_url\":\"https://smoke.example/x\"}" \
  "$API_URL/v1/mini-apps")" = "409" ] \
  || fail "miniapp: duplicate slug did not return 409"
pass "miniapp: slug race -> 409 slug_taken"

# Unknown permission is rejected at write time.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $mdev_token" -H 'Content-Type: application/json' \
  -d "{\"name\":\"Bad\",\"slug\":\"smoke-bad-$(date +%s)\",\"entry_url\":\"https://smoke.example/x\",\"manifest\":{\"permissions\":[\"telepathy\"]}}" \
  "$API_URL/v1/mini-apps")" = "400" ] \
  || fail "miniapp: unknown permission did not return 400"
pass "miniapp: unknown permission -> 400 invalid_manifest"

# Owner launches the draft (preview): 200 + signed v1 launch token.
mlaunch="$(curl -sf -X POST \
  -H "Authorization: Bearer $mdev_token" \
  "$API_URL/v1/mini-apps/$mapp_id/launch")" || fail "miniapp: owner draft launch failed"
mtoken="$(jget "$mlaunch" launch_token)"
[ "${mtoken#v1.}" != "$mtoken" ] || fail "miniapp: launch token is not versioned v1"
python3 - "$SMOKE_MINIAPP_SECRET" "$mtoken" <<PYEOF \
  || fail "miniapp: launch token signature invalid"
import sys, hmac, hashlib, base64, json
secret = sys.argv[1].encode()
token = sys.argv[2]
ver, body, sig = token.split('.')
assert ver == 'v1', 'version tag'
want = base64.urlsafe_b64encode(
    hmac.new(secret, f'{ver}.{body}'.encode(), hashlib.sha256).digest()
).rstrip(b'=').decode()
assert hmac.compare_digest(want, sig), 'signature mismatch'
payload = json.loads(base64.urlsafe_b64decode(body + '=='))
assert payload['v'] == 1 and payload['uid'] and payload['aid'], 'payload shape'
PYEOF
pass "miniapp: owner draft launch -> signed v1 token (HMAC verified offline)"

# Non-owner cannot launch a draft: 403 launch_not_allowed.
ml_body="$(mktemp)"
ml_code="$(curl -s -o "$ml_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $muser_token" \
  "$API_URL/v1/mini-apps/$mapp_id/launch")"
[ "$ml_code" = "403" ] || { rm -f "$ml_body"; fail "miniapp: non-owner draft launch did not 403 (got $ml_code)"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$ml_body"'"))["error"]["code"])')" = "launch_not_allowed" ] \
  || { rm -f "$ml_body"; fail "miniapp: 403 envelope code is not launch_not_allowed"; }
rm -f "$ml_body"
pass "miniapp: non-owner draft launch -> 403 launch_not_allowed"

# Publish; the second user still cannot launch (no install edge) -> 403.
mpub="$(curl -sf -X POST \
  -H "Authorization: Bearer $mdev_token" \
  "$API_URL/v1/mini-apps/$mapp_id/publish")" || fail "miniapp: publish failed"
[ "$(jget "$mpub" status)" = "published" ] || fail "miniapp: app not published"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $muser_token" \
  "$API_URL/v1/mini-apps/$mapp_id/launch")" = "403" ] \
  || fail "miniapp: uninstalled user launch did not 403"
pass "miniapp: published; uninstalled user launch -> 403"

# Install: 201 first, 200 on idempotent retry.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $muser_token" \
  "$API_URL/v1/mini-apps/$mapp_id/install")" = "201" ] \
  || fail "miniapp: install did not return 201"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $muser_token" \
  "$API_URL/v1/mini-apps/$mapp_id/install")" = "200" ] \
  || fail "miniapp: install retry did not return 200"
pass "miniapp: install -> 201 then idempotent 200"

# Installed user launches: 200, token signature still verifies.
mlaunch2="$(curl -sf -X POST \
  -H "Authorization: Bearer $muser_token" \
  "$API_URL/v1/mini-apps/$mapp_id/launch")" || fail "miniapp: installed launch failed"
mtoken2="$(jget "$mlaunch2" launch_token)"
python3 - "$SMOKE_MINIAPP_SECRET" "$mtoken2" <<PYEOF \
  || fail "miniapp: installed launch token invalid"
import sys, hmac, hashlib, base64
secret = sys.argv[1].encode()
token = sys.argv[2]
ver, body, sig = token.split('.')
want = base64.urlsafe_b64encode(
    hmac.new(secret, f'{ver}.{body}'.encode(), hashlib.sha256).digest()
).rstrip(b'=').decode()
assert hmac.compare_digest(want, sig), 'signature mismatch'
PYEOF
pass "miniapp: installed user launch -> 200 signed token"

# Directory lists the published app; the shelf shows entry_url+manifest.
mdir="$(curl -sf -H "Authorization: Bearer $muser_token" "$API_URL/v1/mini-apps?limit=100")" \
  || fail "miniapp: directory failed"
python3 - "$mdir" "$mapp_slug" <<PYEOF || fail "miniapp: app missing from directory"
import json, sys
page = json.loads(sys.argv[1])
slugs = [a['slug'] for a in page['apps']]
assert sys.argv[2] in slugs, f'slug not in directory: {slugs}'
PYEOF
mshelf="$(curl -sf -H "Authorization: Bearer $muser_token" "$API_URL/v1/me/mini-apps")" \
  || fail "miniapp: shelf failed"
python3 - "$mshelf" "$mapp_id" <<PYEOF || fail "miniapp: shelf missing entry_url/manifest"
import json, sys
shelf = json.loads(sys.argv[1])
apps = [a for a in shelf['apps'] if a['app']['id'] == sys.argv[2]]
assert len(apps) == 1, 'app not on shelf'
assert apps[0]['app']['entry_url'].startswith('https://'), 'entry_url missing'
assert set(apps[0]['app']['manifest']['permissions']) == {'storage', 'share'}, 'manifest mismatch'
PYEOF
pass "miniapp: directory lists app; shelf carries entry_url + manifest"

# Suspend: new installs 422, but the installed user still launches.
msus="$(curl -sf -X POST \
  -H "Authorization: Bearer $mdev_token" \
  "$API_URL/v1/mini-apps/$mapp_id/suspend")" || fail "miniapp: suspend failed"
[ "$(jget "$msus" status)" = "suspended" ] || fail "miniapp: app not suspended"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $mdev_token" \
  "$API_URL/v1/mini-apps/$mapp_id/install")" = "422" ] \
  || fail "miniapp: install on suspended app did not 422"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $muser_token" \
  "$API_URL/v1/mini-apps/$mapp_id/launch")" = "200" ] \
  || fail "miniapp: installed user launch on suspended app failed"
pass "miniapp: suspended -> new installs 422, installed launches keep working"

# Archive (owner) -> 204; GET 404s.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $mdev_token" \
  "$API_URL/v1/mini-apps/$mapp_id")" = "204" ] \
  || fail "miniapp: archive did not return 204"
[ "$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $mdev_token" \
  "$API_URL/v1/mini-apps/$mapp_id")" = "404" ] \
  || fail "miniapp: archived app did not 404"
pass "miniapp: archived -> 404 everywhere"

# Fail-closed: secretless api answers 503 launch_unavailable.
echo "smoke: restoring api without MINIAPP_LAUNCH_SECRET"
AUTH_BYPASS= docker compose up -d --no-deps api
wait_for 90 "api /healthz (launchless)" curl -sf "$API_URL/healthz"
mapp2="$(curl -sf -X POST \
  -H "Authorization: Bearer $mdev_token" -H 'Content-Type: application/json' \
  -d "{\"name\":\"Smoke App 2\",\"slug\":\"smoke-app2-$(date +%s)\",\"entry_url\":\"https://smoke.example/b\"}" \
  "$API_URL/v1/mini-apps")" || fail "miniapp: register app 2 failed"
mapp2_id="$(jget "$mapp2" id)"
m503_body="$(mktemp)"
m503_code="$(curl -s -o "$m503_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $mdev_token" \
  "$API_URL/v1/mini-apps/$mapp2_id/launch")"
[ "$m503_code" = "503" ] || { rm -f "$m503_body"; fail "miniapp: secretless launch did not 503 (got $m503_code)"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$m503_body"'"))["error"]["code"])')" = "launch_unavailable" ] \
  || { rm -f "$m503_body"; fail "miniapp: 503 envelope code is not launch_unavailable"; }
rm -f "$m503_body"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $mdev_token" \
  "$API_URL/v1/mini-apps/$mapp2_id")" = "204" ] \
  || fail "miniapp: cleanup archive of app 2 failed"
pass "miniapp: secretless launch -> 503 launch_unavailable (fail-closed)"

# --- Ads platform round-trip (H3 part 3) -------------------------------------
# Reuses the F1 commerce identity (token_c), which already holds a
# registered seller row: campaign create (draft) → unknown targeting key
# 400s (closed targeting) → draft report (not eligible, budget intact,
# no projections, delivery absent) → activate → eligible report →
# creative register + activate → archive → report 404s (archived
# campaigns never leak).
echo "smoke: ads platform round-trip (H3)"
ads_body="$(mktemp)"

# NOTE: country/language codes are LOWERCASE per the advertiser contract
# (internal/ads validates ^[a-z]{2}$; "IN" is a loud 400 invalid_targeting
# by design — the closed targeting set must never silently match).
ads_create_body="$(mktemp)"
ads_create_code="$(curl -s -o "$ads_create_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"name":"Smoke Kurta Campaign","budget_paise":5000000,"daily_budget_paise":500000,"targeting":{"countries":["in"],"languages":["hi","en"],"categories":["audio"],"devices":["mobile"]}}' \
  "$API_URL/v1/ads/campaigns")"
[ "$ads_create_code" = "201" ] || { echo "ads: create response: $(cat "$ads_create_body")"; rm -f "$ads_create_body"; fail "ads: campaign create failed (got $ads_create_code)"; }
camp_json="$(cat "$ads_create_body")"; rm -f "$ads_create_body"
camp_id="$(jget "$camp_json" id)"
[ "$(jget "$camp_json" status)" = "draft" ] || fail "ads: new campaign is not draft"
pass "ads: campaign created as draft: $camp_id"

# Closed targeting: an unknown key fails loudly. A typo'd key silently
# matching everything would spend real money.
ads_code="$(curl -s -o "$ads_body" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"name":"Bad Targeting","budget_paise":100000,"targeting":{"planets":["mars"]}}' \
  "$API_URL/v1/ads/campaigns")"
[ "$ads_code" = "400" ] || fail "ads: unknown targeting key did not 400 (got $ads_code)"
[ "$(python3 -c 'import json; print(json.load(open("'"$ads_body"'"))["error"]["code"])')" = "invalid_targeting" ] \
  || fail "ads: 400 envelope code is not invalid_targeting"
pass "ads: unknown targeting key → 400 invalid_targeting"

# Draft report: not eligible, full budget remaining, no projections
# without spend history, delivery explicitly absent (v1 is accounting,
# not analytics).
rep_draft="$(curl -sf -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/ads/campaigns/$camp_id/report")" || fail "ads: draft report failed"
python3 -c '
import json, sys
r = json.load(sys.stdin)
assert r["eligible_now"] is False, r
assert r["eligibility_reason"] == "not_active", r
assert r["budget_paise"] == 5000000 and r["remaining_paise"] == 5000000, r
assert r["spend_paise"] == 0, r
assert r.get("spend_per_day_paise") is None and r.get("projected_exhaustion_at") is None, r
assert r["delivery"]["present"] is False, r
' <<<"$rep_draft" || fail "ads: draft report shape wrong"
pass "ads: draft report — ineligible, budget intact, no projections, delivery absent"

# Activate → eligible. Bad campaign id → 400.
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/ads/campaigns/not-a-uuid/report")" = "400" ] \
  || fail "ads: report with bad campaign id did not 400"
act_json="$(curl -sf -X POST -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/ads/campaigns/$camp_id/activate")" || fail "ads: activate failed"
[ "$(jget "$act_json" status)" = "active" ] || fail "ads: campaign not active after activate"
rep_live="$(curl -sf -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/ads/campaigns/$camp_id/report")" || fail "ads: active report failed"
[ "$(printf '%s' "$rep_live" | python3 -c 'import json,sys; print(json.load(sys.stdin)["eligible_now"])')" = "True" ] \
  || fail "ads: active campaign not eligible in report"
pass "ads: activate → eligible in report; bad id → 400"

# Creative register (native, draft) → activate.
cre_json="$(curl -sf -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"name":"Smoke Native Creative","kind":"native","format":"native","media_url":"https://smoke.example/img.jpg","landing_url":"https://smoke.example/kurta","headline":"Smoke Kurta","cta":"shop_now"}' \
  "$API_URL/v1/ads/campaigns/$camp_id/creatives")" || fail "ads: creative create failed"
cre_id="$(jget "$cre_json" id)"
[ "$(jget "$cre_json" status)" = "draft" ] || fail "ads: new creative is not draft"
cact_json="$(curl -sf -X POST -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/ads/creatives/$cre_id/activate")" || fail "ads: creative activate failed"
[ "$(jget "$cact_json" status)" = "active" ] || fail "ads: creative not active after activate"
pass "ads: creative registered + activated: $cre_id"

# Serving (H3 part 3b): set a bid on the campaign, then fill a slot.
# The single bidder pays the reserve floor (ADS_RESERVE_PAISE default
# 50); the ad_events ledger row carries the 55/45 split and the
# campaign's spend_paise accrues in the same transaction.
bid_json="$(curl -sf -X PATCH \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"bid_paise":5000}' \
  "$API_URL/v1/ads/campaigns/$camp_id")" || fail "ads: set bid failed"
[ "$(jget "$bid_json" bid_paise)" = "5000" ] || fail "ads: bid_paise not set on campaign"
serve_json="$(curl -sf -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"slot":"smoke_feed_1","categories":["audio"],"language":"hi","country":"IN"}' \
  "$API_URL/v1/ads/serve")" || fail "ads: serve failed"
python3 -c '
import json, sys
s = json.load(sys.stdin)
assert s["creative_id"] == sys.argv[1], s
assert s["campaign_id"] == sys.argv[2], s
assert s["price_paise"] == 50, s          # single bidder → reserve floor
assert s["creator_share_paise"] + s["platform_share_paise"] == s["price_paise"], s
# Money is integer paise: the creator keeps floor(55%) = price*55//100
# (50*55//100 = 27), the platform keeps the remainder. An exact
# creator*100//price == 55 assert is impossible at price=50 under
# integer money math, so pin the floor contract instead (mirrors the
# pinned Go unit test: CreatorSharePaise == 50*55/100).
assert s["creator_share_paise"] == s["price_paise"] * 55 // 100, s
assert s["platform_share_paise"] == s["price_paise"] - s["creator_share_paise"], s
assert s["impression_id"], s
assert s["creative"]["status"] == "active", s
' "$cre_id" "$camp_id" <<<"$serve_json" || fail "ads: serve response shape wrong"
rep_billed="$(curl -sf -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/ads/campaigns/$camp_id/report")" || fail "ads: report after serve failed"
[ "$(printf '%s' "$rep_billed" | python3 -c 'import json,sys; print(json.load(sys.stdin)["spend_paise"])')" = "50" ] \
  || fail "ads: spend did not accrue on report after serve"
pass "ads: serve → 200, reserve-floor price, 55/45 split, spend accrued"

# Serve a slot that no campaign can fill → 204 (organic fallback),
# and bad slot shapes → 400 loud. v1 serving gives every ELIGIBLE
# campaign a quality floor (0.5 — unrelated categories only lose the
# boost, never the fill; pinned by TestContextualQualityMatrix), so
# "no campaign can fill" means no ELIGIBLE campaign: pause first, then
# resume so the rest of the round-trip is unchanged.
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/ads/campaigns/$camp_id/pause")" = "200" ] \
  || fail "ads: pause before no-demand serve failed"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"slot":"empty_slot","categories":["no-such-category-xyz"]}' \
  "$API_URL/v1/ads/serve")" = "204" ] \
  || fail "ads: serve with no eligible demand did not 204"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/ads/campaigns/$camp_id/resume")" = "200" ] \
  || fail "ads: resume after no-demand serve failed"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"categories":["audio"]}' \
  "$API_URL/v1/ads/serve")" = "400" ] \
  || fail "ads: serve without slot did not 400"
pass "ads: no-demand serve → 204; missing slot → 400"

# Archive campaign (terminal) → report 404s (archived campaigns never
# leak); campaign GET 404s too.
arch_json="$(curl -sf -X DELETE -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/ads/campaigns/$camp_id")" || fail "ads: archive failed"
[ "$(jget "$arch_json" status)" = "archived" ] || fail "ads: campaign not archived after DELETE"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token_c" \
  "$API_URL/v1/ads/campaigns/$camp_id/report")" = "404" ] \
  || fail "ads: report on archived campaign did not 404"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_c" -H 'Content-Type: application/json' \
  -d '{"slot":"smoke_feed_1","categories":["audio"],"language":"hi","country":"IN"}' \
  "$API_URL/v1/ads/serve")" = "204" ] \
  || fail "ads: serve after archive did not 204"
rm -f "$ads_body"
pass "ads: archive terminal; report on archived → 404 (cleanup); serve → 204"

# --- Memberships round-trip (H4) ---------------------------------------------------
# Reuses the H1 identities: $token_h (creator) and $token_h2 (member,
# owns nothing). Plans CRUD is pure local config (no provider), so it
# works fully; subscribe/cancel/webhook must fail CLOSED (503) while
# the stub is the only wired provider — a fake 201 would claim a
# mandate that does not exist. The boot log must carry the
# subscriptions fail-closed WARN and no set-but-ignored WARN (creds
# are unset in smoke).
echo "smoke: memberships round-trip (H4)"
sub_body="$(mktemp)"; sub_hdr="$(mktemp)"

plan_json="$(curl -sf -X POST \
  -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"name":"Smoke Inner Circle","description":"paid supporter tier","price_paise":19900,"billing_period":"monthly","currency":"INR"}' \
  "$API_URL/v1/memberships/plans")" || fail "H4: plan create failed"
plan_id="$(jget "$plan_json" id)"
[ "$(jget "$plan_json" is_active)" = "True" ] || fail "H4: new plan is not active"
[ "$(jget "$plan_json" price_paise)" = "19900" ] || fail "H4: plan price not stored"
pass "H4: plan created: $plan_id"

# Zero-price plan → 400 (validation runs before any write).
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"name":"Free Tier","price_paise":0,"billing_period":"monthly"}' \
  "$API_URL/v1/memberships/plans")" = "400" ] \
  || fail "H4: zero-price plan did not 400"
pass "H4: zero-price plan → 400 invalid_field"

# List carries the plan; a different user's read 404s (never leaks
# existence).
plans_json="$(curl -sf -H "Authorization: Bearer $token_h" \
  "$API_URL/v1/memberships/plans")" || fail "H4: plan list failed"
[ "$(printf '%s' "$plans_json" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["plans"]))')" -ge 1 ] \
  || fail "H4: plan list missing the new plan"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token_h2" \
  "$API_URL/v1/memberships/plans/$plan_id")" = "404" ] \
  || fail "H4: other user's plan read did not 404"
pass "H4: list + owner get + other-user 404 (no existence leak)"

# Rename → 200; price_paise is immutable → 400 immutable_field (a client
# that tries to rewrite a member's terms gets a loud error, never a
# silent ignore).
patch_json="$(curl -sf -X PATCH \
  -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"name":"Smoke Inner Circle Plus"}' \
  "$API_URL/v1/memberships/plans/$plan_id")" || fail "H4: plan patch failed"
[ "$(jget "$patch_json" name)" = "Smoke Inner Circle Plus" ] \
  || fail "H4: plan rename did not stick"
imm_code="$(curl -s -o "$sub_body" -w '%{http_code}' -X PATCH \
  -H "Authorization: Bearer $token_h" -H 'Content-Type: application/json' \
  -d '{"price_paise":29900}' \
  "$API_URL/v1/memberships/plans/$plan_id")"
[ "$imm_code" = "400" ] \
  || { rm -f "$sub_body" "$sub_hdr"; fail "H4: price patch did not 400 (got $imm_code)"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$sub_body"'"))["error"]["code"])')" = "immutable_field" ] \
  || { rm -f "$sub_body" "$sub_hdr"; fail "H4: price patch envelope code is not immutable_field"; }
pass "H4: rename → 200; price change → 400 immutable_field"

# Subscribe: the stub fails closed → 503 subscriptions_unavailable +
# Retry-After, never a fake 201. No token → 401.
sub_code="$(curl -s -o "$sub_body" -D "$sub_hdr" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $token_h2" -H 'Content-Type: application/json' \
  -d '{"plan_id":"'"$plan_id"'","customer_name":"Smoke Member","customer_phone":"+919876543210","customer_email":"member@example.com"}' \
  "$API_URL/v1/memberships/subscriptions")"
[ "$sub_code" = "503" ] \
  || { rm -f "$sub_body" "$sub_hdr"; fail "H4: subscribe did not fail closed 503 (got $sub_code)"; }
[ "$(python3 -c 'import json; print(json.load(open("'"$sub_body"'"))["error"]["code"])')" = "subscriptions_unavailable" ] \
  || { rm -f "$sub_body" "$sub_hdr"; fail "H4: subscribe envelope code is not subscriptions_unavailable"; }
grep -qi '^Retry-After: [1-9]' "$sub_hdr" \
  || { rm -f "$sub_body" "$sub_hdr"; fail "H4: subscribe 503 missing Retry-After header"; }
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"plan_id":"'"$plan_id"'","customer_name":"x","customer_phone":"+919876543210","customer_email":"x@y.z"}' \
  "$API_URL/v1/memberships/subscriptions")" = "401" ] \
  || { rm -f "$sub_body" "$sub_hdr"; fail "H4: unauthenticated subscribe did not 401"; }
pass "H4: subscribe fails closed — 503 subscriptions_unavailable + Retry-After; 401 without token"

# The member's own subscription list is empty (no row can exist), and
# the entitlements list is empty. Cancel of an unknown id → 404 (the
# owner-scoped lookup runs before the provider), bad id → 400.
mine_json="$(curl -sf -H "Authorization: Bearer $token_h2" \
  "$API_URL/v1/memberships/subscriptions")" || fail "H4: subscriptions list failed"
python3 -c 'import json,sys; assert json.load(sys.stdin)["subscriptions"] == [], "non-empty"' \
  <<<"$mine_json" || fail "H4: subscriptions list not empty"
ent_json="$(curl -sf -H "Authorization: Bearer $token_h2" \
  "$API_URL/v1/memberships/entitlements")" || fail "H4: entitlements failed"
python3 -c 'import json,sys; assert json.load(sys.stdin)["entitlements"] == [], "non-empty"' \
  <<<"$ent_json" || fail "H4: entitlements not empty"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $token_h2" \
  "$API_URL/v1/memberships/subscriptions/00000000-0000-0000-0000-000000000000/cancel")" = "404" ] \
  || fail "H4: cancel of unknown subscription did not 404"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $token_h2" \
  "$API_URL/v1/memberships/subscriptions/not-a-uuid/cancel")" = "400" ] \
  || fail "H4: cancel with bad id did not 400"
pass "H4: empty subscriptions + entitlements; unknown cancel → 404; bad id → 400"

# Webhook ingress: the stub cannot verify a callback → 503 (an
# unverified callback is never acknowledged, no local state touched).
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"type":"SUBSCRIPTION_PAYMENT_SUCCESS"}' \
  "$API_URL/internal/subscriptions/webhook")" = "503" ] \
  || fail "H4: subscription webhook did not fail closed 503"
pass "H4: webhook ingress fails closed — 503 subscriptions_unavailable"

# Deactivate: plans are never hard-deleted (history and accruals stay).
del_json="$(curl -sf -X DELETE -H "Authorization: Bearer $token_h" \
  "$API_URL/v1/memberships/plans/$plan_id")" || fail "H4: plan deactivate failed"
[ "$(jget "$del_json" is_active)" = "False" ] \
  || fail "H4: plan not inactive after DELETE"
pass "H4: deactivate → is_active false (history intact, no hard delete)"

# Boot log: the subscriptions seam must boot with its loud fail-closed
# WARN and no set-but-ignored WARN (creds are unset in smoke).
api_boot_log="$(mktemp)"
docker compose logs --no-log-prefix api >"$api_boot_log" 2>/dev/null \
  || fail "could not read the api boot log (docker compose logs api failed)"
[ -s "$api_boot_log" ] || fail "api boot log is empty"
grep -q "subscriptions: running with the fail-closed stub" "$api_boot_log" \
  || { echo "smoke: api boot log tail (subscriptions WARN not found):" >&2; tail -30 "$api_boot_log" >&2; rm -f "$api_boot_log" "$sub_body" "$sub_hdr"; fail "api boot log missing the fail-closed subscriptions WARN"; }
pass "api boot log carries the fail-closed subscriptions WARN"
grep -q "subscriptions: CASHFREE_" "$api_boot_log" \
  && { rm -f "$api_boot_log" "$sub_body" "$sub_hdr"; fail "api boot log claims CASHFREE_* set-but-ignored (creds are unset in smoke)"; }
pass "api boot log has no set-but-ignored subscription WARN"
rm -f "$api_boot_log" "$sub_body" "$sub_hdr"

# --- I1 E2E device keys + I2 session-init + I3 message envelope round-trip --
# openssl-minted Ed25519 identity + X25519 signed prekey (signature made
# by the identity key, exactly like a real client) -> Go API register
# (201; cross-key-signed bundle 422s) -> directory -> REST consume of
# one-time prekey id 1 -> websocket user:<subject> join ->
# e2e:prekey_fetch consumes id 2 on the chat side -> REST consume of
# id 3 -> drained-queue fetch over the websocket returns the bundle
# with one_time_prekey null (signed-prekey-only X3DH fallback) ->
# unknown device not_found / empty payload invalid_request ->
# I2: join room:smoke-e2e-room -> push e2e:session_init (fresh X25519
# base key + device identity key + 64 random ciphertext bytes,
# prekey_id 1 audit echo) -> e2e:session_init broadcast (canonical
# envelope, kind e2e_session_init) + sender-only msg:ack (deduped
# false, ids match) -> same-envelope retry -> broadcast + ack with
# deduped:true and the ORIGINAL id (no second session) -> 31-byte key
# -> "handshake public key must be 32 bytes", missing recipient ->
# "handshake envelope missing a required field" -> reconnect + rejoin
# -> the init replays from history (offline-recipient path) ->
# device deleted (cleanup). The REST-1 / WS-2 / REST-3 ordering proves
# the Go API and the chat share one atomic prekey queue.
# I3: after the I2 init flips the room's E2E flag -> message:new +
# e2e_data (openssl-minted ratchet key + 96 random ciphertext bytes) ->
# message:new broadcast (kind e2e_message, canonical ciphertext-only
# envelope) + sender-only msg:ack (deduped false) -> retry -> deduped:true
# with the ORIGINAL id -> mixed body+e2e_data, 31-byte ratchet key,
# empty ciphertext, missing recipient, missing client_msg_id -> error
# replies, nothing persisted -> reconnect + rejoin -> the e2e message
# replays from history (kind e2e_message) -> join reports e2e_enabled:true
# -> GET /v1/conversations/<room>/e2e -> 200 (unknown room -> 404, never
# claim E2E; malformed id -> 400). Runs against
# the real-issuer path (bypass stays off, as for all sections above);
# the chat boots with AUTH_BYPASS=1, so the websocket connects as the
# bypass subject and joins its own user:<subject> topic.
echo "smoke: I1 E2E device keys + I2 session-init + I3 message envelope round-trip"
e2e_client="sutra-e2e-$(date +%s)"
SMOKE_E2E_SECRET="$(openssl rand -hex 16)"
create_jwt_client "$e2e_client" "$SMOKE_E2E_SECRET"
e2e_token="$(hydra_token "$e2e_client" "$SMOKE_E2E_SECRET")" \
  || fail "e2e: hydra token request for '$e2e_client' failed"
[ -n "$e2e_token" ] || fail "e2e: hydra token for '$e2e_client' was empty"
python3 scripts/smoke_e2e.py "$API_URL" "$CHAT_HOST" "$CHAT_PORT" \
  "$e2e_token" "$e2e_client" "$EXPECTED_SUBJECT" \
  || fail "I1 E2E device-key round-trip + I2 session-init + I3 message envelope transport"
pass "I1: E2E device-key round-trip complete; I2: session-init transport round-trip complete; I3: e2e message envelope round-trip complete"

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
