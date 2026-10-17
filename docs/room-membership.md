# Room membership model

**Status:** Wave 1 accepted (B3, 2026-10-17) · **Applies to:** `services/chat`

## Wave 1 — open rooms by ID (current)

Any **authenticated** subject may join any well-formed `room:*` topic and
publish to it. Rooms are created implicitly on first join; there are no
invites, roles, or per-room ACLs.

This is deliberate: it mirrors Telegram/Discord "join by link" pragmatics
for the MVP, and the security posture is still sound because the room ID
is the secret — 64 characters of free choice make unguessable room names
trivial (`crypto.randomBytes(32)` base64url, for example), and the topic
namespace has no enumeration API.

### Enforcement points

| Layer | Check | Where |
|---|---|---|
| Socket connect | Valid OIDC token required; `socket.assigns.user_id` = token subject | `SutraChatWeb.UserSocket.connect/3` (see `auth-contract.md`) |
| Channel join | `user_id` present (defense-in-depth) + `room_id` matches `^[A-Za-z0-9_-]{1,64}$` | `SutraChatWeb.ChatChannel.join/3` via `SutraChat.Chat.RoomPolicy` |
| Send payload | `body` ≤ 4096 chars (Telegram's cap), `client_msg_id` ≤ 128 chars | `ChatChannel` (clean error reply) **and** `Message.changeset` (persistence boundary) |

Join failures reply `{:error, %{reason: "unauthorized" | "invalid room id"}}`;
oversized sends reply `{:error, %{reason: "message too long" | "client_msg_id too long", max_length: N}}`.

## Wave 2 — membership lists (planned)

For private groups with admin control, Wave 2 adds:

- `room_members(room_id, subject, role, joined_at)` — role is `owner | member`.
- Explicit join/invite flow: owners create a room, members join by invite
  token or owner approval.
- `ChatChannel.join/3` authorizes against the membership list instead of
  the open-room rule; publishing re-checks membership (cheap index
  lookup, cached in the socket assigns after join).

`RoomPolicy.authorize_join/2` keeps its `(user_id, room_id)` shape — only
its body gets stricter — so channel code doesn't churn when Wave 2 lands.

## Wave 3+ (out of scope here)

End-to-end encrypted rooms (libsignal 1:1, MLS groups) change the *content*
model, not the membership model: membership gates who receives the
encrypted envelope; the server can no longer read bodies. Tracked in the
Wave 2/3 epics.
