package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func withUser(r *http.Request, u *User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userCtxKey, u))
}

// seedAlertFixture creates two machines with one open alert each and
// returns an operator assigned only to m1.
func seedAlertFixture(t *testing.T, pool *pgxpool.Pool) *User {
	t.Helper()
	mustExecTest(t, pool, `INSERT INTO machines
		(id, name, type, location, status, temperature, load, output, uptime, power_consumption, vibration, last_updated)
		VALUES ('m1', 'ЧПУ-01', 'cnc', 'Цех 1', 'running', 60, 50, 20, 10, 20, 1, now()),
		       ('m2', 'ЧПУ-02', 'cnc', 'Цех 1', 'running', 60, 50, 20, 10, 20, 1, now())`)
	mustExecTest(t, pool, `INSERT INTO alerts
		(id, machine_id, machine_name, type, severity, message, metric_name, current_value, threshold_value, timestamp)
		VALUES ('a1', 'm1', 'ЧПУ-01', 'threshold_exceeded', 'critical', 'x', 'temperature', 96, 95, now()),
		       ('a2', 'm2', 'ЧПУ-02', 'threshold_exceeded', 'critical', 'x', 'temperature', 96, 95, now())`)
	return &User{ID: "u1", Role: RoleOperator, AssignedMachines: []string{"m1"}}
}

func alertAcknowledged(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var ack bool
	if err := pool.QueryRow(context.Background(), `SELECT acknowledged FROM alerts WHERE id=$1`, id).Scan(&ack); err != nil {
		t.Fatalf("read alert %s: %v", id, err)
	}
	return ack
}

func TestAcknowledgeAlert_OperatorCannotAcknowledgeForeignMachine(t *testing.T) {
	pool := requireTestDB(t)
	op := seedAlertFixture(t, pool)
	srv := NewServer(NewStore(pool), NewAuthManager(pool), NewHub())

	req := withUser(httptest.NewRequest(http.MethodPost, "/api/alerts/a2/acknowledge", nil), op)
	req.SetPathValue("id", "a2")
	rec := httptest.NewRecorder()
	srv.handleAcknowledgeAlert(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if alertAcknowledged(t, pool, "a2") {
		t.Error("foreign alert was acknowledged despite the 403")
	}
}

func TestAcknowledgeAlert_OperatorCanAcknowledgeOwnAndSeesOnlyOwn(t *testing.T) {
	pool := requireTestDB(t)
	op := seedAlertFixture(t, pool)
	srv := NewServer(NewStore(pool), NewAuthManager(pool), NewHub())

	req := withUser(httptest.NewRequest(http.MethodPost, "/api/alerts/a1/acknowledge", nil), op)
	req.SetPathValue("id", "a1")
	rec := httptest.NewRecorder()
	srv.handleAcknowledgeAlert(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !alertAcknowledged(t, pool, "a1") {
		t.Error("own alert was not acknowledged")
	}
	var got []Alert
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].MachineID != "m1" {
		t.Errorf("response leaked other machines' alerts: %+v", got)
	}
}

func TestAcknowledgeAlert_UnknownAlertIs404(t *testing.T) {
	pool := requireTestDB(t)
	op := seedAlertFixture(t, pool)
	srv := NewServer(NewStore(pool), NewAuthManager(pool), NewHub())

	req := withUser(httptest.NewRequest(http.MethodPost, "/api/alerts/nope/acknowledge", nil), op)
	req.SetPathValue("id", "nope")
	rec := httptest.NewRecorder()
	srv.handleAcknowledgeAlert(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAcknowledgeAll_OperatorOnlyAffectsOwnMachines(t *testing.T) {
	pool := requireTestDB(t)
	op := seedAlertFixture(t, pool)
	srv := NewServer(NewStore(pool), NewAuthManager(pool), NewHub())

	req := withUser(httptest.NewRequest(http.MethodPost, "/api/alerts/acknowledge-all", nil), op)
	rec := httptest.NewRecorder()
	srv.handleAcknowledgeAll(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !alertAcknowledged(t, pool, "a1") {
		t.Error("own alert should be acknowledged")
	}
	if alertAcknowledged(t, pool, "a2") {
		t.Error("operator's acknowledge-all must not touch a machine they aren't assigned to")
	}
}

func TestAcknowledgeAll_OperatorWithNoMachinesAcknowledgesNothing(t *testing.T) {
	pool := requireTestDB(t)
	seedAlertFixture(t, pool)
	srv := NewServer(NewStore(pool), NewAuthManager(pool), NewHub())

	req := withUser(httptest.NewRequest(http.MethodPost, "/api/alerts/acknowledge-all", nil),
		&User{ID: "u1", Role: RoleOperator})
	rec := httptest.NewRecorder()
	srv.handleAcknowledgeAll(rec, req)

	if alertAcknowledged(t, pool, "a1") || alertAcknowledged(t, pool, "a2") {
		t.Error("an operator with no assigned machines must acknowledge nothing (fail closed)")
	}
}

func TestAcknowledgeAll_ManagerAcknowledgesEverything(t *testing.T) {
	pool := requireTestDB(t)
	seedAlertFixture(t, pool)
	srv := NewServer(NewStore(pool), NewAuthManager(pool), NewHub())

	req := withUser(httptest.NewRequest(http.MethodPost, "/api/alerts/acknowledge-all", nil),
		&User{ID: "u2", Role: RoleManager})
	srv.handleAcknowledgeAll(httptest.NewRecorder(), req)

	if !alertAcknowledged(t, pool, "a1") || !alertAcknowledged(t, pool, "a2") {
		t.Error("manager acknowledge-all should cover every machine")
	}
}
