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
# Identity is exercised through AUTH_BYPASS=1 — the services' documented
# dev-only escape hatch (both log a loud WARN when it is set; it is never
# enabled in production). The chat release fails closed at boot without a
# real SECRET_KEY_BASE, so this script generates an ephemeral one (kept in
# the environment only, never written to disk or committed).
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
