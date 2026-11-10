# Accounts M1a: Building Blocks Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reshape the store and web packages into the structure the accounts work builds on, with no change in behaviour.

**Architecture:** First, two pure moves: `store.go` and `web/auth.go` are split into one file per area. Then the store gets numbered migrations on `PRAGMA user_version`, with today's schema as version 1. The login limiter becomes a reusable `internal/ratelimit` package. The web layer gets `web.Options`, a generic `loadByID`, a `renderStatus` helper, and a route table that decides middleware by class.

**Execution:** subagent-driven. Implementers are Sonnet subagents, one per task; each task's reviewer and the final whole-branch review are Opus. Reuse existing helpers (`clientKey`, `sameOrigin`, `s.fail`, `renderBytes`, `writeHTML`, `newEnv`, `cookieJar`) and write no code a task doesn't need.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (pure Go), `net/http` ServeMux patterns, `html/template`, standard `testing` + `httptest`.

**Spec:** [docs/superpowers/specs/2026-11-10-accounts-design.md](../specs/2026-11-10-accounts-design.md). This plan covers step **M1a** (§2), using §3.1, §3.2, §4.1 and §8.1.

## Global Constraints

- **No behaviour change.** Every existing test passes unmodified. The one exception is `TestRollupBackfillAndPermissions`, which spec §4.1 says must set `PRAGMA user_version = 0` before reopening.
- No new dependencies (`golang.org/x/crypto` arrives in M1d). `go.mod` stays unchanged.
- The first commit of M1a is a pure move of `store.go` (§3.1).
- The store uses one SQLite connection (`SetMaxOpenConns(1)`). Code that runs inside a transaction uses the `querier` it is given, never `s.db`, or it deadlocks (§3.2.4).
- Tests live next to the code and use real SQLite in `t.TempDir()` and `httptest` (§3).
- Every commit passes `gofmt -l .` (empty output), `go vet ./...` and `go test -race -count=1 ./...`.
- Commit messages follow the repo's conventional style: `refactor(store): …`, `feat(ratelimit): …`.
- Out of scope for M1a (later steps): IPv6 /64 grouping in `clientKey` (M1d, since it changes behaviour), the `migrate` `path` parameter and pre-migration backup (M1c, used only by step 2), route classes other than today's two (M1d), `-backup` changes (none).

## Review Focus

1. **Opening a database written by a newer build** (a downgrade after M1c ships). Expected: `Open` fails with a clear "newer than this build supports" error and does not touch the schema. Test: Task 2, `TestMigrateRefusesNewerSchema`.
2. **A pre-versioning database** (`user_version = 0`, original first-release schema, no added columns, no `daily` table, existing rows). Expected: it upgrades to version 1 with all rows intact and rollups backfilled. Test: Task 2, `TestMigrateLegacyDatabase`.
3. **A migration step that fails partway through.** Expected: none of that step's changes remain, `user_version` is unchanged, and the next `Open` retries it. Test: Task 2, `TestMigrateStepIsAtomic`.
4. **A flood of distinct limiter keys** (rotating client addresses). Expected: memory stays bounded at `maxKeys`, the least recently used key is evicted first, and `Allow` on an unknown key stores nothing. Test: Task 4, `TestKeyCapEvictsLeastRecentlyUsed` and `TestAllowDoesNotStoreKeys`.
5. **A route added without the right middleware.** Expected: every admin-class route refuses an anonymous request (401, or a 303 redirect to `/login`), and every POST route rejects a cross-site `Origin` with 403. Test: Task 7, `TestRouteTableDefaultDeny`.

---

## File Structure

**`internal/store`** (after Tasks 1–2)

| File | Contents |
|---|---|
| `store.go` | package doc, `ErrNotFound`, `Store`, `Open`, `Close`, `Ping`, `Backup`, `restrictPerms`, `scanner`, `requireRow`, `boolToInt`, `querier` (Task 2), `dayMS` |
| `migrations.go` | `schema`, `dailySchema`, `steps`, `migrate`, `v1`, `hasColumn` |
| `targets.go` | `Target`, `scanTarget`, the target methods (store.go "targets" section) |
| `checks.go` | `Check`, `Stats`, `UptimePercent`, `Percentile`, the "checks" section including `Prune`, `PruneRollups` |
| `incidents.go` | `Incident` and its methods, `IncidentFilter`, the "incidents" section |
| `alerts.go` | `Alert`, the "alerts" section |
| `maintenance.go` | `Maintenance` and its methods, the "maintenance" section |
| `settings.go` | the "settings" section |

