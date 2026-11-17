# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [1.6.0] - 2026-11-17

### Added

- Settings page, opened from the gear item at the bottom of the sidebar: Profile (email, time zone), Security (password, sign-in methods, sign out everywhere), Notifications and Danger zone (delete account), each its own page under `/settings/` with a shared sub-navigation.
- **Set a password** for an account that signed up with Google or GitHub: with `mail` configured it emails a one-hour link to the account address and the password is chosen on the `/reset` page, which reads "Choose a password" for an account that has none. Limited to 3 links per hour per user, plus the existing per-address reset-mail budget.

### Changed

- Alert channels moved to Settings → Notifications; the sidebar no longer has an Alert channels item. The Account page is gone: its cards are in Profile, Security and Danger zone. The sidebar email is plain text, and the footer's time zone note links to Settings → Profile.
- With `mail` configured, a first password can no longer be set in place after a Google or GitHub sign-in (the POST is refused); use the emailed link. Without `mail` the earlier rule is unchanged.
- Emails that pointed at `/account` now point at `/settings/security`; connecting a provider and signing in again return to Settings → Security. The time zone report from the browser is sent to `/settings/tz-detected`.

### Deprecated

- `GET /account` and `GET /alerts` redirect (302) to `/settings/security` and `/settings/notifications`. The old POST paths (`/account/timezone`, `/account/tz-detected`, `/account/password`, `/account/signout-all`, `/account/delete`, `/account/identities/...`, `/alerts`, `/alerts/test`) remain as aliases and are removed in 1.7.0.
- No schema change.

## [1.5.0] - 2026-11-17

### Added

- Time zones. Each user has a Time zone setting on the Account page: Automatic (the zone the browser last reported, UTC until it has) or any IANA zone, shown with its current offset and searchable. Schema v4 adds `users.time_zone` and `users.detected_tz`; a database with data is backed up to `<db>.pre-v4-<timestamp>.bak` first. The browser reports its zone with `POST /account/tz-detected` and the page reloads only if the shown zone changed. The footer says which zone times are in.
- Only the zones in the list and their common aliases (`Asia/Calcutta`, `Europe/Kiev` and so on) are accepted; aliases show as their listed names. Saving and reporting a zone are each limited to 10 per minute per user.
- Zones are built into the binary (`time/tzdata`), so they work without a system tz database.

### Changed

- Times on signed-in pages, chart axes and the maintenance form use the viewer's zone instead of the server's. The maintenance start time is read in that zone (a time that falls in a daylight saving gap is refused, and so is a form shown before the zone changed), and the browser offset field is gone.
- The public status page shows times in the workspace owner's zone with its abbreviation. The 90-day daily bars keep UTC days and say so.
- System email (sign-in method notices) uses the recipient's zone, and alert emails the workspace owner's, written as "2026-11-17 16:30 IST (UTC+05:30)" instead of RFC 3339 UTC.
- The hourly availability cells are labelled with their start time in the viewer's zone (a UTC hour starts at :30 in some zones).
- The JSON API and logs stay in UTC.
- Scroll bars are thin and follow the light or dark theme. The page no longer scrolls sideways at any width (320 px and up): long monitor names, URLs, error messages, maintenance titles and status page titles wrap or truncate inside their own boxes, and tables scroll sideways only inside their own frame.
- Behind an open slide-over or dialog the page no longer scrolls, and scrolling inside them or inside a menu no longer scrolls the page. Dialogs taller than the screen scroll inside themselves.
- Narrow screens: the navigation wraps instead of scrolling, buttons, fields, tabs, pager buttons and menu rows are at least 40 px tall on phones and touch devices, form fields use 16 px text there (no zoom on focus), and menu option labels are cut with an ellipsis instead of widening the menu. A menu with a filter stays above the on-screen keyboard.
- On very wide screens the content column is centred next to the sidebar. Truncated monitor URLs and error text show the full value as a tooltip.
- Upgrading: until a user's browser has reported its zone, times show in UTC, which is what a Docker install already showed.

## [1.4.0] - 2026-11-17

### Added

- Fallback alert email through the server's system mail. A workspace with no email channel (no SMTP host) now gets alert emails sent from the server to the verified sign-in addresses of its members, never to an address typed into a form. On by default when `mail` is set; the Alert channels page has an Email box to turn it off per workspace, and "Save and send test" tests it. Budgets: 10 per hour and 30 per day per workspace, charged once per alert, also counted against the instance budget. Over a budget the alert is dropped with a WARN `event=alert_mail_dropped` and the Email card reads "Limit reached".
- `limits.mail_per_day`: a daily cap on all system email across the instance. Default `0` (unlimited); a negative value is refused at startup.

### Changed

- A refused SMTP login (reply 535, 530, 534 or 538) now shows "Login rejected — check the username and password" on the Alert channels page instead of the raw reply; the raw error stays in the log.
- An invitation not bound to an address no longer proves the address: when `mail` is set, an account created from it verifies its email like an open sign-up (and gets no alert emails until it does). Bound invitations, and instances without `mail`, are unchanged.
- System email budgets (`limits.mail_per_hour`, `limits.mail_per_day`) are charged once per recipient rather than once per message.
- Upgrading: workspaces with no SMTP host start receiving alert emails from the server (to their members' verified addresses) after the upgrade if the server has `mail` set. Untick Email on the Alert channels page to stop them.
- Fallback alerts from tenant workspaces may use only half of the instance mail budgets; a failure of the server's mail shows as "Server mail failed". A startup WARN appears when `signup` is `open`, `mail` is set and `limits.mail_per_day` is 0.

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
