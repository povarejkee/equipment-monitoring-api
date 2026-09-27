-- Supports manual machine status changes (put into/out of maintenance or
-- offline) and scheduled maintenance reminders. Previously a machine that
-- entered maintenance/offline (only ever set by the seed script) had no
-- way back — Tick() explicitly skips those statuses, and there was no
-- endpoint to change them.
ALTER TABLE machines ADD COLUMN IF NOT EXISTS next_maintenance_at TIMESTAMPTZ;
