// SPDX-License-Identifier: AGPL-3.0-or-later
// Package oidc validates JWT access tokens against the Hydra JWKS endpoint.
// Hydra is the single OIDC issuer for Sutra: one identity across all modules.
// Full verification (signature + claims) lands with issue #4; this file holds
// the seam so services can depend on it now.
package oidc

import (
	"errors"
	"strings"
)

// Verifier validates access tokens. Wire the JWKS URL from config.
type Verifier struct {
	jwksURL string
}

// NewVerifier returns a Verifier for the given JWKS URL.
func NewVerifier(jwksURL string) *Verifier {
	return &Verifier{jwksURL: jwksURL}
}

// Subject extracts the bearer token from an Authorization header and returns
// the subject. TODO(#4): verify signature via JWKS, check exp/iss/aud.
func (v *Verifier) Subject(authHeader string) (string, error) {
	token, ok := strings.CutPrefix(authHeader, "Bearer ")
	if !ok || token == "" {
		return "", errors.New("missing bearer token")
	}
	// Unverified stub — structural only until issue #4 lands.
	return "stub-subject", nil
}
