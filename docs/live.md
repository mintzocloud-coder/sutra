# Live media (Wave 4, G1)

Mintzo's livestream + video-call seam, built on a self-hosted
[LiveKit](https://livekit.io) SFU. LiveKit is the ONE blessed readymade
in the Wave-4 stack — everything around it is custom: the room registry,
the token mint, and the room lifecycle events.

## Architecture

```
client ──POST /v1/live/rooms──▶ Go API ──live:started──▶ chat ──▶ host's user:<subject> topic
client ──POST /v1/live/token──▶ Go API (mints LiveKit JWT, HS256)
client ──wss://livekit────────▶ LiveKit SFU (media; never sees user ids)
client ──POST /v1/live/rooms/{id}/end──▶ Go API ──live:ended──▶ chat
```

Media bytes NEVER touch the API: the SFU terminates WebRTC and relays
tracks. The API owns identity (who may join), the room registry (what
exists), and the token mint (how long they stay).

## Room model (`live_rooms`, migration 0014)

- `host_id` — the user row that opened the room. Only the host can mint
  `role=host` tokens or end the room.
- `kind` — `livestream` (one-to-many) or `call` (small group). v1 does
  not enforce participant caps server-side; the client shows the
  `viewer_cap` soft cap.
- `status` — `live` → `ended` (terminal, idempotent).
- The SFU room name is `sutra:<room_id>` — derived from the row id, not
  the free-text title, so titles can be anything without sanitisation
  risk and name collisions are impossible.

## Token mint (POST /v1/live/token)

The Go seam (`internal/live`) mints LiveKit participant JWTs with the
**standard library only** — no server SDK dependency. Claim shape:

```json
{
  "iss": "<LIVEKIT_API_KEY>",
  "sub": "<caller's OIDC subject>",
  "nbf": 1718000000,
  "exp": 1718007200,
  "jti": "<random>",
  "video": {
    "roomJoin": true,
    "room": "sutra:<room_id>",
    "canPublish": true,        // host only
    "canSubscribe": true,
    "canPublishData": true     // reactions/emoji bursts
  }
}
```

Grants are **room-scoped, never admin**: a token can only join its own
room. `role=host` requires the caller to be the room's host (403
otherwise) and grants media publish; `role=viewer` is any authenticated
user and can never publish tracks. Tokens are short-lived
(`LIVEKIT_TOKEN_TTL_MINUTES`, default 120) and there is no refresh
grant — an expired client re-mints and re-joins, which is the correct
behaviour for a live session. Minting for an ended room is 410.

**Disabled mode:** an empty `LIVEKIT_API_SECRET` leaves `/v1/live/*`
unregistered (503 `live_unavailable` is never reached — the routes
simply don't exist). A key with an empty secret is a misconfiguration
and makes the API **refuse to boot** (fail-closed).

## Room lifecycle events

On room open: `live:started`. On room end: `live:ended`. Both publish
on the **host's** `user:<subject>` topic via the D5 internal-events
contract (see `docs/realtime-events.md`), best-effort, in a detached
goroutine — a slow chat service can never delay the 201. The dedupe key
is `live:<room_id>:<event>:<started_at_unix>`: double-tap ends and
webhook retries stay idempotent.

Viewers are NOT fanned out per-event in v1: they discover rooms via
`GET /v1/live/rooms` and mint their own tokens. Follower fan-out of
`live:started` is a later refinement (the celebrity rule from feed
fan-out would apply).

## Reconciliation (G1 part 2 — next)

The LiveKit webhook ingress (`room_finished` → flip the room to
`ended` and publish `live:ended`) reconciles the registry against SFU
truth, so a crashed host's room doesn't stay `live` forever. Until it
lands, a host who lost their client re-ends the room via the API.

## Configuration

| Env | Default | Meaning |
|---|---|---|
| `LIVEKIT_WS_URL` | `ws://localhost:7880` | client-reachable SFU endpoint returned in token responses |
| `LIVEKIT_API_KEY` | _(empty)_ | LiveKit keypair key — `iss` of minted tokens |
| `LIVEKIT_API_SECRET` | _(empty = disabled)_ | keypair secret — HS256 signing key; never commit |
| `LIVEKIT_TOKEN_TTL_MINUTES` | `120` | participant token lifetime |

In compose, `LIVEKIT_KEYS` on the `livekit` service must carry the same
pair (`${LIVEKIT_API_KEY}:${LIVEKIT_API_SECRET}`); the dev defaults on
both sides match so `docker compose up` works on a laptop. Production
**must** override both (see `docs/production.md`).

## Why LiveKit (and why this shape)

The Wave-4 research concluded that building an SFU is not a moat — the
moat is the platform around it (rooms, identity, tokens, moderation,
discovery). LiveKit OSS (Apache-2.0) is the de-facto self-hosted SFU
with client SDKs for every platform; its JWT auth model fits our
"API owns identity, SFU owns media" split exactly, and its server
webhooks give us the reconciliation path for crashed hosts.
