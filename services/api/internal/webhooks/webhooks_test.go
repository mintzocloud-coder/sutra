// SPDX-License-Identifier: AGPL-3.0-or-later
package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stubSvix is a scriptable fake of the Svix v1 management surface.
// Each handler records what it saw; the test pre-arms responses.
type stubSvix struct {
	t       *testing.T
	authHdr string
	// scripted per (method, path)
	respond map[string]func(w http.ResponseWriter, r *http.Request)
	// last request per key, for shape assertions
	seen map[string][]byte
}

func newStub(t *testing.T) *stubSvix {
	return &stubSvix{t: t, respond: map[string]func(http.ResponseWriter, *http.Request){}, seen: map[string][]byte{}}
}

func (s *stubSvix) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.authHdr = r.Header.Get("Authorization")
	key := r.Method + " " + r.URL.Path
	if fn, ok := s.respond[key]; ok {
		fn(w, r)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func clientFor(t *testing.T, s *stubSvix) *Client {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "test-token", testLog())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestDisabledNoop(t *testing.T) {
	c, err := NewClient("", "", testLog())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.Enabled() {
		t.Fatal("expected disabled client")
	}
	ctx := context.Background()
	if _, err := c.EnsureApp(ctx, "user_1", "n"); !errors.Is(err, ErrWebhooksDisabled) {
		t.Fatalf("EnsureApp: %v", err)
	}
	if _, err := c.CreateEndpoint(ctx, "user_1", EndpointIn{URL: "https://x.example"}); !errors.Is(err, ErrWebhooksDisabled) {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if _, err := c.ListEndpoints(ctx, "user_1"); !errors.Is(err, ErrWebhooksDisabled) {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if err := c.DeleteEndpoint(ctx, "user_1", "ep_1"); !errors.Is(err, ErrWebhooksDisabled) {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if _, err := c.Publish(ctx, "user_1", "order.created", map[string]string{}, "e1"); !errors.Is(err, ErrWebhooksDisabled) {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := c.ListAttempts(ctx, "user_1", "msg_1"); !errors.Is(err, ErrWebhooksDisabled) {
		t.Fatalf("ListAttempts: %v", err)
	}
}

func TestFailClosedBadURL(t *testing.T) {
	for _, bad := range []string{"://bad", "ftp://x", "http://", "not a url %zz"} {
		if _, err := NewClient(bad, "tok", testLog()); err == nil {
			t.Fatalf("expected fail-closed for %q", bad)
		}
	}
}

func TestDefaultURLWhenEmpty(t *testing.T) {
	c, err := NewClient("", "tok", testLog())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.base != DefaultURL {
		t.Fatalf("base = %q, want %q", c.base, DefaultURL)
	}
}

func TestAuthHeaderAndAppUID(t *testing.T) {
	s := newStub(t)
	s.respond["GET /api/v1/app/user_abc"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"id": "app_1"})
	}
	c := clientFor(t, s)
	id, err := c.EnsureApp(context.Background(), AppUIDForUser("abc"), "Dev")
	if err != nil {
		t.Fatalf("EnsureApp: %v", err)
	}
	if id != "app_1" {
		t.Fatalf("id = %q", id)
	}
	if s.authHdr != "Bearer test-token" {
		t.Fatalf("auth = %q", s.authHdr)
	}
}

func TestEnsureAppCreatesOn404(t *testing.T) {
	s := newStub(t)
	s.respond["GET /api/v1/app/user_new"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]string{"code": "not_found", "detail": "app not found"})
	}
	var gotBody map[string]string
	s.respond["POST /api/v1/app"] = func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, 201, map[string]string{"id": "app_2"})
	}
	c := clientFor(t, s)
	id, err := c.EnsureApp(context.Background(), "user_new", "New Dev")
	if err != nil {
		t.Fatalf("EnsureApp: %v", err)
	}
	if id != "app_2" {
		t.Fatalf("id = %q", id)
	}
	if gotBody["uid"] != "user_new" || gotBody["name"] != "New Dev" {
		t.Fatalf("create body = %v", gotBody)
	}
}

func TestEnsureAppSurfacesOtherErrors(t *testing.T) {
	s := newStub(t)
	s.respond["GET /api/v1/app/user_bad"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 401, map[string]string{"code": "authentication_failed", "detail": "bad token"})
	}
	c := clientFor(t, s)
	_, err := c.EnsureApp(context.Background(), "user_bad", "x")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.Unauthorized() {
		t.Fatalf("err = %v", err)
	}
	if apiErr.Code != "authentication_failed" || !strings.Contains(apiErr.Detail, "bad token") {
		t.Fatalf("code/detail = %q/%q", apiErr.Code, apiErr.Detail)
	}
}

