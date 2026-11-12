# Accounts M2 (Email) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** System email: the `mail` config and a bounded mail queue, email verification and resend, forgot password, password-change notices, `signup: "open"`, and pruning of accounts left unverified.

**Architecture:** A new `internal/mail` package wraps `notify.Email.Send` with a bounded queue, one worker and budgets per recipient and for the whole instance. The web layer sees it only through a `Mailer` interface whose `Send` charges the budget *before* it runs a compose callback. That callback is where tokens are created, so a dropped message never invalidates an earlier link. New pages are `verify.go` and `forgot.go`. `/verify` and `/reset` move to the public route class. The prune loop deletes non-operator accounts still unverified after 48 hours.

**Tech Stack:** Go 1.26, SQLite (modernc, single connection), `internal/ratelimit`, `notify.Email`, `netguard`, `html/template`.

**Spec:** `docs/superpowers/specs/2026-11-10-accounts-design.md`. The parts M2 implements are §5.2 (open sign-up), §5.4 (the unverified 403), §5.5 (the password-change notice), §5.6 (verify tokens), §5.7, §5.8, §6.7 (pruning), §8.2 (the M2 rows), §9.1 (`mail`, `limits.mail_per_hour`), §9.3, §12 (`mail_dropped`) and §13 (enumeration).

**Branch:** `accounts-m2`, cut from the tip of `accounts-m1e` after M1e's fix wave. Never push. Never merge into main.

## Global Constraints

- Every POST keeps `sameOrigin` and the 1 MB body limit. Token pages (`GET`) never change state and send `Cache-Control: no-store`, as do their POSTs.
- **Order of checks (§8.3):** rate limits, then field validation, then token validity, then password hashing, then the transaction.
- **Mail budgets:**
  - **Per recipient:** 3 per hour and 10 per day, shared by every system email.
  - **Duplicate sign-up notice:** 1 per 24 hours per account.
  - **Forgot and resend:** 10 per hour per client address, one shared limiter.
  - **Whole instance:** `limits.mail_per_hour` (default 500). When a budget is reached, the message is dropped with a WARN `event=mail_dropped`.
- **Expiry:** verify tokens 24 hours, reset tokens 1 hour. Unverified accounts are pruned after 48 hours.
- **Fixed messages, exact text:**
  - `msgCheckEmail = "Check your email to finish signing up."`
  - `msgResendSent = "If an unverified account exists for that address, we sent a new link."`
  - `msgForgotSent = "If an account exists for that address, we sent a reset link."`
  - `msgUnverified = "Verify your email to sign in."`
  - `msgVerifiedSignIn = "Email verified. Sign in to continue."`
  - `msgBadLink` already exists in `reset.go`: "This link is invalid or has expired."
- **Email contents:** no user-supplied text in system emails except the link. Links are built from `cfg.BaseURL`, never from the Host header.
- **Logs:** never log passwords, tokens, cookies, session IDs or full links. Unknown emails are logged as `email_hash` (the existing `emailHash` helper).
- **Config:** reject anything unsupported with a clear message. Every departure from the spec gets a superseding entry in `docs/decisions.md` (D-126).
- **Tests:** all run under `go test -race ./...`. Mail is tested with a recording fake that implements `web.Mailer`. Real SMTP stays covered by `internal/notify`.

## Review Focus

1. **A mail budget is exhausted when `/forgot` or resend is posted again.**
   - Expected: no new token is created, so the link the user already received keeps working.
   - Tests: Task 2 (`TestSendDroppedSkipsCompose`) and Task 5 (`TestForgotDroppedKeepsOldLink`).
2. **Open sign-up with a duplicate email, including when the workspace cap is reached.**
   - Expected: new and existing emails give an identical status, body and `Set-Cookie`, and charge the same limiter slots.
   - Test: Task 4 (`TestOpenSignupIdenticalResponses`, which includes the at-cap case).
3. **A browser signed in as B opens A's verification link.**
   - Expected: A becomes verified, B's session is untouched, and nobody is signed in as A.
   - Test: Task 4 (`TestVerifyOtherUsersSession`).
4. **The SMTP server is down or hangs.**
   - Expected: requests answer at once, a full queue drops messages with a WARN, and the worker stops on shutdown.
   - Test: Task 2 (`TestQueueFullDrops`, `TestRunStopsOnCancel`).
5. **`mail` is removed from the config while unverified accounts still exist.**
   - Expected: sign-in gives 403 `msgUnverified` without a resend form; `/forgot` and `/verify/resend` return 404; nothing dereferences a nil mailer.
   - Test: Task 4 (`TestUnverifiedLoginWithoutMail`) and Task 5 (`TestForgotWithoutMail404`).

---

