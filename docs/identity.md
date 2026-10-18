# Sutra Identity — D1

How sign-in and token verification work in the Sutra backend, and what is
deliberately *not* wired yet.

## The shape of it

```
                    ┌─────────────┐
  user ──browser──▶ │ Ory Kratos  │  self-service identity store
                    │ :4433/:4434 │  (email+password today; schema in
                    └──────┬──────┘   services/identity/kratos/)
                           │ identities feed Hydra's login/consent
                           ▼ (Wave 1.5 — not yet)
                    ┌─────────────┐
                    │ Ory Hydra   │  OAuth2 / OIDC issuer
                    │ :4444/:4445 │  RS256 JWT access tokens
                    └──────┬──────┘
                           │  JWKS + iss/aud checks (A2 path, unchanged)
              ┌────────────┴────────────┐
              ▼                         ▼
     Go API (:8081)            Elixir chat (:4000)
     RequireAuth                UserSocket.connect
```

**Design decision: Hydra issues, everyone else verifies.** The Go API and
the chat service share one verification contract (see
`docs/auth-contract.md`): RS256 pinned, JWKS fetched from the issuer with
15-minute cache + refresh-on-unknown-kid, `exp`/`iss`/`aud` enforced with
60s leeway. D1 adds **no new auth code** — it stands up the real issuer
(Hydra) that the A2 path was built for. Before D1 the issuer was a
hypothetical; now `docker compose up` boots it.

## Services (docker-compose.yml)

| Service         | Image (pinned)        | Ports       | Notes                                      |
|-----------------|-----------------------|-------------|--------------------------------------------|
| `hydra-migrate` | `oryd/hydra:v26.2.0`  | —           | one-shot: `migrate sql -e --yes`           |
| `hydra`         | `oryd/hydra:v26.2.0`  | 4444 / 4445 | `serve all --dev`; public / admin           |
| `kratos-migrate`| `oryd/kratos:v26.2.0` | —           | one-shot: `migrate sql -e --yes --config …` |
| `kratos`        | `oryd/kratos:v26.2.0` | 4433 / 4434 | `serve --config …`; public / admin          |

Hydra and Kratos each get their OWN Postgres database (`hydra` and
`kratos`, created by `docker/postgres/initdb/init-identity-dbs.sh` on
first volume init) — they MUST NOT share the `sutra` database with
each other or the API: both Ory products create a `networks` table
(plus other same-named relations), so whoever migrates second dies
with `ERROR: relation "networks" already exists` (this exact failure
broke CI smoke). Image tags are pinned — Dependabot/Renovate can
bump them; never use `latest` for identity infrastructure.

Kratos config lives in `services/identity/kratos/kratos.yml` (mounted
read-only) with the identity schema in `identity.schema.json`
(email+password today, optional E.164 `phone` trait reserved for the OTP
future — it is *not* a login identifier yet).

### First boot

```bash
docker compose up -d hydra kratos   # pulls in the migrate one-shots first
curl -s http://localhost:4444/.well-known/openid-configuration | head -5
curl -s http://localhost:4434/health/alive   # "ok" — admin port; /health/* is admin-API only
```

`scripts/smoke.sh` does all of this and then proves the full loop (below).

## The token loop (what smoke.sh proves)

The acceptance test for D1 is a real Hydra-issued JWT reaching
`GET /v1/me`:

1. `smoke.sh` creates an OAuth2 client in Hydra with the
   `urn:ietf:params:oauth:grant-type:jwt-bearer` grant type, which makes
   Hydra mint **JWT** access tokens (opaque by default otherwise).
2. Client-credentials grant → `access_token` (RS256, `iss=http://hydra:4444`,
   `aud=["sutra-api"]`).
