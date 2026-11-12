# Accounts: sign-up, sign-in and separate workspaces

Status: design approved in brainstorming on 2026-11-10, then revised after three
independent reviews (spec quality, security architecture, fit with the
existing code). Each milestone step gets its own implementation plan.

## 1. Goal and context

uptime-monitor is being released as an open-source project that will be run in
two ways:

- as a **public hosted instance** where strangers sign up
- **self-hosted** by others for themselves, their team or their customers

Today it has a single admin password from the config, sessions held in memory,
and one shared set of monitors, alert channels, maintenance windows and status
page.

This design adds:

- user accounts with sign-up and sign-in
- a private **workspace** per account, holding its own monitors, incidents,
  alert channels, maintenance windows and public status page
- an operator role for running the instance
- the protections that strangers signing up make necessary: account isolation,
  internal-network blocking, rate limits and abuse controls

### 1.1 Decisions

| Topic | Decision |
|---|---|
| Tenancy | Data belongs to a **workspace**. Sign-up creates a user plus their own workspace. v1 has exactly one member (the owner) per workspace; `memberships` with roles is in the schema so teammates can be added later without another data migration. |
| Deployment | Hosted-public and self-hosted. Sign-up mode is configurable: `closed` (default), `invite` or `open`. |
| Email | Verification and password reset work when the operator configures system mail. `open` sign-up requires mail. Without mail, the operator resets passwords from the command line. |
| First account | Created on a protected setup page, unlocked by a one-time link printed to the log, or by the old `admin_password` on an upgraded install. It becomes the operator and owns all existing data. |
| Isolation | One SQLite database plus a **scoped store**. Handlers only get a workspace-bound object. Tests fail the build if code outside an allow-list touches the unscoped store, or if a route is unclassified. |
| Internal addresses | `allow_private_targets`: `none`, `operator` (default) or `all`. The operator's workspace, which holds all migrated data, can still monitor the LAN; other workspaces cannot. |
| Password hashing | argon2id from `golang.org/x/crypto`, the only new dependency (maintained by the Go team). |

### 1.2 Success criteria

1. **No cross-account access.** No account can read, change or delete another
   workspace's data through any route, live update, cache or background job.
   An automated suite proves this for every registered route.
2. **No probing of internal networks.** With default settings, a non-operator
   workspace cannot make the server connect to loopback, private, link-local
   (including cloud metadata) or other special-purpose addresses, through
   monitors, "Test URL" or alert channels.
3. **Safe upgrades.** An existing install upgrades without data loss and keeps
   monitoring its LAN targets. A backup is written before migrating, and the
   first account adopts all existing data.
4. **No account enumeration.** Sign-in, sign-up, forgot-password and
   resend-verification reveal nothing about which emails are registered, by
   content, status code or timing.
5. **No abuse amplification.** The instance cannot be used to send unlimited
   email, flood a third-party host, or lock a user out of their account.
6. **Existing checks still pass.** gofmt, vet, race tests, govulncheck, the
   Docker build and the strict CSP keep passing at every step.

### 1.3 Out of scope for v1

- teammates and workspace invites, and editor/viewer roles (the schema reserves
  them)
- OAuth sign-in (Google, GitHub)
- personal API tokens (the JSON API keeps using the session cookie)
- two-factor authentication
- changing an account's email address
- promoting further operators from the UI
- an audit-log table (security events go to the structured log, §12)
- a common-password blocklist and CAPTCHA (rate limits and verification
  cover v1; both can be added behind the existing sign-up checks)
- `security.txt` (SECURITY.md covers reporting)

## 2. Milestones and steps

Each step is planned, implemented, independently reviewed and committed on its
own. Every step leaves the build green and the product releasable.

