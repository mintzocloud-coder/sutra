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
4. Register the webhook URL (`/v1/webhooks/whatsapp`, H2 part 2) with the
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
  Zoho), plus `/v1/webhooks/whatsapp` inbound and the per-seller Tally
  bridge registration. The stubs being wired at boot is deliberate:
  provider-name typos already fail fast today.
- E2E encryption stays Wave 2; gated items stay gated (identity
  fail-closed, real payments post-incorporation).
