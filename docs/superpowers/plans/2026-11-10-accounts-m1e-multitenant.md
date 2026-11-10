# Accounts M1e: Multi-tenant Features Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let strangers have accounts safely, through invite-based sign-up. This covers:
- sign-up modes `closed` and `invite`, operator invites (`-invite`) and invite sign-up
- workspace slugs that are derived and can be edited
- per-workspace limits and abuse controls
- restrictions on alert channels for non-operator workspaces
- account deletion
- the README, SECURITY.md and `config.example.json`

**Architecture:**
- **Store.** The store gains invite tokens and slug derivation and validation. `CreateAccount` gains `Verified` and an in-transaction invite consume. New: `DeleteAccount`, `CountTargets`, `CountWorkspaces`.
- **Monitor.** The monitor gains `RemoveWorkspace`, Check-now coalescing and a User-Agent naming the instance.
- **Notify.** It gains a `Restricted` mode for non-operator workspaces:
  - webhooks and Telegram don't follow redirects
  - Telegram tokens are validated
  - email requires TLS and login on 465/587, with at most 5 recipients
- **Web.** It adds `/signup`, `/account/delete`, slug editing in status settings, and the §8.2 per-workspace limits. The operator workspace is exempt from those limits.
- **Status cache.** It is keyed by workspace ID, not slug, because slugs can now change.

**Execution:** subagent-driven, with Sonnet implementers and Opus reviewers. Reuse what exists:
- `ratelimit` (`Take`/`Undo`), `CreateToken`/`PeekToken`/`consumeToken`, `createWorkspace`, `NormalizeEmail`, `CheckLength`, `Hasher`
- `renderStatus`, `loadByID`, `newEnv`/`e.account`
- the isolation suite's `isoCases`, and `netguard`

Write no code a task doesn't need.

**Tech Stack:** Go 1.26, `modernc.org/sqlite`, `golang.org/x/crypto/argon2`. No new dependencies.

**Spec:** [docs/superpowers/specs/2026-11-10-accounts-design.md](../specs/2026-11-10-accounts-design.md), step **M1e** (§2). Sections used: §5.2 (invite parts), §5.5 (delete), §6.3, §6.5 (slugs), §6.6, §6.7, §7.2, §7.3, §8.2 (M1e rows), §9, §10 (`-invite`), §13 and §14. Also D-122 and D-123 (items moved to this step).

## Global Constraints

- **Branch and merges.** Work on branch `accounts-m1e`, cut from `accounts-m1d`. Nothing is merged or pushed.
- **Sign-up modes.** `signup` accepts `closed` (the default) or `invite`. `open` is rejected with `signup "open" is not supported yet` until M2. Operator invites work in every mode.
- **Startup checks (§9.3).**
  - `signup` other than `closed` with an empty `base_url` is an error.
  - `signup` other than `closed` with an `http` `base_url` whose host is not `localhost` and not a loopback IP is an error.
  - `allow_private_targets: all` with `signup` other than `closed` logs a WARN.
- **`limits` config.** Fields and defaults:
  - `max_monitors` is `*int`, default 20; 0 means unlimited
  - `min_interval` is a Duration, default 30s
  - `max_workspaces` is `*int`, default 0, unlimited
  - `signups_per_hour` is `*int`, default 100
  - `host_requests_per_second` is `*float64`, default 1
  - `mail_per_hour` is rejected with `limits.mail_per_hour is not supported yet` until M2

  Pointer fields are resolved in `applyDefaults` (§3.2.8).
- **Operator exemption.** The operator's workspace is exempt from every per-workspace limit (§8.2). Sign-in, setup and hashing limits apply to everyone.
- **Order of checks (§8.3).** Every request runs, in order: rate limits, then validation, then token/invite validity, then hashing, then the transaction.
- **Invite tokens.** Kind `invite`, a 7-day expiry, an optional bound email, single use. A failed sign-up (validation error or duplicate email) does **not** consume the invite. Issuing a new invite never deletes earlier ones.
- **Accounts created by invite** are verified (`verified_at = now`).
- **Gates.** `gofmt -l .` prints nothing, and `go vet ./...` and `go test -race -count=1 ./...` pass at every commit.

