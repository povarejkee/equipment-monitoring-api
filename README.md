# Equipment Monitoring API (v3)

Go REST + WebSocket backend for the equipment monitoring platform.
PostgreSQL-backed data layer (12 machines, alerts, error log, 30d history)
with live metric updates broadcast every 4s over WebSocket.

## Run locally

Needs a Postgres instance. Quickest way to get one:

```bash
docker run -d --name equipment-monitoring-pg -e POSTGRES_PASSWORD=devpass \
  -e POSTGRES_DB=equipment_monitoring -p 55432:5432 postgres:16-alpine
```

Then:

```bash
export DATABASE_URL="postgres://postgres:devpass@localhost:55432/equipment_monitoring?sslmode=disable"
go run .              # applies migrations automatically, then listens on :8080
```

The server only *migrates* the schema on startup — it doesn't seed data.
On a fresh database, populate the demo dataset once (destructive — wipes
and repopulates the tables it touches):

```bash
go run ./cmd/seed
```

Env vars:
- `DATABASE_URL` — Postgres connection string (required)
- `PORT` — HTTP port (default `8080`)
- `ALLOWED_ORIGINS` — comma-separated CORS allowlist. Leaving it unset opens
  CORS to `*` (fine for local dev; the server logs a warning) — production
  (`render.yaml`) sets it explicitly to the deployed frontend origin.

## Database

- Schema: `internal/db/migrations/*.sql`, applied automatically at startup
  (tracked in a `schema_migrations` table — safe to run repeatedly).
- Seeding: `cmd/seed` — a separate program, not run automatically. Re-run it
  any time you want a fresh demo dataset (e.g. after the Render free
  Postgres plan resets).
- `Store` (`store.go`) is the only thing that talks to the DB; handlers
  never touch SQL directly.
- Report numbers (`GenerateReport`/`buildTimeSeries`) are aggregated from
  `metric_history`, `error_log`, and `downtimes` for the requested period
  — see "Reports" below.

## Auth

- Passwords are bcrypt-hashed and stored in `users.password_hash` — no
  hardcoded credentials in code anymore.
- Sessions live in the `sessions` table (`token_hash` = sha256 of the
  bearer token, `expires_at`, 24h TTL from login). `UserForToken` evicts
  expired sessions on lookup; a periodic sweep (see Maintenance) catches
  ones nobody ever looks up again. `POST /api/auth/logout` deletes the
  session.
- A DB failure while resolving a session is a `502`, never a `401` —
  `authMiddleware` distinguishes "no such session" from "couldn't check."
  Conflating the two would mean a transient DB blip signs every active
  user out (the frontend treats `401` as "log this user out").
- `GET /ws` requires the same bearer token as everything else (via
  `authMiddleware`) — previously a missing token skipped the check
  entirely instead of rejecting.
- User CRUD (`POST`/`PUT`/`DELETE /api/users`) is admin-only
  (`requireRole`); deleting a user unassigns their machines and cascades
  their sessions. Self-delete is blocked. Listing users
  (`GET /api/users`) is manager/admin (`requireRoles`) — operators don't
  get the full user list.
- `POST /api/auth/login` is rate-limited to 5 attempts/minute per real
  client IP (sliding window, in-memory) — see `clientIP` in `ratelimit.go`
  for why it trusts the *last* `X-Forwarded-For` entry, not the first.
  Over the limit → `429`.

## Access control

Enforced server-side, not just filtered in the frontend: an operator only
sees machines in their `assignedMachines` (and, transitively, alerts/
errors for those machines) — `GET /api/machines`, `.../history`,
`.../downtimes`, `.../alerts`, `GET /api/alerts`, and `GET /api/errors`
all apply this. An operator with an empty `assignedMachines` sees
nothing (fails closed). Manager and admin see everything. See
`canAccessMachine`/`scopedMachines` in `middleware.go`.

Not scoped: acknowledging an alert doesn't check whether the caller can
see that alert's machine.

## Tests

```bash
go test ./... -cover                                    # DB-dependent tests skip
TEST_DATABASE_URL=$DATABASE_URL go test ./... -cover    # runs everything
```

Covers `auth.go` (login, unknown/wrong password, session resolution,
session expiry, logout, DB-failure-vs-unauthorized), `store.go`'s
`updateStatus` and the alert engine (crossing opens an alert, repeated
ticks don't duplicate it, re-alerts after acknowledge, nothing fires in
range), `middleware.go` (`authMiddleware`, `corsMiddleware`,
`canAccessMachine`/`scopedMachines`), `hub.go` (concurrent
connect+broadcast under `-race`, a slow client doesn't stall delivery,
cleanup is idempotent), and `ratelimit.go` (limit enforcement, the sweep,
and specifically that a spoofed first `X-Forwarded-For` hop can't reset
the bucket). Auth/middleware/alert tests go through a real DB, so they
need `TEST_DATABASE_URL` and skip cleanly without it — `TestMain`
truncates and reseeds `users` (with a bcrypt hash of the demo password)
before each such test for isolation. Key-logic coverage is 90-100% on
these units; whole-package coverage is lower since handlers/seeding
aren't unit-tested.

A few of these regressions were verified by temporarily reverting the fix
and confirming the new test actually fails against the old code (the WS
crash and the X-Forwarded-For spoof, specifically) — not just that it
passes against the fix.

## WebSocket

Each client gets its own buffered send channel and a dedicated write-pump
goroutine — it's the only goroutine that writes to that connection.
Earlier, the HTTP handler wrote the initial snapshot directly while the
broadcast loop could write concurrently from a different goroutine;
gorilla/websocket allows only one writer and panics on a second,
which crashed the whole process (not just the request). Also: ping/pong
with a read deadline (dead connections used to linger forever), and a
client that falls more than 16 messages behind is dropped instead of
stalling delivery to everyone else.

