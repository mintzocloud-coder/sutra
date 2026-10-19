// SPDX-License-Identifier: AGPL-3.0-or-later
// Package webhooks is the Go side of the Wave-4 developer webhooks
// (G2): a minimal stdlib-only client for the self-hosted Svix server
// (svix/svix-server, MIT, pinned in docker-compose.yml) plus the
// Sutra-side conventions that sit on top of it.
//
// DESIGN: the Svix server owns the hard parts — per-endpoint HMAC
// signatures (svix-id / svix-timestamp / svix-signature headers, v1
// scheme), the delivery retry schedule, and the attempt log. The Go
// API never signs or retries webhook deliveries itself; it maps
// Sutra users to Svix *applications* (one app per user, uid
// "user_<user-id>"), registers developer endpoints on them, and
// publishes platform events to them. A developer's endpoint secret
// (returned once, at creation) is the shared key they use to verify
// signatures; the API never stores it — Svix does.
//
// No third-party Svix SDK: the v1 REST surface we need (app lookup/
// create, endpoint create/list/delete, message create, attempt list)
// is six tiny HTTP calls, and a vendored SDK would be a bigger API to
// audit than the protocol itself.
//
// AUTH: the Svix server's management API takes `Authorization: Bearer
// <JWT>` where the JWT is minted with the server's SVIX_JWT_SECRET
// (`svix-server jwt generate`). The token arrives at this API as the
// SVIX_API_TOKEN env var — it is a Bearer credential, never logged,
// never committed.
//
// CONTRACTS (mirror search/realtime/aiproxy):
//   - empty token = dispatcher disabled (loud WARN, every call is a
//     no-op returning ErrWebhooksDisabled);
//   - bad URL with a token set = configuration bug -> fail-closed
//     error at boot, the API refuses to start;
//   - empty URL with a token set = the compose default
//     (http://svix-server:8071).
package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultURL is the compose service name for the Svix server. Used
// when SVIX_API_TOKEN is set but SVIX_URL is empty.
const DefaultURL = "http://svix-server:8071"

// callTimeout bounds one Svix API call. Publish calls happen from
// detached best-effort goroutines with their own deadline; this is
// the inner bound so a hung Svix server cannot pile up writers.
const callTimeout = 10 * time.Second

// ErrWebhooksDisabled is returned by every Client method when the
// dispatcher was constructed without a token (SVIX_API_TOKEN empty).
// Callers that publish best-effort should swallow it; callers on the
// developer-facing routes map it to 503.
var ErrWebhooksDisabled = errors.New("webhooks: dispatcher disabled (SVIX_API_TOKEN empty)")

// APIError is a non-2xx from the Svix server's management API.
type APIError struct {
	Status int    // HTTP status
	Code   string // svix "code" field when present, else the status text
	// Detail is Svix's "detail" field when present. Svix 422s carry
	// detail as an ARRAY of {loc,msg,type} items (not a string), so it
	// is captured verbatim (JSON) — a string-only parse silently drops
	// the real reason (triaged the hard way: three CI rounds stared at
	// the generic fallback before the array shape was found).
	Detail string
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("webhooks: svix api %d (%s): %s", e.Status, e.Code, e.Detail)
	}
	return fmt.Sprintf("webhooks: svix api %d (%s)", e.Status, e.Code)
}

// NotFound reports whether the error is a 404 from Svix (unknown app,
// endpoint, or message).
func (e *APIError) NotFound() bool { return e.Status == http.StatusNotFound }

// Unauthorized reports whether the error is a 401/403 from Svix (bad
// or expired SVIX_API_TOKEN).
func (e *APIError) Unauthorized() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// Validation reports whether the error is a 422 from Svix (the
// request body failed server-side validation).
func (e *APIError) Validation() bool { return e.Status == http.StatusUnprocessableEntity }

// AppUIDForUser maps a Sutra user id to the Svix application uid that
// owns that user's webhook endpoints. One app per user, stable and
// URL-safe. The separator is an UNDERSCORE, not a colon: Svix validates
// UIDs against ^[a-zA-Z0-9\-_.]+$ (server/svix-server/src/core/types),
// so "user:<uuid>" 422s on the real server (triaged in smoke — the stub
// servers in the tests never enforced the grammar).
func AppUIDForUser(userID string) string { return "user_" + userID }

// Endpoint is one developer-registered delivery target on their Svix
// application.
type Endpoint struct {
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	Description string   `json:"description,omitempty"`
	FilterTypes []string `json:"filterTypes,omitempty"`
	// Secret is the per-endpoint signing key. Svix NEVER includes it in
	// the create response: self-hosted svix-server (v1.101.0, verified
	// against its source) stores the key encrypted and exposes it only
	// through the dedicated secret read — CreateEndpoint performs that
	// read immediately after create, so the handler can hand the key to
	// the developer exactly once, in the 201. It is never returned again.
	// Hand it to the developer once and let them store it — the API
	// does not keep a copy.
	Secret string `json:"secret,omitempty"`
}

