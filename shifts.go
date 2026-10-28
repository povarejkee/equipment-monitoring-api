package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // the alpine runtime image ships no zoneinfo
)

// maxShiftReportDays bounds the range so one request can't fan out into
// an unbounded number of windows.
const maxShiftReportDays = 31

// shiftDef is one of the three fixed 8-hour shifts of a calendar day.
type shiftDef struct {
	Key       string
	Label     string
	StartHour int
}

// Chronological within a calendar day.
var shiftDefs = []shiftDef{
	{"night", "Ночная (00:00–08:00)", 0},
	{"day", "Дневная (08:00–16:00)", 8},
	{"evening", "Вечерняя (16:00–24:00)", 16},
}

const shiftHours = 8

type ShiftMachineRow struct {
	MachineID     string  `json:"machineId"`
	MachineName   string  `json:"machineName"`
	TotalOutput   float64 `json:"totalOutput"`
	UptimePercent float64 `json:"uptimePercent"`
	DowntimeHours float64 `json:"downtimeHours"`
	ErrorCount    int     `json:"errorCount"`
	AvgLoad       float64 `json:"avgLoad"`
}

type ShiftReportRow struct {
	Date          string            `json:"date"`
	Shift         string            `json:"shift"`
	Label         string            `json:"label"`
	From          time.Time         `json:"from"`
	To            time.Time         `json:"to"`
	InProgress    bool              `json:"inProgress"`
	TotalOutput   float64           `json:"totalOutput"`
	UptimePercent float64           `json:"uptimePercent"`
	DowntimeHours float64           `json:"downtimeHours"`
	ErrorCount    int               `json:"errorCount"`
	AvgLoad       float64           `json:"avgLoad"`
	Machines      []ShiftMachineRow `json:"machines"`
}

type ShiftReport struct {
	GeneratedAt time.Time        `json:"generatedAt"`
	DateFrom    string           `json:"dateFrom"`
	DateTo      string           `json:"dateTo"`
	Timezone    string           `json:"timezone"`
	Shifts      []ShiftReportRow `json:"shifts"`
}

// shiftLocation returns the plant's timezone (SHIFT_TIMEZONE, default
// Europe/Moscow) — shift boundaries are wall-clock hours, so they must
// not follow the server's UTC clock.
func shiftLocation() *time.Location {
	name := os.Getenv("SHIFT_TIMEZONE")
	if name == "" {
		name = "Europe/Moscow"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.FixedZone("MSK", 3*3600)
	}
	return loc
}

type shiftWindow struct {
	date       string
	def        shiftDef
	from, to   time.Time
	inProgress bool
}

// shiftWindows lists every shift of the inclusive day range [fromDay,
// toDay] that has started by `now`, oldest first. Windows still running
// are clipped to `now` and flagged.
func shiftWindows(fromDay, toDay time.Time, loc *time.Location, now time.Time) []shiftWindow {
	var out []shiftWindow
	for day := fromDay; !day.After(toDay); day = day.AddDate(0, 0, 1) {
		for _, def := range shiftDefs {
			start := time.Date(day.Year(), day.Month(), day.Day(), def.StartHour, 0, 0, 0, loc)
			end := start.Add(shiftHours * time.Hour)
			if !start.Before(now) {
				continue
			}
			w := shiftWindow{date: day.Format("2006-01-02"), def: def, from: start, to: end}
			if end.After(now) {
				w.to = now
				w.inProgress = true
			}
			out = append(out, w)
		}
	}
	return out
}

type shiftKey struct {
	machine string
	idx     int
}

