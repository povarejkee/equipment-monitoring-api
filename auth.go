package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// tokenTTL is how long a session stays valid after login.
const tokenTTL = 24 * time.Hour

// AuthManager handles credential verification and session lifecycle.
// Sessions live in the DB (see internal/db/migrations/0002_auth.sql) so
// they survive restarts and are shared if the API ever runs with more
// than one instance; only a sha256 hash of the token is stored.
type AuthManager struct {
	pool *pgxpool.Pool
}

func NewAuthManager(pool *pgxpool.Pool) *AuthManager {
	return &AuthManager{pool: pool}
}

// Login verifies email/password against the stored bcrypt hash and, on
// success, creates a new session and returns its token.
func (a *AuthManager) Login(email, password string) (*User, string, bool) {
	ctx := context.Background()
	email = strings.ToLower(strings.TrimSpace(email))

	var u User
	var passwordHash *string
	row := a.pool.QueryRow(ctx,
		`SELECT id, name, email, role, assigned_machines, password_hash FROM users WHERE email=$1`, email)
	if err := row.Scan(&u.ID, &u.Name, &u.Email, &u.Role, &u.AssignedMachines, &passwordHash); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logger.Error("login: query user", "error", err)
		}
		return nil, "", false
	}
	if passwordHash == nil || bcrypt.CompareHashAndPassword([]byte(*passwordHash), []byte(password)) != nil {
		return nil, "", false
	}

	token, err := a.createSession(ctx, u.ID)
	if err != nil {
		logger.Error("login: create session", "error", err, "userId", u.ID)
		return nil, "", false
	}
	return &u, token, true
}

func (a *AuthManager) createSession(ctx context.Context, userID string) (string, error) {
	token := newToken()
	_, err := a.pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1,$2,$3)`,
		hashToken(token), userID, time.Now().Add(tokenTTL))
	if err != nil {
		return "", err
	}
	return token, nil
}

// UserForToken resolves the user behind a bearer token, if any. Expired
// sessions are treated as absent and evicted.
func (a *AuthManager) UserForToken(token string) *User {
	if token == "" {
		return nil
	}
	ctx := context.Background()
	var u User
	var expiresAt time.Time
	row := a.pool.QueryRow(ctx, `SELECT u.id, u.name, u.email, u.role, u.assigned_machines, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash=$1`, hashToken(token))
	if err := row.Scan(&u.ID, &u.Name, &u.Email, &u.Role, &u.AssignedMachines, &expiresAt); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logger.Error("resolve session", "error", err)
		}
		return nil
	}
	if time.Now().After(expiresAt) {
		a.Logout(token)
		return nil
	}
	return &u
}

// Logout invalidates a session. Reports whether a session was found.
func (a *AuthManager) Logout(token string) bool {
	tag, err := a.pool.Exec(context.Background(), `DELETE FROM sessions WHERE token_hash=$1`, hashToken(token))
	if err != nil {
		logger.Error("logout", "error", err)
		return false
	}
	return tag.RowsAffected() > 0
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	// WebSocket clients pass the token as a query param since custom
	// headers aren't available to the browser WebSocket API.
	return r.URL.Query().Get("token")
}
