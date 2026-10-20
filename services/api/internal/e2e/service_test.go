// SPDX-License-Identifier: AGPL-3.0-or-later
package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

// Integration tests: run when TEST_DATABASE_URL is set; otherwise skip
// cleanly (same convention as internal/posts, internal/social).
func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping e2e integration test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn, 5, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func provisionUser(t *testing.T, pool *db.Pool) pgtype.UUID {
	t.Helper()
	// One user per test (unique subject): device/directory assertions
	// stay isolated even though tests share the database.
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	u, err := users.Provision(context.Background(), pool,
		"e2e-test-"+t.Name()+"-"+base64.RawURLEncoding.EncodeToString(b[:]),
		"E2E Tester")
	if err != nil {
		t.Fatalf("users.Provision: %v", err)
	}
	id, err := social.ParseUserID(u.ID)
	if err != nil {
		t.Fatalf("ParseUserID(provisioned): %v", err)
	}
	return id
}

// uniqueDevice returns a device label unique to this test run so
// parallel-test user rows never collide on (user_id, device_id).
func uniqueDevice(t *testing.T, base string) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base + "-" + base64.RawURLEncoding.EncodeToString(b[:])
}

// testBundle builds a fully valid KeyBundle: fresh Ed25519 identity,
// a properly signed X25519 signed prekey, and random one-time prekeys
// for the given ids. The caller keeps the private halves (the server
// never sees them).
func testBundle(t *testing.T, deviceID string, prekeyIDs ...int) (KeyBundle, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	var spk [32]byte
	if _, err := rand.Read(spk[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	b := KeyBundle{
		DeviceID:        deviceID,
		RegistrationID:  4242,
		IdentityKey:     pub,
		SignedPrekey:    spk[:],
		SignedPrekeyID:  7,
		SignedPrekeySig: ed25519.Sign(priv, spk[:]),
	}
	for _, id := range prekeyIDs {
		var k [32]byte
		if _, err := rand.Read(k[:]); err != nil {
			t.Fatalf("rand: %v", err)
		}
		b.OneTimePrekeys = append(b.OneTimePrekeys, OneTimePrekey{ID: id, PublicKey: k[:]})
	}
	return b, priv
}

func TestRegisterAndReregister(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool)
	ctx := context.Background()
	user := provisionUser(t, pool)
	device := uniqueDevice(t, "pixel")

	bundle, _ := testBundle(t, device, 1, 2, 3)
	dev, created, err := svc.RegisterDevice(ctx, user, bundle)
	if err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	if !created {
		t.Fatal("first registration should report created=true")
	}
	if dev.DeviceID != device || dev.RegistrationID != 4242 ||
		dev.SignedPrekeyID != 7 || dev.AvailablePrekeys != 3 {
		t.Fatalf("unexpected device summary: %+v", dev)
	}
	if dev.LastSeenAt == nil {
		t.Fatal("last_seen_at should be set on registration")
	}

	// Re-register the same label with fresh material: replaces the
	// queue (3 old unconsumed rows dropped, 2 new ones stored) and
	// reports created=false.
	bundle2, _ := testBundle(t, device, 10, 11)
	bundle2.SignedPrekeyID = 8
	dev2, created2, err := svc.RegisterDevice(ctx, user, bundle2)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if created2 {
		t.Fatal("re-registration should report created=false")
	}
	if dev2.SignedPrekeyID != 8 || dev2.AvailablePrekeys != 2 {
		t.Fatalf("re-registration did not replace material: %+v", dev2)
	}
	if dev2.ID != dev.ID {
		t.Fatal("re-registration must keep the same device row")
	}
}

func TestRegisterBadSignature(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool)
	ctx := context.Background()
	user := provisionUser(t, pool)
	device := uniqueDevice(t, "tampered")

	bundle, _ := testBundle(t, device, 1)
	bundle.SignedPrekeySig[0] ^= 0xff // corrupt the signature
	if _, _, err := svc.RegisterDevice(ctx, user, bundle); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature, got %v", err)
	}

	// Cross-key forgery: signature made by a DIFFERENT identity key.
	other, _ := testBundle(t, "other", 1)
	bundle2, _ := testBundle(t, device+"2", 1)
	bundle2.IdentityKey = other.IdentityKey
	if _, _, err := svc.RegisterDevice(ctx, user, bundle2); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature for cross-key forgery, got %v", err)
	}
}

