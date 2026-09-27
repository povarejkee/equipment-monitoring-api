package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
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

	// bgCtx cancels the background loops below on shutdown, between ticks
	// — not mid-tick. A bare SIGTERM previously killed the process
	// instantly, which could cut off a Tick() transaction or an in-flight
	// HTTP request; both are now given a chance to finish (see the
	// shutdown sequence at the bottom of main).
	bgCtx, cancelBg := context.WithCancel(context.Background())
	defer cancelBg()

	// Real-time loop: advance metrics every 4s and push to WS clients. A
	// DB blip must not broadcast an empty/nil payload — that would
	// overwrite every connected client's good data with nothing — so a
	// failed tick is logged and skipped, not broadcast.
	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-ticker.C:
				machines, err := store.Tick()
				if err != nil {
					logger.Error("tick failed, skipping broadcast", "error", err)
					continue
				}
				hub.Broadcast("machines", machines)
			}
		}
	}()

	// Daily maintenance: bound metric_history growth (unbounded, it would
	// exhaust Render's free 1GB Postgres plan in ~2 months) and sweep
	// sessions nobody ever looked up again after they expired. Runs once
	// shortly after startup too, rather than waiting a full day for the
	// first pass.
	maintenanceDone := make(chan struct{})
	go func() {
		defer close(maintenanceDone)
		runMaintenance := func() {
			mctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if n, err := store.PruneMetricHistory(mctx); err != nil {
				logger.Error("prune metric_history failed", "error", err)
			} else if n > 0 {
				logger.Info("pruned metric_history", "rows", n)
			}
			if n, err := auth.PruneExpiredSessions(mctx); err != nil {
				logger.Error("prune expired sessions failed", "error", err)
			} else if n > 0 {
				logger.Info("pruned expired sessions", "rows", n)
			}
		}
		runMaintenance()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-ticker.C:
				runMaintenance()
			}
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
	mux.HandleFunc("PUT /api/machines/{id}/status", srv.authMiddleware(requireRoles([]UserRole{RoleManager, RoleAdmin}, srv.handleUpdateMachineStatus)))
	mux.HandleFunc("PUT /api/machines/{id}/maintenance-schedule", srv.authMiddleware(requireRoles([]UserRole{RoleManager, RoleAdmin}, srv.handleUpdateMaintenanceSchedule)))
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
	mux.HandleFunc("GET /api/users", srv.authMiddleware(requireRoles([]UserRole{RoleManager, RoleAdmin}, srv.handleUsers)))
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

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("server starting", "port", port, "origins", originsLabel(allowed))
		serveErr <- server.ListenAndServe()
	}()

	// Render (and most orchestrators) send SIGTERM before killing the
	// process on redeploy/scale-down — this is what turns that into a
	// clean drain instead of requests and DB writes being cut off
	// mid-flight, which was the previous behavior (no signal handling at
	// all: every deploy was a hard kill).
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("server stopped", "error", err)
			os.Exit(1)
		}
		return
	case <-sigCtx.Done():
		logger.Info("shutdown signal received, draining")
	}

	// Stop taking new work: no more ticks/maintenance, no new HTTP
	// connections. Existing in-flight requests get up to 15s to finish.
	cancelBg()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown", "error", err)
	}

	// Let a Tick()/maintenance pass that was already running finish its
	// transaction rather than having it torn down by pool.Close() (the
	// deferred call at the top of main) the instant this function returns.
	for _, done := range []chan struct{}{tickerDone, maintenanceDone} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	logger.Info("shutdown complete")
}

func originsLabel(allowed []string) string {
	if len(allowed) == 0 {
		return "*"
	}
	return strings.Join(allowed, ",")
}