### Task 1: `mail` config, `signup: "open"`, `limits.mail_per_hour`

**Files:**
- Modify: `internal/config/config.go`. Change the `Mail` field (line ~101), `Limits.MailPerHour` (~113), the signup and mail rejections (~249–264), the email notifier validation (~326–370), the `mail_per_hour` rejection (~413) and the fixed default (~448).
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces:
  ```go
  // Mail is the system SMTP account used for verification, reset and notice emails.
  type Mail struct {
      SMTPHost   string `json:"smtp_host"`
      SMTPPort   int    `json:"smtp_port"`   // default 587
      Username   string `json:"username"`
      Password   string `json:"password"`
      From       string `json:"from"`
      RequireTLS *bool  `json:"require_tls"` // default true
  }
  // Config.Mail becomes *Mail (nil = no system mail).
  // Limits.MailPerHour *int: default 500, must be >= 1.
  ```
  Validate `mail` inline in the config's validation, with no shared helper. **Do not touch `ValidateNotify`**, because tightening it could stop existing configs from starting.

- [ ] **Step 1: Write the failing tests** in `config_test.go`:
  - `TestMailDefaults`: `{"mail":{"smtp_host":"smtp.example.com","from":"up@example.com"},"base_url":"https://up.example.com"}` loads with `SMTPPort == 587`, `*RequireTLS == true` and `*Limits.MailPerHour == 500`.
  - `TestMailRequireTLSFalse`: `"require_tls": false` is kept as false.
  - `TestMailValidation`: each of these is rejected:
    - a missing `smtp_host` or `from`
    - a port of -1 or 70000 (0 means omitted and defaults to 587)
    - an unparsable `from`
    - a `from` with a display name such as `"Uptime <up@example.com>"`, because `from` must equal `ParseAddress(from).Address` since it is used as `MAIL FROM`
  - `TestMailNeedsBaseURL`: `mail` without `base_url` gives an error that mentions `base_url`.
  - `TestOpenNeedsMail`: `signup: "open"` without `mail` gives an error that mentions `mail`. With `mail` and an https `base_url`, it loads.
  - `TestMailPerHour`: `limits.mail_per_hour: 0` is rejected, and 50 is kept.
  - Edit the existing table rows that assert "not supported yet" for `mail`, `signup: open` and `mail_per_hour` (`config_test.go` ~124, ~178 and ~228). `"bogus"` must stay rejected, with the message `signup must be "closed", "invite" or "open"`.
- [ ] **Step 2: Run them and confirm they fail:** `go test ./internal/config/ -run 'Mail|Open'`. They should FAIL.
- [ ] **Step 3: Implement.**
  - Accept `signup` values `closed`, `invite` and `open`. The existing `base_url` checks apply to `open` as well.
  - Apply the mail defaults in `applyDefaults` through the pointer pattern.
  - Update the field comments at `config.go` ~100 and ~113.
  - Delete `isSet` if nothing uses it any more.
- [ ] **Step 4: Run the tests and confirm they pass:** `go test ./internal/config/ && go build ./...`.
- [ ] **Step 5: Commit** `feat(config): mail block, signup open, limits.mail_per_hour`.

---

### Task 2: `internal/mail` queue

**Files:**
- Create: `internal/mail/mail.go`, `internal/mail/mail_test.go`

**Interfaces:**
- Consumes: `ratelimit.New(window, max, maxKeys)` with `Take` and `Undo`.
- Produces:
  ```go
  package mail
  // Sender delivers one message; notify.Email satisfies it.
  type Sender interface {
      Send(ctx context.Context, to []string, subject, body string) error
  }
  type Options struct {
      Sender      Sender
      PerHour     int              // instance budget (limits.mail_per_hour)
      Logger      *slog.Logger     // nil = slog.Default()
      Now       func() time.Time // nil = time.Now; set as the limiters' Now
      QueueSize int              // 0 = 100
  }
  func New(o Options) *Queue
  // Send checks the queue has room and the recipient budgets (3/hour,
  // 10/day) and instance budget allow it, then calls compose, then records
  // the budgets and queues the result without blocking. When the queue is
  // full or a budget is spent, the message is dropped with a WARN
  // (event=mail_dropped, reason, email_hash), and compose is NOT called, so
  // no token is created. Send returns compose's error (nothing is charged
  // then), nil otherwise. A dropped message is not an error.
  func (q *Queue) Send(to string, compose func() (subject, body string, err error)) error
  // Notice queues a fixed notice (the password-change email). It is charged
  // only to the instance budget, so /forgot spam cannot use up the recipient
  // budget and suppress it (D-126).
  func (q *Queue) Notice(to, subject, body string)
  // Run delivers queued messages one at a time until ctx is cancelled.
  // Delivery errors are logged as WARN with the error class only, never the
  // error text (SMTP replies often echo the address) or the recipient.
  func (q *Queue) Run(ctx context.Context)
  ```
  - Use one mutex around `Send` and `Notice`: check `len(ch) < cap(ch)`, then `Allow` on each limiter, then compose, then `Hit` the limiters and push. No `Undo` is needed.
  - `notify.Email.send` already applies a 30-second timeout, so there is no per-message timeout option.

