# Accounts M1d: Accounts and Sessions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the single admin password with real accounts. This covers:
- an argon2id password package
- users, tokens and database-backed sessions
- a protected first-run setup page that adopts workspace 1
- email and password sign-in with the spec's rate limits and device cookie
- sign-out
- an account page (change password, sign out everywhere)
- a `/reset` page
- the `-setup-link` and `-reset-password` CLI flags
- per-workspace handlers that take an `*authCtx`
- an isolation suite and static checks that prove one account can't reach another's data

**Architecture:**

- **Store.** The store gains `users.go`, `tokens.go` and `sessions.go`. They use the v2 tables M1c already created, and they share one secret helper (`newSecret` / hash).
- **Web routes.** The web layer swaps `classAdmin` for the spec's route classes (`public`, `setup`, `guest`, `user`, `workspace`). Workspace and user handlers take `(w, r, *authCtx)`, so forgetting the middleware doesn't compile.
- **Setup mode.** It is an `atomic.Bool` loaded at startup: on while no user exists, off once setup commits.
- **Seeding.** Config targets and channels are seeded into the operator's workspace only once a user exists. That happens at startup or in the `AfterSetup` hook.
- **Removed.** The old in-memory sessions, `requireAdmin`, `CheckAuth`, `insecure_no_auth` enforcement, `pinnedWorkspace` and `s.ws(r)` all go.

**Execution:** subagent-driven. Implementers are Sonnet subagents; reviewers are Opus. Reuse what already exists:
- `ratelimit`, `clientKey`, `sameOrigin`, `safeNext`, `passwordEqual` (setup unlock only)
- `render`, `renderStatus`, `withFlash`, `redirect`, `loadByID`
- `store.WS`, `createWorkspace`, `querier`, `newEnv`, `cookieJar`

Write no code a task doesn't need.

**Tech Stack:** Go 1.26, `modernc.org/sqlite`, and `golang.org/x/crypto/argon2`, the one new dependency (D-107).

**Spec:** [docs/superpowers/specs/2026-11-10-accounts-design.md](../specs/2026-11-10-accounts-design.md), step **M1d** (§2). Sections used: §3, §5.1, §5.3–§5.6, §5.8 (`/reset`), §5.9, §6.2, §6.4, §6.5 (owner view), §8.1–§8.3, §9, §10, §12, §13.

## Global Constraints

- **Branch.** Work on `accounts-m1d`, cut from `accounts-m1c`. Nothing is merged or pushed.
- **Passwords.** argon2id with memory 19456 KiB, 2 iterations, parallelism 1, a 16-byte salt and a 32-byte key. Stored as `$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>`, standard base64 without padding. Length rule: 10 characters to 256 bytes, with no composition rules. At most 4 hashes run at once and 64 wait. A wait over 2 seconds, or a full queue, gets 503 "The server is busy, please try again" (§5.3).
- **Order of work per request** (§8.3): rate limits, then field validation (lengths in bytes before parsing), then token validity, then password hashing, then the DB transaction. Hashing happens before `BeginTx`.
- **Secrets.** Tokens and sessions are 32 random bytes, base64url without padding. Only the hex SHA-256 is stored. Only `store/tokens.go` creates secrets (`newSecret`).
- **Messages.** Fixed constants in `login.go` (§5.9). Sign-in failure is exactly "Email or password is incorrect." with status 401. A disabled user gets 403 "This account is disabled.", and only after a correct password.
- **Session cookie.** It is named `__Host-uptime_session` with `Secure` when the request is HTTPS or `base_url` is https. Otherwise it is `uptime_session`. It is always `HttpOnly`, `SameSite=Lax`, `Path=/` and `Max-Age` 30 days.
- **Session lifetime.** 30-day absolute expiry and 7-day idle expiry. Last-seen is updated at most every 5 minutes. At most 20 sessions per user; the oldest is deleted.
- **Logging.** Security events go to slog with an `event` attribute (§12). Passwords, tokens, cookies and session IDs are never logged. The setup link is the documented exception.
- **POST routes.** Every POST keeps `sameOrigin` and the 1 MB body limit.
- **Config support** (§2). This step accepts `signup: "closed"` only and rejects any other value, with `signup "<v>" is not supported yet`. `mail` and `limits` are rejected if present, with `mail is not supported yet` and `limits is not supported yet`. `base_url` is accepted and validated (§9.1, §9.3).
- **Checks.** `gofmt -l .` prints nothing, and `go vet ./...` and `go test -race -count=1 ./...` pass at every commit. Run govulncheck after the `x/crypto` dependency lands.

## Scope rulings (recorded as D-124 in Task 11)

1. **Setup adopts workspace 1 on every install** (follows D-123). A database counts as an **upgraded install** when no user exists and workspace 1 already holds targets, settings or maintenance rows. That decides whether the `admin_password` unlock is offered. At startup in setup mode, config targets and channels are **not** seeded, so a fresh install's workspace 1 stays empty until setup. Setup renames workspace 1 to the chosen name; the slug stays `default` until slug editing lands in M1e.
2. **No `BEGIN IMMEDIATE`.** The store runs on one connection, so transactions are already serial. Setup re-checks "no user exists" inside the transaction.
3. **`password.Hasher` is a type**, not package functions. `web.Options.Hasher` is injected (nil means `password.Default()`), so tests use cheap parameters. The enumeration timing test uses the real defaults.
4. **The `operator` route class and `requireOperator` come in M3**, the first step with operator routes. `authCtx.IsOperator` exists now.
5. **Not in M1d:** unverified-user rejection paths (M2), and account deletion (M1e per §2). `verified_at` is set on every account this step creates.

## Review Focus

1. **Cross-account access through any workspace route,** including IDs passed as query parameters (`?target=`) and aggregates. Test: Task 10, `TestIsolationSuite`.
2. **Setup takeover.** A POST to `/setup` without a valid unused token (or without the right password on an upgraded install) must not create a user. Two concurrent setups must create one user. Test: Task 6, `TestSetup*`.
3. **Account enumeration on sign-in.** Status and body must be byte-identical for existing and unknown emails, and timing medians must differ by less than 50 ms. Test: Task 7, `TestLoginNoEnumeration`.
4. **Lockout of a real user.** Per-email failures only slow sign-in, never block it, and a valid device cookie skips the slowdown. Test: Task 7, `TestLoginPerEmailSlowdown`.
5. **A revoked or expired session keeps working.** Sign-out, sign-out-everywhere, password change or reset, idle expiry and an open SSE stream must all stop it. Tests: Task 4 store tests; Task 9 `TestEventsClosesOnRevoke`.

