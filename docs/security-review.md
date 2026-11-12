# Security review: uptime-monitor

> Historical snapshot from 2026-11-09, taken before the accounts work. The
> findings below are that snapshot; only the status column is kept current.
> Current behaviour is described in the
> [README](../README.md) and [decisions.md](decisions.md).

Date: 2026-11-09. Scope: the whole repository including the uncommitted
production-hardening changes (Go backend, `html/template` UI, vanilla JS,
Docker/compose/systemd/CI). Guidance used: Go general backend security spec
and general web frontend security spec.

## Executive summary

The application's security baseline is good. It has:

- explicit HTTP timeouts and header and body limits
- `html/template` everywhere, plus a strict `script-src 'self'` CSP with no inline script
- parameterized SQL
- `crypto/rand` session tokens
- HttpOnly + SameSite=Lax cookies
- Fetch-Metadata/Origin CSRF checks on every POST
- a database file with mode 0600
- no secrets tracked in git
- a non-root, read-only container
- `govulncheck` in CI

One **High** issue was confirmed by testing. When `trust_proxy` is enabled, which
is the recommended production setup, the login rate limiter can be bypassed by
rotating the leftmost `X-Forwarded-For` value, allowing unlimited password
guessing. Two **Medium** issues follow:

- an anonymous bypass of the status-page cache
- a vulnerable local Go toolchain; Docker and CI builds are not affected

The rest is Low or informational.

| ID | Severity | Title | Status |
|---|---|---|---|
| SEC-1 | High | Login rate limit bypass via spoofed `X-Forwarded-For` | **Fixed**: rightmost entry, `trusted_proxies` option, regression test |
| SEC-2 | Medium | Anonymous `X-Partial` header bypasses the status-page cache | **Fixed**: only admins bypass; fixed set of cache keys; test |
| SEC-3 | Medium | Local Go toolchain 1.27.1 has 13 reachable stdlib vulnerabilities | **Open, operator action**: upgrade local Go to ≥ 1.27.3 (Docker and CI builds are clean) |
| SEC-4 | Low | Status-page cache lock is held during the rebuild, so waiting requests pile up | **Fixed**: one rebuild at a time, stale copy served meanwhile, waiters respect cancellation; test |
| SEC-5 | Low | HSTS is sent automatically and cannot be turned off | **Fixed**: removed; left to the reverse proxy |
| SEC-6 | Low | Admin flash text can be injected via `?msg=` | **Fixed**: HMAC-signed flash messages; test |
| SEC-7 | Low | Password comparison leaks the password length | **Fixed**: constant-time compare of SHA-256 digests |
| SEC-8 | Low | Unauthenticated SMTP falls back to plaintext without STARTTLS | **Fixed**: the `mail` config has `require_tls` (default `true`), and alert email channels in non-operator workspaces always require TLS. Alert email channels in the operator workspace still fall back to plaintext when STARTTLS is not offered |
| SEC-9 | Low | Long-lived sessions with no idle timeout or session cap | **Fixed**: sessions expire after 30 days or 7 days idle, and a user has at most 20 |
| SEC-10 | Info | Accepted or by-design risks | Accepted |
| SEC-11 | Medium | One slow-reading client blocks the public status page for everyone (found by code review) | **Fixed**: lock never held while writing; deterministic regression test confirmed to fail on the old code |
| SEC-12 | Low | Backup file is briefly readable by others, and the existence check is racy (found by code review) | **Fixed**: `O_EXCL` 0600 creation, read-only source, missing source fails |
| SEC-13 | Low | Compose publishes the port on all interfaces while the docs suggest `trust_proxy` (found by code review) | **Fixed**: documented in compose and README |

---

## High

### SEC-1: Login rate limit bypass via spoofed `X-Forwarded-For`

- **Rule:** GO-HTTP-003 (forwarded header trust)
- **Location:** `internal/web/auth.go:271-288` (`clientKey`)
- **Evidence:**
  ```go
  if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
      first := strings.TrimSpace(strings.Split(xff, ",")[0])
  ```
- **Impact:** behind a reverse proxy with `trust_proxy: true`, anyone can guess
  the admin password without limit. Proxies *append* the real client address to
  any `X-Forwarded-For` value the client sent, so the leftmost entry is
  attacker-controlled.
- **Reproduced:** 8 failed logins with `X-Forwarded-For: 10.0.0.$i, 9.9.9.9` all
  returned 401. The same client without spoofing got 429 from the 6th attempt.
