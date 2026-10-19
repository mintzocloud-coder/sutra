// SPDX-License-Identifier: AGPL-3.0-or-later
package integrations

// WhatsApp send route + inbound webhook (H2 part 2a).
//
// Routes (registered in cmd/api/main.go):
//   POST /v1/integrations/whatsapp/send   seller-scoped send (behind RequireAuth)
//   GET  /internal/whatsapp/webhook       Meta hub-challenge verification (public,
//                                         like /internal/livekit/webhook — Meta calls it)
//   POST /internal/whatsapp/webhook       signed inbound callback (public, HMAC-verified)
//
// Send semantics: only a registered seller may send, and only to an E.164
// recipient. The stub provider (the only wired provider until the
// post-investors Meta onboarding) fails closed with 503
// whatsapp_unavailable + Retry-After — the API never pretends a message
// went out. Template grammar is validated up front so a future real
// provider rejects nothing the stub accepted.
//
// Inbound semantics: Meta's Cloud API calls the /internal webhook for
// both the subscription handshake (GET, hub.challenge) and the event
// callbacks (POST, X-Hub-Signature-256). The signature check is the real
// VerifyWebhookSignature from part 1 — pure crypto, no network. Wave 1
// validates + acknowledges the callback and logs its event kinds;
// inbound message processing (persisting buyer messages, marking order
// updates delivered) is Wave 2 — the 200 reply never implies anything
// was stored. The hub handshake fails closed when
// WHATSAPP_WEBHOOK_VERIFY_TOKEN is unset: without a secret there is no
// legitimate token to compare against, so every handshake is refused.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

// maxWhatsAppSendBody caps send-route request bodies (templates carry
// parameter strings; 64 KiB keeps a malicious client from pinning a
// handler goroutine on a giant decode).
const maxWhatsAppSendBody = 64 << 10

// maxWhatsAppWebhookBody caps inbound webhook bodies; Meta callbacks are
// small JSON envelopes.
const maxWhatsAppWebhookBody = 1 << 20

// maxWhatsAppTextRunes caps a free-form message at Meta's text limit.
const maxWhatsAppTextRunes = 4096

// maxWhatsAppParamRunes caps a single template parameter.
const maxWhatsAppParamRunes = 1000

// whatsappTemplateLanguages is the curated allowlist for the send
// route's template language field. Meta supports many more; the
// allowlist grows as the business onboards markets.
var whatsappTemplateLanguages = map[string]bool{
	"en": true, "hi": true, "gu": true, "ta": true,
	"te": true, "bn": true, "kn": true, "ml": true,
	"mr": true,
}

// whatsappTemplateComponentTypes mirrors Meta's template component
// grammar (body/header/button parameters).
var whatsappTemplateComponentTypes = map[string]bool{
	"body": true, "header": true, "button": true,
}

// WhatsAppHandler serves the send route and the inbound webhook for one
// API instance.
type WhatsAppHandler struct {
	pool *db.Pool
	wa   WhatsAppProvider
	// webhookSecret is WHATSAPP_WEBHOOK_VERIFY_TOKEN (the Meta app
	// secret): hub-challenge verification and inbound HMAC checks both
	// fail closed when it is empty.
	webhookSecret string
	log           *slog.Logger
	// sellerID resolves the OIDC subject to the seller row id the
	// caller owns. Production queries the real tables; tests
	// substitute a stub (no DB needed).
	sellerID func(ctx context.Context, subject string) (string, error)
}

// NewWhatsAppHandler builds the handler. wa may be nil (tests, or a
// deployment that wired the webhook before the send seam): the send
// route then answers 503; the webhook still verifies signatures.
func NewWhatsAppHandler(pool *db.Pool, wa WhatsAppProvider, webhookSecret string, log *slog.Logger) *WhatsAppHandler {
	return &WhatsAppHandler{
		pool:          pool,
		wa:            wa,
		webhookSecret: webhookSecret,
		log:           log,
		sellerID:      resolveSeller(pool),
	}
}

// ---------------------------------------------------------------------------
// Send route: POST /v1/integrations/whatsapp/send
// ---------------------------------------------------------------------------

// WhatsAppSendRequest is the send-route body: exactly one of text or
// template must be present.
type WhatsAppSendRequest struct {
	To       string `json:"to"`
	Text     string `json:"text,omitempty"`
	Template *struct {
		Name       string `json:"name"`
		Language   string `json:"language"`
		Components []struct {
			Type       string   `json:"type"`
			Parameters []string `json:"parameters"`
		} `json:"components,omitempty"`
	} `json:"template,omitempty"`
}

