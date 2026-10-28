package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestShiftWindows_SkipsFutureAndClipsRunningShift(t *testing.T) {
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 5, 10, 30, 0, 0, time.UTC)

	ws := shiftWindows(day, day, time.UTC, now)

	if len(ws) != 2 {
		t.Fatalf("got %d windows, want 2 (night, day) — evening hasn't started", len(ws))
	}
	if ws[0].def.Key != "night" || ws[0].inProgress {
		t.Errorf("first window = %+v, want a finished night shift", ws[0])
	}
	if ws[1].def.Key != "day" || !ws[1].inProgress || !ws[1].to.Equal(now) {
		t.Errorf("second window = %+v, want the day shift clipped to now and in progress", ws[1])
	}
}

func TestShiftWindows_UsesPlantTimezoneNotUTC(t *testing.T) {
	msk := time.FixedZone("MSK", 3*3600)
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, msk)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, msk)

	ws := shiftWindows(day, day, msk, now)

	if len(ws) != 3 {
		t.Fatalf("got %d windows, want 3", len(ws))
	}
	// The day shift starts at 08:00 Moscow time = 05:00 UTC.
	if got := ws[1].from.UTC().Hour(); got != 5 {
		t.Errorf("day shift starts at %02d:00 UTC, want 05:00 (08:00 MSK)", got)
	}
}

func TestGenerateShiftReport_AggregatesPerShiftAndSplitsDowntimeAtBoundary(t *testing.T) {
	pool := requireTestDB(t)
	mustExecTest(t, pool, `INSERT INTO machines
		(id, name, type, location, status, temperature, load, output, uptime, power_consumption, vibration, last_updated)
		VALUES ('m1', 'ЧПУ-01', 'cnc', 'Цех 1', 'running', 60, 50, 20, 10, 20, 1, now()),
		       ('m2', 'ЧПУ-02', 'cnc', 'Цех 1', 'running', 60, 50, 20, 10, 20, 1, now())`)
	hist := func(ts string, output, load float64) {
		mustExecTest(t, pool, `INSERT INTO metric_history (machine_id, timestamp, temperature, load, output, vibration, power_consumption)
			VALUES ('m1', $1, 60, $3, $2, 1, 20)`, ts, output, load)
	}
	hist("2026-10-05T03:00:00Z", 10, 40)
	hist("2026-10-05T09:00:00Z", 20, 60)
	hist("2026-10-05T10:00:00Z", 30, 80)
	hist("2026-10-05T17:00:00Z", 5, 20)
	// 07:00–09:00 straddles the night/day boundary at 08:00.
	mustExecTest(t, pool, `INSERT INTO downtimes (id, machine_id, start_time, end_time, duration, reason)
		VALUES ('d1', 'm1', '2026-10-05T07:00:00Z', '2026-10-05T09:00:00Z', 120, 'test')`)
	mustExecTest(t, pool, `INSERT INTO error_log (id, machine_id, machine_name, error_code, error_type, description, timestamp, impact)
		VALUES ('e1', 'm1', 'ЧПУ-01', 'E1', 'overheating', 'x', '2026-10-05T12:00:00Z', 'low')`)

	store := NewStore(pool)
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	machines, _ := store.Machines()

	rep, err := store.GenerateShiftReport(day, day, machines, time.UTC, now)
	if err != nil {
		t.Fatalf("GenerateShiftReport: %v", err)
	}
	if len(rep.Shifts) != 3 {
		t.Fatalf("got %d shifts, want 3", len(rep.Shifts))
	}
	night, dayShift, evening := rep.Shifts[0], rep.Shifts[1], rep.Shifts[2]

	if night.TotalOutput != 10 || dayShift.TotalOutput != 50 || evening.TotalOutput != 5 {
		t.Errorf("output per shift = %v/%v/%v, want 10/50/5", night.TotalOutput, dayShift.TotalOutput, evening.TotalOutput)
	}
	if night.DowntimeHours != 1 || dayShift.DowntimeHours != 1 || evening.DowntimeHours != 0 {
		t.Errorf("downtime per shift = %v/%v/%v, want 1/1/0 (2h downtime split at the 08:00 boundary)",
			night.DowntimeHours, dayShift.DowntimeHours, evening.DowntimeHours)
	}
	if dayShift.ErrorCount != 1 || night.ErrorCount != 0 {
		t.Errorf("errors night/day = %d/%d, want 0/1", night.ErrorCount, dayShift.ErrorCount)
	}
	if math.Abs(dayShift.AvgLoad-70) > 0.05 {
		t.Errorf("day avg load = %v, want 70", dayShift.AvgLoad)
	}
	// 1h down of 2 machines × 8h = 16 machine-hours → 93.75% available.
	if math.Abs(night.UptimePercent-93.8) > 0.05 {
		t.Errorf("night fleet uptime = %v, want ~93.8", night.UptimePercent)
	}
	if len(night.Machines) != 2 {
		t.Fatalf("per-machine rows = %d, want 2", len(night.Machines))
	}
	if night.Machines[0].MachineName != "ЧПУ-01" || math.Abs(night.Machines[0].UptimePercent-87.5) > 0.05 {
		t.Errorf("m1 night = %+v, want 87.5%% uptime", night.Machines[0])
	}
}

