// SPDX-License-Identifier: AGPL-3.0-or-later
package subscriptions

// Membership HTTP routes (H4 part 2).
//
// Routes (registered in cmd/api/main.go):
//
//	POST   /v1/memberships/plans                  creator creates a plan → 201
//	GET    /v1/memberships/plans                  creator's plans → 200
//	GET    /v1/memberships/plans/{id}             owner read (404 for others) → 200
//	PATCH  /v1/memberships/plans/{id}             owner update (name/desc/active; price+period immutable) → 200
//	DELETE /v1/memberships/plans/{id}             deactivate (plans are never hard-deleted) → 200
//	POST   /v1/memberships/subscriptions         member subscribes → 201 (stub: 503 fail-closed)
//	GET    /v1/memberships/subscriptions         member's subscriptions → 200
//	GET    /v1/memberships/subscriptions/{id}    owner read → 200
//	POST   /v1/memberships/subscriptions/{id}/cancel  member cancel → 200 (stub: 503)
//	GET    /v1/memberships/entitlements          member's member_of:<creator> grants → 200
//	POST   /internal/subscriptions/webhook       aggregator callback (public; signature-verified inside) → 200
//
// Plans are creator-local: the creator is the caller's own user row
// (users.Provision on the OIDC subject) — no seller account required,
// memberships are creator economics, not commerce. Subscriptions are
// member-scoped the same way. The fail-closed rule is enforced in the
// SERVICE (provider refused before any local write), so these handlers
// never have to reason about money — they just map typed errors to the
// envelope.
//
// While the stub is wired: subscribe and cancel always answer 503
// subscriptions_unavailable (no mandate can exist, so the honest
// answer is "not available"); plans CRUD, list, and entitlements work
// fully (pure local config); the webhook ingress answers 503 (an
// unverified callback is never acknowledged).

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

// maxMembershipsBody caps membership JSON bodies: metadata only.
const maxMembershipsBody = 64 << 10

// maxSubscriptionWebhookBody caps aggregator callback bodies; Cashfree
// callbacks are small JSON envelopes.
const maxSubscriptionWebhookBody = 1 << 20

// phoneRe is the E.164 shape UPI AutoPay mandate setup requires.
var phoneRe = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// MembershipHandler serves the membership routes. userID is a plain
// func so tests can stub identity without touching the database;
// resolveMembershipUser is the production seam.
type MembershipHandler struct {
	pool     *db.Pool
	plans    *PlanService
	subs     *SubscriptionService
	provider Provider
	log      *slog.Logger
	userID   func(r *http.Request) (pgtype.UUID, error)
}

// NewMembershipHandler builds the membership handler. provider is the
// same fail-closed seam main.go constructs (discarded there until part
// 2 — here it is consumed by subscribe/cancel/webhook).
func NewMembershipHandler(pool *db.Pool, provider Provider, log *slog.Logger) *MembershipHandler {
	plans := NewPlanService(pool)
	return &MembershipHandler{
		pool:     pool,
		plans:    plans,
		subs:     NewSubscriptionService(pool, provider, plans),
		provider: provider,
		log:      log,
		userID:   resolveMembershipUser(pool),
	}
}

func resolveMembershipUser(pool *db.Pool) func(r *http.Request) (pgtype.UUID, error) {
	return func(r *http.Request) (pgtype.UUID, error) {
		u, err := users.Provision(r.Context(), pool, middleware.SubjectFromContext(r.Context()), "")
		if err != nil {
			return pgtype.UUID{}, err
		}
		var id pgtype.UUID
		if err := id.Scan(u.ID); err != nil {
			return pgtype.UUID{}, err
		}
		return id, nil
	}
}

// writeJSON answers 200/201 with the JSON body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// mapError turns typed service/provider errors into the envelope.
func mapError(w http.ResponseWriter, r *http.Request, err error) {
	var disabled *ErrSubscriptionsDisabled
	switch {
	case errors.As(err, &disabled):
		// Fail-closed: the provider is unwired. 503 (not 500) — the
		// operation is unavailable, not broken. Retry-After signals a
		// config change, not a traffic spike; the value is honest.
		w.Header().Set("Retry-After", "3600")
		middleware.WriteJSONError(w, r, http.StatusServiceUnavailable, "subscriptions_unavailable",
			"subscription billing is not available yet — Cashfree Subscriptions + UPI AutoPay onboarding is post-investors")
	case errors.Is(err, ErrPlanNotFound), errors.Is(err, ErrSubscriptionNotFound), errors.Is(err, ErrWebhookNoRow):
		middleware.WriteStatusError(w, r, http.StatusNotFound, "not found")
	case errors.Is(err, ErrAlreadySubscribed), errors.Is(err, ErrBadTransition):
		middleware.WriteJSONError(w, r, http.StatusConflict,
			middleware.ErrorCode(http.StatusConflict), err.Error())
	case errors.Is(err, ErrInvalidPlanInput):
		middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_field", err.Error())
	default:
		middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal error")
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxMembershipsBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return false
	}
	return true
}

