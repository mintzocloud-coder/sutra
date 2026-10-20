// SPDX-License-Identifier: AGPL-3.0-or-later
// Package e2e defines the E2E device-key-management domain for Sutra
// (I1, Phase I).
//
// Signal-grade model, deliberately boring server side:
//
//   - The server stores PUBLIC keys only — an identity key (Ed25519),
//     a signed prekey (X25519), its Ed25519 signature, and one-time
//     prekeys (X25519). Private keys never leave the device, so the
//     database, backups, and logs can never leak message plaintext or
//     session secrets.
//   - The server's entire cryptographic surface in this package is
//     stdlib crypto/ed25519 signature verification of the signed prekey
//     on registration, plus byte-length checks. There are NO custom
//     crypto primitives here: all Diffie-Hellman, X3DH, and ratchet
//     math happens on clients using audited constructions (I2/I3).
//   - The signed prekey signature binds the bundle: an attacker who
//     can write rows (or a malicious relay) but lacks the device's
//     identity private key cannot substitute their own prekeys, which
//     is exactly the MITM this construction exists to stop.
//
// What the server does with these keys (I2 builds on this):
// registration verifies and persists the bundle; session initiation
// fetches one available one-time prekey per device (marking it
// consumed); clients replenish the one-time queue before it runs dry.
// Key data model lives in migration 0021 (e2e_devices,
// e2e_one_time_prekeys).
//
// Key lifecycle is documented for client implementers in docs/e2e.md.
package e2e

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"unicode/utf8"
)

const (
	// Ed25519PubKeyLen is the length of an Ed25519 public key (the
	// identity key): 32 bytes.
	Ed25519PubKeyLen = 32
	// X25519PubKeyLen is the length of an X25519 public key (signed and
	// one-time prekeys): 32 bytes.
	X25519PubKeyLen = 32
	// Ed25519SigLen is the length of an Ed25519 signature: 64 bytes.
	Ed25519SigLen = 64
	// MaxDeviceIDLen is the maximum client-supplied device label length,
	// in runes (mirrors the migration 0021 CHECK).
	MaxDeviceIDLen = 64
	// MinRegistrationID / MaxRegistrationID bound the Signal-style
	// registration id: a uint16 that must be non-zero (mirrors the
	// migration 0021 CHECK).
	MinRegistrationID = 1
	MaxRegistrationID = 65535
	// MinOneTimePrekeys is the minimum number of one-time prekeys a
	// device must register with. A device with an empty queue can still
	// receive sessions (initiators fall back to signed-prekey-only
	// X3DH), but forward secrecy for the first message of each new
	// session is weakened — so registration requires a non-empty
	// queue. Clients should keep ~100 stocked (Signal's target).
	MinOneTimePrekeys = 1
)

// Sentinel validation errors. They are wrapped with context by
// ValidateBundle so callers can match on the sentinel with errors.Is
// while still getting the detail in the message.
var (
	// ErrInvalidBundle is the parent of all key-bundle validation
	// failures.
	ErrInvalidBundle = errors.New("e2e: invalid key bundle")
	// ErrBadSignature is returned when the signed-prekey signature does
	// not verify against the identity key. This is the security-critical
	// check: it proves the bundle was authorized by the holder of the
	// identity private key.
	ErrBadSignature = errors.New("e2e: signed prekey signature does not verify")
)

// KeyBundle is a device's registration payload: everything the server
// needs to persist for the device, and nothing it must not (no private
// keys, ever).
type KeyBundle struct {
	// DeviceID is the client-chosen stable device label
	// (e.g. "pixel-9-pro"). Unique per user.
	DeviceID string
	// RegistrationID is the Signal-style uint16 id (1..65535).
	RegistrationID int
	// IdentityKey is the Ed25519 public key (32 bytes).
	IdentityKey []byte
	// SignedPrekey is the X25519 public key (32 bytes).
	SignedPrekey []byte
	// SignedPrekeyID is the client's id for the signed prekey.
	SignedPrekeyID int
	// SignedPrekeySig is the Ed25519 signature by the identity private
	// key over the RAW 32 SignedPrekey bytes.
	SignedPrekeySig []byte
	// OneTimePrekeys is the initial one-time-prekey queue.
	OneTimePrekeys []OneTimePrekey
}

// OneTimePrekey is a single X25519 public key offered for X3DH session
// establishment. Each is handed out at most once.
type OneTimePrekey struct {
	// ID is the client's prekey id (unique per device).
	ID int
	// PublicKey is the X25519 public key (32 bytes).
	PublicKey []byte
}