**`internal/ratelimit`** (Task 4): `ratelimit.go`, `ratelimit_test.go`.

**`internal/web`** (after Tasks 3–7)

| File | Contents |
|---|---|
| `middleware.go` | `sameOrigin`, `parseForm`, `maxBodyBytes`, `cspBase`, `securityHeaders` |
| `clientip.go` | `clientKey`, `parseAddr` |
| `session.go` | `sessionCookie`, `sessionTTL`, `sessions` and its methods, `isAdmin`, `authEnabled`, `requireAdmin`, `setSession`, `clearSession`, `isHTTPS` |
| `login.go` | `loginData`, `loginPage`, `loginPost`, `logout`, `safeNext`, `passwordEqual`, `loginMaxFailures` |
| `events.go` | `events` (the SSE handler, moved from `api.go`) |
| `routes.go` | `routeClass`, `route`, `routeTable`, `routes` (Task 7) |
| `web.go` | `Options`, `New`, `Handler`, `healthz`, assets, view models |
| `helpers.go` | gains `loadByID` and `renderStatus` (Task 6) |
| `auth.go` | deleted |

---

### Task 1: Split `store.go` by area (pure move)

**Files:**
- Modify: `internal/store/store.go` (shrinks to the core listed above)
- Create: `internal/store/migrations.go`, `targets.go`, `checks.go`, `incidents.go`, `alerts.go`, `maintenance.go`, `settings.go`

**Interfaces:**
- Consumes: nothing.
- Produces: the same package API, now in the files listed under File Structure. Task 2 edits `migrations.go`.

- [ ] **Step 1: Record the declaration set before moving**

Run: `grep -h -E '^(func|type|var|const) ' internal/store/store.go | sort > /tmp/claude-1000/-home-dr--claude-spares-spare-znfqec/fdaf7b40-5bf9-40a7-8b4d-f0f62128d5f3/scratchpad/store-decls-before.txt && wc -l /tmp/claude-1000/-home-dr--claude-spares-spare-znfqec/fdaf7b40-5bf9-40a7-8b4d-f0f62128d5f3/scratchpad/store-decls-before.txt`
Expected: a non-zero line count.

- [ ] **Step 2: Move each section verbatim into its file**

Cut along the existing `// ---- <area> ----` markers and move each type next to its methods, as in the File Structure table. Each new file starts with `package store` and imports only what it uses. Change no lines of code, only their location.

- [ ] **Step 3: Verify the declaration set is unchanged**

Run: `ls internal/store/*.go | grep -v _test.go | xargs grep -h -E '^(func|type|var|const) ' | sort | diff /tmp/claude-1000/-home-dr--claude-spares-spare-znfqec/fdaf7b40-5bf9-40a7-8b4d-f0f62128d5f3/scratchpad/store-decls-before.txt - && echo SAME`
Expected: `SAME`.

- [ ] **Step 4: Run the checks**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: no gofmt output; all packages `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "refactor(store): split store.go into files by area (pure move)"
```

---

### Task 2: Numbered migrations on `user_version`, and `querier`

**Files:**
- Modify: `internal/store/migrations.go`, `internal/store/store.go` (`Open`, add `querier`)
- Modify: `internal/store/store_test.go` (`TestRollupBackfillAndPermissions`: one added line)
- Create: `internal/store/migrations_test.go`

**Interfaces:**
- Consumes: Task 1 file layout.
- Produces:
  - `type querier interface { ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error); QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error); QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row }`, satisfied by `*sql.DB` and `*sql.Tx`, with compile-time assertions `var _ querier = (*sql.DB)(nil)` and `var _ querier = (*sql.Tx)(nil)`.
  - `var steps = []func(context.Context, *sql.Tx) error{v1}` (M1c appends `v2`).
  - `func migrate(ctx context.Context, db *sql.DB) error`. Spec §3.1 writes `migrate(db, path)`. The `path` parameter exists only for the step-2 backup, so M1c adds it together with `v2`.
  - `func hasColumn(ctx context.Context, q querier, table, col string) (bool, error)`.

- [ ] **Step 1: Write the failing tests in `migrations_test.go`**

Helper used by the tests: `userVersion(t, s *Store) int`, which runs `PRAGMA user_version` on `s.db`.

