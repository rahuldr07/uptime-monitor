# Decision log

Every product and technical decision, newest group last. Each entry records
what was decided, why, and what was rejected. To change a decision, add a new
entry that supersedes the old one instead of editing it.

M1a to M3 in the entries below are the delivery steps of the accounts design
in [design/accounts.md](design/accounts.md) (see D-119).

Status values: **Accepted** (in effect or approved for building),
**Superseded** (replaced by a later decision), **Open** (waiting for the
maintainer).

---

## Production hardening (2026-11-09)

Shipped in commits `ed48c6c`..`a2a3380`. Details in
[security-review.md](security-review.md).

### D-001: Keep daily uptime summaries separately from raw checks
- **Status:** Accepted
- **Decision:** Every non-maintenance check also updates a per-day summary
  (`daily` table) in the same transaction. Summaries are kept for 400 days;
  raw checks follow `retention` (default 7 days, minimum 24 hours).
- **Why:** The UI promised 30-day uptime and a 90-day status history, but raw
  checks were deleted after 7 days, so those numbers were wrong.
- **Rejected:** keeping raw checks for 90 days (the database grows a lot for
  frequent checks).

### D-002: Refuse to start without a password on a public address
- **Status:** Superseded by D-104 (accounts always require sign-in)
- **Decision:** An empty `admin_password` is allowed only on a loopback
  address, unless `insecure_no_auth` is set.
- **Why:** An unset `${UPTIME_ADMIN_PASSWORD}` silently exposed a dashboard
  that can edit monitors and probe arbitrary URLs.

### D-003: Email alerts are bounded and encrypted
- **Status:** Accepted
- **Decision:**
  - Port 465 uses implicit TLS; other ports use STARTTLS when offered.
  - Credentials are never sent over an unencrypted connection.
  - Each attempt times out after 30 seconds.
  - Messages carry `Date` and `Message-ID` headers, and line breaks are removed
    from header values.
- **Why:** `smtp.SendMail` had no timeout, so a stalled server could hang an
  alert forever.

### D-004: Deliver alert channels in parallel
- **Status:** Accepted
- **Decision:** Each channel is delivered in its own goroutine, with a
  per-attempt timeout and 3 attempts in total.
- **Why:** One slow or hung channel delayed every other channel.

### D-005: Strict browser security headers; HSTS left to the proxy
- **Status:** Accepted
- **Decision:**
  - CSP with `script-src 'self'` and no inline handlers (replaced with
    `data-action` attributes), plus `nosniff`.
  - HSTS is not sent by the app; it is configured at the reverse proxy.
- **Why:** The CSP blocks script injection. HSTS pins browsers to HTTPS for its
  whole lifetime, which the app cannot undo, so it belongs to whoever owns TLS.

### D-006: The public status page can be embedded; admin pages cannot
- **Status:** Accepted (moves to `/s/<slug>` with accounts, D-114)
- **Decision:** The anonymous status page allows framing; admin pages, and the
  status page as seen by an admin, deny it.
- **Why:** Embedding status pages is common. A framed page is always the
  anonymous view, because the session cookie (SameSite=Lax) isn't sent to
  cross-site frames.

### D-007: Cache the public status page
- **Status:** Accepted
- **Decision:**
  - Anonymous visitors get a copy cached for 10 seconds.
  - Only one rebuild runs at a time; others get the stale copy, and failures
    are shared by everyone waiting.
  - The lock is never held while writing a response.
  - Only admins bypass the cache.
- **Why:** Each visit ran several queries per monitor on the single SQLite
  connection that also records checks. The first version also let one slow
  reader stall every visitor.

### D-008: Saved alert secrets never go back to the browser
- **Status:** Accepted (extended by D-115)
- **Decision:** The Alerts page shows secrets as "Saved". A blank field keeps
  the saved value, and each channel has a "Remove these settings" checkbox.
- **Why:** Secrets appeared in the page's HTML.

### D-009: Sign flash messages
- **Status:** Accepted
- **Decision:** `?msg=` messages carry an HMAC signature; unsigned messages are
  ignored.
- **Why:** A crafted link could show attacker-written text to an admin as if
  the app said it.

### D-010: Rate-limit login by the address the proxy saw
- **Status:** Accepted
- **Decision:** Behind a proxy (`trust_proxy`), the client address is read from
  the right of `X-Forwarded-For`, skipping `trusted_proxies` entries (default
  1). Ports are stripped.
- **Why:** The leftmost entry is supplied by the client, so rotating it allowed
  unlimited password guessing.

### D-011: Docker layout
- **Status:** Accepted
- **Decision:**
  - Go 1.26 build image, non-root user, read-only root filesystem, health check.
  - Compose mounts the config read-only and stores data in the named volume
    `uptime-monitor-data`, and requires a password (no `change-me` default).
  - Without `-config`, the binary uses the first existing of `./config.json`,
    `/etc/uptime-monitor/config.json` and `/etc/uptime-monitor.json`.
