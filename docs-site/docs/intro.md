<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

---
sidebar_position: 1
---

# What the developer platform is

Sutra's developer platform is how third parties build **on** Sutra, not
just **with** it:

- **REST API** — bearer-authenticated, versioned under `/v1/`, one
  standard JSON error envelope, per-IP rate budgets. The spec at
  [`docs/openapi.yaml`][spec] is the single source of truth: CI fails
  the build if code and spec drift (`docs/dev-platform.md`).
- **Webhooks** — your own HTTPS endpoints, registered on a per-user Svix
  app (`user:<id>`), with HMAC-signed deliveries and a delivery log.
  See the [webhooks guide](./webhooks.md).
- **Mini-apps** — sandboxed HTML/JS apps in the platform frame, with a
  permission-manifest JS bridge and HMAC launch tokens. See the
  [mini-apps guide](./mini-apps.md).
- **Realtime events** — Phoenix `user:<id>` topics for server→client push
  (`docs/realtime-events.md` in the repo).

Start with the [onboarding guide](./onboarding.md): one linear walkthrough
from zero to a working webhook and a submitted mini-app.

[spec]: https://github.com/mintzocloud-coder/sutra/blob/main/docs/openapi.yaml
