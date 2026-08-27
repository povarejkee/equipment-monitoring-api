package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the PostgreSQL-backed data layer. Demo data is no longer
// generated here at startup — see cmd/seed for that; this only reads and
// writes the DB. rng simulates readings that don't exist in the real
// world (no real sensors): only Tick() uses it now. rngMu guards it since
// Tick() runs on a ticker goroutine concurrently with HTTP handlers.
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

// Machines returns every machine. A query failure is returned rather than
// swallowed: for a monitoring product, "the database is down" must not
// render as "no machines".
func (s *Store) Machines() ([]*Machine, error) {
	// length-then-id sort keeps demo IDs like m1..m12 in numeric order
	// instead of lexicographic (m1, m10, m11, ...).
	rows, err := s.pool.Query(context.Background(), `SELECT `+machineColumns+` FROM machines ORDER BY length(id), id`)
	if err != nil {
		return nil, fmt.Errorf("query machines: %w", err)
	}
	defer rows.Close()

	out := make([]*Machine, 0)
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, fmt.Errorf("scan machine: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Machine returns one machine, or (nil, nil) if no such id exists.
func (s *Store) Machine(id string) (*Machine, error) {
	row := s.pool.QueryRow(context.Background(), `SELECT `+machineColumns+` FROM machines WHERE id=$1`, id)
	m, err := scanMachine(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query machine %s: %w", id, err)
	}
	return m, nil
}

// Tick advances live metrics for all active machines and records a
// metric_history point for each, then returns the fresh snapshot for
// broadcasting over WebSocket. Machines that are offline/under maintenance
// are left untouched but still included in the returned snapshot. Returns
// an error (and no machines) if the DB can't be reached at all — the
// caller must NOT broadcast a nil/empty payload in that case, since that
// would overwrite every connected client's good data with nothing.
func (s *Store) Tick() ([]*Machine, error) {
	ctx := context.Background()
	machines, err := s.Machines()
	if err != nil {
		return nil, fmt.Errorf("tick: %w", err)
	}
	if len(machines) == 0 {
		return machines, nil
	}

	// Read fresh each tick (not cached) so an admin's PUT /api/thresholds
	// takes effect on the next tick, not after a restart.
	thresholds, err := s.Thresholds()
	if err != nil {
		logger.Error("tick: query thresholds", "error", err)
		thresholds = nil // degrade to no threshold alerting rather than failing the whole tick
	}

	s.rngMu.Lock()
	defer s.rngMu.Unlock()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		logger.Error("tick: begin tx", "error", err)
		return machines, nil
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

		s.checkThresholdAlerts(ctx, tx, m, thresholds)
	}

	if err := tx.Commit(ctx); err != nil {
		logger.Error("tick: commit", "error", err)
	}
	return machines, nil
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

// checkThresholdAlerts compares m's freshly-ticked metrics against the
// current alert thresholds and opens a new alert for any metric that has
// crossed into warning/critical territory — this is the only source of
// alerts besides the initial seed. Previously nothing created alerts at
// runtime at all: the alerts list was frozen at whatever the seed script
// wrote, so a station could overheat for hours and the UI would never
// show a new alert.
//
// Hysteresis: a metric only generates a new alert if it doesn't already
// have an open (unacknowledged) alert for that machine+metric. Once
// acknowledged, a still-out-of-range metric will alert again on the next
// tick — that's intentional (re-alert after ack if the problem persists),
// not spam from a value oscillating around the boundary.
//
// Scope: only the three metrics with real thresholds (temperature, load,
// vibration) are covered. machine_down/machine_offline/maintenance_due/
// anomaly_detected remain seed-only for now — those need a different
// signal (connectivity loss, scheduled maintenance, statistical outlier
// detection) than a simple threshold crossing.
func (s *Store) checkThresholdAlerts(ctx context.Context, tx pgx.Tx, m *Machine, thresholds []*AlertThreshold) {
	for _, th := range thresholds {
		value, ok := thresholdMetricValue(m, th.Metric)
		if !ok {
			continue
		}

		var severity AlertSeverity
		var thresholdValue float64
		switch {
		case value >= th.CriticalValue:
			severity, thresholdValue = SeverityCritical, th.CriticalValue
		case value >= th.WarningValue:
			severity, thresholdValue = SeverityWarning, th.WarningValue
		default:
			continue // within normal range
		}

		var alreadyOpen bool
		err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM alerts WHERE machine_id=$1 AND metric_name=$2 AND acknowledged=false)`,
			m.ID, th.Metric,
		).Scan(&alreadyOpen)
		if err != nil {
			logger.Error("tick: check open alert", "error", err, "machineId", m.ID, "metric", th.Metric)
			continue
		}
		if alreadyOpen {
			continue
		}

		message := fmt.Sprintf("%s: %s превысила %s порог (%.1f %s)",
			m.Name, th.Label, severityRuLabel(severity), value, th.Unit)
		_, err = tx.Exec(ctx, `INSERT INTO alerts
			(id, machine_id, machine_name, type, severity, message, metric_name, current_value, threshold_value, timestamp, acknowledged)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,false)`,
			newAlertID(), m.ID, m.Name, AlertThresholdExceeded, severity, message, th.Metric, value, thresholdValue, time.Now())
		if err != nil {
			logger.Error("tick: insert alert", "error", err, "machineId", m.ID, "metric", th.Metric)
		}
	}
}

func thresholdMetricValue(m *Machine, metric string) (float64, bool) {
	switch metric {
	case "temperature":
		return m.Metrics.Temperature, true
	case "load":
		return m.Metrics.Load, true
	case "vibration":
		if m.Metrics.Vibration == nil {
			return 0, false
		}
		return *m.Metrics.Vibration, true
	default:
		return 0, false
	}
}

func severityRuLabel(sev AlertSeverity) string {
	if sev == SeverityCritical {
		return "критический"
	}
	return "предупредительный"
}

func newAlertID() string {
	b := make([]byte, 6)
	_, _ = crand.Read(b)
	return "alert-" + hex.EncodeToString(b)
}

// ── History ──────────────────────────────────────────────────────────

// metricHistoryRetention bounds how long raw metric_history rows are kept.
// Tick() inserts one row per active machine every 4s — at 10 active
// machines that's ~216,000 rows/day, ~500MB/month. Render's free Postgres
// plan caps out at 1GB, so without a retention policy the table alone
// would exhaust it in roughly two months. 35 days (a little past the 30
// days most reports/history views actually query) caps growth at a
// steady-state size instead of letting it grow forever.
const metricHistoryRetention = 35 * 24 * time.Hour

// PruneMetricHistory deletes metric_history rows older than the retention
// window. Intended to run periodically (see main.go), not on every tick —
// pruning is a maintenance operation, not part of the hot path.
func (s *Store) PruneMetricHistory(ctx context.Context) (int64, error) {
	cutoff := time.Now().Add(-metricHistoryRetention)
	tag, err := s.pool.Exec(ctx, `DELETE FROM metric_history WHERE timestamp < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune metric_history: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *Store) History(machineID string, hours int) ([]MetricHistoryPoint, error) {
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	rows, err := s.pool.Query(context.Background(), `SELECT timestamp, temperature, load, output, vibration, power_consumption
		FROM metric_history WHERE machine_id=$1 AND timestamp >= $2 ORDER BY timestamp ASC`, machineID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query history for %s: %w", machineID, err)
	}
	defer rows.Close()

	out := make([]MetricHistoryPoint, 0)
	for rows.Next() {
		var p MetricHistoryPoint
		if err := rows.Scan(&p.Timestamp, &p.Temperature, &p.Load, &p.Output, &p.Vibration, &p.PowerConsumption); err != nil {
			return nil, fmt.Errorf("scan history point: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ── Downtimes ────────────────────────────────────────────────────────

func (s *Store) Downtimes(machineID string) ([]DowntimeEntry, error) {
	rows, err := s.pool.Query(context.Background(), `SELECT id, machine_id, start_time, end_time, duration, reason
		FROM downtimes WHERE machine_id=$1 ORDER BY start_time DESC`, machineID)
	if err != nil {
		return nil, fmt.Errorf("query downtimes for %s: %w", machineID, err)
	}
	defer rows.Close()

	out := make([]DowntimeEntry, 0)
	for rows.Next() {
		var d DowntimeEntry
		if err := rows.Scan(&d.ID, &d.MachineID, &d.StartTime, &d.EndTime, &d.Duration, &d.Reason); err != nil {
			return nil, fmt.Errorf("scan downtime: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ── Alerts ───────────────────────────────────────────────────────────

// AlertFilter narrows Alerts(); zero values mean "don't filter on this field".
type AlertFilter struct {
	MachineID    string
	Severity     AlertSeverity
	Acknowledged *bool
}

func (s *Store) Alerts(f AlertFilter) ([]*Alert, error) {
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
		return nil, fmt.Errorf("query alerts: %w", err)
	}
	defer rows.Close()

	out := make([]*Alert, 0)
	for rows.Next() {
		var a Alert
		var ackBy *string
		if err := rows.Scan(&a.ID, &a.MachineID, &a.MachineName, &a.Type, &a.Severity, &a.Message, &a.MetricName,
			&a.CurrentValue, &a.ThresholdValue, &a.Timestamp, &a.Acknowledged, &ackBy, &a.AcknowledgedAt); err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		if ackBy != nil {
			a.AcknowledgedBy = *ackBy
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// AcknowledgeAlert marks one alert acknowledged. Returns pgx.ErrNoRows if
// the id doesn't exist or was already acknowledged, so the caller can 404
// instead of silently reporting success either way.
func (s *Store) AcknowledgeAlert(id, userID string) error {
	tag, err := s.pool.Exec(context.Background(), `UPDATE alerts SET acknowledged=true, acknowledged_by=$2, acknowledged_at=now()
		WHERE id=$1 AND acknowledged=false`, id, userID)
	if err != nil {
		return fmt.Errorf("acknowledge alert %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		// Distinguish "doesn't exist" from "already acknowledged" for a
		// clearer error message.
		var exists bool
		if err := s.pool.QueryRow(context.Background(),
			`SELECT EXISTS(SELECT 1 FROM alerts WHERE id=$1)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("check alert existence %s: %w", id, err)
		}
		if !exists {
			return pgx.ErrNoRows
		}
		// Already acknowledged — not an error, just a no-op.
	}
	return nil
}

func (s *Store) AcknowledgeAllAlerts(userID string) error {
	_, err := s.pool.Exec(context.Background(), `UPDATE alerts SET acknowledged=true, acknowledged_by=$1, acknowledged_at=now()
		WHERE acknowledged=false`, userID)
	if err != nil {
		return fmt.Errorf("acknowledge all alerts: %w", err)
	}
	return nil
}

// ── Thresholds ───────────────────────────────────────────────────────

func (s *Store) Thresholds() ([]*AlertThreshold, error) {
	rows, err := s.pool.Query(context.Background(),
		`SELECT metric, label, unit, warning_value, critical_value FROM thresholds ORDER BY metric`)
	if err != nil {
		return nil, fmt.Errorf("query thresholds: %w", err)
	}
	defer rows.Close()

	out := make([]*AlertThreshold, 0)
	for rows.Next() {
		var t AlertThreshold
		if err := rows.Scan(&t.Metric, &t.Label, &t.Unit, &t.WarningValue, &t.CriticalValue); err != nil {
			return nil, fmt.Errorf("scan threshold: %w", err)
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// UpdateThreshold updates one metric's thresholds. Returns pgx.ErrNoRows if
// the metric doesn't exist, so callers can 404 instead of silently no-oping.
func (s *Store) UpdateThreshold(metric string, warning, critical float64) error {
	tag, err := s.pool.Exec(context.Background(),
		`UPDATE thresholds SET warning_value=$2, critical_value=$3 WHERE metric=$1`, metric, warning, critical)
	if err != nil {
		return fmt.Errorf("update threshold %s: %w", metric, err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ── Error log ────────────────────────────────────────────────────────

// ErrorLogFilter narrows ErrorLog(); zero values mean "don't filter/paginate
// on this field" (Limit == 0 returns everything after From/To/MachineID).
type ErrorLogFilter struct {
	MachineID string
	// MachineIDs, if non-empty, restricts results to this set regardless
	// of MachineID — the server-enforced access scope for an operator
	// (see canAccessMachine), as opposed to MachineID which is the
	// caller's own optional query filter. Both apply together.
	MachineIDs []string
	From, To   *time.Time
	Limit      int
	Offset     int
}

// ErrorLog returns entries matching f (newest-first) plus the total match
// count before pagination, so callers can expose it (e.g. as a response
// header) without a second request.
func (s *Store) ErrorLog(f ErrorLogFilter) ([]*ErrorLogEntry, int, error) {
	query := `SELECT id, machine_id, machine_name, error_code, error_type, description, timestamp,
		resolved_at, resolved_by, duration, impact, COUNT(*) OVER() AS total_count
		FROM error_log WHERE 1=1`
	var args []interface{}
	if f.MachineID != "" {
		args = append(args, f.MachineID)
		query += fmt.Sprintf(" AND machine_id=$%d", len(args))
	}
	if f.MachineIDs != nil {
		args = append(args, f.MachineIDs)
		query += fmt.Sprintf(" AND machine_id = ANY($%d)", len(args))
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
		return nil, 0, fmt.Errorf("query error log: %w", err)
	}
	defer rows.Close()

	out := make([]*ErrorLogEntry, 0)
	total := 0
	for rows.Next() {
		var e ErrorLogEntry
		var resolvedBy *string
		if err := rows.Scan(&e.ID, &e.MachineID, &e.MachineName, &e.ErrorCode, &e.ErrorType, &e.Description,
			&e.Timestamp, &e.ResolvedAt, &resolvedBy, &e.Duration, &e.Impact, &total); err != nil {
			return nil, 0, fmt.Errorf("scan error log entry: %w", err)
		}
		if resolvedBy != nil {
			e.ResolvedBy = *resolvedBy
		}
		out = append(out, &e)
	}
	return out, total, rows.Err()
}

// ── Users ────────────────────────────────────────────────────────────

func (s *Store) Users() ([]*User, error) {
	rows, err := s.pool.Query(context.Background(),
		`SELECT id, name, email, role, assigned_machines FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query users: %w", err)
	}
	defer rows.Close()

	out := make([]*User, 0)
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.Email, &u.Role, &u.AssignedMachines); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}

// CreateUser inserts a new user with the given (already-hashed) password.
// u.ID is expected to already be set (see newUserID).
func (s *Store) CreateUser(u User, passwordHash string) (*User, error) {
	if u.AssignedMachines == nil {
		u.AssignedMachines = []string{}
	}
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO users (id, name, email, role, assigned_machines, password_hash) VALUES ($1,$2,$3,$4,$5,$6)`,
		u.ID, u.Name, u.Email, u.Role, u.AssignedMachines, passwordHash)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// UserUpdate is a partial patch for UpdateUser; nil fields are left
// unchanged.
type UserUpdate struct {
	Name             *string
	Email            *string
	Role             *UserRole
	AssignedMachines *[]string
	PasswordHash     *string
}

// UpdateUser applies a partial patch and returns the updated user. Returns
// pgx.ErrNoRows if id doesn't exist.
func (s *Store) UpdateUser(id string, u UserUpdate) (*User, error) {
	var set []string
	var args []interface{}
	add := func(col string, val interface{}) {
		args = append(args, val)
		set = append(set, fmt.Sprintf("%s=$%d", col, len(args)))
	}
	if u.Name != nil {
		add("name", *u.Name)
	}
	if u.Email != nil {
		add("email", *u.Email)
	}
	if u.Role != nil {
		add("role", *u.Role)
	}
	if u.AssignedMachines != nil {
		add("assigned_machines", *u.AssignedMachines)
	}
	if u.PasswordHash != nil {
		add("password_hash", *u.PasswordHash)
	}
	if len(set) == 0 {
		return s.userByID(id)
	}

	args = append(args, id)
	query := fmt.Sprintf(
		"UPDATE users SET %s WHERE id=$%d RETURNING id, name, email, role, assigned_machines",
		strings.Join(set, ", "), len(args),
	)
	var out User
	err := s.pool.QueryRow(context.Background(), query, args...).
		Scan(&out.ID, &out.Name, &out.Email, &out.Role, &out.AssignedMachines)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) userByID(id string) (*User, error) {
	var u User
	err := s.pool.QueryRow(context.Background(),
		`SELECT id, name, email, role, assigned_machines FROM users WHERE id=$1`, id,
	).Scan(&u.ID, &u.Name, &u.Email, &u.Role, &u.AssignedMachines)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// DeleteUser removes a user. Machines assigned to them are unassigned
// (ON DELETE SET NULL) and their sessions are invalidated (ON DELETE
// CASCADE) — see internal/db/migrations/0002_auth.sql. Returns
// pgx.ErrNoRows if id doesn't exist.
func (s *Store) DeleteUser(id string) error {
	tag, err := s.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func newUserID() string {
	b := make([]byte, 6)
	_, _ = crand.Read(b)
	return "u-" + hex.EncodeToString(b)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ── Reports ──────────────────────────────────────────────────────────
//
// Aggregated from metric_history/error_log/downtimes for the requested
// period — no more randomized placeholders. Efficiency has no directly
// stored source, so it's derived as uptime minus a small penalty per
// error (0.5pp each, floored at 0) — a documented heuristic, not a raw
// simulated number.

// machineAggregate holds the raw sums behind one machine's report row.
type machineAggregate struct {
	totalOutput     float64
	errorCount      int
	downtimeMinutes float64
}

func (s *Store) GenerateReport(p ReportParams) (ReportData, error) {
	ctx := context.Background()
	machines, err := s.Machines()
	if err != nil {
		return ReportData{}, fmt.Errorf("report: %w", err)
	}
	filtered := make([]*Machine, 0)
	for _, m := range machines {
		if len(p.MachineIDs) == 0 || contains(p.MachineIDs, m.ID) {
			filtered = append(filtered, m)
		}
	}
	machineIDs := make([]string, len(filtered))
	for i, m := range filtered {
		machineIDs[i] = m.ID
	}

	aggregates, err := s.machineAggregates(ctx, machineIDs, p.DateFrom, p.DateTo)
	if err != nil {
		return ReportData{}, fmt.Errorf("report: %w", err)
	}

	periodMinutes := p.DateTo.Sub(p.DateFrom).Minutes()
	breakdown := make([]MachineReportRow, 0, len(filtered))
	for _, m := range filtered {
		agg := aggregates[m.ID] // zero value if the machine had no activity in the window

		uptimePercent := 100.0
		if periodMinutes > 0 {
			uptimePercent = clamp(100*(1-agg.downtimeMinutes/periodMinutes), 0, 100)
		}
		efficiency := clamp(uptimePercent-float64(agg.errorCount)*0.5, 0, 100)

		breakdown = append(breakdown, MachineReportRow{
			MachineID:     m.ID,
			MachineName:   m.Name,
			TotalOutput:   math.Round(agg.totalOutput),
			UptimePercent: round1(uptimePercent),
			DowntimeHours: round1(agg.downtimeMinutes / 60),
			ErrorCount:    agg.errorCount,
			Efficiency:    round1(efficiency),
		})
	}

	timeSeries, err := s.buildTimeSeries(ctx, p, machineIDs)
	if err != nil {
		return ReportData{}, fmt.Errorf("report: %w", err)
	}

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
	}, nil
}

// machineAggregates computes per-machine output/error/downtime totals for
// [from, to] in a single round trip (previously 3 queries × N machines).
func (s *Store) machineAggregates(ctx context.Context, machineIDs []string, from, to time.Time) (map[string]machineAggregate, error) {
	out := make(map[string]machineAggregate, len(machineIDs))
	if len(machineIDs) == 0 {
		return out, nil
	}

	rows, err := s.pool.Query(ctx, `
		WITH mh AS (
			SELECT machine_id, COALESCE(SUM(output), 0) AS total_output
			FROM metric_history WHERE machine_id = ANY($1) AND timestamp >= $2 AND timestamp <= $3
			GROUP BY machine_id
		), errs AS (
			SELECT machine_id, COUNT(*) AS cnt FROM error_log
			WHERE machine_id = ANY($1) AND timestamp >= $2 AND timestamp <= $3
			GROUP BY machine_id
		), dt AS (
			SELECT machine_id, COALESCE(SUM(duration), 0) AS minutes FROM downtimes
			WHERE machine_id = ANY($1) AND start_time >= $2 AND start_time <= $3
			GROUP BY machine_id
		)
		SELECT ids.id, COALESCE(mh.total_output, 0), COALESCE(errs.cnt, 0), COALESCE(dt.minutes, 0)
		FROM unnest($1::text[]) AS ids(id)
		LEFT JOIN mh ON mh.machine_id = ids.id
		LEFT JOIN errs ON errs.machine_id = ids.id
		LEFT JOIN dt ON dt.machine_id = ids.id`,
		machineIDs, from, to,
	)
	if err != nil {
		return nil, fmt.Errorf("query machine aggregates: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var agg machineAggregate
		if err := rows.Scan(&id, &agg.totalOutput, &agg.errorCount, &agg.downtimeMinutes); err != nil {
			return nil, fmt.Errorf("scan machine aggregate: %w", err)
		}
		out[id] = agg
	}
	return out, rows.Err()
}

// buildTimeSeries buckets metric_history/error_log into p.GroupBy-sized
// windows starting at p.DateFrom (matching the original bucketing, which
// isn't calendar-aligned). Uptime per bucket is derived from downtime
// overlap rather than stored directly.
// bucketAggregate holds one time-series bucket's raw sums.
type bucketAggregate struct {
	output, avgTemp, avgLoad float64
	errCount                 int
}

// buildTimeSeries buckets metric_history/error_log into p.GroupBy-sized
// windows starting at p.DateFrom (matching the original bucketing, which
// isn't calendar-aligned). Uptime per bucket is derived from downtime
// overlap rather than stored directly. All buckets are computed in a
// single query (previously one query per bucket — up to ~720 for an
// hourly report over a month).
func (s *Store) buildTimeSeries(ctx context.Context, p ReportParams, machineIDs []string) ([]TimeSeriesPoint, error) {
	step := stepDuration(p.GroupBy)
	downtimes, err := s.downtimesInRange(ctx, machineIDs, p.DateFrom, p.DateTo)
	if err != nil {
		return nil, err
	}
	numMachines := len(machineIDs)

	buckets, err := s.bucketAggregates(ctx, machineIDs, p.DateFrom, p.DateTo, step)
	if err != nil {
		return nil, err
	}

	points := make([]TimeSeriesPoint, 0)
	for i, t := 0, p.DateFrom; !t.After(p.DateTo); i, t = i+1, t.Add(step) {
		bucketEnd := t.Add(step)
		agg := buckets[i] // zero value if nothing happened in this bucket

		uptime := 100.0
		if bucketMinutes := bucketEnd.Sub(t).Minutes(); numMachines > 0 && bucketMinutes > 0 {
			downtimeMinutes := overlapMinutes(downtimes, t, bucketEnd)
			uptime = clamp(100*(1-downtimeMinutes/(bucketMinutes*float64(numMachines))), 0, 100)
		}

		points = append(points, TimeSeriesPoint{
			Timestamp:      t,
			Output:         math.Round(agg.output),
			Uptime:         round1(uptime),
			AvgTemperature: round1(agg.avgTemp),
			AvgLoad:        round1(agg.avgLoad),
			ErrorCount:     agg.errCount,
		})
	}
	return points, nil
}

// bucketAggregates computes per-bucket sums by classifying every row into
// bucket index floor((timestamp - from) / stepSeconds) in SQL — the same
// arithmetic the caller uses to enumerate buckets from p.DateFrom, so the
// map key lines up with the loop counter exactly.
func (s *Store) bucketAggregates(ctx context.Context, machineIDs []string, from, to time.Time, step time.Duration) (map[int]bucketAggregate, error) {
	out := make(map[int]bucketAggregate)
	if len(machineIDs) == 0 {
		return out, nil
	}
	stepSeconds := step.Seconds()

	rows, err := s.pool.Query(ctx, `
		WITH mh AS (
			SELECT floor(extract(epoch FROM (timestamp - $2)) / $4)::bigint AS bucket,
				COALESCE(SUM(output), 0) AS output,
				COALESCE(AVG(temperature), 0) AS avg_temp,
				COALESCE(AVG(load), 0) AS avg_load
			FROM metric_history WHERE machine_id = ANY($1) AND timestamp >= $2 AND timestamp < $3
			GROUP BY bucket
		), errs AS (
			SELECT floor(extract(epoch FROM (timestamp - $2)) / $4)::bigint AS bucket, COUNT(*) AS cnt
			FROM error_log WHERE machine_id = ANY($1) AND timestamp >= $2 AND timestamp < $3
			GROUP BY bucket
		)
		SELECT COALESCE(mh.bucket, errs.bucket), COALESCE(mh.output, 0), COALESCE(mh.avg_temp, 0),
			COALESCE(mh.avg_load, 0), COALESCE(errs.cnt, 0)
		FROM mh FULL OUTER JOIN errs ON mh.bucket = errs.bucket`,
		machineIDs, from, to, stepSeconds,
	)
	if err != nil {
		return nil, fmt.Errorf("query bucket aggregates: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var bucket int
		var agg bucketAggregate
		if err := rows.Scan(&bucket, &agg.output, &agg.avgTemp, &agg.avgLoad, &agg.errCount); err != nil {
			return nil, fmt.Errorf("scan bucket aggregate: %w", err)
		}
		out[bucket] = agg
	}
	return out, rows.Err()
}

type downtimeInterval struct {
	start, end time.Time
}

// downtimesInRange fetches downtimes overlapping [from, to) for the given
// machines once, so per-bucket uptime can be computed in Go instead of
// re-querying per bucket.
func (s *Store) downtimesInRange(ctx context.Context, machineIDs []string, from, to time.Time) ([]downtimeInterval, error) {
	if len(machineIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT start_time, end_time FROM downtimes
		WHERE machine_id = ANY($1) AND start_time < $3 AND (end_time IS NULL OR end_time > $2)`,
		machineIDs, from, to)
	if err != nil {
		return nil, fmt.Errorf("query downtimes in range: %w", err)
	}
	defer rows.Close()

	var out []downtimeInterval
	for rows.Next() {
		var start time.Time
		var end *time.Time
		if err := rows.Scan(&start, &end); err != nil {
			return nil, fmt.Errorf("scan downtime interval: %w", err)
		}
		e := to // treat a still-open downtime as ongoing through the window
		if end != nil {
			e = *end
		}
		out = append(out, downtimeInterval{start: start, end: e})
	}
	return out, rows.Err()
}

// overlapMinutes sums how many minutes of [from, to) each interval covers.
func overlapMinutes(intervals []downtimeInterval, from, to time.Time) float64 {
	var total float64
	for _, iv := range intervals {
		start, end := iv.start, iv.end
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if end.After(start) {
			total += end.Sub(start).Minutes()
		}
	}
	return total
}

func clamp(v, min, max float64) float64 {
	return math.Min(max, math.Max(min, v))
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