---

## File Structure

| File | Change |
|---|---|
| `internal/password/password.go`, `password_test.go` | new: `Hasher`, `Params`, `Hash`, `Verify`, `VerifyDummy`, `ErrBusy`, `CheckLength` |
| `internal/config/config.go` | `BaseURL`, `Signup`, rejection of `mail`/`limits`, `insecure_no_auth` warn-only, `CheckAuth` removed (Task 5) |
| `internal/store/tokens.go`, `users.go`, `sessions.go` (+ tests) | new |
| `internal/web/clientip.go` | IPv6 /64 grouping |
| `internal/web/session.go` | `authCtx`, `currentAuth`, `requireUser`, cookie helpers, `setupGate` (old in-memory sessions removed) |
| `internal/web/routes.go` | classes `public`, `setup`, `guest`, `user`, `workspace` |
| `internal/web/login.go`, `setup.go`, `account.go`, `reset.go` | pages |
| `internal/web/templates/setup.html`, `reset.html`, `account.html`, `login.html`, `layout.html` | templates (reuse `public.html` / `layout.html`, `.login .card`, `.field`, `.btns`) |
| every workspace handler file | signature `(w, r, a *authCtx)`, `a.WS` replaces `s.ws(r)` |
| `internal/web/workspace.go` | deleted |
| `internal/web/isolation_test.go`, `static_test.go` | new |
| `main.go` | setup mode, seeding moved, `-setup-link`, `-reset-password`, `pruneLoop` prunes sessions/tokens |
| `README.md`, `config.example.json`, `docs/decisions.md` | Task 11 |

---

### Task 1: `internal/password`

**Files:** Create `internal/password/password.go`, `internal/password/password_test.go`. Modify `go.mod`/`go.sum` with `go get golang.org/x/crypto@latest`.

**Interfaces:**
- **Types and constructors.**
  - `type Params struct { Memory uint32; Time uint32; Threads uint8 }`
  - `var DefaultParams = Params{Memory: 19456, Time: 2, Threads: 1}`
  - `type Hasher struct{ … }` holds the params, a 4-slot run semaphore, a 64-slot wait counter, and a `wait time.Duration` of 2s.
  - `func New(p Params) *Hasher`. `func Default() *Hasher` returns a process-wide `New(DefaultParams)`.
- **Hashing.**
  - `func (h *Hasher) Hash(ctx context.Context, pw string) (string, error)` returns a PHC string.
  - `func (h *Hasher) Verify(ctx context.Context, phc, pw string) (bool, error)`. It parses `m`, `t` and `p` from `phc`, so stored hashes keep working after the params change. It compares with `subtle.ConstantTimeCompare`.
  - `func (h *Hasher) VerifyDummy(ctx context.Context)` hashes against a fixed dummy PHC built once with `h`'s params. Use it when the email is unknown.
- **Busy handling.**
  - `var ErrBusy = errors.New("password: server busy")`.
  - All three methods acquire the semaphore. When 64 callers are already waiting, or a wait passes 2s, they return `ErrBusy`. When `ctx` ends first, they return `ctx.Err()`.
- **Length rule.**
  - `func CheckLength(pw string) error` returns `ErrTooShort` (fewer than 10 runes) or `ErrTooLong` (more than 256 bytes).
  - The two errors read "Use at least 10 characters." and "Use at most 256 bytes."

- [ ] **Step 1: Write the failing tests.**

```go
func TestRoundTrip(t *testing.T) // Hash then Verify true; a wrong pw gives false; the PHC starts with "$argon2id$v=19$m=19456,t=2,p=1$" for Default()
func TestVerifyParsesParams(t *testing.T) // a hash made by New(Params{Memory: 64, Time: 1, Threads: 1}) verifies with Default()
func TestVerifyRejectsGarbage(t *testing.T) // "", "$bcrypt$...", a truncated PHC: false with a non-nil error, never a panic
func TestCheckLength(t *testing.T) // "123456789" -> ErrTooShort; "1234567890" ok; 10 multi-byte runes ok; 257 bytes -> ErrTooLong
func TestBusy(t *testing.T)
// h := New(Params{Memory: 64, Time: 1, Threads: 1}). Set h.wait to 50ms in the test (same package).
// Hold the 4 run slots (an unexported test hook, or a blocking hash fn variable).
// A 5th Hash returns ErrBusy after about 50ms.
// With 64 waiters already queued, the 65th returns ErrBusy immediately.
// A cancelled ctx while waiting returns context.Canceled.
```

- [ ] **Step 2: Run to fail.** `go test ./internal/password` fails because the package doesn't exist.
- [ ] **Step 3: Implement.** Keep a seam for the busy test: an unexported `hashFn` field, defaulting to `argon2.IDKey`.
- [ ] **Step 4: Run the full suite** (`gofmt`, `vet`, race tests), then `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`. Expected: no new module findings.
- [ ] **Step 5: Commit** `feat(password): argon2id hasher with bounded concurrency`.

---

### Task 2: Config keys for accounts

**Files:** `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- **New fields.**
  - `BaseURL string` with `json:"base_url"`.
  - `Signup string` with `json:"signup"`. It defaults to `"closed"`, and any other value is an error: `config: signup "%s" is not supported yet`.
  - `Mail json.RawMessage` with `json:"mail"` and `Limits json.RawMessage` with `json:"limits"`. Either one present gives `config: mail is not supported yet` / `config: limits is not supported yet`.
- **`base_url` validation.** It must parse as absolute `http` or `https`, with a host and no path other than `""` or `"/"`, no query and no fragment. Any trailing `/` is trimmed. Otherwise the error is `config: base_url must be an absolute http(s) URL with no path`.
- **Helpers.**
  - `func (c *Config) HTTPSBase() bool` reports whether `BaseURL` starts with `https://`.
  - `func (c *Config) LinkBase() string` returns `BaseURL` when set. Otherwise it returns `"http://" + host`, where the host is the listen host, or `localhost` when the listen host is empty, `0.0.0.0` or `::`, with the listen port (§5.1).
