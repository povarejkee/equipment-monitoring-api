package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestUpdateStatus(t *testing.T) {
	vib := func(v float64) *float64 { return &v }

	cases := []struct {
		name       string
		temp, load float64
		vibration  *float64
		wantStatus MachineStatus
	}{
		{"high temperature -> error", 96, 50, vib(2), StatusError},
		{"high vibration -> error", 60, 50, vib(7.1), StatusError},
		{"error takes priority over warning-level load", 96, 90, vib(2), StatusError},
		{"elevated temperature -> warning", 85, 50, vib(2), StatusWarning},
		{"elevated load -> warning", 60, 90, vib(2), StatusWarning},
		{"elevated vibration -> warning", 60, 50, vib(5), StatusWarning},
		{"low load -> idle", 60, 4, vib(0.1), StatusIdle},
		{"nominal -> running", 60, 50, vib(2), StatusRunning},
		{"nil vibration treated as zero", 60, 50, nil, StatusRunning},
		{"boundary temp 80 is not yet a warning", 80, 50, vib(2), StatusRunning},
		{"boundary temp 80.1 is a warning", 80.1, 50, vib(2), StatusWarning},
	}

	s := &Store{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &Machine{
				Metrics: MachineMetrics{
					Temperature: c.temp,
					Load:        c.load,
					Vibration:   c.vibration,
				},
			}
			s.updateStatus(m)
			if m.Status != c.wantStatus {
				t.Errorf("updateStatus() = %v, want %v", m.Status, c.wantStatus)
			}
		})
	}
}

func TestTick_OpensAlertOnThresholdCrossing(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()

	mustExecTest(t, pool, `INSERT INTO thresholds (metric, label, unit, warning_value, critical_value) VALUES
		('temperature', 'Температура', '°C', 80, 95)`)
	mustExecTest(t, pool, `INSERT INTO machines
		(id, name, type, location, status, temperature, load, output, uptime, power_consumption, vibration, last_updated)
		VALUES ('m1', 'ЧПУ-01', 'cnc', 'Цех 1', 'running', 96, 50, 20, 10, 20, 1, now())`)

	store := NewStore(pool)
	if _, err := store.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM alerts WHERE machine_id='m1' AND metric_name='temperature' AND severity='critical'`,
	).Scan(&count); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 critical temperature alert after crossing threshold, got %d", count)
	}
}

func TestTick_DoesNotDuplicateOpenAlerts(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()

	mustExecTest(t, pool, `INSERT INTO thresholds (metric, label, unit, warning_value, critical_value) VALUES
		('temperature', 'Температура', '°C', 80, 95)`)
	mustExecTest(t, pool, `INSERT INTO machines
		(id, name, type, location, status, temperature, load, output, uptime, power_consumption, vibration, last_updated)
		VALUES ('m1', 'ЧПУ-01', 'cnc', 'Цех 1', 'running', 96, 50, 20, 10, 20, 1, now())`)

	store := NewStore(pool)
	for i := 0; i < 3; i++ {
		if _, err := store.Tick(); err != nil {
			t.Fatalf("Tick #%d: %v", i, err)
		}
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM alerts WHERE machine_id='m1' AND metric_name='temperature'`,
	).Scan(&count); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected ticking 3x to still leave exactly 1 open alert (hysteresis), got %d", count)
	}
}

func TestTick_ReAlertsAfterAcknowledgeIfStillOutOfRange(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()

	mustExecTest(t, pool, `INSERT INTO thresholds (metric, label, unit, warning_value, critical_value) VALUES
		('temperature', 'Температура', '°C', 80, 95)`)
	mustExecTest(t, pool, `INSERT INTO machines
		(id, name, type, location, status, temperature, load, output, uptime, power_consumption, vibration, last_updated)
		VALUES ('m1', 'ЧПУ-01', 'cnc', 'Цех 1', 'running', 96, 50, 20, 10, 20, 1, now())`)

	store := NewStore(pool)
	if _, err := store.Tick(); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	mustExecTest(t, pool, `UPDATE alerts SET acknowledged=true WHERE machine_id='m1' AND metric_name='temperature'`)
	// Pin the temperature back above critical — Tick()'s own random walk
	// could otherwise wander back under the threshold before the second
	// call and make this test flaky.
	mustExecTest(t, pool, `UPDATE machines SET temperature=96 WHERE id='m1'`)

	if _, err := store.Tick(); err != nil {
		t.Fatalf("second Tick: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM alerts WHERE machine_id='m1' AND metric_name='temperature'`,
	).Scan(&count); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected a second alert after the first was acknowledged and the value is still out of range, got %d total", count)
	}
}

