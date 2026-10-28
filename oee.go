package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// ── models ───────────────────────────────────────────────────────────

// OEEFigures is one set of OEE numbers: the three factors and their
// product (percentages, 1 decimal) plus the part counts and hours behind
// them.
type OEEFigures struct {
	Availability  float64 `json:"availability"`
	Performance   float64 `json:"performance"`
	Quality       float64 `json:"quality"`
	OEE           float64 `json:"oee"`
	ProducedParts float64 `json:"producedParts"`
	ScrapParts    float64 `json:"scrapParts"`
	GoodParts     float64 `json:"goodParts"`
	DowntimeHours float64 `json:"downtimeHours"`
	RunningHours  float64 `json:"runningHours"`
}

type OEEMachineRow struct {
	MachineID   string  `json:"machineId"`
	MachineName string  `json:"machineName"`
	RatedOutput float64 `json:"ratedOutput"`
	AvgOutput   float64 `json:"avgOutput"`
	OEEFigures
}

type OEEDayPoint struct {
	Date string `json:"date"`
	OEEFigures
}

type OEEReport struct {
	GeneratedAt time.Time       `json:"generatedAt"`
	DateFrom    string          `json:"dateFrom"`
	DateTo      string          `json:"dateTo"`
	Timezone    string          `json:"timezone"`
	Summary     OEEFigures      `json:"summary"`
	Days        []OEEDayPoint   `json:"days"`
	Machines    []OEEMachineRow `json:"machines"`
}

type ScrapRecord struct {
	ID             string    `json:"id"`
	MachineID      string    `json:"machineId"`
	Count          int       `json:"count"`
	Reason         string    `json:"reason"`
	OccurredAt     time.Time `json:"occurredAt"`
	RecordedBy     string    `json:"recordedBy,omitempty"`
	RecordedByName string    `json:"recordedByName,omitempty"`
}

// ── calculation ──────────────────────────────────────────────────────

// oeeAcc accumulates the raw quantities OEE is derived from. Summing
// accumulators (across days or machines) before calling figures() gives
// properly weighted aggregates — an average of percentages would let a
// machine that ran 10 minutes count as much as one that ran all month.
type oeeAcc struct {
	windowMin  float64 // planned time
	downMin    float64
	producedPt float64 // parts actually made
	idealPt    float64 // parts at the rated output over the running time
	scrapPt    float64
}

func (a *oeeAcc) add(b oeeAcc) {
	a.windowMin += b.windowMin
	a.downMin += b.downMin
	a.producedPt += b.producedPt
	a.idealPt += b.idealPt
	a.scrapPt += b.scrapPt
}

func (a oeeAcc) figures() OEEFigures {
	availability := 1.0
	if a.windowMin > 0 {
		availability = clamp(1-a.downMin/a.windowMin, 0, 1)
	}
	performance := 0.0
	if a.idealPt > 0 {
		performance = clamp(a.producedPt/a.idealPt, 0, 1)
	}
	quality := 1.0
	if a.producedPt > 0 {
		quality = clamp(1-a.scrapPt/a.producedPt, 0, 1)
	}
	runningMin := a.windowMin - a.downMin
	if runningMin < 0 {
		runningMin = 0
	}
	scrap := minFloat(a.scrapPt, a.producedPt)
	return OEEFigures{
		Availability:  round1(availability * 100),
		Performance:   round1(performance * 100),
		Quality:       round1(quality * 100),
		OEE:           round1(availability * performance * quality * 100),
		ProducedParts: roundInt(a.producedPt),
		ScrapParts:    roundInt(scrap),
		GoodParts:     roundInt(a.producedPt - scrap),
		DowntimeHours: round1(minFloat(a.downMin, a.windowMin) / 60),
		RunningHours:  round1(runningMin / 60),
	}
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func roundInt(v float64) float64 { return float64(int64(v + 0.5)) }

// oeeCell is the raw data for one machine on one calendar day.
type oeeCell struct {
	windowMin float64
	downMin   float64
	outSum    float64
	samples   int
	scrap     float64
}

// acc turns a cell into an accumulator. Metric samples exist only while a
// machine runs (Tick skips stopped machines), so their mean is the
// running rate; produced parts = running rate × running hours.
func (c oeeCell) acc(rated float64) oeeAcc {
	running := c.windowMin - c.downMin
	if running < 0 {
		running = 0
	}
	hours := running / 60
	produced := 0.0
	if c.samples > 0 {
		produced = c.outSum / float64(c.samples) * hours
	}
	return oeeAcc{
		windowMin:  c.windowMin,
		downMin:    minFloat(c.downMin, c.windowMin),
		producedPt: produced,
		idealPt:    rated * hours,
		scrapPt:    c.scrap,
	}
}

type oeeDay struct {
	date     string
	from, to time.Time
}

// oeeDays lists the days of [fromDay, toDay] that have started by now,
// the current one clipped to now.
func oeeDays(fromDay, toDay time.Time, loc *time.Location, now time.Time) []oeeDay {
	var out []oeeDay
	for day := fromDay; !day.After(toDay); day = day.AddDate(0, 0, 1) {
		start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
		end := time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc)
		if !start.Before(now) {
			continue
		}
		if end.After(now) {
			end = now
		}
		out = append(out, oeeDay{date: day.Format("2006-01-02"), from: start, to: end})
	}
	return out
}

