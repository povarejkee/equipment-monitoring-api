package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
// success, creates a new session and returns its token. ok is false for
// wrong credentials; err is non-nil only for an actual infrastructure
// failure (DB unreachable) — callers must tell these apart, since "wrong
// password" is a 401 but "database is down" should never look like one.
func (a *AuthManager) Login(email, password string) (user *User, token string, ok bool, err error) {
	ctx := context.Background()
	email = strings.ToLower(strings.TrimSpace(email))

	var u User
	var passwordHash *string
	row := a.pool.QueryRow(ctx,
		`SELECT id, name, email, role, assigned_machines, password_hash FROM users WHERE email=$1`, email)
	if scanErr := row.Scan(&u.ID, &u.Name, &u.Email, &u.Role, &u.AssignedMachines, &passwordHash); scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return nil, "", false, nil
		}
		return nil, "", false, fmt.Errorf("login: query user: %w", scanErr)
	}
	if passwordHash == nil || bcrypt.CompareHashAndPassword([]byte(*passwordHash), []byte(password)) != nil {
		return nil, "", false, nil
	}

	tok, sessErr := a.createSession(ctx, u.ID)
	if sessErr != nil {
		return nil, "", false, fmt.Errorf("login: create session: %w", sessErr)
	}
	return &u, tok, true, nil
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

// UserForToken resolves the user behind a bearer token. A nil user with a
// nil error means "no such session" (401); a non-nil error means the DB
// couldn't be reached at all and the caller must not treat that as an
// invalid token (502) — the frontend logs a user out on 401, so
// mislabeling a DB blip as "unauthorized" would silently sign out every
// active session on the next blip.
func (a *AuthManager) UserForToken(token string) (*User, error) {
	if token == "" {
		return nil, nil
	}
	ctx := context.Background()
	var u User
	var expiresAt time.Time
	row := a.pool.QueryRow(ctx, `SELECT u.id, u.name, u.email, u.role, u.assigned_machines, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash=$1`, hashToken(token))
	if err := row.Scan(&u.ID, &u.Name, &u.Email, &u.Role, &u.AssignedMachines, &expiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve session: %w", err)
	}
	if time.Now().After(expiresAt) {
		a.Logout(token)
		return nil, nil
	}
	return &u, nil
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

// PruneExpiredSessions deletes sessions past their expiry. UserForToken
// already evicts a session the moment it's looked up after expiring, but a
// session nobody ever looks up again (e.g. an abandoned tab) would
// otherwise sit in the table forever — this is the periodic sweep for
// those. Intended to run on a schedule (see main.go), not per-request.
func (a *AuthManager) PruneExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := a.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
	if err != nil {
		return 0, fmt.Errorf("prune expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
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
