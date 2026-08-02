-- Initial schema for the equipment monitoring platform. Column layout
-- mirrors the structs in models.go; JSON field names there are camelCase,
-- these are snake_case.

CREATE TABLE IF NOT EXISTS users (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL,
    email             TEXT NOT NULL UNIQUE,
    role              TEXT NOT NULL CHECK (role IN ('operator', 'manager', 'admin')),
    -- Populated once real auth (bcrypt + DB-backed login) replaces the
    -- hardcoded credential map in auth.go; unused until then.
    password_hash     TEXT,
    assigned_machines TEXT[] NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS machines (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL,
    type              TEXT NOT NULL CHECK (type IN ('cnc', 'lathe', 'milling', 'grinding', 'press')),
    location          TEXT NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('running', 'warning', 'error', 'idle', 'maintenance', 'offline')),
    assigned_operator TEXT REFERENCES users (id),
    temperature       DOUBLE PRECISION NOT NULL,
    load              DOUBLE PRECISION NOT NULL,
    output            DOUBLE PRECISION NOT NULL,
    uptime            DOUBLE PRECISION NOT NULL,
    power_consumption DOUBLE PRECISION NOT NULL,
    spindle_speed     DOUBLE PRECISION,
    vibration         DOUBLE PRECISION,
    last_updated      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS thresholds (
    metric         TEXT PRIMARY KEY,
    label          TEXT NOT NULL,
    unit           TEXT NOT NULL,
    warning_value  DOUBLE PRECISION NOT NULL,
    critical_value DOUBLE PRECISION NOT NULL
);

CREATE TABLE IF NOT EXISTS alerts (
    id              TEXT PRIMARY KEY,
    machine_id      TEXT NOT NULL REFERENCES machines (id),
    -- Denormalized snapshot of the machine name at alert time (matches the
    -- pre-DB in-memory model, which stored it the same way).
    machine_name    TEXT NOT NULL,
    type            TEXT NOT NULL CHECK (type IN ('threshold_exceeded', 'machine_down', 'machine_offline', 'maintenance_due', 'anomaly_detected')),
    severity        TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    message         TEXT NOT NULL,
    metric_name     TEXT NOT NULL,
    current_value   DOUBLE PRECISION NOT NULL,
    threshold_value DOUBLE PRECISION NOT NULL,
    timestamp       TIMESTAMPTZ NOT NULL,
    acknowledged    BOOLEAN NOT NULL DEFAULT false,
    acknowledged_by TEXT,
    acknowledged_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_alerts_machine_id ON alerts (machine_id);
CREATE INDEX IF NOT EXISTS idx_alerts_severity ON alerts (severity);
CREATE INDEX IF NOT EXISTS idx_alerts_acknowledged ON alerts (acknowledged);

CREATE TABLE IF NOT EXISTS error_log (
    id           TEXT PRIMARY KEY,
    machine_id   TEXT NOT NULL REFERENCES machines (id),
    machine_name TEXT NOT NULL,
    error_code   TEXT NOT NULL,
    error_type   TEXT NOT NULL CHECK (error_type IN ('emergency_stop', 'overheating', 'mechanical_failure', 'electrical_fault', 'sensor_failure', 'communication_lost', 'software_error')),
    description  TEXT NOT NULL,
    timestamp    TIMESTAMPTZ NOT NULL,
    resolved_at  TIMESTAMPTZ,
    resolved_by  TEXT,
    duration     INTEGER,
    impact       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_error_log_machine_id ON error_log (machine_id);
CREATE INDEX IF NOT EXISTS idx_error_log_timestamp ON error_log (timestamp);

CREATE TABLE IF NOT EXISTS metric_history (
    id                BIGSERIAL PRIMARY KEY,
    machine_id        TEXT NOT NULL REFERENCES machines (id),
    timestamp         TIMESTAMPTZ NOT NULL,
    temperature       DOUBLE PRECISION NOT NULL,
    load              DOUBLE PRECISION NOT NULL,
    output            DOUBLE PRECISION NOT NULL,
    vibration         DOUBLE PRECISION NOT NULL,
    power_consumption DOUBLE PRECISION NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_metric_history_machine_ts ON metric_history (machine_id, timestamp);

CREATE TABLE IF NOT EXISTS downtimes (
    id         TEXT PRIMARY KEY,
    machine_id TEXT NOT NULL REFERENCES machines (id),
    start_time TIMESTAMPTZ NOT NULL,
    end_time   TIMESTAMPTZ,
    duration   INTEGER,
    reason     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_downtimes_machine_id ON downtimes (machine_id);
