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
// the request context. A DB failure while resolving the session is a 502,
// never a 401 — see AuthManager.UserForToken.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(bearerToken(r))
		user, err := s.auth.UserForToken(token)
		if err != nil {
			writeDBError(w, "resolve session", err)
			return
		}
		if user == nil {
			writeError(w, http.StatusUnauthorized, "Не авторизован")
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// requireRole restricts access to a single role. Must run after
// authMiddleware, which puts the user in context.
func requireRole(role UserRole, next http.HandlerFunc) http.HandlerFunc {
	return requireRoles([]UserRole{role}, next)
}

// requireRoles restricts access to any of the given roles.
func requireRoles(roles []UserRole, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := userFromContext(r)
		if user == nil {
			writeError(w, http.StatusForbidden, "Недостаточно прав")
			return
		}
		for _, role := range roles {
			if user.Role == role {
				next.ServeHTTP(w, r)
				return
			}
		}
		writeError(w, http.StatusForbidden, "Недостаточно прав")
	}
}

// canAccessMachine implements the access rule from the spec: "оператор
// видит свой станок, руководитель — всё". Only operators are scoped; any
// other role (manager, admin) sees everything. An operator with an empty
// AssignedMachines list sees nothing — fail closed, not open. This mirrors
// (and actually enforces, server-side) AuthService.canAccessMachine on the
// frontend, which existed but was never wired into a guard and had no
// effect since the frontend fetched the unfiltered list anyway.
func canAccessMachine(user *User, machineID string) bool {
	if user == nil {
		return false
	}
	if user.Role != RoleOperator {
		return true
	}
	for _, id := range user.AssignedMachines {
		if id == machineID {
			return true
		}
	}
	return false
}

// scopedMachines filters a machine list down to what the caller is allowed
// to see (see canAccessMachine).
func scopedMachines(user *User, machines []*Machine) []*Machine {
	if user == nil || user.Role != RoleOperator {
		return machines
	}
	out := make([]*Machine, 0, len(machines))
	for _, m := range machines {
		if canAccessMachine(user, m.ID) {
			out = append(out, m)
		}
	}
	return out
}