func TestCreateEndpointShape(t *testing.T) {
	s := newStub(t)
	var got EndpointIn
	s.respond["POST /api/v1/app/user_abc/endpoint"] = func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, 201, Endpoint{ID: "ep_1", URL: got.URL, FilterTypes: got.FilterTypes, Secret: "whsec_gen123"})
	}
	c := clientFor(t, s)
	ep, err := c.CreateEndpoint(context.Background(), "user_abc", EndpointIn{
		URL:         "https://dev.example/hook",
		Description: "orders",
		FilterTypes: []string{"order.status_changed"},
	})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if ep.ID != "ep_1" || ep.Secret != "whsec_gen123" {
		t.Fatalf("ep = %+v", ep)
	}
	if got.URL != "https://dev.example/hook" || got.Description != "orders" ||
		len(got.FilterTypes) != 1 || got.FilterTypes[0] != "order.status_changed" {
		t.Fatalf("request shape = %+v", got)
	}
}

func TestCreateEndpointValidation(t *testing.T) {
	s := newStub(t)
	s.respond["POST /api/v1/app/user_abc/endpoint"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 422, map[string]string{"code": "validation_error", "detail": "bad url"})
	}
	c := clientFor(t, s)
	_, err := c.CreateEndpoint(context.Background(), "user_abc", EndpointIn{URL: "https://x.example"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.Validation() {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.CreateEndpoint(context.Background(), "user_abc", EndpointIn{}); err == nil {
		t.Fatal("expected error for empty URL")
	}
}

func TestDeleteEndpointIdempotent(t *testing.T) {
	s := newStub(t)
	s.respond["DELETE /api/v1/app/user_abc/endpoint/ep_gone"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]string{"code": "not_found"})
	}
	s.respond["DELETE /api/v1/app/user_abc/endpoint/ep_1"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}
	c := clientFor(t, s)
	ctx := context.Background()
	if err := c.DeleteEndpoint(ctx, "user_abc", "ep_gone"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if err := c.DeleteEndpoint(ctx, "user_abc", "ep_1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestPublishSendsEventID(t *testing.T) {
	s := newStub(t)
	var got map[string]any
	s.respond["POST /api/v1/app/user_abc/msg"] = func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, 202, map[string]string{"id": "msg_9", "eventType": "order.status_changed"})
	}
	c := clientFor(t, s)
	msg, err := c.Publish(context.Background(), "user_abc", "order.status_changed",
		map[string]any{"order_id": "o1"}, "dedupe-key-1")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if msg.ID != "msg_9" || msg.EventType != "order.status_changed" {
		t.Fatalf("msg = %+v", msg)
	}
	if got["eventType"] != "order.status_changed" || got["eventId"] != "dedupe-key-1" {
		t.Fatalf("request shape = %v", got)
	}
	if _, err := c.Publish(context.Background(), "user_abc", "", map[string]any{}, "k"); err == nil {
		t.Fatal("expected error for empty eventType")
	}
	if _, err := c.Publish(context.Background(), "user_abc", "e", map[string]any{}, ""); err == nil {
		t.Fatal("expected error for empty eventID")
	}
}

func TestListAttemptsMapping(t *testing.T) {
	s := newStub(t)
	s.respond["GET /api/v1/app/user_abc/msg/msg_9/endpoint"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"data": []map[string]any{
			{"id": "atm_1", "status": "success", "responseStatusCode": 200, "timestamp": "2026-01-01T00:00:00Z"},
			{"id": "atm_2", "status": "fail", "responseStatusCode": 500, "timestamp": "2026-01-01T00:01:00Z"},
		}})
	}
	c := clientFor(t, s)
	attempts, err := c.ListAttempts(context.Background(), "user_abc", "msg_9")
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 2 || attempts[0].Status != "success" || attempts[1].ResponseStatus != 500 {
		t.Fatalf("attempts = %+v", attempts)
	}
}

func TestListAttemptsNotFound(t *testing.T) {
	s := newStub(t)
	s.respond["GET /api/v1/app/user_abc/msg/nope/endpoint"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]string{"code": "not_found"})
	}
	c := clientFor(t, s)
	_, err := c.ListAttempts(context.Background(), "user_abc", "nope")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.NotFound() {
		t.Fatalf("err = %v", err)
	}
}

func TestServerDownIsError(t *testing.T) {
	c, err := NewClient("http://127.0.0.1:1", "tok", testLog())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.ListEndpoints(context.Background(), "user_abc")
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v", err)
	}
}
