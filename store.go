package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the PostgreSQL-backed data layer. Demo data is no longer
// generated here at startup — see cmd/seed for that; this only reads and
// writes the DB. rng is kept for the parts that still simulate readings
// that don't exist in the real world yet: Tick() (no real sensors) and
// GenerateReport/buildTimeSeries's placeholder numbers (real aggregation
// lands separately). rngMu guards it since Tick() runs on a ticker
// goroutine concurrently with HTTP handlers.
type Store struct {
	pool  *pgxpool.Pool
	rng   *rand.Rand
	rngMu sync.Mutex
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{
		pool: pool,
		rng:  rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

const machineColumns = `id, name, type, location, status, assigned_operator,
	temperature, load, output, uptime, power_consumption, spindle_speed, vibration, last_updated`

func scanMachine(row pgx.Row) (*Machine, error) {
	var m Machine
	var assignedOperator *string
	err := row.Scan(
		&m.ID, &m.Name, &m.Type, &m.Location, &m.Status, &assignedOperator,
		&m.Metrics.Temperature, &m.Metrics.Load, &m.Metrics.Output, &m.Metrics.Uptime, &m.Metrics.PowerConsumption,
		&m.Metrics.SpindleSpeed, &m.Metrics.Vibration, &m.LastUpdated,
	)
	if err != nil {
		return nil, err
	}
	if assignedOperator != nil {
		m.AssignedOperator = *assignedOperator
	}
	return &m, nil
}

// ── Machines ─────────────────────────────────────────────────────────

func (s *Store) Machines() []*Machine {
	ctx := context.Background()
	// length-then-id sort keeps demo IDs like m1..m12 in numeric order
	// instead of lexicographic (m1, m10, m11, ...).
	rows, err := s.pool.Query(ctx, `SELECT `+machineColumns+` FROM machines ORDER BY length(id), id`)
	if err != nil {
		logger.Error("query machines", "error", err)
		return nil
	}
	defer rows.Close()

	out := make([]*Machine, 0)
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			logger.Error("scan machine", "error", err)
			continue
		}
		out = append(out, m)
	}
	return out
}

func (s *Store) Machine(id string) *Machine {
	row := s.pool.QueryRow(context.Background(), `SELECT `+machineColumns+` FROM machines WHERE id=$1`, id)
	m, err := scanMachine(row)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logger.Error("query machine", "error", err, "id", id)
		}
		return nil
	}
	return m
}

