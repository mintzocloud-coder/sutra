// SPDX-License-Identifier: AGPL-3.0-or-later
// Package config loads service configuration from the environment.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
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
	OTPProvider     string   // SMS gateway for phone OTP: "stub" until DLT
	// registration post-incorporation (see internal/otp, docs/identity.md)
	PostVelocityCap int // posts per rolling hour per author (D3); <=0 = default 30
	// Social graph (D2, D6): abuse guards for the follow path.
	FollowVelocityCap int // new follows per rolling hour per user; <=0 = default 200
	// Home feed (D3b): hybrid fan-out / fan-in on Valkey sorted sets.
	FanoutThreshold int // authors with MORE followers than this are served
	// pull-on-read instead of fan-out-on-write; <=0 = default 10000
	TimelineMax int // entries kept per user home timeline; <=0 = default 2000
	// Recommendations v1 (E5): home-feed ranking weights — recency
	// decay + follow-graph boost + engagement velocity. The ranker
	// normalizes them to sum 1; non-positive/degenerate values fall
	// back to the defaults (0.5/0.2/0.3).
	RankWRecency    float64
	RankWGraph      float64
	RankWEngagement float64
	// Media pipeline (D4): SeaweedFS object storage for uploads.
	SeaweedMasterAddr  string // master for dir/assign, host:port; e.g. seaweedfs:9333
	MediaPublicBaseURL string // client-reachable volume base URL for
	// direct PUTs/downloads, e.g. http://localhost:8080 in dev
	// Transcode worker (D4b): background HLS pipeline draining
	// media:transcode.
	TranscodeGroup         string
	TranscodeConsumer      string
	TranscodeMaxAttempts   int
	TranscodeBackoffBase   time.Duration
	TranscodeBackoffMax    time.Duration
	TranscodeSweepInterval time.Duration
	// Realtime events (D5): publish post:new / follower:new to the chat
	// service's internal signed endpoint. Empty ChatInternalToken disables
	// eventing entirely (dev default); a token with a bad URL makes the
	// API refuse to boot (fail-closed).
	ChatInternalURL   string // e.g. http://chat:4000; path /internal/events appended
	ChatInternalToken string // shared service-to-service secret; never commit
	// AI core (E1): the Python AI service behind /v1/ai/*. Empty
	// AIServiceToken disables the proxy (503 service_unavailable); a
	// token with a bad URL makes the API refuse to boot (fail-closed).
	AIServiceURL   string // e.g. http://ai:8000; the /v1/ai prefix is stripped
	AIServiceToken string // shared service-to-service secret; never commit
	// Search (F2): self-hosted Meilisearch CE for product search.
	// Empty SearchMasterKey disables indexing (dev default — the API
	// logs a WARN at startup); a key with a bad URL refuses to boot
	// (fail-closed), mirroring the AI proxy contract.
	SearchURL       string // e.g. http://meilisearch:7700
	SearchMasterKey string // Meilisearch master key; never commit
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
		OTPProvider:     getEnv("OTP_PROVIDER", "stub"),
		PostVelocityCap: func() int {
			n, _ := strconv.Atoi(getEnv("POST_VELOCITY_PER_HOUR", "30"))
			return n
		}(),
		FollowVelocityCap: func() int {
			n, _ := strconv.Atoi(getEnv("FOLLOW_VELOCITY_PER_HOUR", "200"))
			return n
		}(),
		FanoutThreshold: func() int {
			n, _ := strconv.Atoi(getEnv("FANOUT_THRESHOLD", "10000"))
			return n
		}(),
		TimelineMax: func() int {
			n, _ := strconv.Atoi(getEnv("FEED_TIMELINE_MAX", "2000"))
			return n
		}(),
		// Recommendations v1 (E5): ranking term weights.
		RankWRecency: func() float64 {
			f, _ := strconv.ParseFloat(getEnv("REC_W_RECENCY", "0.5"), 64)
			return f
		}(),
		RankWGraph: func() float64 {
			f, _ := strconv.ParseFloat(getEnv("REC_W_GRAPH", "0.2"), 64)
			return f
		}(),
		RankWEngagement: func() float64 {
			f, _ := strconv.ParseFloat(getEnv("REC_W_ENGAGEMENT", "0.3"), 64)
			return f
		}(),
		SeaweedMasterAddr:  getEnv("SEAWEEDFS_MASTER_ADDR", "seaweedfs:9333"),
		MediaPublicBaseURL: getEnv("MEDIA_PUBLIC_BASE_URL", "http://seaweedfs:8080"),
		// Transcode worker (D4b): background video pipeline.
		TranscodeGroup: func() string { return getEnv("TRANSCODE_GROUP", "transcode-workers") }(),
		TranscodeConsumer: func() string {
			if c := getEnv("TRANSCODE_CONSUMER", ""); c != "" {
				return c
			}
			if h, err := os.Hostname(); err == nil && h != "" {
				return h
			}
			return "worker-1"
		}(),
		TranscodeMaxAttempts: func() int {
			n, _ := strconv.Atoi(getEnv("TRANSCODE_MAX_ATTEMPTS", "5"))
			if n < 1 {
				n = 5
			}
			return n
		}(),
		TranscodeBackoffBase: func() time.Duration {
			d, _ := time.ParseDuration(getEnv("TRANSCODE_BACKOFF_BASE", "30s"))
			if d <= 0 {
				d = 30 * time.Second
			}
			return d
		}(),
		TranscodeBackoffMax: func() time.Duration {
			d, _ := time.ParseDuration(getEnv("TRANSCODE_BACKOFF_MAX", "30m"))
			if d <= 0 {
				d = 30 * time.Minute
			}
			return d
		}(),
		TranscodeSweepInterval: func() time.Duration {
			d, _ := time.ParseDuration(getEnv("TRANSCODE_SWEEP_INTERVAL", "5m"))
			if d <= 0 {
				d = 5 * time.Minute
			}
			return d
		}(),
		ChatInternalURL:   getEnv("CHAT_INTERNAL_URL", "http://chat:4000"),
		ChatInternalToken: getEnv("CHAT_INTERNAL_TOKEN", ""),
		AIServiceURL:      getEnv("AI_SERVICE_URL", "http://ai:8000"),
		AIServiceToken:    getEnv("AI_SERVICE_TOKEN", ""),
		SearchURL:         getEnv("SEARCH_URL", "http://meilisearch:7700"),
		SearchMasterKey:   getEnv("SEARCH_MASTER_KEY", ""),
	}
}
