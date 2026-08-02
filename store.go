package main

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// Store is the in-memory data layer. In v3 this would be backed by
// PostgreSQL + Redis; the public methods form the seam for that swap.
type Store struct {
	mu         sync.RWMutex
	machines   []*Machine
	alerts     []*Alert
	errorLog   []*ErrorLogEntry
	users      []*User
	thresholds []*AlertThreshold
	history    map[string][]MetricHistoryPoint
	downtimes  map[string][]DowntimeEntry
	rng        *rand.Rand
}

func NewStore() *Store {
	s := &Store{
		history:   make(map[string][]MetricHistoryPoint),
		downtimes: make(map[string][]DowntimeEntry),
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	s.seed()
	return s
}

func (s *Store) seed() {
	s.users = []*User{
		{ID: "u1", Name: "Иван Петров", Email: "operator@demo.com", Role: RoleOperator, AssignedMachines: []string{"m1", "m2", "m3", "m4"}},
		{ID: "u2", Name: "Мария Сидорова", Email: "manager@demo.com", Role: RoleManager},
		{ID: "u3", Name: "Алексей Иванов", Email: "admin@demo.com", Role: RoleAdmin},
	}
	s.thresholds = []*AlertThreshold{
		{Metric: "temperature", Label: "Температура", Unit: "°C", WarningValue: 80, CriticalValue: 95},
		{Metric: "load", Label: "Нагрузка", Unit: "%", WarningValue: 85, CriticalValue: 95},
		{Metric: "vibration", Label: "Вибрация", Unit: "мм/с", WarningValue: 4, CriticalValue: 7},
	}
	s.machines = s.generateMachines()
	s.errorLog = s.generateErrorLog()
	s.alerts = s.generateAlerts()
}

// ── Machines ─────────────────────────────────────────────────────────

type machineSpec struct {
	id, name, location, op string
	typ                    MachineType
	status                 MachineStatus
}

func (s *Store) generateMachines() []*Machine {
	specs := []machineSpec{
		{"m1", "ЧПУ-01", "Цех 1", "u1", TypeCNC, StatusRunning},
		{"m2", "ЧПУ-02", "Цех 1", "u1", TypeCNC, StatusWarning},
		{"m3", "Токарный-01", "Цех 1", "u1", TypeLathe, StatusRunning},
		{"m4", "Токарный-02", "Цех 1", "u1", TypeLathe, StatusIdle},
		{"m5", "Фрезерный-01", "Цех 2", "u2", TypeMilling, StatusRunning},
		{"m6", "Фрезерный-02", "Цех 2", "u2", TypeMilling, StatusRunning},
		{"m7", "Шлифовальный-01", "Цех 2", "u2", TypeGrinding, StatusError},
		{"m8", "Шлифовальный-02", "Цех 2", "u2", TypeGrinding, StatusRunning},
		{"m9", "Пресс-01", "Цех 3", "u3", TypePress, StatusRunning},
		{"m10", "Пресс-02", "Цех 3", "u3", TypePress, StatusMaintenance},
		{"m11", "ЧПУ-03", "Цех 3", "u3", TypeCNC, StatusRunning},
		{"m12", "Токарный-03", "Цех 3", "u3", TypeLathe, StatusOffline},
	}
	out := make([]*Machine, 0, len(specs))
	for _, sp := range specs {
		out = append(out, &Machine{
			ID:               sp.id,
			Name:             sp.name,
			Type:             sp.typ,
			Location:         sp.location,
			Status:           sp.status,
			Metrics:          s.generateMetrics(sp.typ, sp.status),
			LastUpdated:      time.Now(),
			AssignedOperator: sp.op,
		})
	}
	return out
}

func (s *Store) generateMetrics(typ MachineType, status MachineStatus) MachineMetrics {
	isRunning := status == StatusRunning || status == StatusWarning
	var temp float64
	switch {
	case status == StatusWarning:
		temp = 85 + s.rng.Float64()*10
	case status == StatusError:
		temp = 95 + s.rng.Float64()*5
	case isRunning:
		temp = 50 + s.rng.Float64()*25
	default:
		temp = 25 + s.rng.Float64()*10
	}
	m := MachineMetrics{
		Temperature:      round1(temp),
		Load:             boolPick(isRunning, 40+s.rng.Float64()*45, s.rng.Float64()*10),
		Output:           boolPick(isRunning, math.Round(15+s.rng.Float64()*20), 0),
		Uptime:           boolPick(isRunning, math.Round(1+s.rng.Float64()*200), 0),
		PowerConsumption: round1(boolPick(isRunning, 15+s.rng.Float64()*20, 2+s.rng.Float64()*3)),
	}
	if typ == TypeCNC || typ == TypeLathe {
		v := boolPick(isRunning, math.Round(800+s.rng.Float64()*1500), 0)
		m.SpindleSpeed = &v
	}
	vib := boolPick(isRunning, round1(1+s.rng.Float64()*5), 0.1)
	m.Vibration = &vib
	return m
}

func (s *Store) Machines() []*Machine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Machine, len(s.machines))
	copy(out, s.machines)
	return out
}

