# Settings page, verified first password, and two-factor authentication

Status: draft for review (revised after a security review). Targets v1.6.0
(Part A) and v1.7.0 (Part B).

## 1. Goals

- One place for personal and security settings, reachable from the sidebar.
- A person who signed up with Google or GitHub proves control of their email
  inbox before they can add a password to the account.
- Optional two-factor authentication (2FA) with an authenticator app, asked on
  every sign-in method, with recovery codes and an operator reset.

Not in scope: passkeys/WebAuthn, "remember this device", forcing 2FA on
anyone, changing the account email, a per-device session list. Each can come
later without changing what is built here.

Accepted risks, stated plainly:
- On an install without `mail`, the operator panel can show a password-reset
  link for any non-operator account, and (Part B) can reset its 2FA. A
  compromised operator account can therefore take over non-operator accounts.
- 2FA secrets are stored unencrypted in the database and are in every backup
  (§6).
- A hard daily limit on wrong codes (§8.4) lets someone who knows a user's
  password block that user's sign-ins for up to a day; the user is told to
  change their password.

## 2. Decisions (agreed 2026-11-17)

| Topic | Decision |
|---|---|
| Settings location | One Settings page, opened from a gear item at the bottom of the sidebar. Alert channels move into it. |
| First password for Google/GitHub accounts | Emailed one-time link (1 hour) to the account address; the password is chosen on that page. |
| 2FA scope | Asked after every sign-in method: password, Google and GitHub. |
| 2FA extras in the first version | Operator can reset a user's 2FA (panel and command line). Recovery codes and a QR code are always included. |

Constraints carried over: Go stdlib only, no new modules; ES5 JS with no
inline scripts (CSP); every POST behind `sameOrigin`; secrets never logged;
copy in the existing tone; tests with `-race` on real SQLite.

---

# Part A — Settings page and verified first password (v1.6.0)

No schema change in Part A.

## 3. Settings page

### 3.1 Navigation

- Sidebar foot gets a **Settings** item (gear icon) above the theme, email and
  Sign out row. It is active on every `/settings/...` page.
- **Alert channels** leaves the main nav. The nav becomes: Overview, Monitors,
  Incidents, Maintenance, Status page, Admin (operators).
- The email text in the sidebar foot stops being a link.
- Footer "Times in IST (Asia/Kolkata)" links to `/settings/profile#timezone`.

### 3.2 Pages

`GET /settings` redirects (303) to `/settings/profile`. Each section is its
own page with a shared sub-navigation: a vertical list beside the content at
900px and wider, wrapping tabs above it on narrower screens (reusing the tab
component and its responsive rules).

| Page | Route | Class | Contents |
|---|---|---|---|
| Profile | `/settings/profile` | user | Email (read-only), Time zone (moved from Account) |
| Security | `/settings/security` | user | Password, Two-factor (Part B), Sign-in methods, Sessions ("Sign out everywhere") |
| Notifications | `/settings/notifications` | workspace | The Alert channels page body, unchanged in behaviour |
| Danger zone | `/settings/danger` | user | Delete account. Operators cannot delete their account here, so for them the page redirects (302) to Profile and the sub-nav omits it. |

POST routes move with their cards:

| Old | New |
|---|---|
| `POST /account/timezone` | `POST /settings/profile/timezone` |
| `POST /account/tz-detected` | `POST /settings/tz-detected` |
| `POST /account/password` | `POST /settings/security/password` |
| `POST /account/signout-all` | `POST /settings/security/signout-all` |
| `POST /account/identities/{p}/connect` | `POST /settings/security/identities/{p}/connect` |
| `POST /account/identities/{p}/delete` | `POST /settings/security/identities/{p}/delete` |
| `POST /account/delete` | `POST /settings/danger/delete` |
| `POST /alerts`, `POST /alerts/test` | `POST /settings/notifications`, `.../test` |

### 3.3 Old links

