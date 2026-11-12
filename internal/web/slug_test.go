package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSlugChange(t *testing.T) {
	e := newEnv(t)
	save := func(c *http.Client, f url.Values) (*http.Response, string) {
		t.Helper()
		return e.post(t, c, "/status/settings", f)
	}
	// Warm the anonymous cache with the old title.
	if _, body := e.get(t, e.client(t), "/s/default"); !strings.Contains(body, "Service status") {
		t.Fatal("expected the default title")
	}
	resp, _ := save(e.op, url.Values{"title": {"Fresh title"}, "slug": {"new-name"}, "name": {"Renamed"}})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/s/new-name") {
		t.Fatalf("save: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	anon := e.client(t)
	if resp, body := e.get(t, anon, "/s/new-name"); resp.StatusCode != 200 || !strings.Contains(body, "Fresh title") {
		t.Fatalf("new address: %d, stale or missing title", resp.StatusCode)
	}
	if resp, _ := e.get(t, anon, "/s/default"); resp.StatusCode != 404 {
		t.Fatalf("old address: %d", resp.StatusCode)
	}
	if _, body := e.get(t, e.op, "/"); !strings.Contains(body, `href="/s/new-name"`) {
		t.Fatal("sidebar link should follow the slug")
	}
	if info, err := e.opWS.Info(t.Context()); err != nil || info.Name != "Renamed" {
		t.Fatalf("name: %+v %v", info, err)
	}

	e.account(t, "taken@example.com")
	for name, f := range map[string]struct {
		form url.Values
		msg  string
	}{
		"taken":   {url.Values{"slug": {"taken"}}, "That address is taken."},
		"invalid": {url.Values{"slug": {"Bad_Slug"}}, "Use 3–40 lowercase letters, digits and dashes."},
		"name":    {url.Values{"slug": {"new-name"}, "name": {strings.Repeat("x", 61)}}, msgWorkspaceLen},
	} {
		resp, body := save(e.op, f.form)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, f.msg) {
			t.Errorf("%s: %d, want 422 with %q", name, resp.StatusCode, f.msg)
		}
	}
	if info, _ := e.opWS.Info(t.Context()); info.Slug != "new-name" || info.Name != "Renamed" {
		t.Fatalf("failed saves changed the workspace: %+v", info)
	}
}

func TestStatusSettingsTextLengthAnd422AndSlugCase(t *testing.T) {
	e := newEnv(t)
	for name, f := range map[string]url.Values{
		"title":       {"title": {strings.Repeat("x", 81)}},
		"description": {"description": {strings.Repeat("x", 201)}},
	} {
		resp, body := e.post(t, e.op, "/status/settings", f)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, msgStatusTextLen) {
			t.Errorf("%s: %d, want 422 with %q", name, resp.StatusCode, msgStatusTextLen)
		}
	}
	// An upper-case slug is lowercased, not refused.
	resp, _ := e.post(t, e.op, "/status/settings", url.Values{"slug": {"  Mixed-Case "}})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/s/mixed-case") {
		t.Fatalf("mixed-case slug: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if info, _ := e.opWS.Info(t.Context()); info.Slug != "mixed-case" {
		t.Fatalf("slug = %q", info.Slug)
	}
}