## Scope rulings (recorded as D-125 in Task 9)

1. **`open` sign-up stays rejected until M2.** It requires mail and verification.
2. **The operator workspace stays workspace 1.** Only setup creates an operator, and setup adopts workspace 1 (D-123/D-124), so `cfg.AllowPrivate(ws == 1)` in `main.go` and `cfg.AllowPrivate(a.IsOperator)` in web agree. Add one test that pins this. The single source becomes `store.OperatorWorkspaceID = 1` (an exported const), used in both places.
3. **The status cache is keyed by workspace ID**, because slugs are now editable (M1c carry-over).
4. **Saved secrets.** A secret stays saved only while its destination is unchanged (§6.6). This applies to every workspace, not just non-operator ones, because it protects any saved credential.
5. **Invite management in the browser comes in M3.** M1e has `-invite` only.

## Review Focus

1. **Invite reuse or misuse.** Expected behaviour:
   - A consumed invite can't be reused.
   - An email-bound invite rejects other addresses.
   - A failed sign-up leaves the invite usable.
   - Two concurrent sign-ups with one invite create exactly one account.

   Test: Task 3, `TestInviteSignup*`.
2. **A stranger's workspace probing internal networks or relaying through alert channels.** Expected behaviour:
   - With the default `operator` setting, non-operator monitors, Test URL, webhooks, Telegram and SMTP are all guarded (M1b).
   - Webhooks don't follow redirects.
   - SMTP refuses plaintext and ports other than 465/587.

   Tests: Task 5, plus the isolation suite.
3. **Flooding a third-party host.** Expected behaviour:
   - Non-operator workspaces together can't schedule more than `host_requests_per_second` checks per second to one hostname.
   - Monitor caps, the minimum interval, the creation rate and Check-now limits all hold.

   Test: Task 4, `TestHostRateLimit*`.
4. **Account deletion.** Expected behaviour:
   - It removes every row and every runner.
   - The last operator can't delete their account.
   - A request already in flight can't restart a runner afterwards.

   Test: Task 6, `TestDeleteAccount*`.
5. **Slug changes.** Expected behaviour:
   - A changed slug moves the public page.
   - The old slug returns 404.
   - Two workspaces can't share a slug.
   - The cache doesn't serve the old page under the new slug.

   Test: Task 7, `TestSlug*`.

---

### Task 1: Config — sign-up modes, limits, startup checks

**Files:** `internal/config/config.go`, `internal/config/config_test.go`, `main.go` (the WARN)

**Interfaces:**
- **Config fields and rules.**
  - `Signup` is `closed` or `invite`.
  - `type Limits struct { MaxMonitors *int; MinInterval Duration; MaxWorkspaces *int; SignupsPerHour *int; HostRequestsPerSecond *float64; MailPerHour *int }`, with the json tags `max_monitors`, `min_interval`, `max_workspaces`, `signups_per_hour`, `host_requests_per_second` and `mail_per_hour`.
  - `Config.Limits Limits`. This replaces the `json.RawMessage` used in M1d, which rejected any `limits` value.
- **Defaults and validation.**
  - Every pointer field is non-nil after `applyDefaults`.
  - Negative values are rejected, and so is `min_interval` under 1s.
  - `MailPerHour` set to anything gives the M2 error.
- **Startup checks.** These are the §9.3 checks from Global Constraints. Each error starts with `config: ` and names the key.
- **`main.go`.** Logs WARN `allow_private_targets is "all" while sign-up is open to others` when the guard is `all` and `signup` is not `closed`.

