# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [1.3.0] - 2026-11-17

### Added

- A 404 page for unknown addresses and missing records. Signed in, it appears inside the normal layout, shows the address and links to the overview; signed out, it is a small card linking to Sign in and the status page. A missing status page (`/s/...`) shows a plain card and is never framed. Partial refreshes, the JSON API, JSON callers and non-GET requests still get the plain-text 404.
- Skeleton loading states: a navigation that takes longer than 250 ms shows a placeholder shaped like the destination (overview, monitors, monitor detail, incidents, other pages), and Test URL shows a placeholder line while the test runs. The placeholders match the real page's layout, the sidebar highlights the destination, and the page comes back unchanged (scroll position included) on back/forward or if the navigation is stopped.

### Changed

- Subtle motion: pages fade in on load, a row whose status changed flashes for a moment on live refresh, status colours and buttons ease, and buttons give press feedback. All of it is off under `prefers-reduced-motion`.

## [1.2.1] - 2026-11-13

### Changed

- Sign-in and sign-up buttons show the Google and GitHub logos.

## [1.2.0] - 2026-11-12

### Added

- Sign in and sign up with Google or GitHub, optional and off until the `oauth` config key is set. Accounts are never linked by email: an existing address must sign in with its password, then connect the provider from Account.
- Account page "Sign-in methods": connect and disconnect Google and GitHub, and set or change a password.
- Operator panel users list shows a badge for each provider a user has connected.

### Changed

- Database schema v3, with an automatic `<db>.pre-v3-<time>.bak` backup before upgrading a database that has data.
- Passwords are optional for accounts created with a provider. Deleting the account, setting a password and disconnecting a provider then need a sign-in within the last 10 minutes. Connecting a provider needs one for every account (an account with a password confirms it), and a notice email is sent when a provider is connected or disconnected.

## [1.1.0] - 2026-11-12

### Added

- Lucide icons (ISC/MIT) across navigation, action buttons (icon with text), status badges, alerts, toasts, tables and empty states. The licence notice is in `THIRD_PARTY_NOTICES.md` and served at `/static/licenses.txt`.
- The theme toggle shows a sun, moon or monitor icon for light, dark or auto.

### Changed

- Icon motion follows reduced-motion and accessibility guidance:
  - Check spins only while a check runs, and only after 300ms.
  - Chevrons, sort arrows and the theme icon transition in 200ms or less.
  - The live indicator pulses briefly at most once a minute instead of continuously.
  - All motion stops under `prefers-reduced-motion` and pauses in background tabs.
- Pause and play icons are solid.
- Small icons use a slightly heavier stroke.
- The Docker image includes `LICENSE`, `THIRD_PARTY_NOTICES.md` and the Lucide licence under `/usr/share/doc/uptime-monitor/`.

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

Compared with pre-release builds:

- Status pages moved to `/s/{slug}`; `/status` redirects to the signed-in user's page or, for anonymous visitors, the operator's.
- Error alerts and form validation upgraded: server errors appear as an alert at the top of the form, and fields are checked inline with a message under each one.
- Toasts show an icon and a close button, and dismiss after 5 seconds; errors stay until closed.
- Custom dropdowns with keyboard and screen-reader support.
- Confirm dialog and side panel, both modal with focus handling.
- Data tables with sorting, search, quick views, paging, column controls, row selection with copy, and phone cards.

### Removed

Compared with pre-release builds:

- `insecure_no_auth` has no effect and logs a WARN if set. `admin_password` only unlocks setup on an upgraded install.

### Upgrade notes

An existing install enters setup mode on its first start and writes a
`.pre-accounts-<timestamp>.bak` backup of the database before migrating. See
"Upgrading" in the [README](README.md#upgrading) before upgrading.