// Tick advances live metrics for all active machines and records a
// metric_history point for each, then returns the fresh snapshot for
// broadcasting over WebSocket. Machines that are offline/under maintenance
// are left untouched but still included in the returned snapshot.
func (s *Store) Tick() []*Machine {
	ctx := context.Background()
	machines := s.Machines()
	if len(machines) == 0 {
		return machines
	}

	s.rngMu.Lock()
	defer s.rngMu.Unlock()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		logger.Error("tick: begin tx", "error", err)
		return machines
	}
	defer tx.Rollback(ctx) // no-op once committed

	now := time.Now()
	for _, m := range machines {
		if m.Status == StatusOffline || m.Status == StatusMaintenance {
			continue
		}
		m.Metrics.Temperature = fluctuate(s.rng, m.Metrics.Temperature, 0.5, 20, 98)
		m.Metrics.Load = fluctuate(s.rng, m.Metrics.Load, 1, 0, 100)
		m.Metrics.Output = math.Max(0, math.Round(fluctuate(s.rng, m.Metrics.Output, 1, 0, 50)))
		m.Metrics.PowerConsumption = fluctuate(s.rng, m.Metrics.PowerConsumption, 0.3, 5, 50)
		if m.Metrics.Vibration != nil {
			v := fluctuate(s.rng, *m.Metrics.Vibration, 0.1, 0, 12)
			m.Metrics.Vibration = &v
		}
		if m.Metrics.SpindleSpeed != nil {
			v := fluctuate(s.rng, *m.Metrics.SpindleSpeed, 50, 500, 3000)
			m.Metrics.SpindleSpeed = &v
		}
		m.LastUpdated = now
		s.updateStatus(m)

		_, err := tx.Exec(ctx, `UPDATE machines SET status=$2, temperature=$3, load=$4, output=$5,
			power_consumption=$6, spindle_speed=$7, vibration=$8, last_updated=$9 WHERE id=$1`,
			m.ID, m.Status, m.Metrics.Temperature, m.Metrics.Load, m.Metrics.Output,
			m.Metrics.PowerConsumption, m.Metrics.SpindleSpeed, m.Metrics.Vibration, m.LastUpdated)
		if err != nil {
			logger.Error("tick: update machine", "error", err, "id", m.ID)
			continue
		}

		vib := 0.0
		if m.Metrics.Vibration != nil {
			vib = *m.Metrics.Vibration
		}
		if _, err := tx.Exec(ctx, `INSERT INTO metric_history
			(machine_id, timestamp, temperature, load, output, vibration, power_consumption)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			m.ID, now, m.Metrics.Temperature, m.Metrics.Load, m.Metrics.Output, vib, m.Metrics.PowerConsumption,
		); err != nil {
			logger.Error("tick: insert history", "error", err, "id", m.ID)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		logger.Error("tick: commit", "error", err)
	}
	return machines
}

func (s *Store) updateStatus(m *Machine) {
	t := m.Metrics.Temperature
	l := m.Metrics.Load
	var vib float64
	if m.Metrics.Vibration != nil {
		vib = *m.Metrics.Vibration
	}
	switch {
	case t > 95 || vib > 7:
		m.Status = StatusError
	case t > 80 || l > 85 || vib > 4:
		m.Status = StatusWarning
	case l < 5:
		m.Status = StatusIdle
	default:
		m.Status = StatusRunning
	}
}

// ── History ──────────────────────────────────────────────────────────

func (s *Store) History(machineID string, hours int) []MetricHistoryPoint {
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	rows, err := s.pool.Query(context.Background(), `SELECT timestamp, temperature, load, output, vibration, power_consumption
		FROM metric_history WHERE machine_id=$1 AND timestamp >= $2 ORDER BY timestamp ASC`, machineID, cutoff)
	if err != nil {
		logger.Error("query history", "error", err, "machineId", machineID)
		return nil
	}
	defer rows.Close()

	out := make([]MetricHistoryPoint, 0)
	for rows.Next() {
		var p MetricHistoryPoint
		if err := rows.Scan(&p.Timestamp, &p.Temperature, &p.Load, &p.Output, &p.Vibration, &p.PowerConsumption); err != nil {
			logger.Error("scan history point", "error", err)
			continue
		}
		out = append(out, p)
	}
	return out
}

// ── Downtimes ────────────────────────────────────────────────────────

func (s *Store) Downtimes(machineID string) []DowntimeEntry {
	rows, err := s.pool.Query(context.Background(), `SELECT id, machine_id, start_time, end_time, duration, reason
		FROM downtimes WHERE machine_id=$1 ORDER BY start_time DESC`, machineID)
	if err != nil {
		logger.Error("query downtimes", "error", err, "machineId", machineID)
		return nil
	}
	defer rows.Close()

	out := make([]DowntimeEntry, 0)
	for rows.Next() {
		var d DowntimeEntry
		if err := rows.Scan(&d.ID, &d.MachineID, &d.StartTime, &d.EndTime, &d.Duration, &d.Reason); err != nil {
			logger.Error("scan downtime", "error", err)
			continue
		}
		out = append(out, d)
	}
	return out
}

// ── Alerts ───────────────────────────────────────────────────────────

// AlertFilter narrows Alerts(); zero values mean "don't filter on this field".
type AlertFilter struct {
	MachineID    string
	Severity     AlertSeverity
	Acknowledged *bool
}

func (s *Store) Alerts(f AlertFilter) []*Alert {
	query := `SELECT id, machine_id, machine_name, type, severity, message, metric_name,
		current_value, threshold_value, timestamp, acknowledged, acknowledged_by, acknowledged_at
		FROM alerts WHERE 1=1`
	var args []interface{}
	if f.MachineID != "" {
		args = append(args, f.MachineID)
		query += fmt.Sprintf(" AND machine_id=$%d", len(args))
	}
	if f.Severity != "" {
		args = append(args, f.Severity)
		query += fmt.Sprintf(" AND severity=$%d", len(args))
	}
	if f.Acknowledged != nil {
		args = append(args, *f.Acknowledged)
		query += fmt.Sprintf(" AND acknowledged=$%d", len(args))
	}
	query += " ORDER BY timestamp DESC"

	rows, err := s.pool.Query(context.Background(), query, args...)
	if err != nil {
		logger.Error("query alerts", "error", err)
		return nil
	}
	defer rows.Close()

	out := make([]*Alert, 0)
	for rows.Next() {
		var a Alert
		var ackBy *string
		if err := rows.Scan(&a.ID, &a.MachineID, &a.MachineName, &a.Type, &a.Severity, &a.Message, &a.MetricName,
			&a.CurrentValue, &a.ThresholdValue, &a.Timestamp, &a.Acknowledged, &ackBy, &a.AcknowledgedAt); err != nil {
			logger.Error("scan alert", "error", err)
			continue
		}
		if ackBy != nil {
			a.AcknowledgedBy = *ackBy
		}
		out = append(out, &a)
	}
	return out
}

func (s *Store) AcknowledgeAlert(id, userID string) {
	_, err := s.pool.Exec(context.Background(), `UPDATE alerts SET acknowledged=true, acknowledged_by=$2, acknowledged_at=now()
		WHERE id=$1 AND acknowledged=false`, id, userID)
	if err != nil {
		logger.Error("acknowledge alert", "error", err, "id", id)
	}
}

func (s *Store) AcknowledgeAllAlerts(userID string) {
	_, err := s.pool.Exec(context.Background(), `UPDATE alerts SET acknowledged=true, acknowledged_by=$1, acknowledged_at=now()
		WHERE acknowledged=false`, userID)
	if err != nil {
		logger.Error("acknowledge all alerts", "error", err)
	}
}

// ── Thresholds ───────────────────────────────────────────────────────

func (s *Store) Thresholds() []*AlertThreshold {
	rows, err := s.pool.Query(context.Background(),
		`SELECT metric, label, unit, warning_value, critical_value FROM thresholds ORDER BY metric`)
	if err != nil {
		logger.Error("query thresholds", "error", err)
		return nil
	}
	defer rows.Close()

	out := make([]*AlertThreshold, 0)
	for rows.Next() {
		var t AlertThreshold
		if err := rows.Scan(&t.Metric, &t.Label, &t.Unit, &t.WarningValue, &t.CriticalValue); err != nil {
			logger.Error("scan threshold", "error", err)
			continue
		}
		out = append(out, &t)
	}
	return out
}

func (s *Store) UpdateThreshold(metric string, warning, critical float64) {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE thresholds SET warning_value=$2, critical_value=$3 WHERE metric=$1`, metric, warning, critical)
	if err != nil {
		logger.Error("update threshold", "error", err, "metric", metric)
	}
}