func (s *Store) Machine(id string) *Machine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.machines {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// Tick advances live metrics for all active machines. Returns the fresh
// snapshot for broadcasting over WebSocket.
func (s *Store) Tick() []*Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.machines {
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
		m.LastUpdated = time.Now()
		s.updateStatus(m)
	}
	out := make([]*Machine, len(s.machines))
	copy(out, s.machines)
	return out
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
	s.mu.Lock()
	if _, ok := s.history[machineID]; !ok {
		s.history[machineID] = s.generateHistory()
	}
	all := s.history[machineID]
	s.mu.Unlock()

	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	out := make([]MetricHistoryPoint, 0)
	for _, p := range all {
		if !p.Timestamp.Before(cutoff) {
			out = append(out, p)
		}
	}
	return out
}

func (s *Store) generateHistory() []MetricHistoryPoint {
	points := make([]MetricHistoryPoint, 0, 30*24*12)
	now := time.Now()
	start := now.Add(-30 * 24 * time.Hour)
	temp, load, output, vib, power := 60.0, 65.0, 20.0, 2.5, 22.0
	for t := start; !t.After(now); t = t.Add(5 * time.Minute) {
		temp = fluctuate(s.rng, temp, 2, 20, 98)
		load = fluctuate(s.rng, load, 3, 0, 100)
		output = math.Max(0, math.Round(fluctuate(s.rng, output, 2, 0, 50)))
		vib = fluctuate(s.rng, vib, 0.3, 0, 12)
		power = fluctuate(s.rng, power, 1, 5, 50)
		points = append(points, MetricHistoryPoint{
			Timestamp: t, Temperature: temp, Load: load,
			Output: output, Vibration: vib, PowerConsumption: power,
		})
	}
	return points
}

// ── Downtimes ────────────────────────────────────────────────────────

func (s *Store) Downtimes(machineID string) []DowntimeEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.downtimes[machineID]; !ok {
		s.downtimes[machineID] = s.generateDowntimes(machineID)
	}
	return s.downtimes[machineID]
}

func (s *Store) generateDowntimes(machineID string) []DowntimeEntry {
	reasons := []string{"Плановое ТО", "Замена инструмента", "Настройка параметров", "Авария", "Нет заготовок"}
	entries := make([]DowntimeEntry, 0, 8)
	now := time.Now()
	for i := 0; i < 8; i++ {
		start := now.Add(-time.Duration(s.rng.Float64()*30*24) * time.Hour)
		dur := int(math.Round(15 + s.rng.Float64()*180))
		end := start.Add(time.Duration(dur) * time.Minute)
		d := dur
		entries = append(entries, DowntimeEntry{
			ID:        fmt.Sprintf("dt-%s-%d", machineID, i),
			MachineID: machineID,
			StartTime: start,
			EndTime:   &end,
			Duration:  &d,
			Reason:    reasons[s.rng.Intn(len(reasons))],
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].StartTime.After(entries[j].StartTime)
	})
	return entries
}

// ── Alerts ───────────────────────────────────────────────────────────