- [ ] **Step 1: Failing tests.**
  - `TestSignupModes`: `invite` is accepted, `open` is rejected, and `bogus` is rejected.
  - `TestLimitsDefaults`: checks every default, and that `max_monitors: 0` stays 0 (unlimited).
  - `TestLimitsValidation`: rejects negatives, `min_interval: "500ms"` and `mail_per_hour`.
  - `TestSignupNeedsBaseURL`:
    - `invite` with no `base_url` is an error
    - `invite` with `http://status.example.com` is an error
    - `invite` with `http://localhost:8080` or `http://127.0.0.1:8080` is OK
    - `invite` with an https URL is OK
- [ ] **Steps 2–4: Fail, implement, full suite.**
- [ ] **Step 5: Commit** `feat(config): invite sign-up mode, limits, startup checks for public instances`.

---

### Task 2: Store — invites, slugs, verified accounts, counts, deletion

**Files:** `internal/store/tokens.go`, `users.go`, `workspace.go`, new `internal/store/delete.go`, tests in `internal/store/accounts_test.go` and `workspace_test.go`

**Interfaces:**
- **Invite tokens.**
  - Add the `TokenInvite` kind.
  - `CreateInvite(ctx, email string, ttl time.Duration, now time.Time) (raw string, err error)`. It stores a normalised email, or NULL when `email` is empty, and deletes nothing.
  - `PeekToken` and `consumeToken` accept `TokenInvite`, which has no user ID.
  - `type Token` gains `Email string`.
  - Today `CreateToken` rejects unknown kinds. Keep invites in their own constructor.
- **`AccountInput`.**
  - Gains `Verified bool` and `InviteToken string`.
  - When `InviteToken` is set, `CreateAccount` consumes it inside the same transaction, after the user insert succeeds. If the user insert fails (for example `ErrEmailTaken`), the whole transaction rolls back and the invite stays unused.
  - When the invite is bound to an email that differs from the normalised `Email`, it returns `ErrInviteEmail`. Check this *before* any write, using `PeekToken`, then re-check it in the consume.
- **Slugs.**
  - `func DeriveSlug(ctx, q querier, name string) (string, error)` follows §6.5 steps 1–4. A collision appends `-2`, `-3` and so on, truncating the base so the result is at most 40 characters.
  - `func ValidSlug(s string) bool`: 3–40 characters of `[a-z0-9-]`, with no leading or trailing `-`.
  - `func (w *WS) SetSlug(ctx, slug string) error` returns `ErrInvalidSlug` or `ErrSlugTaken`.
  - `func (w *WS) Rename(ctx, name string) error`.
  - `CreateAccount` uses `DeriveSlug` when `in.Slug == ""`.
- **Counts.**
  - `func (w *WS) CountTargets(ctx) (int, error)`.
  - `func (s *Store) CountWorkspaces(ctx) (int, error)`.
  - `func (s *Store) TargetsInOtherWorkspaces(ctx, exceptWS int64) ([]Target, error)`, for the host-rate check. It returns the targets of every workspace except `exceptWS` and the operator workspace.
- **Operator workspace.** `const OperatorWorkspaceID int64 = 1`, with a doc comment that refers to D-123/D-124/D-125.
- **Deletion.**
  - `func (s *Store) DeleteAccount(ctx, userID int64) (deletedWorkspaces []int64, err error)` runs in one transaction:
    1. Refuse with `ErrLastOperator` if the user is the only operator.
    2. For each workspace where the user is the only member, delete alerts, incidents, checks and daily rows through its targets, then the workspace. Cascades remove targets, maintenance, settings and memberships.
    3. Delete the user. That cascades to sessions and tokens.
    4. Return the IDs of the deleted workspaces.
  - Sessions are deleted first, by the caller (Task 6).

