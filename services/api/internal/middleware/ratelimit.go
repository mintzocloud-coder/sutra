// SPDX-License-Identifier: AGPL-3.0-or-later
// Per-IP token-bucket rate limiting for the Sutra API service.
package middleware

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// idleBucketTTL is how long a client bucket is kept without traffic before
// the sweeper drops it (bounds memory for NAT/proxy-heavy traffic).
const idleBucketTTL = 10 * time.Minute

// sweepInterval is the minimum time between sweeps of idle buckets.
const sweepInterval = time.Minute

// maxBuckets caps the bucket map; when exceeded, the sweeper evicts the
// stalest entries aggressively. This is a backstop against memory growth
// under a distributed IP-spoof flood (an outer WAF/proxy still owns
// volumetric DDoS defense).
const maxBuckets = 100_000

// ipBucket is one client's token bucket state.
type ipBucket struct {
	tokens float64
	last   time.Time
}

// RateLimiter is a concurrency-safe in-memory token bucket rate limiter,
// keyed by client IP. It is deliberately simple (no Redis): it bounds
// accidental and abusive traffic on a single node. Multi-node deployments
// get approximate per-node limits; a shared store (Valkey) can replace it
// later behind the same Allow(key) contract.
//
// The limiter is a leaky-bucket alternative chosen for burst tolerance:
// clients may burst up to `burst` requests, then settle at `rps`
// sustained. Set rps <= 0 to disable (always allow).
type RateLimiter struct {
	rps   float64
	burst float64

	mu        sync.Mutex
	buckets   map[string]*ipBucket
	lastSweep time.Time
}

// NewRateLimiter builds a limiter allowing `rps` requests/second sustained
// with bursts up to `burst`. Non-positive rps disables limiting.
func NewRateLimiter(rps float64, burst int) *RateLimiter {
	return &RateLimiter{
		rps:       rps,
		burst:     float64(burst),
		buckets:   make(map[string]*ipBucket),
		lastSweep: time.Now(),
	}
}

// Allow reports whether the client identified by key may proceed.
// On denial it returns how long the client should wait before retrying.
func (l *RateLimiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	if l.rps <= 0 {
		return true, 0 // disabled
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if now.Sub(l.lastSweep) >= sweepInterval {
		l.sweep(now)
	}

	b, found := l.buckets[key]
	if !found {
		b = &ipBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	// Refill tokens earned since the last touch, capped at the burst size.
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * l.rps
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// Not enough for one request: tell the client when a token arrives.
	wait := time.Duration((1 - b.tokens) / l.rps * float64(time.Second))
	if wait < time.Millisecond {
		wait = time.Millisecond
	}
	return false, wait
}

// sweep drops buckets idle longer than idleBucketTTL. If the map still
// exceeds maxBuckets afterwards (IP-spoof flood), it evicts the stalest
// half as an emergency backstop. Must be called with l.mu held.
func (l *RateLimiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > idleBucketTTL {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) > maxBuckets {
		// Emergency: keep only the most recently active half.
		type entry struct {
			key  string
			last time.Time
		}
		entries := make([]entry, 0, len(l.buckets))
		for k, b := range l.buckets {
			entries = append(entries, entry{k, b.last})
		}
		// Stale-first so the first half of the slice is evicted.
		sort.Slice(entries, func(i, j int) bool { return entries[i].last.Before(entries[j].last) })
		for _, e := range entries[:len(entries)/2] {
			delete(l.buckets, e.key)
		}
	}
	l.lastSweep = now
}

// clientIP extracts the client IP from the request's remote address. It
// intentionally uses the TCP peer address, NOT X-Forwarded-For: that header
// is trivially spoofable, and honoring it without a trusted-proxy config
// would let one client rotate identities at will. If the service later
// sits behind a known reverse proxy, add an explicit TRUSTED_PROXIES list
// and only then read X-Forwarded-For for in-range peers.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimit rejects requests beyond the limiter's budget with 429 JSON
// (standard error envelope) plus a Retry-After header. It also emits
// X-RateLimit-Limit/Remaining informational headers so well-behaved
// clients can self-throttle before hitting the wall.
func RateLimit(l *RateLimiter, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := clientIP(r)
			ok, wait := l.Allow(key)
			if l.rps > 0 {
				w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%.0f", l.rps))
			}
			if !ok {
				secs := int(wait.Seconds()) + 1 // ceiling, at least 1s
				w.Header().Set("Retry-After", fmt.Sprintf("%d", secs))
				log.Debug("rate limit exceeded",
					"client_ip", key,
					"method", r.Method,
					"path", r.URL.Path,
					"retry_after_s", secs,
					"request_id", FromContext(r.Context()),
				)
				WriteJSONError(w, r, http.StatusTooManyRequests,
					"rate_limited", "too many requests; slow down and retry")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
