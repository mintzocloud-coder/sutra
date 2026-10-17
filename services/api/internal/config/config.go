// SPDX-License-Identifier: AGPL-3.0-or-later
// Package config loads service configuration from the environment.
package config

import (
	"os"
	"strconv"
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
	ShutdownTimeout int // seconds
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Load reads configuration from the environment with sane defaults for
// `docker compose up` (see docker-compose.yml).
func Load() Config {
	timeout, _ := strconv.Atoi(getEnv("SHUTDOWN_TIMEOUT", "10"))
	return Config{
		Port:            getEnv("PORT", "8080"),
		DatabaseURL:     getEnv("DATABASE_URL", "postgres://sutra:sutra@postgres:5432/sutra?sslmode=disable"),
		ValkeyAddr:      getEnv("VALKEY_ADDR", "valkey:6379"),
		HydraJWKSURL:    getEnv("HYDRA_JWKS_URL", "http://hydra:4444/.well-known/jwks.json"),
		HydraIssuer:     getEnv("HYDRA_ISSUER", "http://hydra:4444"),
		JWTAudience:     getEnv("JWT_AUDIENCE", "sutra-api"),
		AuthBypass:      getEnv("AUTH_BYPASS", "") == "1",
		BypassSubject:   getEnv("AUTH_BYPASS_SUBJECT", "dev-user"),
		ShutdownTimeout: timeout,
	}
}