- [ ] **Step 1: Failing tests.**
  - **`TestInviteTokens`**
    - An invite bound to an email has its email normalised. An unbound invite has an empty email.
    - It expires after 7 days.
    - It is single use.
    - Two invites coexist: creating the second does not delete the first.
  - **`TestCreateAccountWithInvite`**
    - A valid invite creates the account and consumes the invite.
    - A duplicate email returns `ErrEmailTaken` and leaves the invite unused, so a retry with a new email succeeds.
    - An invite bound to `a@x.co` used with `b@x.co` returns `ErrInviteEmail`, writes nothing and leaves the invite unused.
    - Two concurrent calls with one invite create exactly one user.
    - `Verified: true` sets `verified_at`.
  - **`TestDeriveSlug`**
    - `"My Team!"` gives `my-team`.
    - `"  ---  "` and `"é"` give `ws-` followed by 6 base32 characters, and the result passes `ValidSlug`.
    - A 60-character name gives a result of at most 40 characters.
    - Collisions give `my-team-2`, then `my-team-3`.
    - A collision on a name that is 40 characters long truncates the base to fit `-2`.
  - **`TestSetSlug`**: invalid slugs and a slug taken by another workspace fail; a valid one succeeds.
  - **`TestCounts`**: `CountTargets`, `CountWorkspaces`, and that `TargetsInOtherWorkspaces` excludes the given workspace and workspace 1.
  - **`TestDeleteAccount`**
    - Seed workspace B with targets, checks, incidents, alerts, maintenance, settings, sessions and tokens.
    - `DeleteAccount` returns `[B]`.
    - Afterwards, no rows reference B's targets in checks, incidents, alerts or daily, and the workspace, user, sessions and tokens are gone.
    - The operator gets `ErrLastOperator`, and their data is untouched.
- [ ] **Steps 2–4: Fail, implement, full suite.**
- [ ] **Step 5: Commit** `feat(store): invites, slug derivation, verified accounts, counts and account deletion`.

---

### Task 3: Invite sign-up and `-invite`

**Files:** new `internal/web/signup.go`, new `templates/signup.html`, and changes to `routes.go`, `login.go`/`templates/login.html`, `main.go`, the tests and `internal/web/web_test.go`. In `web_test.go`, `e.account` switches to `Verified: true`, which removes the reset-token workaround.

**Interfaces:**

**Routes.** `GET /signup` and `POST /signup`, both in `classGuest`.

**`GET /signup`.** Send `Cache-Control: no-store`.
- With `?invite=<raw>` and a valid invite (`PeekToken(TokenInvite)`), show the form with the invite in a hidden field. If the invite is bound to an email, prefill that email and make the field read-only.
- With no invite, the response depends on the mode:
  - `closed`: 404
  - `invite`: 200 with "Sign-up on this instance is by invitation. Ask the operator for a link."
- With an invalid or expired invite: 200 with "This invitation is invalid or has expired." and no form.

**`POST /signup`.** Run these steps in order:
1. **Limits:**
   - `signupByAddr = ratelimit.New(time.Hour, 5, 0)`, called with `Take` and keyed by `clientKey`.
   - An instance-wide `signupGlobal = ratelimit.New(time.Hour, *cfg.Limits.SignupsPerHour, 0)`, called with `Take` on the key `"all"`.
   - If either refuses, return 429 with `msgTooMany`.
2. **Validation:**
   - `NormalizeEmail`.
   - `CheckLength` on the password, and the confirmation must match.
   - The workspace name must be 1–60 characters with no Cc or Cf characters. Reuse setup's validator.
   - On failure, return 422 and keep the invite in the form.
3. **Invite:** check it with `PeekToken`. If it is invalid, return 404.
4. **Workspace cap:** when `*cfg.Limits.MaxWorkspaces > 0` and `CountWorkspaces >= max`, return 403 "This instance is not accepting new accounts."
5. **Hash** the password.
6. **Create the account:** `CreateAccount{Verified: true, InviteToken: raw, Slug: ""}`, which derives the slug.
   - `ErrEmailTaken` gives 422 "An account with this email already exists. Sign in instead."
   - `ErrInviteEmail` gives 422 "This invitation is for a different email address."
   - `ErrNotFound` gives 404.
7. **On success:** log `event=signup` with `user_id` and `ip`, create a session, set the cookie, and redirect 303 to `/`.

**Refunds.** Undo the address and global slots when no account was created for a reason other than validation (404 or 503). Validation failures keep their slot.

