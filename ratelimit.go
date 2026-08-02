package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ipRateLimiter is a per-IP sliding-window limiter: at most `limit` requests
// within `window`. Intended for low-volume, security-sensitive endpoints
// (e.g. login) — not a general-purpose traffic shaper.
type ipRateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

func newIPRateLimiter(limit int, window time.Duration) *ipRateLimiter {
	return &ipRateLimiter{
		limit:  limit,
		window: window,
		hits:   make(map[string][]time.Time),
	}
}

// Allow reports whether a new request from ip may proceed, recording it if so.
func (l *ipRateLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-l.window)
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[ip] = kept
		return false
	}
	l.hits[ip] = append(kept, now)
	return true
}

// rateLimitMiddleware rejects requests over the limit with 429.
func rateLimitMiddleware(l *ipRateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(clientIP(r)) {
			writeError(w, http.StatusTooManyRequests, "Слишком много попыток, повторите позже")
			return
		}
		next.ServeHTTP(w, r)
	}
}

// clientIP extracts the caller's address, preferring X-Forwarded-For (set by
// Render's proxy in front of the service) over the raw connection address.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if ip := strings.TrimSpace(strings.Split(fwd, ",")[0]); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
