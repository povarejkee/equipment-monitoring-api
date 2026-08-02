# Equipment Monitoring API (v2)

Go REST + WebSocket backend for the equipment monitoring platform.
In-memory data layer (12 machines, alerts, error log, 30d history) with
live metric updates broadcast every 4s over WebSocket.

## Run locally

```bash
go run .
# → listening on :8080
```

Env vars:
- `PORT` — HTTP port (default `8080`)
- `ALLOWED_ORIGINS` — comma-separated CORS allowlist. Leaving it unset opens
  CORS to `*` (fine for local dev; the server logs a warning) — production
  (`render.yaml`) sets it explicitly to the deployed frontend origin.

## Security

- `POST /api/auth/login` is rate-limited to 5 attempts/minute per IP
  (sliding window, in-memory). Over the limit → `429`.

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
| GET | `/api/alerts` | ✓ | all alerts |
| POST | `/api/alerts/{id}/acknowledge` | ✓ | acknowledge one |
| POST | `/api/alerts/acknowledge-all` | ✓ | acknowledge all |
| GET | `/api/thresholds` | ✓ | alert thresholds |
| PUT | `/api/thresholds` | ✓ | update a threshold |
| GET | `/api/errors` | ✓ | error log (50 entries) |
| POST | `/api/reports` | ✓ | generate report |
| GET | `/api/users` | ✓ | users |

Demo accounts (password `demo`): `operator@demo.com`, `manager@demo.com`, `admin@demo.com`.

## Deploy (Render)

`render.yaml` defines a free Docker web service. Set `ALLOWED_ORIGINS`
to the frontend URL. `docker build -t em-api .` to build locally.