// EndpointIn is the create payload. FilterTypes is the v1 field name
// for the event-type filter list (the older name eventTypes was
// renamed in the v1 API; the smoke round-trip in G2 part 2 exercises
// the real server, which is the durable contract check).
type EndpointIn struct {
	URL         string   `json:"url"`
	Description string   `json:"description,omitempty"`
	FilterTypes []string `json:"filterTypes,omitempty"`
	// Secret, when empty, makes Svix generate a random one server-side;
	// when empty, CreateEndpoint reads the generated key back through
	// the dedicated secret endpoint and returns it (the create response
	// never carries it — see Endpoint.Secret). Supplying one lets
	// developers bring their own key.
	Secret string `json:"secret,omitempty"`
}

// MessageOut is the Svix server's reply to a message create.
type MessageOut struct {
	ID        string `json:"id"`
	EventType string `json:"eventType"`
}

// MessageStatus is Svix's delivery-attempt status. On the wire Svix
// sends it as an INTEGER enum (verified against svix-server v1.101.0:
// 0=success, 1=pending, 2=fail, 3=sending — the triage-the-hard-way
// finding: decoding the wire int into a Go string fails and the whole
// deliveries call collapses into a 502 "webhook gateway is
// unreachable"). The developer-facing API keeps the string form
// (success/pending/fail/sending), so Status marshals to its name.
type MessageStatus int

const (
	MessageStatusSuccess MessageStatus = 0
	MessageStatusPending MessageStatus = 1
	MessageStatusFail    MessageStatus = 2
	MessageStatusSending MessageStatus = 3
)

// String names the status the way the developer docs and the Svix SDKs
// do.
func (s MessageStatus) String() string {
	switch s {
	case MessageStatusSuccess:
		return "success"
	case MessageStatusPending:
		return "pending"
	case MessageStatusFail:
		return "fail"
	case MessageStatusSending:
		return "sending"
	default:
		return "unknown"
	}
}

// MarshalJSON emits the string name — the developer-facing contract
// for GET /v1/webhooks/deliveries is {"status":"success",...}.
func (s MessageStatus) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// UnmarshalJSON accepts both the wire integer enum and the
// developer-facing string name, so the type round-trips.
func (s *MessageStatus) UnmarshalJSON(raw []byte) error {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		*s = MessageStatus(n)
		return nil
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return err
	}
	switch name {
	case "success":
		*s = MessageStatusSuccess
	case "pending":
		*s = MessageStatusPending
	case "fail":
		*s = MessageStatusFail
	case "sending":
		*s = MessageStatusSending
	default:
		return fmt.Errorf("webhooks: unknown message status %q", name)
	}
	return nil
}

// Attempt is one delivery attempt of a message to one endpoint, from
// Svix's own attempt log — this is the delivery log the developer-
// facing routes surface. Status arrives as a Svix integer enum and is
// surfaced as its string name (success, pending, fail, sending) so
// clients can match the developer docs.
type Attempt struct {
	ID             string        `json:"id"`
	Status         MessageStatus `json:"status"`
	ResponseStatus int           `json:"responseStatusCode"`
	Timestamp      string        `json:"timestamp"`
}

// Client talks to one Svix server instance. The zero client is never
// valid — build it with NewClient.
type Client struct {
	base    string // e.g. http://svix-server:8071, no trailing slash
	token   string // SVIX_API_TOKEN, used as the Bearer value
	http    *http.Client
	log     *slog.Logger
	disable bool
}

// NewClient builds the Svix client. An empty token disables the
// dispatcher (loud WARN — webhooks silently do nothing is worse than
// refusing, but refusing the whole API for a dev convenience would
// be; the developer routes return 503 when disabled). A malformed
// URL with a token set is a configuration bug: fail closed, the API
// refuses to boot.
func NewClient(rawURL, token string, log *slog.Logger) (*Client, error) {
	if token == "" {
		log.Warn("SVIX_API_TOKEN empty — developer webhooks disabled (endpoints/events return 503)")
		return &Client{disable: true, log: log, http: &http.Client{Timeout: callTimeout}}, nil
	}
	if rawURL == "" {
		rawURL = DefaultURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid SVIX_URL %q: want http(s)://host", rawURL)
	}
	base := strings.TrimSuffix(rawURL, "/")
	return &Client{
		base:  base,
		token: token,
		http:  &http.Client{Timeout: callTimeout},
		log:   log,
	}, nil
}