- `GET /account` → 302 `/settings/security`; `GET /alerts` → 302
  `/settings/notifications`. (302, not 301: browsers cache a 301 forever, and
  these are temporary aliases.) Emails and templates are updated to the new
  URLs anyway.
- The old POST paths stay registered for one release as aliases of the new
  handlers (a form open in a tab during the upgrade still works), then are
  removed in v1.7.0.
- OAuth `intent=link` returns to `/settings/security`.
- Notice emails that link to `/account` link to `/settings/security`.

### 3.4 Other wiring

- Skeleton kind for `/settings/*`: generic.
- Route tables, the route-class tests, `sysAllowed` and route counts update.
- The reauth notice ("Sign in again with Google first") moves with the cards
  it gates.

## 4. Verified first password

Applies to an account with no password (signed up with Google or GitHub).
It reuses the existing password-reset link: `/forgot` already emails a reset
link to passwordless accounts (D-129 tells such users to use Forgot
password), so the inbox already proves ownership. No new token kind, route or
schema is added.

### 4.1 Flow

1. Security → Password card says "You sign in with Google and have no
   password." with an **Email me a link** button.
2. The button POSTs to `/settings/security/password-link`. The server issues
   the existing reset token (`TokenReset`) through the same mail path as
   `/forgot`, with an email variant worded "Set a password for your Uptime
   account". The page returns to Security with `?sent=1` (the address is not
   put in the URL) and the Password card shows "Check you@example.com for a
   link to set your password. It works for 1 hour." with a secondary **Send
   again** button. If the mail queue or a mail budget refused the message
   (`mail.ErrDropped`), nothing is claimed as sent: the user sees the "sent
   recently" message below. No recent re-sign-in is needed to request it; the
   inbox is the proof.
3. The link opens the existing `/reset` page, worded "Choose a password" for
   an account that has none. Completing it behaves as `/reset` does today
   (D-126), including signing out other sessions; Part B changes the sign-in
   part for 2FA accounts (§8.5).
4. The existing password-change notice is sent.

The current "set a first password in place after a recent Google/GitHub
sign-in" form is removed when `mail` is configured.

### 4.2 Limits and edge cases

- Budgets: the existing reset-mail budgets apply, plus 3 links per hour per
  user from this button. The per-address budget is checked first, and the
  user's own slot is given back when nothing was sent (dropped or failed).
  Over budget or dropped: "A link was sent recently. Check your inbox or try
  again later." A send failure shows an error and uses no slot.
- No `mail` configured: the email step is impossible, so the current rule
  stays (a Google/GitHub sign-in within the last 10 minutes, then the
  password form on the Security page). The card says so.
- The link goes to `users.email`, the address the account signed up with.
  The account email cannot be changed (out of scope); if the user no longer
  controls that inbox, the operator can help.
- Disabled accounts: the token is not live (existing rule).

---

# Part B — Two-factor authentication (v1.7.0)

## 5. Method

- TOTP, RFC 6238: HMAC-SHA1, 6 digits, 30-second steps, accepting the
  previous, current and next step. Candidates are compared in constant time.
- Replay protection, atomic: a code is accepted only through
  `UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_enabled_at IS NOT NULL AND totp_last_step < ?`
  with the matched step, requiring exactly one changed row. Two concurrent
  requests with the same code cannot both succeed.
- Secret: 20 random bytes, shown as base32 without padding.
- otpauth URI: `otpauth://totp/{issuer}:{email}?secret=…&issuer={issuer}&algorithm=SHA1&digits=6&period=30`,
  issuer "Uptime ({hostname of base_url, without port})", label and issuer
  percent-encoded.

## 6. Storage (schema v5)