- **Unchanged here.** `InsecureNoAuth` and `CheckAuth` stay until Task 5.

- [ ] **Step 1: Failing tests:** `TestAccountsConfigKeys`
  - Default `Signup` is `"closed"`.
  - `"open"` gives an error containing `not supported yet`.
  - `mail: {}` gives an error containing `mail is not supported yet`.
  - `base_url`: `"https://status.example.com/"` passes and is stored without the trailing slash. `"https://x/y"`, `"ftp://x"` and `"status.example.com"` all give errors.
  - `LinkBase` for `:8080` is `http://localhost:8080`; for `127.0.0.1:9` it is `http://127.0.0.1:9`; with a `BaseURL` it is that URL.
- [ ] **Steps 2–4: Fail, implement, run the full suite.**
- [ ] **Step 5: Commit** `feat(config): base_url, signup (closed only), reject mail and limits until supported`.

---

### Task 3: Store users and tokens

**Files:** Create `internal/store/tokens.go`, `internal/store/users.go` and `internal/store/accounts_test.go`.

**Interfaces:**
- **`tokens.go`**
  - `func newSecret() (raw, hash string)`: 32 bytes from `crypto/rand`, base64url (RawURLEncoding), and hex SHA-256.
  - `func hashSecret(raw string) string`.
  - `type TokenKind string` with consts `TokenSetup = "setup"` and `TokenReset = "reset"`. `invite` and `verify` already exist in the schema and are added in later steps.
  - `func (s *Store) CreateToken(ctx, kind TokenKind, userID int64, ttl time.Duration, now time.Time) (raw string, err error)`:
    - deletes earlier unused tokens of the same kind for that user (for `setup`, every unused setup token)
    - inserts the new token
    - `userID` is 0 for `setup`
  - `func (s *Store) PeekToken(ctx, kind, raw string, now time.Time) (Token, error)` is a read-only check:
    - filters by kind and `used_at IS NULL AND expires_at > now`
    - for `reset`, also requires a user who is not disabled
    - returns `ErrNotFound` otherwise
  - `func consumeToken(ctx, q querier, kind TokenKind, raw string, now time.Time) (Token, error)` runs the one `UPDATE … WHERE token_hash=? AND kind=? AND used_at IS NULL AND expires_at>?`, which must affect exactly one row. Otherwise it returns `ErrNotFound`. The disabled-user rule applies to reset tokens here too.
  - `type Token struct { Kind TokenKind; UserID int64; CreatedAt, ExpiresAt time.Time }`.
  - `func (s *Store) PruneTokens(ctx, now time.Time) (int64, error)` deletes used or expired tokens older than 7 days.
- **`users.go`**
  - `type User struct { ID int64; Email string; IsOperator bool; VerifiedAt, DisabledAt *time.Time; CreatedAt time.Time }`.
  - `func NormalizeEmail(s string) (string, error)` follows §5.2:
    - trim, then `net/mail.ParseAddress`
    - the input must equal the parsed `.Address`
    - reject domain literals (`@[`) and domains without a `.`
    - at most 254 bytes
    - lowercase the result
    - the error is `ErrInvalidEmail` ("Enter a valid email address.")
  - `func (s *Store) CountUsers(ctx) (int, error)`.
  - `func (s *Store) UserByEmail(ctx, email string) (User, string, error)` returns the user and their PHC. `email` must already be normalized.
  - `func (s *Store) UserByID(ctx, id int64) (User, error)`.
  - `func (s *Store) IsUpgradedInstall(ctx) (bool, error)`: true when `CountUsers == 0` and workspace 1 has at least one row in `targets`, `settings` or `maintenance`.
  - `type SetupInput struct { Email, PasswordHash, WorkspaceName, SetupToken string; Now time.Time }`. An empty `SetupToken` means the password-unlock path, and the caller has already checked the password.
  - `func (s *Store) CompleteSetup(ctx, in SetupInput) (User, error)` runs in one transaction:
    1. Re-checks that no user exists (otherwise `ErrSetupDone`).
    2. Consumes the setup token when one is given (`ErrNotFound` if the consume fails).
    3. Inserts the user with `is_operator=1` and `verified_at=now`.
    4. Renames workspace 1 to `WorkspaceName`.
    5. Inserts the owner membership for workspace 1.
  - `type AccountInput struct { Email, PasswordHash, WorkspaceName, Slug string; Operator bool; Now time.Time }`.
  - `func (s *Store) CreateAccount(ctx, in AccountInput) (User, *WS, error)` creates a user, a new workspace via `createWorkspace`, and the owner membership, all in one transaction. It is used by tests now, and by invite sign-up in M1e.
  - `func (s *Store) SetPassword(ctx, userID int64, phc string, keepSessionHash string) error` runs in one transaction. It updates the hash, deletes every session of the user except `keepSessionHash` (empty means delete all), and deletes the user's unused `reset` tokens.
  - `func (s *Store) MarkVerified(ctx, userID int64, now time.Time) error` sets `verified_at` only if it is NULL.
  - `func (s *Store) UserWorkspace(ctx, userID int64) (*WS, Workspace, error)` returns the user's single owned workspace (v1 has one membership per user).
  - Errors: `var ErrSetupDone = errors.New("setup is already complete")`. Email uniqueness: inserting a duplicate returns `ErrEmailTaken`.

- [ ] **Step 1: Failing tests** (`accounts_test.go`):