func TestConsumeOrderingAndExhaustion(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool)
	ctx := context.Background()
	user := provisionUser(t, pool)
	device := uniqueDevice(t, "consume")

	bundle, _ := testBundle(t, device, 5, 1, 3)
	if _, _, err := svc.RegisterDevice(ctx, user, bundle); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	// Lowest unconsumed id first: 1, 3, 5 — then the queue is dry.
	for i, want := range []int{1, 3, 5} {
		pb, err := svc.ConsumePrekey(ctx, user, device)
		if err != nil {
			t.Fatalf("ConsumePrekey %d: %v", i, err)
		}
		if pb.OneTimePrekey == nil || pb.OneTimePrekey.ID != want {
			t.Fatalf("consume %d: want prekey id %d, got %+v", i, want, pb.OneTimePrekey)
		}
		if pb.AvailablePrekeys != 2-i {
			t.Fatalf("consume %d: want available %d, got %d", i, 2-i, pb.AvailablePrekeys)
		}
		if pb.IdentityKey == "" || pb.SignedPrekey == "" || pb.SignedPrekeySig == "" {
			t.Fatalf("consume %d: bundle missing public key material", i)
		}
	}
	// Dry queue: bundle still returns, one_time_prekey is null —
	// the initiator falls back to signed-prekey-only X3DH.
	pb, err := svc.ConsumePrekey(ctx, user, device)
	if err != nil {
		t.Fatalf("dry consume: %v", err)
	}
	if pb.OneTimePrekey != nil {
		t.Fatalf("dry queue should yield nil one-time prekey, got %+v", pb.OneTimePrekey)
	}
	if pb.AvailablePrekeys != 0 {
		t.Fatalf("dry queue available = %d, want 0", pb.AvailablePrekeys)
	}

	// Unknown device → 404-class error.
	if _, err := svc.ConsumePrekey(ctx, user, "no-such-device"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("expected ErrDeviceNotFound, got %v", err)
	}
}

func TestConsumeConcurrent(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool)
	ctx := context.Background()
	user := provisionUser(t, pool)
	device := uniqueDevice(t, "racing")

	ids := make([]int, 20)
	for i := range ids {
		ids[i] = i + 1
	}
	bundle, _ := testBundle(t, device, ids...)
	if _, _, err := svc.RegisterDevice(ctx, user, bundle); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	// 20 concurrent consumers must receive 20 DISTINCT prekeys — no
	// prekey is ever handed out twice (the X3DH forward-secrecy
	// property rests on this).
	var (
		mu   sync.Mutex
		seen = map[int]int{}
		wg   sync.WaitGroup
	)
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pb, err := svc.ConsumePrekey(ctx, user, device)
			if err != nil {
				errs <- err
				return
			}
			if pb.OneTimePrekey == nil {
				errs <- errors.New("nil prekey from non-empty queue")
				return
			}
			mu.Lock()
			seen[pb.OneTimePrekey.ID]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent consume: %v", err)
	}
	if len(seen) != 20 {
		t.Fatalf("want 20 distinct prekeys, got %d: %v", len(seen), seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("prekey %d handed out %d times", id, n)
		}
	}
}

