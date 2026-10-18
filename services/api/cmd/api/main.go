// SPDX-License-Identifier: AGPL-3.0-or-later
// Command api is the Sutra Go API service: core request-serving layer.
// Realtime state lives in the Elixir chat service; intelligence in Python.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/mintzocloud-coder/sutra/services/api/internal/aiproxy"
	"github.com/mintzocloud-coder/sutra/services/api/internal/cart"
	"github.com/mintzocloud-coder/sutra/services/api/internal/catalog"
	"github.com/mintzocloud-coder/sutra/services/api/internal/config"
	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/engagement"
	"github.com/mintzocloud-coder/sutra/services/api/internal/feed"
	"github.com/mintzocloud-coder/sutra/services/api/internal/media"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/oidc"
	"github.com/mintzocloud-coder/sutra/services/api/internal/otp"
	"github.com/mintzocloud-coder/sutra/services/api/internal/posts"
	"github.com/mintzocloud-coder/sutra/services/api/internal/realtime"
	"github.com/mintzocloud-coder/sutra/services/api/internal/search"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

// methodGuard wraps h so only the listed methods reach it; anything else
// gets the standard 405 envelope with an Allow header.
func methodGuard(allowed []string, h http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, m := range allowed {
			if r.Method == m {
				h.ServeHTTP(w, r)
				return
			}
		}
		middleware.MethodNotAllowedHandler(allowed...)(w, r)
	}
}

