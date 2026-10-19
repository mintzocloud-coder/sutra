#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# G4: SDK-stub generation check — docs/openapi.yaml is the single source of
# truth for the REST surface. This script proves a machine can consume it as
# an SDK: generate a TypeScript client stub (openapi-typescript) and compile
# a real consumer against it (tsc). If generation silently drops routes or a
# consumer-facing shape drifts, the build fails — the spec cannot rot into
# fiction unnoticed.
#
# Runs locally (needs node + npx) and in CI (the `openapi` job).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT

echo "[sdk-check] generating TypeScript stub from docs/openapi.yaml ..."
npx --yes openapi-typescript@7 "$REPO_ROOT/docs/openapi.yaml" -o "$OUT/sutra-api.ts"

# 1) Surface coverage: the stub must carry the headline routes and schemas.
for needle in '"/v1/me"' '"/v1/ai/chat"' '"/v1/webhooks/endpoints"' '"/v1/mini-apps"' 'User: {' 'WebhookEndpoint: {' 'MiniApp: {'; do
  grep -qF "$needle" "$OUT/sutra-api.ts" || { echo "[sdk-check] MISSING surface: $needle" >&2; exit 1; }
done

# 2) Path parity: generation must not silently drop paths.
spec_paths="$(grep -c '^  /' "$REPO_ROOT/docs/openapi.yaml")"
stub_paths="$(grep -cE '^    "/' "$OUT/sutra-api.ts")"
[ "$spec_paths" -eq "$stub_paths" ] || {
  echo "[sdk-check] PATH MISMATCH: spec has $spec_paths paths, stub has $stub_paths" >&2
  exit 1
}

# 3) The stub must be usable as a real SDK: compile a consumer against it.
cat > "$OUT/consumer.ts" <<'EOF'
import type { paths, components } from "./sutra-api";

// A real SDK consumer: GET /v1/me, typed request + response.
type MeResponse =
  paths["/v1/me"]["get"]["responses"][200]["content"]["application/json"];

declare function getMe(token: string): Promise<MeResponse>;

declare const endpoint: components["schemas"]["WebhookEndpoint"];

async function main(token: string): Promise<string> {
  const me: MeResponse = await getMe(token);
  // If these shapes drift in the spec, this file stops compiling.
  const id: string = me.id;
  const subject: string = me.subject;
  const displayName: string | null = me.display_name;
  const url: string = endpoint.url;
  return `${id}:${subject}:${displayName}:${url}`;
}
export { main };
EOF
npx --yes -p typescript@5 tsc --noEmit --strict --target es2020 \
  --module esnext --moduleResolution bundler "$OUT/consumer.ts"

echo "[sdk-check] OK: $stub_paths paths generated, consumer compiles clean."
