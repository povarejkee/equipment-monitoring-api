package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIP_TrustsLastForwardedForEntry(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	// First entry is whatever the client sent — attacker-controlled. Last
	// entry is what Render's own edge proxy (the only hop in front of this
	// service) actually observed.
	req.Header.Set("X-Forwarded-For", "9.9.9.9, 203.0.113.5")
	if got := clientIP(req); got != "203.0.113.5" {
		t.Errorf("clientIP() = %q, want the last (proxy-observed) entry %q", got, "203.0.113.5")
	}
}

func TestClientIP_FallsBackToRemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.RemoteAddr = "198.51.100.7:54321"
	if got := clientIP(req); got != "198.51.100.7" {
		t.Errorf("clientIP() = %q, want %q", got, "198.51.100.7")
	}
}

// TestRateLimiter_SpoofedFirstHopDoesNotBypassLimit is the regression test
// for the actual vulnerability: previously clientIP() trusted the FIRST
// X-Forwarded-For entry, which a client controls completely — sending a
// fresh fake value on every request got a fresh rate-limit bucket every
// time, making the login rate limit a no-op.
func TestRateLimiter_SpoofedFirstHopDoesNotBypassLimit(t *testing.T) {
	l := newIPRateLimiter(2, time.Minute)
	realIP := "203.0.113.5"

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("spoofed-%d, %s", i, realIP))
		if !l.Allow(clientIP(req)) {
			t.Fatalf("request %d from the real IP was unexpectedly blocked", i)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.Header.Set("X-Forwarded-For", fmt.Sprintf("spoofed-99, %s", realIP))
	if l.Allow(clientIP(req)) {
		t.Fatal("expected the 3rd request from the same real IP to be blocked despite a new spoofed first hop each time")
	}
}

func TestIPRateLimiter_AllowsUpToLimitThenBlocks(t *testing.T) {
	l := newIPRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("4th request should be blocked")
	}
	// A different IP has its own independent bucket.
	if !l.Allow("5.6.7.8") {
		t.Fatal("a different IP should not share the exhausted bucket")
	}
}

func TestIPRateLimiter_Sweep(t *testing.T) {
	l := newIPRateLimiter(5, 50*time.Millisecond)
	l.Allow("1.2.3.4")
	l.Allow("5.6.7.8")

	time.Sleep(60 * time.Millisecond) // let both entries age out of the window
	l.sweep()

	l.mu.Lock()
	n := len(l.hits)
	l.mu.Unlock()
	if n != 0 {
		t.Errorf("expected sweep to remove all aged-out entries, %d remain", n)
	}
}
