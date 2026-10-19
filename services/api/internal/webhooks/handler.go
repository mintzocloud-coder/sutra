// SPDX-License-Identifier: AGPL-3.0-or-later
package webhooks

// Developer-facing webhook routes (G2 part 2). All routes sit behind
// RequireAuth; each developer manages exactly one Svix application —
// their own `user_<id>` app — and can never touch another user's.
//
// Routes:
//   POST   /v1/webhooks/endpoints            register a delivery target
//   GET    /v1/webhooks/endpoints            list registered targets
//   DELETE /v1/webhooks/endpoints/{id}       remove a target (idempotent)
//   POST   /v1/webhooks/events               publish a custom event
//   GET    /v1/webhooks/deliveries?message_id=  Svix attempt log
//
// When the dispatcher is disabled (no SVIX_API_TOKEN) these routes are
// not registered at all (see cmd/api/main.go); a disabled client that
// somehow reaches a handler answers 503 webhooks_unavailable.
//
// Signing and retries are Svix's job — this API never signs deliveries
// or stores endpoint secrets (Svix returns the secret once, at
// creation; the create response below is the only time it is shown).
// See docs/webhooks.md for the verification guide.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/events"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

// maxWebhookBody caps developer-route request bodies (payloads are
// arbitrary JSON, but a 1 MiB ceiling keeps a malicious client from
// pinning a handler goroutine on a giant decode).
const maxWebhookBody = 1 << 20

// eventTypeRe is the grammar for event types: dotted lowercase names
// (order.created, shipment:update) plus developer custom events. Kept
// intentionally ASCII — event types are wire identifiers, not prose.
var eventTypeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// Handler serves the developer webhook routes for one API instance.
type Handler struct {
	pool   *db.Pool
	client *Client
	log    *slog.Logger
	// userID resolves the OIDC subject to the users-table UUID whose
	// Svix app the developer manages. Production just-in-time
	// provisions; tests substitute a stub (no DB needed).
	userID func(ctx context.Context, subject string) (string, error)
}

// NewHandler builds the developer-route handler. The client may be a
// disabled client (no token) — every route then answers 503; main.go
// normally keeps the routes unregistered in that case.
func NewHandler(pool *db.Pool, client *Client, log *slog.Logger) *Handler {
	return &Handler{
		pool:   pool,
		client: client,
		log:    log,
		userID: func(ctx context.Context, subject string) (string, error) {
			u, err := users.Provision(ctx, pool, subject, "")
			if err != nil {
				return "", err
			}
			return u.ID, nil
		},
	}
}

// callerApp resolves the request's subject to the Svix app UID the
// caller owns, writing the error envelope on failure. ok=false means a
// response was already written.
func (h *Handler) callerApp(w http.ResponseWriter, r *http.Request) (appUID string, ok bool) {
	if h.client == nil || !h.client.Enabled() {
		w.Header().Set("Retry-After", "60")
		middleware.WriteJSONError(w, r, http.StatusServiceUnavailable,
			"webhooks_unavailable", "developer webhooks are not enabled on this deployment")
		return "", false
	}
	subject := middleware.SubjectFromContext(r.Context())
	if subject == "" {
		middleware.WriteStatusError(w, r, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	userID, err := h.userID(r.Context(), subject)
	if err != nil {
		h.log.Error("webhooks: user resolution failed", "err", err,
			"request_id", middleware.FromContext(r.Context()))
		middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
		return "", false
	}
	return AppUIDForUser(userID), true
}

// svixError maps Svix client failures onto the error envelope. The
// management token is never leaked: a 401/403 from Svix means OUR
// server-side credential is bad (a deployment bug), so it becomes a
// 500 with a generic message, logged loudly.
func (h *Handler) svixError(w http.ResponseWriter, r *http.Request, op string, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.NotFound():
			middleware.WriteJSONError(w, r, http.StatusNotFound,
				"webhook_not_found", "no such webhook endpoint or message")
			return
		case apiErr.Unauthorized():
			h.log.Error("webhooks: svix rejected our management token — SVIX_API_TOKEN is bad or expired",
				"op", op, "request_id", middleware.FromContext(r.Context()))
			middleware.WriteJSONError(w, r, http.StatusInternalServerError,
				"webhooks_misconfigured", "webhook gateway is misconfigured")
			return
		case apiErr.Validation():
			msg := "the webhook request failed server-side validation"
			if apiErr.Detail != "" {
				msg = "svix: " + apiErr.Detail
			}
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"webhooks_invalid", msg)
			return
		default:
			h.log.Warn("webhooks: svix upstream error", "op", op,
				"status", apiErr.Status, "request_id", middleware.FromContext(r.Context()))
			middleware.WriteJSONError(w, r, http.StatusBadGateway,
				"webhooks_upstream", "webhook gateway returned an error")
			return
		}
	}
	h.log.Warn("webhooks: svix unreachable", "op", op, "err", err,
		"request_id", middleware.FromContext(r.Context()))
	middleware.WriteJSONError(w, r, http.StatusBadGateway,
		"webhooks_upstream", "webhook gateway is unreachable")
}

