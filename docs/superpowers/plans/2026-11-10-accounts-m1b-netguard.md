# Accounts M1b: Internal-Address Guard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the server from connecting to loopback, private, link-local and other special-purpose addresses when `allow_private_targets` says so. This covers monitors, Test URL, and the Slack, Telegram and email alert channels.

**Architecture:** A new `internal/netguard` package is the only place that builds HTTP clients, transports and dialers. A guarded dialer checks the resolved address at connect time (`net.Dialer.ControlContext`), which also covers redirects and DNS rebinding. Guarded transports ignore proxies. A new config key, `allow_private_targets` (`none` | `operator` | `all`, default `operator`), decides guarded vs unguarded through `(*config.Config).AllowPrivate(operator bool)`. Until workspaces exist (M1c), everything belongs to the operator. So `operator` and `all` behave as today, and `none` guards everything. A static test stops anyone from building a client or dialer outside `netguard`.

**Execution:** subagent-driven. Implementers are Sonnet subagents; task reviewers and the final branch review are Opus. Reuse existing helpers (`describeErr`, `postJSON`, `ValidateTarget`, `newEnv`, `renderStatus`, `writeJSONStatus`). Write no code a task doesn't need.

**Tech Stack:** Go 1.26 standard library (`net`, `net/netip`, `net/http`, `syscall`), `modernc.org/sqlite`. No new dependencies.

**Spec:** [docs/superpowers/specs/2026-11-10-accounts-design.md](../specs/2026-11-10-accounts-design.md), step **M1b** (§2), using §1.2 criterion 2, §7.1 and §13 (netguard tests, static tests).

## Global Constraints

- Branch `accounts-m1b`, cut from `accounts-m1a`. Nothing is merged or pushed.
- **Default config changes no behaviour.** With `allow_private_targets` unset (`operator`), every existing test passes unmodified. That includes the web tests that monitor `httptest` servers on 127.0.0.1.
- Error text when blocked is exactly `blocked: destination is an internal address`. No IP address or hostname is ever included (§7.1). Wrapping prefixes such as `slack: ` are allowed.
- Guarded transports set `Proxy: nil` (§7.1). Unguarded ones keep today's `http.ProxyFromEnvironment`.
- The rule is an allow-list. An address passes only if, after `Unmap()`, it is global unicast, is not in any prefix in Task 1's list, and is not in a tunnelling range (§7.1).
- No new dependencies. `gofmt -l .` prints nothing, and `go vet ./...` and `go test -race -count=1 ./...` pass at every commit. Commits use the repo's conventional style.
- Config keys from later steps (`signup`, `base_url`, `mail`, `limits`) are not added here.

## Scope rulings (departures from spec §2's M1b row)

- §2 lists "no-redirect webhook client" and "Telegram token validation" under M1b. §7.2 says they apply only to non-operator workspaces, which first exist in M1e, and that operator workspaces keep today's behaviour. Building them now would ship unused code. They move to M1e, together with their tests. Task 5 records this as decision D-122.
- §3.1 names `netguard.Wrap(*http.Transport, allow)`. Every call site builds a new transport, so `netguard.Transport(allow, dialTimeout)` replaces `Wrap` and the static test can forbid transport literals everywhere else. `Blocked` and `Dialer` keep the spec's names.
- §6.3's `Options.PrivateAllowed func(workspaceID) bool` needs workspaces. For M1b, `monitor.Options.AllowPrivate bool` is enough, and M1c turns it into the per-workspace function.

## Review Focus

