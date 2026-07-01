package main

import "time"

// ── Machine ──────────────────────────────────────────────────────────

type MachineStatus string

const (
	StatusRunning     MachineStatus = "running"
	StatusWarning     MachineStatus = "warning"
	StatusError       MachineStatus = "error"
	StatusIdle        MachineStatus = "idle"
	StatusMaintenance MachineStatus = "maintenance"
	StatusOffline     MachineStatus = "offline"
)

type MachineType string

const (
	TypeCNC      MachineType = "cnc"
	TypeLathe    MachineType = "lathe"
	TypeMilling  MachineType = "milling"
	TypeGrinding MachineType = "grinding"
	TypePress    MachineType = "press"
)

type MachineMetrics struct {
	Temperature      float64  `json:"temperature"`
	Load             float64  `json:"load"`
	Output           float64  `json:"output"`
	Uptime           float64  `json:"uptime"`
	PowerConsumption float64  `json:"powerConsumption"`
	SpindleSpeed     *float64 `json:"spindleSpeed,omitempty"`
	Vibration        *float64 `json:"vibration,omitempty"`
}

type Machine struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Type             MachineType    `json:"type"`
	Location         string         `json:"location"`
	Status           MachineStatus  `json:"status"`
	Metrics          MachineMetrics `json:"metrics"`
	LastUpdated      time.Time      `json:"lastUpdated"`
	AssignedOperator string         `json:"assignedOperator,omitempty"`
}

type MetricHistoryPoint struct {
	Timestamp        time.Time `json:"timestamp"`
	Temperature      float64   `json:"temperature"`
	Load             float64   `json:"load"`
	Output           float64   `json:"output"`
	Vibration        float64   `json:"vibration"`
	PowerConsumption float64   `json:"powerConsumption"`
}

type DowntimeEntry struct {
	ID        string     `json:"id"`
	MachineID string     `json:"machineId"`
	StartTime time.Time  `json:"startTime"`
	EndTime   *time.Time `json:"endTime,omitempty"`
	Duration  *int       `json:"duration,omitempty"`
	Reason    string     `json:"reason"`
}

// ── Alert ────────────────────────────────────────────────────────────

type AlertType string

const (
	AlertThresholdExceeded AlertType = "threshold_exceeded"
	AlertMachineDown       AlertType = "machine_down"
	AlertMachineOffline    AlertType = "machine_offline"
	AlertMaintenanceDue    AlertType = "maintenance_due"
	AlertAnomalyDetected   AlertType = "anomaly_detected"
)

type AlertSeverity string

const (
	SeverityInfo     AlertSeverity = "info"
	SeverityWarning  AlertSeverity = "warning"
	SeverityCritical AlertSeverity = "critical"
)

type Alert struct {
	ID             string        `json:"id"`
	MachineID      string        `json:"machineId"`
	MachineName    string        `json:"machineName"`
	Type           AlertType     `json:"type"`
	Severity       AlertSeverity `json:"severity"`
	Message        string        `json:"message"`
	MetricName     string        `json:"metricName"`
	CurrentValue   float64       `json:"currentValue"`
	ThresholdValue float64       `json:"thresholdValue"`
	Timestamp      time.Time     `json:"timestamp"`
	Acknowledged   bool          `json:"acknowledged"`
	AcknowledgedBy string        `json:"acknowledgedBy,omitempty"`
	AcknowledgedAt *time.Time    `json:"acknowledgedAt,omitempty"`
}

type AlertThreshold struct {
	Metric        string  `json:"metric"`
	Label         string  `json:"label"`
	Unit          string  `json:"unit"`
	WarningValue  float64 `json:"warningValue"`
	CriticalValue float64 `json:"criticalValue"`
}

// ── Error log ────────────────────────────────────────────────────────

type ErrorType string

const (
	ErrEmergencyStop     ErrorType = "emergency_stop"
	ErrOverheating       ErrorType = "overheating"
	ErrMechanicalFailure ErrorType = "mechanical_failure"
	ErrElectricalFault   ErrorType = "electrical_fault"
	ErrSensorFailure     ErrorType = "sensor_failure"
	ErrCommunicationLost ErrorType = "communication_lost"
	ErrSoftwareError     ErrorType = "software_error"
)

type ErrorLogEntry struct {
	ID          string     `json:"id"`
	MachineID   string     `json:"machineId"`
	MachineName string     `json:"machineName"`
	ErrorCode   string     `json:"errorCode"`
	ErrorType   ErrorType  `json:"errorType"`
	Description string     `json:"description"`
	Timestamp   time.Time  `json:"timestamp"`
	ResolvedAt  *time.Time `json:"resolvedAt,omitempty"`
	ResolvedBy  string     `json:"resolvedBy,omitempty"`
	Duration    *int       `json:"duration,omitempty"`
	Impact      string     `json:"impact"`
}

// ── User ─────────────────────────────────────────────────────────────

type UserRole string

const (
	RoleOperator UserRole = "operator"
	RoleManager  UserRole = "manager"
	RoleAdmin    UserRole = "admin"
)

type User struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Email            string   `json:"email"`
	Role             UserRole `json:"role"`
	AssignedMachines []string `json:"assignedMachines,omitempty"`
}

// ── Reports ──────────────────────────────────────────────────────────

type ReportParams struct {
	DateFrom   time.Time `json:"dateFrom"`
	DateTo     time.Time `json:"dateTo"`
	MachineIDs []string  `json:"machineIds"`
	Metrics    []string  `json:"metrics"`
	GroupBy    string    `json:"groupBy"`
}

type TimeSeriesPoint struct {
	Timestamp      time.Time `json:"timestamp"`
	Output         float64   `json:"output"`
	Uptime         float64   `json:"uptime"`
	AvgTemperature float64   `json:"avgTemperature"`
	AvgLoad        float64   `json:"avgLoad"`
	ErrorCount     int       `json:"errorCount"`
}

type MachineReportRow struct {
	MachineID     string  `json:"machineId"`
	MachineName   string  `json:"machineName"`
	TotalOutput   float64 `json:"totalOutput"`
	UptimePercent float64 `json:"uptimePercent"`
	DowntimeHours float64 `json:"downtimeHours"`
	ErrorCount    int     `json:"errorCount"`
	Efficiency    float64 `json:"efficiency"`
}

type ReportSummary struct {
	TotalOutput   float64 `json:"totalOutput"`
	AvgUptime     float64 `json:"avgUptime"`
	TotalDowntime float64 `json:"totalDowntime"`
	TotalErrors   int     `json:"totalErrors"`
	Efficiency    float64 `json:"efficiency"`
}

type ReportData struct {
	GeneratedAt      time.Time          `json:"generatedAt"`
	Params           ReportParams       `json:"params"`
	Summary          ReportSummary      `json:"summary"`
	TimeSeries       []TimeSeriesPoint  `json:"timeSeries"`
	MachineBreakdown []MachineReportRow `json:"machineBreakdown"`
}

// ── Auth DTOs ────────────────────────────────────────────────────────

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type LoginResponse struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}
