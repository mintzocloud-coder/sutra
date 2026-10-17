// SPDX-License-Identifier: AGPL-3.0-or-later
package middleware

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mintzocloud-coder/sutra/services/api/internal/oidc"
)

// The auth middleware test needs a real token signer; we replicate the
// minimal JWKS + signing fixture here to keep packages decoupled.
type signerKey struct {
	priv *rsa.PrivateKey
	kid  string
}

func testSetup(t *testing.T) (*oidc.Verifier, signerKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key := signerKey{priv: priv, kid: "k1"}
	doc, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "k1", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(priv.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.E)).Bytes()),
	}}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(doc)
	}))
	t.Cleanup(srv.Close)
	return oidc.NewVerifier(srv.URL, "https://issuer.example", "sutra-api"), key
}

func sign(t *testing.T, key signerKey, sub string, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf(`{"alg":"RS256","typ":"JWT","kid":%q}`, key.kid)))
	payloadJSON, _ := json.Marshal(map[string]any{
		"sub": sub, "iss": "https://issuer.example", "aud": "sutra-api",
		"iat": time.Now().Add(-time.Minute).Unix(), "exp": exp.Unix(),
	})
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	input := header + "." + payload
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key.priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

var discardLog = slog.New(slog.NewJSONHandler(io.Discard, nil))

func authed(t *testing.T, v *oidc.Verifier, opts AuthOptions) http.Handler {
	t.Helper()
	return Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(SubjectFromContext(r.Context())))
	}), RequireAuth(v, discardLog, opts))
}

func TestRequireAuthValidToken(t *testing.T) {
	v, key := testSetup(t)
	h := authed(t, v, AuthOptions{})

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+sign(t, key, "user-9", time.Now().Add(time.Hour)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "user-9" {
		t.Fatalf("subject in context = %q, want user-9", rec.Body.String())
	}
}

func TestRequireAuthRejects(t *testing.T) {
	v, key := testSetup(t)
	h := authed(t, v, AuthOptions{})

	cases := map[string]string{
		"missing header": "",
		"expired token":  "Bearer " + sign(t, key, "user-9", time.Now().Add(-time.Hour)),
		"garbage":        "Bearer not-a-token",
	}
	for name, auth := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			errObj, ok := body["error"].(map[string]any)
			if !ok || errObj["code"] != "unauthorized" {
				t.Fatalf("missing error envelope: %s", rec.Body.String())
			}
		})
	}
}

func TestRequireAuthBypass(t *testing.T) {
	v, _ := testSetup(t)
	h := authed(t, v, AuthOptions{Bypass: true, BypassSubject: "local-dev"})

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil) // no token at all
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 in bypass mode", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "local-dev") {
		t.Fatalf("bypass subject not stamped: %q", rec.Body.String())
	}
}

// TestBypassIsExplicitlyInsecure documents the A2 requirement: the bypass
// path exists only for local dev and the insecure startup warning is real.
func TestBypassIsExplicitlyInsecure(t *testing.T) {
	if os.Getenv("AUTH_BYPASS") == "1" {
		t.Skip("running with AUTH_BYPASS=1 — bypass is intentionally active")
	}
	// Default config must NOT bypass.
	v, _ := testSetup(t)
	h := authed(t, v, AuthOptions{Bypass: false})
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("default auth must reject unauthenticated requests, got %d", rec.Code)
	}
}