1. **A public hostname that resolves to an internal address** (DNS rebinding, `localhost`, or a name pointing at 169.254.169.254). Expected: it is blocked, because the check runs on the connected address and not the hostname. Test: Task 1, `TestGuardedDialRefusesLoopback` ("localhost" case).
2. **A redirect from an allowed host to an internal one.** Expected: the redirected request fails with the blocked error. Test: Task 1, `TestGuardedRedirectRefused`.
3. **IPv4-mapped, NAT64, 6to4 and Teredo forms of internal IPv4 addresses.** Expected: all blocked. Test: Task 1, `TestBlocked` table.
4. **An environment proxy (`HTTPS_PROXY`) set on the host.** Expected: the guarded client never uses it, because the proxy would fetch internal URLs for the user. Test: Task 1, `TestGuardedTransportIgnoresProxy`.
5. **Error text reaching the UI, the incident cause or the alerts log.** Expected: it never contains the internal IP. Tests: Task 3, `TestProbeBlockedHidesAddress`, and Task 4, `TestNotifiersGuarded`.

---

## File Structure

| File | Change |
|---|---|
| `internal/netguard/netguard.go` | new: `ErrBlocked`, `Blocked`, `Dialer`, `Transport`, `Client`, `CheckHost` |
| `internal/netguard/netguard_test.go` | new: address table, dial, redirect, proxy and host tests |
| `internal/netguard/static_test.go` | new (Task 5): forbids clients and dialers outside `netguard` |
| `internal/config/config.go` | `AllowPrivateTargets`, its default and validation, `AllowPrivate(operator bool) bool` |
| `internal/monitor/monitor.go` | client built by `netguard`; `Options.AllowPrivate`; `(*Monitor).Test`; `describeErr` maps `ErrBlocked` |
| `internal/notify/notify.go` | `FromConfig(cfg, logger, allowPrivate)`; `Email.Dial`; blocked-error mapping; no `http.DefaultClient` |
| `internal/web/targets.go` | `targetTest` calls `s.mon.Test`; save-time `CheckHost` on create, update and test |
| `internal/web/alerts.go` | passes `allowPrivate` to `FromConfig` |
| `main.go` | wires `AllowPrivate` into `monitor.Options` and `FromConfig` |
| `README.md` | config table row for `allow_private_targets` |
| `docs/decisions.md` | D-122 (Task 5) |

---

### Task 1: `internal/netguard`

**Files:**
- Create: `internal/netguard/netguard.go`, `internal/netguard/netguard_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `var ErrBlocked = errors.New("blocked: destination is an internal address")`
  - `func Blocked(a netip.Addr) bool` returns true for every address that is not allowed (see the rule and list below).
  - `func Dialer(allowPrivate bool, timeout time.Duration) *net.Dialer`. When `allowPrivate` is false, its `ControlContext` parses the `address` argument with `netip.ParseAddrPort` and returns `ErrBlocked` if `Blocked(ap.Addr())`. When true, it is a plain dialer with that timeout.
  - `func Transport(allowPrivate bool, dialTimeout time.Duration) *http.Transport` clones `http.DefaultTransport` and sets `DialContext` to `Dialer(allowPrivate, dialTimeout).DialContext`. `Proxy` is `nil` when guarded and `http.ProxyFromEnvironment` otherwise. Callers may adjust fields on the result.
  - `func Client(allowPrivate bool, timeout time.Duration) *http.Client` returns `&http.Client{Timeout: timeout, Transport: Transport(allowPrivate, 10*time.Second)}`.
  - `func CheckHost(host string) error` gives an early, friendly error at save time. It returns `ErrBlocked` when `host` (as from `url.URL.Hostname()`) is `localhost`, ends in `.localhost`, or is an IP literal for which `Blocked` is true. Otherwise it returns nil.
  - Unexported, for tests: `newDialer(timeout time.Duration, blocked func(netip.Addr) bool) *net.Dialer`. `Dialer(false, t)` is `newDialer(t, Blocked)`.

**Blocked rule:** `a = a.Unmap()`. The address is blocked if `!a.IsValid()`, if `!a.IsGlobalUnicast()`, or if `a` falls in any of these prefixes (pinned from the IANA special-purpose registries plus §7.1):

- IPv4: `0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12 192.0.0.0/24 192.0.2.0/24 192.31.196.0/24 192.52.193.0/24 192.88.99.0/24 192.168.0.0/16 192.175.48.0/24 198.18.0.0/15 198.51.100.0/24 203.0.113.0/24 240.0.0.0/4`
- IPv6: `::/128 ::1/128 64:ff9b::/96 64:ff9b:1::/48 100::/64 2001::/23 2001:db8::/32 2002::/16 3fff::/20 5f00::/16 fc00::/7 fe80::/10 fec0::/10`

Parse the prefixes once into a package-level `[]netip.Prefix` using `netip.MustParsePrefix`.

- [ ] **Step 1: Write the failing tests** in `netguard_test.go`

```go
func TestBlocked(t *testing.T) {
	// blocked == true for every one of these:
	// "127.0.0.1", "::1", "0.0.0.0", "::", "10.1.2.3", "172.16.0.1", "192.168.1.1",
	// "169.254.169.254", "100.64.0.1", "fd00::1", "fe80::1", "fec0::1", "240.0.0.1",
	// "255.255.255.255", "224.0.0.1", "192.0.0.1", "192.88.99.1", "198.18.0.1",
	// "100::1", "::ffff:127.0.0.1" (IPv4-mapped), "64:ff9b::7f00:1" (NAT64),
	// "2002:7f00:1::1" (6to4), "2001::1" (Teredo), "2001:db8::1", "3fff::1".
	// blocked == false: "8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111",
	// "2a00:1450:4001::1".
	// Also: Blocked(netip.Addr{}) == true.
}