// Enabled reports whether the dispatcher is live (a token was
// configured).
func (c *Client) Enabled() bool { return c != nil && !c.disable }

// disabled returns the sentinel when the client is off; nil keeps
// every method's happy path indentation flat.
func (c *Client) disabled() error {
	if c.disable {
		return ErrWebhooksDisabled
	}
	return nil
}

// do issues one management-API call. method+path are the Svix v1 REST
// surface (paths below already include the /api/v1 prefix). body, when
// non-nil, is JSON-encoded. On 2xx the response body is decoded into
// out (when out != nil); on anything else a typed *APIError comes
// back so callers can branch on NotFound/Unauthorized/Validation.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("webhooks: encode request: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return fmt.Errorf("webhooks: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("webhooks: svix unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("webhooks: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{Status: resp.StatusCode, Code: http.StatusText(resp.StatusCode)}
		var wire struct {
			Code   string          `json:"code"`
			Detail json.RawMessage `json:"detail"`
		}
		if json.Unmarshal(raw, &wire) == nil {
			if wire.Code != "" {
				apiErr.Code = wire.Code
			}
			// Svix's detail is a string on auth errors but an ARRAY
			// of {loc,msg,type} on 422s — keep it verbatim either
			// way so callers see the real reason.
			if d := strings.TrimSpace(string(wire.Detail)); d != "" && d != "null" {
				apiErr.Detail = d
			}
		}
		return apiErr
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("webhooks: decode response: %w", err)
		}
	}
	return nil
}

// EnsureApp returns the app id for the given Svix application uid,
// creating the application when it does not exist. Application
// creation is idempotent on uid server-side, but a GET-first keeps
// the common path a single read and avoids relying on 409-body
// shapes that differ across versions.
func (c *Client) EnsureApp(ctx context.Context, uid, name string) (string, error) {
	if err := c.disabled(); err != nil {
		return "", err
	}
	if uid == "" {
		return "", errors.New("webhooks: app uid required")
	}
	path := "/api/v1/app/" + url.PathEscape(uid)
	var got struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &got); err == nil {
		return got.ID, nil
	} else {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.NotFound() {
			return "", err
		}
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/app", map[string]string{"uid": uid, "name": name}, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", errors.New("webhooks: svix returned an empty app id")
	}
	return created.ID, nil
}

// EnsureEventType registers an event type name in Svix's org-wide
// catalog when it does not exist yet, and is a no-op when it does.
//
// WHY THIS EXISTS: the real Svix server refuses to create an
// endpoint whose filterTypes reference unregistered event types
// (422 "The following event types don't exist: ..." —
// validate_event_types in svix-server's endpoint crud, which the stub
// servers in the tests never enforced). The developer contract is
// just-in-time: registering an endpoint with
// filter_types:["my.custom.event"] works in one call, the same way
// the handler auto-provisions the caller's Svix app (EnsureApp).
// Message publishing never needed this — only endpoint filters do.
func (c *Client) EnsureEventType(ctx context.Context, name, description string) error {
	if err := c.disabled(); err != nil {
		return err
	}
	if name == "" {
		return errors.New("webhooks: event type name required")
	}
	path := "/api/v1/event-type/" + url.PathEscape(name)
	var got struct {
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &got); err == nil {
		return nil
	} else {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.NotFound() {
			return err
		}
	}
	body := map[string]string{"name": name, "description": description}
	var created struct {
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/event-type", body, &created); err != nil {
		return err
	}
	if created.Name == "" {
		return errors.New("webhooks: svix returned an empty event type name")
	}
	return nil
}

// EndpointSecretOut is the reply to the endpoint secret read:
// GET /api/v1/app/{app_id}/endpoint/{endpoint_id}/secret.
type EndpointSecretOut struct {
	Key string `json:"key"`
}

// CreateEndpoint registers a delivery target on the user's Svix app.
// The returned Endpoint carries Secret exactly once — hand it to the
// developer now, because neither Svix nor this API will show it again.
func (c *Client) CreateEndpoint(ctx context.Context, appUID string, in EndpointIn) (Endpoint, error) {
	var ep Endpoint
	if err := c.disabled(); err != nil {
		return ep, err
	}
	if in.URL == "" {
		return ep, errors.New("webhooks: endpoint url required")
	}
	supplied := in.Secret
	if err := c.do(ctx, http.MethodPost, "/api/v1/app/"+url.PathEscape(appUID)+"/endpoint", in, &ep); err != nil {
		return ep, err
	}
	if supplied != "" {
		// Caller brought their own key; Svix never echoes it back,
		// so carry it forward from the request.
		ep.Secret = supplied
		return ep, nil
	}
	// The create response never carries the generated secret (it is
	// stored encrypted server-side — verified against svix-server
	// v1.101.0; the "returned once at creation" contract holds only on
	// Svix's cloud). Read it immediately through the dedicated secret
	// endpoint so the handler can hand it to the developer exactly once.
	secret, err := c.GetEndpointSecret(ctx, appUID, ep.ID)
	if err != nil {
		// Fail closed: a 201 without the secret would hand the
		// developer an endpoint whose signing key they can never see.
		return ep, fmt.Errorf("webhooks: endpoint %s created but secret unreadable: %w", ep.ID, err)
	}
	ep.Secret = secret
	return ep, nil
}

