package main

import (
	"context"
	"testing"

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
