# Accounts M1c: Workspaces and Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every monitor, incident, maintenance window, alert setting and status page belong to a workspace. The step has four parts:
- a schema version 2 that moves existing data into workspace 1, with a backup taken first
- a scoped store, so web code can only reach one workspace's rows
- a monitor engine that knows about workspaces
- a public status page per workspace at `/s/<slug>`

**Architecture:**
- **Migration.** Schema v2 adds the accounts tables (§4.2) and rebuilds `targets`, `maintenance` and `settings` with a `workspace_id` column (§4.3). It always creates workspace 1, `{Default, default}`, and every existing row moves into it. On a database that already holds data, a 0600 backup is written first.
- **Scoped store.** `store.WS` is a workspace-bound view whose queries always filter by workspace. `*store.Store` keeps only system and engine methods.
- **Monitor engine.** It keys notifiers and the internal-address guard by workspace and tags its events with the workspace.
- **Web layer.** Until accounts arrive in M1d, the web layer is pinned to workspace 1 through one helper.
- **Status pages.** They move to `/s/{slug}`, and `/status` redirects there. The page cache is keyed per slug, bounded in memory, rate limited, and runs at most two rebuilds at once.

**Execution:** subagent-driven. Implementers are Sonnet subagents; reviewers are Opus. Reuse what exists: `querier`, `requireRow`, `scanTarget` and friends, `loadByID`, `renderStatus`, `pageCache`, `ratelimit`, `clientKey`, `newEnv`. Write no code that a task doesn't need.

**Tech Stack:** Go 1.26, `modernc.org/sqlite`, standard library. No new dependencies.

**Spec:** [docs/superpowers/specs/2026-11-10-accounts-design.md](../specs/2026-11-10-accounts-design.md), step **M1c** (§2). Sections used: §3.2.4, §4, §6.1–§6.6, §8.2 (status page row), and §13 (migration tests, aggregates).

## Global Constraints

- **Branch.** Work on `accounts-m1c`, cut from `accounts-m1b`. Nothing is merged or pushed.
- **Connections.** The store uses one SQLite connection. Code inside a transaction uses the given `querier`/`*sql.Tx`, never `s.db` (§3.2.4).
- **Migrations.** Each step runs in its own transaction. `PRAGMA foreign_keys` can't change inside a transaction, so it is switched OFF on the pinned connection before `BeginTx` and back ON after the commit or rollback. `PRAGMA foreign_key_check` must return no rows before the commit (§4.3).
- **Workspace filter.** Every `WS` query filters on its workspace unconditionally. A row in another workspace returns `store.ErrNotFound`. Tables without a `workspace_id` column use the one shared fragment `inWS = "target_id IN (SELECT id FROM targets WHERE workspace_id = ?)"` (§6.1).
- **Alert channels.** A workspace's channels come only from its own stored `notify` setting. At runtime there is no fallback to the config file's `notify` (§6.6).
- **Existing installs keep working.** Every target, check, incident, alert, maintenance window and setting survives into workspace 1. LAN monitoring keeps working under the default `allow_private_targets: operator`, because workspace 1 is the operator's.
- **Gates.** No new dependencies. `gofmt -l .` prints nothing, and `go vet ./...` and `go test -race -count=1 ./...` pass at every commit. Commit messages follow the conventional style.
- **Later-step config.** Keys from later steps (`signup`, `base_url`, `mail`, `limits`) are not added here.

## Scope rulings (departures from the spec, recorded as D-123 in Task 6)

1. **Workspace 1 always exists.** Schema v2 creates workspace 1 `{name: "Default", slug: "default"}` on every database, fresh or upgraded (§4.4 says fresh installs get none). Before M1d there is no setup page, so a fresh install would otherwise have no workspace at all. In M1d, setup adopts workspace 1 instead of creating one. The pre-migration backup still happens only when data exists.
2. **The operator workspace is workspace 1** until M1d adds users. Monitors and notifiers decide internal-address access with `cfg.AllowPrivate(wsID == 1)`.
3. **Moved to M1e, where they are first used:**
   - slug derivation and editing (`DeriveSlug`, `SetSlug`, `Rename`)
   - `CountTargets`
   - `monitor.RemoveWorkspace` and `store.DeleteWorkspace`
   - Check-now coalescing
4. **Moved to M1d,** together with the static allow-list test (§13): renaming `Server.store` to `sys` and the allow-list itself. The field is renamed here, but nothing enforces the allow-list yet.

## Review Focus