## Alerts

Generated in real time by `Tick()` (`checkThresholdAlerts` in
`store.go`), not just at seed time — previously nothing ever inserted
into `alerts` after startup, so the list was frozen at whatever the seed
wrote. Each tick compares temperature/load/vibration against the
*current* `thresholds` row (read fresh, not cached, so a `PUT
/api/thresholds` takes effect on the next tick) and opens an alert when a
machine crosses into warning/critical. Hysteresis: a machine+metric with
an already-open (unacknowledged) alert doesn't get a duplicate every 4s;
acknowledging it lets a still-out-of-range value alert again next tick.
`machine_down`/`machine_offline`/`maintenance_due`/`anomaly_detected`
remain seed-only — those need a different signal than a threshold
crossing.

## Maintenance

A daily background loop (`main.go`) runs two cleanup jobs (also once at
startup, not after a full day's wait):
- `Store.PruneMetricHistory` — deletes rows older than 35 days.
  `Tick()` inserts one row per active machine every 4s with no
  retention otherwise, which is ~500MB/month — Render's free Postgres
  plan is capped at 1GB.
- `AuthManager.PruneExpiredSessions` — deletes sessions past `expires_at`
  that nobody looked up again to trigger the on-read eviction.

## Reliability

- Every `Store` method that reads/writes the DB returns an error instead
  of logging-and-returning-nil; handlers map that to `502` via
  `writeDBError`. Previously a DB hiccup made list endpoints return
  `null` with `200` — indistinguishable from "no data" for a monitoring
  product.
- `Tick()`'s broadcast is skipped (not sent as an empty payload) if the
  tick itself failed, so a DB blip can't overwrite every connected
  client's good data with nothing.
- Graceful shutdown: SIGTERM/SIGINT stop new ticks/maintenance and drain
  in-flight HTTP requests (15s) before the process exits with code 0.
  Previously there was no signal handling at all — every deploy or
  restart was a hard kill that could cut off a request or a `Tick()`
  transaction mid-write.

## Reports

`POST /api/reports` aggregates real data for `[dateFrom, dateTo]`, bucketed
by `groupBy` (`hour`/`day`/`week`/`month`, default `day`) starting at
`dateFrom` (buckets aren't calendar-aligned):

- **Output** — `SUM(output)` from `metric_history`.
- **AvgTemperature`/`AvgLoad`** — `AVG(...)` from `metric_history`.
- **ErrorCount** — `COUNT(*)` from `error_log`.
- **Uptime/UptimePercent** — `100 * (1 - downtime_minutes / window_minutes)`,
  where downtime is computed from `downtimes` interval overlap with the
  window (done in Go over one query per report, not per bucket).
- **Efficiency** — has no directly stored source, so it's a documented
  heuristic: uptime minus 0.5 percentage points per error, floored at 0.

## Logging

Structured JSON logs via `log/slog` (stdout). Logged: server start/stop,
failed login attempts (email + IP), WS upgrade failures, recovered panics
(with stack trace). A top-level `recoverMiddleware` catches panics from any
handler and returns `500` instead of crashing the process.

## Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/api/auth/login` | — | `{email,password}` → `{token,user}`; 5/min/IP |
| GET | `/api/health` | — | health check |
| GET | `/ws?token=<t>` | ✓ | WebSocket, pushes `{topic:"machines",payload}` |
| GET | `/api/auth/me` | ✓ | current user |
| POST | `/api/auth/logout` | ✓ | invalidate the current session |
| GET | `/api/machines` | ✓ scoped | machines (operator: only their assigned ones) |
| GET | `/api/machines/{id}` | ✓ scoped | one machine (403 if not assigned to caller) |
| GET | `/api/machines/{id}/history?hours=` | ✓ scoped | metric history |
| GET | `/api/machines/{id}/downtimes` | ✓ scoped | downtime log |
| GET | `/api/machines/{id}/alerts` | ✓ scoped | alerts for machine |
| GET | `/api/alerts?severity=&machine_id=&acknowledged=` | ✓ scoped | alerts, filtered (all optional) |
| POST | `/api/alerts/{id}/acknowledge` | ✓ | acknowledge one |
| POST | `/api/alerts/acknowledge-all` | ✓ | acknowledge all |
| GET | `/api/thresholds` | ✓ | alert thresholds |
| PUT | `/api/thresholds` | ✓ | update a threshold (rejects warning > critical) |
| GET | `/api/errors?limit=&offset=&machine_id=&from=&to=` | ✓ scoped | error log, paginated/filtered (all optional; total match count in `X-Total-Count`) |
| POST | `/api/reports` | ✓ | generate report |
| GET | `/api/users` | manager/admin | users |
| POST | `/api/users` | admin | create user `{name,email,password,role,assignedMachines?}` |
| PUT | `/api/users/{id}` | admin | partial update (any subset of the create fields) |
| DELETE | `/api/users/{id}` | admin | delete user (can't delete self) |

`from`/`to` on `/api/errors` are RFC3339 timestamps (e.g. `2026-07-01T00:00:00Z`).

Demo accounts (password `demo`): `operator@demo.com`, `manager@demo.com`, `admin@demo.com`.

## Deploy (Render)

`render.yaml` defines a free Docker web service plus a free Postgres
database, wired together via `DATABASE_URL` (Render fills it in). `docker
build -t em-api .` to build locally.

The free Postgres plan is empty on first deploy and resets periodically —
after provisioning (or any reset), seed it once using the external
connection string from the Render dashboard:

```bash
DATABASE_URL="<external connection string from Render>" go run ./cmd/seed
```