- [ ] **Step 1: Write the failing tests:**
  - `TestSendQueuesAndRunDelivers`: a fake sender records calls. After `Send` and `Run`, there is exactly one delivery with `to == []string{addr}` and the subject and body from compose.
  - `TestRecipientBudget`: 3 sends to one address in an hour reach compose and the 4th does not (compose counter == 3). After the fake clock advances 61 minutes, a send is accepted again, but after 10 in a day, the 11th is dropped even across hours.
  - `TestInstanceBudget`: with `PerHour: 2`, three different recipients give two composes. The dropped third does not use up its recipient's budget: after the clock advances an hour, it gets 3 again.
  - `TestSendDroppedSkipsCompose`: when the budget is spent, compose is never called, and the log has `mail_dropped`. Capture slog output with a `bytes.Buffer` JSON handler and assert it does not contain the address.
  - `TestQueueFullDrops`: `QueueSize: 1` with a sender that blocks and signals on an "entered" channel when it starts.
    - Wait for the first message to enter the sender.
    - The second message fills the slot.
    - The third is dropped without calling compose.
  - `TestComposeErrorCharged`: compose returns an error and `Send` returns it. Nothing is charged, so 3 more sends to that address still succeed.
  - `TestNoticeSkipsRecipientBudget`: after 3 `Send`s to an address, a `Notice` to it is still delivered.
  - `TestRunLogsNoAddress`: the sender fails with an error whose text contains the address. The log has `mail send failed`, and the address appears nowhere in it.
  - `TestRunStopsOnCancel`: `Run` returns within 1 second of `cancel()`, even while the sender blocks on its ctx.
- [ ] **Step 2: Run them and confirm they fail:** `go test ./internal/mail/`. They should FAIL to compile.
- [ ] **Step 3: Implement** `mail.go`, using about 120 lines.
  - Keep the limiters keyed by the normalised address the caller passes.
  - Use an `emailHash` equivalent: the first 16 hex characters of the SHA-256, defined locally. Don't import web.
- [ ] **Step 4: Run the tests and confirm they pass:** `go test -race ./internal/mail/`.
- [ ] **Step 5: Commit** `feat(mail): bounded system mail queue with recipient and instance budgets`.

---

### Task 3: Store: verify tokens, `VerifyEmail`, `PruneUnverified`

**Files:**
- Modify: `internal/store/tokens.go` (the `TokenKind` consts at ~47, `tokenLive` at ~65 and `CreateToken` at ~71), `internal/store/delete.go` (extract the shared deletion) and `internal/store/workspace.go` (`WorkspaceBySlug` at ~232).
- Test: `internal/store/accounts_test.go`

**Interfaces:**
- Produces:
  ```go
  const TokenVerify TokenKind = "verify" // needs a user; 24h
  // tokenLive: the not-disabled rule covers kind IN ('reset','verify').
  // VerifyEmail consumes a verify token and sets verified_at in one transaction.
  // ErrNotFound when the token is unknown, used, expired, of another kind or
  // belongs to a disabled user.
  func (s *Store) VerifyEmail(ctx context.Context, raw string, now time.Time) (User, error)
  // WorkspaceBySlug returns ErrNotFound when no owner of the workspace is
  // verified (§5.2: an unverified account's status page is 404).
  // PruneUnverified deletes non-operator users with verified_at NULL and
  // created_at < cutoff, each with the workspaces where they are the only
  // member, one transaction per user. The unverified condition is re-checked
  // inside each transaction. It returns the deleted workspace IDs.
  func (s *Store) PruneUnverified(ctx context.Context, cutoff time.Time) ([]int64, error)
  ```
  - `DeleteAccount`'s body becomes `deleteAccount(ctx, tx querier, userID int64) ([]int64, error)`. `DeleteAccount` and `PruneUnverified` both call it. There is no second copy of the deletion SQL.
  - `CreateAccount` already checks `MaxWorkspaces` before inserting the user (`users.go` ~262–270), so a duplicate email at the cap returns `ErrWorkspaceCap`. Task 4 relies on this. Pin it with a regression test, which passes at Step 2.