// GetEndpointSecret returns the endpoint's signing key. The secret is
// readable any time with the management token (unlike the create
// response, which never carries it on self-hosted Svix), but this API
// only surfaces it once — in the endpoint-create 201 — to keep the
// developer contract "shown exactly once".
func (c *Client) GetEndpointSecret(ctx context.Context, appUID, endpointID string) (string, error) {
	if err := c.disabled(); err != nil {
		return "", err
	}
	if appUID == "" || endpointID == "" {
		return "", errors.New("webhooks: app uid and endpoint id required")
	}
	var out EndpointSecretOut
	if err := c.do(ctx, http.MethodGet, "/api/v1/app/"+url.PathEscape(appUID)+"/endpoint/"+url.PathEscape(endpointID)+"/secret", nil, &out); err != nil {
		return "", err
	}
	if out.Key == "" {
		return "", errors.New("webhooks: svix returned an empty endpoint secret")
	}
	return out.Key, nil
}

// ListEndpoints returns the developer's registered endpoints on their
// Svix app.
func (c *Client) ListEndpoints(ctx context.Context, appUID string) ([]Endpoint, error) {
	if err := c.disabled(); err != nil {
		return nil, err
	}
	var list struct {
		Data []Endpoint `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/app/"+url.PathEscape(appUID)+"/endpoint", nil, &list); err != nil {
		return nil, err
	}
	if list.Data == nil {
		list.Data = []Endpoint{}
	}
	return list.Data, nil
}

// DeleteEndpoint removes a delivery target. Idempotent: deleting an
// already-gone endpoint is a success (the desired end state holds).
func (c *Client) DeleteEndpoint(ctx context.Context, appUID, endpointID string) error {
	if err := c.disabled(); err != nil {
		return err
	}
	err := c.do(ctx, http.MethodDelete,
		"/api/v1/app/"+url.PathEscape(appUID)+"/endpoint/"+url.PathEscape(endpointID), nil, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.NotFound() {
		return nil
	}
	return err
}

// Publish sends one platform event to every endpoint on the user's
// Svix app. eventID is the idempotency key: re-publishing the same
// eventID returns the ORIGINAL message (Svix dedupes on it), so
// crash-retry is safe. Delivery itself (retries, backoff, signing)
// is Svix's job — this call only enqueues the message.
func (c *Client) Publish(ctx context.Context, appUID, eventType string, payload any, eventID string) (MessageOut, error) {
	var msg MessageOut
	if err := c.disabled(); err != nil {
		return msg, err
	}
	if eventType == "" {
		return msg, errors.New("webhooks: eventType required")
	}
	if eventID == "" {
		return msg, errors.New("webhooks: eventID (idempotency key) required")
	}
	body := map[string]any{"eventType": eventType, "payload": payload, "eventId": eventID}
	if err := c.do(ctx, http.MethodPost, "/api/v1/app/"+url.PathEscape(appUID)+"/msg", body, &msg); err != nil {
		return msg, err
	}
	return msg, nil
}

// ListAttempts returns Svix's delivery-attempt log for one message —
// the delivery log behind the developer-facing routes. This is the
// "list attempts by message" route (GET /app/{app_id}/attempt/msg/
// {msg_id}); the similar-looking /msg/{msg_id}/endpoint route lists
// attempted DESTINATIONS (a different shape, no responseStatusCode),
// and hitting it here silently returned the wrong log (triaged in CI:
// the deliveries step saw 502s because Attempt decoded an int-enum
// status into a string field).
func (c *Client) ListAttempts(ctx context.Context, appUID, msgID string) ([]Attempt, error) {
	if err := c.disabled(); err != nil {
		return nil, err
	}
	var list struct {
		Data []Attempt `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet,
		"/api/v1/app/"+url.PathEscape(appUID)+"/attempt/msg/"+url.PathEscape(msgID), nil, &list); err != nil {
		return nil, err
	}
	if list.Data == nil {
		list.Data = []Attempt{}
	}
	return list.Data, nil
}
