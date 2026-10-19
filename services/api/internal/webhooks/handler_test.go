// SPDX-License-Identifier: AGPL-3.0-or-later
package webhooks

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
)

// handlerFor builds a Handler whose user resolution is stubbed (no DB):
// every subject maps to "test-user-id", so the owned app is
// "user_test-user-id".
func handlerFor(t *testing.T, c *Client) *Handler {
	t.Helper()
	h := NewHandler(nil, c, testLog())
	h.userID = func(ctx context.Context, subject string) (string, error) {
		if subject != "test-subject" {
			t.Errorf("unexpected subject %q", subject)
		}
		return "test-user-id", nil
	}
	return h
}

// fullMux wires the handler exactly like cmd/api/main.go does, behind
// the same bypass auth the other route tests use.
func fullMux(t *testing.T, h *Handler) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/v1/webhooks/endpoints", h.Endpoints())
	mux.Handle("/v1/webhooks/endpoints/{id}", h.DeleteEndpoint())
	mux.Handle("/v1/webhooks/events", h.PublishEvent())
	mux.Handle("/v1/webhooks/deliveries", h.Deliveries())
	// The routes sit behind RequireAuth in main.go; the bypass stamps a
	// fixed subject the same way AUTH_BYPASS=1 does in smoke.
	return middleware.RequireAuth(nil, testLog(),
		middleware.AuthOptions{Bypass: true, BypassSubject: "test-subject"})(mux)
}

func doReq(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func envelopeCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not the error envelope: %v (body %q)", err, rec.Body.String())
	}
	return env.Error.Code
}

// armApp scripts the GET-first EnsureApp flow: app missing, then
// created.
func armApp(s *stubSvix) {
	s.respond["GET /api/v1/app/user_test-user-id"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}
	s.respond["POST /api/v1/app"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]string{"id": "app_123"})
	}
}

// armEventTypes scripts the just-in-time event-type registration the
// handler performs for every filter type: the named types already
// exist (GET 200), and any other name is created on demand (POST
// 201). It returns a sink recording the created names, in order.
func armEventTypes(s *stubSvix, existing ...string) *[]string {
	have := map[string]bool{}
	for _, n := range existing {
		have[n] = true
		s.respond["GET /api/v1/event-type/"+n] = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"name": n})
		}
	}
	created := []string{}
	s.respond["POST /api/v1/event-type"] = func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		created = append(created, body["name"])
		writeJSON(w, http.StatusCreated, map[string]string{"name": body["name"]})
	}
	return &created
}

func TestEndpointsDisabled(t *testing.T) {
	c, err := NewClient("", "", testLog()) // no token -> disabled
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	h := fullMux(t, handlerFor(t, c))
	for _, tc := range []struct{ method, target, body string }{
		{"POST", "/v1/webhooks/endpoints", `{"url":"https://example.com/wh"}`},
		{"GET", "/v1/webhooks/endpoints", ""},
		{"POST", "/v1/webhooks/events", `{"event_type":"x.y","payload":{}}`},
		{"GET", "/v1/webhooks/deliveries?message_id=m1", ""},
	} {
		rec := doReq(t, h, tc.method, tc.target, tc.body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: got %d, want 503", tc.method, tc.target, rec.Code)
		}
		if got := envelopeCode(t, rec); got != "webhooks_unavailable" {
			t.Errorf("%s %s: envelope code %q, want webhooks_unavailable", tc.method, tc.target, got)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s %s: missing Retry-After on the 503", tc.method, tc.target)
		}
	}
}

