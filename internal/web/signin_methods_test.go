package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/store"
)

// socialClient signs a fresh social-only user in through the fake and returns its client.
func (o *oenv) socialClient(t *testing.T, email, provider, subject string) (*http.Client, store.User) {
	t.Helper()
	u := o.social(t, email, provider, subject)
	f := o.fake(provider)
	f.Subject, f.Email = subject, email
	c := o.client(t)
	if r, _ := o.run(t, c, provider, ""); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in: %d", r.StatusCode)
	}
	return c, u
}

var newPW = url.Values{"new": {testSecret}, "confirm": {testSecret}}

func TestSetFirstPasswordRecentAuth(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	c, u := o.socialClient(t, "person@example.com", "google", "12345")

	// The page offers "Set a password" and no current-password prompts.
	_, body := o.get(t, c, "/account")
	if !strings.Contains(body, "Set a password") || strings.Contains(body, `name="current"`) {
		t.Errorf("account page: %s", body)
	}
	// Validation comes first.
	if r, _ := o.post(t, c, "/account/password", url.Values{"new": {"short"}, "confirm": {"short"}}); r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("short: %d", r.StatusCode)
	}
	if r, _ := o.post(t, c, "/account/password", url.Values{"new": {testSecret}, "confirm": {"other other other"}}); r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("mismatch: %d", r.StatusCode)
	}

	// Outside the window: refused, with the reauth link; nothing changes.
	o.advance(recentAuthWindow + time.Second)
	resp, body := o.post(t, c, "/account/password", newPW)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Sign in again to continue") ||
		!strings.Contains(body, "For your security, sign in again with Google to continue") ||
		!strings.Contains(body, `/auth/google/start?intent=reauth`) {
		t.Fatalf("outside window: %d %s", resp.StatusCode, body)
	}
	if has, _ := o.st.HasPassword(t.Context(), u.ID); has {
		t.Fatal("password set outside the window")
	}
	_, body = o.get(t, c, "/account")
	if strings.Contains(body, "Set a password</button>") || !strings.Contains(body, "Sign in with Google") {
		t.Errorf("account page outside window: %s", body)
	}

	// Reauth, then it works, and other sessions are signed out.
	other := o.client(t)
	if r, _ := o.run(t, other, "google", ""); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("second session: %d", r.StatusCode)
	}
	if r, _ := o.run(t, c, "google", "intent=reauth&next=%2Faccount"); r.Header.Get("Location") != "/account" {
		t.Fatalf("reauth: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	resp, _ = o.post(t, c, "/account/password", newPW)
	if p, m := flashOf(t, resp); resp.StatusCode != http.StatusSeeOther || p != "/account" || m != msgPasswordChanged {
		t.Fatalf("set: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if has, _ := o.st.HasPassword(t.Context(), u.ID); !has {
		t.Fatal("password not set")
	}
	if signedIn(t, o, other) {
		t.Error("other session survived")
	}
	if !signedIn(t, o, c) {
		t.Error("current session was signed out")
	}
	// The password now works for sign-in.
	if r, _ := o.post(t, o.client(t), "/login", url.Values{"email": {"person@example.com"}, "password": {testSecret}}); r.StatusCode != http.StatusSeeOther {
		t.Errorf("password sign-in: %d", r.StatusCode)
	}
	// And the page now offers Change password.
	if _, body := o.get(t, c, "/account"); !strings.Contains(body, "Change password") {
		t.Errorf("account page after: %s", body)
	}
}

func TestAccountDeleteSocialOnly(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	c, u := o.socialClient(t, "person@example.com", "google", "12345")
	o.advance(recentAuthWindow + time.Second)
	resp, body := o.post(t, c, "/account/delete", nil)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "sign in again with Google") {
		t.Fatalf("outside window: %d %s", resp.StatusCode, body)
	}
	if _, err := o.st.UserByID(t.Context(), u.ID); err != nil {
		t.Fatal("deleted outside the window")
	}
	if r, _ := o.run(t, c, "google", "intent=reauth"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("reauth: %d", r.StatusCode)
	}
	_, body = o.get(t, c, "/account")
	if strings.Contains(body, `name="current"`) || !strings.Contains(body, "Delete account") {
		t.Errorf("delete form inside window: %s", body)
	}
	resp, _ = o.post(t, c, "/account/delete", nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") == "" || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("inside window: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, err := o.st.UserByID(t.Context(), u.ID); err == nil {
		t.Error("account still exists")
	}
}

func TestDisconnectProvider(t *testing.T) {
	o := newOEnv(t, `{}`)
	alice, _ := o.account(t, "alice@example.com")
	aliceID := o.userID(t, "alice@example.com")
	if r, _ := o.run(t, alice, "google", "intent=link"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("link: %d", r.StatusCode)
	}
	_, body := o.get(t, alice, "/account")
	for _, want := range []string{"person@example.com", "/account/identities/google/delete", `/auth/github/start?intent=link`} {
		if !strings.Contains(body, want) {
			t.Errorf("account page lacks %q", want)
		}
	}
	if strings.Contains(body, `/auth/google/start?intent=link`) {
		t.Error("connect link for a connected provider")
	}

	// With a password it must be confirmed.
	del := "/account/identities/google/delete"
	if r, _ := o.post(t, alice, del, nil); r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("no password: %d", r.StatusCode)
	}
	if r, _ := o.post(t, alice, del, url.Values{"current": {"wrong wrong wrong"}}); r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("wrong password: %d", r.StatusCode)
	}
	if ids, _ := o.st.Identities(t.Context(), aliceID); len(ids) != 1 {
		t.Fatal("disconnected without the password")
	}
	resp, _ := o.post(t, alice, del, url.Values{"current": {testSecret}})
	if p, m := flashOf(t, resp); resp.StatusCode != http.StatusSeeOther || p != "/account" || m != "Google disconnected." {
		t.Fatalf("disconnect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if ids, _ := o.st.Identities(t.Context(), aliceID); len(ids) != 0 {
		t.Errorf("identity remains: %+v", ids)
	}
	if !strings.Contains(o.logs.String(), "event=oauth_unlink") {
		t.Error("no oauth_unlink event")
	}
	// Not connected, and not a provider.
	if r, _ := o.post(t, alice, del, url.Values{"current": {testSecret}}); r.StatusCode != http.StatusNotFound {
		t.Errorf("not connected: %d", r.StatusCode)
	}
	if r, _ := o.post(t, alice, "/account/identities/nope/delete", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown provider: %d", r.StatusCode)
	}
}

func TestDisconnectLastMethodAndRecentAuth(t *testing.T) {
	o := newOEnv(t, `{}`)
	c, u := o.socialClient(t, "person@example.com", "google", "12345")
	// The only method cannot go, even with a recent sign-in.
	resp, body := o.post(t, c, "/account/identities/google/delete", nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, msgLastMethod) {
		t.Fatalf("last method: %d %s", resp.StatusCode, body)
	}
	if ids, _ := o.st.Identities(t.Context(), u.ID); len(ids) != 1 {
		t.Fatal("last identity removed")
	}
	// A second provider makes the first removable, but only with a recent sign-in.
	if r, _ := o.run(t, c, "github", "intent=link"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("link github: %d", r.StatusCode)
	}
	o.advance(recentAuthWindow + time.Second)
	resp, body = o.post(t, c, "/account/identities/google/delete", nil)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Sign in again to continue") {
		t.Fatalf("outside window: %d %s", resp.StatusCode, body)
	}
	if ids, _ := o.st.Identities(t.Context(), u.ID); len(ids) != 2 {
		t.Fatal("disconnected outside the window")
	}
	if r, _ := o.run(t, c, "google", "intent=reauth"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("reauth: %d", r.StatusCode)
	}
	resp, _ = o.post(t, c, "/account/identities/google/delete", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("inside window: %d", resp.StatusCode)
	}
	if ids, _ := o.st.Identities(t.Context(), u.ID); len(ids) != 1 || ids[0].Provider != "github" {
		t.Errorf("identities: %+v", ids)
	}
	// Now github is the last one again.
	if r, _ := o.post(t, c, "/account/identities/github/delete", nil); r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("last again: %d", r.StatusCode)
	}
}