func (s *Store) GenerateOEEReport(fromDay, toDay time.Time, machines []*Machine, loc *time.Location, now time.Time) (OEEReport, error) {
	ctx := context.Background()
	days := oeeDays(fromDay, toDay, loc, now)
	report := OEEReport{
		GeneratedAt: now,
		DateFrom:    fromDay.Format("2006-01-02"),
		DateTo:      toDay.Format("2006-01-02"),
		Timezone:    loc.String(),
		Days:        make([]OEEDayPoint, 0, len(days)),
		Machines:    make([]OEEMachineRow, 0, len(machines)),
	}
	if len(days) == 0 || len(machines) == 0 {
		report.Summary = oeeAcc{}.figures()
		return report, nil
	}

	ids := make([]string, len(machines))
	for i, m := range machines {
		ids[i] = m.ID
	}
	rangeFrom, rangeTo := days[0].from, days[len(days)-1].to
	dayIdx := make(map[string]int, len(days))
	for i, d := range days {
		dayIdx[d.date] = i
	}
	cells := make(map[string][]oeeCell, len(machines))
	for _, m := range machines {
		row := make([]oeeCell, len(days))
		for i, d := range days {
			row[i].windowMin = d.to.Sub(d.from).Minutes()
		}
		cells[m.ID] = row
	}

	rows, err := s.pool.Query(ctx, `
		SELECT machine_id, (timestamp AT TIME ZONE $4)::date, COALESCE(SUM(output), 0), COUNT(*)
		FROM metric_history
		WHERE machine_id = ANY($1) AND timestamp >= $2 AND timestamp < $3
		GROUP BY machine_id, 2`, ids, rangeFrom, rangeTo, loc.String())
	if err != nil {
		return OEEReport{}, fmt.Errorf("oee metrics: %w", err)
	}
	for rows.Next() {
		var id string
		var day time.Time
		var sum float64
		var n int
		if err := rows.Scan(&id, &day, &sum, &n); err != nil {
			rows.Close()
			return OEEReport{}, fmt.Errorf("scan oee metrics: %w", err)
		}
		if i, ok := dayIdx[day.Format("2006-01-02")]; ok {
			c := &cells[id][i]
			c.outSum, c.samples = sum, n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return OEEReport{}, fmt.Errorf("oee metrics rows: %w", err)
	}

	srows, err := s.pool.Query(ctx, `
		SELECT machine_id, (occurred_at AT TIME ZONE $4)::date, COALESCE(SUM(count), 0)
		FROM scrap_records
		WHERE machine_id = ANY($1) AND occurred_at >= $2 AND occurred_at < $3
		GROUP BY machine_id, 2`, ids, rangeFrom, rangeTo, loc.String())
	if err != nil {
		return OEEReport{}, fmt.Errorf("oee scrap: %w", err)
	}
	for srows.Next() {
		var id string
		var day time.Time
		var n int64
		if err := srows.Scan(&id, &day, &n); err != nil {
			srows.Close()
			return OEEReport{}, fmt.Errorf("scan oee scrap: %w", err)
		}
		if i, ok := dayIdx[day.Format("2006-01-02")]; ok {
			cells[id][i].scrap = float64(n)
		}
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return OEEReport{}, fmt.Errorf("oee scrap rows: %w", err)
	}

	drows, err := s.pool.Query(ctx, `SELECT machine_id, start_time, end_time FROM downtimes
		WHERE machine_id = ANY($1) AND start_time < $3 AND (end_time IS NULL OR end_time > $2)`,
		ids, rangeFrom, rangeTo)
	if err != nil {
		return OEEReport{}, fmt.Errorf("oee downtimes: %w", err)
	}
	for drows.Next() {
		var id string
		var start time.Time
		var end *time.Time
		if err := drows.Scan(&id, &start, &end); err != nil {
			drows.Close()
			return OEEReport{}, fmt.Errorf("scan oee downtimes: %w", err)
		}
		endAt := now // still-open downtime runs until now
		if end != nil {
			endAt = *end
		}
		for i, d := range days {
			cells[id][i].downMin += overlapMinutes([]downtimeInterval{{start, endAt}}, d.from, d.to)
		}
	}
	drows.Close()
	if err := drows.Err(); err != nil {
		return OEEReport{}, fmt.Errorf("oee downtimes rows: %w", err)
	}

	var total oeeAcc
	dayAccs := make([]oeeAcc, len(days))
	for _, m := range machines {
		var macc oeeAcc
		var outSum float64
		var samples int
		for i, c := range cells[m.ID] {
			a := c.acc(m.RatedOutput)
			macc.add(a)
			dayAccs[i].add(a)
			outSum += c.outSum
			samples += c.samples
		}
		total.add(macc)
		row := OEEMachineRow{MachineID: m.ID, MachineName: m.Name, RatedOutput: m.RatedOutput, OEEFigures: macc.figures()}
		if samples > 0 {
			row.AvgOutput = round1(outSum / float64(samples))
		}
		report.Machines = append(report.Machines, row)
	}
	// Worst first: the point of the report is to find where output is lost.
	sort.SliceStable(report.Machines, func(i, j int) bool { return report.Machines[i].OEE < report.Machines[j].OEE })
	report.Summary = total.figures()
	for i, d := range days {
		report.Days = append(report.Days, OEEDayPoint{Date: d.date, OEEFigures: dayAccs[i].figures()})
	}
	return report, nil
}

// ── scrap + rated output store methods ───────────────────────────────

func newScrapID() string {
	b := make([]byte, 6)
	_, _ = crand.Read(b)
	return "scrap-" + hex.EncodeToString(b)
}

func (s *Store) AddScrap(machineID string, count int, reason string, at time.Time, userID string) (*ScrapRecord, error) {
	rec := ScrapRecord{ID: newScrapID(), MachineID: machineID, Count: count, Reason: reason, OccurredAt: at, RecordedBy: userID}
	_, err := s.pool.Exec(context.Background(), `INSERT INTO scrap_records (id, machine_id, count, reason, occurred_at, recorded_by)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''))`, rec.ID, machineID, count, reason, at, userID)
	if err != nil {
		return nil, fmt.Errorf("add scrap: %w", err)
	}
	return &rec, nil
}

func (s *Store) ScrapRecords(machineID string, limit int) ([]ScrapRecord, error) {
	rows, err := s.pool.Query(context.Background(), `
		SELECT r.id, r.machine_id, r.count, r.reason, r.occurred_at, COALESCE(r.recorded_by, ''), COALESCE(u.name, '')
		FROM scrap_records r LEFT JOIN users u ON u.id = r.recorded_by
		WHERE r.machine_id = $1 ORDER BY r.occurred_at DESC LIMIT $2`, machineID, limit)
	if err != nil {
		return nil, fmt.Errorf("query scrap: %w", err)
	}
	defer rows.Close()
	out := make([]ScrapRecord, 0)
	for rows.Next() {
		var r ScrapRecord
		if err := rows.Scan(&r.ID, &r.MachineID, &r.Count, &r.Reason, &r.OccurredAt, &r.RecordedBy, &r.RecordedByName); err != nil {
			return nil, fmt.Errorf("scan scrap: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteScrap removes one record; pgx.ErrNoRows if it doesn't exist.
func (s *Store) DeleteScrap(id string) error {
	tag, err := s.pool.Exec(context.Background(), `DELETE FROM scrap_records WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete scrap: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// SetRatedOutput updates a machine's ideal output (parts/hour).
func (s *Store) SetRatedOutput(id string, ratedOutput float64) (*Machine, error) {
	tag, err := s.pool.Exec(context.Background(), `UPDATE machines SET rated_output=$2 WHERE id=$1`, id, ratedOutput)
	if err != nil {
		return nil, fmt.Errorf("set rated output: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, pgx.ErrNoRows
	}
	return s.Machine(id)
}

// ── handlers ─────────────────────────────────────────────────────────

func (s *Server) handleOEEReport(w http.ResponseWriter, r *http.Request) {
	loc := shiftLocation()
	now := time.Now().In(loc)
	fromDay, toDay, ok := dayRangeFromQuery(w, r, loc, now)
	if !ok {
		return
	}
	var requested []string
	if v := r.URL.Query().Get("machineIds"); v != "" {
		requested = strings.Split(v, ",")
	}
	all, err := s.store.Machines()
	if err != nil {
		writeDBError(w, "oee machines", err)
		return
	}
	machines := scopedMachineSelection(userFromContext(r), all, requested)
	report, err := s.store.GenerateOEEReport(fromDay, toDay, machines, loc, now)
	if err != nil {
		writeDBError(w, "generate oee report", err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

const (
	maxScrapCount     = 100000
	maxScrapReasonLen = 200
	maxScrapAge       = 31 * 24 * time.Hour
)

type addScrapRequest struct {
	Count      int        `json:"count"`
	Reason     string     `json:"reason"`
	OccurredAt *time.Time `json:"occurredAt"`
}

func (s *Server) handleAddScrap(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user := userFromContext(r)
	if !canAccessMachine(user, id) {
		writeForbiddenMachine(w)
		return
	}
	var req addScrapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Некорректный запрос")
		return
	}
	if req.Count < 1 || req.Count > maxScrapCount {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Количество брака должно быть от 1 до %d", maxScrapCount))
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if utf8.RuneCountInString(reason) > maxScrapReasonLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Причина — не длиннее %d символов", maxScrapReasonLen))
		return
	}
	now := time.Now()
	at := now
	if req.OccurredAt != nil {
		at = *req.OccurredAt
	}
	if at.After(now.Add(5 * time.Minute)) {
		writeError(w, http.StatusBadRequest, "Время брака не может быть в будущем")
		return
	}
	if at.Before(now.Add(-maxScrapAge)) {
		writeError(w, http.StatusBadRequest, "Нельзя вносить брак старше 31 дня")
		return
	}
	machine, err := s.store.Machine(id)
	if err != nil {
		writeDBError(w, "scrap: get machine", err)
		return
	}
	if machine == nil {
		writeError(w, http.StatusNotFound, "Станок не найден")
		return
	}
	uid := ""
	if user != nil {
		uid = user.ID
	}
	rec, err := s.store.AddScrap(id, req.Count, reason, at, uid)
	if err != nil {
		writeDBError(w, "add scrap", err)
		return
	}
	rec.RecordedByName = ""
	if user != nil {
		rec.RecordedByName = user.Name
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) handleMachineScrap(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !canAccessMachine(userFromContext(r), id) {
		writeForbiddenMachine(w)
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	recs, err := s.store.ScrapRecords(id, limit)
	if err != nil {
		writeDBError(w, "list scrap", err)
		return
	}
	writeJSON(w, http.StatusOK, recs)
}

func (s *Server) handleDeleteScrap(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteScrap(r.PathValue("id")); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Запись не найдена")
			return
		}
		writeDBError(w, "delete scrap", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type updateRatedOutputRequest struct {
	RatedOutput float64 `json:"ratedOutput"`
}

func (s *Server) handleUpdateRatedOutput(w http.ResponseWriter, r *http.Request) {
	var req updateRatedOutputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Некорректный запрос")
		return
	}
	if !(req.RatedOutput > 0 && req.RatedOutput <= 1000) {
		writeError(w, http.StatusBadRequest, "Номинальная выработка должна быть больше 0 и не больше 1000 дет./ч")
		return
	}
	m, err := s.store.SetRatedOutput(r.PathValue("id"), req.RatedOutput)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Станок не найден")
			return
		}
		writeDBError(w, "update rated output", err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}
