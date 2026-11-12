# Accounts M3 (Operator Panel) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An operator-only `/admin` panel with two pages:
- **Users:** list and search accounts; disable (which suspends the user's workspaces), enable, delete, resend verification, and issue a reset link.
- **Invites:** create, list and revoke.

It also adds `operator_action` logging and a `-enable-user` recovery flag.

**Architecture:**
- **Store:**
  - New suspension and listing functions: `SetUserDisabled`, `ListUsers`, `ListInvites` and `RevokeInvite`.
  - Suspended workspaces become invisible to the engine (`AllTargets`, `TargetByID`, `TargetsInOtherWorkspaces`) and to the public (`WorkspaceBySlug`).
- **Monitor:**
  - Disabling a user calls `ForgetWorkspace`, the existing tombstone. An in-flight `Reload` therefore cannot restart a runner.
  - Enabling calls a new `RestoreWorkspace` and then restarts the runners.
- **Web:**
  - New `operator` route class, plus a gate in front of the mux. Anything under `/admin` answers 404 to non-operators, whatever the method or origin.
  - The panel lives in `internal/web/admin.go`, with templates on `layout.html`.
  - Panel deletion is disable, then `ForgetWorkspace`, then `DeleteAccount`. `/account` deletion is left untouched.

**Tech Stack:** Go 1.26, SQLite (modernc, single connection), `html/template`, the existing `internal/web` helpers.

**Spec:** `docs/superpowers/specs/2026-11-10-accounts-design.md`:
- §11 the panel
- §6.2 the operator class
- §6.3 Reload refuses suspended workspaces
- §6.5 suspended workspaces return 404
- §6.7 deletion
- §10 `-enable-user`
- §12 `operator_action`
- §13 testing

**Branch:** `accounts-m3`, cut from the tip of `accounts-m2` after M2's fix wave. Never push, and never merge into main.

## Global Constraints

- **Access:**
  - `/admin` and `/admin/*` are for `is_operator = 1` only.
  - Everyone else gets **404**: anonymous visitors, non-operators, wrong methods, unknown sub-paths and cross-site POSTs. That includes a 404 instead of the 403 or 405 the mux would otherwise give.
  - A signed-in operator's cross-site POST still gets 403 from `sameOrigin`.
- **Responses:**
  - **Success:** 303 to `/admin/users` (keeping `q`, `host` and `page` from hidden form fields) or to `/admin/invites`, with a flash. Never use `backTo` or the Referer.
  - **Refusal** (operator target, wrong state): 303 with an explanatory flash.
  - **Wrong password on delete:** 422, re-rendering the delete confirmation page.
  - **One-time link:** a 200 render with `Cache-Control: no-store` and `NoRefresh`, so live refresh can't wipe it. The link never goes in a flash, a URL, a log or a cache.
- **Restrictions:**
  - Operators cannot be disabled, deleted or sent a reset link from the panel.
  - Re-enabling a disabled operator through `-enable-user` is allowed, because it is the recovery path.
  - Panel deletion needs the acting operator's current password (`checkCurrent`) on a confirmation page.
- **Serialisation:** one `sync.Mutex` on `Server` (`adminMu`) is held for the whole of the disable, enable and panel-delete handlers.
- **Logging:**
  - Every successful action logs INFO with these fields: `"event","operator_action"`, `"user_id",<actor>`, `"action",<disable|enable|delete|resend_verification|reset_link|invite_create|invite_revoke>`, `"target_user_id",<id>` (where one applies), `"ip"`.
  - Refusals log WARN with `"result","refused"`.
  - Never log tokens or links.
- **Users page:** 50 rows per page, newest first.
  - Email search is a `LIKE` on email.
  - Host search is a `LIKE` on the monitored URL (D-127 records that it matches anywhere in the URL).
  - Both escape `%`, `_` and `\` with `ESCAPE '\'`.
- **Invites** expire after 7 days. Links are built from `s.cfg.LinkBase()`.
- **UI:**
  - Reuse `layout.html`, the existing table, card and button classes, and `data-confirm`.
  - Template functions are `ts` and `ago`. `ago` takes a `time.Time`, so guard pointers with `{{with}}`.
  - No inline handlers (CSP). No new CSS framework.
- **Decisions** go in `docs/decisions.md` as D-127.
- **Tests:**
  - Everything passes under `go test -race ./...`.
  - Test env names: `e.op` is the operator client; `e.client(t)` is anonymous; `e.account(t, email)` creates a non-operator (see `web_test.go`).
  - Log capture uses `e.srv.logger = slog.New(slog.NewJSONHandler(&buf, nil))` with `syncBuf` (`accounts_test.go` ~617).
  - `last_seen_at` changes only after a 5-minute `e.advance`.

## Review Focus

1. **Non-operators probing `/admin`.** Any non-operator or anonymous request to `/admin...`, with any method or origin, gets 404.
   - Test: Task 4 `TestAdminNotFoundForOthers`.
2. **An in-flight edit racing a disable.** A Reload already running when the user is disabled must leave no runner behind.
   - Test: Task 2 `TestReloadDuringSuspendDoesNotLeaveRunner`.
3. **Restart while disabled.** Monitors stay stopped and the status page stays 404.
   - Tests: Task 1 `TestSuspendedHiddenFromEngine` and Task 2 `TestDisabledStaysStoppedAfterRestart`.
4. **Wildcards in the search.** A search for `50%_off` matches literally and raises no SQL error.
   - Test: Task 1 `TestListUsersSearchEscapes`.
5. **A one-time invite or reset link must not vanish.** Live refresh must not remove it, and it must not reappear on reload.
   - Tests: Task 4 `TestAdminResetLinkWithoutMail` and Task 5 `TestAdminInviteCreate` (assert `data-no-refresh` and `no-store`).

---

### Task 1: Store: suspension, listings, invites

**Files:**
- Create: `internal/store/admin.go`, `internal/store/admin_test.go`
- Modify:
  - `internal/store/targets.go`: `AllTargets` ~69, `TargetByID` ~78
  - `internal/store/workspace.go`: `WorkspaceBySlug`, `TargetsInOtherWorkspaces` ~143
  - `internal/store/delete.go`: `PruneUnverified` gets `AND disabled_at IS NULL`
  - `internal/store/tokens.go`
  - `internal/store/accounts_test.go`: promote the test helper `userByID` ~766 into the real `UserByID`

**Interfaces:**
- Produces:
  ```go
  var ErrOperator = errors.New("store: operators cannot be disabled or deleted here")
  // SetUserDisabled, in one transaction.
  // disabled=true: ErrOperator for operators; sets disabled_at, deletes the
  // user's sessions and unused verify/reset tokens, sets suspended_at on every
  // workspace where the user is the only member, except OperatorWorkspaceID.
  // disabled=false: clears disabled_at and those workspaces' suspended_at
  // (operators allowed: this is the -enable-user recovery path).
  // Returns the affected workspace IDs. ErrNotFound for an unknown user.
  func (s *Store) SetUserDisabled(ctx context.Context, userID int64, disabled bool, now time.Time) ([]int64, error)
  func (s *Store) UserByID(ctx context.Context, id int64) (User, error)
  type UserQuery struct{ Email, Host string; Page int } // Page 0-based, 50 per page
  type UserRow struct {
      User
      LastSeenAt    *time.Time
      WorkspaceID   int64 // first sole-or-owned workspace by ID; 0 if none
      WorkspaceName string
      WorkspaceSlug string
      Monitors      int
  }
  func (s *Store) ListUsers(ctx context.Context, q UserQuery) (rows []UserRow, total int, err error)
  type Invite struct {
      ID                   string // stored token_hash; cannot be used to sign up
      Email                string // "" when unbound
      CreatedAt, ExpiresAt time.Time
  }
  func (s *Store) ListInvites(ctx context.Context, now time.Time) ([]Invite, error) // live only, newest first
  func (s *Store) RevokeInvite(ctx context.Context, id string) error                // ErrNotFound unless an unused invite
  ```
  - The engine queries `AllTargets`, `TargetByID` and `TargetsInOtherWorkspaces` all add `JOIN workspaces w ON w.id = t.workspace_id AND w.suspended_at IS NULL`.
  - `WorkspaceBySlug` adds `suspended_at IS NULL`.
  - The only non-test callers are in the engine (`monitor.go` ~235 and ~295) and the host-rate check. They need no other changes.

- [ ] **Step 1: Write the failing tests** in `admin_test.go`:
  - `TestSetUserDisabled`:
    - Set up a user with a workspace, a target, a session and a verify token.
    - Disable them. Check that `disabled_at` is set, the session and the token are gone, `suspended_at` is set, and the call returned `[wsID]`.
    - Enable them. Check that both columns are cleared and the call returned the same IDs.
  - `TestSetUserDisabledOperator`:
    - Disabling an operator returns `ErrOperator` and changes nothing.
    - Enabling an operator who was disabled through SQL succeeds, and leaves workspace 1 never suspended.
  - `TestSuspendedHiddenFromEngine`:
    - After disabling: `AllTargets` omits the user's target, `TargetByID` returns `ErrNotFound`, `WorkspaceBySlug` returns `ErrNotFound`, and `TargetsInOtherWorkspaces` no longer counts the target.
    - After enabling, all four see it again.
  - `TestListUsers`:
    - Create 55 users with staggered `created_at`.
    - Page 0 has 50 rows, newest first, and `total == 55`. Page 1 has 5 rows.
    - `Monitors` counts the user's targets.
  - `TestListUsersSearchEscapes`:
    - Users `50%_off@x.example` and `500off@x.example`.
    - Searching `50%_` returns only the first.
    - A host search for `api.example` finds the user who has a target `https://api.example.com/x`, and not the other user.
  - `TestUserByID`: one found case and one `ErrNotFound` case.
  - `TestListInvites`: two live invites (one bound, one unbound), one used and one expired. Only the two live ones are listed, newest first, with the email on the bound one.
  - `TestRevokeInvite`:
    - Revoking a live invite makes `PeekToken(TokenInvite, raw)` return `ErrNotFound`.
    - Revoking it again returns `ErrNotFound`.
    - Revoking with a reset token's hash returns `ErrNotFound`, and that token still works.
  - `TestPruneSkipsDisabled`: a disabled unverified user created 49 hours ago survives `PruneUnverified`.
- [ ] **Step 2: Run them and confirm they fail:** `go test ./internal/store/`. Expect FAIL.
- [ ] **Step 3: Implement** as specified above.
- [ ] **Step 4: Run them and confirm they pass:** `go test -race ./internal/store/ ./internal/monitor/ ./internal/web/`.
- [ ] **Step 5: Commit** `feat(store): user suspension, operator listings, invite revocation`.

---

### Task 2: Monitor: `RestoreWorkspace` and the suspension race

**Files:**
- Modify: `internal/monitor/monitor.go` (`ForgetWorkspace` ~458, the `gone` map)
- Test: `internal/monitor/monitor_test.go`, plus `internal/web/account_delete_test.go`-style hooks if needed

**Interfaces:**
- Consumes: Task 1 (`SetUserDisabled`).
- Produces:
  ```go
  // RestoreWorkspace lifts ForgetWorkspace's tombstone so Reload and Start
  // may run that workspace's targets again (used when a user is re-enabled).
  func (m *Monitor) RestoreWorkspace(ws int64)
  ```
- [ ] **Step 1: Write the failing tests:**
  - `TestReloadDuringSuspendDoesNotLeaveRunner`: model it on `TestReloadDuringDeleteDoesNotLeaveRunner` (`internal/web/account_delete_test.go` ~151) and use the `afterReloadCheck` hook.
    1. A `Reload` reads the target.
    2. The test then runs `SetUserDisabled(true)` and `ForgetWorkspace(ws)`.
    3. The `Reload` is released. Assert that no runner exists and no probe reaches the test server.
  - `TestRestoreWorkspace`: after `ForgetWorkspace(ws)`, `Reload` starts nothing. After `SetUserDisabled(false)`, `RestoreWorkspace(ws)` and `Reload`, the target probes again.
  - `TestDisabledStaysStoppedAfterRestart`:
    - With the user disabled, a new `monitor.New` plus `Start` probes nothing for that workspace.
    - It still probes the operator's target.
- [ ] **Step 2: Run them and confirm they fail.** Expect FAIL: `RestoreWorkspace` is undefined.
- [ ] **Step 3: Implement `RestoreWorkspace`.** It deletes `gone[ws]` under `m.mu`.
- [ ] **Step 4: Run them and confirm they pass:** `go test -race ./internal/monitor/ ./internal/web/`.
- [ ] **Step 5: Commit** `feat(monitor): RestoreWorkspace lifts a suspension tombstone`.

---

### Task 3: Web: the operator class and the `/admin` gate

**Files:**
- Modify:
  - `internal/web/routes.go`: `classOperator`, its wrapping
  - `internal/web/session.go`: `requireOperator`
  - `internal/web/web.go`: the gate in `withAuthCache` ~221; `base.IsOperator` and `base.NoRefresh`, set in `s.base` ~290
  - `internal/web/templates/layout.html`: the `data-no-refresh` condition ~30 becomes `.Error` or `.NoRefresh`; add an "Admin" sidebar link when `.IsOperator`
  - `internal/web/static_test.go` ~138 and `internal/web/routes_test.go`: handle `classOperator` in the class switches
- Test: `internal/web/admin_test.go` (create)

**Interfaces:**
- Produces:
  ```go
  const classOperator routeClass = ... // after classWorkspace
  func (s *Server) requireOperator(h func(http.ResponseWriter, *http.Request, *authCtx)) http.HandlerFunc // 404 unless a.User.IsOperator; anonymous → 404, never a login redirect
  // gate: in withAuthCache, before the mux: if path == "/admin" or has prefix "/admin/" and the
  // request is not from a signed-in operator → http.NotFound. Keeps requireOperator as a second line.
  ```
  - Add one placeholder route, `GET /admin`, that redirects 303 to `/admin/users`, so this task's tests have a route. Task 4 adds `/admin/users`.

- [ ] **Step 1: Write the failing test** `TestAdminNotFoundForOthers`:
  - **Requests to make:**
    - every `classOperator` route in `routeTable()`
    - `GET /admin/users/1/disable`, a wrong method on a POST path, added in Task 4
    - `/admin/nope`
    - each POST sent both same-origin and cross-site
  - **Expected responses:**
    - anonymous and non-operator clients get 404 for every request above;
    - the operator's same-origin `GET /admin` gets 303;
    - the operator's cross-site POST gets 403.
  - Update `TestRouteTableDefaultDeny` (`routes_test.go` ~95): anonymous operator-class POSTs now expect 404.
- [ ] **Step 2: Run it and confirm it fails.**
- [ ] **Step 3: Implement** as specified above.
- [ ] **Step 4: Run the tests and confirm they pass:** `go test -race ./internal/web/`.
- [ ] **Step 5: Commit** `feat(web): operator route class and /admin gate`.

---

### Task 4: Web: the users page and its actions

**Files:**
- Create: `internal/web/admin.go`, `internal/web/templates/admin_users.html`, `internal/web/templates/admin_delete.html`
- Modify:
  - `internal/web/routes.go`
  - `internal/web/web.go`: register the templates; add `adminMu sync.Mutex`
  - `internal/web/static_test.go`: `sysAllowed`
  - `internal/web/routes_test.go`: counts
- Test: `internal/web/admin_test.go`

**Interfaces:**
- Consumes:
  - from Task 1: `SetUserDisabled`, `ListUsers`, `UserByID`, `ErrOperator`
  - from Task 2: `RestoreWorkspace`
  - existing: `checkCurrent`, `restartRunners(ctx, *store.WS)`, `s.sys.Workspace(id)`, `sendVerify`, `resetMail`, `s.statusCache` eviction (see how `accountDelete` evicts it), `s.mon.ForgetWorkspace`, `store.CreateToken(TokenReset, id, time.Hour, now)`, `s.cfg.LinkBase()`
- **Routes** (all `classOperator`):
  - `GET /admin/users` (`q`, `host`, `page`)
  - `GET /admin/users/{id}/delete`: the confirmation page with a password field
  - `POST /admin/users/{id}/disable`, `/enable`, `/delete`, `/resend` and `/reset-link`. Each form carries hidden `q`, `host` and `page`.
- **Actions:**
  - **Disable** (holds `adminMu`):
    1. `SetUserDisabled(true)`.
    2. Call `s.mon.ForgetWorkspace(id)` for each returned workspace. This waits for in-flight alerts, up to about 2 minutes; document that in D-127.
    3. Flash "User disabled."
    - `ErrOperator` → refusal flash "Operators cannot be disabled."
  - **Enable** (holds `adminMu`):
    1. `SetUserDisabled(false)`.
    2. For each workspace: `s.mon.RestoreWorkspace(id)`, then `s.restartRunners(ctx, s.sys.Workspace(id))`.
    3. Flash "User enabled."
  - **Delete** (holds `adminMu`):
    - The target must not be an operator; otherwise refusal.
    - `checkCurrent(actor, password)`. A wrong password re-renders `admin_delete` with 422.
    - Then, in order:
      1. `SetUserDisabled(true)`.
      2. `ForgetWorkspace` for each returned workspace.
      3. `DeleteAccount` on `context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)`.
      4. Evict the status cache for those workspaces.
      5. Flash "User deleted."
    - If `DeleteAccount` fails, the user stays disabled; flash "Delete failed; the user is left disabled."
  - **Resend:**
    - 404 unless `s.mail != nil` and the user is unverified and not disabled.
    - Otherwise call `sendVerify` and flash "Verification email queued."
  - **Reset link:**
    - Refused for operators and for disabled users.
    - **With mail:** `s.mail.Send(email, compose)`, where compose creates the token and builds `resetMail(LinkBase+"/reset?token=…")`. Flash "Reset email queued."
    - **Without mail:** create the token and render `admin_users` with 200, `no-store`, `NoRefresh`, and a one-time box holding the link.
- **Users page:**
  - **Columns:** email, workspace name and slug (linked to `/s/<slug>`), created (`ts`), last seen (`ago` inside `{{with}}`), and the monitor count.
  - **Badges:** verified/unverified, disabled, operator.
  - **Row actions:** only the buttons that apply, each with `data-confirm`. Delete is a link to the confirmation page.
  - **Navigation:** previous/next links, and one GET search form with `q` and `host`.
  - **Sub-nav:** Users | Invites.

- [ ] **Step 1: Write the failing tests:**
  - `TestAdminUsersList`: the operator sees both emails and the monitor count; `?q=` and `?host=` filter.
  - `TestAdminDisableEnable`:
    - After disabling B: B's cookie no longer reaches `/` (redirect to login), B's monitor stops probing (the httptest counter is flat across `e.advance` past the interval), and `/s/<slug>` returns 404.
    - After enabling B: B can sign in, the status page returns 200, and the monitor probes again.
  - `TestAdminCannotDisableOperator`: disabling the operator itself, or another operator made through the store, gets a 303 refusal flash and changes nothing.
  - `TestAdminDelete`:
    - The confirmation page renders.
    - A wrong password gives 422 and B still exists.
    - The right password removes B along with B's workspace and runner.
    - Deleting an operator is refused.
  - `TestAdminResetLinkWithoutMail`:
    - The response is 200 with `no-store`, has `data-no-refresh` on the content, and the body contains `/reset?token=`.
    - The link works from an anonymous client.
    - A fresh `GET /admin/users` does not contain the link.
    - Asking for an operator's link is refused.
  - `TestAdminResetLinkWithMail` (`newEnvMail`): one reset mail goes to B.
  - `TestAdminResendVerification` (mail env): an unverified user gets one verify mail; a verified user gets 404.
  - `TestOperatorActionLogged`: after a disable, the captured log contains `"event":"operator_action"`, `"action":"disable"`, `"user_id"` and `"target_user_id"`.
  - `TestAdminNotFoundForOthers` now also covers these routes; add the wrong-method case on `/admin/users/1/disable`.
- [ ] **Step 2: Run them and confirm they fail.**
- [ ] **Step 3: Implement** as specified above, keeping each `sysAllowed` list minimal.
- [ ] **Step 4: Run the tests and confirm they pass:** `go test -race ./internal/web/`, which includes the static, route and isolation tests.
- [ ] **Step 5: Commit** `feat(web): operator users page with disable, enable, delete, resend and reset link`.

---

### Task 5: Web: the invites page

**Files:**
- Create: `internal/web/templates/admin_invites.html`
- Modify:
  - `internal/web/admin.go`
  - `internal/web/routes.go`
  - `internal/web/web.go`
  - `internal/web/static_test.go`
  - `internal/web/routes_test.go`
- Test: `internal/web/admin_test.go`

**Interfaces:**
- Consumes:
  - `store.CreateInvite(ctx, email, 7*24*time.Hour, now)`
  - from Task 1: `ListInvites` and `RevokeInvite`
- **Routes** (all `classOperator`):
  - `GET /admin/invites`
  - `POST /admin/invites`, with an optional `email`
  - `POST /admin/invites/revoke`, with field `id`
- **Create:**
  - An invalid email gives 422 with `msgBadEmail`.
  - On success, render 200 with `no-store`, `NoRefresh`, and a one-time box holding `LinkBase+"/signup?invite=…"`.
  - Log `action=invite_create` without the link.
- **List:** created, expires, and the bound email (or "anyone").
- **Revoke:**
  - Uses `data-confirm`.
  - Unknown or used invites get a 303 with the flash "Invite not found."
  - Log `action=invite_revoke`.

- [ ] **Step 1: Write the failing tests:**
  - `TestAdminInviteCreate`:
    - With and without an email, the response is 200 with `no-store`, `data-no-refresh` and a link.
    - The bound invite prefills its email when opened from an anonymous client.
    - A fresh GET of the page does not contain the link.
  - `TestAdminInviteListAndRevoke`: two invites are listed; after one is revoked, it is gone from the list.
  - `TestRevokedInviteRefused`: `GET /signup?invite=<revoked>` shows `msgBadInvite`.
- [ ] **Step 2: Run them and confirm they fail.**
- [ ] **Step 3: Implement** as specified above.
- [ ] **Step 4: Run the tests and confirm they pass:** `go test -race ./internal/web/`.
- [ ] **Step 5: Commit** `feat(web): operator invites page (create, list, revoke)`.

---

### Task 6: CLI `-enable-user`, docs and D-127

**Files:**
- Modify: `main.go` (the flags ~67–73 and the dispatch beside `-reset-password`), `main_test.go`, `README.md`, `docs/decisions.md`

**Interfaces:**
- Produces: `func enableUser(cfgPath, email string) error`.
  - Opens the store and looks up the user by email.
  - Calls `SetUserDisabled(false)`, which works for operators too.
  - Prints `enabled <email>` and exits.
  - Register it as `flag.String("enable-user", "", "re-enable a disabled account by email and exit (restart the server afterwards so its monitors resume)")`.

- [ ] **Step 1: Write the failing test** `TestEnableUser`, following the existing CLI-flag tests:
  - A disabled user has `disabled_at` and `suspended_at` cleared.
  - A disabled operator is re-enabled.
  - An unknown email returns an error.
- [ ] **Step 2: Run it and confirm it fails.**
- [ ] **Step 3: Implement** as specified above.
- [ ] **Step 4: Docs.**
  - **README:** an "Operator panel" section covering:
    - `/admin` and who can see it;
    - each action and its effect on sessions, workspaces and monitors;
    - that disable and delete wait for in-flight alerts, so the page can be slow;
    - invites;
    - `-enable-user`, followed by a restart.
  - **D-127:**
    - The panel answers 404 to everyone else, including anonymous visitors, wrong methods and cross-site POSTs.
    - Suspension is enforced through store joins plus the monitor tombstone.
    - Panel delete is disable, then forget, then delete; a failure leaves the user disabled. `/account` self-delete is unchanged and relies on the tombstone.
    - Invites are identified by their stored hash.
    - The reset link is shown once when there is no mail.
    - The host search matches anywhere in the URL.
    - An incident that is open at suspension stays open, and its "up" alert after re-enabling includes the suspended time.
    - Disabled unverified users are not pruned (update the README sentence from M2 that says pruning includes disabled accounts, and the matching D-126 note).
    - Operators cannot be sent reset links from the panel.
- [ ] **Step 5: Verify:** `gofmt -l .` prints nothing and `go test -race ./...` passes.
- [ ] **Step 6: Commit** `feat: -enable-user recovery flag; docs: operator panel, D-127`.

---

### Task 7: Step verification

- [ ] **Step 1: CI checks.** Run gofmt, vet, `go test -race -count=1 ./...` and the Docker build. Run govulncheck inside `golang:1.26-alpine`.
- [ ] **Step 2: Smoke test.**
  1. Set up the operator.
  2. Invite B; B signs up and adds a monitor.
  3. Disable B: B's monitor stops, B is signed out, and B's status page returns 404.
  4. Enable B, then delete B with the operator's password.
  5. Create an invite, then revoke it.
  6. Stop the server, run `-enable-user` on a disabled user, and start the server again.
  7. Shut down with `pkill -TERM -x <name>`.
- [ ] **Step 3: Browser check** with `mise exec node@24.21.0 -- playwright-cli`.
  - Cover the users page, search and pagination, the confirmation dialogs, the delete confirmation page, the one-time link box (wait past a live refresh), and the invites page.
  - A non-operator must get 404 on `/admin`.
  - There must be no console or CSP errors.
- [ ] **Step 4: Whole-branch review** of `accounts-m2..accounts-m3` by Opus, then one fix wave. Then a final review of the whole accounts stack (`main..accounts-m3`, against the spec's §1.2 success criteria) by Fable.