func TestShiftReportHandler_OperatorSeesOnlyOwnMachines(t *testing.T) {
	pool := requireTestDB(t)
	op := seedAlertFixture(t, pool) // m1, m2; operator assigned to m1
	srv := NewServer(NewStore(pool), NewAuthManager(pool), NewHub())

	req := withUser(httptest.NewRequest(http.MethodGet, "/api/reports/shifts", nil), op)
	rec := httptest.NewRecorder()
	srv.handleShiftReport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var rep ShiftReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	for _, sh := range rep.Shifts {
		if len(sh.Machines) != 1 || sh.Machines[0].MachineID != "m1" {
			t.Fatalf("operator's shift report leaked machines: %+v", sh.Machines)
		}
	}
}

func TestShiftReportHandler_RejectsBadInput(t *testing.T) {
	pool := requireTestDB(t)
	srv := NewServer(NewStore(pool), NewAuthManager(pool), NewHub())
	admin := &User{ID: "u3", Role: RoleAdmin}

	cases := map[string]string{
		"malformed date":     "?dateFrom=05.10.2026",
		"from after to":      "?dateFrom=2026-10-10&dateTo=2026-10-01",
		"range over a month": "?dateFrom=2026-08-01&dateTo=2026-10-01",
	}
	for name, q := range cases {
		req := withUser(httptest.NewRequest(http.MethodGet, "/api/reports/shifts"+q, nil), admin)
		rec := httptest.NewRecorder()
		srv.handleShiftReport(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}

func TestReportHandler_OperatorIsScopedToOwnMachines(t *testing.T) {
	pool := requireTestDB(t)
	op := seedAlertFixture(t, pool)
	srv := NewServer(NewStore(pool), NewAuthManager(pool), NewHub())

	post := func(body string) *httptest.ResponseRecorder {
		req := withUser(httptest.NewRequest(http.MethodPost, "/api/reports", stringsReader(body)), op)
		rec := httptest.NewRecorder()
		srv.handleReport(rec, req)
		return rec
	}
	from, to := time.Now().Add(-24*time.Hour).Format(time.RFC3339), time.Now().Format(time.RFC3339)

	rec := post(`{"dateFrom":"` + from + `","dateTo":"` + to + `","machineIds":[],"groupBy":"day"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var rep ReportData
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.MachineBreakdown) != 1 || rep.MachineBreakdown[0].MachineID != "m1" {
		t.Errorf("an empty machineIds must not widen an operator to every machine: %+v", rep.MachineBreakdown)
	}

	if rec := post(`{"dateFrom":"` + from + `","dateTo":"` + to + `","machineIds":["m2"],"groupBy":"day"}`); rec.Code != http.StatusForbidden {
		t.Errorf("explicitly requesting a foreign machine: status = %d, want 403", rec.Code)
	}
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }

func TestGenerateShiftReport_BucketsByPlantWallClockNotUTC(t *testing.T) {
	pool := requireTestDB(t)
	msk, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	mustExecTest(t, pool, `INSERT INTO machines
		(id, name, type, location, status, temperature, load, output, uptime, power_consumption, vibration, last_updated)
		VALUES ('m1', 'ЧПУ-01', 'cnc', 'Цех 1', 'running', 60, 50, 20, 10, 20, 1, now())`)
	// 05:30 UTC is 08:30 in Moscow: the DAY shift there, but still the
	// night shift if the SQL bucketing wrongly used UTC hours.
	mustExecTest(t, pool, `INSERT INTO metric_history (machine_id, timestamp, temperature, load, output, vibration, power_consumption)
		VALUES ('m1', '2026-10-05T05:30:00Z', 60, 50, 42, 1, 20)`)

	store := NewStore(pool)
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, msk)
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, msk)
	machines, _ := store.Machines()

	rep, err := store.GenerateShiftReport(day, day, machines, msk, now)
	if err != nil {
		t.Fatalf("GenerateShiftReport: %v", err)
	}
	if rep.Shifts[0].TotalOutput != 0 || rep.Shifts[1].TotalOutput != 42 {
		t.Errorf("night/day output = %v/%v, want 0/42 (08:30 MSK belongs to the day shift)",
			rep.Shifts[0].TotalOutput, rep.Shifts[1].TotalOutput)
	}
}
