// cmd/seed populates the database with the same demo dataset the old
// in-memory Store used to generate at process startup: 12 machines, 3
// users, 3 alert thresholds, 8 alerts, 50 error-log entries, 8 downtime
// entries per machine, and 30 days of 5-minute metric history per machine.
//
// Destructive: it truncates the relevant tables first, so re-running gives
// a fresh, consistent demo state. Not run automatically — invoke it
// explicitly:
//
//	DATABASE_URL=postgres://... go run ./cmd/seed
//
// Enum string values (machine types/statuses, alert types/severities,
// error types) must match the CHECK constraints in
// internal/db/migrations/0001_init.sql and the enums in ../../models.go.
package main

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"time"

	appdb "equipment-monitoring-api/internal/db"

	"github.com/jackc/pgx/v5"
)

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is not set")
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := appdb.Connect(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := appdb.Migrate(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	tx, err := pool.Begin(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "begin: %v\n", err)
		os.Exit(1)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`TRUNCATE users, machines, alerts, error_log, metric_history, downtimes, thresholds RESTART IDENTITY CASCADE`,
	); err != nil {
		fmt.Fprintf(os.Stderr, "truncate: %v\n", err)
		os.Exit(1)
	}

	seedUsers(ctx, tx)
	seedThresholds(ctx, tx)
	machines := seedMachines(ctx, tx, rng)
	names := machineNames(machines)
	seedErrorLog(ctx, tx, rng, names)
	seedAlerts(ctx, tx, rng, names)
	for _, m := range machines {
		seedDowntimes(ctx, tx, rng, m.id)
		seedHistory(ctx, tx, rng, m.id)
	}

	if err := tx.Commit(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "commit: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("seeded %d machines, 3 users, 3 thresholds, 8 alerts, 50 error-log entries, "+
		"8 downtimes/machine, 30d history/machine\n", len(machines))
}

// ── users & thresholds ──────────────────────────────────────────────

func seedUsers(ctx context.Context, tx pgx.Tx) {
	users := []struct{ id, name, email, role string }{
		{"u1", "Иван Петров", "operator@demo.com", "operator"},
		{"u2", "Мария Сидорова", "manager@demo.com", "manager"},
		{"u3", "Алексей Иванов", "admin@demo.com", "admin"},
	}
	assigned := map[string][]string{"u1": {"m1", "m2", "m3", "m4"}}
	for _, u := range users {
		machines := assigned[u.id]
		if machines == nil {
			machines = []string{}
		}
		mustExec(ctx, tx, `INSERT INTO users (id, name, email, role, assigned_machines) VALUES ($1,$2,$3,$4,$5)`,
			u.id, u.name, u.email, u.role, machines)
	}
}

func seedThresholds(ctx context.Context, tx pgx.Tx) {
	thresholds := []struct {
		metric, label, unit string
		warning, critical   float64
	}{
		{"temperature", "Температура", "°C", 80, 95},
		{"load", "Нагрузка", "%", 85, 95},
		{"vibration", "Вибрация", "мм/с", 4, 7},
	}
	for _, t := range thresholds {
		mustExec(ctx, tx, `INSERT INTO thresholds (metric, label, unit, warning_value, critical_value) VALUES ($1,$2,$3,$4,$5)`,
			t.metric, t.label, t.unit, t.warning, t.critical)
	}
}

// ── machines ─────────────────────────────────────────────────────────

type seededMachine struct {
	id, name string
}

func machineNames(machines []seededMachine) map[string]string {
	out := make(map[string]string, len(machines))
	for _, m := range machines {
		out[m.id] = m.name
	}
	return out
}

