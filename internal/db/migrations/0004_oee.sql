-- OEE (overall equipment effectiveness) = availability × performance ×
-- quality. Availability comes from the downtimes table; performance needs
-- a rated (ideal) output per machine; quality needs scrap, which nothing
-- recorded before.

-- Ideal output in parts/hour. Seeded per machine type as a sensible
-- default; managers tune it per machine via the API.
ALTER TABLE machines ADD COLUMN IF NOT EXISTS rated_output DOUBLE PRECISION NOT NULL DEFAULT 50 CHECK (rated_output > 0);
UPDATE machines SET rated_output = CASE type
    WHEN 'cnc' THEN 48
    WHEN 'lathe' THEN 50
    WHEN 'milling' THEN 46
    WHEN 'grinding' THEN 44
    WHEN 'press' THEN 55
    ELSE 50 END;

CREATE TABLE IF NOT EXISTS scrap_records (
    id          TEXT PRIMARY KEY,
    machine_id  TEXT NOT NULL REFERENCES machines (id) ON DELETE CASCADE,
    count       INTEGER NOT NULL CHECK (count > 0),
    reason      TEXT NOT NULL DEFAULT '',
    occurred_at TIMESTAMPTZ NOT NULL,
    -- Kept (as NULL) if the user who logged it is later deleted.
    recorded_by TEXT REFERENCES users (id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_scrap_machine_time ON scrap_records (machine_id, occurred_at);
