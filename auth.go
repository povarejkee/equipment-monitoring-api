package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
)

// credential holds the demo password for a seeded user.
// v2 keeps the same three demo accounts as v1 so the handoff build
// behaves identically; real credential storage lands with the DB in v3.
type credential struct {
	password string
	userID   string
}

type AuthManager struct {
	mu      sync.RWMutex
	creds   map[string]credential // email -> credential
	tokens  map[string]string     // token -> userID
	store   *Store
}

func NewAuthManager(store *Store) *AuthManager {
	return &AuthManager{
		creds: map[string]credential{
			"operator@demo.com": {"demo", "u1"},
			"manager@demo.com":  {"demo", "u2"},
			"admin@demo.com":    {"demo", "u3"},
		},
		tokens: make(map[string]string),
		store:  store,
	}
}

func (a *AuthManager) Login(email, password string) (*User, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cred, ok := a.creds[strings.ToLower(strings.TrimSpace(email))]
	if !ok || cred.password != password {
		return nil, "", false
	}
	var user *User
	for _, u := range a.store.Users() {
		if u.ID == cred.userID {
			user = u
			break
		}
	}
	if user == nil {
		return nil, "", false
	}
	token := newToken()
	a.tokens[token] = user.ID
	return user, token, true
}

// UserForToken resolves the user behind a bearer token, if any.
func (a *AuthManager) UserForToken(token string) *User {
	a.mu.RLock()
	userID, ok := a.tokens[token]
	a.mu.RUnlock()
	if !ok {
		return nil
	}
	for _, u := range a.store.Users() {
		if u.ID == userID {
			return u
		}
	}
	return nil
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