// ValidateBundle checks a registration KeyBundle end to end: shape,
// lengths, id ranges, duplicate prekey ids, and — the security-critical
// check — the Ed25519 signature over the signed prekey against the
// identity key. A nil return means the bundle is safe to persist.
func ValidateBundle(b KeyBundle) error {
	if b.DeviceID == "" {
		return fmt.Errorf("%w: device_id is empty", ErrInvalidBundle)
	}
	if n := utf8.RuneCountInString(b.DeviceID); n > MaxDeviceIDLen {
		return fmt.Errorf("%w: device_id is %d runes, max %d", ErrInvalidBundle, n, MaxDeviceIDLen)
	}
	if b.RegistrationID < MinRegistrationID || b.RegistrationID > MaxRegistrationID {
		return fmt.Errorf("%w: registration_id %d out of range [%d, %d]",
			ErrInvalidBundle, b.RegistrationID, MinRegistrationID, MaxRegistrationID)
	}
	if len(b.IdentityKey) != Ed25519PubKeyLen {
		return fmt.Errorf("%w: identity_key is %d bytes, want %d",
			ErrInvalidBundle, len(b.IdentityKey), Ed25519PubKeyLen)
	}
	if len(b.SignedPrekey) != X25519PubKeyLen {
		return fmt.Errorf("%w: signed_prekey is %d bytes, want %d",
			ErrInvalidBundle, len(b.SignedPrekey), X25519PubKeyLen)
	}
	if b.SignedPrekeyID < 0 {
		return fmt.Errorf("%w: signed_prekey_id %d is negative", ErrInvalidBundle, b.SignedPrekeyID)
	}
	if len(b.SignedPrekeySig) != Ed25519SigLen {
		return fmt.Errorf("%w: signed_prekey_sig is %d bytes, want %d",
			ErrInvalidBundle, len(b.SignedPrekeySig), Ed25519SigLen)
	}
	// The signature is made by the identity PRIVATE key (which never
	// leaves the device) over the RAW signed-prekey bytes — the same
	// construction Signal uses. Verification needs only the public
	// identity key, via the stdlib (no custom crypto).
	if !ed25519.Verify(ed25519.PublicKey(b.IdentityKey), b.SignedPrekey, b.SignedPrekeySig) {
		return fmt.Errorf("%w for device %q", ErrBadSignature, b.DeviceID)
	}
	if len(b.OneTimePrekeys) < MinOneTimePrekeys {
		return fmt.Errorf("%w: need at least %d one-time prekey(s), got %d",
			ErrInvalidBundle, MinOneTimePrekeys, len(b.OneTimePrekeys))
	}
	seen := make(map[int]struct{}, len(b.OneTimePrekeys))
	for i, k := range b.OneTimePrekeys {
		if k.ID < 0 {
			return fmt.Errorf("%w: one_time_prekeys[%d].id %d is negative",
				ErrInvalidBundle, i, k.ID)
		}
		if _, dup := seen[k.ID]; dup {
			return fmt.Errorf("%w: duplicate one_time_prekeys id %d",
				ErrInvalidBundle, k.ID)
		}
		seen[k.ID] = struct{}{}
		if len(k.PublicKey) != X25519PubKeyLen {
			return fmt.Errorf("%w: one_time_prekeys[%d] is %d bytes, want %d",
				ErrInvalidBundle, i, len(k.PublicKey), X25519PubKeyLen)
		}
	}
	return nil
}

// StoredPrekey is a one-time prekey row as the service layer sees it
// (projected from e2e_one_time_prekeys).
type StoredPrekey struct {
	// ID is the prekey id.
	ID int
	// Consumed is true once the row's consumed_at is set.
	Consumed bool
}

// SelectAvailablePrekey picks the one-time prekey to hand out for a new
// X3DH session: the lowest id that has not been consumed. The DB layer
// must lock the chosen row (SELECT ... FOR UPDATE SKIP LOCKED, see the
// migration 0021 index) and set consumed_at in the same transaction —
// this pure function only decides WHICH row. ok=false means the queue
// is empty; the caller then falls back to signed-prekey-only X3DH
// (still authenticated, still forward-secret from the ratchet onward)
// and must signal the device to replenish.
func SelectAvailablePrekey(keys []StoredPrekey) (id int, ok bool) {
	best := -1
	for _, k := range keys {
		if k.Consumed {
			continue
		}
		if best == -1 || k.ID < best {
			best = k.ID
		}
	}
	if best == -1 {
		return 0, false
	}
	return best, true
}

// AvailableCount counts unconsumed one-time prekeys — the number the
// replenishment check (and the GET device response) reports.
func AvailableCount(keys []StoredPrekey) int {
	n := 0
	for _, k := range keys {
		if !k.Consumed {
			n++
		}
	}
	return n
}
