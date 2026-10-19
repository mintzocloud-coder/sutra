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
// "user:<user-id>"), registers developer endpoints on them, and
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
	Detail string // svix "detail" field when present
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
// URL-safe.
func AppUIDForUser(userID string) string { return "user:" + userID }

// Endpoint is one developer-registered delivery target on their Svix
// application.
type Endpoint struct {
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	Description string   `json:"description,omitempty"`
	FilterTypes []string `json:"filterTypes,omitempty"`
	// Secret is the per-endpoint signing key. Svix returns it on
	// create (auto-generated when not supplied); it is never
	// returned again. Hand it to the developer once and let them
	// store it — the API does not keep a copy.
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
	// Secret, when empty, makes Svix generate a random one and
	// return it in the response. Supplying one lets developers
	// bring their own key.
	Secret string `json:"secret,omitempty"`
}

// MessageOut is the Svix server's reply to a message create.
type MessageOut struct {
	ID        string `json:"id"`
	EventType string `json:"eventType"`
}

// Attempt is one delivery attempt of a message to one endpoint, from
// Svix's own attempt log — this is the delivery log the developer-
// facing routes surface. Status is one of "success", "pending",
// "fail", "sending" (Svix wire values, passed through verbatim so
// clients can match the Svix docs).
type Attempt struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	ResponseStatus int    `json:"responseStatusCode"`
	Timestamp      string `json:"timestamp"`
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
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(raw, &wire) == nil {
			if wire.Code != "" {
				apiErr.Code = wire.Code
			}
			apiErr.Detail = wire.Detail
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
	if err := c.do(ctx, http.MethodPost, "/api/v1/app/"+url.PathEscape(appUID)+"/endpoint", in, &ep); err != nil {
		return ep, err
	}
	return ep, nil
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
// the delivery log behind the developer-facing routes. Statuses are
// Svix wire values ("success", "pending", "fail", "sending") passed
// through verbatim.
func (c *Client) ListAttempts(ctx context.Context, appUID, msgID string) ([]Attempt, error) {
	if err := c.disabled(); err != nil {
		return nil, err
	}
	var list struct {
		Data []Attempt `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet,
		"/api/v1/app/"+url.PathEscape(appUID)+"/msg/"+url.PathEscape(msgID)+"/endpoint", nil, &list); err != nil {
		return nil, err
	}
	if list.Data == nil {
		list.Data = []Attempt{}
	}
	return list.Data, nil
}
