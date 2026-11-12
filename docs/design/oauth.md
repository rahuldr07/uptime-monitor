# Sign in with Google and GitHub

Status: approved design for v1.2.0 (2026-11-12). Decisions are recorded in `docs/decisions.md` as D-129.

## 1. Goal

Let people sign up and sign in with a Google or GitHub account, alongside email and password. The feature is optional: each self-hoster turns it on by registering their own OAuth app and adding its keys to the config.

**Success criteria**
- A new user can create an account with one click through Google or GitHub, whenever the sign-up mode allows it.
- A signed-in user can connect and disconnect Google and GitHub from the Account page. They can also add or change a password.
- Nobody can take over an account by controlling an email address at a provider. Accounts are never linked automatically by email.
- Every account always keeps at least one way to sign in.
- With no providers configured, or when a provider is down, everything else works exactly as in v1.1.0.

**Out of scope:** generic OpenID Connect providers, storing provider access tokens or calling provider APIs after sign-in, and SSO-only instances with passwords disabled. The provider code is shaped so another provider is a small addition.

## 2. Decisions (maintainer)

1. **No auto-linking.** If the provider's email belongs to an existing account that isn't linked, the user is told to sign in with their password and connect the provider from Account. This tells an anonymous visitor that the address has an account. That is accepted, and recorded in D-129.
2. **Providers:** Google and GitHub. GitHub is built first because it is simpler.
3. **Passwords are optional.** Social-only accounts can exist. Sensitive actions then need a recent sign-in instead of the password (§7).
4. **Standard library only, no new dependencies.**
   - The code exchange and PKCE use `net/http`.
   - Google identity comes from its OpenID Connect userinfo endpoint, called server to server over TLS with the access token. No JWT or JWKS handling.
   - GitHub identity comes from `/user` and `/user/emails`.
   - All outbound calls go through `netguard` with private addresses refused.

## 3. Configuration

New optional key:
```json
"oauth": {
  "google": { "client_id": "...", "client_secret": "${GOOGLE_CLIENT_SECRET}" },
  "github": { "client_id": "...", "client_secret": "${GITHUB_CLIENT_SECRET}" }
}
```
- A provider is enabled when both values are non-empty after environment expansion. A block whose values are all empty is dropped, the same way empty notify channels are.
- **Startup errors:**
  - one of the two values is set and the other is empty;
  - any provider is configured while `base_url` is empty or not `https://`. Plain `http` is allowed only for localhost and loopback, as with invites;
  - an unknown provider key.
- **Callback URLs:** `<base_url>/auth/google/callback` and `<base_url>/auth/github/callback`. The README lists them for registering the apps.
- **Requested scopes:** Google `openid email`, GitHub `read:user user:email`.

## 4. Data model (schema v3)

- **New table:**
  ```sql
  CREATE TABLE identities (
    provider     TEXT    NOT NULL CHECK (provider IN ('google','github')),
    subject      TEXT    NOT NULL,          -- provider's stable account id (Google sub, GitHub numeric id)
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email        TEXT    NOT NULL,          -- verified email at link time, display only
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    PRIMARY KEY (provider, subject),
    UNIQUE (user_id, provider)              -- one identity per provider per user
  );
  CREATE INDEX idx_identities_user ON identities(user_id);
  ```
- **Social-only accounts:** `users.password_hash` stays `NOT NULL`, so the users table isn't rebuilt. An empty string means the account has no password. Every code path that reads the hash must handle the empty value (§8).
- **Sessions:** `ALTER TABLE sessions ADD COLUMN auth_at INTEGER`. NULL, for sessions created before v3, counts as "not recent". New sessions set it to `created_at`.
- **Migration step v3:**
  - creates the table and index, and adds `sessions.auth_at`;
  - runs in its own transaction. The migrator's backup is generalized: any upgrade of a database that holds data writes `<db>.pre-v<N>-<time>.bak` first, where N is the target version, and an existing backup for the same N is reused. The existing `pre-accounts` name stays for the v2 step;
  - tests cover a v2 database upgrading, with a backup file written at mode 0600, and a fresh v3 install. The upgrade is also run on a copy of a v1.1.0 demo database.
