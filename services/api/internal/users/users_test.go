// SPDX-License-Identifier: AGPL-3.0-or-later
package users

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/oidc"
)

// Pure unit tests: claim → display-name seeding. No database needed.

func TestDisplayNameFromClaims(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]any
		want  string
	}{
		{"name wins", map[string]any{"name": "Alice", "preferred_username": "alice99"}, "Alice"},
		{"username fallback", map[string]any{"preferred_username": "alice99"}, "alice99"},
		{"empty name falls through", map[string]any{"name": "", "preferred_username": "alice99"}, "alice99"},
		{"non-string ignored", map[string]any{"name": 42}, ""},
		{"no claims", map[string]any{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &oidc.Claims{Subject: "s", Extra: tc.extra}
			if got := displayNameFromClaims(c); got != tc.want {
				t.Fatalf("displayNameFromClaims = %q, want %q", got, tc.want)
			}
		})
	}
	if got := displayNameFromClaims(nil); got != "" {
		t.Fatalf("displayNameFromClaims(nil) = %q, want %q", got, "")
	}
}

// Integration tests against a real PostgreSQL (migrations applied).
// Run when TEST_DATABASE_URL is set; otherwise skip cleanly.

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping users integration test")
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

// uniqueSubject keeps test rows isolated across runs on a shared database.
func uniqueSubject(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return t.Name() + "-" + hex.EncodeToString(b[:])
}

func TestProvisionJITIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	sub := uniqueSubject(t)

	// First call: creates the row, seeds the display name.
	u1, err := Provision(ctx, pool, sub, "Alice")
	if err != nil {
		t.Fatalf("Provision (first): %v", err)
	}
	if u1.ID == "" {
		t.Fatal("provisioned user has empty id")
	}
	if u1.Subject != sub {
		t.Fatalf("subject = %q, want %q", u1.Subject, sub)
	}
	if u1.DisplayName == nil || *u1.DisplayName != "Alice" {
		t.Fatalf("display_name = %v, want Alice", u1.DisplayName)
	}
	if u1.Handle != nil {
		t.Fatalf("handle = %q, want nil (unclaimed on JIT)", *u1.Handle)
	}
	if u1.CreatedAt.IsZero() || u1.UpdatedAt.IsZero() {
		t.Fatal("timestamps not populated")
	}

	// Second call: returns the SAME row; a different name seed does NOT
	// overwrite the stored display name.
	u2, err := Provision(ctx, pool, sub, "Bob")
	if err != nil {
		t.Fatalf("Provision (second): %v", err)
	}
	if u2.ID != u1.ID {
		t.Fatalf("second call returned different row: %q vs %q", u2.ID, u1.ID)
	}
	if u2.DisplayName == nil || *u2.DisplayName != "Alice" {
		t.Fatalf("display_name overwritten: %v, want Alice", u2.DisplayName)
	}

	// Empty name seed stays NULL.
	u3, err := Provision(ctx, pool, uniqueSubject(t), "")
	if err != nil {
		t.Fatalf("Provision (empty name): %v", err)
	}
	if u3.DisplayName != nil {
		t.Fatalf("display_name = %q, want nil", *u3.DisplayName)
	}

	// Empty subject is a hard error, never a row.
	if _, err := Provision(ctx, pool, "", "x"); err != ErrEmptySubject {
		t.Fatalf("Provision(\"\") = %v, want ErrEmptySubject", err)
	}
}

func TestProvisionConcurrentIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	sub := uniqueSubject(t)

	// Eight replicas racing to provision the same new subject must
	// converge on ONE row (ON CONFLICT DO NOTHING), not error.
	const racers = 8
	ids := make([]string, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u, err := Provision(ctx, pool, sub, "Racer")
			if err == nil {
				ids[i] = u.ID
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("racer %d got row %q, racer 0 got %q", i, ids[i], ids[0])
		}
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE subject = $1`, sub).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if n != 1 {
		t.Fatalf("subject has %d rows, want 1", n)
	}
}

func TestMeHandlerIntegration(t *testing.T) {
	pool := testPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Exercise the real auth middleware (bypass mode stamps a subject)
	// in front of the handler, like main.go does.
	sub := uniqueSubject(t)
	auth := middleware.RequireAuth(oidc.NewVerifier("", "", ""), log,
		middleware.AuthOptions{Bypass: true, BypassSubject: sub})
	srv := httptest.NewServer(auth(MeHandler(pool, log)))
	defer srv.Close()

	get := func() (int, map[string]any) {
		t.Helper()
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.StatusCode, body
	}

	// First call: 200, creates the row for the bypass subject.
	status, body := get()
	if status != http.StatusOK {
		t.Fatalf("first call status = %d, want 200 (body %v)", status, body)
	}
	if body["subject"] != sub {
		t.Fatalf("subject = %v, want %q", body["subject"], sub)
	}
	id1, _ := body["id"].(string)
	if id1 == "" {
		t.Fatal("first call returned empty id")
	}

	// Second call: same row.
	status, body = get()
	if status != http.StatusOK {
		t.Fatalf("second call status = %d, want 200", status)
	}
	if body["id"] != id1 {
		t.Fatalf("second call id = %v, want %q (row not stable)", body["id"], id1)
	}
}