// parseUUID maps a bad path id to 400 (never 404 — a malformed id is a
// client error, and it must not be distinguishable from "not yours").
func parseUUID(w http.ResponseWriter, r *http.Request, raw string) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_id", "id is not a valid uuid")
		return pgtype.UUID{}, false
	}
	return id, true
}

// PlansMux dispatches POST (create) / GET (list mine) on the plans
// collection.
func (h *MembershipHandler) PlansMux() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			h.createPlan(w, r)
		case http.MethodGet:
			h.listPlans(w, r)
		default:
			middleware.WriteStatusError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

type planCreateBody struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	PricePaise    int64  `json:"price_paise"`
	BillingPeriod string `json:"billing_period"`
	Currency      string `json:"currency"`
}

func (h *MembershipHandler) createPlan(w http.ResponseWriter, r *http.Request) {
	var body planCreateBody
	if !decodeBody(w, r, &body) {
		return
	}
	creatorID, err := h.userID(r)
	if err != nil {
		mapError(w, r, err)
		return
	}
	period := BillingPeriod(body.BillingPeriod)
	if body.Currency == "" {
		body.Currency = CurrencyINR
	}
	p, err := h.plans.CreatePlan(r.Context(), creatorID, body.Name, body.Description,
		body.PricePaise, period, body.Currency)
	if err != nil {
		mapError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *MembershipHandler) listPlans(w http.ResponseWriter, r *http.Request) {
	creatorID, err := h.userID(r)
	if err != nil {
		mapError(w, r, err)
		return
	}
	limit := pageLimit(r)
	plans, err := h.plans.ListPlans(r.Context(), creatorID, limit)
	if err != nil {
		mapError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": plans})
}

// PlanMux dispatches GET / PATCH / DELETE on a single plan.
func (h *MembershipHandler) PlanMux() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseUUID(w, r, r.PathValue("id"))
		if !ok {
			return
		}
		creatorID, err := h.userID(r)
		if err != nil {
			mapError(w, r, err)
			return
		}
		switch r.Method {
		case http.MethodGet:
			p, err := h.plans.GetPlan(r.Context(), creatorID, id)
			if err != nil {
				mapError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, p)
		case http.MethodPatch:
			// price_paise / billing_period are immutable (see
			// PlanService.UpdatePlan): a client that sends them gets a
			// loud 400, never a silent ignore.
			var raw map[string]json.RawMessage
			r.Body = http.MaxBytesReader(w, r.Body, maxMembershipsBody)
			if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
				middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
				return
			}
			for _, k := range []string{"price_paise", "billing_period"} {
				if _, present := raw[k]; present {
					middleware.WriteJSONError(w, r, http.StatusBadRequest, "immutable_field",
						k+" is immutable — create a new plan for a new price or period")
					return
				}
			}
			var body struct {
				Name        *string `json:"name"`
				Description *string `json:"description"`
				IsActive    *bool   `json:"is_active"`
			}
			rawBytes, _ := json.Marshal(raw)
			if err := json.Unmarshal(rawBytes, &body); err != nil {
				middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
				return
			}
			p, err := h.plans.UpdatePlan(r.Context(), creatorID, id, UpdatePlanInput{
				Name: body.Name, Description: body.Description, IsActive: body.IsActive,
			})
			if err != nil {
				mapError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, p)
		case http.MethodDelete:
			p, err := h.plans.DeactivatePlan(r.Context(), creatorID, id)
			if err != nil {
				mapError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, p)
		default:
			middleware.WriteStatusError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

// SubscriptionsMux dispatches POST (subscribe) / GET (list mine).
func (h *MembershipHandler) SubscriptionsMux() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			h.subscribe(w, r)
		case http.MethodGet:
			h.listMine(w, r)
		default:
			middleware.WriteStatusError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

type subscribeBody struct {
	PlanID        string `json:"plan_id"`
	CustomerName  string `json:"customer_name"`
	CustomerPhone string `json:"customer_phone"`
	CustomerEmail string `json:"customer_email"`
}

func (h *MembershipHandler) subscribe(w http.ResponseWriter, r *http.Request) {
	var body subscribeBody
	if !decodeBody(w, r, &body) {
		return
	}
	planID, ok := parseUUID(w, r, body.PlanID)
	if !ok {
		return
	}
	// Customer fields are validated up front so a future real provider
	// rejects nothing the stub accepted: name non-empty (rune-counted),
	// phone E.164 (UPI AutoPay mandate setup requires it), email
	// shape-checked loosely (no MX lookup — the aggregator owns that).
	if strings.TrimSpace(body.CustomerName) == "" || len([]rune(body.CustomerName)) > 120 {
		middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_field", "customer_name must be 1..120 characters")
		return
	}
	if !phoneRe.MatchString(body.CustomerPhone) {
		middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_field", "customer_phone must be E.164 (e.g. +919876543210)")
		return
	}
	if !strings.Contains(body.CustomerEmail, "@") || len(body.CustomerEmail) > 254 {
		middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_field", "customer_email must be a valid email address")
		return
	}
	memberID, err := h.userID(r)
	if err != nil {
		mapError(w, r, err)
		return
	}
	res, err := h.subs.Subscribe(r.Context(), memberID, planID,
		strings.TrimSpace(body.CustomerName), body.CustomerPhone, strings.TrimSpace(body.CustomerEmail))
	if err != nil {
		mapError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"subscription": res.Subscription,
		"auth_url":     res.AuthURL,
	})
}

func (h *MembershipHandler) listMine(w http.ResponseWriter, r *http.Request) {
	memberID, err := h.userID(r)
	if err != nil {
		mapError(w, r, err)
		return
	}
	subs, err := h.subs.ListMine(r.Context(), memberID, pageLimit(r))
	if err != nil {
		mapError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": subs})
}

// SubscriptionMux answers GET on a single subscription (owner only).
func (h *MembershipHandler) SubscriptionMux() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseUUID(w, r, r.PathValue("id"))
		if !ok {
			return
		}
		memberID, err := h.userID(r)
		if err != nil {
			mapError(w, r, err)
			return
		}
		sub, err := h.subs.GetSubscription(r.Context(), memberID, id)
		if err != nil {
			mapError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, sub)
	}
}

