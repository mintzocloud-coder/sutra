# Business integrations (H2)

WhatsApp Cloud API (direct — no BSP), the Tally on-premise bridge, and Zoho
Books, for the Wave-5 business tier. All three ship as fail-closed stubs:
the seams are wired (config → constructor → boot validation) but no
integration can act until its post-investors onboarding completes. The
adapters live in `services/api/internal/integrations`; the boot
fail-fast is in `cmd/api/main.go` next to the payments/logistics seams.

## Design

- One seam per integration, same house contract as `internal/payments`
  and `internal/logistics`: only `"stub"` (and the empty default) boots
  today; any real provider name refuses to boot with a typed error; every
  stub method returns `*ErrIntegrationDisabled` — an explicit error, never
  a silent no-op.
- Credentials are env-loaded (`WHATSAPP_*`, `TALLY_*`, `ZOHO_*`), never
  logged (not even the App ID / client id), never committed. Set-but-ignored
  credentials produce a loud boot WARN, mirroring the payments seam.
- Real provider implementations keep the same interfaces and are safe to
  call concurrently. The stubs keep failing closed after onboarding, so
  `"stub"` stays valid for tests and dev.

## WhatsApp Cloud API — direct, no BSP

The seam is `WhatsAppProvider`: `SendText` + `SendTemplate` (approved
templates, per Meta policy). Messages go through Meta's Graph API
(v22.0+) with a WhatsApp Business Account phone number id — there is
intentionally no business-service-provider middleman.

Real code already in the seam (not stubbed): `VerifyWebhookSignature`
validates Meta's `X-Hub-Signature-256` (HMAC-SHA256 of the raw body with
the app secret) — pure crypto, unit-tested against a known vector. It
fails closed on an unset secret: no callback is ever accepted.

Post-investors onboarding checklist:
1. Create the Meta App (type: Business) in the company Meta account;
   add the WhatsApp product.
2. Register a WhatsApp Business Account + business phone number; copy
   the phone number id and generate a permanent system-user access token
   (scoped to `whatsapp_business_messaging` + `whatsapp_business_management`).
3. Set `WHATSAPP_PROVIDER=cloud-api`, `WHATSAPP_PHONE_NUMBER_ID`,
   `WHATSAPP_ACCESS_TOKEN` from the secret store (never in repo).
4. Register the webhook URL (`/internal/whatsapp/webhook`, H2 part 2) with the
   verify token; set `WHATSAPP_WEBHOOK_VERIFY_TOKEN` (the Meta app secret).
5. Get the marketing/utility templates approved in the WhatsApp Manager;
   free-form text works only inside the 24h customer-service window.
6. Record buyer opt-in (DLT-style consent register) before first send —
   the DPDP duties live May 2027; unsolicited promotional messages are
   a Meta ban + legal risk.

## Tally on-premise bridge

Tally Prime exposes its XML-over-HTTP API only on the merchant's own
machine, so there is no hosted "Tally API" to call. The bridge design:

- The seller installs a small local **bridge agent** (company-signed
  binary) on the Tally machine. The agent listens on a TLS localhost
  port, authenticates jobs from the Sutra API with a shared HMAC secret
  (`TALLY_BRIDGE_SECRET`), and posts vouchers to Tally's XML API.
- The seam is `TallyBridge.PostVoucher` (sales + journal vouchers;
  amounts in integer paise; `ReferenceNo` = our order id as the dedupe
  key, so retries never double-post). The agent ACKs jobs
  asynchronously — Tally-side posting is its job, not the API's.
- Money convention matches `internal/catalog`: integer paise everywhere.

Post-investors deployment checklist:
1. Build + sign the bridge agent; publish the installer with a
   merchant setup guide.
2. Seller onboarding: agent installed → shared secret exchanged out of
   band → `TALLY_PROVIDER=bridge`, `TALLY_BRIDGE_URL`,
   `TALLY_BRIDGE_SECRET` registered for the seller (per-seller wiring
   is H2 part 2 scope).
3. Smoke: post a test journal voucher, verify in Tally's daybook, void it.

## Zoho Books (default accounting ledger)

