<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Developer webhooks (G2)

Sutra delivers platform events to developers' own HTTP endpoints through a
self-hosted [Svix](https://www.svix.com) server (`svix/svix-server`, MIT,
pinned in `docker-compose.yml`). Svix owns the hard parts — per-endpoint
HMAC signatures, the delivery retry schedule, and the attempt log. The
Sutra API only maps users to Svix *applications* (one app per user,
`user_<id>` — underscore, because Svix UIDs must match
`^[a-zA-Z0-9\-_.]+$` and reject colons), registers endpoints on them,
and publishes events to them.

## The model

- **One Svix app per Sutra user.** `POST /v1/webhooks/endpoints` and friends
  always act on the caller's own `user_<id>` app. A developer can never see
  or touch another user's endpoints — the app UID is derived from the
  verified OIDC subject, never from request input.
- **Endpoints are delivery targets** (your HTTPS URL). Register as many as
  you need; each gets its own signing secret.
- **Events are typed JSON.** Platform events (below) plus anything you
  publish yourself via `POST /v1/webhooks/events`.

## Platform event catalog

| Event type            | When it fires                                              | Payload                                                      |
|-----------------------|------------------------------------------------------------|--------------------------------------------------------------|
| `order.created`       | A checkout writes an order (one per seller)                | The order event envelope (`order_id`, `buyer_id`, `seller_id`, `status`, `total_paise`, …) |
| `order.status_changed`| Every order status transition (confirmed → shipped → delivered …) | Same envelope, new `status`                            |
| `live:started`        | A host creates a live room                                 | `room_id`, `room_name`, `title`, `kind`, `host_user_id`, `started_at` |
| `live:ended`          | A room ends (host, SFU webhook, or crash-host reconciliation) | Same, plus `ended_at`                                     |
| `shipment:update`     | A shipment's status or COD money changes                   | `shipment_id`, `order_id`, `status`, tracking fields         |

`order.*` events go to **both** the buyer's and the seller's app (each
receives the same event under their own app). `live:*` goes to the host's
app. `shipment:update` goes to the buyer's app.

**Honest scope note:** `shipment:update` is wired at the emission seam
(`realtime.PublishShipmentUpdate`), but the logistics provider webhook
ingress that produces shipment updates lands with the real logistics
integration (post-investors; the logistics gateway is a fail-closed stub
today — see `docs/logistics.md`). The contract above is final; the event
source is what is still gated.

Delivery is **at-least-once and best-effort**: the database rows are the
source of truth; webhooks are a notification hint. Event IDs are
deterministic per (event, entity) — e.g. `live-started:<room_id>` — so a
crash-retry re-publishes the same ID and Svix dedupes instead of
double-delivering. Your consumer should still dedupe on the event ID.

## Registering an endpoint

```bash
curl -X POST $API/v1/webhooks/endpoints \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"url":"https://your-app.example.com/sutra-webhooks",
       "description":"order notifications",
       "filter_types":["order.created","order.status_changed"]}'
# → 201 {"id":"ep_...","url":"...","filterTypes":[...],"secret":"whsec_..."}
```

**The `secret` is shown exactly once** — in this response. Svix never
returns it again and the Sutra API never stores a copy. Save it now; to
rotate, delete the endpoint and create a new one.

- `filter_types` is optional; omit it to receive every event type.
- `GET /v1/webhooks/endpoints` lists your endpoints (secrets never shown).
- `DELETE /v1/webhooks/endpoints/{id}` removes one (idempotent — deleting
  a gone endpoint is still 204).
- Endpoint URLs must be `http(s)`; localhost is accepted in dev. In
  production, register public URLs (the Svix SSRF guard blocks private
  IPs unless explicitly whitelisted — the dev compose file whitelists the
  docker subnets for the smoke canary only).

## Publishing your own events

```bash
curl -X POST $API/v1/webhooks/events \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"event_type":"inventory.low","payload":{"sku":"KURTA-RED-M","qty":3}}'
# → 202 {"id":"msg_...","event_type":"inventory.low"}
```

`event_id` is optional; when omitted the API mints one. Supply your own to
make re-publishes idempotent — Svix returns the **original** message for a
repeated event ID instead of delivering twice. Event types must match
`[A-Za-z0-9][A-Za-z0-9._:-]{0,127}`.

## Verifying signatures

Every delivery carries three headers:

- `svix-id` — the message ID
- `svix-timestamp` — unix seconds when Svix sent it
- `svix-signature` — one or more space-separated signatures, each
  `v1,<base64>`

The `v1` signature is:

```
base64( HMAC-SHA256( secret,  svix-id + "." + svix-timestamp + "." + raw_body ) )
```

where `raw_body` is the exact request bytes. Verification steps:

1. Reject if `abs(now - svix-timestamp) > 300` seconds (replay window).
2. Compute the expected signature with your endpoint secret.
3. Compare against **each** `v1,` signature with a constant-time compare;
   accept if any matches. (Svix sends several during secret rotation.)

Pseudocode:

```python
import base64, hashlib, hmac, time

def verify(secret: str, headers, body: bytes) -> bool:
    msg_id = headers["svix-id"]
    ts = headers["svix-timestamp"]
    if abs(time.time() - int(ts)) > 300:
        return False
    expected = base64.b64encode(hmac.new(
        secret.encode(), f"{msg_id}.{ts}.".encode() + body,
        hashlib.sha256).digest()).decode()
    return any(
        sig == expected
        for part in headers["svix-signature"].split(" ")
        for sig in [part[3:] if part.startswith("v1,") else ""]
        if sig
    )
```

## Retries and the delivery log

Retries are Svix's job: failed deliveries (non-2xx, timeouts, DNS errors)
are retried on an exponential-backoff schedule configured on the Svix
server (see the server's configuration reference). The Sutra API never
retries itself.

The attempt log is the ground truth — poll it after publishing:

```bash
curl "$API/v1/webhooks/deliveries?message_id=msg_..." \
  -H "Authorization: Bearer $TOKEN"
# → 200 {"message_id":"msg_...","attempts":[
#      {"id":"att_...","status":"success","responseStatusCode":200,
#       "timestamp":"2026-10-19T...Z"}]}
```

Attempt `status` is a Svix wire value (`success`, `pending`, `fail`,
`sending`), passed through verbatim so you can match the Svix docs.

## Failure modes

| Situation | Behavior |
|---|---|
| Webhooks unconfigured (`SVIX_API_TOKEN` empty) | Routes unregistered (404); platform-event hooks no-op |
| Bad `SVIX_API_TOKEN` | 500 `webhooks_misconfigured` (logged server-side; the token never leaks) |
| Svix server unreachable | 502 `webhooks_upstream`; platform-event publishes dropped with a warning (best-effort) |
| Endpoint URL invalid | 422 `invalid_endpoint_url` |
| Bad event type | 422 `invalid_event_type` |
| Unknown endpoint/message | 404 `webhook_not_found` |
