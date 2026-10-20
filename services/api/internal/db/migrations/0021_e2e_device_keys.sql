-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0021: E2E device key management (I1, Phase I).
--
-- Signal-grade device key material, server-side. The server stores
-- PUBLIC keys only — private keys never leave the device, so these rows
-- are safe to replicate, back up, and hand to a forensics team without
-- ever endangering message confidentiality. The server's cryptographic
-- surface is intentionally tiny: it stores 32-byte key blobs (checked
-- by CHECK constraints, never parsed) and verifies one Ed25519
-- signature per registration with the stdlib — all Diffie-Hellman and
-- ratchet math happens on clients (audited constructions only, per I2).
--
-- Key roles (Signal model):
--   identity_key    — Ed25519 public key, the device's long-term
--                     identity. Never rotates; replacing it is a
--                     re-registration (new device row), which is a
--                     security-relevant event clients must surface.
--   signed_prekey   — X25519 public key, medium-term (rotated ~monthly
--                     by the client). Authenticates the bundle.
--   signed_prekey_sig — Ed25519 signature by the identity key over the
--                     RAW 32 signed_prekey bytes. The server verifies
--                     this on every registration — an attacker who can
--                     write rows but lacks the identity private key
--                     cannot substitute their own prekeys.
--   one-time prekeys — X25519 public keys, consumed one per new session
--                     (X3DH, I2). consumed_at is set when handed out;
--                     consumed rows are retained for audit, never reused.
--   registration_id — Signal-style uint16 (1..65535); part of the
--                     X3DH address, lets clients detect device resets.

-- One row per user device. device_id is the client-chosen stable label
-- (e.g. "pixel-9-pro"); the identity_key is the cryptographic identity.
CREATE TABLE e2e_devices (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device_id         text NOT NULL
        CHECK (device_id <> '' AND char_length(device_id) <= 64),
    -- Ed25519 public key (32 bytes). THE public half only.
    identity_key      bytea NOT NULL CHECK (octet_length(identity_key) = 32),
    -- X25519 public key (32 bytes). THE public half only.
    signed_prekey     bytea NOT NULL CHECK (octet_length(signed_prekey) = 32),
    signed_prekey_id  integer NOT NULL CHECK (signed_prekey_id >= 0),
    -- Ed25519 signature over the raw 32 signed_prekey bytes.
    signed_prekey_sig bytea NOT NULL CHECK (octet_length(signed_prekey_sig) = 64),
    registration_id   integer NOT NULL CHECK (registration_id BETWEEN 1 AND 65535),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    last_seen_at      timestamptz,
    UNIQUE (user_id, device_id)
);

CREATE INDEX e2e_devices_user_idx ON e2e_devices (user_id);

-- keep the existing updated_at discipline (see 0002 trigger set_updated_at)
CREATE TRIGGER e2e_devices_set_updated_at
    BEFORE UPDATE ON e2e_devices
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE e2e_devices IS
    'E2E device registrations (I1). PUBLIC KEYS ONLY — identity (Ed25519), signed prekey (X25519), and its Ed25519 signature. Private keys never leave the device.';

-- One-time prekeys: each row is handed out at most once (consumed_at).
-- Clients must replenish when the available count runs low (I2 exposes
-- the count); a device with zero available prekeys can still RECEIVE
-- sessions — the initiator falls back to signed-prekey-only X3DH —
-- but it SHOULD keep the queue stocked (target: 100, same as Signal).
CREATE TABLE e2e_one_time_prekeys (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    device_id   uuid NOT NULL REFERENCES e2e_devices (id) ON DELETE CASCADE,
    prekey_id   integer NOT NULL CHECK (prekey_id >= 0),
    -- X25519 public key (32 bytes). THE public half only.
    public_key  bytea NOT NULL CHECK (octet_length(public_key) = 32),
    created_at  timestamptz NOT NULL DEFAULT now(),
    consumed_at timestamptz,
    UNIQUE (device_id, prekey_id)
);

-- Fast path for the I2 consume query:
--   SELECT ... WHERE device_id = $1 AND consumed_at IS NULL
--   ORDER BY prekey_id LIMIT 1 FOR UPDATE SKIP LOCKED;
CREATE INDEX e2e_one_time_prekeys_available_idx
    ON e2e_one_time_prekeys (device_id, prekey_id)
    WHERE consumed_at IS NULL;

COMMENT ON TABLE e2e_one_time_prekeys IS
    'E2E one-time prekeys (X25519 publics) per device. A row is handed out at most once; consumed rows are retained for audit and never reused.';
