package main

import (
	"encoding/json"
	"net/http"
	"strconv"
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
	all := s.store.Alerts()
	out := make([]*Alert, 0)
	for _, a := range all {
		if a.MachineID == id {
			out = append(out, a)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ── Alerts ───────────────────────────────────────────────────────────

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Alerts())
}

func (s *Server) handleAcknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r)
	uid := "unknown"
	if user != nil {
		uid = user.ID
	}
	s.store.AcknowledgeAlert(r.PathValue("id"), uid)
	writeJSON(w, http.StatusOK, s.store.Alerts())
}

func (s *Server) handleAcknowledgeAll(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r)
	uid := "unknown"
	if user != nil {
		uid = user.ID
	}
	s.store.AcknowledgeAllAlerts(uid)
	writeJSON(w, http.StatusOK, s.store.Alerts())
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
	writeJSON(w, http.StatusOK, s.store.ErrorLog())
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

// ── Users ────────────────────────────────────────────────────────────

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Users())
}

// ── Health ───────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── WebSocket ────────────────────────────────────────────────────────

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// WS auth is best-effort: reject only if a token is present but invalid.
	if tok := bearerToken(r); tok != "" && s.auth.UserForToken(tok) == nil {
		writeError(w, http.StatusUnauthorized, "Не авторизован")
		return
	}
	s.hub.HandleWS(w, r, s.store.Machines())
}