- [ ] **Step 1: Write the failing tests:**
  - `TestVerifyToken`:
    - `CreateToken(TokenVerify, uid, 24h)` then `VerifyEmail` returns the user with `VerifiedAt != nil`.
    - A second `VerifyEmail` with the same token returns `ErrNotFound`.
    - An expired token (now + 25h) returns `ErrNotFound`.
    - A reset token passed to `VerifyEmail` returns `ErrNotFound`.
    - `CreateToken(TokenVerify, 0, …)` returns an error.
  - `TestVerifyTokenDisabledUser`: set `disabled_at` directly in SQL. Then `PeekToken(TokenVerify)` and `VerifyEmail` both return `ErrNotFound`.
  - `TestNewVerifyTokenReplacesOld`: a second `CreateToken(TokenVerify, uid)` makes the first token fail.
  - `TestSetPasswordDeletesVerifyTokens`: confirms the existing behaviour, so no code change.
  - `TestPruneUnverified`:
    - Set up an unverified user created 49h ago, with a workspace and a target; an unverified user created 1h ago; a verified user created 49h ago; and an unverified **operator** created 49h ago (insert it directly).
    - Prune with cutoff now−48h.
    - Only the first user and their workspace are gone, and the returned IDs equal that workspace's ID.
    - Checks, targets and memberships for that workspace are gone.
  - `TestCreateAccountCapBeforeDuplicate`: with `MaxWorkspaces` equal to the current count, `CreateAccount` with an existing email returns `ErrWorkspaceCap`.
  - `TestWorkspaceBySlugUnverified`: an unverified owner's slug returns `ErrNotFound`. After `VerifyEmail`, the slug is found. Workspace 1 stays found.
- [ ] **Step 2: Run them and confirm they fail:** `go test ./internal/store/ -run 'Verify|Prune|CapBefore'`. They should FAIL.
- [ ] **Step 3: Implement** as specified in the Interfaces block.
- [ ] **Step 4: Run the tests and confirm they pass:** `go test -race ./internal/store/`.
- [ ] **Step 5: Commit** `feat(store): verify tokens, VerifyEmail, PruneUnverified`.

---

### Task 4: Web: mailer wiring, open sign-up, `/verify`, resend, unverified sign-in

**Files:**
- Create: `internal/web/verify.go`, `internal/web/mailtext.go`, `internal/web/verify_test.go`, `internal/web/templates/verify.html`
- Modify:
  - `internal/web/web.go`: `Options.Mail`, `Server.mail`, the `mailByAddr` and `dupNotice` limiters
  - `internal/web/signup.go`: the open-mode branch; replace the `Signup != "invite"` checks at lines 37 and 64
  - `internal/web/login.go`: the unverified 403; `safeNext` gains `/verify` and `/forgot`
  - `internal/web/routes.go`
  - `internal/web/static_test.go`: `sysAllowed`
  - `internal/web/templates/login.html`: the resend form and the "Create an account" link
  - `internal/web/templates/signup.html`
  - `internal/web/web_test.go`: the fake mailer and `newEnvMail`
  - `internal/web/routes_test.go`: if it lists the route classes

**Interfaces:**
- Consumes:
  - `mail.Queue.Send` from Task 2, through this interface.
  - `store.TokenVerify`, `VerifyEmail` and `UserByID` from Task 3.
  - The open-mode config from Task 1.
- Produces:
  ```go
  // web.go
  // Mailer queues system email. Send charges the budgets first and calls
  // compose only when the message will be sent (see internal/mail).
  type Mailer interface {
      Send(to string, compose func() (subject, body string, err error)) error
      Notice(to, subject, body string)
  }
  // Options.Mail Mailer: nil = no system mail. New returns an error when
  // cfg.Signup == "open" and Options.Mail == nil.
  // Server.mail Mailer
  // s.mailByAddr = ratelimit.New(time.Hour, 10, 0); s.mailByAddr.Now = now   // forgot + resend, per client address
  // s.dupNotice  = ratelimit.New(24*time.Hour, 1, 0); s.dupNotice.Now = now  // keyed by strconv of user ID
  // compose callbacks create tokens on context.WithoutCancel(r.Context()),
  // so a client disconnect cannot leave an account without its mail.

  // mailtext.go: the only place email copy lives
  func verifyMail(link string) (subject, body string)
  func duplicateMail(base string) (subject, body string)
  func resetMail(link string) (subject, body string)
  func passwordChangedMail(base string) (subject, body string)
  // s.sendVerify(ctx, u store.User) error: compose creates the TokenVerify (24h) and the link <base_url>/verify?token=<raw>
  ```