1. **Upgrading a real v1 database.** It must keep every row, place them in workspace 1, carry `sqlite_sequence` forward (so deleted IDs are never reused), write the backup at 0600, and pass `foreign_key_check`. Test: Task 1 `TestMigrateV2Upgrade`.
2. **A crash or failure partway through v2.** It must leave the database at v1, unchanged, with foreign keys back ON. Test: Task 1 `TestMigrateV2FailureRollsBack`.
3. **Cross-workspace access through any store method.** This includes aggregates where target ID 0 means "all": `Stats`, `RollupStats`, `CountIncidents`, `CountAlerts`, `LastAlerts`'s `MAX(id)`, and `InMaintenance`. Test: Task 2 `TestWSIsolation`.
4. **Live updates.** An event from another workspace, or with workspace 0, must never reach an `/events` stream. Test: Task 4 `TestEventsFilteredByWorkspace`.
5. **Status-page cache under load or many slugs.** Memory stays within the limit, one slug's change doesn't clear others, unknown slugs create no entries, and a client over 120 requests a minute gets 429. Test: Task 5 `TestStatusCacheBounded` and `TestStatusRateLimited`.

---

## File Structure

| File | Change |
|---|---|
| `internal/store/migrations.go` | `migration` type, `migrate(ctx, db, path)`, pinned connection, `foreignKeysOff` handling, `v2`, `hasData`, pre-migration backup |
| `internal/store/workspace.go` | new: `Workspace`, `WS`, `(*Store).Workspace`, `WorkspaceBySlug`, `createWorkspace` (unexported), `inWS` |
| `internal/store/targets.go`, `checks.go`, `incidents.go`, `alerts.go`, `maintenance.go`, `settings.go` | workspace-owned methods move to `*WS`. `*Store` keeps the engine methods (Task 2), and the unscoped copies are deleted in Task 6 |
| `internal/store/workspace_test.go` | new: isolation tests |
| `internal/monitor/monitor.go` | per-workspace notifiers and guard, scoped methods, `Event.WorkspaceID` |
| `internal/web/workspace.go` | new: `pinnedWorkspace`, `(*Server).ws(r)` |
| `internal/web/*.go` | handlers use `s.ws(r)`, and `Server.store` becomes `sys` |
| `internal/web/status.go` | `/s/{slug}`, `/status` redirect, per-slug LRU cache, rebuild cap, rate limit |
| `main.go` | seeds into workspace 1; `Notifiers` and `PrivateAllowed` funcs |
| `README.md`, `docs/decisions.md` | status page URL, the backup, D-123 |

---

### Task 1: Schema v2 and the pre-migration backup

**Files:**
- Modify: `internal/store/migrations.go`, `internal/store/store.go` (`Open` passes `path`), `internal/store/migrations_test.go`, and the unscoped insert methods in `targets.go`, `maintenance.go` and `settings.go` (see below)
- Test: `internal/store/migrations_test.go`

**Interfaces:**
- Consumes: today's `steps`, `runStep`, `v1`, `Backup`.
- Produces:
  - `type migration struct { run func(context.Context, *sql.Tx) error; foreignKeysOff bool }`
  - `var steps = []migration{{run: v1}, {run: v2, foreignKeysOff: true}}`
  - `func migrate(ctx context.Context, db *sql.DB, path string) error`. `Open` passes the database path. When `cur < 2`, `path != ""` and `hasData` is true, it first writes `Backup(ctx, path, path+".pre-accounts-"+time.Now().Format("20060102-150405")+".bak")`. If the backup fails, the migration fails.
  - `func hasData(ctx context.Context, q querier) (bool, error)` is true if any row exists in `targets`, `settings` or `maintenance`. Missing tables count as empty.
  - `runStep` pins a connection with `db.Conn(ctx)` and closes it afterwards. When `foreignKeysOff` is set, it runs `PRAGMA foreign_keys = OFF` on that connection before `BeginTx`, and `PRAGMA foreign_keys = ON` after commit or rollback, in a defer that always runs. Inside the transaction, after `run` and before setting `user_version`, it runs `PRAGMA foreign_key_check` and fails with `migration %d: foreign key check failed` if any row comes back.
  - `v2(ctx, tx)`:
    1. Creates `users`, `workspaces`, `memberships`, `sessions` and `tokens`, plus their indexes, exactly as in spec §4.2.
    2. Inserts workspace 1 `('Default', 'default', <now ms>)`.
    3. Rebuilds `targets`, `maintenance` and `settings` to the §4.3 definitions. For each table: create `<t>_new`, `INSERT INTO <t>_new SELECT … , 1 AS workspace_id FROM <t>`, drop `<t>`, rename, recreate indexes (`UNIQUE (workspace_id, name)` on targets is part of the table definition, plus `idx_maintenance_ws(workspace_id, starts_at)`). For `targets` and `maintenance`, read the old `sqlite_sequence.seq` before the drop and afterwards set it to `max(old seq, max(id))`, so deleted IDs are never reused.
    4. The new `settings` is `(workspace_id, key, value, PRIMARY KEY (workspace_id, key))`.
  - **Temporary bridge, removed in Task 6.** Today's unscoped inserts and queries keep compiling and passing by targeting workspace 1. Add `const legacyWorkspaceID = 1` in `migrations.go`:
    - `CreateTarget` and `CreateMaintenance` insert `workspace_id = legacyWorkspaceID`.
    - `GetSetting` and `SetSetting` use `workspace_id = legacyWorkspaceID`.
    - No other method changes.
  - In `migrations_test.go`, update the existing tests that append to `steps` to the new element type. Keep their assertions unchanged.

