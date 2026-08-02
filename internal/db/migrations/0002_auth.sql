-- DB-backed sessions replace the in-memory opaque-token map in auth.go.
-- token_hash is sha256(token) — the raw bearer token is never stored, only
-- returned to the client at login.
CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions (expires_at);

-- Deleting a user (admin user-management CRUD) should unassign their
-- machines rather than fail with a FK violation.
ALTER TABLE machines DROP CONSTRAINT machines_assigned_operator_fkey;
ALTER TABLE machines
    ADD CONSTRAINT machines_assigned_operator_fkey
    FOREIGN KEY (assigned_operator) REFERENCES users (id) ON DELETE SET NULL;
