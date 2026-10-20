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
> material. X3DH session establishment and the Double Ratchet (I2) and
> the E2E message envelope (I3) are documented when those land. Never
> claim E2E on a conversation where it is not enabled: the API exposes
> an explicit per-conversation E2E flag, and clients must surface the
> transport state honestly.

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