- **Why:** The old Dockerfile couldn't build the module, and its data folder
  wasn't writable by the container user. Config discovery keeps the original
  `/data/config.json` layout working without changes.

### D-012: Safe backups
- **Status:** Accepted
- **Decision:**
  - `-backup` copies the database with `VACUUM INTO` while running.
  - The destination is created with mode 0600 before any data is written.
  - The source is opened read-only, and the backup fails if the source is
    missing.
- **Why:** A wrong relative path produced an empty "backup" that reported
  success.

### D-013: Split the web package by area
- **Status:** Accepted
- **Decision:** `web.go` (about 1,500 lines) became `targets.go`, `alerts.go`,
  `status.go`, `maintenance.go`, `incidents.go`, `api.go` and `helpers.go`.
- **Why:** Maintainability. The same approach applies to `store.go` and
  `auth.go` in accounts step M1a (D-120).

### D-014: Remaining security items
- **Status:** Open
- **SEC-3:** update the local Go toolchain to 1.27.3 or later. Binaries built on
  the maintainer's machine include standard-library vulnerabilities; Docker and
  CI builds are clean.
- **SEC-8:** optional `require_tls` for workspace email channels. For
  non-operator workspaces it is required by D-117; for operator channels it
  remains open. System mail requires TLS by default (D-109).
- **SEC-9:** session lifetime. Resolved by D-106 once accounts ship.

---

## Accounts (2026-11-10)

Approved through brainstorming with the maintainer, then revised after three
independent reviews (spec quality, security architecture, fit with the code).
Full design:
[design/accounts.md](design/accounts.md).

### D-101: Separate accounts with workspaces
- **Status:** Accepted
- **Decision:** Data belongs to a workspace. Sign-up creates a user plus their
  own private workspace. v1 has one member (the owner) per workspace;
  `memberships` with roles is in the schema so teammates can be added without
  migrating data again.