3. The script recreates the `api` container **without** `AUTH_BYPASS`
   (the bypass skips verification entirely, so leaving it on would prove
   nothing) and asserts:
   - `GET /v1/me` with the Hydra JWT → **200**, `subject == "sutra-smoke"`
     (client-credentials `sub` is the client id; JIT provisioning in A4
     creates the row);
   - `GET /v1/me` without a token → **401** (bypass confirmed off).
4. The API fetched the signing keys from
   `http://hydra:4444/.well-known/jwks.json` over the compose network —
   the exact A2 code path, now against a real issuer.

If Hydra is down, the verifier fails closed: API returns 401s, chat
socket connects are refused. There is no "trust me" mode.

## Login & consent UI — not yet

Hydra's `URLS_LOGIN` / `URLS_CONSENT` point at placeholder Kratos browser
URLs. No login/consent UI ships in Wave 1, so the **authorization-code
flow is not exercisable yet** — only `client_credentials` (machine to
machine) is. Wiring a Kratos-backed login app and pointing Hydra at it is
the Wave 1.5 step; Kratos itself is already running so that work starts
from a live identity store, not a design doc.

## Phone OTP — the DLT-gated path

Phone-OTP login ships in D1 as an **adapter interface with a fail-closed
stub**, not as working SMS:

- `services/api/internal/otp` — `Sender` interface (`Send(ctx, phone,
  code)`), `StubSender` (every send returns the typed
  `*ErrSMSSendingDisabled`), `NewSender(provider)` constructor,
  `GenerateCode()` (crypto/rand, 6 digits), `ValidatePhone()` (E.164
  syntax).
- `OTP_PROVIDER` env (default `"stub"`, set explicitly in compose). The
  API constructs the sender at startup and **refuses to boot** on an
  unknown provider — a misconfigured gateway name can never silently
  drop OTPs.
- The stub's `Send` never pretends delivery: it returns an error naming
  the reason, so no test, log line, or UI can mistake "stubbed" for
  "sent".

### Why SMS is gated

Sending commercial SMS in India legally requires **DLT registration**:
principal-entity registration on a DLT portal (e.g. Jio/Vodafone-Idea
through the telemarketer), at least one approved sender **header**, and
approved message **templates** — and all of that presupposes an
incorporated entity, which Sutra does not have yet. Wiring a real gateway
before that would be unlawful, not just premature.

### Post-incorporation checklist (when the gate opens)

1. Incorporate; register the principal entity on a DLT portal.
2. Get sender header(s) + OTP message template(s) approved.
3. Contract an SMS gateway (route: DLT-scrubbed transactional).
4. Implement `otp.Sender` against the gateway (respect DLT template-id
   and header params; never log codes), register it in `NewSender`
   under `OTP_PROVIDER=<name>`, add `OTP_*` env config.
5. Enable Kratos's code/passwordless method for the `phone` trait and
   document the template/header IDs here.

Until every step is done, `OTP_PROVIDER` stays `"stub"` and Kratos's OTP
method stays disabled — enabling it with the stub would fake deliveries,
which the fail-closed design forbids.

## Production notes

- **Secrets:** `SECRETS_SYSTEM` (Hydra, encrypts its DB — must be stable
  across restarts) and Kratos's `secrets.cookie` / `secrets.cipher`
  (32+ chars each) are **dev placeholders** in this repo. Generate real
  ones for production and inject them outside version control
  (`docs/production.md` pre-flight checklist covers this).
- `serve all --dev` is dev-only; production serves Hydra behind TLS
  termination with an explicit config file.
- `AUTH_BYPASS=1` is never set in production (both services log a loud
  WARN if it is).
- Kratos courier (email) is unconfigured: no verification/recovery
  emails are sent. Wire a real mailer post-incorporation alongside SMS.

## Deliberately out of scope

- **E2E encryption** is Wave 2 — nothing here promises it (see
  `docs/auth-contract.md`).
- **User-facing login UI / authorization-code flow** — Wave 1.5.
- **Real SMS/DLT** — post-incorporation, fail-closed until then.
