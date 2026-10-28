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

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.051 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func TestOEEFigures_MultipliesTheThreeFactors(t *testing.T) {
	// 480 min planned, 48 down -> A 90%. 300 produced of 400 ideal -> P 75%.
	// 15 scrap of 300 -> Q 95%. OEE = .9 × .75 × .95 = 64.125%.
	f := oeeAcc{windowMin: 480, downMin: 48, producedPt: 300, idealPt: 400, scrapPt: 15}.figures()

	near(t, "availability", f.Availability, 90)
	near(t, "performance", f.Performance, 75)
	near(t, "quality", f.Quality, 95)
	near(t, "oee", f.OEE, 64.1)
	near(t, "good parts", f.GoodParts, 285)
	near(t, "downtime hours", f.DowntimeHours, 0.8)
	near(t, "running hours", f.RunningHours, 7.2)
}

func TestOEEFigures_PerformanceIsCappedAtOneHundredPercent(t *testing.T) {
	f := oeeAcc{windowMin: 60, producedPt: 500, idealPt: 400}.figures()
	near(t, "performance", f.Performance, 100)
	near(t, "oee", f.OEE, 100)
}

func TestOEEFigures_NoProductionMeansZeroOEEButFullQuality(t *testing.T) {
	f := oeeAcc{windowMin: 60, idealPt: 40}.figures()
	near(t, "performance", f.Performance, 0)
	near(t, "quality", f.Quality, 100) // nothing made, nothing scrapped
	near(t, "oee", f.OEE, 0)
}

func TestOEEFigures_ScrapCannotExceedProduction(t *testing.T) {
	f := oeeAcc{windowMin: 60, producedPt: 10, idealPt: 10, scrapPt: 50}.figures()
	near(t, "quality", f.Quality, 0)
	near(t, "scrap parts", f.ScrapParts, 10)
	near(t, "good parts", f.GoodParts, 0)
}

func TestOEEAggregation_IsWeightedByRunTimeNotAnAverageOfPercentages(t *testing.T) {
	long := oeeAcc{windowMin: 1000, producedPt: 900, idealPt: 1000} // P 90%
	short := oeeAcc{windowMin: 10, producedPt: 1, idealPt: 10}      // P 10%
	var total oeeAcc
	total.add(long)
	total.add(short)

	// A plain mean of 90% and 10% would be 50%; weighted it must stay near 90%.
	near(t, "weighted performance", total.figures().Performance, 89.2)
}

func TestOEEDays_SkipsFutureAndClipsToday(t *testing.T) {
	loc := time.UTC
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, loc)
	to := time.Date(2026, 10, 8, 0, 0, 0, 0, loc)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, loc)

	days := oeeDays(from, to, loc, now)

	if len(days) != 2 {
		t.Fatalf("got %d days, want 2 (the 5th and the part-elapsed 6th)", len(days))
	}
	if !days[1].to.Equal(now) {
		t.Errorf("today's window ends %v, want clipped to now %v", days[1].to, now)
	}
}

func seedOEEMachine(t *testing.T, id, name string, rated float64) {
	t.Helper()
	mustExecTest(t, testPool, `INSERT INTO machines
		(id, name, type, location, status, temperature, load, output, uptime, power_consumption, vibration, last_updated, rated_output)
		VALUES ($1, $2, 'cnc', 'Цех 1', 'running', 60, 50, 20, 10, 20, 1, now(), $3)`, id, name, rated)
}

