<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Realtime social events (D5)

The Go API publishes social events to the chat service; the chat service
fans them out on Phoenix `user:<subject>` topics. This document is the
contract both sides implement. Go publisher: `services/api/internal/realtime`.

## Endpoint

```
POST /internal/events          (chat service, same Phoenix endpoint)
Authorization: Bearer <CHAT_INTERNAL_TOKEN>
Content-Type: application/json
```

Request body:

```json
{
  "topics": ["user:<oidc-subject>", "user:<oidc-subject>"],
  "event": "post:new",
  "dedupe_key": "post:<post-uuid>",
  "payload": { "...": "..." }
}
```

Response: `202 Accepted` with `{"status":"accepted","event":...,"topics":N}`
on receipt (broadcast is async).
Error responses use the same JSON error envelope as the public API.

Auth: the token is compared in constant time against `CHAT_INTERNAL_TOKEN`
(chat `config/runtime.exs`). Missing/wrong token → `401`. The endpoint is
**never exposed to the public internet** — it lives on the internal compose
network / internal k8s service only. Token rotation: restart both services
with the new value; in-flight events may 401 during the window and are
best-effort anyway (see delivery below).

Validation (chat side, before any broadcast):

- `topics` non-empty; every topic matches `^user:[^:]+$`
- `event` is a known name (unknown → `422`)
- `dedupe_key` present and non-empty (required). The chat does NOT keep
  a server-side dedupe store in Wave 1 — the key exists so a future
  retrying publisher and Wave 2's persistent inbox stay idempotent.
  Wave 1 clients dedupe on the stable ids already in the payload
  (`post_id` for post:new; `follower_id` + the recipient topic for
  follower:new).
- payload shape validated per event below

## Topic model

- Topic: `user:<subject>` where `<subject>` is the OIDC `sub` the socket was
  authenticated with (`UserSocket.assigns.user_id`).
- **Join auth:** joining `user:<id>` is allowed only when the socket's
  subject equals `<id>`. Users get only their own topic — a user can never
  subscribe to someone else's events. (Channel tests cover this: cross-user
  join → error.)
- Presence-aware: broadcasts go to currently-connected sockets only.
  **No offline queue, no persistence in Wave 1.** A client that is offline
  when a `post:new` fires simply misses it — the home timeline
  (`GET /v1/feed/home`) is the durable source of truth. Wave 2 adds a
  persistent inbox with per-device delivery receipts; the event names and
  payload shapes here are chosen to be inbox-compatible.

## Event catalog (Wave 1)

### `post:new`

Fired when a post completes fan-out. Recipients: the author's followers'
`user:<subject>` topics (plus nobody else — the author already has the
post from the `201` response).

Payload:

```json
{
  "post_id": "<uuid>",
  "author_id": "<uuid>",
  "author_handle": "handle_or_null",
  "text": "...",
  "media_ids": ["..."],
  "created_at": "2026-10-18T09:00:00Z"
}
```

`dedupe_key`: `post:<post-uuid>` — a retried publish of the same post is
absorbed.

**Celebrity policy (mirrors the home-feed hybrid fan-out exactly):** authors
with more followers than `FANOUT_THRESHOLD` get NO realtime fan-out in
Wave 1. Fanning 100k topics per post would defeat the entire point of the
pull-on-read design. Their followers see the post on the next home-feed
pull. This is a documented limitation, not a bug.

### `follower:new`

Fired when a NEW follow edge is created. Recipient: the followee's
`user:<subject>` topic. Idempotent retries of an existing edge (the `200`
path) do NOT fire again — the followee was already told.

Payload:

```json
{
  "follower_id": "<uuid>",
  "follower_handle": "handle_or_null",
  "created_at": "2026-10-18T09:00:00Z"
}
```

`dedupe_key`: `follow:<follower-uuid>:<followee-uuid>`.

### `shipment:update` (F5)

Fired when a shipment's lifecycle status or COD money changes (Wave 3
commerce; the F6 saga is the producer — published after the DB write
commits, never before). Recipient: the **buyer's** `user:<subject>`
topic — buyers can never subscribe to another user's shipments.

Payload:

```json
{
  "shipment_id": "<uuid>",
  "order_id": "<uuid>",
  "status": "out_for_delivery",
  "tracking_number": "DLV123456789",
  "cod_expected_paise": 99900,
  "cod_collected_paise": 99900,
  "updated_at": "2026-10-19T00:00:00Z"
}
```

`status` is one of the 8 `logistics.TrackingStatus` wire values;
`cod_collected_paise` is omitted until the courier reports (a missing
field is not a zero collection). The chat validates required fields +
`cod_expected_paise` as a non-negative integer and rejects anything
else with 422; it treats the fields as opaque and never interprets
them. `dedupe_key`: `shipment:<shipment-id>:<status>:<updated-at-unix>`.

### Reserved names (do not use)

`post:deleted`, `post:like`, `post:reply`, `follow:request`,
`message:delivered` — Wave 2.

## Delivery semantics (API side)

- **Best-effort, never fatal.** The post/follow write is the source of
  truth; the event is a hint. A publish failure (chat unreachable, 5xx,
  timeout) is logged (`realtime: post:new publish failed (best-effort)`)
  and swallowed — the API request that caused it still succeeds.
- **Never delays the 201.** Publishes run in a detached goroutine
  (`context.WithoutCancel`, 15s budget) after fan-out succeeds. A slow
  chat service cannot slow down posting.
- **Ordering vs fan-out:** the event fires only after `FanoutPost`
  succeeds, so a client that receives `post:new` and immediately fetches
  will find the post in its home timeline (or fetch it by id — the row
  is committed before the response).
- **At-most-once per logical event, at-least-once across retries** is NOT
  promised: there is no retry. One attempt, then log. The `dedupe_key`
  exists so a future retrying publisher (Wave 2 inbox) stays idempotent.
- Disabled mode: with no `CHAT_INTERNAL_TOKEN` the API logs one WARN at
  startup and drops all events. Fan-out, timelines, and posts are
  unaffected.

## Configuration

| Env (API)             | Default               | Meaning                                  |
|-----------------------|-----------------------|------------------------------------------|
| `CHAT_INTERNAL_URL`   | `http://chat:4000`    | chat service base URL                    |
| `CHAT_INTERNAL_TOKEN` | _(empty = disabled)_  | shared service-to-service secret         |

| Env (chat)            | Default | Meaning                                  |
|-----------------------|---------|------------------------------------------|
| `CHAT_INTERNAL_TOKEN` | _(must set)_ | same secret; endpoint 401s without it |

Fail-closed: a configured token with an unparsable URL makes the API
refuse to boot (`internal/realtime.NewPublisher` error → `os.Exit(1)`).

## Client guidance

1. Connect the socket, join `user:<own-subject>`.
2. On `post:new`: render the notification from the payload; do NOT refetch
   unless you need fields outside the payload.
3. On `follower:new`: refresh the follower count / list.
4. On `live:started`: the host's own device should show "you are on air"
   state; `live:ended` tears it down. Host-side clients subscribe to
   these on their own `user:<subject>` topic (G1, Wave 4).
5. Never trust events as the source of truth — timelines and profile
   endpoints are. Events are hints; fetches are facts.
