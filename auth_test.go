package main

import (
	"context"
	"testing"
	"time"
)

func TestLogin_ValidCredentials(t *testing.T) {
	a := NewAuthManager(requireTestDB(t))
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
	a := NewAuthManager(requireTestDB(t))
	if _, _, ok := a.Login("  ADMIN@Demo.com  ", "demo"); !ok {
		t.Fatal("expected login to succeed with mixed-case/padded email")
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	a := NewAuthManager(requireTestDB(t))
	if _, _, ok := a.Login("admin@demo.com", "wrong"); ok {
		t.Fatal("expected login to fail with wrong password")
	}
}

func TestLogin_UnknownEmail(t *testing.T) {
	a := NewAuthManager(requireTestDB(t))
	if _, _, ok := a.Login("nobody@demo.com", "demo"); ok {
		t.Fatal("expected login to fail for unknown email")
	}
}

func TestUserForToken_Valid(t *testing.T) {
	a := NewAuthManager(requireTestDB(t))
	_, token, _ := a.Login("operator@demo.com", "demo")
	user := a.UserForToken(token)
	if user == nil || user.Email != "operator@demo.com" {
		t.Fatalf("expected to resolve operator, got %+v", user)
	}
}

func TestUserForToken_UnknownToken(t *testing.T) {
	a := NewAuthManager(requireTestDB(t))
	if a.UserForToken("does-not-exist") != nil {
		t.Fatal("expected nil user for unknown token")
	}
}

func TestUserForToken_Expired(t *testing.T) {
	pool := requireTestDB(t)
	a := NewAuthManager(pool)
	_, token, _ := a.Login("manager@demo.com", "demo")

	// Backdate the session past its TTL directly in the DB (white-box: same
	// package as auth.go, so hashToken/the sessions table are fair game).
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE sessions SET expires_at=$1 WHERE token_hash=$2`,
		time.Now().Add(-time.Second), hashToken(token)); err != nil {
		t.Fatalf("backdate session: %v", err)
	}

	if a.UserForToken(token) != nil {
		t.Fatal("expected expired token to resolve to no user")
	}

	// Expired sessions are evicted on lookup, not just rejected.
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE token_hash=$1`, hashToken(token)).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Error("expected expired session to be evicted from the sessions table")
	}
}

func TestLogout(t *testing.T) {
	a := NewAuthManager(requireTestDB(t))
	_, token, _ := a.Login("admin@demo.com", "demo")

	if !a.Logout(token) {
		t.Fatal("expected logout to report the session was found")
	}
	if a.UserForToken(token) != nil {
		t.Fatal("expected token to be invalid after logout")
	}
	if a.Logout(token) {
		t.Error("expected logging out an already-invalidated token to report not found")
	}
}