// CancelMux answers POST /v1/memberships/subscriptions/{id}/cancel.
func (h *MembershipHandler) CancelMux() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseUUID(w, r, r.PathValue("id"))
		if !ok {
			return
		}
		var body struct {
			AtPeriodEnd *bool `json:"at_period_end"`
		}
		if r.ContentLength != 0 {
			if !decodeBody(w, r, &body) {
				return
			}
		}
		memberID, err := h.userID(r)
		if err != nil {
			mapError(w, r, err)
			return
		}
		atPeriodEnd := body.AtPeriodEnd != nil && *body.AtPeriodEnd
		sub, err := h.subs.CancelMember(r.Context(), memberID, id, atPeriodEnd)
		if err != nil {
			mapError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, sub)
	}
}

// Entitlements answers GET /v1/memberships/entitlements with the
// member's member_of:<creator_id> grants — the list membership gating
// (exclusive posts, paid groups, live tiers) checks. Computed from the
// DB, never from provider state.
func (h *MembershipHandler) Entitlements() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		memberID, err := h.userID(r)
		if err != nil {
			mapError(w, r, err)
			return
		}
		ids, err := h.subs.ActiveCreatorIDs(r.Context(), memberID)
		if err != nil {
			mapError(w, r, err)
			return
		}
		grants := EntitlementsFor(ids)
		if grants == nil {
			grants = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"entitlements": grants})
	}
}

// Webhook answers POST /internal/subscriptions/webhook — the aggregator
// subscription callback. Public by necessity (the aggregator calls it),
// like /internal/whatsapp/webhook; the signature check inside
// Provider.VerifyWebhook is the auth. While the stub is wired this
// always answers 503 — an unverified callback is never acknowledged,
// and no local state is ever touched.
func (h *MembershipHandler) Webhook() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSubscriptionWebhookBody))
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_body", "could not read webhook body")
			return
		}
		sig := r.Header.Get("X-Cashfree-Signature")
		ev, err := h.provider.VerifyWebhook(r.Context(), raw, sig)
		if err != nil {
			mapError(w, r, err)
			return
		}
		sub, err := h.subs.HandleWebhook(r.Context(), ev)
		if err != nil {
			h.log.Warn("subscriptions: webhook event rejected", "err", err)
			mapError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"subscription_id": sub.ID,
			"status":          sub.Status,
		})
	}
}

// pageLimit clamps the ?limit= param (cost guard).
func pageLimit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return 20
	}
	if n > 100 {
		return 100
	}
	return n
}
