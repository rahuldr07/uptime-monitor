# Product overview

This document describes what uptime-monitor is, who it is for, what it does
today and what is planned. Every decision behind it, with its reasons, is in
[decisions.md](decisions.md). Detailed designs are in [superpowers/specs/](superpowers/specs/).

## What it is

uptime-monitor is a self-contained uptime monitoring service:

- it checks websites and HTTP endpoints on a schedule
- it records every result and opens and closes incidents automatically
- it alerts by Telegram, Slack (or compatible webhooks) and email when something
  goes down or recovers
- it serves a dashboard and a public status page

It ships as **one binary** with an embedded SQLite database. There is no runtime
dependency, no separate database server and no frontend build step.

## Who it is for

It is being released as an **open-source project** and will be run in two ways:

| Who | How they run it |
|---|---|
| **Public instance operator** (the maintainer) | Runs one hosted instance where strangers sign up and get their own private workspace. Needs abuse protection, email verification and account isolation. |
| **Self-hosters** | Run it for themselves, their team or their customers, often monitoring services on their own network. Need easy setup, invite-only or closed sign-up, and LAN monitoring. |

## Principles

- **Single binary, single process.** SQLite in WAL mode through a pure-Go
  driver, so it cross-compiles without cgo. Only one server process per
  database is supported.
- **Safe by default.** Sign-up is closed by default, internal addresses are
  blocked for non-operator workspaces, and secrets are never sent back to the
  browser.
- **Upgrades never lose data.** Schema changes are versioned, run in
  transactions, and are preceded by an automatic backup when data exists.
- **Small dependencies.** Only `modernc.org/sqlite` today, plus
  `golang.org/x/crypto` for password hashing once accounts ship.
- **Follow the codebase's patterns.** Small files split by area, tests next to
  the code using real SQLite, `Options` structs for wiring, one shared
  implementation of each building block (rate limiter, tokens, error
  handling).

## Features today

**Monitoring**

- One goroutine per monitor, each with its own interval and timeout.
- Checks: HTTP status (any 2xx/3xx, or an exact code) and an optional keyword
  that must appear in the body.
- A monitor goes down after N consecutive failures (default 2) and up on the
  first success, so one blip doesn't page anyone.
- Incidents with start, end, duration, cause, the channels alerted,
  acknowledgement and notes.
- State is restored after a restart, so an ongoing incident is not alerted
  again.
- Maintenance windows, for everything or for one group: no incident, no alert,
  excluded from uptime, announced on the status page.
- Per-monitor alert muting and pausing.

**Alerts**

- Telegram, Slack-compatible webhooks and email (port 465 implicit TLS,
  otherwise STARTTLS; passwords never sent unencrypted; 30-second timeout).
- Each delivery is retried three times, every attempt is recorded, and channels
  are delivered in parallel.
- Channels can be enabled, disabled, tested and removed in the browser.

**Dashboard and status page**

- Sidebar app with light, dark and auto themes, keyboard shortcuts, and live
  updates over Server-Sent Events.
- Per-monitor latency charts (24h/7d/30d), hourly availability and recent
  checks.
- 24h, 7-day and 30-day uptime; the 30-day figure and the 90-day status history
  come from daily summaries kept for 400 days.
- A public status page with a configurable title and description, grouped
  monitors and upcoming maintenance. It can be embedded in other sites and is
  cached for 10 seconds for anonymous visitors.
- A JSON API (`/api/status`, `/api/targets/{id}/checks`, `/api/incidents`,
  `/api/maintenance`) and `/healthz`, which also checks the database.

**Security and operations**

- One admin password, with rate-limited login that works correctly behind
  reverse proxies (`trust_proxy`, `trusted_proxies`).
- Strict Content-Security-Policy (no inline scripts), cross-site request
  checks, signed flash messages and 1 MB request limits.
- Owner-only (0600) database and backup files; `-backup` takes a consistent
  copy while running.
- Docker image (non-root, read-only filesystem, health check), Docker Compose
  file, hardened systemd unit, and CI (format, vet, race tests, vulnerability
  scan, Docker build).

## Planned: accounts (in design)

Designed in
[2026-11-10-accounts-design.md](superpowers/specs/2026-11-10-accounts-design.md).

- **Sign-up and sign-in** with email and password (argon2id), database-backed
  sessions (30-day absolute expiry, 7-day idle), and sign-out everywhere.
- **Workspaces:** each account gets a private workspace with its own monitors,
  incidents, alert channels, maintenance windows and status page at
  `/s/<slug>`. The schema already allows teammates to be added later.
- **Sign-up modes:** `closed` (default), `invite` (operator invite links) and
  `open` (requires email verification, so system mail must be configured).
- **First run:** a protected setup page, unlocked by a one-time link in the
  server log, creates the operator. On an upgraded install the operator adopts
  all existing data.
- **Email (when configured):** verification, forgot password, and notices when a
  password changes. Without mail, the operator issues reset links from the
  command line.
- **Operator panel:** disable, enable or delete users, send reset links, and
  manage invites.
- **Abuse protection:**
  - account isolation proven by tests on every route
  - internal-network blocking for non-operator workspaces
  - per-workspace limits on monitors, check interval and traffic to any one host
  - rate limits on sign-in, sign-up, email, Check now, Test URL, the API and
    status pages

Delivered in seven steps (M1a–M1e, M2, M3), each reviewed and releasable on its
own.

## Not planned yet

These are recorded so they aren't forgotten, not promised:

- teammates and roles inside a workspace
- OAuth sign-in (Google, GitHub)
- personal API tokens
- two-factor authentication
- changing an account's email address
- an audit-log table (security events go to the structured log)
- a common-password blocklist and CAPTCHA
- other check types: TCP ports, ping, DNS, SSL certificate expiry, custom
  request methods, headers or auth
- repeat alerts or escalation while something stays down
- generic outgoing webhooks and Prometheus metrics

## Related documents

| Document | Contents |
|---|---|
| [decisions.md](decisions.md) | every product and technical decision, with reasons and rejected alternatives |
| [superpowers/specs/2026-11-10-accounts-design.md](superpowers/specs/2026-11-10-accounts-design.md) | the accounts design |
| [../security_best_practices_report.md](../security_best_practices_report.md) | security review findings and their status |
| [../README.md](../README.md) | installation, configuration, upgrading and operations |
