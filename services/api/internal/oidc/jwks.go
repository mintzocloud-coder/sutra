// SPDX-License-Identifier: AGPL-3.0-or-later
// Package oidc validates JWT access tokens against the OIDC issuer's JWKS
// endpoint (Hydra in production). Hydra is the single identity issuer for
// Sutra: one identity across all modules.
//
// Verification is done with the standard library only (crypto/rsa +
// encoding/base64): no third-party JWT library, which keeps the dependency
// surface minimal and fully AGPL-compatible.
package oidc

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// jwk is the subset of an RFC 7517 JSON Web Key we need for RSA verification.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"` // base64url-encoded modulus
	E   string `json:"e"` // base64url-encoded exponent
}

// jwksDoc is an RFC 7517 JSON Web Key Set document.
type jwksDoc struct {
	Keys []jwk `json:"keys"`
}

// Claims is the validated set of claims carried by an access token.
type Claims struct {
	Subject   string         // sub
	Issuer    string         // iss
	Audience  []string       // aud (normalized to a list)
	ExpiresAt time.Time      // exp
	IssuedAt  time.Time      // iat
	Extra     map[string]any // any other registered/private claims
}

// expiryLeeway tolerates clock skew between the API and the issuer.
const expiryLeeway = 60 * time.Second

// jwksTTL is how long fetched keys are cached before a background refresh.
const jwksTTL = 15 * time.Minute

// Verifier validates bearer tokens against a JWKS endpoint. It is safe for
// concurrent use. Keys are fetched lazily on first verification (so the API
// can boot before Hydra is up) and refreshed when the cache is stale or a
// token's kid is unknown (key rotation).
type Verifier struct {
	jwksURL  string
	issuer   string
	audience string
	client   *http.Client

	mu      sync.RWMutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// NewVerifier returns a Verifier for the given JWKS URL, expected issuer
// (iss claim) and expected audience (aud claim). Empty issuer/audience
// disables that claim check — only used in tests.
func NewVerifier(jwksURL, issuer, audience string) *Verifier {
	return &Verifier{
		jwksURL:  jwksURL,
		issuer:   issuer,
		audience: audience,
		client:   &http.Client{Timeout: 10 * time.Second},
		keys:     map[string]*rsa.PublicKey{},
	}
}

// keyFor returns the RSA public key for kid, refreshing the JWKS cache when
// the key is missing or the cache is stale.
func (v *Verifier) keyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	key, ok := v.keys[kid]
	stale := time.Since(v.fetched) > jwksTTL
	v.mu.RUnlock()

	if ok && !stale {
		return key, nil
	}
	if err := v.refresh(ctx); err != nil {
		return nil, fmt.Errorf("jwks refresh: %w", err)
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	key, ok = v.keys[kid]
	if !ok {
		return nil, fmt.Errorf("unknown key id %q", kid)
	}
	return key, nil
}

// refresh fetches the JWKS document and replaces the cached keys. It holds
// the write lock for the whole fetch so concurrent verifications serialize
// on a single refresh instead of thundering-herding the issuer.
func (v *Verifier) refresh(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks endpoint returned %s", resp.Status)
	}
	var doc jwksDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue // not a usable signing key for us
		}
		pub, err := rsaPublicKey(k)
		if err != nil {
			return fmt.Errorf("bad jwk %q: %w", k.Kid, err)
		}
		keys[k.Kid] = pub
	}
	v.keys = keys
	v.fetched = time.Now()
	return nil
}

// rsaPublicKey converts an RSA JWK (base64url modulus + exponent) into a
// *rsa.PublicKey.
func rsaPublicKey(k jwk) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("bad modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("bad exponent: %w", err)
	}
	if len(eBytes) > 8 {
		return nil, errors.New("exponent too large")
	}
	// e is a big-endian unsigned integer; pad to 8 bytes for uint64.
	var padded [8]byte
	copy(padded[8-len(eBytes):], eBytes)
	e := int(binary.BigEndian.Uint64(padded[:]))
	if e <= 0 {
		return nil, errors.New("non-positive exponent")
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: e,
	}, nil
}