func (s *Store) generateAlerts() []*Alert {
	names := s.machineNames()
	type raw struct {
		id, mID, msg, metric string
		typ                  AlertType
		sev                  AlertSeverity
		cur, thr             float64
		ack                  bool
	}
	rows := []raw{
		{"a1", "m2", "Температура превысила предупредительный порог", "temperature", AlertThresholdExceeded, SeverityWarning, 87.3, 80, false},
		{"a2", "m7", "Аварийная остановка станка", "status", AlertMachineDown, SeverityCritical, 0, 0, false},
		{"a3", "m12", "Потеряна связь со станком", "status", AlertMachineOffline, SeverityCritical, 0, 0, false},
		{"a4", "m1", "Нагрузка превысила предупредительный порог", "load", AlertThresholdExceeded, SeverityWarning, 88.5, 85, true},
		{"a5", "m5", "Плановое ТО через 8 часов", "uptime", AlertMaintenanceDue, SeverityInfo, 312, 320, false},
		{"a6", "m9", "Обнаружена аномалия вибрации", "vibration", AlertAnomalyDetected, SeverityWarning, 5.2, 4, false},
		{"a7", "m3", "Вибрация превысила норму", "vibration", AlertThresholdExceeded, SeverityWarning, 4.8, 4, true},
		{"a8", "m6", "Нагрузка на верхней границе нормы", "load", AlertThresholdExceeded, SeverityInfo, 82.1, 85, false},
	}
	out := make([]*Alert, 0, len(rows))
	now := time.Now()
	for _, r := range rows {
		a := &Alert{
			ID: r.id, MachineID: r.mID, MachineName: names[r.mID],
			Type: r.typ, Severity: r.sev, Message: r.msg, MetricName: r.metric,
			CurrentValue: r.cur, ThresholdValue: r.thr,
			Timestamp:    now.Add(-time.Duration(s.rng.Float64()*12) * time.Hour),
			Acknowledged: r.ack,
		}
		if r.ack {
			a.AcknowledgedBy = "u2"
			at := now.Add(-time.Duration(s.rng.Float64()) * time.Hour)
			a.AcknowledgedAt = &at
		}
		out = append(out, a)
	}
	return out
}

// AlertFilter narrows Alerts(); zero values mean "don't filter on this field".
type AlertFilter struct {
	MachineID    string
	Severity     AlertSeverity
	Acknowledged *bool
}

func (s *Store) Alerts(f AlertFilter) []*Alert {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Alert, 0, len(s.alerts))
	for _, a := range s.alerts {
		if f.MachineID != "" && a.MachineID != f.MachineID {
			continue
		}
		if f.Severity != "" && a.Severity != f.Severity {
			continue
		}
		if f.Acknowledged != nil && a.Acknowledged != *f.Acknowledged {
			continue
		}
		out = append(out, a)
	}
	return out
}

func (s *Store) AcknowledgeAlert(id, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.alerts {
		if a.ID == id && !a.Acknowledged {
			a.Acknowledged = true
			a.AcknowledgedBy = userID
			now := time.Now()
			a.AcknowledgedAt = &now
		}
	}
}

func (s *Store) AcknowledgeAllAlerts(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, a := range s.alerts {
		if !a.Acknowledged {
			a.Acknowledged = true
			a.AcknowledgedBy = userID
			at := now
			a.AcknowledgedAt = &at
		}
	}
}

// ── Thresholds ───────────────────────────────────────────────────────

func (s *Store) Thresholds() []*AlertThreshold {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*AlertThreshold, len(s.thresholds))
	copy(out, s.thresholds)
	return out
}

func (s *Store) UpdateThreshold(metric string, warning, critical float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.thresholds {
		if t.Metric == metric {
			t.WarningValue = warning
			t.CriticalValue = critical
		}
	}
}

// ── Error log ────────────────────────────────────────────────────────

