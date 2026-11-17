package web

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/store"
)

func (e *env) opUser(t *testing.T) store.User {
	t.Helper()
	u, _, err := e.st.UserByEmail(context.Background(), opEmail)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAccountTimeZoneValidation(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		value string
		code  int
		want  string // stored explicit zone afterwards
	}{
		{"Asia/Kolkata", http.StatusSeeOther, "Asia/Kolkata"},
		{"Not/AZone", http.StatusUnprocessableEntity, "Asia/Kolkata"}, // rejected, previous value kept
		{"Local", http.StatusUnprocessableEntity, "Asia/Kolkata"},
		{"../etc/localtime", http.StatusUnprocessableEntity, "Asia/Kolkata"},
		{"", http.StatusSeeOther, ""}, // automatic
	} {
		resp, body := e.post(t, e.op, "/account/timezone", url.Values{"time_zone": {c.value}})
		if resp.StatusCode != c.code {
			t.Errorf("%q: status %d, want %d", c.value, resp.StatusCode, c.code)
		}
		if c.code == http.StatusUnprocessableEntity && !strings.Contains(body, "Choose a time zone") {
			t.Errorf("%q: no error shown", c.value)
		}
		if got := e.opUser(t).TimeZone; got != c.want {
			t.Errorf("%q: stored %q, want %q", c.value, got, c.want)
		}
	}
	if resp, _ := e.post(t, e.client(t), "/account/timezone", url.Values{"time_zone": {"UTC"}}); resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Errorf("anonymous save should go to sign-in: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestAccountPageShowsZoneField(t *testing.T) {
	e := newEnv(t)
	if err := e.st.SetDetectedZone(context.Background(), e.opUser(t).ID, "Asia/Kolkata"); err != nil {
		t.Fatal(err)
	}
	_, body := e.get(t, e.op, "/account")
	body = html.UnescapeString(body)
	for _, want := range []string{`id="timezone"`, `<option value="" selected>Automatic (Asia/Kolkata)</option>`,
		`<option value="Asia/Kolkata">Asia/Kolkata (UTC+05:30)</option>`, `now Asia/Kolkata (UTC+05:30)`} {
		if !strings.Contains(body, want) {
			t.Errorf("account page lacks %q", want)
		}
	}
}

func TestDetectedZoneEndpoint(t *testing.T) {
	e := newEnv(t)
	post := func(c *http.Client, tz string) (*http.Response, string) {
		return e.post(t, c, "/account/tz-detected", url.Values{"tz": {tz}})
	}
	// Signed out: sent to sign in, nothing stored.
	if resp, _ := post(e.client(t), "Asia/Kolkata"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("anonymous: %d", resp.StatusCode)
	}
	// Cross-site posts are refused.
	req, _ := http.NewRequest("POST", e.ts.URL+"/account/tz-detected", strings.NewReader("tz=Asia%2FKolkata"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	if resp, err := e.op.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site: %v %v", resp, err)
	}
	// Junk is ignored.
	for _, junk := range []string{"", "Local", "Nope/Nope", strings.Repeat("A", 100)} {
		if resp, _ := post(e.op, junk); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("junk %q: %d", junk, resp.StatusCode)
		}
	}
	if u := e.opUser(t); u.DetectedTZ != "" || u.TimeZone != "" {
		t.Fatalf("junk was stored: %+v", u)
	}
	// Automatic: the report changes the effective zone, a repeat does not.
	if _, body := post(e.op, "Asia/Kolkata"); !strings.Contains(body, `"changed": true`) {
		t.Errorf("first report: %s", body)
	}
	if _, body := post(e.op, "Asia/Kolkata"); !strings.Contains(body, `"changed": false`) {
		t.Errorf("repeat: %s", body)
	}
	if _, body := e.get(t, e.op, "/"); !strings.Contains(body, `data-tz="Asia/Kolkata"`) || !strings.Contains(body, "Times in IST (Asia/Kolkata)") {
		t.Error("page does not carry the detected zone and its label")
	}
	// An explicit choice wins and is never overwritten; only the detected value follows the browser.
	e.post(t, e.op, "/account/timezone", url.Values{"time_zone": {"America/New_York"}})
	if _, body := post(e.op, "Europe/Paris"); !strings.Contains(body, `"changed": false`) {
		t.Errorf("explicit zone: %s", body)
	}
	if u := e.opUser(t); u.TimeZone != "America/New_York" || u.DetectedTZ != "Europe/Paris" {
		t.Fatalf("zones = %q, %q", u.TimeZone, u.DetectedTZ)
	}
	// Back to automatic: the detected zone applies again.
	e.post(t, e.op, "/account/timezone", url.Values{"time_zone": {""}})
	if got := e.opUser(t).Location().String(); got != "Europe/Paris" {
		t.Fatalf("effective zone = %s", got)
	}
}

// A page shows one instant in the viewer's zone, whatever the server's.
func TestPagesRenderInViewerZone(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	if _, err := e.opWS.CreateMaintenance(ctx, store.Maintenance{Title: "Disk swap", StartsAt: start, EndsAt: start.Add(time.Hour)}, e.now()); err != nil {
		t.Fatal(err)
	}
	_, body := e.get(t, e.op, "/maintenance")
	if !strings.Contains(body, "Nov 8 09:00:00") || !strings.Contains(body, "Times in UTC") {
		t.Fatal("default zone is UTC")
	}
	e.post(t, e.op, "/account/timezone", url.Values{"time_zone": {"Asia/Kolkata"}})
	_, body = e.get(t, e.op, "/maintenance")
	if !strings.Contains(body, "Nov 8 14:30:00") || strings.Contains(body, "Nov 8 09:00:00") || !strings.Contains(body, "Times in IST (Asia/Kolkata)") {
		t.Fatal("page should show 14:30 in Asia/Kolkata")
	}
	// The live-refresh fragment uses the same zone.
	req, _ := http.NewRequest("GET", e.ts.URL+"/maintenance", nil)
	req.Header.Set("X-Partial", "1")
	resp, err := e.op.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 1<<16)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "14:30:00") {
		t.Fatal("partial refresh should use the viewer's zone")
	}
}

