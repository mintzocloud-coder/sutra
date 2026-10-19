<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# Sutra developer platform

The developer platform is how third parties build **on** Sutra: mini-apps,
webhooks, and API clients. One rule governs all of it:

> **`docs/openapi.yaml` is the single source of truth** for the REST
> surface. If a route exists in the code but not in the spec, that is a
> bug. If a route exists in the spec but not in the code, that is also a
> bug. CI fails the build either way.

## The single-source-of-truth gates (shipped)

Two CI gates on the `openapi` job enforce this:

1. **Coverage gate** — `scripts/check_openapi_coverage.py` (G4 part 1).
   Parses route registrations out of `services/api/cmd/api/main.go`
   (including the `mini-apps/{id}/{action}` dispatch, parsed from the
   handler source so it cannot rot) and asserts: every registered route is
   in the spec (path + method level), every spec path is served by the
   code (no stale docs), and the internal `/internal/livekit/webhook`
   route stays out of the public spec.
2. **SDK-stub generation check** — `scripts/check_sdk_generation.sh`
   (G4 part 2). Generates a TypeScript client stub from the spec with
   `openapi-typescript` and compiles a real consumer against it with
   `tsc --strict`. This proves the spec is machine-consumable: generation
   must carry all 50+ paths (parity asserted), and any drift in a
   consumer-facing shape (`User.id`, `WebhookEndpoint.url`, …) breaks the
   consumer compile — the spec cannot silently stop being a valid SDK
   source.

The spec lints clean on every push (redocly; the two deliberately-disabled
rules are documented in `redocly.yaml`).

## Regenerating an SDK locally

```bash
npx --yes openapi-typescript@7 docs/openapi.yaml -o sutra-api.ts
```

That gives typed `paths`/`operations`/`components` namespaces (see the
consumer in `scripts/check_sdk_generation.sh` for the usage pattern). Other
generators (openapi-generator, Kiota, …) work from the same file; the
CI gate guarantees the file is complete and current.

## Platform surfaces

- **REST API** — bearer-authenticated (`docs/openapi.yaml`), versioned
  under `/v1/`, standard JSON error envelope
  (`{"error":{"code","message","request_id"}}`), per-IP rate budgets
  (`docs/production.md`).
- **Webhooks** — per-user Svix apps (`user:<id>`); platform events
  (`order.created`, `order.status_changed`, `live:started`, `live:ended`,
  `shipment:update`) publish to subscribed endpoints with Svix signing,
  retries, and a delivery log. Full contract: `docs/webhooks.md`.
- **Mini-apps** — registry with immutable slugs, permission-manifest
  bridge API, HMAC launch tokens. Directory, install flow, and the
  non-negotiable security rules: `docs/mini-apps.md`.
- **Realtime events** — Phoenix `user:<id>` topics for server-to-client
  push (post/follower/live/shipment events). Contract:
  `docs/realtime-events.md`.
- **Identity** — OIDC (Hydra in dev/compose; Kratos for self-service
  identity). Token contract: `docs/auth-contract.md`,
  `docs/identity.md`.

## Developer docs site + onboarding guide (shipped, G4 part 2b)

- **`docs-site/` — Docusaurus skeleton.** `npm install && npm start` runs
  the developer docs site locally. **Licensing split, deliberate:** the
  skeleton (config, theme plumbing, build scripts, `package.json`) is
  **MIT** (reuse it anywhere); the guide content under `docs-site/docs/`
  carries the repo's `AGPL-3.0-or-later` header like every other doc.
  `scripts/check-headers.sh` excludes `docs-site/` for the skeleton and
  still requires AGPL headers on the guide content.
- **`docs-site/docs/onboarding.md` — the developer onboarding guide.**
  One linear walkthrough: app keys (Hydra OAuth2 client-credentials with
  the `jwt-bearer` grant so tokens are JWT-shaped, audience `sutra-api`),
  just-in-time provisioning on the first `GET /v1/me`, platform
  conventions (error envelope, `client_msg_id` idempotency, keyset
  pagination, 429s), webhook endpoint registration + the signing-secret
  once-only rule + signature verification rules, and the mini-app
  submission lifecycle (immutable slug, permission-minimal manifest,
  bridge surface, launch token, publish checklist). Honest about scope:
  user-facing login UI is Wave 1.5, gated integrations fail closed, E2E
  is Wave 2.
- Until the site is hosted, `docs/dev-setup.md` stays the contributor
  entry point and `docs/openapi.yaml` the REST reference (the SSOT gates
  above guarantee they can't drift).
