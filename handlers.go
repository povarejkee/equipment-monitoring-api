package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

type Server struct {
	store *Store
	auth  *AuthManager
	hub   *Hub
}

func NewServer(store *Store, auth *AuthManager, hub *Hub) *Server {
	return &Server{store: store, auth: auth, hub: hub}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ── Auth ─────────────────────────────────────────────────────────────

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Некорректный запрос")
		return
	}
	user, token, ok := s.auth.Login(req.Email, req.Password)
	if !ok {
		logger.Warn("failed login attempt", "email", req.Email, "ip", clientIP(r))
		writeError(w, http.StatusUnauthorized, "Неверный email или пароль")
		return
	}
	writeJSON(w, http.StatusOK, LoginResponse{Token: token, User: *user})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "Не авторизован")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(bearerToken(r))
	w.WriteHeader(http.StatusNoContent)
}

// ── Machines ─────────────────────────────────────────────────────────

func (s *Server) handleMachines(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Machines())
}

func (s *Server) handleMachine(w http.ResponseWriter, r *http.Request) {
	m := s.store.Machine(r.PathValue("id"))
	if m == nil {
		writeError(w, http.StatusNotFound, "Станок не найден")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleMachineHistory(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if h := r.URL.Query().Get("hours"); h != "" {
		if parsed, err := strconv.Atoi(h); err == nil && parsed > 0 {
			hours = parsed
		}
	}
	writeJSON(w, http.StatusOK, s.store.History(r.PathValue("id"), hours))
}

func (s *Server) handleMachineDowntimes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Downtimes(r.PathValue("id")))
}

func (s *Server) handleMachineAlerts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	writeJSON(w, http.StatusOK, s.store.Alerts(AlertFilter{MachineID: id}))
}

// ── Alerts ───────────────────────────────────────────────────────────

// alertFilterFromQuery reads the optional severity/machine_id/acknowledged
// query params shared by the alert-listing endpoints.
func alertFilterFromQuery(r *http.Request) AlertFilter {
	f := AlertFilter{
		MachineID: r.URL.Query().Get("machine_id"),
		Severity:  AlertSeverity(r.URL.Query().Get("severity")),
	}
	if v := r.URL.Query().Get("acknowledged"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			f.Acknowledged = &b
		}
	}
	return f
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Alerts(alertFilterFromQuery(r)))
}

func (s *Server) handleAcknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r)
	uid := "unknown"
	if user != nil {
		uid = user.ID
	}
	s.store.AcknowledgeAlert(r.PathValue("id"), uid)
	writeJSON(w, http.StatusOK, s.store.Alerts(AlertFilter{}))
}

func (s *Server) handleAcknowledgeAll(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r)
	uid := "unknown"
	if user != nil {
		uid = user.ID
	}
	s.store.AcknowledgeAllAlerts(uid)
	writeJSON(w, http.StatusOK, s.store.Alerts(AlertFilter{}))
}

// ── Thresholds ───────────────────────────────────────────────────────

func (s *Server) handleThresholds(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Thresholds())
}

func (s *Server) handleUpdateThreshold(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Metric        string  `json:"metric"`
		WarningValue  float64 `json:"warningValue"`
		CriticalValue float64 `json:"criticalValue"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Некорректный запрос")
		return
	}
	s.store.UpdateThreshold(req.Metric, req.WarningValue, req.CriticalValue)
	writeJSON(w, http.StatusOK, s.store.Thresholds())
}

// ── Errors ───────────────────────────────────────────────────────────

func (s *Server) handleErrors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ErrorLogFilter{MachineID: q.Get("machine_id")}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Offset = n
		}
	}
	if v := q.Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.From = &t
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.To = &t
		}
	}
	entries, total := s.store.ErrorLog(f)
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	writeJSON(w, http.StatusOK, entries)
}

// ── Reports ──────────────────────────────────────────────────────────

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	var p ReportParams
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "Некорректный запрос")
		return
	}
	writeJSON(w, http.StatusOK, s.store.GenerateReport(p))
}

// ── Users (admin CRUD) ──────────────────────────────────────────────

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Users())
}

func isValidRole(r UserRole) bool {
	switch r {
	case RoleOperator, RoleManager, RoleAdmin:
		return true
	default:
		return false
	}
}

type createUserRequest struct {
	Name             string   `json:"name"`
	Email            string   `json:"email"`
	Password         string   `json:"password"`
	Role             UserRole `json:"role"`
	AssignedMachines []string `json:"assignedMachines,omitempty"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Некорректный запрос")
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Name == "" || req.Email == "" || req.Password == "" || !isValidRole(req.Role) {
		writeError(w, http.StatusBadRequest, "Обязательны name, email, password и корректная role")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		logger.Error("hash password", "error", err)
		writeError(w, http.StatusInternalServerError, "Внутренняя ошибка сервера")
		return
	}
	u := User{
		ID: newUserID(), Name: req.Name, Email: req.Email,
		Role: req.Role, AssignedMachines: req.AssignedMachines,
	}
	created, err := s.store.CreateUser(u, string(hash))
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "Email уже используется")
			return
		}
		logger.Error("create user", "error", err)
		writeError(w, http.StatusInternalServerError, "Внутренняя ошибка сервера")
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

type updateUserRequest struct {
	Name             *string   `json:"name,omitempty"`
	Email            *string   `json:"email,omitempty"`
	Password         *string   `json:"password,omitempty"`
	Role             *UserRole `json:"role,omitempty"`
	AssignedMachines *[]string `json:"assignedMachines,omitempty"`
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req updateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Некорректный запрос")
		return
	}
	if req.Role != nil && !isValidRole(*req.Role) {
		writeError(w, http.StatusBadRequest, "Некорректная роль")
		return
	}

	update := UserUpdate{Name: req.Name, Role: req.Role, AssignedMachines: req.AssignedMachines}
	if req.Email != nil {
		trimmed := strings.ToLower(strings.TrimSpace(*req.Email))
		update.Email = &trimmed
	}
	if req.Password != nil && *req.Password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(*req.Password), bcrypt.DefaultCost)
		if err != nil {
			logger.Error("hash password", "error", err)
			writeError(w, http.StatusInternalServerError, "Внутренняя ошибка сервера")
			return
		}
		h := string(hash)
		update.PasswordHash = &h
	}

	updated, err := s.store.UpdateUser(id, update)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Пользователь не найден")
			return
		}
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "Email уже используется")
			return
		}
		logger.Error("update user", "error", err, "id", id)
		writeError(w, http.StatusInternalServerError, "Внутренняя ошибка сервера")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if actor := userFromContext(r); actor != nil && actor.ID == id {
		writeError(w, http.StatusBadRequest, "Нельзя удалить собственную учётную запись")
		return
	}
	if err := s.store.DeleteUser(id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Пользователь не найден")
			return
		}
		logger.Error("delete user", "error", err, "id", id)
		writeError(w, http.StatusInternalServerError, "Внутренняя ошибка сервера")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Health ───────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── WebSocket ────────────────────────────────────────────────────────

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	s.hub.HandleWS(w, r, s.store.Machines())
}