func TestCreateEndpoint(t *testing.T) {
	s := newStub(t)
	armApp(s)
	armEventTypes(s, "order.created") // filter type already registered
	var gotBody []byte
	s.respond["POST /api/v1/app/user_test-user-id/endpoint"] = func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		// The create response carries no secret on self-hosted Svix;
		// the handler reads it back through the dedicated endpoint.
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": "ep_1", "url": "https://example.com/wh",
			"description": "smoke", "filterTypes": []string{"order.created"},
		})
	}
	s.respond["GET /api/v1/app/user_test-user-id/endpoint/ep_1/secret"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"key": "whsec_only-once"})
	}
	h := fullMux(t, handlerFor(t, clientFor(t, s)))

	rec := doReq(t, h, "POST", "/v1/webhooks/endpoints",
		`{"url":"https://example.com/wh","description":"smoke","filter_types":["order.created"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d (%s), want 201", rec.Code, rec.Body.String())
	}
	var ep Endpoint
	if err := json.Unmarshal(rec.Body.Bytes(), &ep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ep.ID != "ep_1" || ep.Secret != "whsec_only-once" {
		t.Errorf("unexpected endpoint: %+v", ep)
	}
	// The wire body must use Svix's v1 field name filterTypes.
	var seen map[string]any
	if err := json.Unmarshal(gotBody, &seen); err != nil {
		t.Fatalf("seen body: %v", err)
	}
	ft, _ := seen["filterTypes"].([]any)
	if len(ft) != 1 || ft[0] != "order.created" {
		t.Errorf("filterTypes not forwarded as the v1 field name: %v", seen)
	}
}

func TestCreateEndpointRegistersMissingEventTypes(t *testing.T) {
	s := newStub(t)
	armApp(s)
	// "brand.new" is NOT pre-registered: GET 404s (stub default), so
	// the handler must POST it before creating the endpoint.
	created := armEventTypes(s, "order.created")
	s.respond["POST /api/v1/app/user_test-user-id/endpoint"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": "ep_2", "url": "https://example.com/wh",
		})
	}
	s.respond["GET /api/v1/app/user_test-user-id/endpoint/ep_2/secret"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"key": "whsec_x"})
	}
	h := fullMux(t, handlerFor(t, clientFor(t, s)))

	rec := doReq(t, h, "POST", "/v1/webhooks/endpoints",
		`{"url":"https://example.com/wh","filter_types":["order.created","brand.new"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d (%s), want 201", rec.Code, rec.Body.String())
	}
	// Only the missing type was registered; the existing one was a
	// GET-hit no-op.
	if len(*created) != 1 || (*created)[0] != "brand.new" {
		t.Errorf("registered = %v, want [brand.new]", *created)
	}
}

func TestCreateEndpointEventTypeFailureSurfaces(t *testing.T) {
	s := newStub(t)
	armApp(s)
	// The event-type catalog is down: GET 404 then POST 500.
	s.respond["POST /api/v1/event-type"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	s.respond["POST /api/v1/app/user_test-user-id/endpoint"] = func(w http.ResponseWriter, r *http.Request) {
		t.Error("endpoint must not be created when event-type registration fails")
		w.WriteHeader(http.StatusCreated)
	}
	h := fullMux(t, handlerFor(t, clientFor(t, s)))

	rec := doReq(t, h, "POST", "/v1/webhooks/endpoints",
		`{"url":"https://example.com/wh","filter_types":["boom.type"]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d (%s), want 502", rec.Code, rec.Body.String())
	}
	if got := envelopeCode(t, rec); got != "webhooks_upstream" {
		t.Errorf("envelope code %q, want webhooks_upstream", got)
	}
}

func TestCreateEndpointRouteValidation(t *testing.T) {
	s := newStub(t)
	armApp(s)
	armEventTypes(s) // validation rejects before any Svix call here
	h := fullMux(t, handlerFor(t, clientFor(t, s)))

	for _, tc := range []struct {
		name, body, wantCode string
	}{
		{"ftp scheme", `{"url":"ftp://example.com/wh"}`, "invalid_endpoint_url"},
		{"no host", `{"url":"https://"}`, "invalid_endpoint_url"},
		{"empty url", `{"url":""}`, "invalid_endpoint_url"},
		{"bad filter type", `{"url":"https://example.com/wh","filter_types":["no spaces!"]}`, "invalid_event_type"},
		{"unknown field", `{"url":"https://example.com/wh","bogus":1}`, "invalid_request_body"},
		{"not json", `{"url":`, "invalid_request_body"},
	} {
		rec := doReq(t, h, "POST", "/v1/webhooks/endpoints", tc.body)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: got %d, want 400/422", tc.name, rec.Code)
		}
		if got := envelopeCode(t, rec); got != tc.wantCode {
			t.Errorf("%s: envelope code %q, want %q", tc.name, got, tc.wantCode)
		}
	}
}

func TestListAndDeleteEndpoints(t *testing.T) {
	s := newStub(t)
	armApp(s)
	s.respond["GET /api/v1/app/user_test-user-id/endpoint"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{
			map[string]any{"id": "ep_1", "url": "https://example.com/wh"},
		}})
	}
	s.respond["DELETE /api/v1/app/user_test-user-id/endpoint/ep_1"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}
	s.respond["DELETE /api/v1/app/user_test-user-id/endpoint/gone"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}
	h := fullMux(t, handlerFor(t, clientFor(t, s)))

	rec := doReq(t, h, "GET", "/v1/webhooks/endpoints", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: got %d (%s)", rec.Code, rec.Body.String())
	}
	var list struct {
		Endpoints []Endpoint `json:"endpoints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(list.Endpoints) != 1 || list.Endpoints[0].ID != "ep_1" {
		t.Errorf("unexpected list: %+v", list.Endpoints)
	}
	if list.Endpoints[0].Secret != "" {
		t.Errorf("list must never return the endpoint secret")
	}

	// DELETE is served by a separate handler (path id) — rebuild the
	// chain around it.
	h2 := NewHandler(nil, clientFor(t, s), testLog())
	h2.userID = func(ctx context.Context, subject string) (string, error) { return "test-user-id", nil }
	del := middleware.RequireAuth(nil, testLog(),
		middleware.AuthOptions{Bypass: true, BypassSubject: "test-subject"})(h2.DeleteEndpoint())

	for _, tc := range []struct{ target string }{
		{"/v1/webhooks/endpoints/ep_1"},
		{"/v1/webhooks/endpoints/gone"}, // idempotent: 404 from Svix -> 204
	} {
		req := httptest.NewRequest(http.MethodDelete, tc.target, nil)
		req.SetPathValue("id", tc.target[len("/v1/webhooks/endpoints/"):])
		rec := httptest.NewRecorder()
		del.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Errorf("DELETE %s: got %d (%s), want 204", tc.target, rec.Code, rec.Body.String())
		}
	}
}

func TestPublishEvent(t *testing.T) {
	s := newStub(t)
	armApp(s)
	var gotBody []byte
	s.respond["POST /api/v1/app/user_test-user-id/msg"] = func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		writeJSON(w, http.StatusCreated, map[string]string{"id": "msg_1", "eventType": "custom.ping"})
	}
	c := clientFor(t, s)
	h2 := NewHandler(nil, c, testLog())
	h2.userID = func(ctx context.Context, subject string) (string, error) { return "test-user-id", nil }
	pub := middleware.RequireAuth(nil, testLog(),
		middleware.AuthOptions{Bypass: true, BypassSubject: "test-subject"})(h2.PublishEvent())

	rec := doReq(t, pub, "POST", "/v1/webhooks/events",
		`{"event_type":"custom.ping","payload":{"hello":"world"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d (%s), want 202", rec.Code, rec.Body.String())
	}
	var out struct {
		ID        string `json:"id"`
		EventType string `json:"event_type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ID != "msg_1" || out.EventType != "custom.ping" {
		t.Errorf("unexpected publish reply: %+v", out)
	}
	// event_id absent -> the server mints the idempotency key.
	var seen map[string]any
	if err := json.Unmarshal(gotBody, &seen); err != nil {
		t.Fatalf("seen body: %v", err)
	}
	if eid, _ := seen["eventId"].(string); eid == "" {
		t.Errorf("no eventId minted for the publish: %v", seen)
	}
	// Supplied event_id is passed through (caller's idempotency key).
	rec = doReq(t, pub, "POST", "/v1/webhooks/events",
		`{"event_type":"custom.ping","payload":{},"event_id":"my-key-1"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("explicit event_id: got %d (%s)", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct{ name, body, wantCode string }{
		{"bad event type", `{"event_type":"has space","payload":{}}`, "invalid_event_type"},
		{"missing payload", `{"event_type":"custom.ping"}`, "event_payload_required"},
	} {
		rec := doReq(t, pub, "POST", "/v1/webhooks/events", tc.body)
		if got := envelopeCode(t, rec); got != tc.wantCode {
			t.Errorf("%s: envelope code %q, want %q", tc.name, got, tc.wantCode)
		}
	}
}

func TestDeliveries(t *testing.T) {
	s := newStub(t)
	armApp(s)
	s.respond["GET /api/v1/app/user_test-user-id/attempt/msg/msg_1"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{
			// Real Svix wire shape: status is an integer enum.
			map[string]any{"id": "att_1", "status": 0, "responseStatusCode": 200, "timestamp": "2026-10-19T00:00:00Z"},
		}})
	}
	h2 := NewHandler(nil, clientFor(t, s), testLog())
	h2.userID = func(ctx context.Context, subject string) (string, error) { return "test-user-id", nil }
	dlv := middleware.RequireAuth(nil, testLog(),
		middleware.AuthOptions{Bypass: true, BypassSubject: "test-subject"})(h2.Deliveries())

	rec := doReq(t, dlv, "GET", "/v1/webhooks/deliveries", "")
	if rec.Code != http.StatusBadRequest || envelopeCode(t, rec) != "invalid_request_body" {
		t.Errorf("missing message_id: got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = doReq(t, dlv, "GET", "/v1/webhooks/deliveries?message_id=msg_1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var out struct {
		MessageID string    `json:"message_id"`
		Attempts  []Attempt `json:"attempts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.MessageID != "msg_1" || len(out.Attempts) != 1 || out.Attempts[0].Status != MessageStatusSuccess {
		t.Errorf("unexpected delivery log: %+v", out)
	}
	// The developer-facing wire carries the string name.
	if !strings.Contains(rec.Body.String(), `"status":"success"`) {
		t.Errorf("developer JSON lacks the string status: %s", rec.Body.String())
	}
}

func TestSvixUnauthorizedBecomesMisconfigured(t *testing.T) {
	s := newStub(t)
	// Every call 401s: our management token is bad.
	s.respond["GET /api/v1/app/user_test-user-id"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}
	h := fullMux(t, handlerFor(t, clientFor(t, s)))
	rec := doReq(t, h, "GET", "/v1/webhooks/endpoints", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", rec.Code)
	}
	if got := envelopeCode(t, rec); got != "webhooks_misconfigured" {
		t.Errorf("envelope code %q, want webhooks_misconfigured", got)
	}
	if strings.Contains(rec.Body.String(), "test-token") {
		t.Errorf("the management token leaked into the response body")
	}
}

func TestEmitBestEffort(t *testing.T) {
	// Nil and disabled clients are silent no-ops — hook sites call this
	// unconditionally, so it must never panic or block.
	EmitBestEffort(context.Background(), nil, "u1", "order.created", map[string]any{}, "e1", testLog())
	disabled, _ := NewClient("", "", testLog())
	EmitBestEffort(context.Background(), disabled, "u1", "order.created", map[string]any{}, "e1", testLog())

	// Live client: ensure-app + publish happen in a detached goroutine.
	s := newStub(t)
	s.respond["GET /api/v1/app/user_u1"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"id": "app_u1"})
	}
	published := make(chan []byte, 1)
	s.respond["POST /api/v1/app/user_u1/msg"] = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case published <- body:
		default:
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": "msg_9", "eventType": "order.created"})
	}
	EmitBestEffort(context.Background(), clientFor(t, s), "u1", "order.created",
		map[string]any{"order_id": "o1"}, "evt-1", testLog())
	var seen map[string]any
	select {
	case body := <-published:
		if err := json.Unmarshal(body, &seen); err != nil {
			t.Fatalf("seen: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EmitBestEffort never published")
	}
	if seen["eventId"] != "evt-1" || seen["eventType"] != "order.created" {
		t.Errorf("unexpected publish body: %v", seen)
	}
}

func TestEventTypeGrammar(t *testing.T) {
	for _, good := range []string{"order.created", "live:started", "custom.ping", "a", "x.y_z-1:2"} {
		if !validEventType(good) {
			t.Errorf("validEventType(%q) = false", good)
		}
	}
	for _, bad := range []string{"", "has space", ".leading", strings.Repeat("a", 130), "semi;colon"} {
		if validEventType(bad) {
			t.Errorf("validEventType(%q) = true", bad)
		}
	}
}

func TestEndpointURLValidation(t *testing.T) {
	for _, good := range []string{"https://example.com/wh", "http://localhost:8080/h", "http://api:8080/healthz"} {
		if !validEndpointURL(good) {
			t.Errorf("validEndpointURL(%q) = false", good)
		}
	}
	for _, bad := range []string{"", "ftp://x/y", "https://", "//example.com/x", "not a url"} {
		if validEndpointURL(bad) {
			t.Errorf("validEndpointURL(%q) = true", bad)
		}
	}
}
