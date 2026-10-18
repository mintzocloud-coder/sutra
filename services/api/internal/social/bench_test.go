// SPDX-License-Identifier: AGPL-3.0-or-later
package social

// Benchmarks for the social package's pure validation hot paths (D6).
//
// These sit on every profile/follow write and read: handle claims,
// cursor encode/decode on each paginated follow listing, and user-id
// parsing on every /v1/users/{id} route. All are DB-free by design —
// the velocity-cap checks are DB-backed and are exercised by the 429
// assertions in scripts/smoke.sh instead of benchmarks.
//
// Reproduce:  cd services/api && go test -run='^$' -bench=. -benchtime=10s -count=3 ./internal/social/

import (
	"testing"
	"time"
)

// BenchmarkValidateHandle prices the compiled-regex match every handle
// claim and profile PATCH pays.
func BenchmarkValidateHandle(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !ValidateHandle("smoke_claim_42") {
			b.Fatal("valid handle rejected")
		}
	}
}

// BenchmarkValidateHandleInvalid prices the fast-reject path (a handle
// with an illegal character should fail as cheaply as it can).
func BenchmarkValidateHandleInvalid(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ValidateHandle("not a handle!") {
			b.Fatal("invalid handle accepted")
		}
	}
}

// BenchmarkEncodeCursor prices next_cursor generation: one JSON marshal
// + one base64 encode per paginated page.
func BenchmarkEncodeCursor(b *testing.B) {
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EncodeCursor(now, "0192b3c4-5d6e-7f80-90a1-b2c3d4e5f607")
	}
}

// BenchmarkDecodeCursor prices client-cursor parsing on every
// ?cursor= follow-listing request: base64 decode + JSON unmarshal +
// UUID validation.
func BenchmarkDecodeCursor(b *testing.B) {
	cur := EncodeCursor(time.Now(), "0192b3c4-5d6e-7f80-90a1-b2c3d4e5f607")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := DecodeCursor(cur); err != nil {
			b.Fatal("valid cursor rejected")
		}
	}
}

// BenchmarkParseUserID prices the {id} path-parameter parse every
// /v1/users/{id} route pays before touching the database.
func BenchmarkParseUserID(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ParseUserID("0192b3c4-5d6e-7f80-90a1-b2c3d4e5f607"); err != nil {
			b.Fatal("valid uuid rejected")
		}
	}
}