// ── Error log ────────────────────────────────────────────────────────

// ErrorLogFilter narrows ErrorLog(); zero values mean "don't filter/paginate
// on this field" (Limit == 0 returns everything after From/To/MachineID).
type ErrorLogFilter struct {
	MachineID string
	From, To  *time.Time
	Limit     int
	Offset    int
}

// ErrorLog returns entries matching f (newest-first) plus the total match
// count before pagination, so callers can expose it (e.g. as a response
// header) without a second request.
func (s *Store) ErrorLog(f ErrorLogFilter) ([]*ErrorLogEntry, int) {
	query := `SELECT id, machine_id, machine_name, error_code, error_type, description, timestamp,
		resolved_at, resolved_by, duration, impact, COUNT(*) OVER() AS total_count
		FROM error_log WHERE 1=1`
	var args []interface{}
	if f.MachineID != "" {
		args = append(args, f.MachineID)
		query += fmt.Sprintf(" AND machine_id=$%d", len(args))
	}
	if f.From != nil {
		args = append(args, *f.From)
		query += fmt.Sprintf(" AND timestamp >= $%d", len(args))
	}
	if f.To != nil {
		args = append(args, *f.To)
		query += fmt.Sprintf(" AND timestamp <= $%d", len(args))
	}
	query += " ORDER BY timestamp DESC"
	if f.Limit > 0 {
		args = append(args, f.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	if f.Offset > 0 {
		args = append(args, f.Offset)
		query += fmt.Sprintf(" OFFSET $%d", len(args))
	}

	rows, err := s.pool.Query(context.Background(), query, args...)
	if err != nil {
		logger.Error("query error log", "error", err)
		return nil, 0
	}
	defer rows.Close()

	out := make([]*ErrorLogEntry, 0)
	total := 0
	for rows.Next() {
		var e ErrorLogEntry
		var resolvedBy *string
		if err := rows.Scan(&e.ID, &e.MachineID, &e.MachineName, &e.ErrorCode, &e.ErrorType, &e.Description,
			&e.Timestamp, &e.ResolvedAt, &resolvedBy, &e.Duration, &e.Impact, &total); err != nil {
			logger.Error("scan error log entry", "error", err)
			continue
		}
		if resolvedBy != nil {
			e.ResolvedBy = *resolvedBy
		}
		out = append(out, &e)
	}
	return out, total
}

// ── Users ────────────────────────────────────────────────────────────

func (s *Store) Users() []*User {
	rows, err := s.pool.Query(context.Background(),
		`SELECT id, name, email, role, assigned_machines FROM users ORDER BY id`)
	if err != nil {
		logger.Error("query users", "error", err)
		return nil
	}
	defer rows.Close()

	out := make([]*User, 0)
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.Email, &u.Role, &u.AssignedMachines); err != nil {
			logger.Error("scan user", "error", err)
			continue
		}
		out = append(out, &u)
	}
	return out
}