func (s *Store) generateErrorLog() []*ErrorLogEntry {
	names := s.machineNames()
	machineIDs := []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9", "m10", "m11", "m12"}
	types := []ErrorType{ErrEmergencyStop, ErrOverheating, ErrMechanicalFailure, ErrElectricalFault, ErrSensorFailure, ErrCommunicationLost, ErrSoftwareError}
	descriptions := map[ErrorType][]string{
		ErrEmergencyStop:     {"Нажата кнопка аварийного останова", "Сработала защита от перегрузки"},
		ErrOverheating:       {"Перегрев шпинделя", "Перегрев привода"},
		ErrMechanicalFailure: {"Поломка инструмента", "Износ подшипника"},
		ErrElectricalFault:   {"Скачок напряжения", "Отказ частотного преобразователя"},
		ErrSensorFailure:     {"Отказ датчика температуры", "Неисправность энкодера"},
		ErrCommunicationLost: {"Потеря связи с контроллером", "Таймаут сети"},
		ErrSoftwareError:     {"Ошибка выполнения программы", "Сбой файловой системы ЧПУ"},
	}
	impacts := []string{
		"Остановка производства на участке", "Задержка выполнения партии",
		"Потеря 15 деталей", "Требуется замена инструмента", "Плановый ремонт",
	}
	resolvers := []string{"u1", "u2", "u3", ""}
	out := make([]*ErrorLogEntry, 0, 50)
	now := time.Now()
	for i := 0; i < 50; i++ {
		mID := machineIDs[s.rng.Intn(len(machineIDs))]
		typ := types[s.rng.Intn(len(types))]
		ts := now.Add(-time.Duration(s.rng.Float64()*30*24) * time.Hour)
		resolved := s.rng.Float64() > 0.2
		dur := int(math.Round(10 + s.rng.Float64()*240))
		descs := descriptions[typ]
		e := &ErrorLogEntry{
			ID:          fmt.Sprintf("err-%d", i),
			MachineID:   mID,
			MachineName: names[mID],
			ErrorCode:   fmt.Sprintf("E-%03d", s.rng.Intn(99)+1),
			ErrorType:   typ,
			Description: descs[s.rng.Intn(len(descs))],
			Timestamp:   ts,
			Impact:      impacts[s.rng.Intn(len(impacts))],
		}
		if resolved {
			ra := ts.Add(time.Duration(dur) * time.Minute)
			e.ResolvedAt = &ra
			e.ResolvedBy = resolvers[s.rng.Intn(len(resolvers))]
			d := dur
			e.Duration = &d
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	return out
}

// ErrorLogFilter narrows ErrorLog(); zero values mean "don't filter/paginate
// on this field" (Limit == 0 returns everything after From/To/MachineID).
type ErrorLogFilter struct {
	MachineID string
	From, To  *time.Time
	Limit     int
	Offset    int
}

// ErrorLog returns entries matching f (already sorted newest-first) plus the
// total match count before pagination, so callers can expose it (e.g. as a
// response header) without the client needing a second request.
func (s *Store) ErrorLog(f ErrorLogFilter) ([]*ErrorLogEntry, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	filtered := make([]*ErrorLogEntry, 0, len(s.errorLog))
	for _, e := range s.errorLog {
		if f.MachineID != "" && e.MachineID != f.MachineID {
			continue
		}
		if f.From != nil && e.Timestamp.Before(*f.From) {
			continue
		}
		if f.To != nil && e.Timestamp.After(*f.To) {
			continue
		}
		filtered = append(filtered, e)
	}
	total := len(filtered)

	if f.Offset > 0 {
		if f.Offset >= len(filtered) {
			filtered = nil
		} else {
			filtered = filtered[f.Offset:]
		}
	}
	if f.Limit > 0 && f.Limit < len(filtered) {
		filtered = filtered[:f.Limit]
	}

	out := make([]*ErrorLogEntry, len(filtered))
	copy(out, filtered)
	return out, total
}

// ── Users ────────────────────────────────────────────────────────────

func (s *Store) Users() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*User, len(s.users))
	copy(out, s.users)
	return out
}

func (s *Store) machineNames() map[string]string {
	return map[string]string{
		"m1": "ЧПУ-01", "m2": "ЧПУ-02", "m3": "Токарный-01", "m4": "Токарный-02",
		"m5": "Фрезерный-01", "m6": "Фрезерный-02", "m7": "Шлифовальный-01",
		"m8": "Шлифовальный-02", "m9": "Пресс-01", "m10": "Пресс-02",
		"m11": "ЧПУ-03", "m12": "Токарный-03",
	}
}

// ── Reports ──────────────────────────────────────────────────────────

func (s *Store) GenerateReport(p ReportParams) ReportData {
	machines := s.Machines()
	filtered := make([]*Machine, 0)
	for _, m := range machines {
		if len(p.MachineIDs) == 0 || contains(p.MachineIDs, m.ID) {
			filtered = append(filtered, m)
		}
	}

	s.mu.Lock()
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
	s.mu.Unlock()

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

func boolPick(cond bool, a, b float64) float64 {
	if cond {
		return a
	}
	return b
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