func TestGenerateOEEReport_EndToEnd(t *testing.T) {
	requireTestDB(t)
	seedOEEMachine(t, "m1", "ЧПУ-01", 40)
	seedOEEMachine(t, "m2", "ЧПУ-02", 40)
	// m1: samples averaging 20 parts/h all day.
	for _, ts := range []string{"2026-10-05T03:00:00Z", "2026-10-05T09:00:00Z", "2026-10-05T15:00:00Z", "2026-10-05T21:00:00Z"} {
		mustExecTest(t, testPool, `INSERT INTO metric_history (machine_id, timestamp, temperature, load, output, vibration, power_consumption)
			VALUES ('m1', $1, 60, 50, 20, 1, 20)`, ts)
	}
	// 2.4 h of downtime -> availability 90% of the 24 h day.
	mustExecTest(t, testPool, `INSERT INTO downtimes (id, machine_id, start_time, end_time, duration, reason)
		VALUES ('d1', 'm1', '2026-10-05T10:00:00Z', '2026-10-05T12:24:00Z', 144, 'test')`)
	mustExecTest(t, testPool, `INSERT INTO scrap_records (id, machine_id, count, reason, occurred_at)
		VALUES ('s1', 'm1', 43, 'x', '2026-10-05T11:00:00Z')`)
	// m2 produced nothing at all.

	store := NewStore(testPool)
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	machines, _ := store.Machines()

	rep, err := store.GenerateOEEReport(day, day, machines, time.UTC, now)
	if err != nil {
		t.Fatalf("GenerateOEEReport: %v", err)
	}
	if len(rep.Machines) != 2 || len(rep.Days) != 1 {
		t.Fatalf("machines/days = %d/%d, want 2/1", len(rep.Machines), len(rep.Days))
	}

	// Worst first: m2 (no output) before m1.
	if rep.Machines[0].MachineID != "m2" || rep.Machines[1].MachineID != "m1" {
		t.Errorf("order = %s,%s, want m2,m1 (lowest OEE first)", rep.Machines[0].MachineID, rep.Machines[1].MachineID)
	}
	m1 := rep.Machines[1]
	near(t, "m1 availability", m1.Availability, 90)
	near(t, "m1 performance", m1.Performance, 50) // 20 of 40 parts/h
	near(t, "m1 quality", m1.Quality, 90.0)       // 43 scrap of 432 made
	near(t, "m1 oee", m1.OEE, 40.5)
	near(t, "m1 produced", m1.ProducedParts, 432)
	near(t, "m1 avg output", m1.AvgOutput, 20)
	near(t, "m2 oee", rep.Machines[0].OEE, 0)

	// Fleet: ideal-weighted. m1 made 432 of 864 ideal parts, m2 made 0 of
	// 960 (it ran all day), so 432 / 1824 = 23.7%.
	near(t, "fleet performance", rep.Summary.Performance, 23.7)
	near(t, "day matches summary for a one-day range", rep.Days[0].OEE, rep.Summary.OEE)
}

func TestScrapHandlers_RoleAndValidationRules(t *testing.T) {
	requireTestDB(t)
	seedOEEMachine(t, "m1", "ЧПУ-01", 40)
	seedOEEMachine(t, "m2", "ЧПУ-02", 40)
	srv := NewServer(NewStore(testPool), NewAuthManager(testPool), NewHub())
	op := &User{ID: "u1", Name: "Иван", Role: RoleOperator, AssignedMachines: []string{"m1"}}
	mgr := &User{ID: "u2", Name: "Мария", Role: RoleManager}

	post := func(u *User, machine, body string) *httptest.ResponseRecorder {
		req := withUser(httptest.NewRequest(http.MethodPost, "/api/machines/"+machine+"/scrap", strings.NewReader(body)), u)
		req.SetPathValue("id", machine)
		rec := httptest.NewRecorder()
		srv.handleAddScrap(rec, req)
		return rec
	}
	future := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	ancient := time.Now().Add(-40 * 24 * time.Hour).Format(time.RFC3339)

	cases := []struct {
		name   string
		user   *User
		mach   string
		body   string
		status int
	}{
		{"operator logs scrap on own machine", op, "m1", `{"count":3,"reason":"Заусенцы"}`, http.StatusCreated},
		{"operator cannot log on a foreign machine", op, "m2", `{"count":3}`, http.StatusForbidden},
		{"manager can log anywhere", mgr, "m2", `{"count":1}`, http.StatusCreated},
		{"zero count rejected", mgr, "m1", `{"count":0}`, http.StatusBadRequest},
		{"negative count rejected", mgr, "m1", `{"count":-4}`, http.StatusBadRequest},
		{"absurd count rejected", mgr, "m1", `{"count":999999}`, http.StatusBadRequest},
		{"future timestamp rejected", mgr, "m1", `{"count":1,"occurredAt":"` + future + `"}`, http.StatusBadRequest},
		{"timestamp older than 31 days rejected", mgr, "m1", `{"count":1,"occurredAt":"` + ancient + `"}`, http.StatusBadRequest},
		{"over-long reason rejected", mgr, "m1", `{"count":1,"reason":"` + strings.Repeat("я", 201) + `"}`, http.StatusBadRequest},
		{"malformed body rejected", mgr, "m1", `not json`, http.StatusBadRequest},
		{"unknown machine is 404", mgr, "nope", `{"count":1}`, http.StatusNotFound},
	}
	for _, c := range cases {
		if rec := post(c.user, c.mach, c.body); rec.Code != c.status {
			t.Errorf("%s: status = %d, want %d (%s)", c.name, rec.Code, c.status, rec.Body.String())
		}
	}

	var n int
	_ = testPool.QueryRow(t.Context(), `SELECT count(*) FROM scrap_records`).Scan(&n)
	if n != 2 {
		t.Errorf("stored %d scrap records, want exactly the 2 valid ones", n)
	}
}