- **Fix:** use the **rightmost** `X-Forwarded-For` entry, the one appended by the
  trusted proxy. Optionally add a configurable hop count for chained proxies.
  Only fall back to `X-Real-IP` when `X-Forwarded-For` is absent. As
  defense-in-depth, consider a modest global slowdown (not a lockout) after many
  failures across all clients.

## Medium

### SEC-2: Anonymous `X-Partial` header bypasses the status-page cache

- **Rule:** GO-HTTP-001/002 family (resource exhaustion on unauthenticated endpoints)
- **Location:** `internal/web/status.go:55`
- **Evidence:** `if s.isAdmin(r) || r.Header.Get("X-Partial") == "1" {` renders
  live, with uncached queries.
- **Impact:** an unauthenticated client can force a full rebuild on every
  request: several SQLite queries per monitor, on the single connection that
  also records checks. This defeats the cache meant to stop public traffic
  from crowding out monitoring.
- **Fix:** only admins bypass the cache. Anonymous partial requests are never
  legitimate, because the public page is not live-refreshed. Serve them from
  the cache, keyed by theme and partial, or treat them as full-page requests.

### SEC-3: Local Go toolchain has 13 reachable stdlib vulnerabilities

- **Rule:** GO-DEPLOY-001
- **Location:** build environment (`go version` → go1.27.1)
- **Evidence:** `govulncheck ./...` reports GO-2026-6599, -6600, -6603, -6605,
  -6607 to -6613, -6617 (fixed in 1.27.2) and GO-2026-6809 (fixed in 1.27.3).
  The affected areas are `html/template`, `net/http` (HTTP/1 and HTTP/2),
  `crypto/tls`, `crypto/x509` and `mime/multipart`. Example trace:
  `internal/web/helpers.go:34` → `template.Template.ExecuteTemplate`.
- **Impact:** binaries built on this machine, for example for the systemd
  deployment, ship these flaws. The Docker image (Go 1.26.10) and CI (latest
  1.26.x) scan clean.
- **Fix:** upgrade the local toolchain to Go ≥ 1.27.3. CI already runs
  `govulncheck`.

### SEC-11: One slow-reading client blocks the public status page

- **Location:** `internal/web/status.go:68-71`. `defer c.mu.Unlock()` keeps the
  cache lock held while `writeHTML` sends the response, on both the cache-hit
  and rebuild paths.
- **Impact:** an unauthenticated client that opens `/status` and reads very
  slowly holds the lock for up to `WriteTimeout` (30s), repeatably. Every other
  anonymous visitor waits behind it. The code reviewer reproduced this: with 400
  monitors and a 1 KB receive buffer, a normal `curl /status` timed out after
  8s while `/healthz` stayed responsive.
- **Fix:** hold the lock only to read or store the cached body; write the
  response after releasing it. Combine this with SEC-4 (serve stale while a
  single rebuild runs).

## Low

### SEC-12: Backup file briefly readable by others

- **Location:** `internal/store/store.go`, `Backup`: `os.Stat` check, then
  `VACUUM INTO`, then `os.Chmod(dest, 0o600)`.
- **Impact:** `VACUUM INTO` creates the copy with the process umask, which is
  often 0644 under cron as root. It holds the alert secrets and is
  world-readable until the `Chmod` runs. The existence check is also a
  time-of-check/time-of-use race.
- **Fix:** create `dest` with `O_CREATE|O_EXCL` and mode 0600 first. SQLite's
  `VACUUM INTO` accepts an existing empty file. Related correctness bug from the
  review: `-backup` against a missing database creates an empty one and reports
  success, so open the source read-only and fail if it is missing.

### SEC-13: Port published on all interfaces

- **Location:** `docker-compose.yml:9` (`"8080:8080"`)
- **Impact:** if an operator follows the README and sets `trust_proxy` while
  the container port is still reachable directly, clients can spoof
  `X-Forwarded-For` and `X-Forwarded-Proto`.
- **Fix:** document, and show in a compose comment,
  `127.0.0.1:8080:8080` for deployments behind a proxy.

### SEC-4: Status-page cache lock held during rebuild

- **Location:** `internal/web/status.go:68`, which holds `c.mu.Lock()` across
  `statusData` and `renderBytes`.
- **Impact:** if a rebuild is slow, for example while the database is busy,
  every anonymous request queues on a mutex that ignores request cancellation.
  Goroutines accumulate until the rebuild finishes.
