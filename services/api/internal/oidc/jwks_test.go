// SPDX-License-Identifier: AGPL-3.0-or-later
package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testKey bundles a generated RSA key with its JWKS representation.
type testKey struct {
	priv *rsa.PrivateKey
	kid  string
}

func mustKey(t *testing.T, kid string) testKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return testKey{priv: priv, kid: kid}
}

// jwksEntry renders the public half of the key as a JWKS entry.
func (k testKey) jwksEntry() map[string]string {
	return map[string]string{
		"kty": "RSA",
		"kid": k.kid,
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(k.priv.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.priv.E)).Bytes()),
	}
}

// serveJWKS starts an httptest server returning a JWKS document with the
// given keys.
func serveJWKS(t *testing.T, keys ...testKey) *httptest.Server {
	t.Helper()
	entries := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, k.jwksEntry())
	}
	doc, err := json.Marshal(map[string]any{"keys": entries})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// signToken creates an RS256 JWT with the given claims, signed by key.
func signToken(t *testing.T, key testKey, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf(`{"alg":"RS256","typ":"JWT","kid":%q}`, key.kid)))
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	input := header + "." + payload
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key.priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func baseClaims(sub string) map[string]any {
	now := time.Now()
	return map[string]any{
		"sub": sub,
		"iss": "https://issuer.example",
		"aud": "sutra-api",
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

func testVerifier(t *testing.T, srv *httptest.Server) *Verifier {
	t.Helper()
	return NewVerifier(srv.URL, "https://issuer.example", "sutra-api")
}

func TestVerifyValidToken(t *testing.T) {
	key := mustKey(t, "key-1")
	srv := serveJWKS(t, key)
	v := testVerifier(t, srv)

	claims, err := v.VerifyToken(context.Background(), signToken(t, key, baseClaims("user-123")))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Subject != "user-123" {
		t.Fatalf("subject = %q, want user-123", claims.Subject)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != "sutra-api" {
		t.Fatalf("audience = %v", claims.Audience)
	}
}

func TestVerifyExpiredToken(t *testing.T) {
	key := mustKey(t, "key-1")
	srv := serveJWKS(t, key)
	v := testVerifier(t, srv)

	claims := baseClaims("user-123")
	claims["exp"] = time.Now().Add(-2 * time.Hour).Unix() // past the leeway

	_, err := v.VerifyToken(context.Background(), signToken(t, key, claims))
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expiry error, got %v", err)
	}
}

func TestVerifyWrongSignature(t *testing.T) {
	good := mustKey(t, "key-1")
	evil := mustKey(t, "key-1") // same kid, different key material
	srv := serveJWKS(t, good)
	v := testVerifier(t, srv)

	// Sign with the attacker's key but present the legitimate kid: the
	// signature must not verify against the JWKS key.
	_, err := v.VerifyToken(context.Background(), signToken(t, evil, baseClaims("user-123")))
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected signature error, got %v", err)
	}
}

func TestVerifyUnknownKid(t *testing.T) {
	key := mustKey(t, "key-1")
	srv := serveJWKS(t, key)
	v := testVerifier(t, srv)

	rotated := mustKey(t, "key-2") // not in the JWKS
	_, err := v.VerifyToken(context.Background(), signToken(t, rotated, baseClaims("user-123")))
	if err == nil || !strings.Contains(err.Error(), "unknown key id") {
		t.Fatalf("expected unknown-kid error, got %v", err)
	}
}

func TestVerifyWrongAudience(t *testing.T) {
	key := mustKey(t, "key-1")
	srv := serveJWKS(t, key)
	v := testVerifier(t, srv)

	claims := baseClaims("user-123")
	claims["aud"] = "someone-elses-api"

	_, err := v.VerifyToken(context.Background(), signToken(t, key, claims))
	if err == nil || !strings.Contains(err.Error(), "audience") {
		t.Fatalf("expected audience error, got %v", err)
	}
}

func TestVerifyWrongIssuer(t *testing.T) {
	key := mustKey(t, "key-1")
	srv := serveJWKS(t, key)
	v := testVerifier(t, srv)

	claims := baseClaims("user-123")
	claims["iss"] = "https://evil-issuer.example"

	_, err := v.VerifyToken(context.Background(), signToken(t, key, claims))
	if err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("expected issuer error, got %v", err)
	}
}

func TestVerifyMalformedToken(t *testing.T) {
	key := mustKey(t, "key-1")
	srv := serveJWKS(t, key)
	v := testVerifier(t, srv)

	for _, bad := range []string{"", "not.a", "a.b.c.d", "e30.e30."} {
		if _, err := v.VerifyToken(context.Background(), bad); err == nil {
			t.Fatalf("expected error for token %q", bad)
		}
	}
	// alg:none must be rejected.
	noneTok := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + "."
	if _, err := v.VerifyToken(context.Background(), noneTok); err == nil {
		t.Fatal("expected rejection of alg:none")
	}
}

func TestSubjectHeaderHandling(t *testing.T) {
	key := mustKey(t, "key-1")
	srv := serveJWKS(t, key)
	v := testVerifier(t, srv)
	tok := signToken(t, key, baseClaims("user-123"))

	sub, err := v.Subject(context.Background(), "Bearer "+tok)
	if err != nil || sub != "user-123" {
		t.Fatalf("Subject = %q, %v", sub, err)
	}
	for _, bad := range []string{"", "Bearer", "Bearer ", "Token " + tok} {
		if _, err := v.Subject(context.Background(), bad); err == nil {
			t.Fatalf("expected error for header %q", bad)
		}
	}
}

func TestKeyRotationRefresh(t *testing.T) {
	oldKey := mustKey(t, "key-1")
	newKey := mustKey(t, "key-2")

	// Mutable JWKS: starts with only the old key, then rotates.
	keys := []testKey{oldKey}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entries := make([]map[string]string, 0, len(keys))
		for _, k := range keys {
			entries = append(entries, k.jwksEntry())
		}
		doc, _ := json.Marshal(map[string]any{"keys": entries})
		_, _ = w.Write(doc)
	}))
	t.Cleanup(srv.Close)

	v := testVerifier(t, srv)
	// First token primes the cache with the old key.
	if _, err := v.VerifyToken(context.Background(), signToken(t, oldKey, baseClaims("u1"))); err != nil {
		t.Fatalf("prime: %v", err)
	}
	// Rotate: the next token's kid is unknown, so the verifier must refresh.
	keys = []testKey{newKey}
	claims, err := v.VerifyToken(context.Background(), signToken(t, newKey, baseClaims("u2")))
	if err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	if claims.Subject != "u2" {
		t.Fatalf("subject = %q", claims.Subject)
	}
}