- **Rejected:**
  - a single shared team per install (doesn't fit a public instance)
  - shared data with open sign-up (strangers could edit everything and probe
    the network)
  - data owned directly by users (teams would need another full migration)

### D-102: Hosted and self-hosted, with configurable sign-up
- **Status:** Accepted
- **Decision:** `signup` is `closed` (default), `invite` or `open`. Operator
  invite links work in every mode. `open` requires system mail.
- **Why:** The maintainer runs a public instance and others self-host.

### D-103: Email features when mail is configured
- **Status:** Accepted
- **Decision:** With system mail, new accounts verify their email and "forgot
  password" works. Without it, `open` sign-up isn't allowed and the operator
  issues reset links from the command line.
- **Rejected:**
  - always requiring mail (self-hosters without mail couldn't use sign-up)
  - no email features (too weak against fake accounts)

### D-104: Protected setup page creates the first account
- **Status:** Accepted (supersedes D-002)
- **Decision:**
  - The first account is created on `/setup`, unlocked by a one-time link
    printed to the server log (or `-setup-link`), or by the old
    `admin_password` on an upgraded install.
  - The submit is authorised again and the token consumed in the same
    transaction that creates the account.
  - The account becomes the operator and adopts all existing data.
  - `insecure_no_auth` is removed.
- **Rejected:**
  - command line only (less friendly)
  - first visitor claims the instance (takeover risk on public servers)

### D-105: Isolation through a scoped store
- **Status:** Accepted
- **Decision:**
  - One SQLite database.
  - Handlers only receive a workspace-bound `store.WS`, whose every query
    filters by workspace unconditionally; unscoped methods are removed from the
    main store.
  - Tests fail the build if code outside an allow-list touches the unscoped
    store, or if a route isn't classified.
  - An isolation suite attacks every workspace route from another account.
- **Rejected:**
  - passing a workspace ID to every query, enforced by review only (easy to
    forget)
  - one database per workspace (heavy to operate, back up and migrate)

### D-106: Sessions stored in the database
- **Status:** Accepted (resolves SEC-9)
- **Decision:**
  - 256-bit random tokens; only their SHA-256 is stored.
  - 30-day absolute expiry, 7-day idle expiry, at most 20 sessions per user.
  - A new token on every sign-in.
  - Password change or reset revokes sessions; sign out everywhere is
    available.
  - The cookie is `__Host-` prefixed and `Secure` over HTTPS.
- **Why:** In-memory sessions were lost on restart and could not be listed or
  revoked per user.

### D-107: argon2id for passwords
- **Status:** Accepted
- **Decision:**
  - argon2id (19 MiB, 2 iterations, parallelism 1) from `golang.org/x/crypto`,
    the first new dependency.
  - At most 4 hashes run at once and 64 wait; waits are capped at 2 seconds.
  - Passwords are 10 characters to 256 bytes, with no composition rules (NIST
    guidance).
- **Rejected:** `crypto/pbkdf2` (no new dependency, but not memory-hard).

### D-108: No account enumeration
- **Status:** Accepted
- **Decision:**
  - Fixed messages and identical responses for existing and unknown emails.
  - A dummy hash runs for unknown emails, and sign-up always hashes.
  - System email is sent from a queue, so responses don't wait for SMTP.
  - "Disabled" and "unverified" are only revealed after a correct password.
  - A timing test enforces this.

### D-109: System mail
- **Status:** Accepted (M2)
- **Decision:**
  - A `mail` config, sent through the existing email sender with TLS required
    by default.
  - A bounded queue with per-recipient (3/hour, 10/day) and instance-wide
    budgets.
  - Emails contain no user-written text except the link.

### D-110: Tokens and emailed links
- **Status:** Accepted
- **Decision:**
  - Setup, invite, verify and reset tokens are stored hashed, single use, and
    checked by kind.
  - They are consumed atomically, inside the account-creation transaction where
    relevant.
  - Links are built only from `base_url`, never from the request's Host header.
  - Opening a link never changes anything; a button press (POST) does, so email
    scanners can't consume links or sign someone into an attacker's account.

### D-111: Login rate limits never lock a real user out
- **Status:** Accepted
- **Decision:**
  - Hard limits apply per address and per email+address.
  - Per email alone, sign-ins are only slowed (one try every 5 seconds after 20
    failures).
  - Browsers that signed in before (device cookie) skip the slowdown.
- **Rejected:** a hard per-email lockout (anyone could lock the operator out).

### D-112: Internal-address blocking with three levels
- **Status:** Accepted
- **Decision:**
  - `allow_private_targets` is `none`, `operator` (default) or `all`.
  - It is enforced at connection time on the resolved address, using an
    allow-list rule (global unicast, not special-purpose, not a tunnelling
    range).
  - Proxies are ignored while guarded, and errors don't reveal the address.
  - It covers monitors, Test URL, webhooks, Telegram and workspace SMTP.
- **Why:** Strangers must not reach internal networks or cloud metadata, but
  existing self-hosters monitor their LAN. Their data moves into the operator's
  workspace, so `operator` keeps it working.
- **Rejected:** a single on/off switch (either breaks upgrades or opens the
  network to every user).

### D-113: Rate limiting and abuse controls
- **Status:** Accepted
- **Decision:**
  - One sliding-window limiter (`internal/ratelimit`) with bounded memory, and
    IPv6 addresses grouped by /64.
  - Limits cover sign-in, setup, sign-up, hashing, email, Check now (also
    coalesced), Test URL, alert tests, monitor creation, monitors per workspace,
    minimum interval, combined traffic to one host, the API, live updates and
    status pages. The numbers are in spec §8.2.
  - Instance-wide caps are configurable under `limits`; the rest are documented
    constants.
  - The operator's workspace is exempt from per-workspace limits.
- **Known limitation:** limits are in memory, so they reset on restart and
  assume a single process.

### D-114: Status pages at `/s/<slug>`
- **Status:** Accepted
- **Decision:**
  - Each workspace has a public page at `/s/<slug>`; `/status` redirects to the
    operator's page.
  - Slugs are derived from the workspace name, can be changed, and have no
    reserved words (they can't clash with routes under `/s/`).
  - Anonymous pages are built without the visitor's session and cached per
    slug.

### D-115: Alert channels belong only to their workspace
- **Status:** Accepted (extends D-008)
- **Decision:**
  - A workspace's channels come only from its own settings.
  - The config file's `notify` is copied into the operator's workspace once and
    never used as a fallback.
  - A saved secret is kept only while its destination (SMTP host, port,
    username, Telegram chat, webhook host) is unchanged.
- **Why:** Reviews found that a new account would see, and could redirect, the
  operator's credentials.

### D-116: Per-workspace limits
- **Status:** Accepted
- **Decision:**
  - At most 20 monitors per workspace, a 30-second minimum interval, 30 monitor
    creations per hour, and at most 1 request per second per host across
    non-operator workspaces (checked when saving).
  - Probes identify the instance in their User-Agent.
- **Why:** Stops the instance being used to flood third-party sites.

### D-117: Restrictions on non-operator alert channels
- **Status:** Accepted
- **Decision:**
  - Webhooks don't follow redirects, and Telegram tokens are validated.
  - Workspace email channels must use port 465 or 587 with a login and TLS, and
    have at most 5 recipients.
- **Why:** Stops spam relaying and data leaks through alert channels.

### D-118: Upgrades
- **Status:** Accepted
- **Decision:**
  - Migrations are numbered with SQLite's `user_version`, one transaction per
    step.
  - Tables are rebuilt following SQLite's documented procedure, with a foreign
    key check.
  - An automatic owner-only backup is written before the accounts migration
    when data exists.
  - Existing data moves to the "Default" workspace (`/s/default`).

### D-119: Delivery in seven reviewed steps
- **Status:** Accepted
- **Decision:**
  - M1a building blocks
  - M1b internal-address guard
  - M1c workspaces and migration
  - M1d accounts and sessions
  - M1e multi-tenant features
  - M2 email
  - M3 operator panel

  Each step gets its own plan, independent review and commit, and is
  releasable.
- **Why:** M1 as one unit was about 5,000 lines, too large to review safely.

### D-120: Code structure and shared patterns
- **Status:** Accepted
- **Decision:**
  - Split `store.go` and `auth.go` into area files, and add the small packages
    `password`, `netguard`, `ratelimit` and `mail`.
  - One secret/token helper and one limiter type.
  - One account-creation transaction shared by setup and sign-up.
  - Transaction helpers take a `querier`, because there is a single SQLite
    connection.
  - One generic `loadByID` for 404 handling.
  - Routes are declared as a table whose class decides the middleware.
  - A `web.Options` struct; pointer config fields where zero is meaningful.
  - New pages reuse the existing templates and CSS.
- **Why:** The maintainer asked for shared files and patterns instead of
  duplication; the code review mapped every existing function to reuse.

### D-121: Left out of v1
- **Status:** Accepted
- **Decision:** Not in v1:
  - teammates and roles
  - OAuth
  - API tokens
  - two-factor authentication
  - changing email
  - promoting operators in the UI
  - an audit-log table
  - a common-password blocklist and CAPTCHA
  - `security.txt`

  The design leaves room for each.

### D-122: M1b ships the guard only
- **Status:** Accepted (amends the M1b row of the spec's delivery table (§2))
- **Decision:** The non-redirecting webhook client and Telegram token validation move from M1b to M1e. `netguard.Transport` replaces the spec's `netguard.Wrap`, and the monitor takes `AllowPrivate bool` until M1c makes it per workspace. The static test matches text, so it does not catch import aliases of net/http or net.
- **Why:** Both restrictions apply only to non-operator workspaces (§7.2), which first exist in M1e. Built earlier, they would be unused code. Every call site builds a new transport, so a constructor lets the static test forbid transport literals everywhere else.

### D-123: Workspace 1 always exists; M1c scope
- **Status:** Accepted (amends §4.4 and the M1c row of the spec's delivery table)
- **Decision:**
  - Schema v2 creates workspace 1 ("Default", `/s/default`) on every database, fresh or upgraded. In M1d, setup adopts it instead of creating a workspace. The pre-migration backup is still written only when data exists.
  - Until M1d adds users, workspace 1 is the operator's workspace.
  - Slug derivation and editing, `CountTargets`, workspace deletion and Check-now coalescing move to M1e, where they are first used. The static allow-list test for the unscoped store moves to M1d.
  - Because workspace 1 always exists, it no longer marks an upgraded install. `/status` always redirects to `/s/default`, which supersedes §5.1's "404 on fresh installs" (superseded by M1e: `/status` redirects to the signed-in user's status page, or for anonymous visitors to the operator workspace's current slug). M1d's setup mode is "no users exist", and the admin_password unlock must decide "upgraded install" another way, for example by whether workspace 1 holds any targets, settings or maintenance from before setup.
- **Why:** Before M1d there is no setup page, so a fresh install would otherwise have no workspace and nothing would work. With one workspace always present, every step stays releasable and the setup flow stays simpler.

### D-124: M1d scope and security refinements
- **Status:** Accepted (amends §3.1, §8.2, §12 and the M1d row of the spec's delivery table)
- **Decision:**
  - Setup adopts workspace 1. An "upgraded install" means no users exist and workspace 1 has data. Config is not seeded in setup mode. `CompleteSetup` needs an explicit `PasswordUnlocked` and re-checks the upgraded condition inside the transaction.
  - There is no `BEGIN IMMEDIATE`, because the single connection already serialises.
  - `password.Hasher` is an injected type. `VerifyDummy` returns an error (this departs from the §3.1 signature) so that a busy hasher gives the same 503 for known and unknown emails. `Verify` caps stored parameters: m ≤ 4×default, t ≤ 10, p ≤ 4, salt 16 bytes, key 32 bytes. `New` panics above those caps.
  - Sign-in limits reserve a slot with `ratelimit.Take` and give it back with `Undo` on success, busy or error, so a burst cannot exceed the hard limits. Passwords over 256 bytes fail the same way as any wrong password.
  - On HTTPS only the `__Host-` cookies are read, which prevents cookie tossing. The device cookie follows the same rule.
  - A per-user limit on current-password checks: 5 per 15 minutes. This is not in §8.2.
  - `/reset` is a guest route for now. Handling sign-in as a different user moves to M2, together with `/forgot`. An unverified sign-in gives a generic 401 until M2. The operator route class comes in M3. Account deletion comes in M1e.
  - Under §12, the reset link printed by `-reset-password` is a second deliberate exception to "secrets are never printed", next to the setup link.
  - Startup logs a WARN when `trust_proxy` is set without an https `base_url`: a plain-HTTP origin still accepts the `uptime_session` cookie, which allows login CSRF there.
  - `-reset-password` refuses disabled users.
  - If stored hashes ever use params above the `Verify` caps, raise the caps together with `DefaultParams`.
  - The per-email slow gate can be occupied by many attacker addresses after a restart, because the device key is per process. Accepted for M1d: there is one operator, sessions survive restarts, and `-reset-password` bypasses the gate. Persisting the device key is deferred.
  - The `password_check_throttled` event (the per-user current-password limit) is logged but is not in §12.
  - `TestLoginNoEnumeration` keeps a relative timing bound (the gap is under a quarter of the larger median, and unknown emails are not under half the known time) instead of the spec's absolute 50 ms. A hash takes about 44 ms with the real hasher, so an absolute 50 ms bound would still pass if the dummy hash were missing.
  - Session revocation on a password change or reset is covered by the `password_changed` and `password_reset` events; no separate `sessions_revoked` event is logged for those.
- **Why:** These rulings settle points the spec left open or got wrong while M1d was built. Each keeps the sign-in path from leaking which emails exist, from exceeding its limits under a burst, or from taking over an existing install.

### D-125: M1e scope and rulings
- **Status:** Accepted (amends §6.7, §8.2, §9.3, §12 and the M1e row of the spec's delivery table)
- **Decision:**
  - `signup: open` is rejected until M2, which adds email. `signup.go` tests `Signup != "invite"`; change it when open mode lands.
  - The operator workspace is `store.OperatorWorkspaceID` = 1. Operator exemptions (limits, rate limits, private targets under `operator`) follow the operator account in the web layer (`a.IsOperator`) and workspace 1 in the monitor and notifiers (`main.go`). They are equivalent in v1, which has one operator; unifying them is deferred.
  - The status page cache is keyed by workspace ID, so changing a slug does not strand cached pages.
  - The secret-destination rule applies to every workspace: a saved secret is dropped when its destination changes.
  - The invite management UI arrives in M3. M1e has the `-invite` flag only.
  - The instance-wide sign-up budget is charged only after a valid invite, so invalid forms cannot exhaust it. With sign-up closed and no invite, `/signup` returns 404 before any validation. The invite is looked up once, after the address limit.
  - Slug derivation tries `-2` to `-20`, then up to 5 random 4-character suffixes, then fails.
  - `TargetsInOtherWorkspaces` leaves out operator-owned workspaces, returns only ID, workspace ID, URL and interval, and feeds the per-host rate check.
  - The workspace cap is counted inside the account-creation transaction and refused with `ErrWorkspaceCap`. The monitor cap is checked inside the create transaction.
  - An edit, pause/resume or mute/unmute restarts the runner but does not probe straight away when the last check is younger than its interval, unless the edit changed the URL, timeout, expected status or keyword (`probeChanged`); then it probes at once.
  - Account deletion:
    - A precheck refuses the last operator before any session is revoked or runner stopped. `DeleteAccount` repeats the check and stays the authority.
    - Workspace 1 is never deleted, even if its only member is.
    - After the commit the workspace is tombstoned (`Monitor.ForgetWorkspace`) and its runners removed again, so a `Reload` in flight cannot leave an orphan runner.
    - `Reload` cancels a runner it replaces.
    - If the delete fails, the runners are reloaded.
  - `limits.signups_per_hour` below 1 is a startup error, because the limiter would treat 0 as 1. `signup: "closed"` does not switch sign-ups off: outstanding invites keep working until they expire (§5.2). The off switch is `limits.max_workspaces` set to the current workspace count, or revoking the outstanding invites (the `-invite` flag in M1e; the operator invites page in M3). For the same reason the `allow_private_targets: "all"` WARN fires whenever the value is `all`.
  - `hostRateErr` is check-then-act across workspaces: two creates racing can both pass. Accepted; the create limits bound the overshoot.
  - The runner waits at least 10 seconds after the last check before probing for an edit that changed the probe config (`minReprobeGap`), and `interval - age` after a restart when the last check is fresh, so scripted edits cannot flood outbound probes and a restart does not stretch an interval to nearly twice its length.
  - After an edit or restart, the first probe waits at least 10 seconds after the previous probe started, so operator monitors with intervals under 10 seconds can briefly be checked less often than their interval.
  - The last-operator check counts only active (not disabled) operators. The operator never sees the delete form, but the server refuses only the last operator, which matters when M3 adds more.
  - The per-host rate comparison strips a trailing dot from the host.
  - The alert-test limit is taken before the form is saved; when throttled, nothing is saved. Limits answer with an HTML 429 and `Retry-After`.
  - `-invite` rejects extra arguments and flags after the email. It prints the invite link on purpose, a third exception to "secrets are never logged" next to the setup and reset links.
  - `signup_throttled` is logged but is not in §12.
  - Check now sets a per-target busy flag. A check that is running or queued is not started again; the request answers "A check is already running." instead of probing twice.
  - The `limits.host_requests_per_second` value must be positive. `limits.min_interval` of `0s` means the default.
  - Alerts make 3 attempts in total (D-004 said "three retries").
  - The isolation suite seeds workspaces deterministically and uses longer wall-clock deadlines, because short ones flaked under `-race`.
- **Why:** These rulings settle points the spec left open or got wrong while M1e was built. Each keeps one workspace from reaching, starving or outliving another.

### D-126: M2 (email) rulings
- **Status:** Accepted (amends §5.7, §5.8 and the route classes of §6.2)
- **Decision:**
  - A password reset signs the user in only when the browser has no session or has a session for the same user. If the browser holds a session for a different user, that session and its cookie are left alone and the page says "Password changed. Sign in to continue." This departs from §5.8 (which always signs in) and mirrors §5.7.
  - `/verify` (GET and POST) and `/reset` (GET and POST) are `classPublic`: a signed-in user who opens an emailed link sees the page instead of being redirected to `/`. `/forgot` is `classGuest`.
  - `/forgot` answers 200 with the same body for every address (unknown, unverified, disabled or verified), and 404 when no system mail is configured. Unverified accounts get a reset link; completing the reset verifies the address.
  - A reset and a password change on `/account` each send a "Your password was changed" notice through `Mailer.Notice`. The notice is charged only to the instance budget, not to the per-recipient budgets, so `/forgot` spam cannot suppress it.
  - Accepted risk: `/forgot` and `/verify/resend` for an existing user write a token (through the serialised SQLite writer), so under heavy write load their timing can differ from unknown addresses. The responses are identical.
  - Mail budgets are charged before a token is created: `Mailer.Send` takes a compose callback and runs it only when the queue has room and the budgets allow the message. A dropped message therefore never replaces the link the user already has.
  - Without `mail`, `/forgot` and `/verify/resend` return 404 and the sign-in page shows no resend form. An unverified user then gets 403 `msgUnverified`, and the operator recovers accounts with `-reset-password`.
  - Status pages of unverified owners return 404.
  - The system mail dialer allows private addresses, because the operator chose the SMTP host. Alert-channel email in other workspaces keeps the `netguard` rules.
  - Open sign-up at the `max_workspaces` cap answers 403 for every address, existing or not, so the cap cannot be used to probe for accounts.
  - Pruning of unverified accounts (48 hours) skips operators and disabled accounts. D-127 keeps disabled accounts; an earlier version of this note said they were pruned.
  - `TestEnumerationTiming` holds the difference between the medians for existing and unknown addresses on `/forgot`, `/verify/resend` and open `/signup` under 50 ms, with the real hasher.
  - Accepted risk: open sign-up plus sign-in can reveal whether an address has an account. A new address creates an unverified account with the attacker's password, and signing in with it answers 403 `msgUnverified`; an existing address answers 401. Sign-up limits bound this and each probe sends mail. Closing it would contradict §5.4.
  - Accepted risk: auto sign-in after `POST /verify` (§5.7) lets an attacker pre-register an address and sign a session-less victim into the attacker's account. This is spec behaviour.
  - Accepted risk: `/forgot` posts can spend a victim's 10/day recipient mail budget. The operator recovers with `-reset-password`.

### D-127: M3 operator panel rulings
- **Status:** Accepted (implements spec §6.2, §6.3, §6.5, §6.7, §10, §11 and §12)
- **Decision:**
  - The panel answers 404 to everyone else, including anonymous visitors, wrong methods, unknown sub-paths and cross-site POSTs. A signed-in operator's cross-site POST still gets 403 from `sameOrigin`.
  - Suspension is enforced through store joins (suspended workspaces are hidden from the engine and from public pages) plus the monitor tombstone (`ForgetWorkspace`), so an in-flight Reload cannot restart a runner.
  - Panel delete is disable, then forget, then delete. A failure leaves the user disabled. `/account` self-delete is unchanged and relies on the tombstone.
  - Disable and delete wait for in-flight alerts (`ForgetWorkspace`), so they can take about two minutes. The panel holds one server-wide mutex across disable, enable and delete, so those requests queue behind each other. Disable and delete run detached from the client connection, so a dropped connection cannot stop them half way.
  - Invites are identified by their stored hash, so revoke acts on the hash, not on the link text.
  - The reset link is shown once on the response, with `Cache-Control: no-store` and no live refresh, when there is no mail. It is never put in a flash, a URL or a log. With mail it is emailed.
  - Resend uses `base_url` for the verification link.
  - The host search matches anywhere in the URL, not only the host.
  - An incident that is open when a workspace is suspended stays open. Its "up" alert after re-enabling includes the suspended time.
  - Disabled unverified users are not pruned. This amends the D-126 note and the README line on pruning, which said they were.
  - Re-enabling an unverified account older than 48 hours means the next hourly prune deletes it. The operator should send a reset link instead, which verifies the address.
  - Operators cannot be disabled, deleted or sent a reset link from the panel. Disabled users cannot be sent a reset link either. Re-enabling an operator with `-enable-user` is allowed, as the recovery path.
  - `-enable-user` changes only the database. A running server does not resume the user's monitors until it restarts; the panel's enable restarts them directly.
  - Extra log events not in spec §12: `account_pruned`, `email_verified`, `mail_throttled`, `mail_failed`.

### D-128: UI component upgrade (2026-11-12)
- **Status:** Accepted
- **Decision:**
  - Direction chosen by the maintainer: deepen the opencode theme (monospace type, flat panels, 1px borders, small radii, peach accent in dark and blue in light). All colours come from the existing tokens. No fonts or external assets are added; the CSP stays self-only.
  - Form validation is client-side and per field. It runs on submit, and on blur once the field has been touched. The message sits under the field, the field gets `aria-invalid`, and the first invalid field is focused. It is an enhancement only: JS adds `novalidate`, and server validation stays authoritative. Server errors render as an alert at the top of the form (inside the card on sign-in pages).
  - No `minlength` on the current or admin password fields, because upgraded installs may have short legacy passwords.
  - Toasts show an icon and a close button, and a 5-second countdown bar drives dismissal. The countdown pauses on hover and focus. Errors stay until dismissed. At most 4 toasts show at once, and duplicates are merged. Polite and assertive live regions are created at load.
  - Dropdowns: a custom accessible listbox wraps every native select. The native select stays the submitted control and still receives `change` events. The menu is rendered at body level, so tables and panels cannot clip it.
  - Modals: the confirm dialog has a title and icon, focuses Cancel, and closes on backdrop click. The slide-over panel is a modal dialog with a focus trap and focus return.
  - Data tables are vanilla-JS progressive enhancement over server-rendered tables. Features: sort, search, quick views, paging, a View menu, persisted preferences (localStorage key `uptime.tables`), row selection with TSV copy, keyboard rows, column resize and phone cards. Admin users keeps server paging and server search.
  - Live refresh: table state, keyboard focus and the row cursor survive SSE refreshes. Focus is restored only to the same element, matched by identity. Refresh is held while a menu is open, while a field is invalid, or while a pointer is pressed.
  - Global keyboard shortcuts (`/`, `n`, `t`) work everywhere except text inputs. A focused table region claims only arrows, Home, End, Space, Enter and Escape.
  - Out of scope: the plain-text 404 body. It must stay identical so the `/admin` gate is undetectable (D-127).

### D-129: Sign in with Google and GitHub (2026-11-12)
- **Status:** Accepted (implements [design/oauth.md](design/oauth.md))
- **Decision:**
  - Maintainer choices:
    - No auto-linking by email. An address that belongs to an unlinked account gets a 409 telling the person to sign in with their password (or Forgot password), then connect the provider from Account.
    - Providers are Google and GitHub.
    - Passwords are optional, so social-only accounts exist.
    - Standard library only, no new dependencies. Google identity comes from the OpenID Connect userinfo endpoint over TLS with the access token, not from ID token validation, so there is no JWT or JWKS code. GitHub identity comes from `/user` and `/user/emails`.
  - Identities are keyed by `(provider, subject)`, never by email. Only a verified email is accepted. Scopes are `openid email` for Google and `user:email` for GitHub; both send `prompt=select_account`.
  - An empty `users.password_hash` means no password, so the column stays `NOT NULL` and the table is not rebuilt. Sign-in with an empty hash runs `VerifyDummy` and answers the generic bad-login 401. Password checks never call `Verify` on an empty hash.
  - `sessions.auth_at` records the last real authentication. NULL (sessions from before v3) counts as not recent. A social-only account needs `auth_at` within 10 minutes for deleting the account, setting a password and disconnecting a provider. Accounts with a password keep being asked for it.
  - Connecting a provider needs a recent sign-in for every account (checked at start and at the callback). Otherwise a stale or stolen session could link the attacker's provider and then satisfy reauth with it, a backdoor surviving password change and sign-out-everywhere. A password account confirms its password through `/account/identities/{provider}/connect`, which refreshes `auth_at`. Connecting or disconnecting sends a notice email when mail is configured.
  - Reauth limit: there is no way to force a fresh login at either provider and the ID token's `auth_time` is not read, so reauth proves only that this browser holds a live session with the linked provider account. It is weaker than a password and is accepted for social-only accounts.
  - Accepted cost: the 409 "account exists" message tells a visitor that an address has an account. It takes a full OAuth round trip per probe and is rate-limited. Rejected: auto-linking by verified email, which makes control of an address at a provider enough to take over an account.
  - The pre-migration backup is generalised: an upgrade of a database that holds data writes `<db>.pre-v<N>-<time>.bak` (N is the target version) first, and an existing backup for the same N is reused. `pre-accounts` stays for the v2 step.
  - `/auth/{provider}/start` and `/callback` are in the public route class with checks in the handler, because the guest class redirects signed-in users and would break link and reauth. Responses send `Cache-Control: no-store`. `safeNext` rejects `/auth`.
  - Implementation choices:
    - In `closed` mode without an invite, and in `invite` mode without a valid one, sign-up answers 403 "Sign-up on this instance is by invitation."
    - New accounts get the workspace "My workspace", so the email's local part never appears in a public `/s/<slug>` address.
    - The state cookie lasts 10 minutes and is single-use. Starts and callbacks share a limit of 20 per minute per client address. OAuth sign-in skips the per-email login throttle.
    - Social-only accounts have no password until they set one or use Forgot password.
    - Account "Sign-in methods" (connect, disconnect, set password) and the operator panel provider badges follow design §6 and §7. Disconnecting the last sign-in method is refused inside the store transaction (`ErrLastSignInMethod`).

### D-130: 404 page, skeleton loading states and motion (2026-11-17)
- **Status:** Accepted
- **Decision:**
  - Skeletons appear only on navigations slower than 250 ms and on pending actions (Test URL). Pages are server-rendered, so there is nothing to show until the response arrives, and a skeleton on every click would flash on fast loads. The real `#content` nodes are kept and put back on a back/forward cache restore (`pageshow` with `persisted`) or after 10 s without an unload (Stop, a download). Live refresh never shows a skeleton and holds off while one is up. Check now has no skeleton: it has no result area (the outcome is a toast followed by a refresh) and its button already shows a spinner while the request is pending.
  - Skeleton markup lives in `<template>` elements in `layout.html` (the `skeletons` partial), so JavaScript only clones them. Public pages have no sidebar and carry none.
  - No staggered entrances and no count-up numbers. Motion is limited to a 160 ms page fade, an 800 ms highlight on a row whose `data-k` state changed between refreshes, 200 ms colour easing and button press feedback, and every rule sits under `prefers-reduced-motion: no-preference`. Live refresh replaces elements, so colour easing shows on hover and theme changes, not across a refresh; the row highlight covers that case.
  - The 404 page rules: a GET that wants HTML gets the page with status 404 and `Cache-Control: no-store` (in the layout when signed in, a card when not). Only signed-in visitors see the address echoed. The page always sends `frame-ancestors 'none'` and `X-Frame-Options: DENY`, because the `/s/` prefix is otherwise frameable. Any 404 under `/s/` renders the public card "Status page not found", never the signed-in layout. Partial (`X-Partial: 1`), `/api/`, JSON callers and non-GET requests get the bare plain-text 404, with no extra headers, so it is byte-identical to the mux's own 404. This supersedes the D-128 note that the plain-text 404 body is out of scope. Every existing 404 stays where it was, so cross-workspace access is still 404 (D-127 and the isolation tests). The `/admin` gate and `requireOperator` call the same `notFound`, so an operator-only path answers exactly what an unknown path answers for the same visitor, headers and body included (`TestAdminGateMatchesUnknownPath`).
  - Routing: `GET /` is the catch-all in the public class (`GET /{$}` is the overview). A `GET /` pattern would make any other method on an unknown path a 405, so non-GET and non-HEAD requests go to a mux built without it: unknown paths stay a plain 404 and known paths keep their 405. A GET on a POST-only path now gets the 404 page instead of 405.
  - Skeleton details: the placeholders are sized from measurements of the real pages (first row and tile positions within about 1 px at 1280 and 375). `/targets/{id}` with a tab other than overview gets the generic shape. A hidden `role="status"` "Loading…" announces the wait, `#content` carries no ARIA state, scroll is saved and restored, and the page that follows a skeleton skips its enter fade. Test URL keeps the visible text "Testing…", disables its button while pending and re-enables it on every path.

### D-131: Fallback alert email through system mail (2026-11-17)
- **Status:** Accepted
- **Decision:**
  - A workspace with no email channel (no `smtp_host`) gets alert emails from the server's `mail` account. A configured but unticked channel means the user turned email off, so no fallback. Stored as `server_email_off` in the `notify` JSON, absent meaning on, and carried over by any form that does not show the box.
  - Recipients are the verified, enabled sign-in addresses of the workspace's members, looked up at send time. The channel's To list is never used. Otherwise any signed-in user could make the operator's Brevo account send mail to arbitrary strangers (a relay), and the operator's sender reputation would carry the abuse. Verified addresses belong to people who proved control of them and chose to sign up.
  - On by default, because the point is that a new workspace gets alerts without setting up SMTP, and the addressee is the person who signed up. A per-workspace switch turns it off.
  - Budgets, in memory like the other mail budgets: 10 per hour and 30 per day per workspace, charged once per alert before the three delivery attempts, and counted against the instance budget. New `limits.mail_per_day` (default 0, unlimited) caps all system email per day. The provider's free plan is the reason: Brevo's free plan allows 300 emails a day, shared by verification, reset and alert mail, so one noisy workspace must not be able to use the day up, and the operator can set `mail_per_day` below the provider's cap.
  - A dropped alert is recorded as a failed delivery with the text "Limit reached", which the existing last-status badge shows, plus a WARN `event=alert_mail_dropped` with the workspace and reason.
  - Delivery is synchronous through the system mailer's sender, not the queue, so the monitor's retries and the recorded outcome are real. It gets the same TLS and address checks as other system mail.
  - SMTP login failures (530, 534, 535, 538) show "Login rejected — check the username and password" for every email path; the log keeps the raw error. The SMTP password is no longer trimmed.
  - Judgement call: "members" means every member with a verified account, not only owners; today accounts own their workspace, so this is the owner.

