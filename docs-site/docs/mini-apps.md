<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

---
sidebar_position: 4
---

# Mini-apps

Mini-apps are Telegram-model sandboxed apps: **your** HTML/JS runs inside a
platform-owned frame (iframe on web, WebView on mobile), with exactly one
door into the device — the `SutraMiniApp` JS bridge. The frame's code is
untrusted; the bridge is the trust boundary.

The deep runtime contract — trust model, the closed bridge API table,
launch flow, init-data shape, and the non-negotiable security rules —
lives in [`docs/mini-apps.md`][contract] in the repo.

## The lifecycle at a glance

1. **Register** — `POST /v1/mini-apps`. You pick a `slug`
   (`[a-z0-9-]{3,40}`, **immutable**, first-come via the registry), an
   `entry_url`, and a `manifest` with your requested permissions. The
   app starts as `draft`.
2. **Develop against the bridge** — the bridge surface is *closed*: 14
   dotted methods, each mapped to a manifest permission (`init.getData`
   is permission-free). Your manifest must be a subset of the platform
   allowlist — unknown permissions are rejected at registration, unknown
   method names are loud client errors, not silent denies.
3. **Publish** — `POST /v1/mini-apps/{id}/publish` (draft → published).
   Suspended apps cannot be installed; archived is terminal.
4. **Install** — users `POST /v1/mini-apps/{id}/install` (published apps
   only, idempotent); the directory (`GET /v1/mini-apps`) lists published
   apps, cursor-paged.
5. **Launch** — `POST /v1/mini-apps/{id}/launch` returns an HMAC-signed
   launch token (`v1.<b64>.<b64>`, 5-minute TTL). The client verifies it
   and injects init data (`user`, `app`, `theme`, the token) before your
   code runs.

## Submission checklist

- Slug chosen carefully (it can never change; slugs are first-come).
- `entry_url` and `icon_url` are `https://` (no `javascript:`, no http).
- Manifest permissions ⊆ your app's actual bridge calls — request the
  minimum; over-scoped manifests are a rejection signal in review.
- App launches clean with **only** `init.getData` (prove it works with
  zero permissions first).
- Handle the bridge contract: every call is async; errors come back with
  reasons, not exceptions — surface them in your UI.
- Confirm `GET /v1/mini-apps/{id}` shows your published listing, and
  that an *uninstalled* user's launch is denied (403) — that is the
  privacy guarantee your users rely on.

[contract]: https://github.com/mintzocloud-coder/sutra/blob/main/docs/mini-apps.md