```
ALTER TABLE users ADD COLUMN totp_secret     TEXT    NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN totp_pending    TEXT    NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN totp_pending_at INTEGER;
ALTER TABLE users ADD COLUMN totp_enabled_at INTEGER;
ALTER TABLE users ADD COLUMN totp_last_step  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN mfa_at       INTEGER;
CREATE TABLE recovery_codes (
  user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  code_hash TEXT    NOT NULL,
  used_at   INTEGER,
  PRIMARY KEY (user_id, code_hash)
);
CREATE TABLE pending_logins (
  token_hash      TEXT    PRIMARY KEY,
  user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  method          TEXT    NOT NULL,          -- password | google | github
  next            TEXT    NOT NULL DEFAULT '',
  totp_enabled_at INTEGER NOT NULL,          -- must still match at the code step
  created_at      INTEGER NOT NULL,
  expires_at      INTEGER NOT NULL,
  tries           INTEGER NOT NULL DEFAULT 0,
  used_at         INTEGER
);
CREATE INDEX idx_pending_logins_user ON pending_logins(user_id);
```

- Pre-migration backup `pre-v5`, as for v3 and v4. Expired `pending_logins`
  rows are removed by the hourly prune.
- 2FA is on when `totp_enabled_at` is set.
- `totp_last_step` is never lowered, including when 2FA is turned off or
  reset, so an old code can never be replayed after re-enabling.
- Recovery codes: 10 per user, 16 base32 characters shown as
  `xxxx-xxxx-xxxx-xxxx` (80 bits), stored as SHA-256 of the normalised code;
  input ignores case, spaces and dashes. A code is used only through
  `UPDATE recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`,
  requiring one changed row.
- The TOTP secret is stored unencrypted, like the saved SMTP passwords; the
  database and backups are mode 0600. It is never shown again after setup.
  Anyone with a copy of the database or a backup can generate codes; the
  README says to protect backups accordingly. Encrypting it needs a key kept
  apart from the database; that is a later option, recorded in D-134.

## 7. Turning 2FA on

Security → Two-factor card: "Off" with **Set up**.

1. **Set up** needs a recent sign-in (10 minutes, §9 rules for accounts
   without 2FA). It creates a pending secret (`totp_pending`, valid 15
   minutes); reloading the page shows the same pending secret until it
   expires. The page shows:
   - a QR code of the otpauth URI (inline SVG, from `internal/qr`);
   - the secret in groups of four for typing in by hand;
   - a code field.
2. A correct code (through the shared limiter, §8.4) moves the pending secret
   to `totp_secret`, sets `totp_enabled_at` and `totp_last_step`, creates 10
   recovery codes, sets `mfa_at` on the current session, and **signs out every
   other session**. The codes are shown once with **Copy** and **Download**
   (the `.txt` file is built in the browser by external JavaScript from the
   codes on the page; the server keeps only hashes). The page says they will
   not be shown again. Setup and codes pages send `Cache-Control: no-store`.
3. A notice email: "Two-factor authentication was turned on."

The Security card shows how many recovery codes remain, with a warning at 2
or fewer. **Regenerate recovery codes** needs a fresh code (§9), replaces all
10, shows the new set once, signs out other sessions, and sends a notice.

## 8. Signing in with 2FA on

### 8.1 One way to start a session

Every session is created through one helper, `startSession(w, r, user,
method, next)`. For an account with 2FA on, it creates a `pending_logins` row
instead and redirects to `/login/2fa`. A test fails if `CreateSession` is
called anywhere else. The current call sites that move behind it:
`login.go` (password), `oauth.go` (linked sign-in, sign-up, and the concurrent
sign-up path), `reset.go`, `verify.go`, `signup.go`, `setup.go`. Sign-up and
setup always create new accounts, which have no 2FA, but go through the
helper anyway.

The browser's previous session is deleted, and the device cookie is set, only
when a session is actually created (after the code step for 2FA accounts).
Today both happen at the first step.

### 8.2 The pending step