func TestScrapList_ScopedAndNewestFirstWithAuthorName(t *testing.T) {
	requireTestDB(t)
	seedOEEMachine(t, "m1", "ЧПУ-01", 40)
	srv := NewServer(NewStore(testPool), NewAuthManager(testPool), NewHub())
	// requireTestDB seeds u1..u3.
	mustExecTest(t, testPool, `INSERT INTO scrap_records (id, machine_id, count, reason, occurred_at, recorded_by)
		VALUES ('old', 'm1', 1, 'a', now() - interval '2 hours', 'u1'), ('new', 'm1', 2, 'b', now() - interval '1 hour', 'u1')`)

	foreign := &User{ID: "ux", Role: RoleOperator, AssignedMachines: []string{"m9"}}
	req := withUser(httptest.NewRequest(http.MethodGet, "/api/machines/m1/scrap", nil), foreign)
	req.SetPathValue("id", "m1")
	rec := httptest.NewRecorder()
	srv.handleMachineScrap(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign operator status = %d, want 403", rec.Code)
	}

	req = withUser(httptest.NewRequest(http.MethodGet, "/api/machines/m1/scrap", nil), &User{ID: "u2", Role: RoleManager})
	req.SetPathValue("id", "m1")
	rec = httptest.NewRecorder()
	srv.handleMachineScrap(rec, req)
	var got []ScrapRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "new" {
		t.Fatalf("records = %+v, want newest first", got)
	}
	if got[0].RecordedByName != "Иван Петров" {
		t.Errorf("recordedByName = %q, want the seeded user's name", got[0].RecordedByName)
	}
}

func TestScrapDelete_ManagersOnlyAnd404ForUnknown(t *testing.T) {
	requireTestDB(t)
	seedOEEMachine(t, "m1", "ЧПУ-01", 40)
	srv := NewServer(NewStore(testPool), NewAuthManager(testPool), NewHub())
	mustExecTest(t, testPool, `INSERT INTO scrap_records (id, machine_id, count, reason, occurred_at) VALUES ('s1', 'm1', 1, '', now())`)
	guarded := requireRoles([]UserRole{RoleManager, RoleAdmin}, srv.handleDeleteScrap)

	del := func(u *User, id string) int {
		req := withUser(httptest.NewRequest(http.MethodDelete, "/api/scrap/"+id, nil), u)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		guarded(rec, req)
		return rec.Code
	}
	if code := del(&User{ID: "u1", Role: RoleOperator}, "s1"); code != http.StatusForbidden {
		t.Errorf("operator delete = %d, want 403", code)
	}
	if code := del(&User{ID: "u2", Role: RoleManager}, "nope"); code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", code)
	}
	if code := del(&User{ID: "u2", Role: RoleManager}, "s1"); code != http.StatusNoContent {
		t.Errorf("manager delete = %d, want 204", code)
	}
}

func TestRatedOutput_ValidationAndPersistence(t *testing.T) {
	requireTestDB(t)
	seedOEEMachine(t, "m1", "ЧПУ-01", 40)
	store := NewStore(testPool)
	srv := NewServer(store, NewAuthManager(testPool), NewHub())

	put := func(machine, body string) int {
		req := withUser(httptest.NewRequest(http.MethodPut, "/api/machines/"+machine+"/rated-output", strings.NewReader(body)), &User{ID: "u2", Role: RoleManager})
		req.SetPathValue("id", machine)
		rec := httptest.NewRecorder()
		srv.handleUpdateRatedOutput(rec, req)
		return rec.Code
	}
	for _, bad := range []string{`{"ratedOutput":0}`, `{"ratedOutput":-5}`, `{"ratedOutput":5000}`, `{}`, `junk`} {
		if code := put("m1", bad); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", bad, code)
		}
	}
	if code := put("nope", `{"ratedOutput":10}`); code != http.StatusNotFound {
		t.Errorf("unknown machine = %d, want 404", code)
	}
	if code := put("m1", `{"ratedOutput":55.5}`); code != http.StatusOK {
		t.Fatalf("valid update = %d, want 200", code)
	}
	m, _ := store.Machine("m1")
	if m.RatedOutput != 55.5 {
		t.Errorf("persisted rated output = %v, want 55.5", m.RatedOutput)
	}
}

func TestOEEHandler_OperatorSeesOnlyOwnMachines(t *testing.T) {
	requireTestDB(t)
	seedOEEMachine(t, "m1", "ЧПУ-01", 40)
	seedOEEMachine(t, "m2", "ЧПУ-02", 40)
	srv := NewServer(NewStore(testPool), NewAuthManager(testPool), NewHub())

	req := withUser(httptest.NewRequest(http.MethodGet, "/api/oee", nil),
		&User{ID: "u1", Role: RoleOperator, AssignedMachines: []string{"m1"}})
	rec := httptest.NewRecorder()
	srv.handleOEEReport(rec, req)

	var rep OEEReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Machines) != 1 || rep.Machines[0].MachineID != "m1" {
		t.Errorf("operator's OEE report leaked machines: %+v", rep.Machines)
	}
}
