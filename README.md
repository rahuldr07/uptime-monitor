# Uptime monitor

A single-binary uptime monitor written in Go. It checks URLs on a schedule,
records every result in SQLite, opens and closes incidents, alerts over
Telegram, Slack or email when a service goes down or recovers, and serves a
dashboard, an admin UI and a public status page. No runtime dependencies, no
build step for the frontend.

## Features

- Sidebar app shell with light, dark and auto themes, toasts, and keyboard shortcuts (`n` new monitor, `/` search, `t` theme)
- First-run guide, and a slide-over to add a monitor with a "Test URL" probe before saving
- Live updates on every page over Server-Sent Events, no reloads
- Incident acknowledgement and notes; per-monitor alert muting; per-channel enable toggle with last-delivery status
- Public status page with a configurable title and description, and monitors grouped by name
- Maintenance windows for the whole organisation or one group: planned downtime opens no incident, sends no alert, is left out of uptime, and is announced on the status page

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
- Accounts: sign in with email and password, database-backed sessions, rate-limited sign-in
- JSON API for scripting
- State is restored on restart, so an ongoing incident is not re-alerted

## Quick start

```bash
cp config.example.json config.json
# edit targets, then:
go run . -config config.json
```

Open http://localhost:8080. The public status page is at `/s/default`; `/status` redirects to it.

### Accounts

Sign in with email and password. On first run there are no users, and the
server logs a setup link at WARN (`setup required: open …/setup?token=…`).
Open it to create the operator account. If you missed it, run
`uptime-monitor -setup-link` for a fresh link. Only one setup token is valid
at a time: each server start in setup mode, and each `-setup-link`, replaces the previous link. A link expires after 24 hours. Setup adopts workspace 1 and renames
it; the config's `targets` and `notify` are seeded into it afterwards.

- **Passwords** are at least 10 characters and at most 256 bytes, hashed with argon2id.
- **Sessions** are stored in the database. They expire 30 days after sign-in
  or after 7 days idle, and a user has at most 20. "Sign out everywhere" on
  `/account` revokes them all. Changing the password keeps only the current
  session. A password reset signs the user out everywhere, then signs them back in.
- **Cookies.** The session cookie is `uptime_session`, or `__Host-uptime_session`
  (Secure) when the request is HTTPS or `base_url` is https. A device cookie,
  `uptime_device` (`__Host-uptime_device` under the same rule), lasts 90 days
  and exempts a browser that has signed in before from the per-email slowdown
  below. Its HMAC key is created per process, so a restart invalidates device
  cookies (sessions are in the database and survive a restart).
- **Lost password.** `uptime-monitor -reset-password you@example.com` prints a
  link that works for 1 hour. It refuses unknown and disabled users.
- **Plain HTTP.** With an https `base_url` the session cookie is `__Host-`
  and Secure, so signing in over plain HTTP (for example
  `http://<lan-ip>:8080`) silently fails. Use the https address.
- **Links.** `base_url` is the external URL used in the setup and reset links.
  Without it the links are built from `listen`.

Rate limits are kept in memory, so they reset on restart; run one server
process only. IPv6 clients are grouped by /64 (logs still show the full address).

| What | Limit |
|---|---|
| Sign-in failures | 5 per minute per address; 10 per 15 minutes per email and address |
| Sign-in slowdown | after 20 failures in 15 minutes for one email, at most 1 attempt every 5 seconds. This slows sign-in down but never locks the account |
| Setup attempts | 5 per 15 minutes per address |
| Current-password checks | 5 per 15 minutes per user |
| JSON API | 120 per minute per session |
| Live updates | 5 streams per user |
| Status pages | 120 per minute per address, at most 2 rebuilds at once |
| Password hashing | 4 at once, 64 waiting; a request that can't queue, or waits more than 2 seconds, gets 503 |

A restart invalidates device cookies, so during an attack the per-email
slowdown can then delay the owner's own sign-in. Recover with
`uptime-monitor -reset-password <email>`: its link signs you in without going
through the sign-in limits.