1. `startSession` stores a `pending_logins` row (5 minutes) and sets a cookie
   `__Host-uptime_mfa` (or `uptime_mfa` over http on loopback; HttpOnly,
   SameSite=Lax, Max-Age 300) holding only a random token. The row holds the
   user, method, `next` and the user's current `totp_enabled_at`.
2. `/login/2fa` (public route class, `no-store`, handler checks the cookie)
   asks for the 6-digit code (`inputmode=numeric`,
   `autocomplete=one-time-code`). Copy: "Enter the code from your
   authenticator app." Links: **Use a recovery code** (`/login/2fa?recovery=1`,
   works without JavaScript) and **Cancel** (deletes the row and cookie, back
   to sign-in).
3. On submit the row is consumed with one conditional update (unused, not
   expired, `tries` below 5), `tries` incremented atomically. The handler
   re-checks the user is enabled and verified and that `totp_enabled_at` still
   matches the row; if 2FA was reset, turned off or re-enabled meanwhile, or
   the account was disabled, it goes back to sign-in and never signs in
   without a code.
4. A correct code creates the session (`auth_at` and `mfa_at` = now), sets
   the device cookie, deletes the row and cookie, and continues to `next`. A
   recovery code works the same, and sends "A recovery code was used to sign
   in. N left."
5. States: wrong code ("That code didn't work. Check the time on your phone
   and try again."); expired or used row ("That took too long. Sign in
   again."); limit reached (§8.4 message).
6. `/password` and `/login` stay in `safeNext`'s deny list; `next` is checked
   when the row is created.

### 8.3 Signing out during the pending step

There is no session yet; **Cancel** or the expiry ends it. Signing out
elsewhere does not affect it.

### 8.4 Limits on code entry (all entry points)

Every code or recovery-code check, wherever it is entered (`/login/2fa`, setup
confirmation, the code field on sensitive forms, turning off, regenerating
codes), first takes a slot from one per-user 2FA limiter. Slots are refunded
on success. The limiter does not consult the device cookie.

- 5 failures per 15 minutes and 20 per 24 hours, per user, both hard.
- The first time either limit is reached in a day, the user gets a notice:
  "Someone entered your password but not your two-factor code. If it wasn't
  you, change your password." (Wording adapts for a Google/GitHub sign-in.)
- Message on the page: "Too many attempts. Try again later." with the wait
  in minutes.
- With three codes valid at once, 20 guesses a day gives an attacker who
  knows the password about a 2% chance over a year, and the owner is warned
  on the first day.
- The existing per-address sign-in limiter also counts each try at
  `/login/2fa`.
- The limiter is in memory like the others and resets on restart; the
  pending row's `tries` cap (5) and the daily notice still bound a restart
  loop.

### 8.5 Links that used to sign in

For an account with 2FA on, `/reset` and `/verify` never create a session:
after a password reset the page goes to `/login` with "Password changed. Sign
in to continue." This amends D-126 for 2FA accounts only.

### 8.6 Linking a provider while signed in

Not a sign-in and asks for no second step, but connecting is a sensitive
action (§9).

## 9. Recent verification and sensitive actions

Two timestamps on the session:

- `auth_at`: set at sign-in and by password or provider re-sign-in (today's
  rule).
- `mfa_at`: set only by a valid code or recovery code (the sign-in step,
  setup confirmation, or the code field on a sensitive form).

For an account **with** 2FA, a recent verification means `mfa_at` within the
last 10 minutes; `auth_at` alone does not count. For an account **without**
2FA, today's rules are unchanged (`auth_at`, or the current password where
the form asks for it).

Sensitive actions:
- turn off 2FA, regenerate recovery codes;
- set or change the password (password accounts still type the current
  password as well, as today);
- connect or disconnect Google/GitHub;
- delete the account.

Forms for these show a code field when `mfa_at` is older than 10 minutes.

## 10. Turning 2FA off and recovery