func seedMachines(ctx context.Context, tx pgx.Tx, rng *rand.Rand) []seededMachine {
	type spec struct {
		id, name, location, op string
		typ                    string
		status                 string
	}
	specs := []spec{
		{"m1", "ЧПУ-01", "Цех 1", "u1", "cnc", "running"},
		{"m2", "ЧПУ-02", "Цех 1", "u1", "cnc", "warning"},
		{"m3", "Токарный-01", "Цех 1", "u1", "lathe", "running"},
		{"m4", "Токарный-02", "Цех 1", "u1", "lathe", "idle"},
		{"m5", "Фрезерный-01", "Цех 2", "u2", "milling", "running"},
		{"m6", "Фрезерный-02", "Цех 2", "u2", "milling", "running"},
		{"m7", "Шлифовальный-01", "Цех 2", "u2", "grinding", "error"},
		{"m8", "Шлифовальный-02", "Цех 2", "u2", "grinding", "running"},
		{"m9", "Пресс-01", "Цех 3", "u3", "press", "running"},
		{"m10", "Пресс-02", "Цех 3", "u3", "press", "maintenance"},
		{"m11", "ЧПУ-03", "Цех 3", "u3", "cnc", "running"},
		{"m12", "Токарный-03", "Цех 3", "u3", "lathe", "offline"},
	}

	out := make([]seededMachine, 0, len(specs))
	for _, sp := range specs {
		isRunning := sp.status == "running" || sp.status == "warning"
		temp := generateTemp(rng, sp.status)
		load := boolPick(isRunning, 40+rng.Float64()*45, rng.Float64()*10)
		output := boolPick(isRunning, math.Round(15+rng.Float64()*20), 0)
		uptime := boolPick(isRunning, math.Round(1+rng.Float64()*200), 0)
		power := round1(boolPick(isRunning, 15+rng.Float64()*20, 2+rng.Float64()*3))

		var spindleSpeed *float64
		if sp.typ == "cnc" || sp.typ == "lathe" {
			v := boolPick(isRunning, math.Round(800+rng.Float64()*1500), 0)
			spindleSpeed = &v
		}
		vibration := boolPick(isRunning, round1(1+rng.Float64()*5), 0.1)

		mustExec(ctx, tx, `INSERT INTO machines
			(id, name, type, location, status, assigned_operator, temperature, load, output, uptime,
			 power_consumption, spindle_speed, vibration, last_updated)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			sp.id, sp.name, sp.typ, sp.location, sp.status, sp.op, round1(temp), round1(load), output, uptime,
			power, spindleSpeed, round1(vibration), time.Now())

		out = append(out, seededMachine{id: sp.id, name: sp.name})
	}
	return out
}

func generateTemp(rng *rand.Rand, status string) float64 {
	switch status {
	case "warning":
		return 85 + rng.Float64()*10
	case "error":
		return 95 + rng.Float64()*5
	case "running":
		return 50 + rng.Float64()*25
	default:
		return 25 + rng.Float64()*10
	}
}

// ── alerts ───────────────────────────────────────────────────────────

func seedAlerts(ctx context.Context, tx pgx.Tx, rng *rand.Rand, names map[string]string) {
	type raw struct {
		id, mID, msg, metric string
		typ, sev             string
		cur, thr             float64
		ack                  bool
	}
	rows := []raw{
		{"a1", "m2", "Температура превысила предупредительный порог", "temperature", "threshold_exceeded", "warning", 87.3, 80, false},
		{"a2", "m7", "Аварийная остановка станка", "status", "machine_down", "critical", 0, 0, false},
		{"a3", "m12", "Потеряна связь со станком", "status", "machine_offline", "critical", 0, 0, false},
		{"a4", "m1", "Нагрузка превысила предупредительный порог", "load", "threshold_exceeded", "warning", 88.5, 85, true},
		{"a5", "m5", "Плановое ТО через 8 часов", "uptime", "maintenance_due", "info", 312, 320, false},
		{"a6", "m9", "Обнаружена аномалия вибрации", "vibration", "anomaly_detected", "warning", 5.2, 4, false},
		{"a7", "m3", "Вибрация превысила норму", "vibration", "threshold_exceeded", "warning", 4.8, 4, true},
		{"a8", "m6", "Нагрузка на верхней границе нормы", "load", "threshold_exceeded", "info", 82.1, 85, false},
	}
	now := time.Now()
	for _, r := range rows {
		ts := now.Add(-time.Duration(rng.Float64()*12) * time.Hour)
		var ackBy *string
		var ackAt *time.Time
		if r.ack {
			by := "u2"
			ackBy = &by
			at := now.Add(-time.Duration(rng.Float64()) * time.Hour)
			ackAt = &at
		}
		mustExec(ctx, tx, `INSERT INTO alerts
			(id, machine_id, machine_name, type, severity, message, metric_name, current_value, threshold_value,
			 timestamp, acknowledged, acknowledged_by, acknowledged_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			r.id, r.mID, names[r.mID], r.typ, r.sev, r.msg, r.metric, r.cur, r.thr, ts, r.ack, ackBy, ackAt)
	}
}

// ── error log ────────────────────────────────────────────────────────

func seedErrorLog(ctx context.Context, tx pgx.Tx, rng *rand.Rand, names map[string]string) {
	machineIDs := []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9", "m10", "m11", "m12"}
	types := []string{"emergency_stop", "overheating", "mechanical_failure", "electrical_fault", "sensor_failure", "communication_lost", "software_error"}
	descriptions := map[string][]string{
		"emergency_stop":     {"Нажата кнопка аварийного останова", "Сработала защита от перегрузки"},
		"overheating":        {"Перегрев шпинделя", "Перегрев привода"},
		"mechanical_failure": {"Поломка инструмента", "Износ подшипника"},
		"electrical_fault":   {"Скачок напряжения", "Отказ частотного преобразователя"},
		"sensor_failure":     {"Отказ датчика температуры", "Неисправность энкодера"},
		"communication_lost": {"Потеря связи с контроллером", "Таймаут сети"},
		"software_error":     {"Ошибка выполнения программы", "Сбой файловой системы ЧПУ"},
	}
	impacts := []string{
		"Остановка производства на участке", "Задержка выполнения партии",
		"Потеря 15 деталей", "Требуется замена инструмента", "Плановый ремонт",
	}
	resolvers := []string{"u1", "u2", "u3", ""}
	now := time.Now()

	for i := 0; i < 50; i++ {
		mID := machineIDs[rng.Intn(len(machineIDs))]
		typ := types[rng.Intn(len(types))]
		ts := now.Add(-time.Duration(rng.Float64()*30*24) * time.Hour)
		resolved := rng.Float64() > 0.2
		dur := int(math.Round(10 + rng.Float64()*240))
		descs := descriptions[typ]

		var resolvedAt *time.Time
		var resolvedBy *string
		var duration *int
		if resolved {
			ra := ts.Add(time.Duration(dur) * time.Minute)
			resolvedAt = &ra
			by := resolvers[rng.Intn(len(resolvers))]
			resolvedBy = &by
			duration = &dur
		}

		mustExec(ctx, tx, `INSERT INTO error_log
			(id, machine_id, machine_name, error_code, error_type, description, timestamp, resolved_at, resolved_by, duration, impact)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			fmt.Sprintf("err-%d", i), mID, names[mID], fmt.Sprintf("E-%03d", rng.Intn(99)+1), typ,
			descs[rng.Intn(len(descs))], ts, resolvedAt, resolvedBy, duration, impacts[rng.Intn(len(impacts))])
	}
}

// ── downtimes ────────────────────────────────────────────────────────

func seedDowntimes(ctx context.Context, tx pgx.Tx, rng *rand.Rand, machineID string) {
	reasons := []string{"Плановое ТО", "Замена инструмента", "Настройка параметров", "Авария", "Нет заготовок"}
	now := time.Now()
	for i := 0; i < 8; i++ {
		start := now.Add(-time.Duration(rng.Float64()*30*24) * time.Hour)
		dur := int(math.Round(15 + rng.Float64()*180))
		end := start.Add(time.Duration(dur) * time.Minute)
		mustExec(ctx, tx, `INSERT INTO downtimes (id, machine_id, start_time, end_time, duration, reason)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			fmt.Sprintf("dt-%s-%d", machineID, i), machineID, start, end, dur, reasons[rng.Intn(len(reasons))])
	}
}

// ── metric history ───────────────────────────────────────────────────

func seedHistory(ctx context.Context, tx pgx.Tx, rng *rand.Rand, machineID string) {
	now := time.Now()
	start := now.Add(-30 * 24 * time.Hour)
	temp, load, output, vib, power := 60.0, 65.0, 20.0, 2.5, 22.0

	batch := &pgx.Batch{}
	const insertHistory = `INSERT INTO metric_history (machine_id, timestamp, temperature, load, output, vibration, power_consumption)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`
	for t := start; !t.After(now); t = t.Add(5 * time.Minute) {
		temp = fluctuate(rng, temp, 2, 20, 98)
		load = fluctuate(rng, load, 3, 0, 100)
		output = math.Max(0, math.Round(fluctuate(rng, output, 2, 0, 50)))
		vib = fluctuate(rng, vib, 0.3, 0, 12)
		power = fluctuate(rng, power, 1, 5, 50)
		batch.Queue(insertHistory, machineID, t, temp, load, output, vib, power)
	}
	br := tx.SendBatch(ctx, batch)
	if err := br.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "seed history for %s: %v\n", machineID, err)
		os.Exit(1)
	}
}

// ── helpers (mirrors store.go's simulation math) ───────────────────────

func fluctuate(rng *rand.Rand, val, delta, min, max float64) float64 {
	change := (rng.Float64() - 0.5) * 2 * delta
	return round1(math.Min(max, math.Max(min, val+change)))
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func boolPick(cond bool, a, b float64) float64 {
	if cond {
		return a
	}
	return b
}

func mustExec(ctx context.Context, tx pgx.Tx, sql string, args ...interface{}) {
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		fmt.Fprintf(os.Stderr, "exec failed: %v\nsql: %s\n", err, sql)
		os.Exit(1)
	}
}