Deleted data stays in backups and in `.pre-accounts-*.bak` files until you
delete those files. Account deletion arrives in a later release.

With Docker:

```bash
cp config.example.json config.json   # keep it readable (0644): the container reads it as a non-root user
docker compose up -d
docker compose logs uptime | grep 'setup required'   # the setup link
# or: docker compose exec uptime uptime-monitor -setup-link
```

The config is mounted read-only at `/etc/uptime-monitor/config.json` and the
database lives in the `uptime-monitor-data` volume at `/data`. The container
runs as a non-root user with a read-only root filesystem and a health check
on `/healthz` (which assumes the default `listen` of `:8080`). If a reverse
proxy on the same host fronts it, publish the port as `127.0.0.1:8080:8080`
(see the comment in `docker-compose.yml`).

## Upgrading

From a release with an admin password:

- **Setup mode.** An existing install with data enters setup mode on the first
  start. Monitoring keeps running.
- **Unlock.** Open the setup link from the log (or `-setup-link`), or unlock
  setup with the old `admin_password` (upgraded installs only). Setup adopts
  all existing data into workspace 1 and you become the operator.
- **After setup** `admin_password` is ignored and a WARN is logged. Remove it.
- **`insecure_no_auth` is removed.** If it is still set, a WARN is logged.
- **LAN monitoring keeps working**, because the operator owns workspace 1 and
  `allow_private_targets` defaults to `operator`.

From a release before workspaces:

- **Backup file.** When the database already has data, the first start writes
  `<db>.pre-accounts-<timestamp>.bak` (mode `0600`) before migrating. Keep it
  until you have checked the upgrade, then delete it, because it holds alert
  secrets.
- **Status page.** `/status` now redirects to `/s/default`. Existing links
  keep working.
- **Alert settings** saved in the browser now belong to the workspace.
- **Status page rate limit.** Status pages (`/s/{slug}`) allow 120 requests
  per minute per client address, and at most 2 page rebuilds run at once.
  The client address comes from `X-Forwarded-For` (or `X-Real-IP`) only when
  `trust_proxy` is set. Behind a reverse proxy without it, every visitor
  shares one limit, so proxied installs should set `trust_proxy`.

From a release before the production hardening:

- **Empty password.** (Superseded: since accounts the server starts without
  a password and sign-in is always required, so `insecure_no_auth` is gone.)
  Before that, the server refused to start with an empty `admin_password` on a
  non-loopback `listen`.
- **`retention`** must be at least `24h`.
- **Docker, original layout** (`./data:/data` with `data/config.json`): it keeps
  working. Without `-config` the binary uses `/data/config.json` when it
  exists. To keep that layout with the new `docker-compose.yml`, replace its
  two volume lines with `- ./data:/data`.
- **Docker: the password is now required.** (Superseded: since accounts the
  password is optional and only unlocks setup on an upgraded install, so
  the env file is optional.) The old `docker-compose.yml` fell back to the
  password `change-me`, and for a while the new one refused to run any command
  until `UPTIME_ADMIN_PASSWORD` was set.

- **Docker, moving to the new layout** (named volume `uptime-monitor-data`,
  config outside it), (optionally) after creating `.env`:

  ```bash
  docker compose down
  cp data/config.json ./config.json && chmod 644 config.json
  docker compose up --no-start   # creates the uptime-monitor-data volume
  docker run --rm -v "$PWD/data:/old:ro" -v uptime-monitor-data:/data alpine \
    sh -c 'cp -a /old/uptime.db* /data/ && chown -R 10001:10001 /data'
  docker compose up -d
  ```

  The first start builds the daily uptime rollups from existing checks and
  logs when it is done.

## Configuration

`config.json` (environment variables in `${VAR}` form are expanded):