```go
func TestTokensSingleUseKindAndExpiry(t *testing.T)
// The stored value is the hash: SELECT token_hash differs from raw, and len == 64.
// PeekToken works, and is rejected after expiry.
// A setup token is rejected as reset (ErrNotFound).
// Two goroutines consume the same token: exactly one succeeds.
// A new setup token deletes the previous unused one.
func TestCompleteSetup(t *testing.T)
// Token path:
//   creates an operator who is verified, renames ws1, adds an owner membership, consumes the token.
//   A second call returns ErrSetupDone.
//   Reusing the token returns ErrNotFound and no user is created.
// Two concurrent CompleteSetup calls with two different valid tokens: exactly one user exists afterwards.
func TestIsUpgradedInstall(t *testing.T) // fresh: false; after ws1.CreateTarget: true; after setup: false
func TestNormalizeEmail(t *testing.T)
// "  A@Example.COM " -> "a@example.com".
// Errors for: "Bob <a@b.c>", "a@[127.0.0.1]", "a@localhost", "", a 255-byte address.
func TestSetPasswordRevokes(t *testing.T) // keeps only keepSessionHash, deletes unused reset tokens (needs Task 4's sessions: skip that part here, assert tokens only)
func TestCreateAccountAndDuplicate(t *testing.T) // new user + workspace + membership; same email again -> ErrEmailTaken
```

- [ ] **Steps 2–4: Fail, implement, full suite.**
- [ ] **Step 5: Commit** `feat(store): users, setup and single-use tokens`.

---

### Task 4: Store sessions

**Files:** Create `internal/store/sessions.go`. Add tests to `accounts_test.go`.

**Interfaces:**
- `const SessionTTL = 30 * 24 * time.Hour`, `SessionIdle = 7 * 24 * time.Hour`, `sessionTouchEvery = 5 * time.Minute`, `MaxSessionsPerUser = 20`.
- `func (s *Store) CreateSession(ctx, userID int64, now time.Time) (raw string, err error)`. It inserts `{hash, user, now, now, now+TTL}`, then deletes the oldest of that user's sessions beyond 20.
- `type Auth struct { User User; Workspace Workspace; WS *WS; SessionHash string }`.
- `func (s *Store) SessionAuth(ctx, raw string, now time.Time) (Auth, error)`:
  - It is one query joining `sessions`, `users`, `memberships` and `workspaces`.
  - It returns `ErrNotFound` if the session is expired, has been idle for more than `SessionIdle`, the user is disabled, or the user is unverified.
  - When `now - last_seen_at >= sessionTouchEvery`, it updates `last_seen_at` on the session and the user.
- `func (s *Store) DeleteSession(ctx, raw string) error` hashes the token, then deletes. A missing session is not an error.
- `func (s *Store) DeleteUserSessions(ctx, userID int64) error`.
- `func (s *Store) PruneSessions(ctx, now time.Time) (int64, error)` deletes sessions that are expired or idle.
- Extend `SetPassword`'s session deletion test from Task 3.

- [ ] **Step 1: Failing tests.**

```go
func TestSessionLifecycle(t *testing.T)
// The stored hash is not equal to raw.
// SessionAuth returns the user, the workspace and a WS whose ID is the user's workspace.
// At now+SessionIdle+1s: ErrNotFound.
// A session touched at +6 days still works at +12 days, and fails at +30 days+1s (absolute expiry).
// After UPDATE users SET disabled_at: ErrNotFound. After verified_at=NULL: ErrNotFound.
func TestSessionCapAndRevoke(t *testing.T)
// 21 sessions: the oldest fails and the newest 20 work.
// DeleteUserSessions removes all of them.
// SetPassword with keep=h keeps only h.
// PruneSessions removes expired and idle sessions only.
```

- [ ] **Steps 2–4: Fail, implement, full suite.**
- [ ] **Step 5: Commit** `feat(store): database-backed sessions with idle and absolute expiry`.

---

### Task 5: Switch the web layer to accounts (the atomic switch)

This is the one task that changes authentication. It must land whole.

**Files:**
- Modify: `internal/web/session.go`, `routes.go`, `login.go`, `web.go` (Options, base), every handler file, `templates/login.html`, `templates/layout.html`, `internal/web/*_test.go`, `internal/config/config.go`, `main.go`.
- Delete: `internal/web/workspace.go`.

**Interfaces:**
- **`web.Options`** gains:
  - `Hasher *password.Hasher` (nil means `password.Default()`)
  - `SetupMode bool`
  - `Upgraded bool`
  - `AfterSetup func(ctx context.Context) error`
