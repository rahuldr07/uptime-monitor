# Uptime monitor

A single-binary uptime monitor written in Go. It checks URLs on a schedule,
records every result in SQLite, opens and closes incidents, alerts over
Telegram, Slack or email when a service goes down or recovers, and serves a
dashboard, an admin UI and a public status page. No runtime dependencies, no
build step for the frontend.

## Features

- One goroutine per target, each with its own interval and timeout
- Status code and optional body keyword checks
- A target is marked down after N consecutive failures (default 2) and up on
  the first success, so a single blip does not page you
- Incidents with start, end, duration, cause and which channels were alerted
- Alerts retried three times with backoff; every attempt is logged
- Dashboard with heartbeat bars, 24h/7d uptime and live refresh
- Target detail with latency chart (24h/7d/30d), incidents and recent checks
- Targets and alert channels editable in the browser, no restart needed
- Public status page with 90-day history
- Optional admin password with rate-limited login
- JSON API for scripting
- State is restored on restart, so an ongoing incident is not re-alerted

## Quick start

```bash
cp config.example.json config.json
# edit targets; set UPTIME_ADMIN_PASSWORD or put a password in the file
go run . -config config.json
```

Open http://localhost:8080. The public status page is at `/status`.

With Docker:

```bash
mkdir -p data && cp config.example.json data/config.json
UPTIME_ADMIN_PASSWORD=choose-one docker compose up -d
```

## Configuration

`config.json` (environment variables in `${VAR}` form are expanded):

| Key | Default | Meaning |
|---|---|---|
| `listen` | `:8080` | address to serve on |
| `db` | `uptime.db` | SQLite file |
| `admin_password` | empty | protects everything except `/status` and `/healthz`; empty means no login |
| `trust_proxy` | `false` | behind a reverse proxy, read the client IP from `X-Forwarded-For` for login rate limiting |
| `failure_threshold` | `2` | consecutive failures before a target is down |
| `interval`, `timeout` | `30s`, `10s` | defaults for targets |
| `retention` | `168h` | checks older than this are pruned hourly |
| `targets[]` | | `name`, `url`, optional `interval`, `timeout`, `expect_status`, `keyword` |
| `notify` | | `telegram {bot_token, chat_id}`, `slack {webhook_url}`, `email {smtp_host, smtp_port, username, password, from, to[]}` |

Targets in the config file are added to the database on first start. After
that the browser is the source of truth: edits and additions made in the UI
are kept across restarts and config targets are only inserted when a target
with that name does not already exist. Alert settings work the same way.

## API

All endpoints need the admin session cookie when a password is set.

- `GET /api/status` current state and uptime for every target
- `GET /api/targets/{id}/checks?range=24h|7d|30d` raw checks
- `GET /api/incidents?target={id}&ongoing=1` incidents
- `GET /healthz` liveness

## Layout

```
main.go               wiring, seeding, shutdown, retention loop
internal/config       config file parsing and validation
internal/store        SQLite schema and queries
internal/monitor      scheduler, HTTP probe, state machine, alert dispatch
internal/notify       Telegram, Slack, email, log channels
internal/web          handlers, templates, auth, SVG charts
deploy/               systemd unit
```

## Development

```bash
go test -race ./...
go vet ./...
```

## Notes

- SQLite runs in WAL mode through a pure-Go driver, so the binary cross-compiles without cgo.
- Secrets saved from the Alerts page are stored in the database in plain text. Keep the database file private.
- Login sessions are random tokens kept in memory for 30 days; a restart logs everyone out and logout revokes the token immediately.
- State-changing requests are rejected when the browser reports a different origin, so the UI is safe from cross-site form posts even with no password set.