The seam is `ZohoBooksProvider`: `CreateContact` + `CreateInvoice`
(INR-only, integer paise, `ReferenceNo` = our order id as the dedupe
key). Zoho is the default ledger for Indian merchants; Tally stays the
bridge for merchants who already run Tally on-premise.

Post-investors onboarding checklist:
1. Create the company's Zoho developer account; register a Zoho
   self-client with scopes `ZohoBooks.settings.CREATE`,
   `ZohoBooks.contacts.CREATE`, `ZohoBooks.invoices.CREATE`.
2. Generate the refresh token (offline access) against the company's
   INR Books organization; record the organization id (`ZOHO_ORG_ID`).
3. Set `ZOHO_PROVIDER=books`, `ZOHO_CLIENT_ID`, `ZOHO_CLIENT_SECRET`,
   `ZOHO_REFRESH_TOKEN`, `ZOHO_ORG_ID` from the secret store (never in
   repo). The real adapter must refresh the access token itself and
   never log tokens.
4. Smoke: create a test contact + draft invoice, void both.

## What's not in this part

- No send routes on the API yet — the H2 part-2 wiring consumes these
  seams for order notifications (WhatsApp), accounting export (Tally /
  Zoho), plus `/internal/whatsapp/webhook` inbound and the per-seller Tally
  bridge registration. The stubs being wired at boot is deliberate:
  provider-name typos already fail fast today.
- E2E encryption stays Wave 2; gated items stay gated (identity
  fail-closed, real payments post-incorporation).

## Routes (H2 part 2a — shipped)

- `POST /v1/integrations/whatsapp/send` (behind RequireAuth, seller-scoped):
  exactly one of `text` (≤4096 chars) or `template` (name/language allowlist,
  body/header/button components) to one E.164 recipient. Unknown fields are
  rejected; invalid grammar → 400/422; a caller with no seller account →
  403 `not_a_seller`. Until the post-investors Meta onboarding the stub
  fails closed: 503 `whatsapp_unavailable` + Retry-After on every send —
  the API never implies a message went out. Post-investors the real
  provider returns 202 with the Meta `wamid.*` message id.
- `GET /internal/whatsapp/webhook` — Meta's subscription handshake:
  `hub.mode=subscribe` + the exact `hub.verify_token` (from
  `WHATSAPP_WEBHOOK_VERIFY_TOKEN`) echoes `hub.challenge` verbatim. Fails
  closed (401) when the verify token is unset — no secret, no handshake.
- `POST /internal/whatsapp/webhook` — the signed event callback. The
  `X-Hub-Signature-256` HMAC is verified over the raw body with the real
  `VerifyWebhookSignature` before anything else; missing/bad signature →
  401, foreign `object` → 400. Wave 1 acknowledges (200) and logs the
  event kinds (messages vs statuses) — the 200 never implies storage;
  inbound message processing is Wave 2.

Note: the webhook lives at `/internal/*` (public by necessity — Meta
calls it), not under `/v1/*`, which is behind RequireAuth. The OpenAPI
coverage gate tracks it as an internal route, same as
`/internal/livekit/webhook`.

## Routes (H2 part 2b — shipped)

- `POST /v1/integrations/tally/vouchers` (behind RequireAuth,
  seller-scoped): posts a `sales`/`journal` voucher to the seller's own
  on-premise bridge agent. Amounts are integer paise (positive = debit,
  negative = credit); the voucher grammar is validated up front —
  calendar-valid YYYYMMDD, non-empty `reference_no`, 1–100 ledger lines,
  non-zero per-line amounts capped at ₹10 crore, no duplicate ledgers,
  and the lines must balance to zero. Invalid grammar → 400
  `invalid_request`; no seller account → 403 `not_a_seller`. Per-seller
  wiring: the bridge is resolved from the `seller_tally_bridges` registry
  (migration 0016) — one row per seller, `seller_id` PK, FK-cascade on
  seller delete, bridge secrets deliberately NOT stored. No registered
  bridge → 503 `tally_not_registered`; registered but only the stub
  wired → 503 `tally_unavailable`; Retry-After on both. Post-investors
  the real per-seller bridge returns 202 with the bridge job id.
