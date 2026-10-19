// SPDX-License-Identifier: AGPL-3.0-or-later
package miniapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Pure unit tests for the runtime contract (G3 part 2): bridge
// allowlist gating and the signed launch token. DB-gated launch tests
// live in the integration section at the bottom.

func manifestWith(perms ...string) Manifest {
	return Manifest{Permissions: perms}
}

func TestBridgeMethodAllowed(t *testing.T) {
	empty := manifestWith()
	cam := manifestWith("camera")
	all := manifestWith("camera", "microphone", "geolocation", "notifications", "payments", "storage", "share", "clipboard", "vibration", "biometric")
	cases := []struct {
		name   string
		m      Manifest
		method string
		want   bool
	}{
		{"init needs no permission", empty, "init.getData", true},
		{"camera declared", cam, "camera.capture", true},
		{"camera undeclared", empty, "camera.capture", false},
		{"geolocation undeclared", empty, "geolocation.getPosition", false},
		{"geolocation declared", all, "geolocation.getPosition", true},
		{"payments declared", all, "payments.initiate", true},
		{"payments undeclared", cam, "payments.initiate", false},
		{"storage declared", all, "storage.setItem", true},
		{"mic stop with mic declared", all, "microphone.stopListening", true},
		{"biometric undeclared", cam, "biometric.authenticate", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MethodAllowed(tc.m, tc.method)
			if err != nil {
				t.Fatalf("MethodAllowed(%q) error = %v", tc.method, err)
			}
			if got != tc.want {
				t.Errorf("MethodAllowed(%q) = %v, want %v", tc.method, got, tc.want)
			}
		})
	}
}

func TestBridgeMethodUnknownIsErrorNotDeny(t *testing.T) {
	// An unknown method must fail loudly (developer bug), never be
	// silently denied.
	if _, err := MethodAllowed(manifestWith(), "camera.explodify"); !errors.Is(err, ErrUnknownBridgeMethod) {
		t.Fatalf("MethodAllowed(unknown) = %v, want ErrUnknownBridgeMethod", err)
	}
	if _, err := MethodAllowed(manifestWith("camera"), "payments"); !errors.Is(err, ErrUnknownBridgeMethod) {
		t.Fatalf("MethodAllowed(partial name) = %v, want ErrUnknownBridgeMethod", err)
	}
}

func TestBridgeAllowlistCoversManifestPermissions(t *testing.T) {
	// Every permission the registry allows a manifest to declare must
	// gate at least one bridge method — a dead permission is a contract
	// bug (the manifest promises something the bridge never grants).
	used := map[string]bool{}
	for _, m := range BridgeMethods {
		if m.Permission != "" {
			used[m.Permission] = true
		}
	}
	for p := range BridgePermissions {
		if !used[p] {
			t.Errorf("manifest permission %q gates no bridge method", p)
		}
	}
}

func randUUID(t *testing.T) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	// pgtype.UUID is [16]byte-backed; derive a v4-ish value from time —
	// only uniqueness within the test matters.
	now := time.Now().UnixNano()
	for i := 0; i < 16; i++ {
		id.Bytes[i] = byte(now >> (i % 8))
	}
	id.Valid = true
	return id
}

func TestLaunchTokenRoundTrip(t *testing.T) {
	secret := []byte("test-launch-secret-32-bytes-minimum")
	uid, aid := randUUID(t), randUUID(t)
	token, exp, err := MintLaunchToken(secret, uid, aid, LaunchTokenTTL)
	if err != nil {
		t.Fatalf("MintLaunchToken: %v", err)
	}
	if !exp.After(time.Now().Add(4 * time.Minute)) {
		t.Errorf("expiry %v is not ~5min out", exp)
	}
	gotUID, gotAID, err := VerifyLaunchToken(secret, token)
	if err != nil {
		t.Fatalf("VerifyLaunchToken: %v", err)
	}
	if gotUID != uid || gotAID != aid {
		t.Errorf("identity mismatch: got (%s,%s) want (%s,%s)", gotUID, gotAID, uid, aid)
	}
}

