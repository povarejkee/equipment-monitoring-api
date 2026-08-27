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
	l := &ipRateLimiter{
		limit:  limit,
		window: window,
		hits:   make(map[string][]time.Time),
	}
	go l.sweepLoop()
	return l
}

// sweepLoop periodically drops IPs with no hits left inside the window.
// Without this, the map only ever grows: every distinct IP that has ever
// made a request (real or, before the X-Forwarded-For fix below, freely
// spoofed) leaves a permanent entry behind even after its hits have aged
// out — a slow unbounded memory leak on a public endpoint.
func (l *ipRateLimiter) sweepLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		l.sweep()
	}
}

func (l *ipRateLimiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-l.window)
	for ip, hits := range l.hits {
		live := hits[:0]
		for _, t := range hits {
			if t.After(cutoff) {
				live = append(live, t)
			}
		}
		if len(live) == 0 {
			delete(l.hits, ip)
		} else {
			l.hits[ip] = live
		}
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

// clientIP extracts the caller's real address for rate-limiting purposes.
//
// X-Forwarded-For is a comma-separated list that grows by one entry per
// proxy hop, each hop appending the address it saw the request come from.
// The FIRST entry is whatever the original client put there — completely
// attacker-controlled, since nothing stops a client from sending
// `X-Forwarded-For: 1.2.3.4` (or a fresh random value on every request) and
// getting a brand new rate-limit bucket each time. The trustworthy value is
// the LAST entry: the address Render's own edge proxy (the only hop in
// front of this service) observed directly, which the client cannot forge.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.Split(fwd, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