**Login page** (`login.html`) gets a `SignupMode` field:
- `invite`: show "Have an invitation link? Open it to sign up."
- `closed`: show nothing.

**`main.go` `-invite [email]`.** Open the store, call `CreateInvite(email, 7*24h)`, print `<LinkBase>/signup?invite=<raw>` and exit. It works in every mode. In setup mode it errors with "finish setup first".

**`safeNext`** already denies `/signup`. Verify that it does.

- [ ] **Step 1: Failing tests.**
  - **`TestSignupModes`**
    - `closed`: `/signup` gives 404, and a valid invite gives 200 with the form.
    - `invite`: no invite gives 200 with the message; a valid invite gives the form.
  - **`TestInviteSignupFlow`**
    - A POST gives 303. The new account's session works, and its workspace has a derived slug.
    - Reusing the same invite gives 404.
    - The account is verified.
  - **`TestInviteSignupFailuresKeepInvite`**: a duplicate email gives 422 and a password mismatch gives 422. After both, a corrected POST with the same invite succeeds.
  - **`TestInviteBoundEmail`**: a POST with a different email gives 422 and the invite stays usable.
  - **`TestSignupLimits`**: the 6th POST from one address within an hour gives 429. With `signups_per_hour: 2`, the 3rd sign-up from any address gives 429. With `max_workspaces` already reached, the response is 403.
  - **`TestInviteCLI`**: the helper prints a link, and the link works.
- [ ] **Steps 2–4: Fail, implement, full suite.** Update `routes_test.go` (count and guest set) and add `isoCases`/coverage entries if any routes are workspace-class.
- [ ] **Step 5: Commit** `feat(web): invite sign-up, sign-up modes and -invite`.

---

### Task 4: Per-workspace limits and abuse controls

**Files:** `internal/web/targets.go`, `alerts.go`, `web.go` (limiters), `internal/monitor/monitor.go` (coalescing, User-Agent), and tests.

**Interfaces:**
- **Exemption.** Use `a.IsOperator` to decide who is exempt. Every check below is skipped for operators.
- **Monitor creation (`targetCreate`).** Checks run in this order:
  1. Creation-rate limit: `createLimiter = ratelimit.New(time.Hour, 30, 0)`, keyed by workspace ID, via `Take`. On refusal, return 429 "You're creating monitors too quickly."
  2. Validation (as today) plus the minimum interval: `interval >= cfg.Limits.MinInterval`, otherwise 422 "Check interval must be at least <min_interval>."
  3. Monitor cap: when `*MaxMonitors > 0` and `CountTargets >= max`, return 422 "This workspace has reached its limit of <n> monitors."
  4. Host rate: the sum of 1/interval over `TargetsInOtherWorkspaces(ws)` with the same lowercased hostname, plus this target's 1/interval, must be ≤ `*HostRequestsPerSecond`. Otherwise return 422 "Too many checks to this host from this instance. Use a longer interval."
- **`targetUpdate`:** same checks except the creation rate.
  - The monitor cap doesn't apply.
  - For the host rate, exclude the target's own current row.
- **Check now:**
  - **Coalescing.** `monitor.CheckAsync` returns `bool`: false when a check for that target is already running or queued. Track this with a per-target `inflight` flag under the state lock. A coalesced request still returns 303 with flash "A check is already running."
  - **Limits.** `checkTarget = ratelimit.New(10*time.Second, 1, 0)` keyed by target ID, and `checkWS = ratelimit.New(time.Minute, 30, 0)` keyed by workspace ID. On refusal, return 429.
- **Test URL:** `testURL = ratelimit.New(time.Minute, 10, 0)` keyed by workspace ID. Return JSON 429 on refusal.
- **Alert test send:** `alertTest = ratelimit.New(time.Minute, 5, 0)` keyed by workspace ID.
- **User-Agent:** `monitor.Options.UserAgent string`. `main` sets it to `uptime-monitor/<version>`, plus ` (+<base_url>)` when `base_url` is set. `Probe` uses it, falling back to today's string.