// newHandler builds the full HTTP serving stack (routes + middleware) so
// integration tests can exercise the exact same wiring as production.
// feedSvc may be nil (unit tests): the feed route is then unregistered
// and post handlers run without fan-out hooks. mediaSvc may likewise be
// nil: the media routes are unregistered and the post media gate is off.
// rt may be nil: realtime event hooks are then unregistered. aiProxy may
// be nil: the /v1/ai/* proxy routes are then unregistered. translator
// may be nil or disabled: ?translate= is then a no-op and feed items
// keep their original text.
func newHandler(cfg config.Config, pool *db.Pool, verifier *oidc.Verifier, feedSvc *feed.Service, mediaSvc *media.Service, rt realtime.Publisher, aiProxy *aiproxy.Proxy, log *slog.Logger, translator *feed.Translator, searchClient *search.Client) http.Handler {
	auth := middleware.RequireAuth(verifier, log, middleware.AuthOptions{
		Bypass:        cfg.AuthBypass,
		BypassSubject: cfg.BypassSubject,
	})

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		// Liveness: the process is alive. Never depends on the DB.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		// Readiness: fail when the database is unreachable so the
		// orchestrator stops routing traffic until it recovers.
		if err := pool.Ping(r.Context()); err != nil {
			log.Warn("readyz: database unreachable", "err", err,
				"request_id", middleware.FromContext(r.Context()))
			middleware.WriteStatusError(w, r, http.StatusServiceUnavailable,
				"not ready: database unreachable")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Protected API routes: every /v1/* handler runs behind RequireAuth,
	// so the verified subject is always in the request context. GET /v1/me
	// returns the caller's identity record, provisioning it on first sight
	// (see internal/users).
	//
	// Method routing is explicit (not Go 1.22 method patterns) so wrong
	// methods get the standard 405 envelope with an Allow header instead
	// of the mux's plain-text reply; the inner catch-all keeps unknown
	// /v1/* paths in the envelope too.
	v1 := http.NewServeMux()
	meHandler := users.MeHandler(pool, log)
	v1.Handle("/v1/me", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			middleware.MethodNotAllowedHandler(http.MethodGet)(w, r)
			return
		}
		meHandler(w, r)
	}))

	// Social graph (D2): profiles + follows. Path-segment wildcards need
	// Go 1.22+ mux (see go.mod); method discipline stays explicit so
	// wrong methods get the standard 405 envelope.
	v1.Handle("/v1/me/profile", methodGuard([]string{http.MethodPatch},
		social.ProfileHandler(pool, log)))
	v1.Handle("/v1/users/{id}", methodGuard([]string{http.MethodGet},
		social.UserHandler(pool, log)))
	// FollowHandler dispatches POST/DELETE itself (405 + Allow inside).
	// The AfterFollow hook publishes follower:new to the chat service
	// (D5) — best-effort, after the 201 is already written.
	followHooks := social.FollowHooks{}
	if rt != nil && rt.Enabled() {
		followHooks.AfterFollow = func(ctx context.Context, follower, followee pgtype.UUID) {
			go func() {
				fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				defer cancel()
				if err := realtime.PublishFollowerNew(fctx, pool, follower, followee, rt); err != nil {
					log.Warn("realtime: follower:new publish failed (best-effort)",
						"err", err)
				}
			}()
		}
	}
	v1.Handle("/v1/users/{id}/follow", social.FollowHandler(pool, cfg.FollowVelocityCap, followHooks, log))
	v1.Handle("/v1/users/{id}/followers", methodGuard([]string{http.MethodGet},
		social.FollowersHandler(pool, log)))
	v1.Handle("/v1/users/{id}/following", methodGuard([]string{http.MethodGet},
		social.FollowingHandler(pool, log)))

	// Posts (D3): publish, author timeline, author-only delete.
	postsSvc := posts.NewService(pool, cfg.PostVelocityCap)

	// Media pipeline (D4 part 1): presigned uploads + the "reference only
	// ready media" gate. mediaSvc == nil (unit tests) disables both.
	if mediaSvc != nil {
		postsSvc.MediaReady = mediaSvc.CheckReady
		v1.Handle("/v1/media/upload", methodGuard([]string{http.MethodPost},
			media.UploadHandler(mediaSvc, log)))
		v1.Handle("/v1/media/{id}", methodGuard([]string{http.MethodGet},
			media.MediaHandler(mediaSvc, log)))
		v1.Handle("/v1/media/{id}/complete", methodGuard([]string{http.MethodPost},
			media.CompleteHandler(mediaSvc, log)))
	}

	// Recommendations v1 (E5): the like store feeds the home-feed
	// ranker. Created ahead of the feed block so the /v1/feed/home
	// handler can consume it.
	engagementSvc := engagement.NewService(pool)

	// Home feed (D3b): fan-out hooks plug the Valkey timelines into the
	// post lifecycle. The zero value (feedSvc == nil, e.g. unit tests)
	// disables them: posts stay pure-PostgreSQL. Realtime events (D5)
	// ride along: after a successful fan-out, post:new is published to
	// the followers' user topics on the chat service — best-effort, in a
	// detached goroutine, so a slow chat service can never delay the 201.
	feedHooks := posts.PublishHooks{}
	if feedSvc != nil {
		feedHooks.AfterCreate = func(ctx context.Context, p *posts.Post) error {
			if err := feedSvc.FanoutPost(ctx, p); err != nil {
				return err
			}
			if rt != nil && rt.Enabled() {
				go func() {
					pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
					defer cancel()
					if err := realtime.PublishPostNew(pctx, pool, p, cfg.FanoutThreshold, rt); err != nil {
						log.Warn("realtime: post:new publish failed (best-effort)",
							"post_id", p.ID, "err", err)
					}
				}()
			}
			return nil
		}
		feedHooks.AfterDelete = func(ctx context.Context, postID string) error {
			return feedSvc.UnfanoutPost(ctx, postID)
		}
		rankW := feed.RankWeights{
			Recency:    cfg.RankWRecency,
			Graph:      cfg.RankWGraph,
			Engagement: cfg.RankWEngagement,
		}
		v1.Handle("/v1/feed/home", methodGuard([]string{http.MethodGet},
			feed.HomeFeedHandler(pool, feedSvc, engagementSvc, translator, rankW, log)))
	}
	// E3 creator studio: auto_caption=true on POST /v1/posts asks the
	// caption worker to transcribe the post's video attachments.
	// Best-effort per attachment (logged, never fatal to the 201);
	// the media row's auto_caption flag + caption_status are the
	// durable record, so a missed request is safe to retry.
	if mediaSvc != nil {
		feedHooks.AfterCreateCaptions = func(ctx context.Context, p *posts.Post) error {
			authorID, err := social.ParseUserID(p.AuthorID)
			if err != nil {
				return nil // AuthorID came from a provisioned user; cannot be bad
			}
			for _, mid := range p.MediaIDs {
				mediaID, err := social.ParseUserID(mid)
				if err != nil {
					continue // shape-validated at create; skip defensively
				}
				if err := mediaSvc.RequestCaptions(ctx, mediaID, authorID); err != nil {
					log.Warn("captions: request failed for attachment",
						"post_id", p.ID, "media_id", mid, "err", err)
				}
			}
			return nil
		}
	}
	v1.Handle("/v1/posts", methodGuard([]string{http.MethodPost},
		posts.CreatePostHandler(postsSvc, feedHooks, log)))
	v1.Handle("/v1/posts/{id}", methodGuard([]string{http.MethodDelete},
		posts.DeletePostHandler(postsSvc, feedHooks, log)))
	// Recommendations v1 (E5): likes are the engagement signal feeding
	// the home-feed ranker. One route dispatches POST (like) and DELETE
	// (unlike); both are idempotent. engagementSvc was created above
	// the feed block (the /v1/feed/home handler consumes it).
	v1.Handle("/v1/posts/{id}/like", methodGuard([]string{http.MethodPost, http.MethodDelete},
		engagement.LikeHandler(pool, engagementSvc, log)))
	v1.Handle("/v1/users/{id}/posts", methodGuard([]string{http.MethodGet},
		posts.UserPostsHandler(postsSvc, pool, log)))

	// Commerce catalog (F1): seller onboarding + the curated category
	// tree. Storefront reads are public (any caller); registration is
	// one seller account per authenticated user (Wave-1 rule).
	catalogSvc := catalog.NewService(pool)

	// Search indexing (F2 part 1): after a successful product write,
	// build the search document and upsert/delete it — detached,
	// best-effort, never delaying the response. searchClient may be
	// nil in unit tests; the zero hooks value then disables indexing.
	prodHooks := catalog.ProductHooks{}
	if searchClient != nil && searchClient.Enabled() {
		prodHooks.AfterProductWrite = func(ctx context.Context, productID string) {
			go func() {
				cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				defer cancel()
				var pid pgtype.UUID
				if err := pid.Scan(productID); err != nil {
					log.Warn("search: bad product id from hook", "product_id", productID)
					return
				}
				doc, err := catalogSvc.BuildProductDoc(cctx, pid)
				if err != nil {
					if errors.Is(err, catalog.ErrProductNotFound) {
						if derr := searchClient.DeleteProduct(cctx, productID); derr != nil {
							log.Warn("search: delete failed", "product_id", productID, "err", derr)
						}
						return
					}
					log.Warn("search: build doc failed", "product_id", productID, "err", err)
					return
				}
				if doc.Status == "active" {
					if err := searchClient.IndexProduct(cctx, doc); err != nil {
						log.Warn("search: index failed", "product_id", productID, "err", err)
					}
				} else {
					// Drafts and archived listings leave the index.
					if err := searchClient.DeleteProduct(cctx, doc.ID); err != nil {
						log.Warn("search: delete failed", "product_id", productID, "err", err)
					}
				}
			}()
		}
	}
	v1.Handle("/v1/sellers", methodGuard([]string{http.MethodPost},
		catalog.RegisterSellerHandler(pool, catalogSvc, log)))
	v1.Handle("/v1/sellers/{id}", methodGuard([]string{http.MethodGet},
		catalog.GetSellerHandler(catalogSvc, log)))
	v1.Handle("/v1/categories", methodGuard([]string{http.MethodGet},
		catalog.ListCategoriesHandler(catalogSvc, log)))

	// Commerce catalog (F1b): product CRUD, variant ops and inventory.
	// Reads (GET) are public storefront reads; every write resolves
	// the caller's seller account and refuses non-owners (403).
	v1.Handle("/v1/products", methodGuard([]string{http.MethodPost, http.MethodGet},
		catalog.ProductsMux(pool, catalogSvc, prodHooks, log)))
	v1.Handle("/v1/products/{id}", methodGuard([]string{http.MethodGet, http.MethodPatch, http.MethodDelete},
		catalog.ProductMux(pool, catalogSvc, prodHooks, log)))
	v1.Handle("/v1/products/{id}/variants", methodGuard([]string{http.MethodPost},
		catalog.AddVariantHandler(pool, catalogSvc, prodHooks, log)))
	v1.Handle("/v1/variants/{id}", methodGuard([]string{http.MethodPatch, http.MethodDelete},
		catalog.VariantMux(pool, catalogSvc, prodHooks, log)))
	v1.Handle("/v1/variants/{id}/inventory", methodGuard([]string{http.MethodGet},
		catalog.GetInventoryHandler(catalogSvc, log)))
	v1.Handle("/v1/variants/{id}/inventory/adjust", methodGuard([]string{http.MethodPost},
		catalog.AdjustInventoryHandler(pool, catalogSvc, log)))

	// Commerce search (F2 part 2a): the storefront search proxy.
	// Behind the same RequireAuth as the rest of /v1/; the handler
	// 503s when search is unconfigured and on upstream outages.
	v1.Handle("/v1/search", methodGuard([]string{http.MethodGet},
		search.SearchHandler(searchClient, log)))

	// Commerce cart (F3 part 1): per-user cart. CartHandler dispatches
	// GET/DELETE itself (405 + Allow inside, same as FollowHandler);
	// CartItemHandler dispatches PATCH/DELETE the same way.
	cartSvc := cart.NewService(pool)
	v1.Handle("/v1/cart", cart.CartHandler(pool, cartSvc, log))
	v1.Handle("/v1/cart/items", methodGuard([]string{http.MethodPost},
		cart.AddItemHandler(pool, cartSvc, log)))
	v1.Handle("/v1/cart/items/{variant_id}", cart.CartItemHandler(pool, cartSvc, log))

	// AI core (E1): /v1/ai/* proxies to the Python AI service. Runs
	// behind the same RequireAuth as everything else under /v1/, so the
	// proxy always knows the verified subject (forwarded as
	// X-Sutra-Subject). Both spellings route to the proxy so a bare
	// GET /v1/ai lands on the AI service's root instead of the 404
	// catch-all below.
	if aiProxy != nil {
		v1.Handle("/v1/ai/", aiProxy.Handler())
		v1.Handle("/v1/ai", aiProxy.Handler())
	}

	v1.Handle("/", middleware.NotFoundHandler())
	mux.Handle("/v1/", auth(v1))

	// Unknown paths (outside /healthz, /readyz, /v1/): 404 in the standard
	// envelope instead of Go's plain-text default.
	mux.Handle("/", middleware.NotFoundHandler())

	// Middleware order (outermost first): RequestID → Recover →
	// SecurityHeaders → CORS → RateLimit → Logger.
	// RequestID is outermost so every response — including recovered
	// panics and limiter 429s — carries the request ID. Recover sits next
	// so it also catches panics in the remaining middleware; it must stay
	// outside them. SecurityHeaders is outside everything that can produce
	// a response so all replies carry the baseline headers; CORS runs
	// before RateLimit so preflights aren't counted against the budget.
	limiter := middleware.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
	cors := middleware.CORS(middleware.CORSConfig{
		Origins: cfg.CORSOrigins,
		MaxAge:  cfg.CORSMaxAge,
	})
	return middleware.Chain(mux,
		middleware.RequestID,
		middleware.Recover(log),
		middleware.SecurityHeaders(),
		cors,
		middleware.RateLimit(limiter, log),
		middleware.Logger(log),
	)
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	cfg := config.Load()
	if cfg.AuthBypass {
		log.Warn("AUTH_BYPASS=1: JWT verification DISABLED — every request is "+
			"authenticated as the bypass subject. INSECURE: local development only, "+
			"never enable in production.",
			"bypass_subject", cfg.BypassSubject)
	}
	verifier := oidc.NewVerifier(cfg.HydraJWKSURL, cfg.HydraIssuer, cfg.JWTAudience)

	// Phone-OTP seam (D1): fail fast on an unknown OTP_PROVIDER so a
	// misconfigured gateway name refuses to boot instead of silently
	// dropping OTPs. Until DLT registration the only provider is the
	// fail-closed stub (see internal/otp); the constructed sender is
	// consumed by the login endpoints landing in D2+.
	if _, err := otp.NewSender(cfg.OTPProvider); err != nil {
		log.Error("invalid OTP_PROVIDER — refusing to start", "err", err)
		os.Exit(1)
	}
	log.Info("otp provider configured", "provider", cfg.OTPProvider)

	// Database: connect (fail fast if unreachable) then apply pending
	// migrations before serving any traffic.
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 60*time.Second)
	pool, err := db.Open(startupCtx, cfg.DatabaseURL, int32(cfg.DBMaxConns), log)
	if err != nil {
		startupCancel()
		log.Error("database unavailable at startup", "err", err)
		os.Exit(1)
	}
	if err := db.Migrate(startupCtx, pool); err != nil {
		startupCancel()
		log.Error("migrations failed", "err", err)
		os.Exit(1)
	}
	startupCancel()
	defer pool.Close()

	// Home-feed timeline store (D3b): fail closed at boot. Without
	// Valkey, post creation could never satisfy its "201 means visible
	// in home timelines" contract (the handler rolls back on fan-out
	// failure), so refuse to start rather than serve a silently broken
	// feed. Mid-run outages degrade to 503s on feed paths and self-heal.
	rdb := redis.NewClient(&redis.Options{Addr: cfg.ValkeyAddr})
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		pingCancel()
		log.Error("valkey unavailable at startup — refusing to start",
			"addr", cfg.ValkeyAddr, "err", err)
		os.Exit(1)
	}
	pingCancel()
	defer rdb.Close()
	log.Info("valkey connected", "addr", cfg.ValkeyAddr)
	feedSvc := feed.NewService(pool, rdb, cfg.FanoutThreshold, cfg.TimelineMax)

	// Media pipeline (D4): SeaweedFS object storage. Unlike Valkey, an
	// unreachable master does NOT refuse boot — everything except
	// POST /v1/media/upload keeps working, and that endpoint 503s
	// honestly at request time. Log the state so operators see it.
	mediaSvc := media.NewService(pool, rdb, cfg.SeaweedMasterAddr, cfg.MediaPublicBaseURL)
	if err := media.PingMaster(startupCtx, cfg.SeaweedMasterAddr); err != nil {
		log.Warn("seaweedfs master unreachable at startup — media uploads will 503 until it recovers",
			"master", cfg.SeaweedMasterAddr, "err", err)
	} else {
		log.Info("seaweedfs master reachable", "master", cfg.SeaweedMasterAddr)
	}

	// Realtime events (D5): publish post:new / follower:new to the chat
	// service's internal signed endpoint. Empty token = disabled (dev
	// default); a token with a bad URL is a configuration bug — refuse
	// to boot rather than silently drop events.
	rtPub, err := realtime.NewPublisher(cfg.ChatInternalURL, cfg.ChatInternalToken, log)
	if err != nil {
		log.Error("invalid realtime config — refusing to start", "err", err)
		os.Exit(1)
	}
	if rtPub.Enabled() {
		log.Info("realtime events enabled", "chat", cfg.ChatInternalURL)
	}

	// AI core (E1): proxy /v1/ai/* to the Python AI service. Empty token
	// = the proxy answers 503 on those paths (dev default); a token with
	// a bad URL is a configuration bug — refuse to boot rather than serve
	// a silently broken AI surface.
	aiProxy, err := aiproxy.NewProxy(cfg.AIServiceURL, cfg.AIServiceToken, log)
	if err != nil {
		log.Error("invalid AI proxy config — refusing to start", "err", err)
		os.Exit(1)
	}
	if aiProxy.Enabled() {
		log.Info("ai proxy enabled", "ai", cfg.AIServiceURL)
	} else {
		log.Warn("AI_SERVICE_TOKEN empty — /v1/ai/* disabled (503 service_unavailable)")
	}

	// Feed translation (E4 part 2): ?translate=xx decorates home-feed
	// reads via the AI service. Empty token = disabled (a loud WARN;
	// ?translate= then no-ops best-effort). A token with a bad URL is a
	// configuration bug — refuse to boot, same as the AI proxy.
	translator, err := feed.NewTranslator(cfg.AIServiceURL, cfg.AIServiceToken, log)
	if err != nil {
		log.Error("invalid feed-translator config — refusing to start", "err", err)
		os.Exit(1)
	}
	if translator.Enabled() {
		log.Info("feed translation enabled", "ai", cfg.AIServiceURL)
	} else {
		log.Warn("AI_SERVICE_TOKEN empty — ?translate= disabled (feed items keep original text)")
	}

	// Commerce search (F2 part 1): Meilisearch product indexing. Empty
	// master key = indexing disabled (dev default, loud WARN); a key
	// with a bad URL is a configuration bug — refuse to boot, same as
	// the realtime publisher and the AI proxy.
	searchClient, err := search.NewClient(cfg.SearchURL, cfg.SearchMasterKey, log)
	if err != nil {
		log.Error("invalid search config — refusing to start", "err", err)
		os.Exit(1)
	}
	if searchClient.Enabled() {
		log.Info("product search indexing enabled", "search", cfg.SearchURL)
	}

	handler := newHandler(cfg, pool, verifier, feedSvc, mediaSvc, rtPub, aiProxy, log, translator, searchClient)
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: handler}

	go func() {
		log.Info("api listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.ShutdownTimeout)*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("shutdown failed", "err", err)
		os.Exit(1)
	}
	log.Info("api stopped")
}