// validateSendRequest enforces the request grammar before any provider
// call; it returns the envelope code for the first violation.
func validateSendRequest(req *WhatsAppSendRequest) (code string, msg string) {
	if err := ValidatePhone(req.To); err != nil {
		return "invalid_phone", err.Error()
	}
	hasText := strings.TrimSpace(req.Text) != ""
	hasTemplate := req.Template != nil
	switch {
	case hasText && hasTemplate:
		return "invalid_request", "send either \"text\" or \"template\", not both"
	case !hasText && !hasTemplate:
		return "invalid_request", "send one of \"text\" or \"template\""
	}
	if hasText {
		if n := utf8.RuneCountInString(req.Text); n > maxWhatsAppTextRunes {
			return "invalid_request", "text exceeds 4096 characters"
		}
		return "", ""
	}
	t := req.Template
	if strings.TrimSpace(t.Name) == "" || len(t.Name) > 512 {
		return "invalid_request", "template.name is required (max 512 chars)"
	}
	if !whatsappTemplateLanguages[t.Language] {
		return "invalid_request", "template.language must be one of: en hi gu ta te bn kn ml mr"
	}
	for i, c := range t.Components {
		if !whatsappTemplateComponentTypes[c.Type] {
			return "invalid_request", "template.components[" + itoa(i) + "].type must be body, header, or button"
		}
		for _, p := range c.Parameters {
			if utf8.RuneCountInString(p) > maxWhatsAppParamRunes {
				return "invalid_request", "template.components[" + itoa(i) + "].parameters exceed 1000 characters"
			}
		}
	}
	return "", ""
}

// itoa renders a small non-negative int without importing strconv into
// the package doc surface; it exists only for error strings.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

// Send serves POST /v1/integrations/whatsapp/send.
func (h *WhatsAppHandler) Send() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject := middleware.SubjectFromContext(r.Context())
		if subject == "" {
			middleware.WriteStatusError(w, r, http.StatusUnauthorized, "unauthorized")
			return
		}
		var req WhatsAppSendRequest
		dec := json.NewDecoder(io.LimitReader(r.Body, maxWhatsAppSendBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_request", "request body must be valid JSON with known fields only")
			return
		}
		if code, msg := validateSendRequest(&req); code != "" {
			status := http.StatusBadRequest
			if code == "invalid_phone" {
				status = http.StatusUnprocessableEntity
			}
			middleware.WriteJSONError(w, r, status, code, msg)
			return
		}
		sellerID, err := h.sellerID(r.Context(), subject)
		if err != nil {
			if errors.Is(err, errNoSellerRow) {
				middleware.WriteJSONError(w, r, http.StatusForbidden,
					"not_a_seller", "WhatsApp sends are seller-scoped: register a seller account first")
				return
			}
			h.log.Error("whatsapp: seller resolution failed", "err", err,
				"request_id", middleware.FromContext(r.Context()))
			middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
			return
		}
		if h.wa == nil {
			h.disabled(w, r)
			return
		}
		ctx := r.Context()
		var (
			messageID string
			provErr   error
		)
		if req.Template != nil {
			t := WhatsAppTemplate{To: req.To, Name: req.Template.Name, Language: req.Template.Language}
			for _, c := range req.Template.Components {
				t.Components = append(t.Components, TemplateComponent{Type: c.Type, Parameters: c.Parameters})
			}
			messageID, provErr = h.wa.SendTemplate(ctx, t)
		} else {
			messageID, provErr = h.wa.SendText(ctx, WhatsAppText{To: req.To, Body: req.Text})
		}
		if provErr != nil {
			var dis *ErrIntegrationDisabled
			if errors.As(provErr, &dis) {
				h.disabled(w, r)
				return
			}
			h.log.Error("whatsapp: provider send failed", "err", provErr,
				"seller_id", sellerID, "request_id", middleware.FromContext(r.Context()))
			middleware.WriteJSONError(w, r, http.StatusBadGateway,
				"whatsapp_upstream", "the WhatsApp gateway failed to accept the message")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"message_id": messageID})
	})
}

// errNoSellerRow is the sentinel the sellerID seam returns when the
// caller owns no seller row.
var errNoSellerRow = errors.New("integrations: caller has no seller account")