// writeJSONResp is the success-path JSON writer (the test file owns
// the bare `writeJSON` name for its stub server).
func writeJSONResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeBody JSON-decodes the request with a size cap and no unknown
// fields; every failure shape gets the 400 envelope.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_request_body", "request body must be valid JSON matching the documented schema")
		return false
	}
	return true
}

// validEventType reports whether s is a well-formed event type.
func validEventType(s string) bool { return eventTypeRe.MatchString(s) }

// validEndpointURL reports whether s is an http(s) delivery target.
// Localhost is allowed (dev + the smoke canary); production
// developers register public URLs (see docs/webhooks.md).
func validEndpointURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// Endpoints dispatches POST (register) and GET (list) on
// /v1/webhooks/endpoints; anything else is a 405 with Allow.
func (h *Handler) Endpoints() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			h.createEndpoint(w, r)
		case http.MethodGet:
			h.listEndpoints(w, r)
		default:
			middleware.MethodNotAllowedHandler(http.MethodPost, http.MethodGet)(w, r)
		}
	}
}

type endpointCreateIn struct {
	URL         string   `json:"url"`
	Description string   `json:"description"`
	FilterTypes []string `json:"filter_types"`
	Secret      string   `json:"secret"`
}

// createEndpoint registers a delivery target on the caller's Svix app.
// 201 carries the endpoint INCLUDING its signing secret — the one and
// only time the secret is shown. Hand it to the developer now.
func (h *Handler) createEndpoint(w http.ResponseWriter, r *http.Request) {
	appUID, ok := h.callerApp(w, r)
	if !ok {
		return
	}
	var in endpointCreateIn
	if !decodeBody(w, r, &in) {
		return
	}
	if !validEndpointURL(in.URL) {
		middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
			"invalid_endpoint_url", "endpoint url must be an http(s) URL with a host")
		return
	}
	for _, ft := range in.FilterTypes {
		if !validEventType(ft) {
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"invalid_event_type", fmt.Sprintf("filter type %q is not a valid event type", ft))
			return
		}
	}
	ctx := r.Context()
	if _, err := h.client.EnsureApp(ctx, appUID, appUID); err != nil {
		h.svixError(w, r, "ensure app", err)
		return
	}
	// Just-in-time event-type registration: Svix rejects endpoint
	// filters that name unregistered event types (422), so ensure
	// each filter type exists before the endpoint create — same
	// philosophy as the app ensure above.
	for _, ft := range in.FilterTypes {
		if err := h.client.EnsureEventType(ctx, ft,
			"auto-registered by the Sutra webhooks API"); err != nil {
			h.svixError(w, r, "ensure event type", err)
			return
		}
	}
	ep, err := h.client.CreateEndpoint(ctx, appUID, EndpointIn{
		URL:         in.URL,
		Description: in.Description,
		FilterTypes: in.FilterTypes,
		Secret:      in.Secret,
	})
	if err != nil {
		h.svixError(w, r, "create endpoint", err)
		return
	}
	writeJSONResp(w, http.StatusCreated, ep)
}

