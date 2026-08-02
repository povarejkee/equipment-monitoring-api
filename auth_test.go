package main

import (
	"testing"
	"time"
)

func TestLogin_ValidCredentials(t *testing.T) {
	a := NewAuthManager(NewStore(requireTestDB(t)))
	user, token, ok := a.Login("admin@demo.com", "demo")
	if !ok {
		t.Fatal("expected login to succeed")
	}
	if token == "" {
		t.Error("expected non-empty token")
	}
	if user.Email != "admin@demo.com" {
		t.Errorf("got user %q, want admin@demo.com", user.Email)
	}
}

func TestLogin_CaseAndWhitespaceInsensitiveEmail(t *testing.T) {
	a := NewAuthManager(NewStore(requireTestDB(t)))
	if _, _, ok := a.Login("  ADMIN@Demo.com  ", "demo"); !ok {
		t.Fatal("expected login to succeed with mixed-case/padded email")
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	a := NewAuthManager(NewStore(requireTestDB(t)))
	if _, _, ok := a.Login("admin@demo.com", "wrong"); ok {
		t.Fatal("expected login to fail with wrong password")
	}
}

func TestLogin_UnknownEmail(t *testing.T) {
	a := NewAuthManager(NewStore(requireTestDB(t)))
	if _, _, ok := a.Login("nobody@demo.com", "demo"); ok {
		t.Fatal("expected login to fail for unknown email")
	}
}

func TestUserForToken_Valid(t *testing.T) {
	a := NewAuthManager(NewStore(requireTestDB(t)))
	_, token, _ := a.Login("operator@demo.com", "demo")
	user := a.UserForToken(token)
	if user == nil || user.Email != "operator@demo.com" {
		t.Fatalf("expected to resolve operator, got %+v", user)
	}
}

func TestUserForToken_UnknownToken(t *testing.T) {
	a := NewAuthManager(NewStore(requireTestDB(t)))
	if a.UserForToken("does-not-exist") != nil {
		t.Fatal("expected nil user for unknown token")
	}
}

func TestUserForToken_Expired(t *testing.T) {
	a := NewAuthManager(NewStore(requireTestDB(t)))
	_, token, _ := a.Login("manager@demo.com", "demo")

	// Backdate the token past its TTL (white-box: same package as AuthManager).
	a.mu.Lock()
	entry := a.tokens[token]
	entry.expiresAt = time.Now().Add(-time.Second)
	a.tokens[token] = entry
	a.mu.Unlock()

	if a.UserForToken(token) != nil {
		t.Fatal("expected expired token to resolve to no user")
	}

	// Expired tokens are evicted on lookup, not just rejected.
	a.mu.RLock()
	_, stillPresent := a.tokens[token]
	a.mu.RUnlock()
	if stillPresent {
		t.Error("expected expired token to be evicted from the token map")
	}
}