// disabled answers with the honest 503: the stub cannot send, and the
// API must not imply otherwise.
func (h *WhatsAppHandler) disabled(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "60")
	middleware.WriteJSONError(w, r, http.StatusServiceUnavailable,
		"whatsapp_unavailable", "WhatsApp sends are disabled on this deployment — Meta Cloud API onboarding is post-investors (see docs/integrations.md)")
}

// ---------------------------------------------------------------------------
// Inbound webhook: /internal/whatsapp/webhook (GET + POST)
// ---------------------------------------------------------------------------

// Webhook serves /internal/whatsapp/webhook, dispatching on method:
// GET answers Meta's subscription handshake; POST handles the signed
// event callback.
func (h *WhatsAppHandler) Webhook() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			h.hubChallenge(w, r)
			return
		}
		h.inboundCallback(w, r)
	})
}

// hubChallenge answers Meta's subscription handshake: hub.mode=subscribe
// + the exact hub.verify_token we configured in the Meta dashboard, and
// the raw hub.challenge is echoed back verbatim. Fails closed when the
// verify secret is unset — no secret, no handshake.
func (h *WhatsAppHandler) hubChallenge(w http.ResponseWriter, r *http.Request) {
	if h.webhookSecret == "" {
		middleware.WriteJSONError(w, r, http.StatusUnauthorized,
			"whatsapp_webhook_unconfigured", "the WhatsApp webhook verify token is not configured on this deployment")
		return
	}
	q := r.URL.Query()
	if q.Get("hub.mode") != "subscribe" {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_request", "hub.mode must be \"subscribe\"")
		return
	}
	if q.Get("hub.verify_token") != h.webhookSecret {
		middleware.WriteJSONError(w, r, http.StatusForbidden,
			"webhook_verify_failed", "hub.verify_token does not match")
		return
	}
	challenge := q.Get("hub.challenge")
	if challenge == "" {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_request", "hub.challenge is required")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, challenge)
}

// webhookEnvelope is the minimal Meta callback shape we inspect:
// enough to count events and reject foreign callbacks, without
// pretending to process inbound messages in Wave 1.
type webhookEnvelope struct {
	Object string `json:"object"`
	Entry  []struct {
		Changes []struct {
			Value struct {
				MessagingProduct string `json:"messaging_product"`
				Metadata         struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				Contacts []json.RawMessage `json:"contacts"`
				Messages []json.RawMessage `json:"messages"`
				Statuses []json.RawMessage `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// inboundCallback verifies the X-Hub-Signature-256 HMAC over the raw
// body, then acknowledges the callback. Wave 1 inspects and logs the
// event kinds (messages vs statuses); inbound message processing is
// Wave 2 — the 200 reply acknowledges receipt only.
func (h *WhatsAppHandler) inboundCallback(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWhatsAppWebhookBody))
	if err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_request", "could not read the webhook body")
		return
	}
	if err := VerifyWebhookSignature(raw, h.webhookSecret, r.Header.Get("X-Hub-Signature-256")); err != nil {
		middleware.WriteJSONError(w, r, http.StatusUnauthorized,
			"webhook_signature_invalid", "the callback signature could not be verified")
		return
	}
	var env webhookEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_request", "the webhook body is not valid JSON")
		return
	}
	if env.Object != "whatsapp_business_account" {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_request", "unknown webhook object")
		return
	}
	var msgs, statuses int
	for _, e := range env.Entry {
		for _, c := range e.Changes {
			msgs += len(c.Value.Messages)
			statuses += len(c.Value.Statuses)
		}
	}
	h.log.Info("whatsapp: inbound callback received",
		"messages", msgs, "statuses", statuses,
		"request_id", middleware.FromContext(r.Context()))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "received"})
}

// resolveSeller is the production sellerID seam: provision the user,
// then require a seller row. It stays a plain func so tests can stub
// the handler's sellerID field instead of touching the database.
func resolveSeller(pool *db.Pool) func(ctx context.Context, subject string) (string, error) {
	return func(ctx context.Context, subject string) (string, error) {
		u, err := users.Provision(ctx, pool, subject, "")
		if err != nil {
			return "", err
		}
		var id string
		if err := pool.QueryRow(ctx,
			`SELECT id::text FROM sellers WHERE user_id = $1::uuid`, u.ID).
			Scan(&id); err != nil {
			return "", errNoSellerRow
		}
		return id, nil
	}
}