```go
func TestMigrateFreshDatabase(t *testing.T) {
	// Open a new file: user_version is len(steps) (1), and table daily exists.
	// Close it, DROP TABLE daily through a second raw sql.Open, then Open again:
	// daily is still missing, which proves step 1 does not run again at version 1.
}

func TestMigrateLegacyDatabase(t *testing.T) {
	// With a raw sql.Open on a new file, create the first-release schema: the
	// `schema` constant WITHOUT targets.grp/muted, incidents.acked_at/note and
	// checks.maint, with no daily table and user_version left at 0. Insert one
	// target and two checks (one ok with latency 50, one failed, on different days).
	// Open() must succeed with: userVersion == 1; hasColumn true for all five
	// added columns; GetTarget returns the row; RollupStats over 7 days gives
	// Total 2, OKCount 1.
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	// Open, then set PRAGMA user_version = 99 and close. Open again must
	// return an error whose text contains "newer than this build supports".
	// user_version must still read 99.
}

func TestMigrateStepIsAtomic(t *testing.T) {
	// Open (version 1), then close. Temporarily append a step that runs
	// CREATE TABLE half_done(x) and then returns errors.New("boom"), and
	// restore steps with t.Cleanup. Open must fail with an error containing "boom".
	// A raw connection must show user_version 1 and no half_done table. After
	// steps is restored, Open succeeds.
}
```

Add the one required line to `TestRollupBackfillAndPermissions` right after `DROP TABLE daily`:
`s.db.Exec("PRAGMA user_version = 0")` (check the error).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store -run 'TestMigrate' -v`
Expected: FAIL. `userVersion` is 0 on a fresh database, and `TestMigrateRefusesNewerSchema` gets no error.

- [ ] **Step 3: Implement `querier`, `v1`, `migrate`; call `migrate(context.Background(), db)` from `Open`**

- `Open` no longer runs `db.Exec(schema)` itself; `v1` does it.
- `v1(ctx, tx)` runs, in order and all through `tx`:
  1. `schema` (already `IF NOT EXISTS`)
  2. the five column additions using `hasColumn(ctx, tx, …)`
  3. if `sqlite_master` has no `daily` table, the two `slog.Info` messages around `dailySchema`, exactly as today
- `migrate`:
  1. read `PRAGMA user_version`
  2. if it is greater than `len(steps)`, return `fmt.Errorf("database schema version %d is newer than this build supports (%d); use a newer uptime-monitor", cur, len(steps))`
  3. for each step numbered `v` above `cur`: `BeginTx`, run the step, `Exec(fmt.Sprintf("PRAGMA user_version = %d", v))` inside the same transaction, `Commit`; on error roll back and return `fmt.Errorf("migration %d: %w", v, err)`

- [ ] **Step 4: Run the store tests to verify they pass**

Run: `go test -race -count=1 ./internal/store -v 2>&1 | tail -20`
Expected: every test PASS, including the four new ones and `TestRollupBackfillAndPermissions`.

- [ ] **Step 5: Run the full suite and commit**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: no gofmt output; all `ok`.

```bash
git add internal/store
git commit -m "feat(store): numbered migrations on user_version, querier for transactions"
```

---

### Task 3: Split `web/auth.go` and move the SSE handler (pure move)

**Files:**
- Delete: `internal/web/auth.go`
- Create: `internal/web/middleware.go`, `clientip.go`, `session.go`, `login.go`, `events.go`
- Modify: `internal/web/api.go` (the `events` handler and its "live events" section leave)

**Interfaces:**
- Consumes: nothing.
- Produces: the same package API, in the files listed under File Structure.

- [ ] **Step 1: Record the declaration set**

Run: `cat internal/web/auth.go internal/web/api.go | grep -E '^(func|type|var|const) ' | sort > /tmp/claude-1000/-home-dr--claude-spares-spare-znfqec/fdaf7b40-5bf9-40a7-8b4d-f0f62128d5f3/scratchpad/web-decls-before.txt`

- [ ] **Step 2: Move the code verbatim** into the five files, following the File Structure table. Each file imports only what it uses. Change no code.

- [ ] **Step 3: Verify the declarations are unchanged**

Run: `cat internal/web/{middleware,clientip,session,login,events,api}.go | grep -E '^(func|type|var|const) ' | sort | diff /tmp/claude-1000/-home-dr--claude-spares-spare-znfqec/fdaf7b40-5bf9-40a7-8b4d-f0f62128d5f3/scratchpad/web-decls-before.txt - && echo SAME`
Expected: `SAME`.

- [ ] **Step 4: Run the checks**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: no gofmt output; all `ok`.

- [ ] **Step 5: Commit**

```bash
git add -A internal/web
git commit -m "refactor(web): split auth.go by area and move the SSE handler to events.go (pure move)"
```

---

### Task 4: `internal/ratelimit`, replacing `loginLimiter`

**Files:**
- Create: `internal/ratelimit/ratelimit.go`, `internal/ratelimit/ratelimit_test.go`
- Modify: `internal/web/login.go` (delete `loginLimiter`, `newLoginLimiter`, `blocked`, `fail`, `prune`; keep `loginMaxFailures = 5`), `internal/web/web.go` (`Server.limiter` type and construction)

**Interfaces:**
- Consumes: nothing.
- Produces (package `ratelimit`; safe for concurrent use):
  - `type Limiter struct { Now func() time.Time /* nil means time.Now; set only before first use */ ; … }`
  - `func New(window time.Duration, max, maxKeys int) *Limiter`. A `maxKeys` of 0 or less means `DefaultMaxKeys`.
  - `const DefaultMaxKeys = 100_000`
  - `func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration)`: `ok` is true when fewer than `max` events for `key` fall in `(now-window, now]`. When not ok, `retryAfter` is the time until the oldest counted event leaves the window (always > 0). `Allow` never stores a new key.
  - `func (l *Limiter) Hit(key string)` records one event at `now`.
  - `func (l *Limiter) Reset(key string)` forgets the key.
- Web uses it as `s.limiter = ratelimit.New(time.Minute, loginMaxFailures, 0)` with `s.limiter.Now = s.now`. In `loginPost`, `blocked(key)` becomes `!ok` from `Allow(key)`, and `fail(key)` becomes `Hit(key)`. Responses, messages and status codes are unchanged.

Data structure: `map[string]*list.Element` plus a `container/list` ordered by last use (`Hit` and `Allow` on an existing key both move it to the front). Each entry keeps at most `max` timestamps, the most recent ones. That is enough to decide `Allow`, and it bounds memory per key. When `Hit` adds a key while the map is full, evict the back of the list.

- [ ] **Step 1: Write the failing tests** in `ratelimit_test.go` (package `ratelimit`, with a fake clock `now := time.Unix(0,0)`, `l.Now = func() time.Time { return now }`)

```go
func TestWindow(t *testing.T) {
	// New(time.Minute, 3, 0). Allow("a") ok; 3× Hit("a") at 0s, 10s, 20s
	// (advance now between hits). At 30s: Allow -> ok false, retryAfter 30s.
	// At 59.999s still blocked; at exactly 60s ok (the 0s event is out of
	// (now-window, now]); at 70s: Hit, then Allow -> false, retryAfter 10s.
}

