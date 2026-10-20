# E2E encryption

SPDX-License-Identifier: AGPL-3.0-or-later

Sutra's end-to-end encryption follows the Signal protocol family:
X25519 identity/signed-prekey/one-time-prekey bundles, X3DH session
establishment, and the Double Ratchet for ongoing messages. The server
is a dumb, authenticated key directory — it stores **public keys only**
and **ciphertext only**, never plaintext, never private keys. A
compromised server, database backup, or insider can read contact lists
and message timing, but not message content. That is the entire point.

> Scope note: this document covers **device key management (I1)** —
> generation, registration, rotation, and replenishment of key
> material — **X3DH session establishment + Double Ratchet (I2)**: the
> audited-construction profile, the initiator/recipient steps, and the
> server transport contract — and the **E2E message envelope (I3)**:
> ciphertext over `message:new`, the per-conversation E2E flag, and the
> client rules for honest E2E claims. Never claim E2E on a conversation
> where it is not enabled: the API exposes an explicit per-conversation
> E2E flag, and clients must surface the transport state honestly.

## Key roles

| Key | Type | Lifetime | Purpose |
|---|---|---|---|
| Identity key | Ed25519 (32-byte public) | Never rotates — replacing it is a re-registration | The device's long-term identity; signs the signed prekey; its fingerprint is what users verify out-of-band |
| Signed prekey | X25519 (32-byte public) | Medium-term; client rotates ~monthly | Authenticates the key bundle; used in X3DH when no one-time prekey is available |
| One-time prekeys | X25519 (32-byte public) | Single use; each handed out at most once | Fresh DH material per new X3DH session; the main forward-secrecy lever for session setup |
| Registration id | uint16, 1–65535 | Stable per install | Part of the X3DH address; lets clients detect a device reset |

## Key lifecycle (client implementer's guide)

### 1. Generation (first launch)

On first launch the client generates, **on device, in secure storage**:

1. An Ed25519 identity keypair.
2. A signed-prekey X25519 keypair + an incrementing signed-prekey id.
3. The Ed25519 signature over the **raw 32 signed-prekey public bytes**
   with the identity private key.