func TestTick_NoAlertWithinNormalRange(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()

	mustExecTest(t, pool, `INSERT INTO thresholds (metric, label, unit, warning_value, critical_value) VALUES
		('temperature', 'Температура', '°C', 80, 95)`)
	mustExecTest(t, pool, `INSERT INTO machines
		(id, name, type, location, status, temperature, load, output, uptime, power_consumption, vibration, last_updated)
		VALUES ('m1', 'ЧПУ-01', 'cnc', 'Цех 1', 'running', 50, 50, 20, 10, 20, 1, now())`)

	store := NewStore(pool)
	if _, err := store.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE machine_id='m1'`).Scan(&count); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	// Tick()'s fluctuation is at most ±0.5°C from 50 in one step, nowhere
	// near the 80/95 thresholds, so no alert should fire.
	if count != 0 {
		t.Fatalf("expected no alert for a value nowhere near threshold, got %d", count)
	}
}

func mustExecTest(t *testing.T, pool *pgxpool.Pool, sql string, args ...interface{}) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec failed: %v\nsql: %s", err, sql)
	}
}

// ── SetMachineStatus / downtimes / uptime ───────────────────────────────

func seedRunningMachine(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	mustExecTest(t, pool, `INSERT INTO machines
		(id, name, type, location, status, temperature, load, output, uptime, power_consumption, vibration, last_updated)
		VALUES ($1, 'ЧПУ-01', 'cnc', 'Цех 1', 'running', 50, 50, 20, 10, 20, 1, now())`, id)
}

func TestSetMachineStatus_StoppingOpensADowntime(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	seedRunningMachine(t, pool, "m1")
	store := NewStore(pool)

	m, err := store.SetMachineStatus("m1", StatusMaintenance, "Плановая профилактика")
	if err != nil {
		t.Fatalf("SetMachineStatus: %v", err)
	}
	if m.Status != StatusMaintenance {
		t.Errorf("status = %v, want %v", m.Status, StatusMaintenance)
	}
	if m.Metrics.Uptime != 0 {
		t.Errorf("uptime = %v, want 0 after stopping", m.Metrics.Uptime)
	}

	var count int
	var reason string
	var endTime *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM downtimes WHERE machine_id='m1'`,
	).Scan(&count); err != nil {
		t.Fatalf("count downtimes: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 downtime opened, got %d", count)
	}
	if err := pool.QueryRow(ctx,
		`SELECT reason, end_time FROM downtimes WHERE machine_id='m1'`,
	).Scan(&reason, &endTime); err != nil {
		t.Fatalf("query downtime: %v", err)
	}
	if reason != "Плановая профилактика" {
		t.Errorf("reason = %q, want the given reason", reason)
	}
	if endTime != nil {
		t.Error("expected the new downtime to still be open (end_time NULL)")
	}
}

