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
	DBMaxConns      int     // pgxpool MaxConns
	ShutdownTimeout int     // seconds
	RateLimitRPS    float64 // sustained requests/sec per client IP; <=0 disables
	RateLimitBurst  int     // max burst per client IP
	// Commerce surface (F6 part 3): the Wave-3 marketplace endpoints
	// (sellers, products, variants, categories, search, cart, orders)
	// get an ADDITIONAL tighter per-IP budget on top of the global
	// limiter (see internal/middleware.CommerceRateLimit). <=0 disables.
	CommerceLimitRPS   float64
	CommerceLimitBurst int
	CORSOrigins        []string // CORS allowlist; empty = CORS disabled
	CORSMaxAge         int      // preflight cache seconds
	OTPProvider        string   // SMS gateway for phone OTP: "stub" until DLT
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
	// Live media (G1, Wave 4): LiveKit SFU seam. Empty LiveKitAPISecret
	// disables the whole /v1/live/* surface (dev default); a key with a
	// bad/empty secret refuses to boot fail-closed in main.go.
	LiveKitWSURL        string // client-reachable LiveKit WS URL, e.g. ws://localhost:7880
	LiveKitAPIKey       string // LiveKit API key (iss claim of minted tokens)
	LiveKitAPISecret    string // LiveKit API secret (HS256 signing key); never commit
	LiveTokenTTLMinutes int    // participant token lifetime; <=0 = 120
	// LiveKitAPIURL is the SERVER-side LiveKit HTTP API base (the
	// reconciler's RoomService calls); e.g. http://livekit:7880 in
	// compose, http://localhost:7880 on a laptop. Client devices use
	// LiveKitWSURL, never this.
	LiveKitAPIURL string
	// LiveKitReconcileMinutes is the crash-host sweep interval; <=0 = 15.
	LiveKitReconcileMinutes int
	// LiveKitReconcileGraceHours is how old a "live" registry row must
	// be before the sweep checks it against the SFU; <=0 = 6.
	LiveKitReconcileGraceHours int
	// Mini-app runtime (G3 part 2): HMAC key for the signed launch
	// token minted at POST /v1/mini-apps/{id}/launch. Empty disables
	// the launch endpoint fail-closed (dev default); production must
	// set MINIAPP_LAUNCH_SECRET. Never commit a value.
	MiniappLaunchSecret string
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
	// Events (F6): NATS JetStream order event bus. Empty NatsURL
	// disables publishing (dev default — the API logs a WARN); a URL
	// that will not connect refuses to boot (fail-closed), mirroring
	// the search contract.
	NatsURL string // e.g. nats://nats:4222; empty = bus disabled
	// Webhooks (G2): self-hosted Svix server for developer webhooks.
	// Empty SvixAPIToken disables the dispatcher (dev default — the
	// API logs a WARN); a token with a bad URL refuses to boot
	// (fail-closed), mirroring the search contract.
	SvixURL      string // e.g. http://svix-server:8071; empty = compose default
	SvixAPIToken string // Bearer JWT minted via `svix-server jwt generate`; never commit
	// Payments (F4): payment-aggregator seam. PAYMENTS_PROVIDER is
	// "stub" until the Cashfree onboarding completes post-investors
	// (see internal/payments, docs/payments.md); any other value
	// makes the API refuse to boot (fail-closed). CASHFREE_* are the
	// future live credentials — loaded, never logged, never in repo;
	// the stub ignores them (and warns loudly if they are set).
	PaymentsProvider string // payment aggregator name; "stub" until onboarding
	CashfreeAppID    string // CASHFREE_APP_ID; unset = no live creds
	CashfreeSecret   string // CASHFREE_SECRET_KEY; never logged, never committed
	CashfreeEnv      string // "sandbox" | "prod"; "" = unset
	// Logistics (F5): logistics-gateway seam. LOGISTICS_PROVIDER is
	// "stub" until a logistics partner is onboarded post-investors
	// (see internal/logistics, docs/logistics.md); any other value
	// makes the API refuse to boot (fail-closed). LOGISTICS_* are the
	// future live credentials — loaded, never logged, never in repo;
	// the stub ignores them (and warns loudly if they are set).
	LogisticsProvider string // logistics partner name; "stub" until onboarding
	LogisticsAPIKey   string // LOGISTICS_API_KEY; unset = no live creds
	LogisticsSecret   string // LOGISTICS_SECRET; never logged, never committed
	LogisticsEnv      string // "sandbox" | "prod"; "" = unset
	// Integrations (H2): business-integrations seams — WhatsApp Cloud
	// API direct (no BSP), Tally on-premise bridge, Zoho Books. Each
	// provider name is "stub" until the post-investors onboarding for
	// that integration (see internal/integrations, docs/integrations.md);
	// any other value makes the API refuse to boot (fail-closed).
	// Credentials are loaded, never logged, never in repo; the stubs
	// ignore them (and warn loudly if they are set).
	WhatsAppProvider      string // "stub" until Meta onboarding
	WhatsAppPhoneNumberID string // WHATSAPP_PHONE_NUMBER_ID; unset = no live creds
	WhatsAppAccessToken   string // WHATSAPP_ACCESS_TOKEN; never logged, never committed
	WhatsAppWebhookSecret string // WHATSAPP_WEBHOOK_VERIFY_TOKEN (Meta app secret); never logged
	TallyProvider         string // "stub" until a merchant bridge agent is deployed
	TallyBridgeURL        string // TALLY_BRIDGE_URL; the bridge agent's base URL
	TallySharedSecret     string // TALLY_BRIDGE_SECRET; never logged, never committed
	ZohoProvider          string // "stub" until Zoho OAuth onboarding
	ZohoClientID          string // ZOHO_CLIENT_ID; unset = no live creds
	ZohoClientSecret      string // ZOHO_CLIENT_SECRET; never logged, never committed
	ZohoRefreshToken      string // ZOHO_REFRESH_TOKEN; never logged, never committed
	ZohoOrgID             string // ZOHO_ORG_ID (INR organization)
	// Subscriptions (H4): subscription-billing seam. SUBSCRIPTIONS_PROVIDER
	// is "stub" until the Cashfree Subscriptions + UPI AutoPay onboarding
	// completes post-investors (see internal/subscriptions,
	// docs/subscriptions.md); any other value makes the API refuse to boot
	// (fail-closed). Cashfree credentials are shared with the payments
	// seam (CASHFREE_*): loaded, never logged, never in repo; the stub
	// ignores them (and warns loudly if they are set).
	SubscriptionsProvider string // subscription provider name; "stub" until onboarding
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
		// Commerce surface (F6 part 3): tighter than the global budget —
		// these writes are money-adjacent. <=0 disables the extra budget.
		CommerceLimitRPS: func() float64 {
			f, _ := strconv.ParseFloat(getEnv("COMMERCE_LIMIT_RPS", "30"), 64)
			return f
		}(),
		CommerceLimitBurst: func() int {
			n, _ := strconv.Atoi(getEnv("COMMERCE_LIMIT_BURST", "60"))
			return n
		}(),
		CORSOrigins: parseCORSOrigins(getEnv("CORS_ORIGINS", "")),
		CORSMaxAge:  maxAge,
		OTPProvider: getEnv("OTP_PROVIDER", "stub"),
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
		// Live media (G1, Wave 4): LiveKit SFU seam. Empty secret = the
		// seam is disabled; the API never mints half-configured tokens.
		LiveKitWSURL:     getEnv("LIVEKIT_WS_URL", "ws://localhost:7880"),
		LiveKitAPIKey:    getEnv("LIVEKIT_API_KEY", ""),
		LiveKitAPISecret: getEnv("LIVEKIT_API_SECRET", ""),
		LiveTokenTTLMinutes: func() int {
			n, _ := strconv.Atoi(getEnv("LIVEKIT_TOKEN_TTL_MINUTES", "120"))
			return n
		}(),
		// LiveKit server-side HTTP API base (reconciler). In compose the
		// API reaches the SFU by service name; on a laptop, localhost.
		LiveKitAPIURL: getEnv("LIVEKIT_API_URL", "http://localhost:7880"),
		LiveKitReconcileMinutes: func() int {
			n, _ := strconv.Atoi(getEnv("LIVEKIT_RECONCILE_MINUTES", "15"))
			return n
		}(),
		LiveKitReconcileGraceHours: func() int {
			n, _ := strconv.Atoi(getEnv("LIVEKIT_RECONCILE_GRACE_HOURS", "6"))
			return n
		}(),
		// Mini-app runtime (G3 part 2): empty = launch endpoint
		// disabled fail-closed (503 launch_unavailable).
		MiniappLaunchSecret: getEnv("MINIAPP_LAUNCH_SECRET", ""),
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
		SvixURL:           getEnv("SVIX_URL", ""),
		SvixAPIToken:      getEnv("SVIX_API_TOKEN", ""),
		NatsURL:           getEnv("NATS_URL", ""),
		PaymentsProvider:  getEnv("PAYMENTS_PROVIDER", "stub"),
		CashfreeAppID:     getEnv("CASHFREE_APP_ID", ""),
		CashfreeSecret:    getEnv("CASHFREE_SECRET_KEY", ""),
		CashfreeEnv:       getEnv("CASHFREE_ENV", ""),
		LogisticsProvider: getEnv("LOGISTICS_PROVIDER", "stub"),
		LogisticsAPIKey:   getEnv("LOGISTICS_API_KEY", ""),
		LogisticsSecret:   getEnv("LOGISTICS_SECRET", ""),
		LogisticsEnv:      getEnv("LOGISTICS_ENV", ""),
		// Integrations (H2): business-integration seams, all fail-closed
		// stubs until the post-investors onboardings. Credentials are
		// loaded, never logged, never in repo; stubs ignore them.
		WhatsAppProvider:      getEnv("WHATSAPP_PROVIDER", "stub"),
		WhatsAppPhoneNumberID: getEnv("WHATSAPP_PHONE_NUMBER_ID", ""),
		WhatsAppAccessToken:   getEnv("WHATSAPP_ACCESS_TOKEN", ""),
		WhatsAppWebhookSecret: getEnv("WHATSAPP_WEBHOOK_VERIFY_TOKEN", ""),
		TallyProvider:         getEnv("TALLY_PROVIDER", "stub"),
		TallyBridgeURL:        getEnv("TALLY_BRIDGE_URL", ""),
		TallySharedSecret:     getEnv("TALLY_BRIDGE_SECRET", ""),
		ZohoProvider:          getEnv("ZOHO_PROVIDER", "stub"),
		ZohoClientID:          getEnv("ZOHO_CLIENT_ID", ""),
		ZohoClientSecret:      getEnv("ZOHO_CLIENT_SECRET", ""),
		ZohoRefreshToken:      getEnv("ZOHO_REFRESH_TOKEN", ""),
		ZohoOrgID:             getEnv("ZOHO_ORG_ID", ""),
		// Subscriptions (H4): subscription-billing seam, fail-closed stub
		// until the post-investors Cashfree Subscriptions + UPI AutoPay
		// onboarding.
		SubscriptionsProvider: getEnv("SUBSCRIPTIONS_PROVIDER", "stub"),
	}
}
