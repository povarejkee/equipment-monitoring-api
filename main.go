package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	appdb "equipment-monitoring-api/internal/db"
)

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		logger.Error("DATABASE_URL is not set")
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := appdb.Connect(ctx, databaseURL)
	if err != nil {
		logger.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := appdb.Migrate(ctx, pool); err != nil {
		logger.Error("migration failed", "error", err)
		os.Exit(1)
	}

	store := NewStore(pool)
	auth := NewAuthManager(pool)
	hub := NewHub()
	srv := NewServer(store, auth, hub)
	loginLimiter := newIPRateLimiter(5, time.Minute)

	// Real-time loop: advance metrics every 4s and push to WS clients. A
	// DB blip must not broadcast an empty/nil payload — that would
	// overwrite every connected client's good data with nothing — so a
	// failed tick is logged and skipped, not broadcast.
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			machines, err := store.Tick()
			if err != nil {
				logger.Error("tick failed, skipping broadcast", "error", err)
				continue
			}
			hub.Broadcast("machines", machines)
		}
	}()

	mux := http.NewServeMux()

	// Public
	mux.HandleFunc("POST /api/auth/login", rateLimitMiddleware(loginLimiter, srv.handleLogin))
	mux.HandleFunc("GET /api/health", srv.handleHealth)

	// Protected
	mux.HandleFunc("GET /ws", srv.authMiddleware(srv.handleWS))
	mux.HandleFunc("GET /api/auth/me", srv.authMiddleware(srv.handleMe))
	mux.HandleFunc("POST /api/auth/logout", srv.authMiddleware(srv.handleLogout))
	mux.HandleFunc("GET /api/machines", srv.authMiddleware(srv.handleMachines))
	mux.HandleFunc("GET /api/machines/{id}", srv.authMiddleware(srv.handleMachine))
	mux.HandleFunc("GET /api/machines/{id}/history", srv.authMiddleware(srv.handleMachineHistory))
	mux.HandleFunc("GET /api/machines/{id}/downtimes", srv.authMiddleware(srv.handleMachineDowntimes))
	mux.HandleFunc("GET /api/machines/{id}/alerts", srv.authMiddleware(srv.handleMachineAlerts))
	mux.HandleFunc("GET /api/alerts", srv.authMiddleware(srv.handleAlerts))
	mux.HandleFunc("POST /api/alerts/acknowledge-all", srv.authMiddleware(srv.handleAcknowledgeAll))
	mux.HandleFunc("POST /api/alerts/{id}/acknowledge", srv.authMiddleware(srv.handleAcknowledgeAlert))
	mux.HandleFunc("GET /api/thresholds", srv.authMiddleware(srv.handleThresholds))
	mux.HandleFunc("PUT /api/thresholds", srv.authMiddleware(srv.handleUpdateThreshold))
	mux.HandleFunc("GET /api/errors", srv.authMiddleware(srv.handleErrors))
	mux.HandleFunc("POST /api/reports", srv.authMiddleware(srv.handleReport))
	mux.HandleFunc("GET /api/users", srv.authMiddleware(srv.handleUsers))
	mux.HandleFunc("POST /api/users", srv.authMiddleware(requireRole(RoleAdmin, srv.handleCreateUser)))
	mux.HandleFunc("PUT /api/users/{id}", srv.authMiddleware(requireRole(RoleAdmin, srv.handleUpdateUser)))
	mux.HandleFunc("DELETE /api/users/{id}", srv.authMiddleware(requireRole(RoleAdmin, srv.handleDeleteUser)))

	var allowed []string
	if raw := os.Getenv("ALLOWED_ORIGINS"); raw != "" {
		for _, o := range strings.Split(raw, ",") {
			if o = strings.TrimSpace(o); o != "" {
				allowed = append(allowed, o)
			}
		}
	}
	if len(allowed) == 0 {
		logger.Warn("ALLOWED_ORIGINS is not set — CORS is wide open (*); set it to the frontend origin in production")
	}
	handler := recoverMiddleware(corsMiddleware(allowed, mux))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	logger.Info("server starting", "port", port, "origins", originsLabel(allowed))
	if err := server.ListenAndServe(); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func originsLabel(allowed []string) string {
	if len(allowed) == 0 {
		return "*"
	}
	return strings.Join(allowed, ",")
}