- [ ] **Step 1: Failing tests** (non-operator via `e.account`; operator via `e.op`).
  - **`TestMonitorCapAndInterval`**
    - With `max_monitors: 2`, the 3rd create gives 422.
    - An interval of `10s` gives 422.
    - The operator is not limited by either.
  - **`TestCreateRate`:** the 31st create in an hour gives 429, even when monitors are deleted in between.
  - **`TestHostRateLimitAcrossWorkspaces`**
    - Accounts B and C each add `https://same.example/` at a 2s interval (0.5 + 0.5 = 1). C's next add at a 10s interval gives 422.
    - A different host still works.
    - The operator's targets don't count toward the limit and aren't limited by it.
  - **`TestCheckNowLimitsAndCoalescing`**
    - Two immediate Check-now POSTs: the second is coalesced or limited, and only one extra check is recorded.
    - The workspace limit is 30 per minute.
  - **`TestTestURLAndAlertTestLimits`:** the 11th Test URL gives 429, and the 6th alert test gives 429.
  - **`TestUserAgent`:** an `httptest` server records `User-Agent == "uptime-monitor/dev (+https://status.example.com)"`.
- [ ] **Steps 2–4: Fail, implement, full suite.**
- [ ] **Step 5: Commit** `feat: per-workspace monitor limits, host rate cap, check-now coalescing and limits, instance User-Agent`.

---

### Task 5: Alert-channel restrictions and secret destinations

**Files:** `internal/notify/notify.go`, `internal/config/config.go`, `internal/web/alerts.go`, `main.go` (Notifiers func) and tests.

**Interfaces:**
- **`FromConfig`.** The signature becomes `FromConfig(cfg config.Notify, logger, allowPrivate, restricted bool) Multi`. When `restricted`:
  - The HTTP client doesn't follow redirects: `CheckRedirect` returns `http.ErrUseLastResponse`. Build it with `netguard.Client`, then set `CheckRedirect` on the returned client.
  - `Email.RequireTLS = true`.
- **Email:**
  - New field `Email.RequireTLS bool`. When set, on any port other than 465, `send` fails with `email: server does not offer STARTTLS` if STARTTLS isn't offered.
  - Extract `func (e Email) Send(ctx, to []string, subject, body string) error`, used by `Notify` (§3.3, for M2).
- **Validation.**
  - `config.ValidateNotifyRestricted(n *Notify) error` runs `ValidateNotify` plus:
    - the Telegram token must match `^\d+:[A-Za-z0-9_-]{30,}$`
    - the email port must be 465 or 587
    - an email username is required
    - at most 5 `to` addresses
  - The messages name the field.
  - `alerts.go` `saveNotify` uses the restricted validator when `!a.IsOperator`.
- **Secret destinations (§6.6, all workspaces).** In `parseNotifyForm`, keep a blank secret field's saved value only while its destination is unchanged:
  - Email: `smtp_host`, `smtp_port` and `username`.
  - Telegram: `chat_id`.
  - Slack: the webhook *host*. A blank field with a changed host isn't possible, because the webhook URL is itself the secret, so this only means a blank field keeps the URL.

  When the destination changes, a blank secret becomes empty, and validation then asks for it ("re-enter the password").
- **`main.go` Notifiers func:** `restricted := ws != store.OperatorWorkspaceID`, and `allowPrivate := cfg.AllowPrivate(!restricted)`.
- **Web test sends** use the same `FromConfig` flags.

- [ ] **Step 1: Failing tests.**
  - `notify`, `TestRestrictedNoRedirects`:
    - A webhook server that answers 307 to a second server: with `restricted`, the error mentions status 307 and the second server sees no request.
    - Without `restricted`, the redirect is followed.
  - `notify`, `TestRequireTLS`: a fake SMTP server without STARTTLS on a port that isn't 465 is refused when `RequireTLS` is set, and accepted otherwise.
  - `config`, `TestValidateNotifyRestricted`: one case per rule.
  - `web`, `TestRestrictedChannelsForAccounts`:
    - A non-operator saving SMTP port 25 gets 422, and so does saving a bad Telegram token.
    - The operator can still save port 25.
  - `web`, `TestSecretClearedWhenDestinationChanges`:
    - With an SMTP password saved, changing `smtp_host` with a blank password gives 422 asking for the password.
    - With the host unchanged, a blank password keeps the saved one.