| Step | Name | Contents | Size (code + tests) |
|---|---|---|---|
| **M1a** | Building blocks | No behaviour change. Split `store.go` into files; `migrations.go` with `user_version` (version 1 = today's schema); the `querier` interface; `internal/ratelimit` extracted from `loginLimiter`; routes declared as a table; generic `loadByID`; `web.Options`. | ~400 |
| **M1b** | Internal-address guard | `internal/netguard`; `allow_private_targets`; Test URL through the monitor's client; guarded clients for notify (HTTP and SMTP); no-redirect webhook client; Telegram token validation; static test against stray clients and dialers. | ~600 |
| **M1c** | Workspaces and migration | Schema v2 with backup; workspace 1 adopts existing data; `store.WS`; workspace-aware monitor engine and live updates; alert settings per workspace with the config fallback removed; web pinned to workspace 1 through one helper; `/s/{slug}` status pages and the `/status` redirect; status-page cache limits. | ~1,500 |
| **M1d** | Accounts and sessions | `internal/password`; users, sessions and tokens; setup page; email sign-in with rate limits and device cookie; sign-out; account page; `/reset` page; `-setup-link` and `-reset-password`; removal of the old password auth; reworked test environment; isolation suite and static checks. | ~2,000 |
| **M1e** | Multi-tenant features | Invites (`-invite`), invite sign-up, `closed`/`invite` modes, slug derivation; per-workspace limits and abuse controls; channel restrictions for non-operator workspaces; account deletion; README and SECURITY.md. | ~1,400 |
| **M2** | Email | `mail` config and the mail queue; verification; resend; forgot password; password-change notices; `open` sign-up; pruning of unverified accounts. | ~1,200 |
| **M3** | Operator panel | `/admin`: users (disable, enable, delete, resend verification, reset link) with workspace suspension; invites (create, list, revoke). | ~1,300 |

Configuration values not yet supported by the running step are rejected at
startup with a clear message. For example, before M1e only `signup: "closed"`
is accepted, and before M2 `signup: "open"` and `mail` are rejected.

## 3. Code structure and conventions

The implementation follows the project's existing conventions: small files
split by area, tests next to the code using real SQLite in `t.TempDir()` and
`httptest`, `Options` structs for wiring, and `ErrNotFound` mapped to 404.

### 3.1 Packages and files

**`internal/store`**: split `store.go` along its existing sections. The first
commit of M1a is a pure move.

| File | Contents |
|---|---|
| `store.go` | `Open`, `Close`, `Ping`, `Backup`, `restrictPerms`, `scanner`, `requireRow`, `boolToInt`, `querier` |
| `migrations.go` | `var steps = []func(context.Context, *sql.Tx) error{v1, v2}` and `migrate(db, path)` |
| `targets.go`, `checks.go`, `incidents.go`, `alerts.go`, `maintenance.go`, `settings.go` | today's queries, moved |
| `workspace.go` | `Workspace` type, `WS` type, `(*Store).Workspace`, `WorkspaceBySlug`, `OperatorWorkspace`, `DeriveSlug`, `ValidSlug` |
| `users.go` | `User`, `CreateAccount`, `CompleteSetup`, `UserByEmail`, `SetPassword`, `DeleteUser`, `CountUsers` |
| `sessions.go` | `CreateSession`, `SessionAuth`, `DeleteSession`, `DeleteUserSessions`, `PruneSessions` |
| `tokens.go` | `newSecret()`, `CreateToken`, `PeekToken`, `ConsumeToken`, `PruneTokens` |

**New packages**

| Package | Responsibility | Size |
|---|---|---|
| `internal/password` | `Hash(ctx, pw)`, `Verify(ctx, phc, pw) (bool, error)`, `VerifyDummy(ctx)`; owns the hashing semaphore | ~150 |
| `internal/netguard` | `Blocked(netip.Addr) bool`, `Dialer(allow bool, timeout) *net.Dialer`, `Wrap(*http.Transport, allow bool) *http.Transport` | ~150 |
| `internal/ratelimit` | the one sliding-window limiter used by web, mail and monitor (§8.1) | ~150 |
| `internal/mail` (M2) | bounded queue, worker and recipient budgets in front of `notify.Email.Send` | ~200 |

**`internal/web`**: split `auth.go` (about 370 lines today):

| File | Contents |
|---|---|
| `middleware.go` | `sameOrigin`, `parseForm`, `maxBodyBytes`, `cspBase`, `securityHeaders` |
| `clientip.go` | `clientKey`, `parseAddr`, IPv6 /64 grouping |
| `session.go` | `authCtx`, `currentAuth`, `requireUser`, `requireOperator`, `setupGate`, cookie helpers, `isHTTPS` |
| `routes.go` | the route table and route classes |
| `setup.go`, `login.go`, `signup.go`, `account.go`, `reset.go` | M1d/M1e pages |
| `verify.go`, `forgot.go` | M2 pages |
| `admin.go` | M3 panel |
| `events.go` | the SSE handler, moved out of `api.go` |

**Templates**: new pages reuse `public.html` (setup, sign-in, sign-up, reset,
verify, forgot) and `layout.html` (account, admin), with the existing CSS
classes such as `.login .card`, `.field` and `.btns`. No new CSS framework, no
inline handlers (CSP), no JavaScript changes beyond what the pages need.
`layout.html`'s sidebar footer shows the account email (linking to `/account`)
and a sign-out button. Status-page links use the workspace's own `/s/<slug>`.

### 3.2 Patterns to standardise

1. **Secrets in one place.** Only `store/tokens.go` creates random secrets
   (`newSecret`) and hashes them. Sessions and all token kinds go through it.
   The web layer only ever handles raw values.
2. **One limiter type** (`internal/ratelimit`), parameterised by window, limit
   and key cap. No copies of limiter logic.
3. **Account creation in one transaction.** Setup and sign-up both call
   `createAccount(ctx, q querier, in AccountInput)`, which optionally consumes a
   token in the same transaction.
4. **Transactions and the single connection.** The store uses one SQLite
   connection (`SetMaxOpenConns(1)`), so calling `s.db` inside a transaction
   deadlocks. Helpers that run inside transactions take a `querier`
   (`ExecContext`, `QueryContext`, `QueryRowContext`), satisfied by `*sql.DB`
   and `*sql.Tx`. Password hashing happens **before** `BeginTx`, so the
   connection is never held during the hash.
5. **Errors.**
   - The store returns `ErrNotFound` for missing rows *and* for rows in another
     workspace.
   - Handlers load objects through one generic
     `loadByID[T](s, w, r, get func(ctx, int64) (T, error)) (T, bool)`, which
     maps `ErrNotFound` to 404 and anything else to `s.fail`. It replaces the
     three copies `loadTarget`, `loadIncident` and `loadMaintenance`.
   - Validation errors re-render the form with 422 through one helper,
     `s.renderStatus(w, r, code, page, data)`.
   - Authentication messages are constants in `login.go`, so the enumeration
     tests compare against a single source.
6. **Options structs.** `web.New(web.Options{...})` replaces the five
   positional parameters, mirroring `monitor.Options`.
7. **Routes as data, default deny.** `routes.go` declares
   `[]route{method, pattern, class, handler}`. The class decides the middleware
   (§6.2), and tests iterate the same slice. `http.ServeMux` cannot list its
   routes, so the table is the single source of truth.
8. **Config defaults.** Keys whose zero value is meaningful (`max_monitors: 0`
   means unlimited, `require_tls: false`) use pointer types resolved in
   `applyDefaults`, following the existing pattern in `config.go`.

### 3.3 Reuse map

| Existing code | Used for |
|---|---|
| `loginLimiter` (`web/auth.go`) | becomes `internal/ratelimit` |
| `clientKey`, `parseAddr` | every per-address limit |
| `sameOrigin` | every new POST, including admin actions |
| `safeNext` | `next` after sign-in; its deny list gains `/setup`, `/signup`, `/reset`, `/verify` |
| `passwordEqual` | the `admin_password` setup unlock only |
| `isHTTPS`, `setSession`/`clearSession` shape | the DB-backed session cookie writer |
| `render`, `renderBytes`, `withFlash`, `redirect`, `backTo`, `writeJSON*` | all new pages; signed flashes are allowed on public auth pages ("Password changed, please sign in") |
| `formatTS`, `ago` | account and admin pages |
| `pageCache` (`web/status.go`) | status pages, keyed by slug (§6.5) |
| `store.Backup` | the pre-migration backup |
| `notify.Email` | system mail: extract `Send(ctx, to, subject, body)`; `Notify` calls it; add `RequireTLS` and an injectable `Dial` |
| `config.ValidateNotify` email branch | extract `validateSMTP` for `mail` |
| `monitor.Probe` | Test URL through `(*Monitor).Test` |
| `main.seedTargets`, `main.loadNotifiers` | take a `*store.WS`; run at startup and from the `AfterSetup` hook |
| `pruneLoop` | also prunes sessions, tokens and (M2) unverified accounts |
| `cookieJar`, `env.get/post` (`web_test.go`) | flow tests and the isolation suite |

## 4. Data model and migrations

### 4.1 Schema versioning

Migrations use `PRAGMA user_version`. `migrate(db, path)` runs every step whose
number is above the stored version, each in its own transaction, ending with
`PRAGMA user_version = N`.

- **Step 1** is today's schema plus the existing column additions and the
  `daily` backfill. It is idempotent, so it runs safely on databases created
  before versioning existed (`user_version = 0`).
- **Step 2** is the accounts schema (§4.3).
- A fresh database runs every step in order; there is no separate
  "latest schema" copy to keep in sync.
- `store_test.go`'s rollup backfill test sets `PRAGMA user_version = 0` before
  reopening, so step 1 runs again.

### 4.2 New tables (step 2)

```sql
CREATE TABLE users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  email         TEXT    NOT NULL UNIQUE,      -- normalised, §5.2
  password_hash TEXT    NOT NULL,             -- argon2id PHC string
  is_operator   INTEGER NOT NULL DEFAULT 0,
  verified_at   INTEGER,                      -- unix ms; NULL = unverified
  disabled_at   INTEGER,                      -- unix ms; NULL = active
  created_at    INTEGER NOT NULL,
  last_seen_at  INTEGER
);
CREATE TABLE workspaces (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  name         TEXT    NOT NULL,
  slug         TEXT    NOT NULL UNIQUE,
  suspended_at INTEGER,                       -- used from M3
  created_at   INTEGER NOT NULL
);
CREATE TABLE memberships (
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  role         TEXT    NOT NULL CHECK (role IN ('owner','editor','viewer')),
  created_at   INTEGER NOT NULL,
  PRIMARY KEY (user_id, workspace_id)
);
CREATE TABLE sessions (
  token_hash   TEXT    PRIMARY KEY,           -- hex SHA-256 of the cookie value
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE TABLE tokens (
  token_hash TEXT    PRIMARY KEY,             -- hex SHA-256
  kind       TEXT    NOT NULL CHECK (kind IN ('setup','invite','verify','reset')),
  user_id    INTEGER REFERENCES users(id) ON DELETE CASCADE,  -- verify, reset
  email      TEXT,                            -- invite bound to one email (optional)
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER
);
CREATE INDEX idx_tokens_user ON tokens(user_id, kind);
```

### 4.3 Rebuilt tables (step 2)

`targets`, `maintenance` and `settings` are rebuilt following SQLite's
documented table-rebuild procedure:

- `PRAGMA foreign_keys = OFF` on the connection **before** the transaction
- in the transaction: create `<table>_new`, copy rows, drop the old table,
  rename, recreate indexes, carry the `sqlite_sequence` value across (so
  deleted IDs are never reused), then `PRAGMA foreign_key_check`, which must
  return no rows
- `PRAGMA foreign_keys = ON` after commit

New definitions:

- **`targets`**: adds `workspace_id INTEGER NOT NULL REFERENCES workspaces(id)
  ON DELETE CASCADE`. The unique constraint becomes `UNIQUE (workspace_id,
  name)`, whose left prefix also serves workspace lookups (no extra index).
- **`maintenance`**: adds `workspace_id INTEGER NOT NULL REFERENCES
  workspaces(id) ON DELETE CASCADE`, with **no default**, and index
  `idx_maintenance_ws(workspace_id, starts_at)`.
- **`settings`**: `(workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON
  DELETE CASCADE, key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY
  (workspace_id, key))`. There are no instance-wide settings rows.
- **`checks`, `incidents`, `alerts`, `daily`**: unchanged. Rewriting `checks`
  would be expensive; workspace queries reach these tables through
  `targets.workspace_id`, and workspace deletion removes them explicitly
  (§6.7).

### 4.4 Migrating an existing install

Step 2 classifies the database by its contents: it **has data** if any row
exists in `targets`, `settings` or `maintenance`.

- **Has data (upgraded install):**
  1. Before the step-2 transaction, write a backup to
     `<db>.pre-accounts-<YYYYMMDD-HHMMSS>.bak` (mode 0600) with `store.Backup`.
     If the backup fails, startup fails.
  2. In the transaction, create workspace 1 `{name: "Default", slug:
     "default"}` and copy every row of the three rebuilt tables into it.
- **No data (fresh install, or one that never stored anything):** no backup and
  no workspace. Setup creates the first workspace.

After step 2 no users exist, so the instance is in **setup mode** (§5.1).
Because workspace 1 exists only on upgraded installs, "upgraded install in
setup mode" is exactly "workspace 1 exists and no users exist"; no extra flag
is stored.

The command-line actions (§10) open the store too, so they run pending
migrations, including the backup, before acting.

## 5. Authentication

### 5.1 Setup (no users exist)

**Setup mode.** While no user exists:

- Allowed routes: `GET /healthz`, `/static/*`, `GET|POST /setup`, `GET
  /s/{slug}`, and `GET /status` (which redirects to `/s/default` on an
  upgraded install, so existing public status links keep working, and returns
  404 on a fresh install).
- Every other route redirects to `/setup`, or returns 503 for `/api/*` and
  `/events`.
- `setupGate` holds an `atomic.Bool` loaded at startup and cleared when setup
  completes, so there is no database query per request.

**Setup token.**

- On every start in setup mode the server creates a `setup` token (24h expiry),
  deletes older setup tokens, and logs at WARN:
  `setup required: open <base>/setup?token=<token>`.
- `<base>` is `base_url` when set, otherwise `http://<listen>`, with
  `localhost` substituted when the listen host is empty or unspecified (`:8080`,
  `0.0.0.0:8080`, `[::]:8080`).
- `-setup-link` (§10) does the same and prints the link. Only one setup token is
  valid at a time.
- The README states that the setup link appears in the server log.

**Unlocking and submitting.**

- `GET /setup?token=…` with a valid token shows the form, carrying the token in
  a hidden field. Responses on setup pages send `Cache-Control: no-store`.
- On an upgraded install with a non-empty `admin_password`, `GET /setup` without
  a token shows a password field instead. That path exists only on upgraded
  installs.
- Unlock attempts by either method share the setup limiter (§8.2). Every attempt
  is logged at WARN with the client address.
- The form asks for email, password (twice) and workspace name. On an upgraded
  install the workspace name is prefilled with "Default" and the slug is kept.
- **`POST /setup` is authorised on its own.** It must carry a valid setup token
  or the correct `admin_password` (on an upgraded install). Anything else gets
  404. The password is hashed first; then in **one `BEGIN IMMEDIATE`
  transaction** the handler:
  - re-checks that no user exists
  - consumes the setup token (§5.6), when the token path was used
  - creates the user with `is_operator = 1` and `verified_at = now`
  - creates a workspace (fresh install) or uses workspace 1 (upgraded)
  - creates the `owner` membership
- After commit the handler clears setup mode, calls the `AfterSetup` hook
  (§9.3, which seeds config targets and alert channels into the operator's
  workspace and starts their runners) and signs the user in (§5.4).
- After setup, `/setup` returns 404, and `admin_password` is ignored with a
  WARN at startup if it is still set.

### 5.2 Sign-up (M1e for invites, M2 for open)

**Modes** (config `signup`):

| Mode | `/signup` without an invite | Sign-in page | With a valid invite |
|---|---|---|---|
| `closed` | 404 | no sign-up mention | works |
| `invite` | 200: "Sign-up on this instance is by invitation. Ask the operator for a link." | "Have an invitation link? Open it to sign up." | works |
| `open` | the sign-up form (requires `mail`) | "Create an account" link | works |

Operator invites (`-invite` in M1e, the panel in M3) work in every mode.

**The form:** email, password, workspace name.

- **Email:** parsed with `net/mail.ParseAddress`. The input must equal the
  parsed `Address` (no display names). Domain literals (`a@[127.0.0.1]`) and
  domains without a dot are rejected. At most 254 bytes. Stored trimmed and
  lowercased; this is the normalised form used everywhere.
- **Password:** at least 10 characters and at most 256 bytes. No composition
  rules.
- **Workspace name:** 1–60 characters, no control or format characters
  (Unicode categories Cc and Cf).
- **The slug** is derived from the workspace name (§6.5). The owner can change
  it later in the status settings.

**Order of work** (§8.3): rate limits and field validation first, then the
password hash, then one transaction.

**Invite sign-up:**

- `GET /signup?invite=…` shows the form with the token in a hidden field. If the
  invite is bound to an email, that email is prefilled and enforced.
- `POST /signup` consumes the invite and creates the account (verified, since
  the operator vouched for it) in one transaction through `createAccount`, then
  signs the user in.
- A failed sign-up (validation error, duplicate email) does **not** consume the
  invite.
- A duplicate email gives "An account with this email already exists. Sign in
  instead." This reveals existence only to invite holders, and sign-up attempts
  are rate limited (§8.2).

**Open sign-up (M2):**

- Every request hashes the password, including duplicates, so timing does not
  depend on whether the email exists.
- New email: the user and workspace are created unverified and a verification
  email is queued (§5.7).
- Duplicate email: no account is created. A notice is queued for the existing
  owner ("someone tried to sign up with your address; if it was you, sign in or
  reset your password"), at most once per 24h per account.
- Both cases return the same page: "Check your email to finish signing up." Mail
  is sent from the queue, so delivery failures are logged and never shown.
- An unverified account cannot sign in, and its status page returns 404.
  Accounts still unverified after 48h are deleted with their workspace by the
  prune loop (§6.7).

### 5.3 Password hashing

- **argon2id**: memory 19 MiB (19456 KiB), 2 iterations, parallelism 1, 16-byte
  random salt, 32-byte key. Stored as
  `$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>`.
- `Verify` parses the parameters from the stored string, so they can be raised
  later. Re-hashing on sign-in is added when the parameters first change.
- **Bounded concurrency:**
  - at most 4 hashes run at once
  - at most 64 requests wait
  - a wait longer than 2 seconds or a full queue returns 503 "The server is busy,
    please try again"
  - waiting respects the request context
- Rate limits and validation run before any hash (§8.3).

### 5.4 Sign-in and sessions

**`POST /login`** takes email and password.

- If the user doesn't exist, `VerifyDummy` runs so the timing matches.
- Any failure responds "Email or password is incorrect." with status 401.
- Only **after a correct password**:
  - a disabled user gets "This account is disabled." (403)
  - an unverified user gets "Verify your email to sign in." with a Resend form
    (403)
- Rate limits are in §8.2. The per-email throttle never fully locks an account,
  and a **device cookie** lets browsers that have signed in before skip it:
  - `uptime_device`, an HMAC (per-process key) of the user ID, set on successful
    sign-in, valid 90 days, `HttpOnly`, `SameSite=Lax`
  - a restart invalidates device cookies, which only means the normal throttle
    applies until the next successful sign-in
- `next` goes through `safeNext`.

**Creating a session:**

- 32 random bytes, base64url, as the cookie value. The database stores the hex
  SHA-256.
- **Cookie:**
  - name `__Host-uptime_session` with `Secure` when the request is HTTPS or
    `base_url` is https
  - otherwise `uptime_session` without `Secure` (local HTTP use)
  - always `HttpOnly`, `SameSite=Lax`, `Path=/`, `Max-Age` 30 days
- `expires_at = now + 30 days`.
- Sign-in deletes the session named by the incoming cookie, if any, and always
  issues a new token.
- A user has at most 20 live sessions; creating one more deletes the oldest.

**Validating a session** (`SessionAuth`, one query joining sessions, users,
memberships and workspaces):

- rejected if expired, idle for more than 7 days, or if the user is disabled or
  unverified
- `last_seen_at` on the session and the user is updated at most every 5 minutes

**Revoking:**

- sign-out deletes the session and redirects to `/login`
- "Sign out everywhere" deletes all of the user's sessions
- a password change deletes the other sessions and keeps the current one
- a password reset deletes all sessions, then signs the user in with a new
  session (§5.8)
- a password change, a reset or disabling a user also deletes the user's unused
  verify and reset tokens
- the hourly prune loop deletes expired sessions

### 5.5 Account page (`/account`)

- **Change password:** requires the current password; the new password follows
  §5.2; revokes the other sessions (§5.4). From M2, a notice email is queued
  when mail is configured.
- **Sign out everywhere.**
- **Delete account:**
  - requires the current password
  - removes the user and every workspace where they are the only member, with
    all of its data (§6.7)
  - the last remaining operator cannot delete their account
  - the confirmation uses the existing `data-confirm` dialog

### 5.6 Tokens

- 32 bytes from `crypto/rand`, base64url without padding. Only the hex SHA-256
  is stored.
- **Every lookup and consume filters by `kind`.** `verify` and `reset`
  additionally require a non-null `user_id` belonging to a user who is not
  disabled.
- Consuming is a single
  `UPDATE tokens SET used_at = ? WHERE token_hash = ? AND kind = ? AND used_at
  IS NULL AND expires_at > ?` that must affect exactly one row (two concurrent
  uses: one wins). When account creation depends on the token, the consume runs
  in that same transaction.
- A new `setup`, `verify` or `reset` token deletes the user's or instance's
  earlier unused tokens of the same kind.
- **Expiry:** setup 24h, invite 7 days, verify 24h, reset 1h.
- Used and expired tokens older than 7 days are deleted by the prune loop.

**Token pages follow one pattern:**

- `GET` shows a page with the token in a hidden field and never changes state
  (link scanners and prefetching consume nothing)
- `POST` (behind `sameOrigin`) consumes the token
- both send `Cache-Control: no-store`
- this applies to setup, invite sign-up, reset and verify

### 5.7 Email verification (M2)

- **On open sign-up:** create a `verify` token and queue the link
  `<base_url>/verify?token=…`.
- **`GET /verify`** shows "Confirm your email" with a button.
- **`POST /verify`:**
  - consumes the token and sets `verified_at`
  - if the browser has no session, or a session for this same user, it signs
    the user in and redirects to `/`
  - if the browser is signed in as a different user, it only says "Email
    verified. Sign in to continue", so nobody can be signed into an
    attacker's account
  - an invalid or expired token shows "This link is invalid or has expired",
    with a Resend form
- **`POST /verify/resend`** (email field) always responds "If an unverified
  account exists for that address, we sent a new link." Limits are in §8.2.

### 5.8 Forgot and reset password

- **`GET|POST /forgot` (M2):**
  - always responds "If an account exists for that address, we sent a reset
    link."
  - disabled users get no email
  - limits are in §8.2
  - the link is `<base_url>/reset?token=…`
- **`GET|POST /reset` (M1d):**
  - `GET` with a valid token shows the new-password form
  - `POST`:
    - sets the password and consumes the token
    - deletes all of the user's sessions and their unused verify and reset
      tokens
    - sets `verified_at` if it was empty, since receiving the link proves the
      user owns the address
    - signs the user in with a new session
    - queues a "your password was changed" notice (M2)
- **Without mail:** `-reset-password <email>` (§10) prints a reset link, which the
  operator delivers.

### 5.9 Messages

Authentication responses use fixed messages, defined as constants in
`login.go`, and never echo internal errors. Every POST keeps `sameOrigin`
and the 1 MB body limit.

## 6. Workspace isolation

### 6.1 Scoped store

- **`*store.Store`** keeps system and engine methods:
  - `Open`, `Close`, `Ping`, `Backup`, `Workspace`, `WorkspaceBySlug`,
    `OperatorWorkspace`
  - users, sessions and tokens
  - `DeleteWorkspace`, `Prune`, `PruneRollups`
  - the engine methods, all keyed by target ID: `AllTargets`, `TargetByID`,
    `InsertCheck`, `LatestCheck`, `OngoingIncident`, `OpenIncident`,
    `CloseIncident`, `InsertAlert`
- **`(*Store).Workspace(id) *WS`** returns `&WS{db, id}`. It panics for
  `id <= 0`, which can only be a programming error.
- **`*store.WS`** has every method the web layer uses:
  - workspace: `ID`, `Info`, `Rename`, `SetSlug`
  - targets: `ListTargets`, `GetTarget`, `GetTargetByName`, `CreateTarget`,
    `UpdateTarget`, `SetPaused`, `SetMuted`, `DeleteTarget`, `CountTargets`
  - checks and stats: `RecentChecks`, `ChecksSince`, `Stats`, `DailyUptime`,
    `RollupStats`, `Latencies`, `HourlyUptime`
  - incidents: `GetIncident`, `ListIncidents`, `AcknowledgeIncident`,
    `SetIncidentNote`, `CountUnacknowledged`, `CountIncidents`
  - alerts: `LastAlerts`, `CountAlerts`
  - maintenance: `CreateMaintenance`, `GetMaintenance`, `EndMaintenance`,
    `DeleteMaintenance`, `ListMaintenance`, `ActiveMaintenance`,
    `InMaintenance`
  - settings: `GetSetting`, `SetSetting`, `Notify() (config.Notify, error)`
- **Every `WS` query constrains the workspace unconditionally.**
  - Tables with `workspace_id` filter on it.
  - Tables without it use one shared fragment,
    ``inWS = `target_id IN (SELECT id FROM targets WHERE workspace_id = ?)` ``.
  - A target filter, where a method accepts one, is added *on top*, and
    `targetID 0` means "all targets in this workspace".
  - `LastAlerts` applies the filter inside its `MAX(id)` subquery.
- `CreateTarget` and `UpdateTarget` set `WorkspaceID` from the `WS` and return
  the stored target, so a form-built value can never carry a wrong or zero
  workspace.
- A row in another workspace returns `ErrNotFound`.
- The workspace-owned methods are removed from `*Store`, so old unscoped calls
  no longer compile.
- `store.Target` gains `WorkspaceID int64`.

### 6.2 Web layer

**`web.Server`** fields: `sys *store.Store`, `mon *monitor.Monitor`, `cfg`,
`logger`, `now`, `limits` (§8), `statusCache`, and from M2 `mail`.

**`authCtx`:**

- `User`, `WS *store.WS`, `Workspace` (name, slug), `IsOperator`
- `currentAuth(r)` validates the session cookie
- `requireUser` and `requireOperator` build on it
- public pages (`/s/{slug}`, `/login`) call `currentAuth` optionally

**Route classes** (in `routes.go`):

| Class | Middleware | Handler signature |
|---|---|---|
| `public` | none | `http.HandlerFunc` |
| `setup` | allowed only in setup mode, otherwise 404 | `http.HandlerFunc` |
| `guest` | signed-in users are redirected to `/` | `http.HandlerFunc` |
| `user` | `requireUser` | `func(w, r, *authCtx)` |
| `workspace` | `requireUser`, then per-session API limit for `/api/*` | `func(w, r, *authCtx)` |
| `operator` | `requireOperator` (404 for others) | `func(w, r, *authCtx)` |

- POST routes in every class are wrapped with `sameOrigin`.
- `requireUser` responds 401 for API, partial and SSE requests, and otherwise
  redirects to `/login?next=…`.
- Because workspace handlers take `*authCtx` as a parameter, forgetting the
  middleware does not compile.

**Shared view helpers**, which take the workspace explicitly:

- `s.page(r, title)` sets theme, time and flash, and is used by public pages
- `s.wsBase(r, a, page, title)` adds sidebar counts, maintenance and groups
  from `a.WS`
- `view`, `views` and `uptime` take `ws *store.WS`

**The `AfterSetup` hook** in `web.Options` is a function provided by `main` that
seeds the operator's workspace and starts its runners after setup.

### 6.3 Monitor engine

- **Notifiers.** `Options.Notifiers func(ctx, workspaceID int64)
  (notify.Multi, error)`, cached per workspace.
  - `SetNotifiers(ws, n)` replaces one entry.
  - If loading fails, the error is logged and the workspace falls back to the
    log channel.
- **Internal addresses.** `Options.PrivateAllowed func(workspaceID int64) bool`
  decides between the guarded and unguarded HTTP client for each target
  (§7.1).
- **Scoped methods:**
  - `Snapshot(ws)`, `Get(ws, id)`, `Notifiers(ws)` and `SendTest(ctx, ws,
    channel)` only see that workspace's data.
  - `Test(ctx, ws, target)` probes an unsaved target with the client for that
    workspace.
  - `CheckAsync(ws, id)` does nothing for another workspace's target.
- **`Reload(ctx, targetID)`** re-reads the target and its workspace through the
  store and ignores the caller's copy. From M3 it refuses suspended
  workspaces.
- **Removal.** `Remove(id)` takes the workspace from the stored state.
  `RemoveWorkspace(ws)` stops every runner of a workspace and waits for
  in-flight probes.
- **Maintenance lookups** use `store.Workspace(t.WorkspaceID).InMaintenance`.
- **Events.** `Event` gains `WorkspaceID int64` (`json:"-"`). Maintenance
  events published by the web layer carry the workspace ID.
- **Check now is coalesced:** if a check for the target is running or queued, a
  new request is ignored (§8.2).

### 6.4 Live updates (SSE)

- `/events` forwards only events whose `WorkspaceID` equals the session's
  workspace. Events with `WorkspaceID == 0` are dropped.
- Each stream re-checks its session every 60 seconds and closes when the
  session has been revoked or the user disabled.
- At most 5 concurrent streams per user (§8.2).

### 6.5 Public status pages

- **`GET /s/{slug}`:**
  - unknown slugs, unverified owners and (M3) suspended workspaces return 404
  - a signed-in **owner** of that workspace gets the live page with the settings
    form, and it cannot be framed
  - everyone else gets the cached anonymous page, which may be framed
- **The anonymous page** is built by `statusData(ctx, ws, theme, partial)` with
  no request, session or cookies involved.
  - Every count and banner comes from that workspace.
  - `ShareURL` uses `base_url` when set, otherwise the request's scheme and host.
    It appears only on the owner's live page, which is never cached.
- **`GET /status`:**
  - redirects (302) to `/s/<slug of the operator's workspace>`
  - in setup mode it redirects to `/s/default` on upgraded installs, and returns
    404 on fresh ones
  - in-app links use the user's own `/s/<slug>`
- **The cache** is keyed by `slug|theme` plus `|partial`.
  - Entries exist only for existing slugs.
  - Invalidation is per slug, so one workspace's change does not clear the
    others.
  - The cache is an LRU limited to 32 MB.
  - Limits are in §8.2.
- **Framing:**
  - `securityHeaders` allows `frame-ancestors *` for anonymous `GET` and `HEAD`
    on `/s/{slug}`
  - the owner's view, the `/status` redirect and every other route deny framing
- **Slug rules:**
  - 3–40 characters of `[a-z0-9-]`, no leading or trailing `-`, unique
  - no reserved words: status pages live under `/s/`, so slugs cannot clash with
    routes
- **Deriving a slug** from a workspace name:
  1. lowercase, map runs of other characters to `-`, strip `-` at both ends
  2. truncate to 40 characters, then strip trailing `-` again
  3. if shorter than 3 characters, use `ws-` plus 6 random base32 characters
  4. on a collision, truncate so the suffix fits, then append `-2`, `-3` and so
     on
  The result always satisfies the rules above.

### 6.6 Alert channels per workspace

- A workspace's channels come **only** from its own `settings` row (`notify`).
  - There is no runtime fallback to the config file's `notify`.
  - A workspace with no stored setting has no channels, which means the log
    channel.
  - `alerts.go`'s `notifyConfig` fallback to `s.cfg.Notify` is removed.
- The config file's `notify` is copied into the operator's workspace at
  seeding, but only when that workspace has no stored `notify` setting
  (today's behaviour of `loadNotifiers`).
- **Saving channels:**
  - "blank keeps the saved secret" applies only while the secret's
    **destination** is unchanged
  - a changed SMTP host, port or username, a changed Telegram chat ID, or a
    changed Slack webhook host clears the saved secret, which must then be
    typed again
  - this stops a saved password from being sent to a server someone else chose
- Restrictions for non-operator workspaces are in §7.2.

### 6.7 Deleting accounts and workspaces

`DeleteWorkspace` and account deletion run in this order:

1. Delete the user's sessions. From M3, also mark the workspace suspended.
2. `mon.RemoveWorkspace(ws)` stops the runners and waits for in-flight probes.
3. In one transaction, delete the workspace's alerts, incidents, checks and
   `daily` rows (through its targets), then the workspace; the cascades remove
   targets, maintenance, settings and memberships. Delete the user if they no
   longer belong to any workspace.
4. Evict the workspace's notifier cache entry and its status-page cache entries.

Notes:

- `Reload` re-reads the target, so a request that was already running in
  another tab cannot restart a runner after step 3 (the target is gone).
- Alerts already in flight may finish; this is documented.
- Deleted data remains in pre-migration and `-backup` files until the operator
  removes them; the README says so.

## 7. Outbound safety

### 7.1 Internal-address guard (`internal/netguard`)

**Rule: an address is allowed only if all of these hold:**

- after `Unmap()`, it is global unicast
- it is not in the IANA IPv4 or IPv6 Special-Purpose Address Registries
- it is not in a translation or tunnelling range: `64:ff9b::/96`,
  `64:ff9b:1::/48`, `2002::/16`, `2001::/32`

**Examples of blocked addresses:**

- loopback, `0.0.0.0/8`, unspecified
- RFC 1918, `100.64.0.0/10`, `fc00::/7`
- link-local `169.254.0.0/16` (cloud metadata) and `fe80::/10`
- `fec0::/10`, `240.0.0.0/4`, multicast, broadcast, documentation ranges
- `192.0.0.0/24`, `192.88.99.0/24`, `198.18.0.0/15`, `100::/64`

**Where it is enforced:**

- in `net.Dialer.ControlContext`, on the actual address being connected to,
  after DNS resolution
- this covers redirects and DNS rebinding for every connection
- guarded transports set `Proxy: nil`, because environment proxies would fetch
  internal addresses on the user's behalf

**Error text:** "blocked: destination is an internal address", with no IP, so
the guard cannot be used to look up internal hostnames.

**Which client is guarded** (`allow_private_targets`):

| Setting | Operator workspaces | Other workspaces |
|---|---|---|
| `none` | guarded | guarded |
| `operator` (default) | unguarded | guarded |
| `all` | unguarded | unguarded |

Startup logs a WARN when `all` is combined with any `signup` mode other than
`closed`.

**Applied to:**

- the monitor client (monitors, Test URL)
- the notify HTTP client (Slack, Telegram)
- the per-workspace SMTP dialer

It is not applied to the operator-configured system `mail` dialer.

**Save-time validation** gives an early, friendly error for URLs whose host is
an IP literal or `localhost`. The dial-time check is the real control.

**A static test** fails if `http.Client{`, `&http.Client{`, `net.Dialer{` or
`&net.Dialer{` appears in non-test code outside `internal/netguard` and the
system mail sender. Today's `targetTest` client (`&http.Client{}`) is removed.

### 7.2 Alert channels from non-operator workspaces (M1e)

- **Webhook and Telegram clients** do not follow redirects
  (`CheckRedirect` returns `http.ErrUseLastResponse`), since 307 and 308 would
  resend the POST body.
- **Telegram bot tokens** must match `^\d+:[A-Za-z0-9_-]{30,}$` before being
  put in the request path.
- **Workspace email channels:**
  - port 465 or 587 only
  - a username is required, and TLS is required (implicit on 465, STARTTLS
    mandatory on 587)
  - at most 5 `To` addresses
- Test sends are rate limited (§8.2).
- The operator's workspaces keep today's options.

### 7.3 Identifying the monitor

Probes send `User-Agent: uptime-monitor/<version> (+<base_url>)` when
`base_url` is set, so the owner of a probed site can find the instance and its
abuse contact.

## 8. Rate limiting and abuse controls

### 8.1 The limiter (`internal/ratelimit`)

- **Sliding window:** `New(window, max, maxKeys)`, with:
  - `Allow(key) (ok bool, retryAfter time.Duration)` to check a key
  - `Hit(key)` to record an event
  - `Reset(key)`
- **Bounded memory:** at most `maxKeys` keys (default 100,000), with the least
  recently used evicted first.
- **Client keys:** the address from `clientKey`. IPv6 addresses are grouped by
  /64, so rotating addresses inside one allocation does not create new
  buckets.
- **Responses:** forms re-render with a message and status 429; the API returns
  JSON 429 with `Retry-After`. The message never depends on whether an account
  exists.
- **Scope:** state is in memory, so it resets on restart and assumes a single
  server process, which is the supported deployment. This is documented, and
  argon2id stays the main defence against guessing.

### 8.2 Limits

| Area | Limit | Step |
|---|---|---|
| Sign-in, per address | 5 failures per minute, then refused until the window passes | M1d |
| Sign-in, per email + address | 10 failures per 15 minutes, then refused for that pair | M1d |
| Sign-in, per email | after 20 failures in 15 minutes, at most one attempt every 5 seconds (slowed, never locked). Browsers with a valid device cookie for that account are exempt | M1d |
| Setup unlock | 5 attempts per 15 minutes per address (token and password paths together) | M1d |
| Sign-up | 5 per hour per address; `limits.signups_per_hour` (default 100) across the instance; optional `limits.max_workspaces` | M1e / M2 |
| Password hashing | 4 running, 64 waiting, 2-second wait, then 503 | M1d |
| System email, per recipient | 3 per hour and 10 per day, shared by verify, resend, forgot, duplicate-sign-up and password-change emails | M2 |
| Duplicate sign-up notice | at most one per 24 hours per account | M2 |
| Forgot and resend, per address | 10 per hour | M2 |
| System email, whole instance | `limits.mail_per_hour` (default 500); when reached, messages are dropped with a WARN | M2 |
| Check now | ignored if that monitor's check is running or queued; 1 per 10 seconds per monitor; 30 per minute per workspace | M1e |
| Test URL | 10 per minute per workspace | M1e |
| Alert test send | 5 per minute per workspace | M1e |
| Creating monitors | 30 per hour per workspace (stops delete-and-re-create from getting around the monitor cap) | M1e |
| Monitors per workspace | `limits.max_monitors` (default 20; 0 is unlimited) | M1e |
| Check interval | `limits.min_interval` (default 30s) | M1e |
| Traffic to one host | across all non-operator workspaces, the combined check rate to one hostname (the sum of 1/interval) may not exceed `limits.host_requests_per_second` (default 1). Checked when a monitor is created or updated | M1e |
| JSON API | 120 requests per minute per session | M1d |
| Live updates | 5 streams per user | M1d |
| Public status pages | 120 requests per minute per address; at most 2 page rebuilds running across the instance (others get the stale copy or wait, as today) | M1c |

The operator's workspaces are exempt from the per-workspace limits (Check now,
Test URL, alert tests, monitor creation, caps, interval, host rate). Sign-in,
setup and hashing limits apply to everyone.

### 8.3 Order of checks

To keep floods cheap to reject, each request runs these steps in order:

1. rate limits
2. form and field validation (lengths in bytes before parsing)
3. token or invite validity
4. password hashing
5. the database transaction

## 9. Configuration

### 9.1 New keys

| Key | Default | Meaning |
|---|---|---|
| `base_url` | empty | External URL, e.g. `https://status.example.com`: absolute http(s), no path. Used for email links, the setup link, `ShareURL` and the User-Agent. |
| `signup` | `closed` | `closed`, `invite` or `open` |
| `allow_private_targets` | `operator` | `none`, `operator` or `all` (§7.1) |
| `limits.max_monitors` | `20` | per workspace; 0 is unlimited |
| `limits.min_interval` | `30s` | per workspace |
| `limits.max_workspaces` | `0` | 0 is unlimited; counts all workspaces |
| `limits.signups_per_hour` | `100` | across the instance |
| `limits.mail_per_hour` | `500` | system email across the instance |
| `limits.host_requests_per_second` | `1` | §8.2 |
| `mail` | none | system SMTP (M2): `smtp_host`, `smtp_port` (default 587), `username`, `password`, `from`, `require_tls` (default **true**). Uses `notify.Email.Send`. |

The other rate limits in §8.2 are constants in the code, listed in the README.

### 9.2 Changed keys

- **`admin_password`:** only unlocks setup on an upgraded install, and is
  ignored with a WARN once a user exists. `CheckAuth` is removed.
- **`insecure_no_auth`:** removed. If present, startup logs a WARN that it has
  no effect.
- **`targets` and `notify`:** seeded into the operator's workspace at startup
  and through `AfterSetup`, only when that workspace exists. Targets are seeded
  by name (today's rule); `notify` only when the workspace has none stored.
  Config-seeded monitors bypass the per-workspace limits because they belong to
  the operator.

### 9.3 Startup checks

Startup fails with a clear message when:

- `signup` is `open` without `mail`
- `signup` is not `closed` and `base_url` is empty
- `mail` is set without `base_url`
- `base_url` is invalid
- `signup` is not `closed` and `base_url` uses `http` with a host other than
  `localhost` or a loopback address (a public instance must use HTTPS)
- a value is not supported by the running step (§2)

## 10. Command line

| Flag | Behaviour |
|---|---|
| `-setup-link` | In setup mode, creates a setup token (invalidating the previous one) and prints the link. Otherwise errors with "setup is already complete". |
| `-reset-password <email>` | Creates a 1-hour reset token for that user and prints `<base>/reset?token=…`. Errors if there is no such user. |
| `-invite [email]` | (M1e) Creates a 7-day invite, optionally bound to an email, and prints `<base>/signup?invite=…`. |
| `-backup <path>` | Unchanged. |

`<base>` is resolved as in §5.1. These flags open the database (running any
pending migration), act, print and exit; they never start the server.

## 11. Operator panel (M3)

- `/admin/*` is available to users with `is_operator = 1`; everyone else gets
  404.
- **Users page:** email, workspace name and slug, created, last seen, verified,
  disabled, monitor count. 50 per page, newest first.
  - Email search escapes `%` and `_` for `LIKE`.
  - Search by monitored hostname helps find the source of abusive traffic.
- **Actions** (POST, behind `sameOrigin`, with confirmations):
  - **Disable:**
    - sets `disabled_at`
    - deletes the user's sessions and their verify and reset tokens
    - suspends their workspaces and stops their runners
    - an operator cannot disable themselves or another operator
  - **Enable:** reverses disable and reloads the runners.
  - **Delete:**
    - as in §6.7
    - requires the operator's password in the confirmation form
    - operators cannot be deleted here
  - **Resend verification:** only for unverified users.
  - **Reset link:** emailed when mail is configured, otherwise shown once on the
    page with `Cache-Control: no-store`.
- **Invites page:**
  - create (optional email; the link is shown once)
  - list pending invites (created, expires, bound email)
  - revoke
  - invites expire after 7 days, the same as the CLI
- Every action is logged as an `operator_action` event (§12).
- A CLI `-enable-user <email>` exists for recovery.

## 12. Security logging

Security events go to the structured log, using the existing slog JSON format.

**Events:**

- `login_ok`, `login_failed`, `login_throttled`
- `setup_attempt`, `setup_complete`
- `signup`, `signup_duplicate`
- `password_changed`, `password_reset`
- `sessions_revoked`, `account_deleted`
- `operator_action`
- `mail_dropped`

**Fields:** `event`, `user_id` when known, `ip`, and for unknown emails
`email_hash` (SHA-256 truncated to 16 hex characters). Passwords, tokens,
cookies and session IDs are **never** logged. The one exception is the setup
link, which is printed on purpose and documented.

## 13. Testing

All tests run under `go test -race`. Each step adds the tests for what it
builds.

**Test environment.**

- `newEnv(t)` creates the operator account, workspace and a signed-in client
  through the store.
- `e.anon(t)` returns a signed-out client.
- `e.account(t, email)` creates another account and returns its client and
  `*store.WS`.
- Test environments set `allow_private_targets: "all"`, because `httptest`
  servers listen on 127.0.0.1.
- Mail is tested with a recording fake that implements the web layer's mailer
  interface. Real SMTP stays covered by the notify package tests.

**Unit tests:**

- **password:** round trip, wrong password, parameter parsing, dummy verify,
  concurrency bounds and the 503 path
- **tokens:**
  - expiry, single use, stored hashed
  - two concurrent consumes: exactly one wins
  - a token of one kind is rejected as another kind
  - disabled users' tokens are rejected
- **sessions:**
  - absolute and idle expiry; a new token on every sign-in
  - the incoming session is deleted on sign-in
  - revocation on password change and reset
  - disabled and unverified users rejected
  - the 20-session cap
- **slugs:** derivation (edges, truncation, short names, collisions at 40
  characters) and validation
- **ratelimit:** window, retry-after, key cap and eviction, IPv6 /64 grouping
- **config:** every startup check in §9.3; pointer defaults
- **netguard:**
  - an address table including IPv4-mapped IPv6, NAT64, 6to4, Teredo,
    `0.0.0.0`, `[::]`, `169.254.169.254`, `100.64.0.1`, `fd00::1`, `fec0::1`,
    `240.0.0.1`
  - a test server on 127.0.0.1 must be refused when reached directly, through a
    redirect from an allowed host, and through a hostname resolving to
    127.0.0.1 when guarded, and must be reachable when unguarded
  - the error text contains no IP

**Migration tests:**

- **Upgrade:** a database created with the step-1 schema plus sample rows in
  every table is opened, and:
  - all rows are in workspace 1, and settings moved
  - two workspaces can then hold monitors with the same name
  - the backup exists with mode 0600
  - `user_version` is 2 and `foreign_key_check` passes
  - a second start does not migrate again
- **Fresh:** a new database creates no workspace and no backup.
- **Sequences:** a deleted target's ID is not reused after the rebuild.

**Isolation suite:**

- Workspaces A and B each have a monitor, checks, an incident, a maintenance
  window, alert settings and a status page.
- For every `workspace` route in the route table, B's client sends the
  A-targeting request (A's IDs in paths and in query parameters such as
  `?target=`).
  - The expected result is 404, or a response containing none of A's data.
  - A database snapshot of A's rows must be unchanged afterwards.
- **Aggregates:** B's overview counts, p95, `/api/status`, `/api/incidents`,
  `/api/maintenance` and the alerts "last delivery" map are identical whether or
  not A exists.
- **Live updates:** B's `/events` receives no events for A's targets or
  maintenance.
- **Status pages:**
  - `/s/<A>` shows only A's monitors
  - B, signed in, warming the cache for `/s/<A>` while B has a down monitor and
    an active maintenance window leaks nothing of B into the anonymous page
  - only A's owner gets the settings form
- **Alert channels:** a new workspace with no stored `notify` setting gets no
  channels, and its `/alerts` page shows none of the operator's config.
  Changing a channel's destination clears its saved secret.

**Static tests:**

- every route is in the route table with a class
- every `workspace` route has an isolation case
- `s.sys` appears only in an explicit allow-list of files (`session.go`,
  `setup.go`, `login.go`, `signup.go`, `reset.go`, `verify.go`, `forgot.go`,
  `account.go`, `admin.go`, `web.go`'s healthz, `status.go`'s slug resolver)
- no `http.Client{` or `net.Dialer{` outside `netguard` and the system mail
  sender

**Flow tests:**

- **Setup:**
  - the token works once
  - `POST /setup` without a valid token or with a used token gets 404
  - the `admin_password` path works only on upgraded installs
  - the unlock limiter works
  - `/setup` is 404 afterwards
  - `/status` behaves correctly in setup mode
- **Sign-up:**
  - each mode, matching the table in §5.2
  - invites: single use, expiry, email binding, and a failed sign-up doesn't
    consume the invite
- **Verification and reset:** the GET page changes nothing; the POST consumes;
  sign-in after verification respects an existing session for a different user.
- **Enumeration:** for login failures, forgot, resend and open sign-up, the
  status code and body are byte-identical for existing and unknown emails. A
  timing check requires the medians of 30 requests each to differ by less than
  50 ms.
- **Rate limits:** every limit in §8.2, including the per-email slowdown and the
  device-cookie exemption.
- **Account deletion:** all data and runners are removed, the last operator is
  protected, and an in-flight request cannot restart a runner afterwards.
- **Limits:** enforced for other workspaces; the operator is exempt.

**For every step:**

- gofmt, vet, race tests, govulncheck and a Docker build
- a browser check of new pages under the CSP (Playwright, or a manual check if
  unavailable)
- an independent code and security review before committing

## 14. Documentation

- **README:** accounts overview, sign-up modes, `base_url`, mail,
  `allow_private_targets`, limits and the rate-limit constants, operator CLI,
  that the setup link appears in the log, and that deleted data remains in
  backups. An "Upgrading to accounts" section covers the setup link, the role
  of `admin_password`, the removed `insecure_no_auth`, the automatic backup,
  and the LAN monitoring default.
- **SECURITY.md:** supported versions, private reporting through GitHub
  security advisories, what to include, and the expected response time.
- **config.example.json:** gains the new keys, with `signup: "closed"`.

## 15. Risks and mitigations

| Risk | Mitigation |
|---|---|
| A query misses its workspace filter | scoped store with unscoped methods removed; unconditional workspace filter in every `WS` query; default-deny `s.sys` allow-list; route-classified isolation suite including aggregates |
| Operator secrets reach another workspace | no config fallback at runtime; secrets kept only while the destination is unchanged; isolation tests |
| Setup takeover | POST authorised and token consumed in the same transaction as account creation; password unlock only on upgraded installs; unlock limiter |
| Cache shows another account's data | anonymous pages built without the request or session; per-slug keys |
| SSRF | allow-list rule at dial time; tunnelling ranges blocked; proxies ignored; no redirects for webhooks; static test against stray clients |
| Account enumeration | identical responses; dummy hash; mail queued so responses don't wait for SMTP; sign-up always hashes; timing test |
| Brute force and lockout of real users | per-address and per-pair limits; slowdown instead of lockout per email; device cookie exemption; argon2id |
| Email abuse | per-recipient and instance budgets; notices rate-limited; no user text in system emails except the link |
| Flooding third parties | monitor caps, minimum interval, per-host rate across workspaces, Check now coalescing and limits, User-Agent with contact |
| Hashing floods | cheap checks first; 4 running, 64 waiting, 2-second wait |
| Login CSRF through emailed links | GET never changes state; POST does not replace another user's session |
| Host-header poisoning | email and setup links built from `base_url`; `ShareURL` only on the uncached owner view |
| Data loss on upgrade | automatic backup; documented rebuild procedure with `foreign_key_check`; one transaction per step; `user_version` |
| LAN monitoring breaks on upgrade | `allow_private_targets` defaults to `operator`, which owns all migrated data |
| In-memory limiters reset on restart | documented: single process is the supported deployment; argon2id remains the main defence |
