package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	appdb "equipment-monitoring-api/internal/db"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	pool := requireTestDB(t)
	store := NewStore(pool)
	auth := NewAuthManager(pool)
	srv := NewServer(store, auth, NewHub())
	_, token, _, _ := auth.Login("admin@demo.com", "demo")
	return srv, token
}

func TestAuthMiddleware_NoToken(t *testing.T) {
	srv, _ := newTestServer(t)
	called := false
	h := srv.authMiddleware(func(w http.ResponseWriter, r *http.Request) { called = true })

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/machines", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if called {
		t.Error("expected next handler not to be called without a token")
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.authMiddleware(func(w http.ResponseWriter, r *http.Request) {})

	req := httptest.NewRequest(http.MethodGet, "/api/machines", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestAuthMiddleware_ValidToken(t *testing.T) {
	srv, token := newTestServer(t)
	var gotUser *User
	h := srv.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		gotUser = userFromContext(r)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/machines", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if gotUser == nil || gotUser.Email != "admin@demo.com" {
		t.Errorf("expected admin user in context, got %+v", gotUser)
	}
}

func TestAuthMiddleware_ValidToken_WSQueryParam(t *testing.T) {
	srv, token := newTestServer(t)
	h := srv.authMiddleware(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/ws?token="+token, nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestAuthMiddleware_DBFailureIsNotUnauthorized is the middleware-level
// counterpart to TestUserForToken_DBFailureIsNotUnauthorized: a session
// lookup that fails because the DB is unreachable must surface as 502, not
// 401, since the frontend treats 401 as "log this user out."
func TestAuthMiddleware_DBFailureIsNotUnauthorized(t *testing.T) {
	testDBURL := requireTestDBURL(t)
	deadPool, err := appdb.Connect(context.Background(), testDBURL)
	if err != nil {
		t.Fatalf("connect throwaway pool: %v", err)
	}
	deadPool.Close()

	srv := NewServer(NewStore(deadPool), NewAuthManager(deadPool), NewHub())
	h := srv.authMiddleware(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/api/machines", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d (502) for a DB failure, not 401", rec.Code, http.StatusBadGateway)
	}
}

func TestCorsMiddleware_WildcardDefault(t *testing.T) {
	h := corsMiddleware(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Origin", "https://anything.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

func TestCorsMiddleware_AllowlistRejectsUnknownOrigin(t *testing.T) {
	h := corsMiddleware([]string{"https://allowed.example"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty for disallowed origin", got)
	}
}

func TestCanAccessMachine(t *testing.T) {
	operator := &User{Role: RoleOperator, AssignedMachines: []string{"m1", "m2"}}
	operatorNoMachines := &User{Role: RoleOperator, AssignedMachines: nil}
	manager := &User{Role: RoleManager}
	admin := &User{Role: RoleAdmin}

	cases := []struct {
		name string
		user *User
		id   string
		want bool
	}{
		{"operator sees an assigned machine", operator, "m1", true},
		{"operator does not see an unassigned machine", operator, "m5", false},
		{"operator with no assignments sees nothing (fail closed)", operatorNoMachines, "m1", false},
		{"manager sees everything", manager, "m99", true},
		{"admin sees everything", admin, "m99", true},
		{"nil user sees nothing", nil, "m1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := canAccessMachine(c.user, c.id); got != c.want {
				t.Errorf("canAccessMachine() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestScopedMachines(t *testing.T) {
	all := []*Machine{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}}

	t.Run("operator sees only assigned machines", func(t *testing.T) {
		operator := &User{Role: RoleOperator, AssignedMachines: []string{"m1", "m3"}}
		got := scopedMachines(operator, all)
		if len(got) != 2 || got[0].ID != "m1" || got[1].ID != "m3" {
			t.Errorf("scopedMachines() = %v, want [m1 m3]", got)
		}
	})

	t.Run("operator with no assignments sees nothing", func(t *testing.T) {
		operator := &User{Role: RoleOperator}
		got := scopedMachines(operator, all)
		if len(got) != 0 {
			t.Errorf("scopedMachines() = %v, want empty", got)
		}
	})

	t.Run("manager sees everything unfiltered", func(t *testing.T) {
		manager := &User{Role: RoleManager}
		got := scopedMachines(manager, all)
		if len(got) != len(all) {
			t.Errorf("scopedMachines() = %d machines, want %d", len(got), len(all))
		}
	})
}
