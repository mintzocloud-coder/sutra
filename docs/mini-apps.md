# Mini-app runtime (G3)

Sutra mini-apps are Telegram-model sandboxed apps: the developer's own
HTML/JS running inside a platform-owned frame (iframe on web, WebView on
mobile), with exactly one door into the device — the `SutraMiniApp` JS
bridge. This document is the runtime contract both sides implement:
clients (web/mobile) enforce the bridge gating; developers build
against the init data and the method surface below.

## Trust model

The frame's code is **untrusted** — it ships from the developer's own
hosting, not from us. The platform trusts exactly two things:

1. **The registry row** — app id, slug, declared permission manifest
   (`POST /v1/mini-apps`, `services/api/internal/miniapp`). The manifest
   is the allowlist; the bridge denies any capability not declared in it.
2. **The launch token** — a 5-minute HMAC-SHA256 over
   `(user_id, app_id)`, minted at `POST /v1/mini-apps/{id}/launch`
   (G3 part 2). The client injects it into the frame as part of init
   data; the developer backend can verify attribution with the shared
   launch secret. It proves "user U launched app A at time T" and
   nothing more — it is not a session and carries no privileges on the
   Sutra API.

Everything else — DOM access, network to the developer's backend,
cookies — is isolated per app by the frame's sandbox attributes and
the (user, app)-scoped storage keys.

## Launch flow

1. User taps the app in the Sutra client (directory or their shelf).
2. The client calls `POST /v1/mini-apps/{id}/launch` (empty body).
   - The owner may launch in **any** status, including `draft` — this is
     how developers test before publishing.
   - Other users need the install edge; `draft` apps are never
     launchable by non-owners. `suspended` apps keep working for
     already-installed users; `archived` apps 404 everywhere.
   - No `MINIAPP_LAUNCH_SECRET` on the server → `503
     launch_unavailable` (fail-closed).
3. The client boots the frame at the app's `entry_url`, injecting init
   data (see below). The frame runs sandboxed: no direct Sutra API
   access, no raw device APIs.

## Init data

The client injects a JSON object before the frame's first script runs
(web: `window.SutraMiniApp.initData`; mobile WebView: same object via
the native bridge handshake). Shape:

```json
{
  "app":      {"id": "uuid", "slug": "groceries", "name": "Groceries", "version": "1.2.0"},
  "user":     {"id": "uuid"},
  "launch_token": "v1.<payload>.<hmac>",
  "theme":    {"scheme": "dark", "accent": "#7c3aed"},
  "locale":   "en-IN"
}
```

The `launch_token` is the HMAC token from step 2. If the developer's
backend needs to confirm a request really came from the Sutra client
(forged frames exist), it verifies the token with the launch secret:
`HMAC-SHA256(secret, "v1." + base64url(payload))` must equal the
signature, and `exp` must be in the future. Never accept a token older
than 5 minutes — treat it as replay, not identity.

## JS API surface

`window.SutraMiniApp` exposes the methods in
`services/api/internal/miniapp.BridgeMethods`. Every method maps to one
manifest permission; the client denies calls the manifest doesn't
declare (silent deny is forbidden — unknown method names error loudly
so developers catch typos in testing).

| Method | Permission | Notes |
|---|---|---|
| `init.getData` | — | init data above; always available |
| `storage.getItem` / `setItem` / `removeItem` | `storage` | platform KV scoped to (user, app); 5MB/app |
| `camera.capture` | `camera` | system capture sheet; no silent snapshot |
| `microphone.startListening` / `stopListening` | `microphone` | foreground + visible indicator |
| `geolocation.getPosition` | `geolocation` | one-shot; location disclosure sheet |
| `notifications.subscribe` | `notifications` | app push channel; OS prompt owned by client |
| `payments.initiate` | `payments` | platform payment sheet (UPI/cards); the frame never sees raw card data |
| `share.invoke` | `share` | OS share sheet |
| `clipboard.writeText` | `clipboard` | user-gesture required |
| `vibration.vibrate` | `vibration` | haptic pulse, client-capped |
| `biometric.authenticate` | `biometric` | frame learns pass/fail only |

The manifest permission list is the closed allowlist
(`BridgePermissions`); declaring anything else is rejected at
registration **and** denied at runtime. Adding a capability is a
platform release — client update ships first, registry allowlist second.

## Security rules (non-negotiable)

- `entry_url` / `icon_url` must be `http(s)` — `javascript:`, `data:`,
  and custom schemes are rejected at registration.
- The frame loads with sandbox attributes equivalent to
  `sandbox="allow-scripts allow-same-origin"` and **no**
  `allow-top-navigation`: the app cannot break out of the frame or
  navigate the host app.
- All bridge traffic goes through `postMessage` with explicit origin
  checks on both sides (client ↔ frame).
- Permissions are least-privilege and declared up front: the directory
  shows the app's permission list before the user installs.
- Revocation is instant: `suspend` stops new launches; `archive`
  deletes the install edges and 404s the app everywhere. Installed
  clients must re-check launch eligibility on every cold start.

## Developer checklist

1. `POST /v1/mini-apps` with name, slug, `entry_url`, and the
   `manifest.permissions` you actually need.
2. `POST /v1/mini-apps/{id}/launch` as the developer to preview the
   draft in the real frame.
3. Declare only the permissions you use — over-declaration looks
   suspicious in the directory and costs installs.
4. `POST /v1/mini-apps/{id}/publish` when the frame is ready.
5. Rotate nothing yourself: slug is immutable, launch tokens are
   minted per boot, and the manifest can be tightened with
   `PATCH /v1/mini-apps/{id}` at any time (loosening permissions
   re-prompts existing users on next launch).