// A social-only operator deletes a user with a recent sign-in instead of a password.
func TestAdminDeleteSocialOnlyOperator(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	if _, _, err := o.st.CreateAccount(t.Context(), store.AccountInput{Email: "socialop@example.com", WorkspaceName: "Ops",
		Slug: "ops", Verified: true, Operator: true,
		Identity: &store.NewIdentity{Provider: "google", Subject: "9", Email: "socialop@example.com"}, Now: o.now()}); err != nil {
		t.Fatal(err)
	}
	o.g.Subject, o.g.Email = "9", "socialop@example.com"
	op := o.client(t)
	if r, _ := o.run(t, op, "google", ""); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("operator sign-in: %d", r.StatusCode)
	}
	_, _ = o.account(t, "victim@example.com")
	vid := o.userID(t, "victim@example.com")
	path := "/admin/users/" + strconv.FormatInt(vid, 10) + "/delete"

	_, body := o.get(t, op, path)
	if strings.Contains(body, `name="current"`) || !strings.Contains(body, "Delete user") {
		t.Errorf("delete page inside window: %s", body)
	}
	o.advance(recentAuthWindow + time.Second)
	_, body = o.get(t, op, path)
	if strings.Contains(body, `name="current"`) || !strings.Contains(body, "sign in again with Google") ||
		!strings.Contains(body, url.QueryEscape(path)) {
		t.Errorf("delete page outside window: %s", body)
	}
	resp, body := o.post(t, op, path, nil)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Sign in again to continue") {
		t.Fatalf("outside window: %d %s", resp.StatusCode, body)
	}
	if _, err := o.st.UserByID(t.Context(), vid); err != nil {
		t.Fatal("deleted outside the window")
	}
	if r, _ := o.run(t, op, "google", "intent=reauth&next="+url.QueryEscape(path)); r.Header.Get("Location") != path {
		t.Fatalf("reauth: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if resp, _ := o.post(t, op, path, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("inside window: %d", resp.StatusCode)
	}
	if _, err := o.st.UserByID(t.Context(), vid); err == nil {
		t.Error("user not deleted")
	}
}

func TestAdminProviderBadges(t *testing.T) {
	o := newOEnv(t, `{}`)
	both := o.social(t, "both@example.com", "google", "1")
	if err := o.st.LinkIdentity(t.Context(), both.ID, "github", "2", "both@example.com", o.now()); err != nil {
		t.Fatal(err)
	}
	o.social(t, "gonly@example.com", "google", "3")
	_, _ = o.account(t, "plain@example.com")
	_, body := o.get(t, o.op, "/admin/users")
	rows := strings.Split(body, "<tr data-id=")
	got := map[string][2]bool{}
	for _, r := range rows[1:] {
		for _, email := range []string{"both@example.com", "gonly@example.com", "plain@example.com"} {
			if strings.Contains(r, `data-label="`+email+`"`) {
				got[email] = [2]bool{strings.Contains(r, `<span class="badge">Google</span>`), strings.Contains(r, `<span class="badge">GitHub</span>`)}
			}
		}
	}
	want := map[string][2]bool{"both@example.com": {true, true}, "gonly@example.com": {true, false}, "plain@example.com": {false, false}}
	for e, w := range want {
		if got[e] != w {
			t.Errorf("%s badges = %v, want %v", e, got[e], w)
		}
	}
}