- **Email copy** (exact; `%s` is the link, or `cfg.BaseURL`):
  - **verify:** subject `Confirm your email`. Body: `Open this link to finish signing up. It is valid for 24 hours.\n\n%s\n\nIf you did not sign up, ignore this email.\n`
  - **duplicate:** subject `Sign-up attempt with your address`. Body: `Someone tried to sign up with your email address. If it was you, sign in or reset your password:\n\n%s/login\n%s/forgot\n`
  - **reset:** subject `Reset your password`. Body: `Open this link to choose a new password. It is valid for 1 hour.\n\n%s\n\nIf you did not ask for this, ignore this email.\n`
  - **changed:** subject `Your password was changed`. Body: `The password for your account was just changed. If this was not you, reset it now:\n\n%s/forgot\n`
- **Routes:**
  - `GET /verify` and `POST /verify`: `classPublic`, not guest, because a signed-in browser must still reach them.
  - `POST /verify/resend`: `classPublic`.
- **Behaviour:**
  - **`signupPage`:**
    - `closed` without an invite: 404.
    - `invite` without an invite: the hint.
    - `open` without an invite: the form.
    - Invite links work in every mode (unchanged).
  - **Open mode in the template and handler:**
    - Add `signupData.Open bool`. `signup.html` shows the form when `.Invite` or `.Open` is set and there is no `.Hint`.
    - In `signupPost`, skip the invite peek entirely when `raw == ""` and the mode is open. In open mode a validation failure keeps the address slot charged, like any other attempt.
  - **`signupPost` without an invite in open mode,** in order:
    1. The per-address limit.
    2. Field validation.
    3. The instance limit (`signupGlobal`).
    4. **Always** hash.
    5. `CreateAccount{Verified: false, MaxWorkspaces}`.
    - **Outcomes:**
      - **Success:** `sendVerify`, then log `signup`.
      - **`ErrEmailTaken`:** log `signup_duplicate`. Look up the owner with `UserByEmail`. If the owner is not disabled and `dupNotice.Take(id)` succeeds, send `duplicateMail`.
      - **`ErrWorkspaceCap`:** 403 `msgNoNewAccounts`, the same for new and existing emails (Task 3 order).
    - **Response:** both the success and the duplicate case answer 200 with the `signup` page showing `Hint: msgCheckEmail`. No cookie is set, and neither case refunds a limiter slot. Mail errors from compose are logged and never change the response.
  - **`verifyPage` (GET):**
    - Peek the token. If it is valid, render a "Confirm your email" button form with the token hidden.
    - Otherwise render `msgBadLink`, with the resend form when `s.mail != nil`, status 404.
  - **`verifyPost`:**
    1. `VerifyEmail`. On `ErrNotFound`, render as the GET's bad-link case.
    2. If the browser has no valid session (`s.currentAuth(r)`), or a session for the same user: delete the incoming session if any, create a new session, set the cookie, and send 303 to `/`. The same-user case can't happen today, because unverified users hold no valid session. Keep the comparison but don't test it.
    3. If the session belongs to a different user: render the `verify` page 200 with `msgVerifiedSignIn`, and leave the cookie and session alone.
  - **`verifyResend`:**
    - 404 when `s.mail == nil`.
    - Over `mailByAddr`: 429 with `msgTooMany`.
    - If the email normalises and its user exists, is unverified and is not disabled, call `sendVerify`.
    - Always 200 `msgResendSent`, byte-identical in every case.
  - **`loginPost`:** after a correct password, an unverified user gets 403 `msgUnverified`. The page includes a resend form prefilled with the email only when `s.mail != nil`. Today this case returns 401 `msgBadLogin`.
  - **`login.html`:**
    - The "Create an account" link to `/signup` appears only when `Signup == "open"`.
    - The "Forgot password?" link to `/forgot` appears only when mail is configured. Add `Mail bool` to `loginData`, and use the existing `SignupMode` (`login.go` ~118) for open mode.
  - Register the `"verify"` template in the public page list (`web.go` ~187).
  - The route-count tests (`static_test.go` ~119 `want = 43`, `routes_test.go` ~17) become 46. `/verify` (GET and POST) and `/verify/resend` go in `routes_test.go`'s `classPublic` list (~50–53).
- **`sysAllowed` additions** (the static test is `TestSysAllowList`; it also fails on unused entries):
  - `signupPost`: `UserByEmail`, `CreateToken`
  - `verifyPage`: `PeekToken`
  - `verifyPost`: `VerifyEmail`, `DeleteSession`, `CreateSession`
  - `verifyResend`: `UserByEmail`, `CreateToken`
  - `sendVerify`: `CreateToken`
  - Use whatever split into functions you actually write, keeping each list minimal.