- **Identity lookups always use `(provider, subject)`,** never the email.

## 5. Sign-in flow

**Start:** `GET /auth/{provider}/start?intent=login|link|reauth&next=…&invite=…`
- This is a plain link, not a form, because the CSP sets `form-action 'self'`.
- Unknown or disabled provider: 404.
- `intent=link` and `intent=reauth` require a signed-in session; without one, 404.
- Rate limit: 20 starts per minute per client address. Over the limit, the auth page re-renders with the sign-in rate-limit message and status 429.
- It generates a state of 32 random bytes and a PKCE verifier of 32 random bytes (S256 challenge). There is no nonce: the ID token isn't read, so a nonce would never be checked.
- It sets the cookie `__Host-uptime_oauth` (or `uptime_oauth` on http localhost):
  - HttpOnly, SameSite=Lax, Path=/, Max-Age 600;
  - the value is the state, the verifier, the provider, the intent, the safe `next`, the invite token if any, the user ID for link/reauth, and an issue time;
  - it is signed with HMAC-SHA256 using a per-process key, like the device cookie, and base64url-encoded.
- It redirects 303 to the provider's authorization URL with `response_type=code`, `client_id`, `redirect_uri`, `scope`, `state`, `code_challenge` and `code_challenge_method=S256`.
- Google also gets `prompt=select_account`.
- For `reauth`, Google also gets `prompt=consent select_account`, so the user goes through an interaction step. GitHub has no prompt or max_age.
- **Limit, stated plainly:** the server doesn't read the ID token's `auth_time`, so reauth proves only that this browser holds a live session for the linked provider account, not that the user just typed a password. That is weaker than asking for a password, and it's accepted for social-only accounts (D-129).

**Callback:** `GET /auth/{provider}/callback?code=…&state=…`
- It reads and verifies the cookie and deletes it straight away; a cookie is used once.
- The callback is rejected with "Sign-in failed. Try again." and status 400 when:
  - the cookie is missing, has a bad signature or is older than 10 minutes;
  - the provider doesn't match;
  - the `state` doesn't match (constant-time comparison);
  - the provider returned `error=` (for example, the user pressed Cancel);
  - the code exchange or identity fetch fails or times out (10s).
- In that last case the real error is logged and never shown.
- Rate limit: callbacks count against the same 20-per-minute bucket.
- **Code exchange:** POST the token endpoint with `grant_type=authorization_code`, `code`, `redirect_uri`, `client_id`, `client_secret` and `code_verifier`, with `Accept: application/json`. The access token stays in memory only.
- **Fetching the identity:**
  - **Google:** `GET https://openidconnect.googleapis.com/v1/userinfo`. It needs `sub`, `email` and `email_verified == true`.
  - **GitHub:** `GET https://api.github.com/user` for `id`, then `GET https://api.github.com/user/emails`, taking the entry with `primary && verified`.
  - Without a verified email the result is "Your Google account has no verified email address." or the GitHub equivalent, status 400.
  - The subject is the stable ID: Google `sub`, GitHub numeric `id` as a decimal string.
  - The email is normalized with the existing `NormalizeEmail`. If it fails to normalize, that is the same no-verified-email error.

**Decision table.** Each row's outcome happens in a single store transaction where that matters.