func TestLaunchTokenTamper(t *testing.T) {
	secret := []byte("test-launch-secret-32-bytes-minimum")
	token, _, err := MintLaunchToken(secret, randUUID(t), randUUID(t), LaunchTokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	forged := token[:len(token)-4] + "AAAA"
	if _, _, err := VerifyLaunchToken(secret, forged); !errors.Is(err, ErrLaunchTokenInvalid) {
		t.Errorf("forged signature = %v, want ErrLaunchTokenInvalid", err)
	}
	wrongSecret := []byte("a-different-secret-000000000000000")
	if _, _, err := VerifyLaunchToken(wrongSecret, token); !errors.Is(err, ErrLaunchTokenInvalid) {
		t.Errorf("wrong secret = %v, want ErrLaunchTokenInvalid", err)
	}
}

func TestLaunchTokenExpiry(t *testing.T) {
	secret := []byte("test-launch-secret-32-bytes-minimum")
	token, _, err := MintLaunchToken(secret, randUUID(t), randUUID(t), -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyLaunchToken(secret, token); !errors.Is(err, ErrLaunchTokenExpired) {
		t.Errorf("expired token = %v, want ErrLaunchTokenExpired", err)
	}
}

func TestLaunchTokenMalformed(t *testing.T) {
	secret := []byte("test-launch-secret-32-bytes-minimum")
	for _, tok := range []string{"", "v1", "v1..", "v2.a.b", "not-a-token"} {
		if _, _, err := VerifyLaunchToken(secret, tok); !errors.Is(err, ErrLaunchTokenInvalid) {
			t.Errorf("VerifyLaunchToken(%q) = %v, want ErrLaunchTokenInvalid", tok, err)
		}
	}
}

func TestLaunchTokenNeedsSecret(t *testing.T) {
	if _, _, err := MintLaunchToken(nil, randUUID(t), randUUID(t), LaunchTokenTTL); !errors.Is(err, ErrLaunchSecretMissing) {
		t.Errorf("MintLaunchToken(nil secret) = %v, want ErrLaunchSecretMissing", err)
	}
	if _, _, err := VerifyLaunchToken(nil, "v1.a.b"); !errors.Is(err, ErrLaunchSecretMissing) {
		t.Errorf("VerifyLaunchToken(nil secret) = %v, want ErrLaunchSecretMissing", err)
	}
}

// Integration test for the launch gate (G3 part 2) against real PG.
// Run when TEST_DATABASE_URL is set; otherwise skip cleanly.

func TestLaunchGateIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	secret := []byte("integration-launch-secret-32-bytes!")

	dev := testUser(t, pool)
	user := testUser(t, pool)
	slug := uniqueStr(t, "launch")
	app := registerApp(t, svc, dev, slug)

	// No secret → fail closed, before any state check.
	if _, err := svc.Launch(ctx, dev, app.ID, nil); !errors.Is(err, ErrLaunchSecretMissing) {
		t.Fatalf("launch without secret = %v, want ErrLaunchSecretMissing", err)
	}

	// Owner may launch the draft (preview) without installing.
	launch, err := svc.Launch(ctx, dev, app.ID, secret)
	if err != nil {
		t.Fatalf("owner draft launch: %v", err)
	}
	if launch.App.ID != app.ID || launch.LaunchToken == "" {
		t.Fatalf("bad launch result: %+v", launch)
	}
	if _, _, err := VerifyLaunchToken(secret, launch.LaunchToken); err != nil {
		t.Fatalf("launch token does not verify: %v", err)
	}

	// Non-owner, no install edge: draft is never launchable...
	if _, err := svc.Launch(ctx, user, app.ID, secret); !errors.Is(err, ErrLaunchNotAllowed) {
		t.Fatalf("non-owner draft launch = %v, want ErrLaunchNotAllowed", err)
	}

	// ...publish, then install, then launch works.
	if _, err := svc.Transition(ctx, app.ID, dev, StatusPublished); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := svc.Launch(ctx, user, app.ID, secret); !errors.Is(err, ErrLaunchNotAllowed) {
		t.Fatalf("non-installed published launch = %v, want ErrLaunchNotAllowed", err)
	}
	if _, err := svc.Install(ctx, user, app.ID); err != nil {
		t.Fatalf("install: %v", err)
	}
	l2, err := svc.Launch(ctx, user, app.ID, secret)
	if err != nil {
		t.Fatalf("installed launch: %v", err)
	}
	gotUID, gotAID, err := VerifyLaunchToken(secret, l2.LaunchToken)
	if err != nil || gotUID != user || gotAID != app.ID {
		t.Fatalf("installed launch token identity mismatch: %v %v %v", gotUID, gotAID, err)
	}

	// Archived app: owner launch 404s like everything else.
	if _, err := svc.Transition(ctx, app.ID, dev, StatusArchived); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := svc.Launch(ctx, dev, app.ID, secret); !errors.Is(err, ErrMiniAppNotFound) {
		t.Fatalf("archived launch = %v, want ErrMiniAppNotFound", err)
	}
}
