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

	"github.com/mintzocloud-coder/sutra/services/api/internal/ads"
	"github.com/mintzocloud-coder/sutra/services/api/internal/aiproxy"
	"github.com/mintzocloud-coder/sutra/services/api/internal/cart"
	"github.com/mintzocloud-coder/sutra/services/api/internal/catalog"
	"github.com/mintzocloud-coder/sutra/services/api/internal/config"
	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/e2e"
	"github.com/mintzocloud-coder/sutra/services/api/internal/engagement"
	"github.com/mintzocloud-coder/sutra/services/api/internal/events"
	"github.com/mintzocloud-coder/sutra/services/api/internal/feed"
	"github.com/mintzocloud-coder/sutra/services/api/internal/integrations"
	"github.com/mintzocloud-coder/sutra/services/api/internal/live"
	"github.com/mintzocloud-coder/sutra/services/api/internal/logistics"
	"github.com/mintzocloud-coder/sutra/services/api/internal/media"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/miniapp"
	"github.com/mintzocloud-coder/sutra/services/api/internal/oidc"
	"github.com/mintzocloud-coder/sutra/services/api/internal/orders"
	"github.com/mintzocloud-coder/sutra/services/api/internal/otp"
	"github.com/mintzocloud-coder/sutra/services/api/internal/payments"
	"github.com/mintzocloud-coder/sutra/services/api/internal/posts"
	"github.com/mintzocloud-coder/sutra/services/api/internal/realtime"
	"github.com/mintzocloud-coder/sutra/services/api/internal/search"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
	"github.com/mintzocloud-coder/sutra/services/api/internal/subscriptions"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
	"github.com/mintzocloud-coder/sutra/services/api/internal/webhooks"
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
// keep their original text. whClient may be nil or disabled: the
// developer webhook routes (G2 part 2) are then unregistered. waProvider
// may be nil: the send route answers 503; the /internal/whatsapp/webhook
// is always registered (the HMAC check needs no provider). tallyBridge
// may be nil: the voucher route answers 503 (the registry lookup runs
// regardless). zohoProvider may be nil: the Zoho routes answer 503.
func newHandler(bg context.Context, cfg config.Config, pool *db.Pool, verifier *oidc.Verifier, feedSvc *feed.Service, mediaSvc *media.Service, rt realtime.Publisher, aiProxy *aiproxy.Proxy, log *slog.Logger, translator *feed.Translator, searchClient *search.Client, orderBus events.EventBus, whClient *webhooks.Client, waProvider integrations.WhatsAppProvider, tallyBridge integrations.TallyBridge, zohoProvider integrations.ZohoBooksProvider, subProvider subscriptions.Provider) http.Handler {
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

	// WhatsApp send route (H2 part 2a): seller-scoped order updates via
	// the Meta Cloud API. Registered whenever the integrations seam is
	// wired (the stub fails closed with 503 — it never sends).
	waHandler := integrations.NewWhatsAppHandler(pool, waProvider, cfg.WhatsAppWebhookSecret, log)
	v1.Handle("/v1/integrations/whatsapp/send", methodGuard([]string{http.MethodPost},
		waHandler.Send()))
	// WhatsApp inbound webhook (H2 part 2a): Meta's hub-challenge GET +
	// the signed callback POST. Public by necessity (Meta calls it), like
	// /internal/livekit/webhook; the HMAC check is the auth.
	mux.Handle("/internal/whatsapp/webhook", methodGuard([]string{http.MethodGet, http.MethodPost},
		waHandler.Webhook()))
	log.Info("whatsapp routes registered",
		"routes", "/v1/integrations/whatsapp/send,/internal/whatsapp/webhook")

	// Tally voucher route (H2 part 2b): per-seller accounting vouchers
	// through the on-premise bridge agent. The bridge is resolved per
	// seller from the seller_tally_bridges registry — never globally —
	// and the stub fails closed with 503 (never implies a voucher was
	// posted).
	tallyHandler := integrations.NewTallyHandler(pool, tallyBridge, log)
	v1.Handle("/v1/integrations/tally/vouchers", methodGuard([]string{http.MethodPost},
		tallyHandler.PostVoucher()))
	log.Info("tally route registered",
		"route", "/v1/integrations/tally/vouchers")

	// Zoho Books routes (H2 final piece): seller-scoped contact and
	// invoice creation against the deployment-wide provider. The stub
	// fails closed with 503 (it never books anything) until the
	// post-investors Zoho OAuth onboarding.
	zohoHandler := integrations.NewZohoHandler(pool, zohoProvider, log)
	v1.Handle("/v1/integrations/zoho/contacts", methodGuard([]string{http.MethodPost},
		zohoHandler.CreateContact()))
	v1.Handle("/v1/integrations/zoho/invoices", methodGuard([]string{http.MethodPost},
		zohoHandler.CreateInvoice()))
	log.Info("zoho routes registered",
		"routes", "/v1/integrations/zoho/contacts,/v1/integrations/zoho/invoices")
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

	// E2E device keys (I1, Phase I): registration, replenishment, the
	// public device directory, and atomic one-time-prekey consumption
	// for X3DH session initiation. The service needs only the pool, so
	// it is built here; unit tests pass pool == nil, in which case the
	// routes stay unregistered (same convention as mediaSvc).
	var e2eSvc *e2e.Service
	if pool != nil {
		e2eSvc = e2e.NewService(pool)
	}
	if e2eSvc != nil {
		// DevicesHandler dispatches POST (register) / GET (list) with
		// 405 + Allow inside, like FollowHandler.
		v1.Handle("/v1/e2e/devices", e2e.DevicesHandler(e2eSvc, log))
		v1.Handle("/v1/e2e/devices/{device_id}/prekeys", methodGuard([]string{http.MethodPost},
			e2e.ReplenishHandler(e2eSvc, log)))
		v1.Handle("/v1/e2e/devices/{device_id}", methodGuard([]string{http.MethodDelete},
			e2e.DeleteHandler(e2eSvc, log)))
		v1.Handle("/v1/users/{id}/devices", methodGuard([]string{http.MethodGet},
			e2e.DirectoryHandler(e2eSvc, log)))
		v1.Handle("/v1/users/{id}/devices/{device_id}/prekey-bundle", methodGuard([]string{http.MethodGet},
			e2e.PrekeyHandler(e2eSvc, log)))
		// I3: per-conversation E2E flag (read-only here — the chat
		// service writes it when it observes the first session-init).
		v1.Handle("/v1/conversations/{room_id}/e2e", methodGuard([]string{http.MethodGet},
			e2e.ConversationE2EHandler(e2eSvc, log)))
	}

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

	// Live media (G1, Wave 4): LiveKit SFU seam — room registry + token
	// mint. liveSvc == nil (unit tests / disabled deployments) leaves
	// the routes unregistered. An empty LIVEKIT_API_SECRET refuses boot
	// fail-closed (see the config seam); a LiveKit server that is down
	// does NOT refuse boot — minting fails 503 honestly at request
	// time, and the room registry keeps working.
	liveSvc := live.NewService(pool, cfg.LiveKitAPIKey, cfg.LiveKitAPISecret,
		cfg.LiveKitWSURL, cfg.LiveTokenTTLMinutes)
	if liveSvc.Enabled() {
		v1.Handle("/v1/live/token", methodGuard([]string{http.MethodPost},
			live.TokenHandler(liveSvc, pool, log)))
		v1.Handle("/v1/live/rooms", live.RoomsHandler(liveSvc, pool, rt, whClient, log))
		v1.Handle("/v1/live/rooms/{id}/end", methodGuard([]string{http.MethodPost},
			live.EndRoomHandler(liveSvc, pool, rt, whClient, log)))
		// G1 part 2: LiveKit server-webhook ingress. NOT behind
		// RequireAuth — the SFU has no user credential; its own signed
		// webhook token is the auth (verified inside the handler).
		mux.Handle("/internal/livekit/webhook", methodGuard([]string{http.MethodPost},
			live.WebhookHandler(liveSvc, pool, rt, whClient, log)))
		// Crash-host sweeper: reconciles the registry against SFU truth
		// on an interval. Fail-closed per pass (see internal/live).
		reconcileInterval := time.Duration(cfg.LiveKitReconcileMinutes) * time.Minute
		if reconcileInterval <= 0 {
			reconcileInterval = 15 * time.Minute
		}
		reconcileGrace := time.Duration(cfg.LiveKitReconcileGraceHours) * time.Hour
		if reconcileGrace <= 0 {
			reconcileGrace = 6 * time.Hour
		}
		go live.StartReconciler(bg, liveSvc, pool, rt, whClient, cfg.LiveKitAPIURL,
			reconcileInterval, reconcileGrace, log)
		log.Info("live media enabled", "ws_url", cfg.LiveKitWSURL,
			"token_ttl_minutes", cfg.LiveTokenTTLMinutes,
			"webhook", "/internal/livekit/webhook",
			"reconcile_interval_minutes", int(reconcileInterval.Minutes()),
			"reconcile_grace_hours", int(reconcileGrace.Hours()))
	} else {
		log.Warn("live media disabled — LIVEKIT_API_SECRET is not set; " +
			"/v1/live/* routes are unregistered")
	}

	// Developer webhooks (G2 part 2): endpoint registry, custom event
	// publish, and the Svix delivery-attempt log. One Svix app per user
	// (user:<id>); RequireAuth (the outer v1 wrapper) guarantees the
	// caller can only ever touch their own. Unregistered when the
	// dispatcher is disabled — developers then get 404, not a broken
	// 503, and the platform-event hooks below no-op.
	if whClient != nil && whClient.Enabled() {
		whHandler := webhooks.NewHandler(pool, whClient, log)
		v1.Handle("/v1/webhooks/endpoints", whHandler.Endpoints())
		v1.Handle("/v1/webhooks/endpoints/{id}", methodGuard([]string{http.MethodDelete},
			whHandler.DeleteEndpoint()))
		v1.Handle("/v1/webhooks/events", methodGuard([]string{http.MethodPost},
			whHandler.PublishEvent()))
		v1.Handle("/v1/webhooks/deliveries", methodGuard([]string{http.MethodGet},
			whHandler.Deliveries()))
		log.Info("developer webhook routes registered",
			"routes", "/v1/webhooks/endpoints,/v1/webhooks/events,/v1/webhooks/deliveries")
	}

	// Mini-app registry (G3 part 1): developer registration of sandboxed
	// mini-apps, the public published-only directory, status transitions,
	// and the per-user installed shelf. The {id}/{action} route carries
	// publish/suspend/install actions; AppMux dispatches on the action
	// path value (empty action = the bare item route).
	miniappSvc := miniapp.NewService(pool)
	launchSecret := []byte(cfg.MiniappLaunchSecret)
	if len(launchSecret) == 0 {
		log.Warn("MINIAPP_LAUNCH_SECRET is empty — /v1/mini-apps/{id}/launch will answer 503 (fail-closed)")
	}
	v1.Handle("/v1/mini-apps", methodGuard([]string{http.MethodPost, http.MethodGet},
		miniapp.AppsMux(pool, miniappSvc, log)))
	v1.Handle("/v1/mini-apps/{id}", methodGuard([]string{http.MethodGet, http.MethodPatch, http.MethodDelete},
		miniapp.AppMux(pool, miniappSvc, log, launchSecret)))
	v1.Handle("/v1/mini-apps/{id}/{action}", methodGuard([]string{http.MethodPost, http.MethodDelete},
		miniapp.AppMux(pool, miniappSvc, log, launchSecret)))
	v1.Handle("/v1/me/mini-apps", methodGuard([]string{http.MethodGet},
		miniapp.MyInstallsHandler(pool, miniappSvc, log)))

	// Ads platform (H3 part 2): the advertiser API — campaigns, budgets,
	// targeting over the GSP auction kernel. Seller-scoped: the
	// advertiser is the caller's seller row, so non-sellers get 403
	// not_a_seller and advertisers can never see each other's
	// campaigns. Explicit action routes (activate/pause/resume) rather
	// than an {action} dispatch, so the OpenAPI coverage gate sees real
	// paths.
	adsSvc := ads.NewCampaignService(pool)
	adsHandler := ads.NewCampaignHandler(pool, adsSvc, log)
	v1.Handle("/v1/ads/campaigns", methodGuard([]string{http.MethodPost, http.MethodGet},
		adsHandler.CampaignsMux()))
	v1.Handle("/v1/ads/campaigns/{id}", methodGuard([]string{http.MethodGet, http.MethodPatch, http.MethodDelete},
		adsHandler.CampaignMux()))
	v1.Handle("/v1/ads/campaigns/{id}/activate", methodGuard([]string{http.MethodPost},
		adsHandler.TransitionTo(ads.CampaignActive)))
	v1.Handle("/v1/ads/campaigns/{id}/pause", methodGuard([]string{http.MethodPost},
		adsHandler.TransitionTo(ads.CampaignPaused)))
	v1.Handle("/v1/ads/campaigns/{id}/resume", methodGuard([]string{http.MethodPost},
		adsHandler.TransitionTo(ads.CampaignActive)))
	v1.Handle("/v1/ads/campaigns/{id}/report", methodGuard([]string{http.MethodGet},
		http.HandlerFunc(adsHandler.Report)))
	log.Info("ads routes registered",
		"routes", "/v1/ads/campaigns (+item, activate/pause/resume, report)")

	// Creative registry (H3 part 3a): seller-scoped creative CRUD under
	// campaigns, plus the explicit activate action route.
	creativeSvc := ads.NewCreativeService(pool, adsSvc)
	creativeHandler := ads.NewCreativeHandler(pool, creativeSvc, log)
	v1.Handle("/v1/ads/campaigns/{id}/creatives", methodGuard([]string{http.MethodPost, http.MethodGet},
		creativeHandler.CampaignCreativesMux()))
	v1.Handle("/v1/ads/creatives/{id}", methodGuard([]string{http.MethodGet, http.MethodPatch, http.MethodDelete},
		creativeHandler.CreativeMux()))
	v1.Handle("/v1/ads/creatives/{id}/activate", methodGuard([]string{http.MethodPost},
		creativeHandler.ActivateTo()))
	log.Info("creative routes registered",
		"routes", "/v1/ads/campaigns/{id}/creatives, /v1/ads/creatives/{id} (+activate)")

	// Publisher serving (H3 part 3b): POST /v1/ads/serve turns a slot's
	// content context into the winning creative (or 204 when nothing is
	// servable). Same RequireAuth as every /v1/ route; the seam itself
	// stays fail-closed via ADS_SERVING_ENABLED (503 serving_disabled).
	serveHandler := ads.NewServeHandler(pool,
		ads.ServingDeps{Campaigns: adsSvc, Creatives: creativeSvc},
		ads.ServingConfigFromEnv(), log)
	v1.Handle("/v1/ads/serve", methodGuard([]string{http.MethodPost}, serveHandler))
	log.Info("ads serving route registered", "route", "/v1/ads/serve")

	// Memberships / subscriptions (H4 part 2): creator membership plans,
	// member subscriptions, the member_of:<creator> entitlement list,
	// and the aggregator subscription-webhook ingress. Everything money
	// touches rides the fail-closed provider seam (part 1): while the
	// stub is wired, subscribe/cancel/webhook answer 503
	// subscriptions_unavailable and plans CRUD runs fully local. The
	// webhook ingress is public by necessity (the aggregator calls it)
	// like /internal/whatsapp/webhook; the signature check inside
	// Provider.VerifyWebhook is the auth, so it stays out of the
	// public OpenAPI spec (see scripts/check_openapi_coverage.py
	// INTERNAL_ROUTES).
	subHandler := subscriptions.NewMembershipHandler(pool, subProvider, log)
	v1.Handle("/v1/memberships/plans", methodGuard([]string{http.MethodPost, http.MethodGet},
		subHandler.PlansMux()))
	v1.Handle("/v1/memberships/plans/{id}", methodGuard([]string{http.MethodGet, http.MethodPatch, http.MethodDelete},
		subHandler.PlanMux()))
	v1.Handle("/v1/memberships/subscriptions", methodGuard([]string{http.MethodPost, http.MethodGet},
		subHandler.SubscriptionsMux()))
	v1.Handle("/v1/memberships/subscriptions/{id}", methodGuard([]string{http.MethodGet},
		subHandler.SubscriptionMux()))
	v1.Handle("/v1/memberships/subscriptions/{id}/cancel", methodGuard([]string{http.MethodPost},
		subHandler.CancelMux()))
	v1.Handle("/v1/memberships/entitlements", methodGuard([]string{http.MethodGet},
		http.HandlerFunc(subHandler.Entitlements())))
	mux.Handle("/internal/subscriptions/webhook", methodGuard([]string{http.MethodPost},
		subHandler.Webhook()))
	log.Info("membership routes registered",
		"routes", "/v1/memberships/plans (+{id}),/v1/memberships/subscriptions (+{id},+{id}/cancel),/v1/memberships/entitlements,/internal/subscriptions/webhook")

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

	// Commerce orders (F3 part 2a): COD-first checkout + the order
	// state machine. OrdersHandler dispatches POST (checkout) / GET
	// (list, ?role=buyer|seller) itself (405 + Allow inside);
	// OrderStatusHandler takes PATCH only (405 + Allow inside).
	ordersSvc := orders.NewService(pool)
	// Order events (F6 part 1 + G2 part 2): the hook detaches with its
	// own 15s deadline — publishing is best-effort and never delays the
	// order response. A nil bus (tests) means no JetStream emission; a
	// nil/disabled webhook client means no webhook fan-out. Each sink
	// works without the other.
	if (orderBus != nil && orderBus.Enabled()) || (whClient != nil && whClient.Enabled()) {
		ordersSvc.EventHook = func(ctx context.Context, ev events.OrderEvent) {
			go func() {
				cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				defer cancel()
				if orderBus != nil && orderBus.Enabled() {
					if err := orderBus.PublishOrderEvent(cctx, ev); err != nil {
						log.Warn("events: order event publish failed", "event", ev.Event, "order_id", ev.OrderID, "err", err)
					}
				}
				// Developer webhooks (G2 part 2): the same order
				// lifecycle fans out to the buyer + seller's Svix apps.
				// EmitBestEffort no-ops when webhooks are unconfigured.
				webhooks.EmitBestEffort(cctx, whClient, ev.BuyerID, ev.Event, ev, ev.EventID, log)
				webhooks.EmitBestEffort(cctx, whClient, ev.SellerID, ev.Event, ev, ev.EventID, log)
			}()
		}
	}
	v1.Handle("/v1/orders", orders.OrdersHandler(pool, ordersSvc, log))
	v1.Handle("/v1/orders/{id}", methodGuard([]string{http.MethodGet},
		orders.OrderDetailHandler(pool, ordersSvc, log)))
	v1.Handle("/v1/orders/{id}/status", methodGuard([]string{http.MethodPatch},
		orders.OrderStatusHandler(pool, ordersSvc, log)))

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
	// SecurityHeaders → CORS → RateLimit → CommerceRateLimit → Logger.
	// RequestID is outermost so every response — including recovered
	// panics and limiter 429s — carries the request ID. Recover sits next
	// so it also catches panics in the remaining middleware; it must stay
	// outside them. SecurityHeaders is outside everything that can produce
	// a response so all replies carry the baseline headers; CORS runs
	// before RateLimit so preflights aren't counted against the budget.
	// The commerce limiter stacks AFTER the global one: a request needs
	// room in both budgets to proceed.
	limiter := middleware.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)
	commerceLimiter := middleware.NewRateLimiter(cfg.CommerceLimitRPS, cfg.CommerceLimitBurst)
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
		middleware.CommerceRateLimit(commerceLimiter, log),
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

	// Payments seam (F4): fail fast on an unknown PAYMENTS_PROVIDER so a
	// misconfigured aggregator name refuses to boot instead of silently
	// taking no payments. Until the Cashfree onboarding completes
	// post-investors the only provider is the fail-closed stub (see
	// internal/payments): every operation — CreateOrder, VerifyWebhook,
	// SplitTransfer, Payout — is an explicit typed error, never a charge.
	// The constructed provider is discarded for now and consumed by the
	// online-checkout wiring landing in F6. Real keys MUST come from the
	// CASHFREE_* environment; they are never committed and never logged.
	if _, err := payments.NewProvider(cfg.PaymentsProvider, payments.CashfreeConfig{
		AppID:  cfg.CashfreeAppID,
		Secret: cfg.CashfreeSecret,
		Env:    cfg.CashfreeEnv,
	}); err != nil {
		log.Error("invalid payments config — refusing to start", "err", err)
		os.Exit(1)
	}
	log.Warn("payments: running with the fail-closed stub — real payment processing is DISABLED until the post-investors Cashfree onboarding (see docs/payments.md)")
	if cfg.CashfreeAppID != "" || cfg.CashfreeSecret != "" {
		log.Warn("payments: CASHFREE_* credentials are set but no real provider is wired — they are IGNORED; online payments cannot process")
	}

	// Logistics seam (F5): fail fast on an unknown LOGISTICS_PROVIDER so a
	// misconfigured partner name refuses to boot instead of silently
	// booking nothing. Until a logistics partner is onboarded
	// post-investors the only provider is the fail-closed stub (see
	// internal/logistics): every operation — CreateShipment, Track,
	// Cancel — is an explicit typed error, never a booking. The
	// constructed provider is discarded for now and consumed by the
	// shipment-booking routes landing in F5 part 2. Real keys MUST come
	// from the LOGISTICS_* environment; they are never committed and
	// never logged.
	if _, err := logistics.NewProvider(cfg.LogisticsProvider, logistics.ProviderConfig{
		APIKey: cfg.LogisticsAPIKey,
		Secret: cfg.LogisticsSecret,
		Env:    cfg.LogisticsEnv,
	}); err != nil {
		log.Error("invalid logistics config — refusing to start", "err", err)
		os.Exit(1)
	}
	log.Warn("logistics: running with the fail-closed stub — shipment booking is DISABLED until a logistics partner is onboarded post-investors (see docs/logistics.md)")
	if cfg.LogisticsAPIKey != "" || cfg.LogisticsSecret != "" {
		log.Warn("logistics: LOGISTICS_* credentials are set but no real provider is wired — they are IGNORED; no shipments can be booked")
	}

	// Business-integrations seams (H2): fail fast on an unknown provider
	// name for each of WhatsApp, Tally, and Zoho Books, so a
	// misconfigured integration refuses to boot instead of silently
	// delivering nothing. Until the post-investors onboardings complete,
	// all three are the fail-closed stub (see internal/integrations):
	// every operation is an explicit typed error — no WhatsApp message
	// can be sent, no Tally voucher posted, no Zoho invoice created.
	// The WhatsApp provider is kept and passed to newHandler: the send
	// route and the inbound webhook need it. The Tally bridge is kept and
	// passed too: the voucher route resolves it per seller against the
	// seller_tally_bridges registry. The Zoho provider is kept and
	// passed as well: the /v1/integrations/zoho/* routes fail closed
	// against the deployment-wide provider seam (no per-seller
	// registry — one self-client per organization).
	// Real keys MUST come from the WHATSAPP_*/TALLY_*/ZOHO_*
	// environment; they are never committed and never logged.
	// The WhatsApp provider is kept (the H2 part 2a send route and the
	// inbound webhook handler need it); the Tally bridge is kept for the
	// H2 part 2b voucher route (per-seller registry wiring); the Zoho
	// provider is kept for the H2 final-piece Zoho routes.
	waProvider, err := integrations.NewWhatsApp(cfg.WhatsAppProvider, integrations.WhatsAppConfig{
		PhoneNumberID:       cfg.WhatsAppPhoneNumberID,
		AccessToken:         cfg.WhatsAppAccessToken,
		WebhookVerifySecret: cfg.WhatsAppWebhookSecret,
	})
	if err != nil {
		log.Error("invalid WhatsApp config — refusing to start", "err", err)
		os.Exit(1)
	}
	log.Warn("integrations: WhatsApp running with the fail-closed stub — order updates are DISABLED until the post-investors Meta Cloud API onboarding (see docs/integrations.md)")
	if cfg.WhatsAppAccessToken != "" || cfg.WhatsAppPhoneNumberID != "" {
		log.Warn("integrations: WHATSAPP_* credentials are set but no real provider is wired — they are IGNORED; no messages can be sent")
	}
	if cfg.WhatsAppWebhookSecret == "" {
		log.Warn("integrations: WHATSAPP_WEBHOOK_VERIFY_TOKEN is unset — the /internal/whatsapp/webhook handshake fails closed (401); inbound Meta callbacks are REFUSED")
	}
	// The Tally bridge is kept and passed to newHandler: the voucher
	// route resolves it per seller against the seller_tally_bridges
	// registry (H2 part 2b). The stub is the only wired provider until
	// the post-investors bridge-agent deployment.
	tallyBridge, err := integrations.NewTally(cfg.TallyProvider, integrations.TallyConfig{
		BridgeURL:    cfg.TallyBridgeURL,
		SharedSecret: cfg.TallySharedSecret,
	})
	if err != nil {
		log.Error("invalid Tally config — refusing to start", "err", err)
		os.Exit(1)
	}
	log.Warn("integrations: Tally bridge running with the fail-closed stub — accounting vouchers are DISABLED until a merchant bridge agent is deployed post-investors (see docs/integrations.md)")
	if cfg.TallyBridgeURL != "" {
		log.Warn("integrations: TALLY_BRIDGE_URL is set but no real bridge is wired — it is IGNORED; no vouchers can be posted")
	}
	// Zoho Books is kept and passed to newHandler: the
	// /v1/integrations/zoho/* routes (seller-scoped, deployment-wide
	// provider) need it. The stub is the only wired provider until the
	// post-investors Zoho OAuth onboarding.
	zohoProvider, err := integrations.NewZoho(cfg.ZohoProvider, integrations.ZohoConfig{
		ClientID:     cfg.ZohoClientID,
		ClientSecret: cfg.ZohoClientSecret,
		RefreshToken: cfg.ZohoRefreshToken,
		OrgID:        cfg.ZohoOrgID,
	})
	if err != nil {
		log.Error("invalid Zoho config — refusing to start", "err", err)
		os.Exit(1)
	}
	log.Warn("integrations: Zoho Books running with the fail-closed stub — invoice sync is DISABLED until the post-investors Zoho OAuth onboarding (see docs/integrations.md)")
	if cfg.ZohoClientID != "" || cfg.ZohoRefreshToken != "" {
		log.Warn("integrations: ZOHO_* credentials are set but no real provider is wired — they are IGNORED; no invoices can be created")
	}

	// Subscription-billing seam (H4): fail fast on an unknown
	// SUBSCRIPTIONS_PROVIDER so a misconfigured provider name refuses
	// to boot instead of silently billing nothing. Until the
	// post-investors Cashfree Subscriptions + UPI AutoPay onboarding the
	// only provider is the fail-closed stub (see internal/subscriptions):
	// every operation — CreateSubscription, CancelSubscription,
	// VerifyWebhook — is an explicit typed error, never a mandate and
	// never a charge. The constructed provider is consumed by the
	// membership routes (H4 part 2). Cashfree credentials are shared
	// with the payments seam (CASHFREE_*): loaded, never logged, never
	// committed; the stub ignores them.
	subProvider, err := subscriptions.NewProvider(cfg.SubscriptionsProvider)
	if err != nil {
		log.Error("invalid subscriptions config — refusing to start", "err", err)
		os.Exit(1)
	}
	log.Warn("subscriptions: running with the fail-closed stub — recurring billing is DISABLED until the post-investors Cashfree Subscriptions + UPI AutoPay onboarding (see docs/subscriptions.md)")
	if cfg.CashfreeAppID != "" || cfg.CashfreeSecret != "" {
		log.Warn("subscriptions: CASHFREE_* credentials are set but no real subscription provider is wired — they are IGNORED; no mandates can be created")
	}

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

	// Live media seam (G1): a LiveKit API key without its secret is a
	// misconfiguration — refuse to boot rather than mint tokens nobody
	// can verify. An empty key+secret pair simply disables the seam.
	if cfg.LiveKitAPIKey != "" && cfg.LiveKitAPISecret == "" {
		log.Error("LIVEKIT_API_KEY is set but LIVEKIT_API_SECRET is empty — refusing to start")
		os.Exit(1)
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

	// Order events (F6 part 1): NATS JetStream order bus. Empty URL =
	// disabled (loud WARN, no-op bus); a URL that will not connect is
	// a configuration bug — refuse to boot, same as search/realtime/
	// aiproxy.
	orderBus, err := events.NewBus(cfg.NatsURL, log)
	if err != nil {
		log.Error("invalid NATS config — refusing to start", "err", err)
		os.Exit(1)
	}
	if orderBus.Enabled() {
		log.Info("order event bus enabled", "nats", cfg.NatsURL)
	}

	// Developer webhooks (G2 part 1): self-hosted Svix server. Empty
	// token = dispatcher disabled (dev default, loud WARN); a token
	// with a bad URL is a configuration bug — refuse to boot, same as
	// search/realtime/aiproxy. Routes land in G2 part 2.
	whClient, err := webhooks.NewClient(cfg.SvixURL, cfg.SvixAPIToken, log)
	if err != nil {
		log.Error("invalid Svix config — refusing to start", "err", err)
		os.Exit(1)
	}
	if whClient.Enabled() {
		svixURL := cfg.SvixURL
		if svixURL == "" {
			svixURL = webhooks.DefaultURL
		}
		log.Info("developer webhooks enabled", "svix", svixURL)
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel() // stops the live reconciler (G1) on shutdown
	handler := newHandler(bgCtx, cfg, pool, verifier, feedSvc, mediaSvc, rtPub, aiProxy, log, translator, searchClient, orderBus, whClient, waProvider, tallyBridge, zohoProvider, subProvider)
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