func TestGuardedDialRefusesLoopback(t *testing.T) {
	// srv := httptest.NewServer(200 "ok"); port from srv.URL.
	// Client(false, 5s).Get for both "http://127.0.0.1:<port>/" and "http://localhost:<port>/":
	// errors.Is(err, ErrBlocked) is true. (Only errors.Is here: the url.Error and
	// net.OpError wrappers carry the address; Tasks 3 and 4 strip them before any
	// message reaches users.)
	// Client(true, 5s).Get("http://127.0.0.1:<port>/") succeeds with status 200.
}

func TestGuardedRedirectRefused(t *testing.T) {
	// target := httptest.NewServer on 127.0.0.1 (returns 200).
	// front: a listener on 127.0.0.2:0 (skip the test with t.Skip if Listen fails)
	//   serving a 302 redirect to target.URL.
	// c := &http.Client{Transport: a clone of Transport(false, 5s) whose DialContext
	//   is newDialer(5s, func(a) bool { return a != netip.MustParseAddr("127.0.0.2") }).DialContext}
	// c.Get("http://127.0.0.2:<port>/"): errors.Is(err, ErrBlocked).
}

func TestGuardedTransportIgnoresProxy(t *testing.T) {
	// Transport(false, time.Second).Proxy == nil
	// Transport(true, time.Second).Proxy != nil
}

