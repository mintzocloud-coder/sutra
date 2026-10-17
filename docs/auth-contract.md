# Auth contract — one identity across the API and chat services

**Status:** Accepted (B2, 2026-10-17) · **Applies to:** `services/api` (Go), `services/chat` (Elixir)

Sutra has one identity issuer (Ory Hydra in production). Both services
verify the same OIDC access tokens under the same rules, so a token that
authenticates a REST call also authenticates a websocket — no
service-specific token formats, no second login.

## How a client authenticates

- **REST (`services/api`):** `Authorization: Bearer <access token>` header.
  Enforced by `internal/middleware.RequireAuth`; the subject lands in the
  request context.
- **Websocket (`services/chat`):** `token` connect param —
  `new Socket("/socket", {params: {token: "<access token>"}})`.
  Enforced in `SutraChatWeb.UserSocket.connect/3`; the subject becomes
  `socket.assigns.user_id`. No token (or an invalid one) → the connect is
  refused and no channel is ever joined. There is no anonymous access.

## Verification rules (identical in both services)

| Rule | Detail |
|---|---|
| Algorithm | Pinned to **RS256**. `alg: none` and every other algorithm are rejected — this pin is what defeats algorithm-confusion attacks. |
| Key selection | `kid` header required; the signature is checked against that key only. |
| Key source | The issuer's JWKS document at `<issuer>/.well-known/jwks.json`. |
| Key cache | 15-minute TTL; a token with an unknown `kid` triggers one immediate refresh (key rotation) before failing. Keys are fetched lazily — both services boot fine before the issuer is up. |
| Expiry | `exp` enforced with 60s clock-skew leeway (a token expiring within 60s counts as expired, on both sides). |
| Issuer | `iss` must equal the configured issuer. |
| Audience | `aud` (string or list) must contain the configured audience. |
| Subject | `sub` required — it is the user's identity in channels, presence, and persisted messages. |

Error behavior differs only in transport: the API returns the JSON error
envelope (`401`), the socket refuses the connect (`:error`).

## Configuration (one place, both services)

| Env var | Default | Meaning |
|---|---|---|
| `HYDRA_ISSUER` | `http://hydra:4444` | OIDC issuer; JWKS fetched from `<issuer>/.well-known/jwks.json` |
| `JWT_AUDIENCE` | `sutra-api` | Expected `aud` claim |
| `AUTH_BYPASS` | unset | `1` disables verification — **local dev only**, loudly logged at startup |
| `AUTH_BYPASS_SUBJECT` | `dev-user` | Subject stamped on every request/socket in bypass mode |

Both services read the same variables, so identity is configured once.

## Implementation map

- Go: `services/api/internal/oidc/jwks.go` (stdlib-only verifier) +
  `services/api/internal/middleware/auth.go` (`RequireAuth`).
- Elixir: `services/chat/lib/sutra_chat/auth/token.ex` (pure verifier,
  stdlib `:crypto`/`:public_key` + Jason, no JWT library) +
  `services/chat/lib/sutra_chat/auth/token_verifier.ex` (GenServer JWKS
  cache; serializes refreshes so connects never thundering-herd the
  issuer) + `SutraChatWeb.UserSocket.connect/3`.

Deliberately no third-party JWT dependency on either side — the
dependency surface stays minimal and fully AGPL-compatible.