// ── Reports ──────────────────────────────────────────────────────────
//
// Numbers here are still simulated (s.rng), matching pre-DB behavior — the
// swap to real aggregation from metric_history/downtimes is a separate,
// dedicated change. Only the machine list now comes from Postgres.

func (s *Store) GenerateReport(p ReportParams) ReportData {
	machines := s.Machines()
	filtered := make([]*Machine, 0)
	for _, m := range machines {
		if len(p.MachineIDs) == 0 || contains(p.MachineIDs, m.ID) {
			filtered = append(filtered, m)
		}
	}

	s.rngMu.Lock()
	breakdown := make([]MachineReportRow, 0, len(filtered))
	for _, m := range filtered {
		breakdown = append(breakdown, MachineReportRow{
			MachineID:     m.ID,
			MachineName:   m.Name,
			TotalOutput:   math.Round(200 + s.rng.Float64()*800),
			UptimePercent: round1(75 + s.rng.Float64()*20),
			DowntimeHours: round1(1 + s.rng.Float64()*8),
			ErrorCount:    int(math.Round(s.rng.Float64() * 5)),
			Efficiency:    round1(70 + s.rng.Float64()*25),
		})
	}
	timeSeries := s.buildTimeSeries(p)
	s.rngMu.Unlock()

	var totalOutput, sumUptime, totalDowntime, sumEff float64
	var totalErrors int
	for _, r := range breakdown {
		totalOutput += r.TotalOutput
		sumUptime += r.UptimePercent
		totalDowntime += r.DowntimeHours
		totalErrors += r.ErrorCount
		sumEff += r.Efficiency
	}
	n := float64(len(breakdown))
	if n == 0 {
		n = 1
	}
	return ReportData{
		GeneratedAt: time.Now(),
		Params:      p,
		Summary: ReportSummary{
			TotalOutput:   totalOutput,
			AvgUptime:     round1(sumUptime / n),
			TotalDowntime: round1(totalDowntime),
			TotalErrors:   totalErrors,
			Efficiency:    round1(sumEff / n),
		},
		TimeSeries:       timeSeries,
		MachineBreakdown: breakdown,
	}
}

// buildTimeSeries must be called with rngMu already held.
func (s *Store) buildTimeSeries(p ReportParams) []TimeSeriesPoint {
	step := stepDuration(p.GroupBy)
	points := make([]TimeSeriesPoint, 0)
	for t := p.DateFrom; !t.After(p.DateTo); t = t.Add(step) {
		points = append(points, TimeSeriesPoint{
			Timestamp:      t,
			Output:         math.Round(100 + s.rng.Float64()*300),
			Uptime:         round1(70 + s.rng.Float64()*25),
			AvgTemperature: round1(50 + s.rng.Float64()*30),
			AvgLoad:        round1(40 + s.rng.Float64()*40),
			ErrorCount:     s.rng.Intn(4),
		})
	}
	return points
}

func stepDuration(groupBy string) time.Duration {
	switch groupBy {
	case "hour":
		return time.Hour
	case "week":
		return 7 * 24 * time.Hour
	case "month":
		return 30 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// ── helpers ──────────────────────────────────────────────────────────

func fluctuate(rng *rand.Rand, val, delta, min, max float64) float64 {
	change := (rng.Float64() - 0.5) * 2 * delta
	return round1(math.Min(max, math.Max(min, val+change)))
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
