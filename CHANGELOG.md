# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [1.0.0] - 2026-11-12

First public release.

### Added

Monitoring and alerts

- One goroutine per target with its own interval and timeout; status code and optional body keyword checks.
- A target goes down after N consecutive failures (default 2) and up on the first success. State is restored on restart, so an ongoing incident is not re-alerted.
- Incidents with start, end, duration, cause and the channels alerted, plus acknowledgement and notes.
- Alerts over Telegram, Slack or email, delivered concurrently and tried 3 times with backoff; every attempt is logged.
- Maintenance windows for the whole organisation or one group: no incident, no alert, excluded from uptime, announced on the status page.
- Public status page with 90-day history, a configurable title and description, and monitors grouped by name.
- Dashboard with heartbeat bars and 24h/7d uptime, a monitor detail page with a latency chart (24h/7d/30d), and live updates over Server-Sent Events.
- Targets and alert channels editable in the browser, a "Test URL" probe before saving, per-monitor alert muting and per-channel enable toggles.
- Daily uptime rollups kept for 400 days, independent of raw-check retention.
- JSON API, `/healthz`, `-backup`, `-version`, JSON log format, config file discovery.
- Docker image (non-root, read-only root filesystem, health check), `docker-compose.yml` and a hardened systemd unit.

Accounts and workspaces

- Email and password sign-in (argon2id) with database-backed sessions and rate limits that slow attackers without locking owners out.
- First-run setup flow with a one-time setup link; `-setup-link` prints a fresh one.
- Sign-up modes `closed`, `invite` and `open`, with one-time invitations (`-invite`).
- System email (`mail` config) for verification, forgot/reset password and notices; per-recipient and per-instance budgets.
- Workspaces, each with its own monitors, alert channels and status page at `/s/{slug}`, whose address the owner can change.
- Per-workspace limits (monitors, minimum interval, per-host check rate, rate limits) and the `allow_private_targets` guard against internal addresses, with restricted alert channels in non-operator workspaces.
- Account deletion from `/account`.
- Operator panel at `/admin`: users (disable, enable, delete, resend verification, reset link) and invites.
- Recovery flags `-reset-password` and `-enable-user`.

Interface

- Opencode-inspired theme: monospace type, tighter radii, flat toasts and banners, light and dark palettes.

### Changed

- Status pages moved to `/s/{slug}`; `/status` redirects to the signed-in user's page or, for anonymous visitors, the operator's.
- Error alerts and form validation upgraded: server errors appear as an alert at the top of the form, and fields are checked inline with a message under each one.
- Toasts show an icon and a close button, and dismiss after 5 seconds; errors stay until closed.
- Custom dropdowns with keyboard and screen-reader support.
- Confirm dialog and side panel, both modal with focus handling.
- Data tables with sorting, search, quick views, paging, column controls, row selection with copy, and phone cards.

### Removed

- `insecure_no_auth` has no effect and logs a WARN if set. `admin_password` only unlocks setup on an upgraded install.

### Upgrade notes

An existing install enters setup mode on its first start and writes a
`.pre-accounts-<timestamp>.bak` backup of the database before migrating. See
"Upgrading" in the [README](README.md#upgrading) before upgrading.