func TestPublicStatusPageUsesOwnerZone(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	start := e.now().Add(24 * time.Hour).Truncate(time.Hour) // 2026-11-08 12:00 UTC
	if _, err := e.opWS.CreateMaintenance(ctx, store.Maintenance{Title: "Disk swap", StartsAt: start, EndsAt: start.Add(time.Hour)}, e.now()); err != nil {
		t.Fatal(err)
	}
	anon := e.client(t)
	_, body := e.get(t, anon, "/s/default")
	if !strings.Contains(body, "Nov 8 12:00:00 UTC") {
		t.Fatalf("owner without a zone: status page should show UTC with its label:\n%s", body)
	}
	// Viewer zones do not matter: an anonymous visitor sees the owner's. Changing it drops the cached page.
	e.post(t, e.op, "/account/timezone", url.Values{"time_zone": {"Asia/Kolkata"}})
	_, body = e.get(t, anon, "/s/default")
	if !strings.Contains(body, "Nov 8 17:30:00 IST") || strings.Contains(body, "12:00:00 UTC") {
		t.Fatal("status page should use the owner's zone, with its abbreviation")
	}
	// Daily bars stay UTC days whatever the zone: 2026-11-07 12:00 UTC is already Nov 8 in Auckland.
	e.post(t, e.op, "/account/timezone", url.Values{"time_zone": {"Pacific/Auckland"}})
	tg, err := e.opWS.CreateTarget(ctx, store.Target{Name: "api", URL: "http://x.example", Interval: time.Hour, Timeout: time.Second}, e.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.mon.Reload(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	e.srv.resetStatus(e.opWS)
	_, body = e.get(t, anon, "/s/default")
	if !strings.Contains(body, `title="Nov 7 UTC · `) || strings.Contains(body, `title="Nov 8 UTC · `) || !strings.Contains(body, "days in UTC") {
		t.Fatal("daily bars should be UTC days")
	}
}

func TestSignInMethodMailUsesRecipientZone(t *testing.T) {
	at := time.Date(2026, 11, 17, 11, 0, 0, 0, time.UTC)
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	_, body := signInMethodMail("https://m.example", "GitHub", true, at, kolkata)
	if !strings.Contains(body, "on 2026-11-17 16:30 IST.") {
		t.Errorf("body = %q", body)
	}
	_, body = signInMethodMail("https://m.example", "GitHub", true, at, nil)
	if !strings.Contains(body, "on 2026-11-17 11:00 UTC.") {
		t.Errorf("body = %q", body)
	}
}
