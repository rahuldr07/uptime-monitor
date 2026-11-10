package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"uptimemon/internal/store"
)

func (e *env) countChecks(t *testing.T, targetID int64) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM checks WHERE target_id = ?`, targetID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeleteAccountRemovesEverything(t *testing.T) {
	e := newSignupEnv(t, `{"signup":"invite","base_url":"http://localhost:8080"}`)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	const email = "bob@example.com"
	bc, bws := e.account(t, email)
	tg, err := bws.CreateTarget(t.Context(), store.Target{Name: "B", URL: up.URL, Interval: time.Second, Timeout: time.Second}, e.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.mon.Reload(t.Context(), tg.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.countChecks(t, tg.ID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no check recorded before delete")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if resp, _ := e.get(t, e.client(t), "/s/bob"); resp.StatusCode != 200 {
		t.Fatalf("status page before delete: %d", resp.StatusCode)
	}

	resp, body := e.post(t, bc, "/account/delete", url.Values{"current": {testSecret}})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login?") {
		t.Fatalf("delete: %d %q %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	if _, body := e.get(t, e.client(t), resp.Header.Get("Location")); !strings.Contains(body, "Your account was deleted.") {
		t.Fatalf("flash missing: %s", body)
	}
	if e.status(t, bc) != http.StatusUnauthorized {
		t.Error("session survived")
	}
	n := e.countChecks(t, tg.ID)
	time.Sleep(1500 * time.Millisecond)
	if m := e.countChecks(t, tg.ID); m != n || m != 0 {
		t.Errorf("checks after delete: %d then %d, want none", n, m)
	}
	if _, ok := e.mon.Get(bws.ID(), tg.ID); ok {
		t.Error("monitor still tracks the target")
	}
	if resp, _ := e.get(t, e.client(t), "/s/bob"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status page after delete: %d", resp.StatusCode)
	}
	e.srv.statusCache.mu.Lock()
	for k := range e.srv.statusCache.entries {
		if wsOf(k) == bws.ID() {
			t.Errorf("cache entry survived: %q", k)
		}
	}
	e.srv.statusCache.mu.Unlock()
	if resp, _ := e.post(t, e.client(t), "/login", url.Values{"email": {email}, "password": {testSecret}}); resp.StatusCode == http.StatusSeeOther {
		t.Error("deleted account can sign in")
	}
	raw := e.invite(t, "")
	if resp, body := e.post(t, e.client(t), "/signup", signupForm(raw, email)); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign up again: %d %s", resp.StatusCode, body)
	}
}

func TestDeleteAccountWrongPassword(t *testing.T) {
	e := newEnv(t)
	bc, bws := e.account(t, "bob@example.com")
	resp, body := e.post(t, bc, "/account/delete", url.Values{"current": {"not the password"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, msgBadCurrent) {
		t.Fatalf("wrong password: %d %s", resp.StatusCode, body)
	}
	if e.status(t, bc) != http.StatusOK {
		t.Error("session was revoked")
	}
	if _, _, err := e.st.UserByEmail(context.Background(), "bob@example.com"); err != nil {
		t.Errorf("user gone: %v", err)
	}
	if _, err := e.st.Workspace(bws.ID()).Info(context.Background()); err != nil {
		t.Errorf("workspace gone: %v", err)
	}
}

func TestLastOperatorCannotDelete(t *testing.T) {
	e := newEnv(t)
	resp, body := e.post(t, e.op, "/account/delete", url.Values{"current": {testSecret}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "The last operator can&#39;t delete their account.") {
		t.Fatalf("last operator: %d %s", resp.StatusCode, body)
	}
	if e.status(t, e.op) != http.StatusOK {
		t.Error("operator session was revoked")
	}
	if _, err := e.opWS.Info(context.Background()); err != nil {
		t.Errorf("operator workspace gone: %v", err)
	}
}

func TestDeletedTargetNotRestarted(t *testing.T) {
	e := newEnv(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	bc, bws := e.account(t, "bob@example.com")
	tg, err := bws.CreateTarget(t.Context(), store.Target{Name: "B", URL: up.URL, Interval: time.Second, Timeout: time.Second}, e.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.mon.Reload(t.Context(), tg.ID); err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.post(t, bc, "/account/delete", url.Values{"current": {testSecret}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if err := e.mon.Reload(t.Context(), tg.ID); err != nil {
		t.Fatalf("reload of a deleted target: %v", err)
	}
	if _, ok := e.mon.Get(bws.ID(), tg.ID); ok {
		t.Error("deleted target has a runner again")
	}
	if len(e.mon.Snapshot(bws.ID())) != 0 {
		t.Error("snapshot not empty")
	}
}

// A request in flight can Reload a target between RemoveWorkspace and
// DeleteAccount; the tombstone must stop that runner for good.
func TestReloadDuringDeleteDoesNotLeaveRunner(t *testing.T) {
	e := newEnv(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	_, bws := e.account(t, "bob@example.com")
	tg, err := bws.CreateTarget(t.Context(), store.Target{Name: "B", URL: up.URL, Interval: time.Second, Timeout: time.Second}, e.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.mon.Reload(t.Context(), tg.ID); err != nil {
		t.Fatal(err)
	}
	e.mon.RemoveWorkspace(bws.ID())
	if err := e.mon.Reload(t.Context(), tg.ID); err != nil { // target still in the DB
		t.Fatal(err)
	}
	u, _, err := e.st.UserByEmail(t.Context(), "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := e.st.DeleteAccount(t.Context(), u.ID)
	if err != nil || len(deleted) != 1 {
		t.Fatalf("DeleteAccount: %v %v", deleted, err)
	}
	e.mon.ForgetWorkspace(bws.ID())
	if _, ok := e.mon.Get(bws.ID(), tg.ID); ok {
		t.Fatal("orphan runner state survived")
	}
	if err := e.mon.Reload(t.Context(), tg.ID); err != nil {
		t.Fatal(err)
	}
	n := e.countChecks(t, tg.ID)
	time.Sleep(1500 * time.Millisecond)
	if m := e.countChecks(t, tg.ID); m != n {
		t.Errorf("checks kept arriving for a deleted target: %d -> %d", n, m)
	}
}
