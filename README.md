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
- Report numbers (`GenerateReport`/`buildTimeSeries`) are still simulated
  placeholders, not aggregated from `metric_history`/`downtimes` yet — real
  aggregation is a separate change.

## Security

- `POST /api/auth/login` is rate-limited to 5 attempts/minute per IP
  (sliding window, in-memory). Over the limit → `429`.

## Tests

```bash
go test ./... -cover                                    # DB-dependent tests skip
TEST_DATABASE_URL=$DATABASE_URL go test ./... -cover    # runs everything
```

Covers `auth.go` (login, unknown/wrong password, token resolution, token
expiry), `store.go`'s `updateStatus` (status transitions from
temperature/load/vibration), and `middleware.go` (`authMiddleware`,
`corsMiddleware`). Auth/middleware tests now go through a real `Store`
(since `Login` resolves users via the DB), so they need `TEST_DATABASE_URL`
and skip cleanly without it — `TestMain` truncates and reseeds the `users`
table before each such test for isolation. Key-logic coverage is 90-100% on
these units; whole-package coverage is lower since handlers/hub/seeding
aren't unit-tested.

Bearer tokens now carry a 24h TTL (in-memory) instead of never expiring —
minimal groundwork so expiry is testable; full JWT/DB-session auth is a
separate, larger change.

## Logging

Structured JSON logs via `log/slog` (stdout). Logged: server start/stop,
failed login attempts (email + IP), WS upgrade failures, recovered panics
(with stack trace). A top-level `recoverMiddleware` catches panics from any
handler and returns `500` instead of crashing the process.

## Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/api/auth/login` | — | `{email,password}` → `{token,user}` |
| GET | `/api/health` | — | health check |
| GET | `/ws?token=<t>` | opt | WebSocket, pushes `{topic:"machines",payload}` |
| GET | `/api/machines` | ✓ | all machines |
| GET | `/api/machines/{id}` | ✓ | one machine |
| GET | `/api/machines/{id}/history?hours=` | ✓ | metric history |
| GET | `/api/machines/{id}/downtimes` | ✓ | downtime log |
| GET | `/api/machines/{id}/alerts` | ✓ | alerts for machine |
| GET | `/api/alerts?severity=&machine_id=&acknowledged=` | ✓ | alerts, filtered (all optional) |
| POST | `/api/alerts/{id}/acknowledge` | ✓ | acknowledge one |
| POST | `/api/alerts/acknowledge-all` | ✓ | acknowledge all |
| GET | `/api/thresholds` | ✓ | alert thresholds |
| PUT | `/api/thresholds` | ✓ | update a threshold |
| GET | `/api/errors?limit=&offset=&machine_id=&from=&to=` | ✓ | error log, paginated/filtered (all optional; total match count in `X-Total-Count`) |
| POST | `/api/reports` | ✓ | generate report |
| GET | `/api/users` | ✓ | users |

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