| Key | Default | Meaning |
|---|---|---|
| `listen` | `:8080` | address to serve on |
| `db` | `uptime.db` | SQLite file |
| `admin_password` | empty | only unlocks setup on an upgraded install (one that had data before accounts); ignored, with a WARN, once an account exists |
| `insecure_no_auth` | `false` | removed; has no effect and logs a WARN if set |
| `base_url` | empty | external URL, e.g. `https://status.example.com`: absolute http(s), no path, userinfo or query. Used for the setup and reset links; an https value makes session cookies always Secure |
| `signup` | `closed` | only `closed` is accepted for now. The `mail` and `limits` keys are rejected until later releases |
| `trust_proxy` | `false` | behind a reverse proxy, trust `X-Forwarded-For` (sign-in and status page rate limiting) and `X-Forwarded-Proto` (secure cookies) |
| `trusted_proxies` | `1` | how many proxies are in front (e.g. `2` for a CDN plus nginx); the client address is read that many entries from the right of `X-Forwarded-For`, because entries further left are supplied by the client |
| `allow_private_targets` | `operator` | `none`, `operator` or `all`. Which workspaces may reach loopback, private, link-local (cloud metadata) and other internal addresses through monitors, Test URL and alert channels. Until other workspaces exist everything belongs to the operator, so only `none` changes anything: it blocks all internal destinations. |
| `failure_threshold` | `2` | consecutive failures before a target is down |
| `interval`, `timeout` | `30s`, `10s` | defaults for targets |
| `retention` | `168h` | raw checks older than this are pruned hourly; they feed the latency charts and recent-check lists (minimum `24h`) |
| `targets[]` | | `name`, `url`, optional `interval`, `timeout`, `expect_status`, `keyword`, `group` |
| `notify` | | copied into workspace 1 once, if no alert settings have been saved yet. `telegram {bot_token, chat_id}`, `slack {webhook_url}`, `email {smtp_host, smtp_port, username, password, from, to[]}` |

Uptime is also rolled up per day and kept for 400 days, independent of
`retention`, so the 30-day uptime figures and the 90-day status page history
stay accurate with a short raw retention. A channel whose fields are all
empty (for example `${TELEGRAM_BOT_TOKEN}` with the variable unset) is
treated as not configured.

Command-line flags:

| Flag | Meaning |
|---|---|
| `-config path` | config file (default: the first that exists of `./config.json`, `/etc/uptime-monitor/config.json`, `/etc/uptime-monitor.json`) |
| `-log-format text\|json` | log output format (default `text`; the Docker image uses `json`) |
| `-backup path` | write a consistent copy of the database to `path` (which must not exist) and exit; safe while the monitor is running; fails if the configured database does not exist |
| `-setup-link` | during first-run setup, print a fresh setup link (replacing the previous one) and exit; fails once setup is complete |
| `-reset-password email` | print a password reset link, valid for 1 hour, for that user and exit; fails for unknown and disabled users |
| `-version` | print the version and exit |

Targets in the config file are added to the database on first start. After
that the browser is the source of truth: edits and additions made in the UI
are kept across restarts and config targets are only inserted when a target
with that name does not already exist. Config targets and alert settings are added to workspace 1; see `notify` above.

## API

All endpoints need a signed-in session cookie.

- `GET /s/{slug}` (public) the public status page (`/status` redirects to `/s/default`)
- `GET /api/status` current state and uptime for every target
- `GET /api/targets/{id}/checks?range=24h|7d|30d` raw checks
- `GET /api/incidents?target={id}&ongoing=1` incidents, including acknowledgement and notes
- `GET /api/maintenance` active and upcoming maintenance windows
- `GET /events` Server-Sent Events stream of state changes (signed-in session required)
- `GET /healthz` health check: `200 ok` when the database answers, `503` otherwise

## Layout

```
main.go               wiring, seeding, shutdown, retention loop
internal/config       config file parsing and validation
internal/store        SQLite schema and queries
internal/monitor      scheduler, HTTP probe, state machine, alert dispatch
internal/netguard     blocks internal destinations for monitors, Test URL and alert channels
internal/notify       Telegram, Slack, email, log channels
internal/web          handlers, templates, sessions, sign-in, SVG charts
internal/password     argon2id hashing under a concurrency bound
deploy/               hardened systemd unit
```