- **Test env:**
  - `fakeMailer{mu sync.Mutex; sent []sentMail; drop bool}`, where `sentMail{To, Subject, Body string}`.
    - `Send` returns nil without calling compose when `drop` is set; otherwise it calls compose and records the result.
    - `Notice` records the message.
  - Add an `env.mail *fakeMailer` field, and give `newEnvConfig` (`web_test.go` ~60, which builds `Options` inline) an options hook, or add a sibling constructor. `newEnvMail(t, cfgJSON string) *env` then passes `Options.Mail: e.mail`.
  - `linkFrom(body string) string` extracts the token URL.
  - Use `e.clientFrom` (`signup_test.go` ~181) for distinct client addresses.
  - **Byte-identical comparisons:** there are no CSRF or nonce values. The check-email, resend and forgot pages must never echo the submitted address. The at-cap 403 re-renders the form with the email, so replace the email before comparing, as `accounts_test.go` ~449 does.

- [ ] **Step 1: Write the failing tests** in `verify_test.go`, with config `{"signup":"open","base_url":"http://127.0.0.1","mail":{"smtp_host":"x.example","from":"up@example.com"}}`:
  - `TestOpenSignupSendsVerify`: the POST answers 200 with `msgCheckEmail` and no session cookie. The fake has one message to the address whose link contains `/verify?token=`. Signing in now gives 403 `msgUnverified`.
  - `TestVerifyGetChangesNothing`: GET the link twice, and the user is still unverified.
  - `TestVerifyPostSignsIn`: POST the token and get 303 to `/`. The session works, and the user is verified. A second POST gets 404 and `msgBadLink`.
  - `TestVerifyOtherUsersSession`: B is signed in and POSTs A's token. A is verified, the response is 200 with `msgVerifiedSignIn`, B's client still reaches `/` as B, and no new cookie is set.
  - `TestOpenSignupIdenticalResponses`:
    - Sign up `new@x.example` and an existing email. The status and body are byte-identical, after replacing the CSRF/nonce values if the page has any; reuse how the M1d enumeration tests normalise bodies. Neither response sets a `Set-Cookie` session.
    - With `limits.max_workspaces` at the current count, both emails get an identical 403.
  - `TestDuplicateNoticeOncePerDay`: two duplicate sign-ups for one account give one notice mail. After `e.advance(25*time.Hour)`, one more is sent.
  - `TestResendIdentical`:
    - Use an unknown email, a verified email, an unverified email and a disabled one.
    - All four answer byte-identical 200 bodies.
    - Only the unverified one produces mail.
    - The 11th request from one address gets 429.
  - `TestUnverifiedLoginWithoutMail`: in the env without mail, insert an unverified user. Sign-in answers 403 `msgUnverified`, and the body has no resend form. `POST /verify/resend` returns 404.
  - `TestOpenSignupForm`: in the mail env, `GET /signup` without an invite shows the form. The existing `TestSignupModes` (`signup_test.go` ~35) still passes unchanged.
  - `TestOpenWithoutMailerRefused`: `New` with `signup: "open"` and no `Options.Mail` returns an error.
  - `TestUnverifiedStatusPage404`: after open sign-up, `/s/<slug>` returns 404. After verification, it returns 200.
- [ ] **Step 2: Run them and confirm they fail:** `go test ./internal/web/ -run 'Verify|OpenSignup|Duplicate|Resend|Unverified|SignupModes'`. They should FAIL.
- [ ] **Step 3: Implement** as specified above. Reuse `s.page`, `s.renderStatus`, `clientIP`/`clientKey`, `emailHash`, and the existing `public.html` classes. Add no new CSS. Add no inline handlers.
- [ ] **Step 4: Run the tests and confirm they pass:** `go test -race ./internal/web/`. That includes `TestSysAllowList` and the route-class test.
- [ ] **Step 5: Commit** `feat(web): open sign-up, email verification and resend`.

---

### Task 5: Web: `/forgot`, public `/reset`, password-change notices

**Files:**
- Create: `internal/web/forgot.go`, `internal/web/forgot_test.go`, `internal/web/templates/forgot.html`
- Modify:
  - `internal/web/reset.go`: the session handling after reset; send the notice
  - `internal/web/account.go`: send the notice after `SetPassword`
  - `internal/web/routes.go`: `/reset` becomes `classPublic`; add `/forgot` as `classGuest`
  - `internal/web/routes_test.go` and `internal/web/static_test.go`: the counts become 48; move `/reset` to `classPublic` and add `/forgot` to `classGuest`; add the `sysAllowed` entry `forgot.go|(*Server).forgotPost: UserByEmail, CreateToken`
  - `internal/web/web.go`: register the `"forgot"` template
  - `docs/decisions.md`: D-126 starts here; Task 7 completes it

