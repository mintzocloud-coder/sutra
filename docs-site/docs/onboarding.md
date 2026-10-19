<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

---
sidebar_position: 2
---

# Developer onboarding guide

One linear walkthrough from zero to a working webhook and a submitted
mini-app. Assumes you have the Sutra stack running locally
([`docs/dev-setup.md`][dev-setup]); every command below is written
against that dev setup. Production hardening lives in
[`docs/production.md`][production].

## Step 0 — The mental model

- **You are a developer identity.** Every API call carries a bearer token
  for *your* Sutra user. The platform derives everything from that
  token's subject — your webhook app (`user:<id>`), your mini-apps,
  your installs. There is no separate "developer portal account" to
  create: register your OAuth client and you're in.
- **The spec is the contract.** If the code accepts it but the spec
  doesn't document it (or vice versa), that's a CI-blocking bug. Generate
  a typed client instead of hand-writing HTTP:
  `npx --yes openapi-typescript@7 docs/openapi.yaml -o sutra-api.ts`.

## Step 1: Get an app key

Identity is OIDC: Ory Hydra issues RS256 JWTs, the API and chat services
verify them through a shared JWKS path (`docs/identity.md`). In dev, mint
an OAuth2 client on Hydra's admin port and take the client-credentials
flow:

```bash
# Inside the hydra container (compose service name: hydra, admin port 4445).
# The jwt-bearer grant type is what makes Hydra mint JWT (not opaque)
# access tokens; client_secret_basic is the token-endpoint auth method.
docker compose run --rm hydra hydra create client \
  --endpoint http://hydra:4445 \
  --id my-dev-app \
  --secret "$(openssl rand -hex 24)" \
  --grant-type client_credentials \
  --grant-type urn:ietf:params:oauth:grant-type:jwt-bearer \
  --response-type token \
  --scope openid \
  --audience sutra-api \
  --token-endpoint-auth-method client_secret_basic \
  --name my-dev-app
```

Mint a JWT access token against Hydra's public port (4444) and call the
API (the API runs on host port 8081 in dev compose):

```bash
TOKEN=$(curl -sf -u "<client_id>:<client_secret>" \
  --data-urlencode 'grant_type=client_credentials' \
  --data-urlencode 'audience=sutra-api' \
  http://localhost:4444/oauth2/token | python3 -c \
  'import sys,json; print(json.load(sys.stdin)["access_token"])')
case "$TOKEN" in
  eyJ*) : ;; # JWT-shaped — good
  *) echo "warning: token is not JWT-shaped — re-check the jwt-bearer grant flag" ;;
esac

curl -H "Authorization: Bearer $TOKEN" http://localhost:8081/v1/me
# → 200 {"id":"<uuid>","subject":"my-dev-app","display_name":null,...}
```

The first call **just-in-time provisions** your user row — no signup API
needed. Keep the secret like a password; in production it lives in a
secret manager, never in your repo.

**Honest scope note:** client-credentials is the dev path. User-facing
login (Kratos self-service + the OAuth2 login/consent UI) is Wave 1.5 —
`docs/identity.md` has the design. Don't build a production auth UX on
client-credentials alone.

## Step 2 — Learn the conventions

Three conventions keep you out of trouble on every endpoint:

1. **Error envelope.** Every non-2xx is
   `{"error":{"code","message","request_id"}}` — log the `request_id`
   with any bug report.
2. **Idempotency and retries.** Sends carry a `client_msg_id` you choose;
   retries with the same ID are deduped server-side (the message path even
   tells you `deduped: true` when a retry hit an existing row). Retry
   safely on timeouts; never change the payload on a retry.
3. **Pagination is keyset, never OFFSET.** Feeds, followers, directories,
   and histories return `next_cursor`; pass it back. Cursors are opaque —
   don't construct or parse them.

Also respect `429` + `Retry-After` (per-IP rate budgets), and send
`X-Request-ID` on your calls — the platform echoes it, which makes
support conversations ten times faster.

## Step 3 — Register a webhook endpoint

Platform events (`order.created`, `order.status_changed`, `live:started`,
`live:ended`, `shipment:update`) go to HTTPS endpoints you register on
your own `user:<id>` Svix app:

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"url":"https://example.com/sutra/events","description":"order events"}' \
     http://localhost:8081/v1/webhooks/endpoints