- **Fix:** serve the stale copy while a single goroutine refreshes it, or bound
  the wait with the request context. A channel-based lock or
  `singleflight` with `DoChan` can do this.

### SEC-5: HSTS sent automatically

- **Location:** `internal/web/auth.go:190-192`
- **Impact:** HSTS has lasting effects: browsers refuse plain HTTP for that host
  for a year. Setting it automatically on any trusted-HTTPS request can lock
  users out if the deployment later moves or serves HTTP on that hostname.
  This is usually better owned by the reverse proxy.
- **Fix:** remove it, or make it opt-in (`"hsts": true`) and document it.

### SEC-6: Admin flash text can be injected via `?msg=`

- **Location:** `internal/web/web.go:201` (`b.Flash = r.URL.Query().Get("msg")`)
- **Impact:** a crafted link such as `/?msg=Session+expired,+re-enter+password+at+evil.example`
  shows attacker-chosen text as a trusted toast to a logged-in admin. The text is
  escaped, so this is content spoofing and phishing, not XSS.
- **Fix:** pass message keys (`?msg=saved`) mapped to fixed strings server-side,
  or carry the flash in a short-lived cookie set by the redirecting handler.

### SEC-7: Password comparison leaks length

- **Location:** `internal/web/auth.go:290-291`
- **Evidence:** `subtle.ConstantTimeCompare` returns immediately when the
  lengths differ.
- **Fix:** compare `sha256.Sum256` digests of both values in constant time.

### SEC-8: Opportunistic STARTTLS for unauthenticated SMTP

- **Location:** `internal/notify/notify.go:209-215`
- **Impact:** when no username is set, mail is sent in plaintext if the server
  doesn't advertise STARTTLS, or if an on-path attacker strips it. Credentials are
  already protected: `smtp.PlainAuth` refuses non-TLS connections.
- **Fix:** add an opt-in `require_tls` email option that fails when STARTTLS is
  unavailable.

### SEC-9: Long-lived sessions

- **Location:** `internal/web/auth.go:18` (`sessionTTL = 30 days`), plus the
  in-memory session map
- **Impact:** a stolen cookie stays valid for 30 days, with no idle expiry and no
  limit on concurrent sessions.
- **Fix:** add an idle timeout, for example 7 days sliding, and optionally cap
  the number of live sessions.

## Informational / accepted risks (SEC-10)

- **Plaintext channel secrets in SQLite.** This is documented. It is mitigated by
  0600 file and backup permissions, and secrets are never echoed to the browser.
- **Admins can make the server fetch any http(s) URL.** Monitors and "Test URL"
  can reach internal networks (SSRF by design). Only trusted admins should have
  the password.
- **CSP has `style-src 'unsafe-inline'`.** The templates need it for inline
  styles. Script execution is still restricted to `'self'`.
- **CSRF uses a custom Fetch-Metadata/Origin check.** It is equivalent in spirit
  to Go's `http.CrossOriginProtection`, and switching would replace custom code
  with the standard library version. Requests with neither header (curl) are
  allowed by design. Browsers still get SameSite=Lax cookies.
- **`app.js` `innerHTML` (line 156) and `location.href` (lines 74, 83, 118)** use
  only same-origin, server-rendered or server-built values. They are not
  attacker-controlled.

## Controls verified as present

- **HTTP server:** `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`,
  `IdleTimeout`, `MaxHeaderBytes`. Request bodies are capped at 1 MB with a 413
  response.
- **No dangerous sinks:** no pprof or expvar, no `os/exec`, no `math/rand`, no
  `InsecureSkipVerify`, no `text/template`, no `unsafe`.
- **SQL:** all queries are parameterized. `Sprintf` is used only with constant
  identifiers in migrations.
- **Templates:** `template.HTML` appears only for server-generated SVG made of
  numbers and times. Target URLs in `href` go through html/template URL
  sanitization, and only http(s) is accepted.
- **Redirects:** `safeNext` blocks open redirects. `backTo` requires a same-host
  Referer.
- **Outbound requests:** outbound clients have timeouts and bounded reads. The
  notifier strips the URL (Telegram token) from errors.
- **Secrets:** no secrets are tracked in git, and `.dockerignore` excludes
  config and database files from the build context.
- **Container:** non-root, read-only root filesystem, `cap_drop: ALL`,
  `no-new-privileges`.
- **systemd:** the unit is sandboxed.
- **CI:** runs `go test -race` and `govulncheck`.
