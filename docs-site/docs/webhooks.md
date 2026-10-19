<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

---
sidebar_position: 3
---

# Webhooks

Sutra delivers platform events to your own HTTPS endpoints through a
self-hosted Svix server. The model: **one Svix app per Sutra user**
(`user:<id>`); you register delivery targets on it; Svix signs every
delivery and retries failures; a delivery log shows what happened.

The deep contract — event catalog, emission semantics, the full signature
verification code — lives in
[`docs/webhooks.md`][contract] in the repo, kept in sync with the code.

## The 5-minute path

1. Get a bearer token ([onboarding](./onboarding.md#step-1-get-an-app-key)).
2. Register an endpoint:
   `POST /v1/webhooks/endpoints` with your HTTPS URL
   ([spec][endpoints]).
3. Svix shows you the endpoint's **signing secret** once — store it, you
   won't see it again in the API (listing endpoints never returns secrets).
4. Verify signatures on every delivery (see below), return 2xx fast, and
   dedupe on the event ID.
5. Watch `GET /v1/webhooks/deliveries` to confirm real deliveries arrive.

## Verifying signatures

Every delivery carries three headers:

- `svix-id` — the message ID (dedupe key)
- `svix-timestamp` — unix seconds when Svix sent it
- `svix-signature` — one or more space-separated signatures, each
  `v1,<base64>`

The `v1` signature is:

```
base64( HMAC-SHA256( secret,  svix-id + "." + svix-timestamp + "." + raw_body ) )
```

**Rules that will save you an incident:** compute over the *raw* request
body (before any JSON parsing rewrites whitespace); reject timestamps
older than 5 minutes (replay protection); compare with a constant-time
compare (`hmac.compare_digest`); if none of the `v1` signatures match,
drop the delivery — never process unsigned events. The full verification
snippet and the retry/delivery-log contract are in
[`docs/webhooks.md`][contract].

[endpoints]: https://github.com/mintzocloud-coder/sutra/blob/main/docs/openapi.yaml
[contract]: https://github.com/mintzocloud-coder/sutra/blob/main/docs/webhooks.md