4. A batch of one-time X25519 keypairs (target: 100, Signal's number),
   each with a unique prekey id.
5. A random registration id in 1–65535.

The private halves never leave the device — not at registration, not
ever. There is no "export keys" API, and the server has no endpoint
that accepts a private key.

### 2. Registration

The client POSTs the bundle to the device-registration endpoint
(see "Server contract" below). The server:

- validates shapes and byte lengths (32/32/64),
- **verifies the Ed25519 signature over the signed prekey against the
  identity key** — this is the anti-MITM check; a bundle whose prekeys
  were not authorized by the identity private key is rejected with a
  422,
- persists the row in `e2e_devices` plus the one-time queue in
  `e2e_one_time_prekeys`.

Re-registering the same `device_id` with a **different** identity key is
treated as a device reset: the server replaces the row, and clients that
had sessions with the old identity must surface a "security number
changed"-style warning before sending again. Re-registering with the
**same** identity key (e.g. signed-prekey rotation) is a quiet update.

### 3. Signed-prekey rotation

Clients rotate the signed prekey roughly monthly (and immediately if
they suspect the signed-prekey private half leaked — which, since it
never leaves the device, means device compromise). Rotation is a
re-registration of the same device with the same identity key and a new
signed prekey + signature + id. The server keeps only the latest; old
signed prekeys are not retained, because X3DH sessions already
established have ratcheted forward and do not need them.

### 4. One-time-prekey replenishment

Every new incoming session consumes one one-time prekey
(`consumed_at` is set; consumed rows are never reused). The client
watches the available count (returned by the device-fetch endpoint and
by a low-water warning) and uploads a fresh batch **before** the queue
empties — at 10 remaining at the latest.

If the queue is empty, new sessions still work: the initiator falls
back to signed-prekey-only X3DH. That session is still authenticated
and still gets forward secrecy from the Double Ratchet once messages
flow — but its *initial* message lacks the extra one-time-key DH. So:
an empty queue is a degradation, not an outage, and the client's job is
to never let it sit empty.

### 5. Device removal

Deleting a device (user removes it, or the account is deleted) deletes
`e2e_devices` and cascades to `e2e_one_time_prekeys`. The private keys
die with the device; there is no server-side recovery, by design.

## Server contract

Migration `0021_e2e_device_keys.sql` owns the tables:

- `e2e_devices` — one row per user device; `UNIQUE(user_id, device_id)`;
  every key column carries a byte-length CHECK (32/32/64); the comment
  on the table states the public-keys-only invariant.
- `e2e_one_time_prekeys` — the per-device queue; `UNIQUE(device_id,
  prekey_id)`; partial index on unconsumed rows backing the consume
  query: `... WHERE device_id = $1 AND consumed_at IS NULL ORDER BY
  prekey_id LIMIT 1 FOR UPDATE SKIP LOCKED`.

The Go domain package `internal/e2e` holds the pure rules:
`ValidateBundle` (shape + length + id-range + duplicate-id checks +
Ed25519 signature verification, all with typed sentinel errors) and
`SelectAvailablePrekey` (lowest unconsumed id — the DB layer locks and
marks it consumed in the same transaction). No crypto beyond
`crypto/ed25519` verification and length checks lives server-side; no
custom primitives anywhere.

## HTTP API (I1 part 2)

All key bytes travel as standard base64 (with padding). Request bodies
are strict: unknown fields are rejected, and undecodable key fields
are 422 `e2e_invalid_key_encoding` naming the bad field. A bundle whose
signature does not verify is 422 `e2e_invalid_bundle` — rejected, never
persisted.

- `POST /v1/e2e/devices` — register (201) or re-register (200) the
  caller's device bundle. Re-registration REPLACES the key material and
  drops the unconsumed one-time queue (their private halves are gone
  with the old install); consumed rows are kept for audit.
- `GET /v1/e2e/devices` — the caller's devices: ids, registration ids,
  live prekey counts, timestamps. No key material.
- `POST /v1/e2e/devices/{device_id}/prekeys` — replenish the one-time
  queue. Idempotent on retry (same id + same key = no-op); the same id
  with different bytes is 409 `e2e_prekey_id_conflict`. Returns the new
  available count.
- `DELETE /v1/e2e/devices/{device_id}` — remove the device (204); the
  queue cascades. The private keys die with the device.
- `GET /v1/users/{id}/devices` — the public key directory: identity
  key, signed prekey + id + signature, registration id, and live
  prekey count per device. Any authenticated user may read it —
  initiating a session requires the peer's public bundle.
- `GET /v1/users/{id}/devices/{device_id}/prekey-bundle` — the
  session-initiation fetch: returns the public bundle and ATOMICALLY
  consumes the lowest-id available one-time prekey (`SELECT ... FOR
  UPDATE SKIP LOCKED`, so concurrent initiators never receive the same
  prekey). When the queue is dry the bundle still returns with
  `one_time_prekey: null` and the initiator falls back to
  signed-prekey-only X3DH; `available_prekeys` reports the post-consume
  depth so the initiator can nudge the device to replenish.

Abuse note: the consume endpoint spends a finite resource (the
device's prekey queue). There is no per-device consume rate limit
beyond the global per-IP limiter — a hostile initiator can drain a
queue and weaken (not break) first-message forward secrecy until the
device replenishes. Clients MUST treat a falling `available_prekeys`
as a replenish signal and SHOULD alert the user on abnormal drain;
server-side per-target throttling is future work, not a silent
assumption.

## X3DH session establishment (I2)

The server performs **no** X3DH computation — every ECDH, KDF, and AEAD
step below runs on the two devices. The server's only jobs are (a) the
public-key directory (I1), which it already is, and (b) reliable,
validated, replayable transport of the initiator's first message over
the `e2e:session_init` channel event (this section).

### Audited-construction profile (mandatory for clients)

No custom primitives, no novel combinations. Client implementations
MUST use exactly:

| Operation | Construction |
|---|---|
| Key agreement | X25519 ECDH (RFC 7748) |
| Identity signatures | Ed25519 (RFC 8032) |
| Key derivation | HKDF-SHA256 (RFC 5869), info strings as in the Signal X3DH spec |
| Initial-message AEAD | XChaCha20-Poly1305 (draft-arciszewski-xchacha) — 24-byte nonces survive client RNG reuse better than 96-bit nonces |
| Ratchet-message AEAD | XChaCha20-Poly1305 |
| Session state KDF | The Double Ratchet's HKDF-SHA256 chain/root steps, unmodified |

Any client that substitutes a primitive (AES-GCM here, a home-grown
KDF there) is NOT implementing this protocol and MUST NOT claim
compatibility. Security reviews start at this table.

### Initiator steps (Alice → Bob)

1. Fetch Bob's bundle: `GET /v1/users/{bob}/devices/{device}/prekey-bundle`
   (or `e2e:prekey_fetch` on the `user:` channel). The response spends
   Bob's lowest-id one-time prekey atomically — **each initiator gets a
   distinct prekey**; the server guarantees this under concurrency
   (`SELECT ... FOR UPDATE SKIP LOCKED`, race-tested in I1).
2. **Verify the signed prekey's Ed25519 signature against Bob's
   identity key** before touching any ECDH. A bundle that fails this
   check is discarded — never "try anyway".
3. Generate an ephemeral X25519 keypair E_A. Compute the X3DH shared
   secret: `DH(I_A, SPK_B) || DH(E_A, I_B) || DH(E_A, SPK_B) [||
   DH(E_A, OPK_B)]` (the last term only when a one-time prekey was
   returned; when `one_time_prekey` is null the session still works —
   signed-prekey-only X3DH, with the documented degradation).
4. HKDF the shared secret into the root key + first chain key; run the
   Double-Ratchet sending-chain step; AEAD-encrypt the first payload
   (which SHOULD be an empty "session established" marker, not user
   text — it lets Bob's client build the session without surfacing a
   phantom message).
5. Push `e2e:session_init` on the 1:1 room's channel with the envelope:
   `recipient` (Bob's subject), `sender_device_id`, `registration_id`,
   `identity_key` + `base_key` (E_A public, 32B base64 each),
   `signed_prekey_id`, `prekey_id` (the consumed one-time id, or null),
   `ciphertext` (the AEAD initial message, ≤ 64KiB), `client_msg_id`.
6. **Delete E_A's private half immediately after step 4.** The
   initiator's ephemeral is the most disposable secret in the whole
   protocol — holding it past the send buys nothing and risks
   everything.

### Recipient steps (Bob)

1. Receive `e2e:session_init` live, or replay it from room history on
   the next join — the server persists it before broadcast precisely
   so an offline Bob never misses session establishment.
2. Match `recipient` to self; load the private halves for
   `signed_prekey_id` (+ `prekey_id` if present — if the id is unknown
   because Bob rotated, fall back to signed-prekey-only computation).
3. Recompute the X3DH shared secret with the mirror DH sequence,
   derive root + receiving chain, AEAD-decrypt. **On AEAD failure:
   drop the envelope, keep no session, surface a "couldn't decrypt"
   state — never retry with weakened checks.**
4. **Delete the consumed one-time prekey's private half** (if used).
   The signed prekey's private half stays until the next rotation.
5. From here the Double Ratchet runs (below). Bob's first reply
   performs a DH ratchet step, which is what heals the session.

### The server transport contract (`e2e:session_init`)

- **Validation, fail-closed.** `SutraChat.E2E.Handshake.validate_init/1`
  (pure, unit-tested): every field present, base64 decodes, public
  keys are exactly 32 bytes, `registration_id` 1–65535, prekey ids
  1–16777215, ciphertext 1 byte–64KiB. Malformed envelopes get an
  error reply and are never persisted, never broadcast.
- **Persist before broadcast.** The canonical JSON envelope is stored
  with `kind = "e2e_session_init"` (ciphertext + public keys only —
  the messages table holds no plaintext for E2E rows, same invariant
  as I3). Crash between persist and broadcast is the only ordering
  the server promises; see `docs/delivery-semantics.md`.
- **Replayable.** Joins return the init message in history like any
  other row; clients decode `body` as JSON when `kind ==
  "e2e_session_init"`. The recipient's client MUST treat a replayed
  init as idempotent: if a session for `(sender identity,
  base_key)` already exists, the replay is a no-op, never a second
  session.
- **Idempotent sends.** `(sender_subject, client_msg_id)` dedupe is
  the same as `message:new`: the sender's retry gets `deduped: true`
  in the `msg:ack`, never a duplicate row or a duplicate session.
- **No existence oracle beyond the directory.** Fetching a bundle for
  an unknown user/device returns `not_found` with no further detail
  (I1); the channel does not confirm or deny recipient validity.

### Forward secrecy — who enforces what

| Property | Enforced by |
|---|---|
| Each X3DH initiation consumes a **distinct** one-time prekey | **Server** — atomic consume, race-tested (I1: 20 concurrent consumers → 20 distinct prekeys) |
| One-time prekey private halves deleted after use | **Client** — recipient step 4; also on replenishment the server drops only *unconsumed* rows |
| Initiator ephemeral deleted after the send | **Client** — initiator step 6 |
| Chain keys deleted after each ratchet step; message keys deleted after decrypt | **Client** — Double Ratchet implementation |
| Past messages stay unreadable after a device theft | The combination: a thief gets current chain state, not deleted keys — **provided** the client actually deletes (audit this in client reviews) |

### Post-compromise healing — who enforces what

Healing comes from the DH ratchet: every time the conversation turns
around (Alice→Bob→Alice), each side mixes a fresh ephemeral DH into
the root key, so a compromise of *current* chain state stops yielding
*future* plaintext after one round trip. The server's contribution is
purely transport: it must deliver both directions' messages promptly
and without silent drops — which is the existing at-least-once
broadcast contract. **Clients MUST perform a DH ratchet step on every
direction change** (not just on a timer); a client that reuses the
sending chain across direction changes silently forfeits healing.

## Double Ratchet (I2)

Per 1:1 conversation, per device pair, the session state is:

- a **root key** (ratchets on every DH step),
- a **sending chain key** and **receiving chain key** (ratchet per
  message; each step's message key is derived then the chain key
  advances),
- the peer's current ratchet public key + our ephemeral pair,
- counters: message numbers per chain, and a small skipped-message-key
  store (cap: 1000 keys, then drop oldest — bounds the memory a
  malicious out-of-order flood can consume).

Rules clients MUST implement (Signal's, unchanged):

1. **Delete chain keys immediately after deriving the message key.**
   The message key may be retained until its message decrypts
   (out-of-order tolerance), then deleted.
2. **Never reuse a (chain key, message number) pair.** The AEAD nonce
   is derived from the message number; reuse breaks the AEAD.
3. **Cap skipped keys** (1000) and cap the number of DH ratchet steps
   you'll process from a single peer message (defense against a
   malicious peer forcing unbounded KDF work — 5 is Signal's number).
4. **Out-of-order init replay is a no-op** (see transport contract).
5. **Session reset UX:** when the peer's identity key changes
   (re-registration with a new identity), the client MUST archive the
   old session, require explicit user confirmation ("security number
   changed"), and only then X3DH anew. Silent auto-replacement is a
   MITM-shaped hole.

The server stores none of this state — it is per-device, in secure
storage, and leaves with the device.

## E2E message envelope (I3)

After the session is established, every 1:1 message travels as
ciphertext inside an `e2e_data` envelope pushed with the **same**
`message:new` channel event as plaintext sends. The server validates
the envelope shape, persists the canonical envelope as JSON with
message kind `"e2e_message"` (**ciphertext only, never plaintext**),
broadcasts it as `message:new` (recipients switch on `kind`), and
acks the sender exactly like a normal send (idempotent on
`(sender_subject, client_msg_id)` — a retry never duplicates a
message).

### Envelope shape

All fields required. `client_msg_id` stays top-level in the push
(transport-level idempotency); the crypto fields live in `e2e_data`:

| Field | Type | Meaning |
|---|---|---|
| `recipient` | subject string, ≤ 256 B | The intended recipient, so the right device picks the envelope out of the room broadcast |
| `sender_device_id` | string, 1–128 B | The sending device label |
| `ratchet_key` | base64, exactly 32 B | The sender's current Double-Ratchet ephemeral DH public key (X25519). The recipient uses it for the DH ratchet step on direction change |
| `prev_chain_length` | uint, 0–4294967295 | PN in Signal's header: length of the previous sending chain |
| `message_number` | uint, 0–4294967295 | N in Signal's header: position in the current sending chain |
| `ciphertext` | base64, 1 B–64 KiB decoded | The XChaCha20-Poly1305 AEAD ciphertext. Opaque to the server |

The server stamps `"version": 1` and re-encodes key bytes to canonical
base64 before persisting. Envelope limits: `SutraChat.E2E.Envelope`.

### Server transport contract

- **Validation, fail-closed.** `SutraChat.E2E.Envelope.validate_message/1`
  (pure, unit-tested): every field present, base64 decodes, ratchet
  key exactly 32 bytes, counters non-negative integers (uint32-capped),
  ciphertext 1 byte–64KiB. Malformed envelopes get an error reply and
  are never persisted, never broadcast.
- **Plaintext-or-E2E, never both.** A push carrying both `body` and
  `e2e_data` is rejected with an error reply — a confused client cannot
  leak plaintext into an E2E conversation through a mixed payload.
- **Persist before broadcast.** The canonical JSON envelope is stored
  with `kind = "e2e_message"`. The messages table holds no plaintext
  for E2E rows (the same invariant as `e2e_session_init`); history
  replays them to offline recipients, whose ratchet steps derive from
  exactly these messages.
- **Idempotent sends.** `(sender_subject, client_msg_id)` dedupe is the
  same as `message:new`: the sender's retry gets `deduped: true` in the
  `msg:ack`, never a duplicate row.
- **Server computes no crypto.** The ratchet runs entirely on the
  devices; the server could not decrypt even if it wanted to — it holds
  no private keys.

### The per-conversation E2E flag

The first persisted `e2e:session_init` in a room is the server's
evidence that X3DH establishment happened there. At that moment the
chat service flips the per-conversation flag:

- **Writer:** the chat service (it owns rooms in Wave 1 and produces
  the evidence), into the Go-owned `e2e_conversations` table
  (migration `0022_e2e_conversations.sql`). The flip is monotonic —
  once enabled, never back; `first_init_message_id` pins the first
  init row as the evidence anchor. The write is best-effort relative
  to the send (the persisted init row is the real evidence), so a
  write failure is logged, never fatal to the send.
- **Readers:** the chat channel serves `e2e_enabled` in every room join
  response; the Go REST API serves `GET /v1/conversations/{room_id}/e2e`
  (`200 {room_id, e2e_enabled, enabled_at}`; `404
  conversation_not_found` on unknown rooms; `400 invalid_room_id` on
  malformed ids). Unknown is never E2E.

The server does **not** reject plaintext `message:new` in a flagged
room: the flag marks "session establishment observed", and enforcement
of ciphertext-only sending is a client UX duty (a server-side reject
could break pre-session UX and multi-device transitions). What the
server DOES guarantee is that it never mislabels: the flag flips only
on a real persisted handshake, and E2E rows carry ciphertext only.

### Client rules (mandatory)

1. **Decode on `kind`.** When `kind == "e2e_message"`, decode `body` as
   JSON and feed the envelope to the local Double-Ratchet session.
   When `kind == "text"`, render `body` as plaintext.
2. **Never claim E2E where it is not enabled.** Display E2E UI (lock
   icons, "end-to-end encrypted" labels) ONLY when the conversation's
   flag is set (`e2e_enabled` from the join response or the REST
   endpoint) AND the local device holds a verified session for the
   peer. A 404 / `false` / missing flag means NOT E2E — surface
   plaintext transport honestly.
3. **Verify the peer.** Before sending the first E2E message, verify
   the signed prekey's Ed25519 signature against the peer's identity
   key (I2 initiator step 2), and surface identity-key changes with
   explicit user confirmation (I2 rule 5).
4. **Handle `deduped: true` as success.** A retry that returns the
   original id established nothing twice — never rebuild the session.
5. **Out-of-order and replay discipline** follow the I2 Double Ratchet
   rules above (skipped-key cap 1000, DH-step cap 5, replayed inits
   are no-ops).

## Security properties (what this construction buys)

- **Server compromise ≠ message compromise.** The database holds
  public keys and (from I3) ciphertext. Without a device's private
  keys, past and future message content is unrecoverable.
- **Bundle authenticity.** The signed-prekey signature proves the
  bundle was authorized by the identity private key holder. An
  attacker who can write to the key directory but does not hold that
  private key cannot substitute their own prekeys to MITM new
  sessions.
- **Forward secrecy at setup.** Each one-time prekey is used once and
  then discarded by clients after the X3DH; stealing a device later
  does not decrypt sessions it established before the theft (the
  ratchet, I2, extends this to every message).

## Explicit non-goals

- The server never sees plaintext and never needs to: abuse/reporting
  flows work on ciphertext hashes and metadata, never content.
- There is no key escrow, no "admin decrypt", no lawful-access backdoor.
  If a product requirement ever asks for one, it is a new phase with
  founder sign-off, not a quiet change to this one.
- Group E2E (sender-key ratchets) is out of scope for Phase I; 1:1
  sessions first.