- **Turn off**: needs a fresh code or a recovery code. Clears the secret,
  pending secret and all recovery codes (keeps `totp_last_step`), signs out
  every other session, and sends a notice.
- **Lost phone**: sign in with a recovery code, then set up 2FA again.
- **Lost phone and codes**: the operator resets it (§11).

## 11. Operator reset

- Admin → Users shows a "2FA" badge on accounts that have it, and a
  **Reset 2FA** action behind a confirm dialog, for non-operator accounts
  only. It is refused for operator accounts, the acting operator included.
- The acting operator must have 2FA on and a recent code (`mfa_at` within 10
  minutes); otherwise the action is hidden with a note "Turn on two-factor
  authentication to reset it for others."
- Reset clears the secret, pending secret and recovery codes, signs the user
  out everywhere, deletes their pending sign-ins, and emails them: "An
  administrator turned off two-factor authentication on your account."
- Command line, for operators (including a locked-out operator):
  `uptime-monitor -reset-2fa person@example.com`. It does the same as the
  panel, prints what it did and exits, like `-reset-password`.
- All 2FA notices use the notice path (`Mailer.Notice`, charged only to the
  instance budget, D-126), so `/forgot` traffic cannot suppress them.
- Log events: `mfa_enabled`, `mfa_disabled`, `mfa_failed`, `mfa_limited`,
  `mfa_recovery_used`, `mfa_reset` (with `actor` = operator id or `cli`).

## 12. QR code

Stdlib has no QR encoder, so `internal/qr` implements one: byte mode, error
correction level M, versions 1–10 (up to 213 bytes, enough for the otpauth
URI with a long issuer and email), one fixed mask pattern (any valid mask
scans; penalty scoring is unnecessary), rendered as an SVG of rectangles with
a quiet zone. Tested against the ISO/IEC 18004 worked example and fixed
vectors, and by scanning with two phone apps during verification. If the URI
is too long for version 10, the page shows only the typed secret, with a
note.

## 13. Testing

Part A:
- Every route and redirect in §3.2–3.3, route classes, old POST aliases,
  302 not 301.
- "Set a password" sends the reset link with the set-password wording;
  budgets; the no-mail fallback; the reset page wording for an account with
  no password.

Part B:
- TOTP against the RFC 6238 test vectors; window ±1; replay refused,
  including two goroutines submitting the same code (exactly one wins).
- Migration v5 upgrade, fresh install and no-op.
- Setup: pending expiry and reload, wrong code, recovery codes shown once and
  stored hashed, other sessions signed out.
- `startSession`: a test that fails if `CreateSession` is called elsewhere.
- Sign-in with password, Google and GitHub (fake provider): no session and
  no device cookie before the code; previous session kept until the code
  succeeds; pending token tampering, expiry, reuse, tries cap; 2FA reset or
  disable or account disable during a pending sign-in; Cancel; recovery code
  without JavaScript; notices.
- Limiter shared across every code-entry point; 5/15 min and 20/day; daily
  notice sent once; device cookie ignored.
- `/reset` and `/verify` do not sign in a 2FA account.
- Password or provider re-sign-in does not satisfy §9 for a 2FA account; a
  code does, for 10 minutes.
- Operator reset: refused for operators, requires the actor's 2FA and recent
  code, revokes sessions and pending rows, notice; CLI path.
- QR encoder vectors.
- Browser check of every new page in both themes at 375 and 1280, and a real
  scan of the QR code with an authenticator app.

## 14. Docs and release

- README sections for Settings, first password, and 2FA (including protecting
  backups); CHANGELOG entries.
- Decision entries: D-133 (Settings and verified first password), D-134 (2FA,
  including the accepted risks in §1 and the amendment to D-126 in §8.5).
- v1.6.0 ships Part A; v1.7.0 ships Part B. Each gets the full release gate
  (gofmt, vet, race tests, govulncheck, Docker build, the demo suites, a
  browser check) and a pre-upgrade backup on the server.