**Interfaces:**
- Consumes: `Mailer` (`Send` and `Notice`), `mailByAddr`, `resetMail` and `passwordChangedMail` (Task 4); `store.TokenReset`.
- **Behaviour:**
  - **`GET /forgot`:** 404 when `s.mail == nil`; otherwise the email form.
  - **`POST /forgot`:**
    - 404 without mail. Over `mailByAddr`: 429 `msgTooMany`.
    - If the email normalises, the user exists, and the user is not disabled (unverified users are included), call `s.mail.Send(email, compose)`. compose runs `CreateToken(TokenReset, id, time.Hour)` and builds `resetMail(base+"/reset?token="+raw)`.
    - Always 200 `msgForgotSent`, byte-identical in every case.
  - **`resetPost`, after a successful `ResetPassword`:**
    - If the request carries a valid session for a **different** user (`s.currentAuth(r)`, looked up before the reset), leave that session and cookie alone. Render the `reset` page 200 with `msgPasswordChangedSignIn = "Password changed. Sign in to continue."`. This mirrors §5.7.
    - Otherwise sign in as today.
    - In both cases log `password_reset` and call `s.mail.Notice(email, passwordChangedMail(base))` when `s.mail != nil`.
    - Not signing in departs from §5.8 and gets a D-126 entry.
  - **`resetPage` and `resetPost`** are now `classPublic`. A signed-in user opening a reset link sees the form and is no longer redirected to `/`.
  - **`accountPassword`:** after `SetPassword` succeeds, call `s.mail.Notice(a.User.Email, passwordChangedMail(base))` when `s.mail != nil`.
  - **`login.html`:** the "Forgot password?" link from Task 4 now leads to a working page.

- [ ] **Step 1: Write the failing tests** in `forgot_test.go`:
  - `TestForgotSendsResetLink`: POST `/forgot` for a verified user gives 200 `msgForgotSent` and one mail whose link contains `/reset?token=`. Opening the link and POSTing a new password works.
  - `TestForgotIdentical`:
    - Use an unknown, a verified, an unverified and a disabled email.
    - All four bodies are byte-identical.
    - Mail goes only to the verified and unverified users.
    - Resetting the unverified user's password verifies them, and they can sign in.
  - `TestForgotRateLimit`: from one address, the 11th request gets 429. The limit is shared with `/verify/resend`, so 6 forgot requests plus 5 resend requests give the 11th a 429.
  - `TestForgotDroppedKeepsOldLink`: request a link, then set `e.mail.drop = true` and request again. The first link still works. This is Review Focus 1.
  - `TestForgotWithoutMail404`: in the env without mail, GET and POST `/forgot` both return 404.
  - `TestResetOtherUsersSession`:
    - B is signed in and POSTs A's reset link.
    - A's password changes, and the response is 200 with `msgPasswordChangedSignIn`.
    - No `Set-Cookie` session is sent, and the client still reaches `/` as B.
  - `TestResetSignedInSameUser`: the same user's session is replaced by a new one, and the flow redirects 303 to `/`.
  - `TestPasswordChangeNotice`: changing the password on `/account` sends one "Your password was changed" mail. A reset sends one too.
- [ ] **Step 2: Run them and confirm they fail:** `go test ./internal/web/ -run 'Forgot|Reset|PasswordChangeNotice'`. They should FAIL.
- [ ] **Step 3: Implement** as specified above, and write the D-126 bullets for the reset ruling and the public class.
- [ ] **Step 4: Run the tests and confirm they pass:** `go test -race ./internal/web/`.
- [ ] **Step 5: Commit** `feat(web): forgot password, public reset, password-change notices`.

---

### Task 6: `main` wiring and pruning unverified accounts

**Files:**
- Modify: `main.go` (`run` at ~137, `pruneLoop` at ~423)
- Test: `main_test.go` (create it if missing)

**Interfaces:**
- Consumes: `mail.New` and `mail.Queue.Run` (Task 2); `store.PruneUnverified` (Task 3); `web.Options.Mail` (Task 4).
- **Behaviour:**
  - Add `func newMailer(ctx context.Context, cfg *config.Config, logger *slog.Logger) web.Mailer`.
    - It returns a literal `nil` when `cfg.Mail == nil`.
    - Otherwise it builds the sender `notify.Email{…from cfg.Mail…, RequireTLS: *cfg.Mail.RequireTLS, Dial: netguard.Dialer(true, 30*time.Second).DialContext}`. The `true` says the operator chose this host; it is ruled in D-126.
    - It builds the queue with `mail.New(mail.Options{Sender: sender, PerHour: *cfg.Limits.MailPerHour, Logger: logger})`, starts `go q.Run(ctx)`, and returns the queue.
    - `run` passes the result as `Options.Mail`.
  - `pruneLoop` takes the monitor too. Each hour it calls `st.PruneUnverified(ctx, now.Add(-48*time.Hour))` and then `mon.ForgetWorkspace(id)` for each returned ID, and logs `INFO` with `event=account_pruned` and the count. Errors are logged like the other prune steps.
  - Startup logs one INFO saying whether system mail is on, with the host but not the username or password.