// listEndpoints returns the caller's registered delivery targets.
// Secrets are never returned here — Svix only shows them at creation.
func (h *Handler) listEndpoints(w http.ResponseWriter, r *http.Request) {
	appUID, ok := h.callerApp(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if _, err := h.client.EnsureApp(ctx, appUID, appUID); err != nil {
		h.svixError(w, r, "ensure app", err)
		return
	}
	eps, err := h.client.ListEndpoints(ctx, appUID)
	if err != nil {
		h.svixError(w, r, "list endpoints", err)
		return
	}
	writeJSONResp(w, http.StatusOK, map[string]any{"endpoints": eps})
}

// DeleteEndpoint handles DELETE /v1/webhooks/endpoints/{id}.
// Idempotent: deleting an already-gone endpoint is a 204 — the desired
// end state (no such endpoint) already holds.
func (h *Handler) DeleteEndpoint() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		appUID, ok := h.callerApp(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if id == "" {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_request_body", "endpoint id is required in the path")
			return
		}
		ctx := r.Context()
		if _, err := h.client.EnsureApp(ctx, appUID, appUID); err != nil {
			h.svixError(w, r, "ensure app", err)
			return
		}
		if err := h.client.DeleteEndpoint(ctx, appUID, id); err != nil {
			h.svixError(w, r, "delete endpoint", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type eventPublishIn struct {
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
	EventID   string          `json:"event_id"`
}

type eventPublishOut struct {
	ID        string `json:"id"`
	EventType string `json:"event_type"`
}

// PublishEvent handles POST /v1/webhooks/events: the developer
// publishes a custom event to their own app's endpoints. 202 —
// delivery is async (Svix retries); the attempt log proves it landed.
// event_id is the idempotency key: re-publishing the same event_id
// returns the ORIGINAL message instead of double-delivering.
func (h *Handler) PublishEvent() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		appUID, ok := h.callerApp(w, r)
		if !ok {
			return
		}
		var in eventPublishIn
		if !decodeBody(w, r, &in) {
			return
		}
		if !validEventType(in.EventType) {
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"invalid_event_type", "event_type must match [A-Za-z0-9][A-Za-z0-9._:-]{0,127}")
			return
		}
		if len(in.Payload) == 0 {
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"event_payload_required", "payload is required (any JSON value)")
			return
		}
		eventID := in.EventID
		if eventID == "" {
			eventID = events.NewEventID()
		}
		ctx := r.Context()
		if _, err := h.client.EnsureApp(ctx, appUID, appUID); err != nil {
			h.svixError(w, r, "ensure app", err)
			return
		}
		msg, err := h.client.Publish(ctx, appUID, in.EventType, in.Payload, eventID)
		if err != nil {
			h.svixError(w, r, "publish event", err)
			return
		}
		writeJSONResp(w, http.StatusAccepted, eventPublishOut{ID: msg.ID, EventType: msg.EventType})
	}
}

// Deliveries handles GET /v1/webhooks/deliveries?message_id=: Svix's
// per-message delivery-attempt log — the proof of delivery developers
// poll after publishing. Statuses are Svix wire values (success,
// pending, fail, sending) passed through verbatim.
func (h *Handler) Deliveries() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		appUID, ok := h.callerApp(w, r)
		if !ok {
			return
		}
		msgID := r.URL.Query().Get("message_id")
		if msgID == "" {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_request_body", "message_id query parameter is required")
			return
		}
		ctx := r.Context()
		if _, err := h.client.EnsureApp(ctx, appUID, appUID); err != nil {
			h.svixError(w, r, "ensure app", err)
			return
		}
		attempts, err := h.client.ListAttempts(ctx, appUID, msgID)
		if err != nil {
			h.svixError(w, r, "list attempts", err)
			return
		}
		writeJSONResp(w, http.StatusOK, map[string]any{
			"message_id": msgID,
			"attempts":   attempts,
		})
	}
}