- [ ] **Step 1: Write the failing tests** in `migrations_test.go`:

```go
func TestMigrateV2Upgrade(t *testing.T) {
	// Build a v1 database:
	//   Open a temp path with `steps` temporarily set to steps[:1] (restore with t.Cleanup).
	//   Insert targets "A" and "B", checks for both, an incident and an alert on A,
	//   a maintenance window and settings("notify", `{}`). Delete target B.
	//   Close, then restore the full `steps`.
	// Open again (this runs v2). Then:
	//   - user_version == 2
	//   - workspaces holds exactly one row: {1, "Default", "default"}
	//   - targets has workspace_id = 1 for A
	//   - maintenance and settings rows have workspace_id 1, and GetSetting("notify") == `{}`
	//   - checks, incidents and alerts row counts are unchanged
	//   - a new target gets an ID greater than B's old ID (sqlite_sequence carried)
	//   - PRAGMA foreign_key_check returns nothing; PRAGMA foreign_keys == 1
	//   - exactly one file matches path+".pre-accounts-*.bak", with mode 0600
	// Open a third time: still exactly one backup file, user_version still 2.
}

func TestMigrateV2Fresh(t *testing.T) {
	// A new path: user_version == 2, workspace 1 exists, no .bak file anywhere in the dir.
}

func TestMigrateV2FailureRollsBack(t *testing.T) {
	// Build a v1 database with one target, as above.
	// Temporarily replace steps[1].run with a function that calls v2 and then
	// returns errors.New("boom"). Open fails with an error containing "boom".
	// A raw connection then shows: user_version == 1; no workspaces table;
	// the targets table has no workspace_id column (use hasColumn); the target row
	// is still there; PRAGMA foreign_keys returns 1 on a new store Open with the
	// real steps, and that Open succeeds (v2 now applies).
}

func TestTwoWorkspacesSameTargetName(t *testing.T) {
	// After Open, insert workspace 2 directly with SQL. Then:
	// INSERT INTO targets (workspace_id, name, …) twice with name "X", once per
	// workspace: both succeed. A second "X" in workspace 1 fails with a UNIQUE error.
}
```