- [ ] **Steps 2–4: Fail, implement, full suite.**
- [ ] **Step 5: Commit** `feat(notify): restricted channels for non-operator workspaces; secrets tied to their destination`.

---

### Task 6: Account deletion and workspace removal

**Files:** `internal/monitor/monitor.go`, `internal/web/account.go`, `templates/account.html`, `internal/web/status.go` (cache eviction), tests.

**Interfaces:**
- **Monitor:**
  - `func (m *Monitor) RemoveWorkspace(ws int64)` stops every runner of `ws` and waits for that workspace's in-flight probes (a per-workspace `sync.WaitGroup`, or an existing per-target wait). It then drops the workspace's notifier cache entry.
  - `Reload` refuses targets whose workspace no longer exists. It already returns nil on `ErrNotFound`, so verify that.
- **Route:** `POST /account/delete` (`user` class, POST so `sameOrigin`).
  1. Run the current-password check with the same per-user limiter as change password.
  2. Delete the user's sessions.
  3. For the user's workspace, call `mon.RemoveWorkspace`.
  4. Call `store.DeleteAccount`. On `ErrLastOperator`, return 422 "The last operator can't delete their account."
  5. Evict the status cache entries for each deleted workspace.
  6. Clear the cookie.
  7. Log `event=account_deleted user_id=…`.
  8. Redirect 303 to `/login` with flash "Your account was deleted."
- **Status cache keyed by workspace ID** (ruling 3): keys become `<wsID>|theme[|partial]`, with `reset(wsID)` and `evict(wsID)`. Update call sites and tests mechanically.
- **Template:** `account.html` gets a "Delete account" form that asks for the current password and uses the existing `data-confirm` dialog. It is shown only when `!a.User.IsOperator`.

- [ ] **Step 1: Failing tests.**
  - `TestDeleteAccountRemovesEverything`:
    - Account B has a target running at a 1s interval against `httptest`.
    - The delete POST returns 303.
    - B's session gets 401.
    - Checks recorded for B's target stop growing: compare counts across 1.5s through a raw query.
    - `/s/<B slug>` returns 404.
    - B's email can't sign in.
    - A fresh invite can sign up again with the same email.
  - `TestDeleteAccountWrongPassword`: returns 422 and nothing is deleted.
  - `TestLastOperatorCannotDelete`: the operator gets 422 and the data is intact.
  - `TestDeletedTargetNotRestarted`: call `mon.Reload` for a deleted target and assert no runner exists afterwards.
- [ ] **Steps 2–4: Fail, implement, full suite.** Add the new route to `routes_test.go` (user class).
- [ ] **Step 5: Commit** `feat: account deletion removes data and runners; status cache keyed by workspace`.

---

### Task 7: Slug editing

**Files:** `internal/web/status.go` (status settings), `templates/status.html`, tests.

**Interfaces:**
- **Status settings form.** It gains a `slug` field, prefilled with the current slug, and an optional `name` field for the workspace name.
- **On POST:**
  - When `slug` has changed:
    - `ValidSlug` false gives 422 "Use 3–40 lowercase letters, digits and dashes."
    - `ErrSlugTaken` gives 422 "That address is taken."
    - Otherwise call `ws.SetSlug`.
  - When `name` has changed: validate it with the workspace-name rule, then call `ws.Rename`.
  - Then reset that workspace's status cache and redirect to the new `/s/<slug>`.
- **No URL rewriting.** The old slug simply returns 404 (§6.5).
- **Sidebar link.** `base.StatusURL` already reads the current slug, so the sidebar link follows the change.