- [ ] **Step 1: Write the failing test** `TestNewMailer`:
  - Without `mail`, `newMailer(...) == nil` holds as an interface comparison.
  - With `mail`, the result is non-nil.
- [ ] **Step 2: Run it and confirm it fails:** `go test . -run NewMailer`. It should FAIL.
- [ ] **Step 3: Implement** as specified above.
- [ ] **Step 4: Verify** with `go build ./... && go vet ./... && go test -race ./...`. All must PASS.
- [ ] **Step 5: Commit** `feat: wire system mail and prune unverified accounts`.

---

### Task 7: Docs, D-126 and the enumeration timing test

**Files:**
- Modify: `README.md`, `docs/decisions.md`, `config.example.json`
- Test: `internal/web/verify_test.go` (the timing test)

- [ ] **Step 1: Write the timing test.**
  - `TestEnumerationTiming`: for `/forgot`, `/verify/resend` and open `/signup`, send 30 requests with an existing email and 30 with an unknown one, then compare the medians.
  - The difference must be under 50 ms.
  - Use `newEnvConfig(t, password.Default(), …)` with the real hasher (as `accounts_test.go` ~434 does). Otherwise a missing "always hash" passes trivially.
  - Call `e.advance(25*time.Hour)` each round, so the duplicate-notice path stays cold every round.
  - For resend, the "existing" email is an unverified user.
  - Use `e.clientFrom` with separate addresses so rate limits don't trip.
  - Skip under `testing.Short()`.
- [ ] **Step 2: Run it:** `go test -race ./internal/web/ -run EnumerationTiming`. It should PASS. If it fails, fix the handler path that differs; don't loosen the bound.
- [ ] **Step 3: Write the docs.**
  - **README:**
    - A "System email" section covering the `mail` keys and defaults, the 587/465 ports, and `require_tls` on by default.
    - `signup: "open"` needs `mail` and `base_url`.
    - Verification, the 48-hour pruning, forgot password, and the password-change notice.
    - The budget table: 3 per hour and 10 per day per recipient; 10 per hour per address for forgot and resend; 1 duplicate notice per 24 hours; `mail_per_hour`.
    - Without mail, `/forgot` is unavailable and the operator uses `-reset-password`.
  - **`config.example.json`:** add `"mail_per_hour": 500` to `limits`. Leave `mail` out, since it needs real SMTP, and mention it in the README.
  - **D-126**, one bullet each:
    - Budgets are charged before the token is created (compose callback).
    - The reset ruling for a different user's session (not signed in, mirroring §5.7).
    - The password-change notice is charged only to the instance budget, so `/forgot` spam can't suppress it.
    - Unverified owners' status pages return 404.
    - `/verify` and `/reset` are in the public route class.
    - Without mail, `/forgot` and `/verify/resend` return 404 and sign-in shows no resend form.
    - The system mail dialer allows private addresses because the operator chose the host.
    - Open sign-up at the workspace cap answers 403 for every email.
    - Pruning skips operators.
- [ ] **Step 4: Verify** with `gofmt -l .` (expect empty output) and `go test -race ./...` (expect PASS).
- [ ] **Step 5: Commit** `docs: system email, open sign-up, D-126`.

---

### Task 8: Step verification

- [ ] **Step 1: CI checks.**
  - Run gofmt, vet, `go test -race -count=1 ./...` and the Docker build.
  - Run govulncheck with a patched toolchain; the standard-library findings are toolchain-only.
- [ ] **Step 2: Smoke test** with `signup: "open"`, `base_url: http://127.0.0.1:<port>`, and `mail` pointed at a local SMTP sink.
  - Use `smtp_port` 2525 and `require_tls: false`.
  - For the sink, run a tiny Go program in the scratchpad that runs `net/smtp`'s server side, or a Python `aiosmtpd`.
  - Sign up, receive the link, verify, and sign in. Run forgot and reset. Confirm a duplicate sign-up sends the notice.
  - Stop the server with `pkill -TERM -x <name>`.
- [ ] **Step 3: Browser check** with `mise exec node@24.21.0 -- playwright-cli`.
  - Cover the open sign-up form, the verify confirmation page, the forgot page, the reset form while signed in as another user, and the login page's links and resend form.
  - Check for console and CSP errors.
- [ ] **Step 4: Whole-branch review** of `accounts-m1e..accounts-m2` by Fable, then one fix wave.
