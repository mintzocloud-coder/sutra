// SPDX-License-Identifier: AGPL-3.0-or-later
// Package config loads service configuration from the environment.
package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds the API service configuration.
type Config struct {
	Port            string
	DatabaseURL     string
	ValkeyAddr      string
	HydraJWKSURL    string
	HydraIssuer     string
	JWTAudience     string
	AuthBypass      bool
	BypassSubject   string
	DBMaxConns      int      // pgxpool MaxConns
	ShutdownTimeout int      // seconds
	RateLimitRPS    float64  // sustained requests/sec per client IP; <=0 disables
	RateLimitBurst  int      // max burst per client IP
	CORSOrigins     []string // CORS allowlist; empty = CORS disabled
	CORSMaxAge      int      // preflight cache seconds
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseCORSOrigins splits a comma-separated origins list (the CORS_ORIGINS
// env var), trimming whitespace and dropping empties.
func parseCORSOrigins(s string) []string {
	var out []string
	for _, o := range strings.Split(s, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}

// Load reads configuration from the environment with sane defaults for
// `docker compose up` (see docker-compose.yml).
func Load() Config {
	timeout, _ := strconv.Atoi(getEnv("SHUTDOWN_TIMEOUT", "10"))
	maxConns, _ := strconv.Atoi(getEnv("DB_MAX_CONNS", "25"))
	rps, _ := strconv.ParseFloat(getEnv("RATE_LIMIT_RPS", "100"), 64)
	burst, _ := strconv.Atoi(getEnv("RATE_LIMIT_BURST", "200"))
	maxAge, _ := strconv.Atoi(getEnv("CORS_MAX_AGE", "86400"))
	return Config{
		Port:            getEnv("PORT", "8080"),
		DatabaseURL:     getEnv("DATABASE_URL", "postgres://sutra:sutra@postgres:5432/sutra?sslmode=disable"),
		ValkeyAddr:      getEnv("VALKEY_ADDR", "valkey:6379"),
		HydraJWKSURL:    getEnv("HYDRA_JWKS_URL", "http://hydra:4444/.well-known/jwks.json"),
		HydraIssuer:     getEnv("HYDRA_ISSUER", "http://hydra:4444"),
		JWTAudience:     getEnv("JWT_AUDIENCE", "sutra-api"),
		AuthBypass:      getEnv("AUTH_BYPASS", "") == "1",
		BypassSubject:   getEnv("AUTH_BYPASS_SUBJECT", "dev-user"),
		DBMaxConns:      maxConns,
		ShutdownTimeout: timeout,
		RateLimitRPS:    rps,
		RateLimitBurst:  burst,
		CORSOrigins:     parseCORSOrigins(getEnv("CORS_ORIGINS", "")),
		CORSMaxAge:      maxAge,
	}
}
