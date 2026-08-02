package main

import (
	"context"
	"net/http"
	"runtime/debug"
	"strings"
)

type ctxKey string

const userCtxKey ctxKey = "user"

func userFromContext(r *http.Request) *User {
	if u, ok := r.Context().Value(userCtxKey).(*User); ok {
		return u
	}
	return nil
}

// corsMiddleware allows the configured browser origins. Auth uses bearer
// tokens (not cookies), so a wildcard origin is safe here.
func corsMiddleware(allowed []string, next http.Handler) http.Handler {
	allowAll := len(allowed) == 0 || contains(allowed, "*")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		switch {
		case allowAll:
			w.Header().Set("Access-Control-Allow-Origin", "*")
		case origin != "" && contains(allowed, origin):
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoverMiddleware catches panics from downstream handlers so one bad
// request can't take the whole server down, and logs them with a stack
// trace instead of letting net/http print to stderr and close the conn.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic recovered",
					"error", rec,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				writeError(w, http.StatusInternalServerError, "Внутренняя ошибка сервера")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// authMiddleware requires a valid bearer token and injects the user into
// the request context.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(bearerToken(r))
		user := s.auth.UserForToken(token)
		if user == nil {
			writeError(w, http.StatusUnauthorized, "Не авторизован")
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}