- **`session.go`:**
  - `type authCtx struct { User store.User; WS *store.WS; Workspace store.Workspace; IsOperator bool; sessionHash string }`.
  - `func (s *Server) currentAuth(r *http.Request) (*authCtx, bool)`. It reads the session cookie (both names), calls `s.sys.SessionAuth`, and caches the result in the request context so one request does one lookup.
  - `func (s *Server) cookieName(r) string` returns `__Host-uptime_session` when `s.isHTTPS(r) || s.cfg.HTTPSBase()`, otherwise `uptime_session`.
  - `func (s *Server) setSessionCookie(w, r, raw string)` and `func (s *Server) clearSessionCookie(w, r)`.
  - `func (s *Server) requireUser(h func(http.ResponseWriter, *http.Request, *authCtx)) http.HandlerFunc`. It responds 401 to API, `X-Partial` and SSE requests (same detection as today's `requireAdmin`). For everything else it redirects to `/login?next=` (GET only; non-GET uses `/`).
  - `func (s *Server) setupGate(next http.Handler) http.Handler` wraps the mux. It reads `s.setupMode` (`atomic.Bool`). In setup mode, only `GET /healthz`, `/static/*`, `GET|POST /setup`, `GET /s/{slug}` and `GET /status` pass. `/api/*` and `/events` get 503 "setup required". Everything else redirects 303 to `/setup`.
- **Delete:** the `sessions` type, `isAdmin`, `authEnabled`, `requireAdmin`, `setSession`, `clearSession`.
- **Routes** (`routes.go`):
  - Classes: `classPublic`, `classSetup`, `classGuest`, `classUser`, `classWorkspace`.
  - `type route struct { method, pattern string; class routeClass; handler http.HandlerFunc; authed func(http.ResponseWriter, *http.Request, *authCtx) }`. `user` and `workspace` routes set `authed`; the others set `handler`.
  - `routes()` wraps each class:
    - `setup`: 404 unless in setup mode.
    - `guest`: a signed-in user is redirected 303 to `/`.
    - `user` and `workspace`: `requireUser`.
    - Every POST is then wrapped in `sameOrigin`.
    - The handler is `setupGate(securityHeaders(mux))`.
  - Classes for existing routes:
    - `/login` GET and POST are `guest`.
    - `/logout` POST is `user`.
    - Every former `classAdmin` route becomes `workspace`.
    - `/setup` is added in Task 6, and `/account` and `/reset` in Task 8.
- **Handlers:**
  - Every former admin handler becomes `func (s *Server) X(w http.ResponseWriter, r *http.Request, a *authCtx)` and uses `a.WS` wherever it used `s.ws(r)`.
  - `checkDestination` and `saveNotify` use `s.cfg.AllowPrivate(a.IsOperator)`.
  - The `base` view model drops `AuthEnabled` and gains `UserEmail string`.
  - Public pages (login, setup, reset, the status page) build their base with `s.page(r, title)`, which has no workspace.
- **`login.go`:**
  - `loginPost` reads `email` and `password`.
  - It applies the per-address limiter. The full set of limits arrives in Task 7.
  - It normalizes the email. An invalid email follows the same path as an unknown one.
  - It calls `UserByEmail`, then `Verify` (or `VerifyDummy` when the user doesn't exist).
  - On failure it returns 401 with `msgBadLogin`.
  - On success it deletes the session named by the incoming cookie, calls `CreateSession`, sets the cookie, and redirects to `safeNext(next)`.
  - `ErrBusy` gives 503 with `msgBusy`.
  - Constants in `login.go`: `msgBadLogin = "Email or password is incorrect."`, `msgDisabled = "This account is disabled."`, `msgBusy = "The server is busy, please try again."`, `msgTooMany = "Too many attempts. Wait a minute and try again."`.
  - `logout` calls `DeleteSession`, clears the cookie, and redirects 303 to `/login`.
  - `safeNext`'s deny list gains `/setup` and `/reset`.
- **Templates:**
  - `login.html` has email and password fields: `autocomplete="username"` and `"current-password"`, title "Sign in".
  - `layout.html`'s footer shows `{{.UserEmail}}`, linking to `/account`, plus a "Sign out" POST form.
- **Config and main:**
  - Remove `CheckAuth` and the `InsecureNoAuth` refusal. If `insecure_no_auth` is set, `main` logs a WARN: "insecure_no_auth has no effect; sign-in is always required".
  - `main` computes `setup := CountUsers == 0` and `upgraded := IsUpgradedInstall` and passes both in `web.Options`.
  - In setup mode, `main` skips `seedTargets` and `seedNotify`. `AfterSetup` is a closure that seeds the operator's workspace (workspace 1) and calls `mon.Reload` for each seeded target.
  - Out of setup mode, if `admin_password` is set, log a WARN: "admin_password is ignored once an account exists".
  - `pruneLoop` also calls `PruneSessions` and `PruneTokens`.
- **Test env** (`web_test.go`), reworked:
  - `newEnv(t)` drops the `password` parameter.
    - It builds the server with `Hasher: password.New(password.Params{Memory: 64, Time: 1, Threads: 1})`.
    - It creates the operator `op@example.com` / `correct horse battery` via `CompleteSetup`, with a hash from that hasher and `SetupToken` from `CreateToken(TokenSetup)`.
    - It signs that client in with a real POST to `/login`.
    - The fields `e.op` (`*http.Client`) and `e.opWS` (`*store.WS`) are signed in.
    - `e.client(t)` stays and is anonymous.
  - `e.account(t, email string) (*http.Client, *store.WS)` uses `CreateAccount` with slug `strings.SplitN(email,"@",2)[0]` and signs in.
  - Update every existing test mechanically:
    - `newEnv(t, "pw")` and `newEnv(t, "")` become `newEnv(t)`.
    - Clients that logged in with the password use `e.op`.
    - Tests about the single admin password (`TestAuthFlow`, `TestLogoutRevokesSession`, `TestLoginLimiterNotBypassedBySpoofedXFF`, `TestPasswordEqual`) are rewritten for email sign-in, keeping each test's intent. `passwordEqual` stays and is used by setup in Task 6.

- [ ] **Step 1: Write the new tests first.**

```go
func TestSignInFlow(t *testing.T)
// An anonymous GET / gives 303 to /login?next=%2F.
// Anonymous /api/status gives 401.
// A wrong password gives 401 with a body containing msgBadLogin.
// An unknown email gives 401 with the same message.
// A correct login gives 303 and a session cookie named uptime_session (HttpOnly, SameSite=Lax).
// GET / gives 200 and the body contains "op@example.com".
// POST /logout gives 303 to /login, and the old cookie then gets 401 on /api/status.
func TestSignInReplacesIncomingSession(t *testing.T) // logging in again with an existing cookie deletes that session in the store and issues a new one
func TestHostCookieOverHTTPS(t *testing.T) // trust_proxy with X-Forwarded-Proto https: the cookie is __Host-uptime_session; Secure; Path=/
func TestSignedInUserRedirectedFromLogin(t *testing.T) // e.op GET /login gives 303 to /
```

- [ ] **Step 2: Run them to see them fail.**
- [ ] **Step 3: Implement the switch.** Then run `grep -rn 'isAdmin\|requireAdmin\|authEnabled\|AdminPassword\|pinnedWorkspace\|s\.ws(r)' internal/web --include=*.go | grep -v _test`. Expected: only `setup.go`'s planned `AdminPassword` use (none yet), so no output.
- [ ] **Step 4: Run the full suite.**
- [ ] **Step 5: Commit** `feat(web): email sign-in with database sessions; handlers take the signed-in workspace`.

---

### Task 6: First-run setup

**Files:**
- Create: `internal/web/setup.go`, `templates/setup.html`, and the setup tests in `internal/web/accounts_test.go`.
- Modify: `routes.go`, `main.go` (the setup token on start and `-setup-link`).

**Interfaces:**
- **Routes:** `GET /setup` and `POST /setup`, both `classSetup`.
- **Limiter:** `s.setupLimiter = ratelimit.New(15*time.Minute, 5, 0)`. It is keyed by `clientKey` and shared by the token path and the password path. It is checked first.
- **Logging:** every attempt is logged at WARN with `event=setup_attempt` and `ip`.
- **Shared headers:** both setup routes send `Cache-Control: no-store`.
- **`GET /setup`:**
  - With `?token=` and a token that passes `PeekToken(TokenSetup)`, it renders the form with the token in a hidden field.
  - Without a token, on an upgraded install with a non-empty `admin_password`, it renders the form with a "current admin password" field.
  - Otherwise it renders "Open the setup link printed in the server log." The status is 200, and the page contains no form.
  - The workspace name is prefilled with workspace 1's current name.
- **`POST /setup`:**
  1. The limiter runs first.
  2. Validate the fields: the email with `NormalizeEmail`, the password with `CheckLength`, and that both password fields match. The workspace name must be 1–60 characters with no Cc or Cf characters. Validation errors re-render the page with 422 through `renderStatus`, keeping the token.
  3. Authorise. The request needs a token that passes `PeekToken`, or `s.upgraded && cfg.AdminPassword != "" && passwordEqual(form, cfg.AdminPassword)`. Anything else gets 404.
  4. Hash the password.
  5. Call `CompleteSetup`. `ErrNotFound` or `ErrSetupDone` gets 404.
  6. On success:
     - Set `s.setupMode` to false.
     - Call `AfterSetup`. An error is logged, and setup still succeeds.
     - Log `event=setup_complete`.
     - Create a session and set the cookie.
     - Redirect 303 to `/`.
- **`main.go`:**
  - In setup mode on every start, call `CreateToken(TokenSetup, 0, 24h)` and log WARN `setup required: open <cfg.LinkBase()>/setup?token=<raw>`.
  - New flag `-setup-link`: open the store. In setup mode, create a token and print the link. Otherwise print "setup is already complete" to stderr and exit 1. It never starts the server.
- **Template:** `setup.html` builds on `public.html` with the `.login .card` styles. It has email, password, confirm password and workspace name fields, plus a hidden token or the admin password field.

- [ ] **Step 1: Failing tests.** In `accounts_test.go`, add a `newSetupEnv(t, upgraded bool, adminPassword string)` helper. It builds a server in setup mode with no users. When `upgraded` is true it first creates a ws1 target.

```go
func TestSetupGate(t *testing.T)
// In setup mode:
//   GET / -> 303 /setup; GET /api/status -> 503; GET /healthz -> 200; GET /s/default -> 200; GET /status -> 302 /s/default.
//   GET /login -> 303 /setup.
func TestSetupTokenFlow(t *testing.T)
// Create a token through the store. GET /setup?token=… shows the form, and the token is still unused.
// POST with a valid form -> 303 /, the session works, and ws1 is renamed.
// /setup afterwards -> 404.
// Re-POSTing the same token in a new env -> 404.
// POST with no token -> 404, and no user is created.
func TestSetupAdminPasswordOnlyOnUpgraded(t *testing.T)
// upgraded=false with admin_password set: GET /setup has no password field; POST with the password -> 404.
// upgraded=true: POST with the right password -> 303; with the wrong password -> 404.
func TestSetupLimiter(t *testing.T) // the 6th POST from one address within 15 minutes -> 429
func TestSetupValidation(t *testing.T) // mismatched passwords or a 9-character password -> 422; the token is not consumed (a correct retry succeeds)
func TestSetupConcurrent(t *testing.T) // two valid tokens POSTed concurrently -> exactly one user exists
```

- [ ] **Steps 2–4: Fail, implement, run the full suite.**
- [ ] **Step 5: Commit** `feat(web): protected first-run setup that adopts workspace 1`.

---

### Task 7: Sign-in rate limits, device cookie, enumeration and logging

**Files:** `internal/web/login.go`, `internal/web/clientip.go`, and tests in `accounts_test.go` and `web_test.go` (the `clientKey` test).

**Interfaces:**
- **`clientKey`:** IPv6 addresses are reduced to their /64 prefix (`netip.PrefixFrom(addr, 64).Masked()`), formatted as `"2001:db8::/64"`. IPv4 is unchanged.
- **Limiters**, with constants in `login.go`:
  - `loginByAddr = ratelimit.New(time.Minute, 5, 0)`: hard limit.
  - `loginByPair = ratelimit.New(15*time.Minute, 10, 0)`: key is `email + "|" + addr`, hard limit.
  - `loginByEmail = ratelimit.New(15*time.Minute, 20, 0)`: when it is exceeded, the email is "slowed".
  - `loginSlowGate = ratelimit.New(5*time.Second, 1, 0)`: key is the email, allows one attempt every 5 seconds while slowed.
  - All four use `Now = s.now`.
- **Checks, in order**, all before any hashing:
  1. addr
  2. pair
  3. the slow gate, applied only when the email is slowed and the browser has no valid device cookie for that user
- **Failure:** record a hit on addr, pair and email. A refusal re-renders the page with 429 and `msgTooMany`. The message never depends on whether the account exists.
- **Device cookie:**
  - Name `uptime_device`. Value is `<userID>.<hex HMAC-SHA256(key, "device|"+userID)>`, where the key is 32 random bytes generated once per process.
  - Set on a successful sign-in with `HttpOnly`, `SameSite=Lax` and `Max-Age` 90 days. `Secure` follows the session cookie rule.
  - It is "valid for that account" when the HMAC matches the user ID that `UserByEmail` found. That check happens only after the lookup and only decides the slowdown. The order stays enumeration-safe because the slowdown check uses the email key alone; it does not depend on whether the user exists.
- **Disabled users:** only after a correct password, return 403 with `msgDisabled`.
- **Logging** (§12):
  - `event=login_ok` with `user_id` and `ip`
  - `event=login_failed` with `ip`, plus `user_id` when known, otherwise `email_hash` (the first 16 hex characters of the SHA-256 of the normalized email)
  - `event=login_throttled` with `ip`
  - Passwords are never logged.

- [ ] **Step 1: Failing tests.**

```go
func TestLoginPerAddressLimit(t *testing.T) // the 6th failure from one address within a minute -> 429; another address still gets 401
func TestLoginPerPairLimit(t *testing.T) // 10 failures for (a@x, addr1), spread over addresses so the per-address limit isn't hit (use X-Forwarded-For with trust_proxy); the 11th for that pair -> 429
func TestLoginPerEmailSlowdown(t *testing.T)
// 21 failures for one email from many addresses: the next attempt within 5s -> 429.
// After advancing the clock 5s, one attempt is allowed and the correct password succeeds (never locked out).
// A browser holding a valid device cookie for that user skips the slowdown at once.
func TestLoginNoEnumeration(t *testing.T)
// Real hasher: password.Default().
// Wrong password for an existing user vs an unknown email: identical status and identical body bytes.
// Medians of 30 requests each differ by less than 50ms.
// Run with t.Parallel() off. Skip under -short.
// Each pair uses a fresh address so the limits don't interfere: trust_proxy with a unique X-Forwarded-For per request.
func TestDisabledOnlyAfterCorrectPassword(t *testing.T) // disabled user + wrong password -> 401 msgBadLogin; correct password -> 403 msgDisabled
func TestHashBusyGives503(t *testing.T) // inject a Hasher whose run slots are all held (test hook from Task 1, same as TestBusy) -> 503 msgBusy
func TestClientKeyIPv6Grouping(t *testing.T) // "2001:db8::1" and "2001:db8::ffff" -> the same key; "2001:db8:0:1::1" -> a different key; "203.0.113.9" unchanged
```

- [ ] **Steps 2–4: Fail, implement, run the full suite.**
- [ ] **Step 5: Commit** `feat(web): sign-in limits that never lock users out, device cookie, security log events`.

---

### Task 8: Account page, password reset and `-reset-password`

**Files:**
- Create `internal/web/account.go`, `internal/web/reset.go`, `templates/account.html` and `templates/reset.html`.
- Modify `routes.go` and `main.go`.

**Interfaces:**
- **Routes:**
  - `GET /account` (`user`)
  - `POST /account/password` (`user`)
  - `POST /account/signout-all` (`user`)
  - `GET /reset` and `POST /reset` (`guest`)
- **Change password** (`POST /account/password`):
  - Fields `current`, `new` and `confirm`.
  - Validate `CheckLength` and that the two new values match, then verify `current`. A wrong current password gives 422 "Current password is incorrect."
  - Hash, then `SetPassword(user, phc, keepSessionHash=a.sessionHash)`.
  - Log `event=password_changed`.
  - Redirect 303 to `/account` with flash "Password changed. Other sessions were signed out."
- **Sign out everywhere** (`POST /account/signout-all`):
  - `DeleteUserSessions`, then clear the cookie.
  - Log `event=sessions_revoked`.
  - Redirect 303 to `/login` with flash "Signed out everywhere." The `/login` page shows signed flashes.
- **`GET /reset?token=`:**
  - Send `Cache-Control: no-store`.
  - With a valid token from `PeekToken(TokenReset)`, show the new-password form with the token in a hidden field.
  - Otherwise show "This link is invalid or has expired." with status 200 and no form.
  - The GET never changes state.
- **`POST /reset`:**
  - Validate the input, then hash the new password.
  - In one store call, `ResetPassword(ctx, raw, phc, now) (User, error)`:
    - consume the token
    - set the password
    - delete all of the user's sessions and their unused reset tokens
    - set `verified_at` if it is NULL
  - Add `ResetPassword` to `users.go`, sharing the inner helper with `SetPassword`.
  - Then create a session, set the cookie, log `event=password_reset`, and redirect 303 to `/`.
  - An invalid token gives 404.
- **`main.go`:** new flag `-reset-password <email>`. It opens the store, finds the user with a normalized email, creates a reset token with a 1h TTL, and prints `<cfg.LinkBase()>/reset?token=<raw>`. It exits 1 with "no such user" if there is no match.
- **Templates:**
  - `account.html` builds on `layout.html`. It shows the email and two forms. "Sign out everywhere" uses the existing `data-confirm` dialog.
  - `reset.html` builds on `public.html`.

- [ ] **Step 1: Failing tests.**

```go
func TestChangePassword(t *testing.T) // wrong current -> 422; success -> other client's session revoked, this one still works, new password signs in
func TestSignOutEverywhere(t *testing.T) // two clients for op; signout-all from one -> both get 401 on /api/status
func TestResetFlow(t *testing.T)
// CreateToken(TokenReset, op.ID, time.Hour); GET /reset?token -> form, token still unused (PeekToken ok);
// POST -> 303 /, all prior sessions revoked, new session works, old password fails, token reused -> 404
func TestResetGetNeverConsumes(t *testing.T) // GET twice then POST succeeds
```

- [ ] **Steps 2–4: Fail, implement, run the full suite.**
- [ ] **Step 5: Commit** `feat(web): account page, password reset and -reset-password`.

---

### Task 9: Session-aware live updates, API limit, status owner view

**Files:** `internal/web/events.go`, `api.go`, `status.go`, `routes.go` (the API limiter in the workspace class), tests

**Interfaces:**
- **`/events`:**
  - A per-user stream counter caps each user at 5 concurrent streams. The 6th gets 429.
  - Every 60s (`eventsRecheck`, a package var so tests can shorten it), the stream calls `SessionAuth` again with the cookie value. It closes the stream when that returns an error.
  - The stream forwards only events where `ev.WorkspaceID == a.WS.ID()` and the ID is not 0.
- **API limit:** `apiLimiter = ratelimit.New(time.Minute, 120, 0)`, keyed by `a.sessionHash`. It applies to `workspace`-class routes whose path starts with `/api/`. When exceeded, it returns JSON 429 `{"error":"too many requests"}` with `Retry-After`.
- **Status page owner view:** the page shows the owner view when `currentAuth` succeeds and `a.Workspace.ID == info.ID`. This replaces `isAdmin`. Anyone else gets the cached anonymous page.

- [ ] **Step 1: Failing tests.**

```go
func TestEventsClosesOnRevoke(t *testing.T) // eventsRecheck=50ms; open /events as op; DeleteUserSessions; the stream ends within 1s
func TestEventsStreamCap(t *testing.T) // 5 open streams for op; the 6th -> 429
func TestAPILimitPerSession(t *testing.T) // 121 GET /api/status -> the last is 429 JSON with Retry-After; a new session is not limited
func TestStatusOwnerViewOnlyForOwner(t *testing.T) // op sees the settings form on /s/default; a second account sees the anonymous page there
```

- [ ] **Steps 2–4: Fail, implement, run the full suite.**
- [ ] **Step 5: Commit** `feat(web): live updates re-check sessions and cap streams; API rate limit; owner-only status settings`.

---

### Task 10: Isolation suite and static checks

**Files:** Create `internal/web/isolation_test.go` and `internal/web/static_test.go`.

**Interfaces:**
- **`TestIsolationSuite`** sets up two workspaces, A (`e.opWS`) and B (`e.account(t, "b@example.com")`). A gets a target with checks, an incident, a maintenance window, a stored notify setting with a Slack URL `https://hooks.example/A-secret`, and status settings.
  - **Every workspace route.** For each `classWorkspace` route in `routeTable()`, B's client sends a request that targets A:
    - `{id}` path segments are replaced with A's matching ID: the target, incident or maintenance ID, according to the route prefix.
    - GET routes also get `?target=<A target id>` where the handler reads it.
    - POST routes get a plausible form (`name=x`, `url=https://example.com`, `note=x`, `title=x`, `duration=1h`).
    - Expected result: a 404, or a 2xx/3xx whose body has none of A's markers: A's target name `A-target-zz`, its maintenance title `A-maint-zz`, and the strings `A-secret` and `hooks.example`.
    - After all requests, a snapshot of A's rows taken through `e.opWS` must equal the snapshot taken before: targets, incidents (ack and note), maintenance and notify.
  - **Aggregates.** B's `/api/status`, `/api/incidents` and `/api/maintenance` responses, and the overview counts, must be identical whether or not A has data. Create B and B's data first, and capture those responses before any of A's data exists. Then add A's data and capture again. The env clock is fixed, so the responses must be byte-identical.
  - **Live updates.** B's `/events` receives nothing while A's target is checked (`e.mon.CheckAsync`) and A's maintenance is created.
  - **Status pages.**
    - `/s/default` (A's) shows only A's monitors.
    - Warm the cache for `/s/default` while signed in as B, with B having a down monitor and active maintenance. The anonymous page must contain nothing of B's.
- **Coverage.** `TestEveryWorkspaceRouteHasIsolationCase`: the suite records the routes it exercised, and this test fails if any `classWorkspace` route is missing.
- **`TestSysAllowList`** in `static_test.go` parses the `internal/web` non-test files. It fails if `s.sys.` appears outside this allow-list:
  - `session.go`, `setup.go`, `login.go`, `account.go`, `reset.go`
  - `web.go` (healthz Ping)
  - `status.go` (slug resolver)
  - `events.go` (session re-check)
- **`TestEveryRouteClassified`** checks that every route has a valid class, that `authed` is set exactly for user and workspace classes, and that the count matches the table.

- [ ] **Step 1: Write the tests.** They should pass against Tasks 5–9. If one fails, it has found a leak: fix the handler, and report what was found.
- [ ] **Step 2: Prove the suite bites.** Temporarily make `incidentAck` call `s.sys.Workspace(1)` instead of `a.WS`. Both `TestIsolationSuite` and `TestSysAllowList` must fail. Revert, and record both outputs in the report.
- [ ] **Step 3: Run the full suite. Commit** `test(web): cross-account isolation suite and static checks`.

---

### Task 11: Docs, config example, decisions

**Files:** `README.md`, `config.example.json`, `docs/decisions.md`

- **README:**
  - **Accounts.** Sign in with email and password. On first run, open the setup link printed in the server log, or run `uptime-monitor -setup-link`.
  - **Upgrading to accounts.** Setup adopts all existing data. `admin_password` unlocks setup only on an upgraded install and is ignored afterwards. `insecure_no_auth` is removed. LAN monitoring keeps working for the operator under `allow_private_targets: operator`.
  - **Config table.** Add `base_url` and `signup` (only `closed` for now), and update the `admin_password` row.
  - **Flags.** Add `-setup-link` and `-reset-password <email>`.
  - **Sessions.** Sessions are stored in the database: 30 days absolute, 7 days idle, at most 20 per user. Revoked on password change or reset, and by "sign out everywhere".
  - **Rate-limit constants** for sign-in, setup and the API.
  - **Notes.** Replace the in-memory sessions note.
- **`config.example.json`:** add `"base_url": ""` and `"signup": "closed"`. Remove nothing else.
- **`docs/decisions.md` D-124:** scope rulings 1–5 above, in the D-12x format.
- [ ] **Commit** `docs: accounts, upgrading to accounts, D-124`.

---

### Task 12: Step verification

- [ ] **Step 1: Run the CI checks.** gofmt, vet, `go test -race`, govulncheck and the Docker build. govulncheck must report no new findings from `golang.org/x/crypto`.
- [ ] **Step 2: Smoke-test the upgrade (M1c to M1d).**
  - Run the M1c binary with a LAN target and `admin_password` set.
  - Stop it with `pkill -TERM -x`, using the exact binary name. Copy the database and its `-wal` file.
  - Run the M1d binary on the copy. Expect all of the following:
    - The log prints a setup link at WARN.
    - `/` redirects to `/setup`.
    - The LAN target is still being monitored during setup mode.
    - `/status` returns 302 to `/s/default`.
    - Setup through the admin password works and adopts the target.
    - Signing out and back in works.
    - `-setup-link` afterwards fails with "setup is already complete".
- [ ] **Step 3: Smoke-test a fresh install.** On an empty database:
  - The setup link works.
  - Config targets appear only after setup.
- [ ] **Step 4: Browser check under the CSP with `playwright-cli`** (`mise exec node@24.21.0 -- playwright-cli` from the scratchpad).
  - Pages: setup, sign-in, dashboard, account (change password, sign out everywhere), and reset via a `-reset-password` link.
  - Confirm there are no console or CSP errors, and that the sidebar footer shows the email.
- [ ] **Step 5: Opus whole-branch review** of `accounts-m1c..accounts-m1d`.