func TestSetMachineStatus_ResumingClosesTheDowntime(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	seedRunningMachine(t, pool, "m1")
	store := NewStore(pool)

	if _, err := store.SetMachineStatus("m1", StatusOffline, "Авария"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Backdate the start so duration is meaningfully non-zero and this
	// isn't sensitive to test execution speed.
	mustExecTest(t, pool, `UPDATE downtimes SET start_time = now() - interval '30 minutes' WHERE machine_id='m1'`)

	m, err := store.SetMachineStatus("m1", StatusRunning, "")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if m.Status != StatusRunning {
		t.Errorf("status = %v, want %v", m.Status, StatusRunning)
	}

	var endTime *time.Time
	var duration *int
	if err := pool.QueryRow(ctx,
		`SELECT end_time, duration FROM downtimes WHERE machine_id='m1'`,
	).Scan(&endTime, &duration); err != nil {
		t.Fatalf("query downtime: %v", err)
	}
	if endTime == nil {
		t.Fatal("expected the downtime to be closed (end_time set)")
	}
	if duration == nil || *duration < 29 || *duration > 31 {
		t.Errorf("duration = %v, want ~30 minutes", duration)
	}
}

func TestSetMachineStatus_RejectsNonManualStatus(t *testing.T) {
	pool := requireTestDB(t)
	seedRunningMachine(t, pool, "m1")
	store := NewStore(pool)

	if _, err := store.SetMachineStatus("m1", StatusWarning, ""); !errors.Is(err, errInvalidStatus) {
		t.Errorf("expected errInvalidStatus for a derived status, got %v", err)
	}
}

func TestSetMachineStatus_UnknownMachine(t *testing.T) {
	pool := requireTestDB(t)
	store := NewStore(pool)

	if _, err := store.SetMachineStatus("does-not-exist", StatusMaintenance, ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("expected pgx.ErrNoRows, got %v", err)
	}
}

func TestTick_AccumulatesUptimeForRunningMachines(t *testing.T) {
	pool := requireTestDB(t)
	seedRunningMachine(t, pool, "m1")
	// Backdate last_updated so Tick() sees a known elapsed window.
	mustExecTest(t, pool, `UPDATE machines SET last_updated = now() - interval '30 minutes', uptime = 5 WHERE id='m1'`)

	store := NewStore(pool)
	machines, err := store.Tick()
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(machines) != 1 {
		t.Fatalf("expected 1 machine, got %d", len(machines))
	}
	got := machines[0].Metrics.Uptime
	if got < 5.4 || got > 5.6 {
		t.Errorf("uptime = %v, want ~5.5 (5 + 0.5h elapsed)", got)
	}
}

func TestTick_DoesNotAccumulateUptimeForStoppedMachines(t *testing.T) {
	pool := requireTestDB(t)
	mustExecTest(t, pool, `INSERT INTO machines
		(id, name, type, location, status, temperature, load, output, uptime, power_consumption, vibration, last_updated)
		VALUES ('m1', 'ЧПУ-01', 'cnc', 'Цех 1', 'maintenance', 50, 50, 20, 5, 20, 1, now() - interval '30 minutes')`)

	store := NewStore(pool)
	machines, err := store.Tick()
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if machines[0].Metrics.Uptime != 5 {
		t.Errorf("uptime = %v, want unchanged 5 (machine is stopped)", machines[0].Metrics.Uptime)
	}
}

// ── Maintenance-due alerts ───────────────────────────────────────────

func TestTick_RaisesMaintenanceDueAlertWithinWindow(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	seedRunningMachine(t, pool, "m1")
	due := time.Now().Add(24 * time.Hour)
	mustExecTest(t, pool, `UPDATE machines SET next_maintenance_at=$1 WHERE id='m1'`, due)

	store := NewStore(pool)
	if _, err := store.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM alerts WHERE machine_id='m1' AND type='maintenance_due'`,
	).Scan(&count); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 maintenance_due alert, got %d", count)
	}
}

func TestTick_NoMaintenanceAlertOutsideWindow(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	seedRunningMachine(t, pool, "m1")
	farFuture := time.Now().Add(30 * 24 * time.Hour)
	mustExecTest(t, pool, `UPDATE machines SET next_maintenance_at=$1 WHERE id='m1'`, farFuture)

	store := NewStore(pool)
	if _, err := store.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE machine_id='m1'`).Scan(&count); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no alert for a maintenance date 30 days out, got %d", count)
	}
}

func TestTick_DoesNotDuplicateMaintenanceAlert(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	seedRunningMachine(t, pool, "m1")
	due := time.Now().Add(24 * time.Hour)
	mustExecTest(t, pool, `UPDATE machines SET next_maintenance_at=$1 WHERE id='m1'`, due)

	store := NewStore(pool)
	for i := 0; i < 3; i++ {
		if _, err := store.Tick(); err != nil {
			t.Fatalf("Tick #%d: %v", i, err)
		}
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM alerts WHERE machine_id='m1' AND type='maintenance_due'`,
	).Scan(&count); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected ticking 3x to still leave exactly 1 maintenance alert, got %d", count)
	}
}
