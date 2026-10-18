// SPDX-License-Identifier: AGPL-3.0-or-later
// Package aiproxy forwards /v1/ai/* to the Python AI service (E1).
//
// The AI service is never reached directly by user clients: this proxy
// runs behind the API's RequireAuth (so the JWT subject is always known),
// strips the /v1/ai prefix, and re-issues the request to the AI service
// with the shared internal Bearer token plus X-Sutra-Subject. SSE streams
// (the AG-UI endpoint) pass through unbuffered.
package aiproxy

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
)

// proxyTimeout bounds one proxied request. SSE streams (E2 chat) hold the
// connection open; the timeout covers dial/TLS/response-header, not the
// whole stream — ReverseProxy's streaming path keeps flowing after headers.
const proxyTimeout = 30 * time.Second

// Proxy is a reverse proxy to the Python AI service.
type Proxy struct {
	target *url.URL
	token  string
	rp     *httputil.ReverseProxy
	log    *slog.Logger
}

// NewProxy builds the proxy. A non-empty token with an unparseable base
// URL is a configuration bug — it returns an error so the API refuses to
// boot instead of 503ing every AI request. An empty token is the
// legitimate "AI disabled" state: NewProxy succeeds and Handler answers
// 503 (envelope code service_unavailable, message "ai service not
// configured").
func NewProxy(baseURL, token string, log *slog.Logger) (*Proxy, error) {
	if token != "" {
		target, err := url.Parse(baseURL)
		if err != nil || target.Scheme == "" || target.Host == "" {
			return nil, &url.Error{Op: "parse", URL: baseURL, Err: err}
		}
		p := &Proxy{target: target, token: token, log: log}
		p.rp = &httputil.ReverseProxy{
			Director: p.director,
			// Flush immediately: the AG-UI endpoint streams SSE and
			// clients must see events as they arrive, not buffered.
			FlushInterval: -1,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
			},
			ErrorHandler: p.onError,
		}
		return p, nil
	}
	return &Proxy{token: "", log: log}, nil
}

// Enabled reports whether AI proxying is configured (token present).
func (p *Proxy) Enabled() bool { return p.token != "" }

// director rewrites the incoming /v1/ai/* request for the AI service.
func (p *Proxy) director(req *http.Request) {
	// Strip the /v1/ai prefix: "/v1/ai/healthz" -> "/healthz",
	// bare "/v1/ai" -> "/".
	path := strings.TrimPrefix(req.URL.Path, "/v1/ai")
	if path == "" {
		path = "/"
	}
	req.URL.Scheme = p.target.Scheme
	req.URL.Host = p.target.Host
	req.URL.Path = singleJoin(p.target.Path, path)
	req.Host = p.target.Host
	req.Header.Set("Authorization", "Bearer "+p.token)
	// The AI service keys per-user state (E2 conversation memory, token
	// budgets) on this; it never sees the user's JWT.
	if sub := middleware.SubjectFromContext(req.Context()); sub != "" {
		req.Header.Set("X-Sutra-Subject", sub)
	}
	// Don't leak the hop: the proxy is one logical service.
	req.Header.Del("X-Forwarded-For")
}

// onError turns transport failures into the standard envelope so AI
// outages look like every other downstream failure: 503 with envelope
// code service_unavailable and message "ai service unavailable", plus a
// Retry-After hint.
func (p *Proxy) onError(w http.ResponseWriter, r *http.Request, err error) {
	p.log.Warn("ai proxy: upstream unreachable",
		"err", err, "request_id", middleware.FromContext(r.Context()))
	w.Header().Set("Retry-After", "5")
	middleware.WriteStatusError(w, r, http.StatusServiceUnavailable, "ai service unavailable")
}

// Handler serves /v1/ai and /v1/ai/*.
func (p *Proxy) Handler() http.Handler {
	if !p.Enabled() {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			middleware.WriteStatusError(w, r, http.StatusServiceUnavailable,
				"ai service not configured")
		})
	}
	return p.rp
}

// singleJoin joins a base path and a request path with exactly one slash.
func singleJoin(base, path string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
}