- [ ] **Step 2: Run them and confirm they fail.**
Run: `go test ./internal/store -run 'TestMigrateV2|TestTwoWorkspaces'`
Expected: FAIL (`user_version` is 1, and `workspaces` doesn't exist).

- [ ] **Step 3: Implement** what Interfaces describes.

- [ ] **Step 4: Run the whole suite.**
Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: gofmt prints nothing and every package is `ok`. The bridge keeps the existing tests in other packages passing unmodified.

- [ ] **Step 5: Commit**
```bash
git add internal/store
git commit -m "feat(store): schema v2 with workspaces and accounts tables, pre-migration backup"
```

---

### Task 2: The scoped store `store.WS` and engine methods

**Files:**
- Create: `internal/store/workspace.go`, `internal/store/workspace_test.go`
- Modify: `internal/store/targets.go`, `checks.go`, `incidents.go`, `alerts.go`, `maintenance.go`, `settings.go`

**Interfaces:**
- Consumes: the Task 1 schema.
- Produces (the old unscoped methods stay until Task 6, so callers keep compiling):
  - `store.Target` gains `WorkspaceID int64`. `scanTarget` and every target `SELECT` read it.
  - `type Workspace struct { ID int64; Name, Slug string; CreatedAt time.Time }`
  - `func (s *Store) Workspace(id int64) *WS` panics with `"store: invalid workspace id"` when `id <= 0`.
  - `func (s *Store) WorkspaceBySlug(ctx context.Context, slug string) (Workspace, error)` returns `ErrNotFound` when no workspace has that slug.
  - `func createWorkspace(ctx context.Context, q querier, name, slug string, now time.Time) (Workspace, error)`. It is unexported, so tests can use it now and M1d can call it from `createAccount`.
  - `type WS struct { db *sql.DB; id int64 }`.
  - The WS methods have the same signatures as today's `*Store` methods, but every one is scoped to the workspace:
    - Workspace and targets:
      - `ID() int64`
      - `Info(ctx) (Workspace, error)`
      - `ListTargets`, `GetTarget`, `GetTargetByName`
      - `CreateTarget(ctx, t, now) (Target, error)` sets `t.WorkspaceID = w.id` and ignores whatever the caller passed
      - `UpdateTarget(ctx, t) (Target, error)` filters on `id AND workspace_id` and returns the stored row. This changes the return type, and callers adapt in Task 4.
      - `SetPaused`, `SetMuted`, `DeleteTarget`
    - Checks: `RecentChecks`, `ChecksSince`, `Stats`, `DailyUptime`, `RollupStats`, `Latencies`, `HourlyUptime`
    - Incidents: `GetIncident`, `ListIncidents`, `AcknowledgeIncident`, `SetIncidentNote`, `CountUnacknowledged`, `CountIncidents`
    - Alerts: `LastAlerts`, `CountAlerts`
    - Maintenance: `CreateMaintenance`, `GetMaintenance`, `EndMaintenance`, `DeleteMaintenance`, `ListMaintenance`, `ActiveMaintenance`, `InMaintenance`
    - Settings: `GetSetting`, `SetSetting`, and `Notify(ctx) (config.Notify, error)`. `Notify` decodes the `notify` setting. If none is stored it returns the zero `config.Notify` and nil.
  - How the filtering works:
    - Wherever a method takes a target ID, `0` means "all targets in this workspace". Any other ID is added as a filter on top of the workspace filter.
    - `LastAlerts` applies the workspace filter inside its `MAX(id)` subquery.
    - `DeleteTarget` deletes the target's checks, incidents, alerts and daily rows only after confirming that the target belongs to this workspace. Otherwise it returns `ErrNotFound`.
  - Engine methods stay on `*Store`, all keyed by target ID:
    - `AllTargets(ctx) ([]Target, error)`: every workspace, including `WorkspaceID`
    - `TargetByID(ctx, id) (Target, error)`
    - `InsertCheck`, `LatestCheck`, `OngoingIncident`, `OpenIncident`, `CloseIncident`, `InsertAlert`, `Prune`, `PruneRollups`: these keep their current signatures
  - Share code. Each `WS` method should call the same query helper that the old method used, or move the old body into the `WS` method and have the old method delegate as `s.Workspace(legacyWorkspaceID).X(...)`. Never keep two copies of one SQL statement.
  - **Ruling.** `WS.UpdateTarget` returns `(Target, error)`, as §6.1 says ("return the stored target"). The old `(*Store).UpdateTarget` keeps returning `error` until Task 6.

- [ ] **Step 1: Write the failing tests** in `workspace_test.go`:

```go
func TestWSIsolation(t *testing.T) {
	// Setup: s := Open; a := s.Workspace(1); ws2 created with createWorkspace(ctx, s.db, "B", "b", now); b := s.Workspace(ws2.ID).
	// Each workspace gets: a target, two checks (one ok, one failed), an incident
	// with an alert, a maintenance window (group ""), and a notify setting.
	// Assert for b, using a's IDs:
	//   - GetTarget, GetIncident and GetMaintenance return ErrNotFound
	//   - UpdateTarget, SetPaused, SetMuted, DeleteTarget, AcknowledgeIncident,
	//     SetIncidentNote, EndMaintenance and DeleteMaintenance return ErrNotFound
	//     and leave a's rows unchanged (re-read them through a)
	//   - RecentChecks, ChecksSince and Latencies return empty for a's target
	// Aggregates: b.Stats(ctx, 0, since), b.RollupStats(ctx, 0, since), b.CountIncidents,
	//   b.CountAlerts, b.CountUnacknowledged, b.LastAlerts and b.ListMaintenance count only b's rows.
	//   Assert equality with the values recorded before a's rows were added.
	// b.InMaintenance(ctx, "", now): true only for b's own window. With b's window
	//   deleted, it is false even though a's window is active.
	// b.Notify returns b's channels, never a's.
	// b.CreateTarget(Target{WorkspaceID: 1, Name: "x", …}) stores WorkspaceID == ws2.ID.
}

func TestWorkspaceLookups(t *testing.T) {
	// WorkspaceBySlug(ctx, "default") gives ID 1, Name "Default"; "nope" gives ErrNotFound.
	// Workspace(0) panics.
	// AllTargets returns targets from both workspaces with the correct WorkspaceID.
	// TargetByID works across workspaces.
}
```

- [ ] **Step 2: Run them and confirm they fail.**
Run: `go test ./internal/store -run 'TestWS|TestWorkspace'`
Expected: FAIL (undefined: `WS`).

- [ ] **Step 3: Implement** what Interfaces describes.

- [ ] **Step 4: Run the whole suite.**
Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: everything `ok`.

- [ ] **Step 5: Commit**
```bash
git add internal/store
git commit -m "feat(store): workspace-scoped store WS and engine methods"
```

---

### Task 3: Make the monitor engine workspace-aware

**Files:**
- Modify: `internal/monitor/monitor.go`, `internal/monitor/monitor_test.go`, `main.go`
- Create: `internal/web/workspace.go`
- Modify: the web call sites of monitor methods, using `s.ws(r).ID()`

**Interfaces:**
- Consumes: from Task 2, `AllTargets`, `TargetByID`, `Target.WorkspaceID` and `(*Store).Workspace(id).InMaintenance`.
- Produces:
  - `monitor.Options`:
    - Remove `AllowPrivate bool` and `Notifiers notify.Multi`.
    - Add `Notifiers func(ctx context.Context, workspaceID int64) (notify.Multi, error)` and `PrivateAllowed func(workspaceID int64) bool`. A nil `PrivateAllowed` guards every workspace (fails closed).
    - With `Client` nil, `New` builds two clients the same way as today: a guarded one with `netguard.Client(false, 0)` and an unguarded one with `netguard.Client(true, 0)`. Both get today's transport adjustments, including HTTP/1.1 only and `NextProtos = nil`. Put the adjustment in one small helper. When `Client` is set (tests), both are that client.
  - Each target is probed with `clientFor(t.WorkspaceID)`.
  - **Notifiers.** These are cached per workspace in a map. On first use they load through `Options.Notifiers`. If loading fails, the error is logged and that workspace falls back to `notify.Multi{{Name: "log", Notifier: notify.Log{Logger: logger}}}` for that call; a later call tries again.
    - `SetNotifiers(ws int64, n notify.Multi)` replaces one entry.
    - `Notifiers(ctx, ws) notify.Multi`.
    - `alert` uses the notifiers of the target's workspace.
  - **Scoped methods:**
    - `Snapshot(ws int64) []State`
    - `Get(ws, id int64) (State, bool)`, which returns false for another workspace's target
    - `SendTest(ctx, ws int64, channel string) error`
    - `Test(ctx, ws int64, t store.Target) store.Check`, which probes with `clientFor(ws)`
    - `CheckAsync(ws, id int64)`, which does nothing for another workspace's target
  - **`Reload(ctx, targetID int64) error`.** It ignores any caller copy and re-reads the target with `TargetByID`. `ErrNotFound` means the target is gone, so it removes it and returns nil.
  - **`Event` gains `WorkspaceID int64` with tag `json:"-"`.** Every event the monitor publishes carries the target's workspace.
  - **Maintenance lookups** use `m.store.Workspace(t.WorkspaceID).InMaintenance`.
  - **`main.go`** sets:
    - `Notifiers: func(ctx, ws) { n, err := st.Workspace(ws).Notify(ctx); …; return notify.FromConfig(n, logger, cfg.AllowPrivate(ws == 1)), nil }`
    - `PrivateAllowed: func(ws int64) bool { return cfg.AllowPrivate(ws == 1) }`
    - `loadNotifiers` goes away. Its seeding role (copying `cfg.Notify` into workspace 1 when nothing is stored) becomes `seedNotify(ctx, st.Workspace(1), cfg)`.
    - `seedTargets` takes `*store.WS` and uses `ws.GetTargetByName` and `ws.CreateTarget`.
  - **Web.** `internal/web/workspace.go` defines:
    - `const pinnedWorkspace int64 = 1`
    - `func (s *Server) ws(r *http.Request) *store.WS { return s.store.Workspace(pinnedWorkspace) }`, with a doc comment saying M1d replaces it with the signed-in user's workspace
    - Every monitor call in web passes `s.ws(r).ID()`. `saveNotify` calls `s.mon.SetNotifiers(ws.ID(), notify.FromConfig(n, s.logger, s.cfg.AllowPrivate(ws.ID() == pinnedWorkspace)))`.
    - Other store calls stay unscoped until Task 4.
  - **Tests.** `newEnv` passes `PrivateAllowed: func(int64) bool { return true }` and a `Notifiers` func that returns the log channel. Monitor tests change to the new `Options` fields; this is a mechanical edit.

- [ ] **Step 1: Write the failing tests** in `monitor_test.go`:

```go
func TestMonitorScopedByWorkspace(t *testing.T) {
	// A store with workspace 1 and a second workspace (insert it with SQL in the test,
	// or use a store test helper if one is exported for tests; otherwise use raw SQL
	// through a second sql.Open, as other tests do), one target in each, both pointing
	// at an httptest server. Start the monitor.
	// Assertions:
	//   - Snapshot(1) has only ws1's target; Snapshot(2) has only ws2's.
	//   - Get(2, ws1TargetID) gives ok == false.
	//   - CheckAsync(2, ws1TargetID) records no new check for ws1's target (count checks before and after).
	//   - Every Event received from m.Events while both targets get checked carries the WorkspaceID of its target.
}

func TestNotifiersPerWorkspace(t *testing.T) {
	// Options.Notifiers returns a recording notifier named "ws<N>" for workspace N.
	// Make ws2's target go down: only "ws2" records the alert.
	// SetNotifiers(2, other) replaces only ws2's channel.
	// If Options.Notifiers returns an error for ws 3, alerts for ws3 go to the log
	// channel (no panic). The function is called again on the next alert.
}

func TestPrivateAllowedPerWorkspace(t *testing.T) {
	// No Options.Client; PrivateAllowed returns true only for ws 1. httptest server on 127.0.0.1.
	// Test(ctx, 1, target) → OK; Test(ctx, 2, target) → Error == "blocked: destination is an internal address".
}
```

- [ ] **Step 2: Run them and confirm they fail.**
Run: `go test ./internal/monitor -run 'Scoped|PerWorkspace'`
Expected: FAIL (compile errors on the new `Options` fields and method signatures).

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run the whole suite.**
Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: everything `ok`.

- [ ] **Step 5: Commit**
```bash
git add internal/monitor internal/web main.go
git commit -m "feat(monitor): per-workspace notifiers, guard, events and scoped lookups"
```

---

### Task 4: Move the web layer onto the scoped store

**Files:**
- Modify: every `internal/web/*.go` that calls `s.store.<workspace-owned method>`, plus `internal/web/web_test.go` and `hardening_test.go` (mechanical renames only)

**Interfaces:**
- Consumes: `(*Server).ws(r)` from Task 3; `WS` from Task 2.
- Produces:
  - `Server.store` is renamed to `Server.sys`. Every workspace data call goes through `ws := s.ws(r)`.
  - `s.sys` stays only for:
    - `Ping` in `healthz`
    - `WorkspaceBySlug` (Task 5)
    - `Workspace(...)` inside `s.ws`
  - `view`, `views`, `uptime`, `targetData`, `maintenanceData`, `statusData` and the base helper take `ws *store.WS` explicitly. Don't re-derive it inside.
  - `loadByID` callers pass `ws.GetTarget`, `ws.GetIncident` and `ws.GetMaintenance`.
  - **Alerts.**
    - `notifyConfig(ctx, ws)` returns `ws.Notify(ctx)`. The fallback to `s.cfg.Notify` is removed.
    - `saveNotify` writes through `ws.SetSetting`.
    - With nothing stored, the Alerts page shows no channels, even when `cfg.Notify` is set.
  - **Live updates.**
    - `events` forwards an event only when `ev.WorkspaceID == s.ws(r).ID()`. It drops `WorkspaceID == 0`.
    - Maintenance events the web publishes carry `WorkspaceID: ws.ID()`.
  - **Engine reloads.** Target create, update, pause and delete call `s.mon.Reload(ctx, id)` (re-reads) and `s.mon.Remove(id)`.

- [ ] **Step 1: Write the failing tests** in `web_test.go`:

```go
func TestEventsFilteredByWorkspace(t *testing.T) {
	// Open /events with an SSE reader (reuse how existing tests read events, or read the
	// body line by line with a deadline).
	// e.mon.Events.Publish: Event{WorkspaceID: 2, Kind: "check"}, then {WorkspaceID: 0, Kind: "check"},
	// then {WorkspaceID: 1, Kind: "marker"}.
	// The first event received is "marker"; the two before it never arrive.
}

func TestAlertsNoConfigFallback(t *testing.T) {
	// e.cfg.Notify = config.Notify{Slack: &config.Slack{WebhookURL: "https://hooks.example/x"}}, nothing stored.
	// GET /alerts: the body does not contain "hooks.example", and the Slack channel shows as not configured
	// (assert whatever marker the alerts template uses for an unconfigured channel).
}
```

- [ ] **Step 2: Run them and confirm they fail.**
Run: `go test ./internal/web -run 'TestEventsFilteredByWorkspace|TestAlertsNoConfigFallback'`
Expected: FAIL (events leak, and the alerts page falls back to config).

- [ ] **Step 3: Implement.** Afterwards, check that nothing workspace-owned is still reached unscoped:
Run: `grep -n 's\.sys\.' internal/web/*.go | grep -v _test`
Expected: only `Ping`, `Workspace(` and `WorkspaceBySlug`.

- [ ] **Step 4: Run the whole suite.**
Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: everything `ok`.

- [ ] **Step 5: Commit**
```bash
git add internal/web
git commit -m "feat(web): handlers use the workspace-scoped store; live updates and alerts per workspace"
```

---

### Task 5: Status pages at `/s/{slug}`

**Files:**
- Modify: `internal/web/status.go`, `routes.go`, `middleware.go` (framing), `web.go`/`helpers.go` (the base gets `StatusURL`), `templates/layout.html` (the status link), `routes_test.go` (the route count and public set change), `hardening_test.go` (the status tests use `/s/default`)
- Test: `internal/web/hardening_test.go`

**Interfaces:**
- Consumes: `WorkspaceBySlug`, `WS.Info`, `ratelimit.New`, `clientKey`, today's `pageCache`.
- Produces:
  - **Routes.**
    - `GET /s/{slug}` is `classPublic`.
    - `GET /status` stays `classPublic` and now redirects with a 302 to `/s/<slug of workspace pinnedWorkspace>`.
    - `POST /status/settings` stays admin.
    - The route table grows to 33. Update `TestRouteTableIsComplete`'s count and public set (a mechanical edit).
  - **`statusPage` for `/s/{slug}`.** It resolves the slug with `s.sys.WorkspaceBySlug`. An unknown slug returns 404 without touching the cache.
    - If `s.isAdmin(r)` and the workspace is `s.ws(r)`, it renders the live owner view with the settings form. Framing is denied, as today.
    - Everyone else gets the cached anonymous page.
  - **`statusData(ctx, ws *store.WS, theme string, partial bool)`.** It is built with no `*http.Request`. Every count and banner comes from `ws`. `ShareURL` is computed by the owner-view caller from the request scheme and host, and it never enters the cached anonymous page.
  - **Cache.**
    - The key is `slug + "|" + theme`, plus `"|partial"` when `partial` is true.
    - `reset(slug string)` invalidates only that slug's entries. Every call site that changes a workspace's status data passes `ws`'s slug. Those are: target create, update, pause, delete and mute; maintenance changes; and status settings.
    - The cache is bounded to `statusCacheMaxBytes = 32 << 20` total body bytes. When adding an entry pushes the total over the limit, the least recently used entries are evicted, and an entry that is currently rebuilding is never evicted.
    - At most `statusMaxRebuilds = 2` builds run at once across all keys. A caller that can't get a rebuild slot gets the stale copy if one exists. Otherwise it waits for a slot, respecting `ctx`.
  - **Rate limit.** `s.statusLimiter = ratelimit.New(time.Minute, 120, 0)`, keyed by `clientKey(r, s.cfg.ProxyHops())`, and it applies to anonymous `/s/{slug}` requests. When it's exceeded, the response is 429 with `Retry-After` set to the whole seconds of `retryAfter`, rounded up, and the body is the plain text `too many requests`.
  - **Framing.** `securityHeaders` allows `frame-ancestors *` for anonymous `GET` and `HEAD` on paths starting with `/s/`, instead of `/status`. The owner view still sets deny, as today.
  - **Links.** The base view gets `StatusURL string` (`/s/<slug>` of `s.ws(r)`). `layout.html`'s "Status page" link uses it.

- [ ] **Step 1: Write the failing tests.** Change the existing status tests to `/s/default` (a mechanical path change), then add:

```go
func TestStatusSlugRouting(t *testing.T) {
	// GET /status → 302, Location "/s/default".
	// GET /s/nope → 404; afterwards the cache map has no "nope" key.
	// GET /s/default (anonymous) → 200, with CSP containing "frame-ancestors *".
}

func TestStatusCacheBounded(t *testing.T) {
	// A pageCache with a small byte limit (make the limit a field, set it from
	// statusCacheMaxBytes in New, and set it small in this test). Insert entries until
	// the limit is exceeded: the total stays <= limit and the oldest-used key is evicted
	// first. reset("a") marks only a's entries stale; "b" is still served from cache.
}

func TestStatusRebuildCap(t *testing.T) {
	// With 3 different keys and builds that block on a channel, at most 2 builds are
	// running at the same time (measure with an atomic counter inside build).
}

func TestStatusRateLimited(t *testing.T) {
	// 121 anonymous GETs to /s/default from one client: the 121st returns 429
	// with a Retry-After header. A signed-in admin GET right after is not limited.
}
```

- [ ] **Step 2: Run them and confirm they fail.**
Run: `go test ./internal/web -run 'TestStatus'`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run the whole suite.**
Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: everything `ok`.

- [ ] **Step 5: Commit**
```bash
git add internal/web
git commit -m "feat(web): status pages at /s/{slug} with per-slug bounded cache and rate limit"
```

---

### Task 6: Remove unscoped methods; write the docs and decision

**Files:**
- Modify: `internal/store/*.go` (remove what is listed below), `README.md`, `docs/decisions.md`

**Interfaces:**
- Produces:
  - Remove `legacyWorkspaceID` and every workspace-owned method still on `*Store`. Everything listed under `WS` in Task 2 goes; the engine methods stay. If any non-test caller remains, the build fails, which proves that nothing reaches workspace data unscoped. Store tests that used the removed methods switch to `s.Workspace(1)`; this is a mechanical edit.
  - **README:**
    - The status page is at `/s/default`, and `/status` redirects to it.
    - An upgrade writes `<db>.pre-accounts-<timestamp>.bak` (mode 0600) before migrating. Keep it until you have checked the upgrade, then delete it, because it contains alert secrets.
    - Alert channels saved in the browser belong to the workspace, and the config file's `notify` is copied into workspace 1 once.
    - Update the API section's status URL and the "Layout" line for `internal/netguard` if it is missing.
  - **`docs/decisions.md` D-123**, after D-122:

```markdown
### D-123: Workspace 1 always exists; M1c scope
- **Status:** Accepted (amends §4.4 and the M1c row of the spec's delivery table)
- **Decision:**
  - Schema v2 creates workspace 1 ("Default", `/s/default`) on every database, fresh or upgraded. In M1d, setup adopts it instead of creating a workspace. The pre-migration backup is still written only when data exists.
  - Until M1d adds users, workspace 1 is the operator's workspace.
  - Slug derivation and editing, `CountTargets`, workspace deletion and Check-now coalescing move to M1e, where they are first used. The static allow-list test for the unscoped store moves to M1d.
- **Why:** Before M1d there is no setup page, so a fresh install would otherwise have no workspace and nothing would work. With one workspace always present, every step stays releasable and the setup flow stays simpler.
```

- [ ] **Step 1: Remove the methods. Build and test:**
Run: `go build ./... && gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: everything `ok`.
- [ ] **Step 2: Verify no unscoped workspace methods remain:**
Run: `grep -nE '^func \(s \*Store\) (ListTargets|GetTarget\(|CreateTarget|UpdateTarget|SetPaused|SetMuted|DeleteTarget|RecentChecks|ChecksSince|Stats|DailyUptime|RollupStats|Latencies|HourlyUptime|GetIncident|ListIncidents|AcknowledgeIncident|SetIncidentNote|CountUnacknowledged|CountIncidents|LastAlerts|CountAlerts|CreateMaintenance|GetMaintenance|EndMaintenance|DeleteMaintenance|ListMaintenance|ActiveMaintenance|InMaintenance|GetSetting|SetSetting)' internal/store/*.go`
Expected: no output.
- [ ] **Step 3: Update the docs and commit.**
```bash
git add internal README.md docs/decisions.md
git commit -m "refactor(store): remove unscoped workspace methods; document workspaces and D-123"
```

---

### Task 7: Verify the step

- [ ] **Step 1: CI checks.** Run gofmt, vet, `go test -race`, govulncheck (expect only the 13 known SEC-3 IDs) and the Docker build.
- [ ] **Step 2: Upgrade smoke test (v1 to v2).**
  1. Build the `accounts-m1b` binary. Run it on a temp DB with a LAN target (`http://127.0.0.1:<port>` of a small local HTTP server) and a maintenance window, until checks are recorded.
  2. Stop it with `pkill -TERM -x <name>`, and copy the db together with its `-wal` file.
  3. Run the M1c binary on the copy.
  4. Expect all of the following:
     - a `.pre-accounts-*.bak` file at mode 0600
     - `user_version` 2
     - all rows present
     - the LAN target still checked and up
     - `GET /status` returns 302 to `/s/default`, which returns 200 with the target listed
- [ ] **Step 3: Browser check under CSP with `playwright-cli`** (`mise exec node@24.21.0 -- playwright-cli` from the scratchpad).
  - Dashboard, monitor detail, maintenance, alerts and `/s/default` all render.
  - The sidebar "Status page" link goes to `/s/default`.
  - Live updates still refresh the overview.
  - The console shows no CSP errors.
- [ ] **Step 4: Opus whole-branch review** of `accounts-m1b..accounts-m1c`.