- [ ] **Step 1: Failing tests.**
  - **`TestSlugChange`**
    - The owner changes the slug to `new-name`.
    - Afterwards `/s/new-name` returns 200, `/s/<old>` returns 404, and the sidebar link updates.
    - Setting a slug that another account already uses returns 422.
    - Setting `Bad_Slug` returns 422.
    - `/s/new-name` must show the new title after the change, not a cached copy of the old page.
- [ ] **Steps 2–4: Fail, implement, full suite.** The isolation suite already attacks `POST /status/settings`. Extend its `isoCases` form with `slug=attacker-slug`. Assert that A's slug is unchanged in the snapshot (add the workspace slug and name to the snapshot).
- [ ] **Step 5: Commit** `feat(web): edit the status page address and workspace name`.

---

### Task 8: Isolation suite and static checks for the new surface

**Files:** `internal/web/isolation_test.go`, `internal/web/static_test.go`

- [ ] **Step 1: Extend the suite.**
  - Every new workspace-class route needs an `isoCase`. The coverage test enforces this.
  - The snapshot now includes the workspace name and slug.
  - Add a case where B's `/account/delete` can only ever delete B: A's snapshot is unchanged and A's runner keeps going.
  - Add a case where B signing up again through an invite bound to A's email fails, so it can't take over A's address.
  - Update `sysAllowed` for new `s.sys` calls in `signup.go` and `account.go`, at function granularity, and justify each one in the report.
- [ ] **Step 2: Prove the suite bites.** Temporarily make `statusSettings` call `s.sys.Workspace(store.OperatorWorkspaceID).SetSlug`, and confirm the suite fails. Revert.
- [ ] **Step 3: Full suite. Commit** `test(web): isolation suite covers sign-up, deletion and slug changes`.

---

### Task 9: Docs, SECURITY.md, config example, D-125

**Files:** `README.md`, new `SECURITY.md`, `config.example.json`, `docs/decisions.md`

- **README:**
  - Sign-up modes and invites, and the `-invite` flag.
  - A `limits` config table with defaults and the operator exemption.
  - The fixed limits: monitor creation 30/h, Check now 1 per 10s per monitor and 30/min per workspace, Test URL 10/min, alert tests 5/min, sign-ups 5/h per address.
  - Restrictions on non-operator alert channels.
  - Account deletion; deleted data remains in backups until you delete them.
  - Slug editing.
  - The User-Agent.
  - The §9.3 startup errors for public instances.
- **SECURITY.md:**
  - Supported versions: the latest release.
  - Private reporting through GitHub security advisories ("Report a vulnerability" on the repository's Security tab).
  - What to include.
  - Expected response: an acknowledgement within 7 days.
  - Scope: the server and its default config. Out of scope: self-hosters' own misconfiguration, such as `allow_private_targets: all` on a public instance.
- **`config.example.json`:** add `"limits": {"max_monitors": 20, "min_interval": "30s", "max_workspaces": 0, "signups_per_hour": 100, "host_requests_per_second": 1}`.
- **D-125:** the rulings above, plus each departure ledgered during execution.
- [ ] **Commit** `docs: invites, limits, channel restrictions, SECURITY.md, D-125`.

---

### Task 10: Step verification

- [ ] **Step 1: CI checks.** Run gofmt, vet, race tests, govulncheck (known findings only) and the Docker build.
- [ ] **Step 2: Smoke test** with an M1d database and `signup: invite`, `base_url: http://127.0.0.1:<port>`:
  - `-invite b@example.com` prints a link.
  - Signing up through it creates a separate workspace with a derived slug, and that workspace can't see the operator's monitors.
  - A non-operator adding `http://127.0.0.1:1/` is blocked.
  - Deleting B's account stops B's monitor.
  - Stop the server with `pkill -TERM -x <name>`.
- [ ] **Step 3: Browser check under CSP** with `playwright-cli` (`mise exec node@24.21.0 -- playwright-cli`). Cover the sign-up page through an invite, the dashboard for B, status settings with a slug change, the account page's delete dialog, and console errors.
- [ ] **Step 4: Opus whole-branch review** of `accounts-m1d..accounts-m1e`.