func TestReplenish(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool)
	ctx := context.Background()
	user := provisionUser(t, pool)
	device := uniqueDevice(t, "restock")

	bundle, _ := testBundle(t, device, 1)
	if _, _, err := svc.RegisterDevice(ctx, user, bundle); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	if _, err := svc.ConsumePrekey(ctx, user, device); err != nil {
		t.Fatalf("ConsumePrekey: %v", err)
	}

	var k2, k3 [32]byte
	if _, err := rand.Read(k2[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(k3[:]); err != nil {
		t.Fatal(err)
	}
	n, err := svc.ReplenishPrekeys(ctx, user, device, []OneTimePrekey{
		{ID: 2, PublicKey: k2[:]},
		{ID: 3, PublicKey: k3[:]},
	})
	if err != nil {
		t.Fatalf("ReplenishPrekeys: %v", err)
	}
	if n != 2 {
		t.Fatalf("want available=2, got %d", n)
	}

	// Idempotent retry: same id + same bytes = no-op, no error.
	n, err = svc.ReplenishPrekeys(ctx, user, device, []OneTimePrekey{
		{ID: 2, PublicKey: k2[:]},
	})
	if err != nil {
		t.Fatalf("idempotent replenish: %v", err)
	}
	if n != 2 {
		t.Fatalf("idempotent retry changed the count: %d", n)
	}

	// Same id + DIFFERENT bytes = client bug → conflict.
	var other [32]byte
	if _, err := rand.Read(other[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReplenishPrekeys(ctx, user, device, []OneTimePrekey{
		{ID: 2, PublicKey: other[:]},
	}); !errors.Is(err, ErrPrekeyIDConflict) {
		t.Fatalf("expected ErrPrekeyIDConflict, got %v", err)
	}

	// Unknown device → not found.
	if _, err := svc.ReplenishPrekeys(ctx, user, "ghost", []OneTimePrekey{
		{ID: 9, PublicKey: k2[:]},
	}); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("expected ErrDeviceNotFound, got %v", err)
	}
}

func TestDeleteDevice(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool)
	ctx := context.Background()
	user := provisionUser(t, pool)
	device := uniqueDevice(t, "retired")

	bundle, _ := testBundle(t, device, 1, 2)
	if _, _, err := svc.RegisterDevice(ctx, user, bundle); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	if err := svc.DeleteDevice(ctx, user, device); err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}
	devs, err := svc.Devices(ctx, user)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devs) != 0 {
		t.Fatalf("want 0 devices after delete, got %d", len(devs))
	}
	// The queue cascaded: consuming now 404s.
	if _, err := svc.ConsumePrekey(ctx, user, device); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("expected ErrDeviceNotFound after delete, got %v", err)
	}
	if err := svc.DeleteDevice(ctx, user, device); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("double delete: expected ErrDeviceNotFound, got %v", err)
	}
}

func TestDirectory(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool)
	ctx := context.Background()
	user := provisionUser(t, pool)
	device := uniqueDevice(t, "listed")

	bundle, _ := testBundle(t, device, 1)
	if _, _, err := svc.RegisterDevice(ctx, user, bundle); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	dir, err := svc.Directory(ctx, user)
	if err != nil {
		t.Fatalf("Directory: %v", err)
	}
	if len(dir) != 1 {
		t.Fatalf("want 1 directory entry, got %d", len(dir))
	}
	d := dir[0]
	if d.DeviceID != device || d.RegistrationID != 4242 || d.AvailablePrekeys != 1 {
		t.Fatalf("unexpected directory entry: %+v", d)
	}
	// The public key material round-trips byte-exact through base64.
	if d.IdentityKey != base64.StdEncoding.EncodeToString(bundle.IdentityKey) {
		t.Fatal("identity_key mismatch")
	}
	if d.SignedPrekey != base64.StdEncoding.EncodeToString(bundle.SignedPrekey) {
		t.Fatal("signed_prekey mismatch")
	}
	if d.SignedPrekeySig != base64.StdEncoding.EncodeToString(bundle.SignedPrekeySig) {
		t.Fatal("signed_prekey_sig mismatch")
	}

	// Unknown user → empty list, not an error (no existence oracle).
	other := provisionUser(t, pool)
	dir, err = svc.Directory(ctx, other)
	if err != nil {
		t.Fatalf("Directory(unknown user): %v", err)
	}
	if len(dir) != 0 {
		t.Fatalf("want empty directory for fresh user, got %d", len(dir))
	}
}

// TestToBundleEncoding is a pure unit test for the base64 layer: the
// server must reject undecodable key fields before any DB touch, and
// pinpoint the bad field.
func TestToBundleEncoding(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(make([]byte, 32))
	in := registerBody{
		DeviceID: "d1", RegistrationID: 1,
		IdentityKey: good, SignedPrekey: good,
		SignedPrekeyID: 1, SignedPrekeySig: base64.StdEncoding.EncodeToString(make([]byte, 64)),
		OneTimePrekeys: []prekeyBody{{ID: 1, PublicKey: "!!!not-base64!!!"}},
	}
	_, err := toBundle(in)
	var kee *keyEncodingError
	if !errors.As(err, &kee) {
		t.Fatalf("expected *keyEncodingError, got %v", err)
	}
	if kee.field != "one_time_prekeys.public_key" {
		t.Fatalf("wrong field reported: %q", kee.field)
	}
}
