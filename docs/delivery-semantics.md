# Delivery semantics

How message delivery works in the Sutra chat service: what the server
guarantees, what it doesn't, and what clients must do. Read this before
writing a client.

## The send path

Every `message:new` push travels this exact pipeline on the server:

1. **Authorize** — payload validated (`SutraChat.Chat.RoomPolicy`).
2. **Persist** — single atomic `INSERT ... ON CONFLICT` keyed on
   `(sender_subject, client_msg_id)`. A retried send returns the original
   row; it can never create a second one.
3. **Broadcast** — `message:new` fanned out to all channel subscribers,
   including the sender.
4. **Ack** — a `msg:ack` push event sent to the sender only.

Steps run strictly in order. The ack is a pushed event rather than the
push reply, so there is exactly one confirmation path — and it fires only
after **both** persist and broadcast completed. No ack ever precedes
durability: if a client holds an ack, the message is in PostgreSQL.

## The `msg:ack` event

```json
{
  "id": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
  "room_id": "general",
  "client_msg_id": "01J...",
  "deduped": false,
  "sent_at": 1760734128123
}
```

- `id` — server-assigned message UUID. Use it as the canonical identity.
- `room_id` — the room the message was sent to.
- `client_msg_id` — echoed so the client can correlate the ack with its
  in-flight send.
- `deduped` — `true` when this send was a retry of an already-stored
  message (no new row was created). A client that timed out and retried
  can show "sent" with confidence instead of guessing whether the first
  attempt landed.
- `sent_at` — server `inserted_at` as Unix milliseconds; useful for
  measuring send latency.

Exactly one `msg:ack` is pushed per `message:new` push that passes
validation. Validation failures reply with an error and produce no ack.

## At-least-once, not exactly-once

The system guarantees **at-least-once delivery to the server** and
**exactly-once storage**:

- If the client receives `msg:ack`, the message is stored exactly once.
- If the client does **not** receive an ack (timeout, disconnect, crash),
  the message may or may not be stored. The client MUST retry with the
  **same** `client_msg_id` — never mint a new one for a retry.
- A retried send that finds the original row returns it with
  `deduped: true`. Storage stays exactly-once no matter how many times
  the send is retried, even concurrently from multiple chat nodes.

Subscribers (including the sender, which receives its own broadcasts)
must therefore tolerate **duplicate `message:new` broadcasts**: a crash
between persist and broadcast, or a client retry in that window, can
re-broadcast an already-seen message. Deduplicate on `id` (or on
`(sender_subject, client_msg_id)` before the server id is known).

## Crash windows

| Crash point | Client sees | Recovery |
|---|---|---|
| Before persist | No ack | Retry same `client_msg_id` → stored fresh, `deduped: false` |
| Between persist and broadcast | No ack | Retry → dedupe hit, broadcast re-dispatched, ack with `deduped: true`. Subscribers may see the message twice — dedupe client-side. |
| Between broadcast and ack | No ack, but subscribers (maybe) saw it | Same as above. |
| After ack | Ack in hand | Nothing to do — the message is durable. |

## Client rules

1. Generate `client_msg_id` as a UUID (v4/v7) per send attempt — one id
   per logical message, reused across all retries of that message.
2. Wait for `msg:ack` correlated by `client_msg_id`. Suggested timeout:
   10 seconds; on timeout, retry with the same `client_msg_id`.
3. Treat `deduped: true` as success, not an error — it means "your
   earlier attempt already landed".
4. `client_msg_id` must be unique per **user**, not per device. Two
   devices of the same user sending with the same `client_msg_id`
   collide on the unique index — scope ids per device (e.g. prefix with
   a device id) or share one id namespace across the user's devices.
5. On reconnect/rejoin, reconcile with room history (cursor-paged, see
   the channel join payload) before assuming anything about what the
   server holds.

## Ordering

The server orders messages by `(inserted_at, id)` — microsecond
timestamps plus the UUID tiebreak. Concurrent senders interleave; the
history cursor is stable under concurrent inserts (never OFFSET-based).
Per-sender causal order is the client's responsibility: don't treat
`msg:ack` for message N+1 as proof about message N.

## What the ack does NOT mean

- It is **not** a read receipt and **not** a delivery receipt to other
  clients. It confirms: persisted on the server + broadcast dispatched.
- It says nothing about who is online (see presence) or who has seen
  the message (read receipts are a Wave 2 item).

## Wave 2+ roadmap

- Per-recipient delivery/read receipts (`message:delivered`,
  `message:read`) built on the same event pattern.
- Multi-device session model: per-device ack routing so a retry from
  phone doesn't confuse the tablet's in-flight state.