| Intent | Identity linked? | Email owned by an account? | Outcome |
|---|---|---|---|
| login | yes, to user U | — | If U is disabled: 403 `msgDisabled`. Otherwise update `last_used_at`, create a session for U (deleting the browser's previous session), set the device cookie, log `oauth_login_ok`, and redirect to `next` (default `/`). |
| login | no | yes | No account is created or linked. Show 409: "An account with this email already exists. Sign in with your password (or use Forgot password if you don't have one), then connect {Provider} from your Account page." Forgot-password sets a password and verifies the address, which also rescues an unverified account someone else created with this email in open mode. Log `oauth_login_unlinked` with `email_hash`. |
| login | no | no | Sign-up. Mode `closed` with no invite: 403 "Sign-up on this instance is by invitation." In `invite` mode a valid invite token is required, carried in the cookie. If the invite is bound to an email, the provider email must equal it, otherwise `msgInviteEmail`. In `open` mode it is allowed. Run the sign-up limits (per address and `signups_per_hour`). Create the account with an empty password hash, `Verified: true`, a workspace with the neutral name "My workspace" (and the slug derived from that, as for any new workspace; the owner can rename both in status settings), so the email's local part never appears in a public `/s/<slug>` address, and the identity row, all in one transaction that also consumes the invite. Then sign in as above and log `oauth_signup`. A workspace cap gives the existing `msgNoNewAccounts`. |
| link | yes, to this user | — | Redirect to `/account` with the flash "{Provider} is already connected." |
| link | yes, to another user | — | 409 "This {Provider} account is already connected to a different account." |
| link | no | — | The cookie's user ID must equal the current session's user, otherwise 400. The user must not already have an identity for this provider (UNIQUE), otherwise 409. Insert the identity, log `oauth_link`, and redirect to `/account` with the flash "{Provider} connected." The provider email may differ from the account email, which is allowed and shown. |
| reauth | yes, to this user | — | Mark the session as recently authenticated (§7). Redirect to `next` (default `/account`). |
| reauth | otherwise | — | 400 "Sign in with the {Provider} account connected to this account." |

**Other rules**
- `/auth/{provider}/start` and `/auth/{provider}/callback` are in the **public** route class, with checks in the handler. The guest class would redirect signed-in users and break link and reauth. Both responses send `Cache-Control: no-store`. `safeNext` rejects `/auth`. The `sysAllowed` static test and the route-count tests are updated.
- `next` always goes through the existing `safeNext`.
- The callback never reveals more than the table above.
- OAuth sign-in skips the per-email login throttle, since no password guessing is possible. The per-address limit above applies instead.

## 6. User interface

- **Sign-in page:** "Continue with Google" and "Continue with GitHub" buttons (Lucide-style mark icons, or a neutral `log-in` icon if a brand mark would need a new symbol) above the email form, separated by "or". Each button is shown only when that provider is enabled.
- **Sign-up page:** the same buttons in `open` mode, and in `invite` mode when a valid invite is present (the start link carries `invite=`). Not shown in `closed` mode without an invite.
- **Account page, new "Sign-in methods" card:**
  - **Password:** if the account has one, "Change password", the existing form. If not, "Set a password", a form with new password and confirm that needs a recent sign-in (§7).
  - **Each enabled provider:** if connected, show its email and "Disconnect"; otherwise "Connect", a link to `/auth/{p}/start?intent=link`.
  - Disconnect is a POST to `/account/identities/{provider}/delete` behind `sameOrigin` and the confirm dialog.
  - Disconnect is refused if it would remove the last sign-in method, meaning no password and no other identity, with the message "Add a password or connect another provider first." The rule is enforced inside the store transaction that deletes the identity (`ErrLastSignInMethod`), not only in the handler.
- **Operator panel:** the users list shows a small "Google" or "GitHub" badge per connected provider.

## 7. Recent authentication for social-only accounts

- Sessions record `auth_at`, the time of the last real authentication. It is set when a session is created and refreshed by a `reauth` callback.
  - The migration adds `sessions.auth_at INTEGER` (nullable).
  - `SessionAuth` returns `AuthAt` and `HasPassword`.
- **Sensitive actions:**
  - deleting the account;
  - setting a password when there is none;
  - disconnecting a provider when the account has no password.
- If the user has a password, these keep asking for it, as today; disconnecting also asks for it.
- If the user has no password, they need `now - auth_at ≤ 10 minutes`. Otherwise the page shows "For your security, sign in again with {Provider} to continue" with a reauth link for each connected provider (next = `/account`).
- **Changing a password** that already exists stays as today: it needs the current password.

## 8. Changes needed for empty passwords

- **`loginPost`:** an account with an empty hash runs `VerifyDummy` and fails with the generic `msgBadLogin` and status 401, the same timing and response as an unknown email. It must never return 500.
- **`checkCurrent`** (used by password change, account delete and admin delete): for an account with no password it must never call `Verify` on the empty hash. A posted `current` value is refused with "Sign in again to continue" (403), and the caller shows the §7 reauth links. It never returns 500.
- **Forgot and reset:** unchanged. A reset link sets a password for a social-only account. That's acceptable, because whoever controls the email already controls the account through reset.
- **Admin delete:** unchanged. It uses the operator's own password, or §7 if the operator is social-only.
- **Setup:** unchanged. The first operator always sets a password.
- **`-reset-password`, `-enable-user`:** unchanged.

## 9. Security

- **CSRF and login CSRF:**
  - state is bound to the signed cookie and checked in constant time;
  - PKCE S256 is used;
  - the cookie is single-use and short-lived, and SameSite=Lax lets the cookie reach the top-level callback;
  - link and reauth also bind the user ID and check it against the live session.
- **Account takeover:**
  - identities are keyed by provider subject, never email;
  - no auto-linking;
  - only verified emails are accepted;
  - disabled users can't sign in through a provider.
- **SSRF:** endpoints are fixed constants, not configurable. Calls go through `netguard.Client(false, 10s)`, with no redirects followed on the token call.
- **Secrets and logging:**
  - client secrets only come from config, with `${ENV}` expansion recommended;
  - access tokens, codes, state and cookies are never logged or stored;
  - errors from providers are logged by kind, never the response body.
- **Enumeration:** the sign-up-with-existing-email message reveals that an account exists. This is the accepted cost of decision 1. It costs an attacker a full OAuth round trip per probe and is rate-limited.
- **Log events:** `oauth_login_ok`, `oauth_login_unlinked`, `oauth_signup`, `oauth_link`, `oauth_unlink`, `oauth_reauth` and `oauth_failed` (with a `reason`). Fields: `event`, `provider`, `user_id` where known, `ip`, and `email_hash` for unknown users.

## 10. Testing

- **The provider package is tested against an `httptest` fake provider.**
  - The fake implements the authorize redirect, the token endpoint (checking `code_verifier` against the challenge and `client_secret`), the userinfo endpoints and the emails endpoint.
  - `oauth.Provider` values are built by `main` from config with fixed production endpoints, and passed in through `web.Options.OAuth`, as the mailer is. Tests build providers with `oauth.NewForTest(name, endpoints, client)`, which points at the fake and takes a non-guarded client. Production code can't configure endpoints.
  - Cases: a good flow for each provider, an unverified email, GitHub with no primary verified email, a bad PKCE verifier, a provider error, a timeout, and a non-JSON response.
- **Web tests use the fake provider,** covering every decision-table row plus:
  - state tampering, a replayed cookie, a wrong provider in the cookie, an expired cookie;
  - `next` open-redirect attempts;
  - a closed-mode sign-up refusal, invite sign-up and an invite email mismatch;
  - the workspace cap;
  - a disabled user;
  - link to another user's identity;
  - disconnecting the last method refused;
  - a social-only user who can't log in with any password (401, not 500);
  - recent-auth gating and reauth refreshing `auth_at`;
  - the per-address rate limit.
- **Store tests:** the v3 migration (upgrade and fresh), identity uniqueness, the cascade when a user is deleted, and `HasPassword`.
- **Static test:** the outbound OAuth client uses `netguard`, so the existing stray-client test still passes.
- **Browser check:** the sign-in and sign-up buttons render only when enabled. The start link goes to the provider's URL with the expected parameters, checked from the 303 `Location`. The Account "Sign-in methods" card renders.

## 11. Documentation

- **README:** a "Sign in with Google or GitHub" section covering how to register each app (callback URLs, scopes), the config block, the linking rules and their reason, and social-only accounts.
- **`docs/decisions.md`:** D-129 covers the decisions in §2, the empty-hash convention, `auth_at` with its 10-minute window, and the accepted enumeration cost.
- **CHANGELOG:** a `[1.2.0]` entry.
