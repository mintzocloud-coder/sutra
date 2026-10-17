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
export AUTH_BYPASS=1

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
docker compose build --quiet api chat 2>/dev/null || docker compose build api chat

echo "smoke: starting postgres, valkey, seaweedfs"
docker compose up -d postgres valkey seaweedfs
wait_for 90 "postgres to accept connections" \
  docker compose exec -T postgres pg_isready -U sutra

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
# NOTE: the api is intentionally left running WITHOUT the bypass from here
# on — the more production-faithful state. Nothing later in this script
# needs the bypass (chat keeps its own for the websocket round-trip).

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
