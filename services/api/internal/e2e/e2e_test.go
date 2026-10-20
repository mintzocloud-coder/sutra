// SPDX-License-Identifier: AGPL-3.0-or-later
package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

// validBundle builds a correctly-signed bundle the way a client would:
// fresh Ed25519 identity keypair, random X25519 prekeys, signature over
// the raw signed-prekey bytes with the identity private key.
func validBundle(t *testing.T) (KeyBundle, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	spk := make([]byte, X25519PubKeyLen)
	if _, err := rand.Read(spk); err != nil {
		t.Fatalf("rand.Read spk: %v", err)
	}
	sig := ed25519.Sign(priv, spk)

	otps := make([]OneTimePrekey, 3)
	for i := range otps {
		k := make([]byte, X25519PubKeyLen)
		if _, err := rand.Read(k); err != nil {
			t.Fatalf("rand.Read otp: %v", err)
		}
		otps[i] = OneTimePrekey{ID: i + 1, PublicKey: k}
	}
	return KeyBundle{
		DeviceID:        "pixel-9-pro",
		RegistrationID:  4242,
		IdentityKey:     pub,
		SignedPrekey:    spk,
		SignedPrekeyID:  7,
		SignedPrekeySig: sig,
		OneTimePrekeys:  otps,
	}, priv
}

func TestValidateBundleValid(t *testing.T) {
	b, _ := validBundle(t)
	if err := ValidateBundle(b); err != nil {
		t.Fatalf("ValidateBundle(valid): %v", err)
	}
}

func TestValidateBundleRejectsForgedSignature(t *testing.T) {
	// The core MITM defense: a bundle whose signed prekey was NOT
	// authorized by the identity private key must be rejected, even
	// when every length check passes.
	b, _ := validBundle(t)
	b.SignedPrekey[0] ^= 0x01 // attacker swaps the prekey bytes
	if err := ValidateBundle(b); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature, got %v", err)
	}

	b, _ = validBundle(t)
	b.SignedPrekeySig[0] ^= 0x01 // attacker mangles the signature
	if err := ValidateBundle(b); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature, got %v", err)
	}

	// Cross-key forgery: sign with a DIFFERENT identity key — this is
	// the "attacker registers their own prekeys under your name" case.
	b, _ = validBundle(t)
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	b.IdentityKey = otherPub
	if err := ValidateBundle(b); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature for wrong identity key, got %v", err)
	}
}

func TestValidateBundleShapeChecks(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*KeyBundle)
	}{
		{"empty device id", func(b *KeyBundle) { b.DeviceID = "" }},
		{"device id too long", func(b *KeyBundle) { b.DeviceID = strings.Repeat("x", MaxDeviceIDLen+1) }},
		{"registration id zero", func(b *KeyBundle) { b.RegistrationID = 0 }},
		{"registration id negative", func(b *KeyBundle) { b.RegistrationID = -1 }},
		{"registration id too big", func(b *KeyBundle) { b.RegistrationID = MaxRegistrationID + 1 }},
		{"short identity key", func(b *KeyBundle) { b.IdentityKey = b.IdentityKey[:16] }},
		{"empty identity key", func(b *KeyBundle) { b.IdentityKey = nil }},
		{"short signed prekey", func(b *KeyBundle) { b.SignedPrekey = b.SignedPrekey[:31] }},
		{"negative signed prekey id", func(b *KeyBundle) { b.SignedPrekeyID = -1 }},
		{"short signature", func(b *KeyBundle) { b.SignedPrekeySig = b.SignedPrekeySig[:63] }},
		{"no one-time prekeys", func(b *KeyBundle) { b.OneTimePrekeys = nil }},
		{"duplicate one-time prekey ids", func(b *KeyBundle) {
			b.OneTimePrekeys[1].ID = b.OneTimePrekeys[0].ID
		}},
		{"negative one-time prekey id", func(b *KeyBundle) { b.OneTimePrekeys[0].ID = -5 }},
		{"short one-time prekey", func(b *KeyBundle) {
			b.OneTimePrekeys[0].PublicKey = b.OneTimePrekeys[0].PublicKey[:20]
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := validBundle(t)
			tc.mutate(&b)
			if err := ValidateBundle(b); !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("expected ErrInvalidBundle, got %v", err)
			}
		})
	}
}

func TestValidateBundleDeviceIDBoundary(t *testing.T) {
	b, _ := validBundle(t)
	b.DeviceID = strings.Repeat("y", MaxDeviceIDLen)
	if err := ValidateBundle(b); err != nil {
		t.Fatalf("max-length device id should pass: %v", err)
	}
	b.RegistrationID = MaxRegistrationID
	if err := ValidateBundle(b); err != nil {
		t.Fatalf("max registration id should pass: %v", err)
	}
}

func TestSelectAvailablePrekey(t *testing.T) {
	keys := []StoredPrekey{
		{ID: 5, Consumed: true},
		{ID: 3, Consumed: false},
		{ID: 9, Consumed: false},
		{ID: 1, Consumed: true},
	}
	id, ok := SelectAvailablePrekey(keys)
	if !ok || id != 3 {
		t.Fatalf("SelectAvailablePrekey: got (%d, %v), want (3, true)", id, ok)
	}

	allConsumed := []StoredPrekey{{ID: 1, Consumed: true}, {ID: 2, Consumed: true}}
	if _, ok := SelectAvailablePrekey(allConsumed); ok {
		t.Fatalf("SelectAvailablePrekey(all consumed): want ok=false")
	}
	if _, ok := SelectAvailablePrekey(nil); ok {
		t.Fatalf("SelectAvailablePrekey(nil): want ok=false")
	}
}

func TestAvailableCount(t *testing.T) {
	keys := []StoredPrekey{
		{ID: 1, Consumed: true},
		{ID: 2, Consumed: false},
		{ID: 3, Consumed: false},
	}
	if n := AvailableCount(keys); n != 2 {
		t.Fatalf("AvailableCount: got %d, want 2", n)
	}
}