// VerifyToken parses and fully verifies a JWT: structure, RS256 signature
// against the issuer's JWKS, expiry (with skew leeway), issuer, and
// audience. Returns the validated claims.
func (v *Verifier) VerifyToken(ctx context.Context, token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, errors.New("malformed token")
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("bad token header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("bad token header: %w", err)
	}
	// RS256 only. Rejecting `alg: none` and algorithm-confusion attacks is
	// the whole point of pinning the algorithm here.
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("unsupported algorithm %q", header.Alg)
	}
	if header.Kid == "" {
		return nil, errors.New("missing key id")
	}

	pub, err := v.keyFor(ctx, header.Kid)
	if err != nil {
		return nil, err
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("bad token signature: %w", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return nil, errors.New("invalid signature")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("bad token payload: %w", err)
	}
	claims, err := parseClaims(payloadBytes)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	if now.Add(expiryLeeway).After(claims.ExpiresAt) {
		return nil, errors.New("token expired")
	}
	if v.issuer != "" && claims.Issuer != v.issuer {
		return nil, fmt.Errorf("unexpected issuer %q", claims.Issuer)
	}
	if v.audience != "" && !audienceContains(claims.Audience, v.audience) {
		return nil, fmt.Errorf("audience %q not granted", v.audience)
	}
	return claims, nil
}

// parseClaims decodes the registered claims we enforce and keeps the rest
// in Extra for handlers that need them.
func parseClaims(payload []byte) (*Claims, error) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("bad token payload: %w", err)
	}
	c := &Claims{Extra: map[string]any{}}
	for k, val := range raw {
		switch k {
		case "sub":
			c.Subject, _ = val.(string)
		case "iss":
			c.Issuer, _ = val.(string)
		case "aud":
			c.Audience = normalizeAudience(val)
		case "exp":
			c.ExpiresAt = numericDate(val)
		case "iat":
			c.IssuedAt = numericDate(val)
		default:
			c.Extra[k] = val
		}
	}
	if c.Subject == "" {
		return nil, errors.New("missing subject")
	}
	if c.ExpiresAt.IsZero() {
		return nil, errors.New("missing expiry")
	}
	return c, nil
}

// normalizeAudience accepts the two legal forms of the aud claim: a single
// string or an array of strings.
func normalizeAudience(v any) []string {
	switch a := v.(type) {
	case string:
		return []string{a}
	case []any:
		out := make([]string, 0, len(a))
		for _, s := range a {
			if str, ok := s.(string); ok {
				out = append(out, str)
			}
		}
		return out
	default:
		return nil
	}
}

// numericDate converts a NumericDate (seconds since epoch, possibly
// fractional) into a time.Time.
func numericDate(v any) time.Time {
	var secs float64
	switch n := v.(type) {
	case float64:
		secs = n
	case json.Number:
		secs, _ = n.Float64()
	case int64:
		secs = float64(n)
	default:
		return time.Time{}
	}
	return time.Unix(int64(secs), int64((secs-float64(int64(secs)))*1e9)).UTC()
}

func audienceContains(aud []string, want string) bool {
	for _, a := range aud {
		if a == want {
			return true
		}
	}
	return false
}

// VerifyHeader extracts the bearer token from an Authorization header and
// verifies it, returning the validated claims. Prefer
// middleware.RequireAuth + middleware.SubjectFromContext for HTTP handlers.
func (v *Verifier) VerifyHeader(ctx context.Context, authHeader string) (*Claims, error) {
	token, ok := strings.CutPrefix(authHeader, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return nil, errors.New("missing bearer token")
	}
	return v.VerifyToken(ctx, token)
}

// Subject is a convenience helper: it extracts the bearer token from an
// Authorization header, verifies it, and returns the subject.
func (v *Verifier) Subject(ctx context.Context, authHeader string) (string, error) {
	claims, err := v.VerifyHeader(ctx, authHeader)
	if err != nil {
		return "", err
	}
	return claims.Subject, nil
}