// GenerateShiftReport aggregates output, availability, errors and load per
// shift and machine over the inclusive day range, for the given machines.
func (s *Store) GenerateShiftReport(fromDay, toDay time.Time, machines []*Machine, loc *time.Location, now time.Time) (ShiftReport, error) {
	ctx := context.Background()
	windows := shiftWindows(fromDay, toDay, loc, now)
	report := ShiftReport{
		GeneratedAt: now,
		DateFrom:    fromDay.Format("2006-01-02"),
		DateTo:      toDay.Format("2006-01-02"),
		Timezone:    loc.String(),
		Shifts:      make([]ShiftReportRow, 0, len(windows)),
	}
	if len(windows) == 0 || len(machines) == 0 {
		for _, w := range windows {
			report.Shifts = append(report.Shifts, ShiftReportRow{
				Date: w.date, Shift: w.def.Key, Label: w.def.Label, From: w.from, To: w.to,
				InProgress: w.inProgress, UptimePercent: 100, Machines: []ShiftMachineRow{},
			})
		}
		return report, nil
	}

	ids := make([]string, len(machines))
	for i, m := range machines {
		ids[i] = m.ID
	}
	rangeFrom, rangeTo := windows[0].from, windows[len(windows)-1].to

	// Output and load, bucketed into shifts in SQL by the same wall-clock
	// arithmetic shiftWindows uses (hour-of-day in the plant's timezone).
	type metricAgg struct {
		output, loadSum float64
		samples         int
	}
	metrics := make(map[shiftKey]*metricAgg)
	rows, err := s.pool.Query(ctx, `
		SELECT machine_id,
			(timestamp AT TIME ZONE $4)::date AS day,
			floor(extract(hour FROM (timestamp AT TIME ZONE $4)) / 8)::int AS shift_idx,
			COALESCE(SUM(output), 0), COALESCE(SUM(load), 0), COUNT(*)
		FROM metric_history
		WHERE machine_id = ANY($1) AND timestamp >= $2 AND timestamp < $3
		GROUP BY machine_id, day, shift_idx`, ids, rangeFrom, rangeTo, loc.String())
	if err != nil {
		return ShiftReport{}, fmt.Errorf("shift report metrics: %w", err)
	}
	dayIndex := make(map[string]int) // date+shift -> window index
	for i, w := range windows {
		dayIndex[w.date+"/"+w.def.Key] = i
	}
	for rows.Next() {
		var id string
		var day time.Time
		var idx int
		var a metricAgg
		if err := rows.Scan(&id, &day, &idx, &a.output, &a.loadSum, &a.samples); err != nil {
			rows.Close()
			return ShiftReport{}, fmt.Errorf("scan shift metrics: %w", err)
		}
		if idx < 0 || idx >= len(shiftDefs) {
			continue
		}
		if wi, ok := dayIndex[day.Format("2006-01-02")+"/"+shiftDefs[idx].Key]; ok {
			metrics[shiftKey{id, wi}] = &metricAgg{a.output, a.loadSum, a.samples}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ShiftReport{}, fmt.Errorf("shift metrics rows: %w", err)
	}

	errCounts := make(map[shiftKey]int)
	erows, err := s.pool.Query(ctx, `SELECT machine_id, timestamp FROM error_log
		WHERE machine_id = ANY($1) AND timestamp >= $2 AND timestamp < $3`, ids, rangeFrom, rangeTo)
	if err != nil {
		return ShiftReport{}, fmt.Errorf("shift report errors: %w", err)
	}
	for erows.Next() {
		var id string
		var ts time.Time
		if err := erows.Scan(&id, &ts); err != nil {
			erows.Close()
			return ShiftReport{}, fmt.Errorf("scan shift errors: %w", err)
		}
		for wi, w := range windows {
			if !ts.Before(w.from) && ts.Before(w.to) {
				errCounts[shiftKey{id, wi}]++
				break
			}
		}
	}
	erows.Close()
	if err := erows.Err(); err != nil {
		return ShiftReport{}, fmt.Errorf("shift errors rows: %w", err)
	}

	type dt struct {
		machine    string
		start, end time.Time
	}
	var downs []dt
	drows, err := s.pool.Query(ctx, `SELECT machine_id, start_time, end_time FROM downtimes
		WHERE machine_id = ANY($1) AND start_time < $3 AND (end_time IS NULL OR end_time > $2)`,
		ids, rangeFrom, rangeTo)
	if err != nil {
		return ShiftReport{}, fmt.Errorf("shift report downtimes: %w", err)
	}
	for drows.Next() {
		var d dt
		var end *time.Time
		if err := drows.Scan(&d.machine, &d.start, &end); err != nil {
			drows.Close()
			return ShiftReport{}, fmt.Errorf("scan shift downtimes: %w", err)
		}
		d.end = now // a still-open downtime runs until now
		if end != nil {
			d.end = *end
		}
		downs = append(downs, d)
	}
	drows.Close()
	if err := drows.Err(); err != nil {
		return ShiftReport{}, fmt.Errorf("shift downtimes rows: %w", err)
	}
	downMinutes := make(map[shiftKey]float64)
	for _, d := range downs {
		for wi, w := range windows {
			downMinutes[shiftKey{d.machine, wi}] += overlapMinutes([]downtimeInterval{{d.start, d.end}}, w.from, w.to)
		}
	}

	for wi, w := range windows {
		row := ShiftReportRow{
			Date: w.date, Shift: w.def.Key, Label: w.def.Label, From: w.from, To: w.to,
			InProgress: w.inProgress, Machines: make([]ShiftMachineRow, 0, len(machines)),
		}
		windowMinutes := w.to.Sub(w.from).Minutes()
		var loadSum float64
		var samples int
		var downTotal float64
		for _, m := range machines {
			k := shiftKey{m.ID, wi}
			mr := ShiftMachineRow{MachineID: m.ID, MachineName: m.Name, ErrorCount: errCounts[k]}
			if a := metrics[k]; a != nil {
				mr.TotalOutput = math.Round(a.output)
				if a.samples > 0 {
					mr.AvgLoad = round1(a.loadSum / float64(a.samples))
				}
				loadSum += a.loadSum
				samples += a.samples
			}
			down := math.Min(downMinutes[k], windowMinutes)
			mr.DowntimeHours = round1(down / 60)
			mr.UptimePercent = 100
			if windowMinutes > 0 {
				mr.UptimePercent = round1(clamp(100*(1-down/windowMinutes), 0, 100))
			}
			downTotal += down
			row.TotalOutput += mr.TotalOutput
			row.ErrorCount += mr.ErrorCount
			row.Machines = append(row.Machines, mr)
		}
		sort.Slice(row.Machines, func(i, j int) bool { return row.Machines[i].MachineName < row.Machines[j].MachineName })
		row.DowntimeHours = round1(downTotal / 60)
		row.UptimePercent = 100
		if fleetMinutes := windowMinutes * float64(len(machines)); fleetMinutes > 0 {
			row.UptimePercent = round1(clamp(100*(1-downTotal/fleetMinutes), 0, 100))
		}
		if samples > 0 {
			row.AvgLoad = round1(loadSum / float64(samples))
		}
		report.Shifts = append(report.Shifts, row)
	}
	return report, nil
}

// scopedMachineSelection resolves the machines a report covers: the
// requested ids (all machines when none are requested), narrowed to what
// the caller may see.
func scopedMachineSelection(user *User, all []*Machine, requested []string) []*Machine {
	out := make([]*Machine, 0, len(all))
	for _, m := range scopedMachines(user, all) {
		if len(requested) == 0 || contains(requested, m.ID) {
			out = append(out, m)
		}
	}
	return out
}

// dayRangeFromQuery parses dateFrom/dateTo (YYYY-MM-DD, plant timezone;
// both default to today) and enforces the range limits. On failure it has
// already written the 400 and returns ok=false.
func dayRangeFromQuery(w http.ResponseWriter, r *http.Request, loc *time.Location, now time.Time) (fromDay, toDay time.Time, ok bool) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	parseDay := func(key string, def time.Time) (time.Time, bool) {
		v := r.URL.Query().Get(key)
		if v == "" {
			return def, true
		}
		t, err := time.ParseInLocation("2006-01-02", v, loc)
		return t, err == nil
	}
	toDay, ok1 := parseDay("dateTo", today)
	fromDay, ok2 := parseDay("dateFrom", toDay)
	if !ok1 || !ok2 {
		writeError(w, http.StatusBadRequest, "Даты должны быть в формате ГГГГ-ММ-ДД")
		return time.Time{}, time.Time{}, false
	}
	if fromDay.After(toDay) {
		writeError(w, http.StatusBadRequest, "Начальная дата позже конечной")
		return time.Time{}, time.Time{}, false
	}
	if days := int(toDay.Sub(fromDay).Hours()/24) + 1; days > maxShiftReportDays {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Максимальный период — %d дней", maxShiftReportDays))
		return time.Time{}, time.Time{}, false
	}
	return fromDay, toDay, true
}

func (s *Server) handleShiftReport(w http.ResponseWriter, r *http.Request) {
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
		writeDBError(w, "shift report machines", err)
		return
	}
	machines := scopedMachineSelection(userFromContext(r), all, requested)

	report, err := s.store.GenerateShiftReport(fromDay, toDay, machines, loc, now)
	if err != nil {
		writeDBError(w, "generate shift report", err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}