func TestKeysAreIndependentAndReset(t *testing.T) {
	// max 1: Hit("a") blocks a but not b; Reset("a") unblocks a.
}

func TestAllowDoesNotStoreKeys(t *testing.T) {
	// 1000× Allow(fmt.Sprint(i)) -> len(l.keys) == 0 (internal map).
}

func TestKeyCapEvictsLeastRecentlyUsed(t *testing.T) {
	// New(time.Minute, 1, 2). Hit a, Hit b, Allow a (touches a), Hit c:
	// len(l.keys) == 2; b was evicted (Allow("b") ok), a still blocked, c blocked.
}

func TestPerKeyMemoryBounded(t *testing.T) {
	// max 3: 100× Hit("a") -> stored timestamps for "a" == 3.
}

func TestConcurrentUse(t *testing.T) {
	// 50 goroutines × 100 Hit/Allow on 10 keys with the real clock; run with -race.
}

func TestDefaultMaxKeys(t *testing.T) {
	// New(time.Minute, 1, 0).maxKeys == DefaultMaxKeys.
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/ratelimit -v`
Expected: FAIL (package has no `New`).

- [ ] **Step 3: Implement `ratelimit.go`** to the interface above, with a package doc comment saying that state is in memory and resets on restart (§8.1).

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=1 ./internal/ratelimit -v`
Expected: all PASS.

- [ ] **Step 5: Switch `web` to the new limiter** as described under Interfaces.

- [ ] **Step 6: Run the full suite**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: all `ok`. `TestAuthFlow`, `TestLogoutRevokesSession` and `TestLoginLimiterNotBypassedBySpoofedXFF` pass unchanged.

- [ ] **Step 7: Commit**

```bash
git add internal/ratelimit internal/web
git commit -m "feat(ratelimit): extract the sliding-window limiter from the login handler"
```

---

### Task 5: `web.Options`

**Files:**
- Modify: `internal/web/web.go` (`New`), `main.go` (the `web.New` call), `internal/web/web_test.go:48`, `internal/web/hardening_test.go:301`

**Interfaces:**
- Consumes: nothing.
- Produces:

```go
// Options configure a Server.
type Options struct {
	Config  *config.Config
	Store   *store.Store
	Monitor *monitor.Monitor
	Logger  *slog.Logger     // nil means slog.Default(), as in monitor.New
	Now     func() time.Time // nil means time.Now
}
func New(o Options) (*Server, error)
```

`New` returns an error when `Config`, `Store` or `Monitor` is nil (`"web: Options.Config, Store and Monitor are required"`).

- [ ] **Step 1: Write the failing test** in `web_test.go`

```go
func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New must reject missing Config, Store and Monitor")
	}
}
```

- [ ] **Step 2: Run it to verify it fails to compile**

Run: `go test ./internal/web -run TestNewRequiresDependencies`
Expected: FAIL, `undefined: Options`.

- [ ] **Step 3: Implement `Options` and the new `New`; update the three call sites** to the struct form.

- [ ] **Step 4: Run the full suite**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: all `ok`.

- [ ] **Step 5: Commit**

```bash
git add main.go internal/web
git commit -m "refactor(web): web.Options replaces positional New parameters"
```

---

### Task 6: Generic `loadByID` and `renderStatus`

**Files:**
- Modify: `internal/web/helpers.go` (add both), `targets.go` (`loadTarget` body; 422 sites at the current lines ~181 and ~344), `incidents.go` (delete `loadIncident`), `maintenance.go` (delete `loadMaintenance`; 422 site ~126), `alerts.go` (422 site ~135), `login.go` (401 and 429 sites)
- Test: `internal/web/web_test.go`

**Interfaces:**
- Consumes: `s.fail`, `s.renderBytes`, `writeHTML` (existing).
- Produces:
  - `func loadByID[T any](s *Server, w http.ResponseWriter, r *http.Request, get func(context.Context, int64) (T, error)) (T, bool)`. It parses `r.PathValue("id")` as int64. A parse error or `store.ErrNotFound` gives `http.NotFound`; any other error goes to `s.fail`. Both return the zero `T` and false.
  - `loadTarget` stays and keeps its signature `(store.Target, monitor.State, bool)`. It calls `loadByID(s, w, r, s.store.GetTarget)`, then adds the monitor state as today.
  - Call sites of `loadIncident` and `loadMaintenance` become `loadByID(s, w, r, s.store.GetIncident)` and `loadByID(s, w, r, s.store.GetMaintenance)`.
  - `func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, code int, page string, data any)`. It renders to bytes first. On a template error it calls `s.fail`, which gives a 500, never a header-written 4xx with a broken body. Otherwise it sets the content type, calls `WriteHeader(code)` and writes the body. It replaces every `w.WriteHeader(4xx)` followed by `s.render(...)` pair.

- [ ] **Step 1: Write the failing test**

```go
func TestLoadByID(t *testing.T) {
	e := newEnv(t, "")
	s, _ := New(Options{Config: e.cfg, Store: e.st, Monitor: e.mon})
	boom := errors.New("boom")
	get := func(_ context.Context, id int64) (string, error) {
		switch id {
		case 1: return "one", nil
		case 2: return "", boom
		}
		return "", store.ErrNotFound
	}
	for _, tc := range []struct{ id string; code int; want string; ok bool }{
		{"1", 200, "one", true}, {"abc", 404, "", false}, {"7", 404, "", false}, {"2", 500, "", false},
	} {
		r := httptest.NewRequest("GET", "/x/"+tc.id, nil)
		r.SetPathValue("id", tc.id)
		w := httptest.NewRecorder()
		got, ok := loadByID(s, w, r, get)
		// ok == tc.ok, got == tc.want, and when !ok, w.Code == tc.code
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/web -run TestLoadByID`
Expected: FAIL, `undefined: loadByID`.

- [ ] **Step 3: Implement `loadByID` and `renderStatus`; replace the copies and the 4xx render pairs.**

- [ ] **Step 4: Verify that no duplicates remain**

Run: `grep -nE 'func \(s \*Server\) load(Incident|Maintenance)|WriteHeader\(http\.Status(Unprocessable|Unauthorized|TooMany)' internal/web/*.go | grep -v _test`
Expected: no output.

- [ ] **Step 5: Run the full suite**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: all `ok`. The existing 422, 401 and 429 assertions in `web_test.go` pass unchanged.

- [ ] **Step 6: Commit**

```bash
git add internal/web
git commit -m "refactor(web): one loadByID for 404 handling and renderStatus for error pages"
```

---

### Task 7: Routes as a table with default-deny classes

**Files:**
- Create: `internal/web/routes.go`, `internal/web/routes_test.go`
- Modify: `internal/web/web.go` (delete `routes()`; `New` calls the new one)

**Interfaces:**
- Consumes: `requireAdmin`, `sameOrigin`, and every existing handler.
- Produces:

```go
type routeClass int

const (
	classPublic routeClass = iota + 1 // no auth; the zero value is invalid on purpose
	classAdmin                        // requireAdmin; M1d replaces it with user/workspace/operator
)

type route struct {
	method  string // "GET" or "POST"
	pattern string // ServeMux pattern without the method, e.g. "/targets/{id}"
	class   routeClass
	handler http.HandlerFunc
}

func (s *Server) routeTable() []route // the single source of truth
func (s *Server) routes()             // builds s.mux from routeTable()
```

`routes()` wraps each handler by class: `classPublic` gets nothing, `classAdmin` gets `s.requireAdmin`. Every `POST` is then wrapped in `sameOrigin` on the outside, matching today's `sameOrigin(s.requireAdmin(h))`. Any other class value panics with `"web: route %s %s has no class"`. The static file server enters the table as a `classPublic` `GET /static/` route, with handler `http.StripPrefix(...).ServeHTTP`. The table holds exactly the 32 routes registered today, with the same methods, patterns and auth.

- [ ] **Step 1: Write the failing tests** in `routes_test.go`

```go
func TestRouteTableIsComplete(t *testing.T) {
	// For newEnv(t, "pw")'s server: every route has class classPublic or classAdmin,
	// method GET or POST, a non-nil handler; no duplicate method+pattern; len == 32.
	// classPublic set is exactly: GET /static/, GET /healthz, GET /status,
	// GET /login, POST /login, POST /logout.
}

func TestRouteTableDefaultDeny(t *testing.T) {
	// newEnv(t, "pw"), anonymous client, no redirects followed. For every
	// classAdmin route, substitute {id} with 1 and "/{$}" with "/" and send method+path (POSTs with an empty form):
	// status is 401, or 303 with Location starting "/login". Never 2xx.
	// For every POST route: with header Origin: https://evil.example -> 403.
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/web -run TestRouteTable`
Expected: FAIL, `undefined: routeTable`.

- [ ] **Step 3: Implement `routes.go`** and delete the old `routes()` from `web.go`.

- [ ] **Step 4: Run the full suite**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: all `ok`, including `TestSecurityHeaders`, `TestSafeNextAndCSRF` and `TestRequestBodyLimit` unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/web
git commit -m "refactor(web): declare routes as a table whose class decides the middleware"
```

---

### Task 8: Step verification

**Files:** none changed, unless a check fails.

- [ ] **Step 1: Run the CI checks locally**

Run: `test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./... && go run golang.org/x/vuln/cmd/govulncheck@latest ./...`
Expected: all pass. govulncheck reports only the standard-library findings already recorded as SEC-3 for the local Go 1.27.1 toolchain, and none in module code. If it reports anything new, stop and report it.

- [ ] **Step 2: Run the Docker build**

Run: `docker build --build-arg VERSION=m1a -t uptime-monitor:m1a .`
Expected: the build succeeds.

- [ ] **Step 3: Smoke test an upgraded database**

Copy a database created by the pre-M1a binary (`git stash`, or `go run` at `2167908` with a temp config) and start the M1a binary against the copy. Expected: it starts, `/healthz` returns `ok`, `sqlite3 <db> 'PRAGMA user_version'` (or a short Go snippet) prints `1`, and targets and incidents are all present.

- [ ] **Step 4: Request an independent code review** of the M1a branch diff, per spec §13 "For every step".