## Running in production

1. Run behind a TLS-terminating reverse proxy (Caddy, nginx, Traefik) and set
   `"trust_proxy": true` so per-client sign-in and status
   page rate limiting work (without it, all visitors share one status page
   limit). The proxy must append the client address to `X-Forwarded-For` (Caddy
   and Traefik do by default; in nginx use
   `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`). A proxy
   that only sets `X-Real-IP` and passes the client's own `X-Forwarded-For`
   through is not safe with `trust_proxy`. Do not set it if clients can reach
   the server directly. Configure HSTS at the proxy if
   you want it.
2. Set `base_url` to your https address (e.g. `"base_url": "https://status.example.com"`)
   so session cookies are always Secure and the setup and reset links are
   correct. The server warns at startup when `trust_proxy` is set without an
   https `base_url`.
3. Back up the database on a schedule, e.g. daily from cron:

   ```bash
   # bare metal / systemd: run from the state directory, because a relative
   # "db" path in the config is resolved against the working directory
   cd /var/lib/uptime-monitor && uptime-monitor -config /etc/uptime-monitor.json -backup /backups/uptime-$(date +%F).db
   # docker compose
   docker compose exec uptime uptime-monitor -backup /data/backup-$(date +%F).db
   ```

   The command prints the absolute path of the database it copied and fails
   if that database does not exist, so a wrong path cannot produce an empty
   backup. Backups are created with mode `0600`.

4. Point your orchestrator or external monitor at `/healthz`.
5. For systemd: create the user (`useradd --system uptime`), install the binary to
   `/usr/local/bin`, the config to `/etc/uptime-monitor.json`, secrets to
   `/etc/uptime-monitor.env` (mode 600), then `systemctl enable --now uptime-monitor`.

Build with a version stamp: `go build -ldflags "-X main.version=1.2.3" .` or
`docker compose build --build-arg VERSION=1.2.3`.

## Development

```bash
go test -race ./...
go vet ./...
```

## Notes

- SQLite runs in WAL mode through a pure-Go driver, so the binary cross-compiles without cgo.
- Secrets saved from the Alerts page are stored in the database in plain text. The database (and backups) are created with mode `0600`; keep them private. Saved secrets are never sent back to the browser: the Alerts page shows them as "Saved", a blank field keeps the stored value, and "Remove these settings" deletes a channel.
- Sessions are random tokens; only a hash is stored in the database, so a restart does not sign anyone out and revoking a session takes effect immediately.
- State-changing requests are rejected when the browser reports a different origin, so the UI is safe from cross-site form posts. Request bodies are capped at 1 MB.
- Every response carries a strict Content-Security-Policy (no inline scripts) and `nosniff`. Admin pages refuse to be framed; the public status page (`/s/{slug}`) may be embedded in other sites (a framed page is always the anonymous view, because the session cookie is not sent to cross-site frames). HSTS is left to the reverse proxy.
- Flash messages in `?msg=` are signed by the server, so a crafted link cannot show attacker-chosen text in the admin UI.
- Static assets are linked with a content hash (`app.js?v=…`), so browsers pick up new code immediately after an upgrade.
- The public status page is cached for 10 seconds for anonymous visitors and rebuilt by one request at a time while others get the previous copy (at most 2 rebuilds run at once, and each client address is limited to 120 requests per minute), so heavy or slow traffic cannot crowd out check recording or stall other visitors. The workspace owner always sees live data.
- Passwords are hashed with argon2id, and failed sign-ins are rate limited per address and per email. Secrets are never logged, except the setup and reset links, which are printed on purpose.
- Email uses implicit TLS on port 465 and STARTTLS elsewhere, never sends credentials over an unencrypted connection, and gives up on a stalled server after 30 seconds. Alert channels are delivered in parallel, so one slow channel does not delay the others.