# → 201 {"id":"ep_...","url":"https://example.com/sutra/events",...}
# The signing secret is returned ONCE. Store it now — listings never
# include it again.
```

Then:

- **Verify signatures on every delivery** — headers `svix-id`,
  `svix-timestamp`, `svix-signature`; the `v1` signature is
  `base64(HMAC-SHA256(secret, svix-id + "." + svix-timestamp + "." + raw_body))`.
  Compute over the *raw* body, reject timestamps older than 5 minutes,
  use a constant-time compare, dedupe on the message ID, return 2xx
  fast. The full snippet and the event catalog are in
  [`docs/webhooks.md`][webhooks].
- **Watch deliveries:** `GET /v1/webhooks/deliveries` shows Svix's
  attempt log (signatures, retries, failures) — your first stop when an
  event "didn't arrive".
- Publish your own events for testing with `POST /v1/webhooks/events`
  before waiting on real platform traffic.

Delivery is at-least-once and best-effort — the database rows are the
source of truth; webhooks are a notification hint.

## Step 4 — Submit a mini-app

1. **Register** (status starts as `draft`):
   ```bash
   curl -s -H "Authorization: Bearer $TOKEN" \
        -H "Content-Type: application/json" \
        -d '{"name":"My Mini App","slug":"my-mini-app",
             "entry_url":"https://example.com/miniapp",
             "icon_url":"https://example.com/icon.png",
             "manifest":{"permissions":["media.upload","haptics"]}}' \
        http://localhost:8081/v1/mini-apps
   # → 201 {"id":"<uuid>","slug":"my-mini-app","status":"draft",...}
   ```
   The slug is **immutable and first-come** — choose it like a domain
   name. Permissions must be a subset of the platform allowlist.
2. **Build against the bridge.** Your code runs in the platform frame;
   the only door out is the `SutraMiniApp` JS bridge — a *closed* surface
   of 14 dotted methods, each gated on a manifest permission
   (`init.getData` is permission-free). Unknown methods are loud errors.
   Full API table: [`docs/mini-apps.md`][mini-apps].
3. **Launch-token flow.** When a user opens your app, the client calls
   `POST /v1/mini-apps/{id}/launch` and hands you a signed launch token
   (`v1.<b64>.<b64>`, 5-minute TTL) plus init data (`user`, `app`,
   `theme`, the token). Verify the HMAC server-side before trusting it.
4. **Publish.** `POST /v1/mini-apps/{id}/publish` moves draft → published
   and makes the app installable; `GET /v1/mini-apps` lists the directory.
   Suspended apps can't be installed; archived is terminal.
5. Run the **submission checklist** in the [mini-apps guide](./mini-apps.md#submission-checklist)
   before asking for review.

## Step 5 — Ship responsibly

- **Rate limits:** per-IP token buckets; watch for `429` with
  `Retry-After` and back off exponentially.
- **Secrets hygiene:** `AUTH_BYPASS=1` exists in dev compose — it is a
  dev-only hatch that skips JWT verification, logged as a loud WARN.
  It must never be set in production (`docs/production.md` pre-flight).
- **Gated integrations:** real SMS/DLT, payment-aggregator onboarding,
  and WhatsApp/Tally/Zoho credentials are post-incorporation /
  post-investors and **fail closed** — the stubs exist with documented
  contracts; don't ship against them expecting real delivery.
- **No E2E promises:** end-to-end encryption is Wave 2. Don't tell your
  users their messages are end-to-end encrypted. They aren't yet.
- **SDK:** regenerate from `docs/openapi.yaml` whenever the API moves —
  the CI SDK-stub check guarantees the spec stays machine-consumable.

Questions about the platform surface go to the GitHub issue tracker
(tag `dev-platform`); pull requests against `docs/` are welcome — just
keep the spec, the code, and the docs in agreement.

[dev-setup]: https://github.com/mintzocloud-coder/sutra/blob/main/docs/dev-setup.md
[production]: https://github.com/mintzocloud-coder/sutra/blob/main/docs/production.md
[webhooks]: https://github.com/mintzocloud-coder/sutra/blob/main/docs/webhooks.md
[mini-apps]: https://github.com/mintzocloud-coder/sutra/blob/main/docs/mini-apps.md
