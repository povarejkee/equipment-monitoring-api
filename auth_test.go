package main

import (
	"context"
	"testing"
	"time"

	appdb "equipment-monitoring-api/internal/db"
)

func TestLogin_ValidCredentials(t *testing.T) {
	a := NewAuthManager(requireTestDB(t))
	user, token, ok, err := a.Login("admin@demo.com", "demo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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
	if _, _, ok, err := a.Login("  ADMIN@Demo.com  ", "demo"); err != nil || !ok {
		t.Fatalf("expected login to succeed with mixed-case/padded email, ok=%v err=%v", ok, err)
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	a := NewAuthManager(requireTestDB(t))
	_, _, ok, err := a.Login("admin@demo.com", "wrong")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected login to fail with wrong password")
	}
}

func TestLogin_UnknownEmail(t *testing.T) {
	a := NewAuthManager(requireTestDB(t))
	_, _, ok, err := a.Login("nobody@demo.com", "demo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected login to fail for unknown email")
	}
}

func TestUserForToken_Valid(t *testing.T) {
	a := NewAuthManager(requireTestDB(t))
	_, token, _, _ := a.Login("operator@demo.com", "demo")
	user, err := a.UserForToken(token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user == nil || user.Email != "operator@demo.com" {
		t.Fatalf("expected to resolve operator, got %+v", user)
	}
}

func TestUserForToken_UnknownToken(t *testing.T) {
	a := NewAuthManager(requireTestDB(t))
	user, err := a.UserForToken("does-not-exist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user != nil {
		t.Fatal("expected nil user for unknown token")
	}
}

// TestUserForToken_DBFailureIsNotUnauthorized guards against a specific
// regression: if a DB error is ever swallowed and treated the same as "no
// such session", every active user would appear logged-out (401) on any
// transient DB blip. The frontend logs a user out on 401, so this would
// silently sign everyone out whenever the database hiccups. Force a real
// DB-level error (not pgx.ErrNoRows) via a pool closed out from under the
// query, and assert it comes back as a non-nil error, not a nil user.
func TestUserForToken_DBFailureIsNotUnauthorized(t *testing.T) {
	testDBURL := requireTestDBURL(t)
	deadPool, err := appdb.Connect(context.Background(), testDBURL)
	if err != nil {
		t.Fatalf("connect throwaway pool: %v", err)
	}
	a := NewAuthManager(deadPool)
	deadPool.Close() // every subsequent query now fails with a real error

	user, err := a.UserForToken("anything")
	if err == nil {
		t.Fatal("expected a DB error, got nil — a closed pool must not look like an unknown token")
	}
	if user != nil {
		t.Fatal("expected nil user alongside the error")
	}
}

func TestUserForToken_Expired(t *testing.T) {
	pool := requireTestDB(t)
	a := NewAuthManager(pool)
	_, token, _, _ := a.Login("manager@demo.com", "demo")

	// Backdate the session past its TTL directly in the DB (white-box: same
	// package as auth.go, so hashToken/the sessions table are fair game).
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE sessions SET expires_at=$1 WHERE token_hash=$2`,
		time.Now().Add(-time.Second), hashToken(token)); err != nil {
		t.Fatalf("backdate session: %v", err)
	}

	user, err := a.UserForToken(token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user != nil {
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
	_, token, _, _ := a.Login("admin@demo.com", "demo")

	if !a.Logout(token) {
		t.Fatal("expected logout to report the session was found")
	}
	user, err := a.UserForToken(token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user != nil {
		t.Fatal("expected token to be invalid after logout")
	}
	if a.Logout(token) {
		t.Error("expected logging out an already-invalidated token to report not found")
	}
}