func TestCheckHost(t *testing.T) {
	// ErrBlocked for "localhost", "LOCALHOST", "api.localhost", "127.0.0.1", "::1",
	//   "169.254.169.254", "10.0.0.5".
	// nil for "example.com", "8.8.8.8", "2606:4700:4700::1111", "" (empty: left to ValidateTarget).
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/netguard`
Expected: FAIL (the package does not exist yet).

- [ ] **Step 3: Implement `netguard.go`** to the interfaces above. In the package doc comment, state that it is the only place allowed to build HTTP clients, transports and dialers, and why.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race -count=1 ./internal/netguard -v`
Expected: all PASS (or SKIP for the 127.0.0.2 case only on systems without that loopback alias).

- [ ] **Step 5: Run the full suite and commit**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: no gofmt output; all `ok`.

```bash
git add internal/netguard
git commit -m "feat(netguard): connect-time guard against internal addresses"
```

---

### Task 2: `allow_private_targets` config

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`, `README.md` (configuration table)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `Config.AllowPrivateTargets string` with json tag `"allow_private_targets"`.
  - In `applyDefaults`: an empty value becomes `"operator"`. Any value other than `none`, `operator` or `all` fails with `config: allow_private_targets must be "none", "operator" or "all"`.
  - `func (c *Config) AllowPrivate(operator bool) bool` returns `none` → false, `operator` → `operator`, `all` → true.

- [ ] **Step 1: Write the failing test**

```go
func TestAllowPrivateTargets(t *testing.T) {
	// Parse(`{}`): AllowPrivateTargets == "operator"; AllowPrivate(true) true; AllowPrivate(false) false.
	// Parse(`{"allow_private_targets":"none"}`): AllowPrivate(true) false.
	// Parse(`{"allow_private_targets":"all"}`): AllowPrivate(false) true.
	// Parse(`{"allow_private_targets":"some"}`): error containing `allow_private_targets must be`.
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/config -run TestAllowPrivateTargets`
Expected: FAIL (field undefined).

- [ ] **Step 3: Implement it, and add a README configuration table row** after `trusted_proxies`:
`| allow_private_targets | operator | none, operator or all. Which workspaces may reach loopback, private, link-local (cloud metadata) and other internal addresses through monitors, Test URL and alert channels. Until accounts exist everything belongs to the operator, so only none changes anything: it blocks all internal destinations. |`

- [ ] **Step 4: Run the tests and commit**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: all `ok`.

```bash
git add internal/config README.md
git commit -m "feat(config): allow_private_targets (none, operator, all)"
```

---

### Task 3: Guard the monitor and Test URL

**Files:**
- Modify: `internal/monitor/monitor.go`, `internal/monitor/monitor_test.go`, `internal/web/targets.go`, `internal/web/web_test.go`, `main.go`

**Interfaces:**
- Consumes: `netguard.Transport`, `netguard.ErrBlocked`, `netguard.CheckHost` (Task 1); `(*config.Config).AllowPrivate` (Task 2).
- Produces:
  - `monitor.Options.AllowPrivate bool`. When `Options.Client` is nil, `New` uses `c := netguard.Client(o.AllowPrivate, 0)` (no overall timeout; each probe has its own context deadline) and then sets these fields on `tr := c.Transport.(*http.Transport)`, matching today's values: `TLSHandshakeTimeout = 10*time.Second`, `MaxIdleConnsPerHost = 2`, `IdleConnTimeout = 90*time.Second`, `DisableKeepAlives = true`. No `http.Client{`, `http.Transport{` or `net.Dialer{` literal remains in `monitor.go`.
  - `func (m *Monitor) Test(ctx context.Context, t store.Target) store.Check` returns `Probe(ctx, m.client, t, m.now)`. It probes an unsaved target with the monitor's own guarded or unguarded client (§3.3: "Test URL through `(*Monitor).Test`").
  - `describeErr` returns `netguard.ErrBlocked.Error()` when `errors.Is(err, netguard.ErrBlocked)`, before any other formatting.
  - `main.go` passes `AllowPrivate: cfg.AllowPrivate(true)` to `monitor.Options`.
  - Web, at save time: in `targetCreate`, `targetUpdate` and `targetTest`, once `toTarget` succeeds and when `!s.cfg.AllowPrivate(true)`, parse the URL and call `netguard.CheckHost(u.Hostname())`. A non-nil error is reported exactly as a validation error is today on that path: a 422 page for create and update, JSON 422 for test. Put this in one helper, `func (s *Server) checkDestination(rawURL string) error`, in `targets.go`.
  - `targetTest` calls `s.mon.Test(r.Context(), t)` instead of building a client.

- [ ] **Step 1: Write the failing tests**

```go
// monitor_test.go
func TestProbeBlockedHidesAddress(t *testing.T) {
	// srv := httptest.NewServer(200).
	// m := New(Options{Store: <temp store as the other tests open it>, AllowPrivate: false})
	// c := m.Test(ctx, store.Target{URL: srv.URL, Timeout: 2 * time.Second})
	// c.OK == false; c.Error == "blocked: destination is an internal address";
	// strings.Contains(c.Error, "127.0.0.1") == false.
	// Same target with New(Options{..., AllowPrivate: true}).Test: c.OK == true.
}

// web_test.go
func TestPrivateTargetsBlockedWhenNone(t *testing.T) {
	// e := newEnv(t, ""); e.cfg.AllowPrivateTargets = "none"
	// POST /targets name=x url=http://127.0.0.1:9/ -> 422, body contains "blocked: destination is an internal address".
	// POST /targets/test url=http://localhost:9/ -> 422 JSON {"error": "blocked: destination is an internal address"}.
	// With e.cfg.AllowPrivateTargets = "operator": POST /targets name=y url=http://127.0.0.1:9/ -> 303 (created).
}
```

Note: `newEnv` builds its monitor with `Options.Client` unset. The web test checks only the save-time path, so the monitor's own client doesn't matter there.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/monitor -run TestProbeBlockedHidesAddress; go test ./internal/web -run TestPrivateTargetsBlockedWhenNone`
Expected: FAIL (`AllowPrivate` and `Test` are undefined).

- [ ] **Step 3: Implement it** as described under Interfaces.

- [ ] **Step 4: Run the full suite and commit**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: all `ok`.

```bash
git add internal/monitor internal/web main.go
git commit -m "feat(monitor): guard probes and Test URL with allow_private_targets"
```

---

### Task 4: Guard the alert channels

**Files:**
- Modify: `internal/notify/notify.go`, `internal/notify/notify_test.go`, `internal/web/alerts.go`, `main.go`

**Interfaces:**
- Consumes: `netguard.Client`, `netguard.Dialer`, `netguard.ErrBlocked` (Task 1); `AllowPrivate` (Task 2).
- Produces:
  - `func FromConfig(cfg config.Notify, logger *slog.Logger, allowPrivate bool) Multi`. Its shared HTTP client is `netguard.Client(allowPrivate, 15*time.Second)`. Its `Email` gets `Dial: netguard.Dialer(allowPrivate, 30*time.Second).DialContext`.
  - `Email.Dial func(ctx context.Context, network, addr string) (net.Conn, error)`. `send` uses it, and a nil value means `netguard.Dialer(true, 0).DialContext` (unguarded, as today, so tests that build `Email{}` directly keep working). The `var d net.Dialer` goes away.
  - `postJSON` with a nil client uses `netguard.Client(true, 15*time.Second)` instead of `http.DefaultClient`.
  - Blocked errors never carry an address. In `postJSON`, after unwrapping `*url.Error`, if `errors.Is(err, netguard.ErrBlocked)` return `fmt.Errorf("%s: %w", name, netguard.ErrBlocked)`. In `Email.Notify`, if `errors.Is(err, netguard.ErrBlocked)` return `fmt.Errorf("email: %w", netguard.ErrBlocked)`.
  - Callers: `main.go` (both `FromConfig` calls) passes `cfg.AllowPrivate(true)`. `web/alerts.go` `saveNotify` passes `s.cfg.AllowPrivate(true)`. `notify_test.go` call sites add `true`, which is a mechanical signature update.

- [ ] **Step 1: Write the failing test**

```go
func TestNotifiersGuarded(t *testing.T) {
	// srv := httptest.NewServer(200).
	// m := FromConfig(config.Notify{Slack: &config.Slack{WebhookURL: srv.URL}}, slog.Default(), false)
	// err := m.Notify(ctx, Event{Target: "x"})
	// errors.Is(err, netguard.ErrBlocked); !strings.Contains(err.Error(), "127.0.0.1").
	// The same config with allowPrivate=true: err == nil.
	// Email: port := a fake SMTP listener on 127.0.0.1 (reuse fakeSMTPAt);
	// FromConfig(config.Notify{Email: &config.Email{SMTPHost: "127.0.0.1", SMTPPort: port,
	//   From: "a@b.c", To: []string{"d@e.f"}}}, slog.Default(), false).Notify(...)
	// errors.Is(err, netguard.ErrBlocked) and the message contains no "127.0.0.1".
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/notify -run TestNotifiersGuarded`
Expected: FAIL (`FromConfig` has the wrong number of arguments).

- [ ] **Step 3: Implement it** as described under Interfaces.

- [ ] **Step 4: Run the full suite and commit**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: all `ok`.

```bash
git add internal/notify internal/web main.go
git commit -m "feat(notify): guard Slack, Telegram and email with allow_private_targets"
```

---

### Task 5: Static test against stray clients, plus decision record

**Files:**
- Create: `internal/netguard/static_test.go`
- Modify: `docs/decisions.md`, plus any production file the test flags

**Interfaces:**
- Consumes: Tasks 1–4.
- Produces: `TestNoStrayClients`. It walks the repository root (`../..` from the package directory) and reads every `.go` file that is not a `_test.go` file, skipping `internal/netguard/` and `.git`. It fails, listing `file:line`, on any line matching the regexp `http\.Client\{|net\.Dialer\{|var \w+ net\.Dialer|http\.Transport\{|http\.DefaultClient|http\.DefaultTransport|http\.(Get|Post|Head|PostForm)\(|smtp\.SendMail\(|net\.Dial(Timeout)?\(|tls\.Dial\(`. That covers the spec's literal patterns (§7.1) plus the package-level shortcuts that reach the network without a guard. Comment lines (trimmed prefix `//`) are skipped.

- [ ] **Step 1: Write the test.**

- [ ] **Step 2: Run it**

Run: `go test ./internal/netguard -run TestNoStrayClients -v`
Expected: PASS if Tasks 3–4 left nothing behind. Otherwise it fails and lists the offending lines. Fix each one by using `netguard`, then re-run until it passes.

- [ ] **Step 3: Prove the test catches regressions**

Temporarily add `var _ = &http.Client{}` to `internal/web/helpers.go` and run the test. Expected: FAIL naming `internal/web/helpers.go`. Remove the line and re-run. Expected: PASS. Record both outputs in the report.

- [ ] **Step 4: Add decision D-122** to `docs/decisions.md`, under the Accounts section after D-121:

```markdown
### D-122: M1b ships the guard only
- **Status:** Accepted (amends D-119's step contents)
- **Decision:** The non-redirecting webhook client and Telegram token validation move from M1b to M1e. `netguard.Transport` replaces the spec's `netguard.Wrap`, and the monitor takes `AllowPrivate bool` until M1c makes it per workspace.
- **Why:** Both restrictions apply only to non-operator workspaces (§7.2), which first exist in M1e. Built earlier, they would be unused code. Every call site builds a new transport, so a constructor lets the static test forbid transport literals everywhere else.
```

- [ ] **Step 5: Run the full suite and commit**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: all `ok`.

```bash
git add internal docs/decisions.md
git commit -m "test(netguard): forbid clients and dialers outside netguard; record D-122"
```

---

### Task 6: Step verification

- [ ] **Step 1: CI checks.** Run `test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./... && go run golang.org/x/vuln/cmd/govulncheck@latest ./...`. Expected: everything passes, and govulncheck lists only the 13 known SEC-3 standard-library IDs.
- [ ] **Step 2: Docker build.** Run `docker build -q --build-arg VERSION=m1b -t uptime-monitor:m1b .`. Expected: success.
- [ ] **Step 3: Smoke test.** Start the binary with a temp config `{"listen":"127.0.0.1:18941","allow_private_targets":"none","targets":[{"name":"Local","url":"http://127.0.0.1:1/","interval":"1s","timeout":"500ms"}]}`. Expected:
  - `/api/status` shows `Local` down, with an error equal to `blocked: destination is an internal address`.
  - The log line `target down` carries the same cause, with no IP.
  - Stop the binary with `pkill -TERM -x <binary name>`.
- [ ] **Step 4: Browser check under the CSP with `playwright-cli`.** No new pages. With `allow_private_targets: none`, open the dashboard, use the add-monitor slide-over's Test URL on `http://localhost:1/`, and confirm the blocked message appears as a toast with no console CSP errors.
- [ ] **Step 5: Opus whole-branch review** of `accounts-m1a..accounts-m1b`.
