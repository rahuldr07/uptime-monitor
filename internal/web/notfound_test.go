package web

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestNotFoundSignedOut(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	resp, body := e.get(t, c, "/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", resp.Header.Get("Cache-Control"))
	}
	for _, want := range []string{"Page not found", "/does-not-exist", `href="/login"`, `href="/status"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, `class="side"`) {
		t.Error("signed-out page must not render the sidebar")
	}
}

func TestNotFoundSignedIn(t *testing.T) {
	e := newEnv(t)
	resp, body := e.get(t, e.op, "/nothing/here")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
	for _, want := range []string{"Page not found", `class="side"`, "/nothing/here", `href="/targets"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestNotFoundEscapesAndTruncatesPath(t *testing.T) {
	e := newEnv(t)
	_, body := e.get(t, e.op, "/%3Cb%3Ex%3C/b%3E")
	if strings.Contains(body, "<b>x</b>") || !strings.Contains(body, "&lt;b&gt;x&lt;/b&gt;") {
		t.Errorf("path not escaped: %s", body)
	}
	long := "/" + strings.Repeat("a", 300)
	_, body = e.get(t, e.op, long)
	if strings.Contains(body, strings.Repeat("a", 100)) || !strings.Contains(body, "…") {
		t.Error("long path not truncated")
	}
}

func TestNotFoundPlainWhereNoPageBelongs(t *testing.T) {
	e := newEnv(t)
	check := func(name string, resp *http.Response, body string) {
		t.Helper()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, resp.StatusCode)
		}
		if strings.Contains(body, "Page not found") || strings.Contains(body, "<html") || strings.Contains(body, `class="side"`) {
			t.Errorf("%s: want a plain 404, got %q", name, body)
		}
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/nope", nil)
	req.Header.Set("X-Partial", "1")
	resp, err := e.op.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	check("partial", resp, readBody(resp))

	resp, body := e.get(t, e.op, "/api/nope")
	check("api", resp, body)

	req, _ = http.NewRequest("GET", e.ts.URL+"/nope", nil)
	req.Header.Set("Accept", "application/json")
	resp, err = e.op.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	check("json", resp, readBody(resp))

	resp, body = e.post(t, e.op, "/nope", nil)
	check("post", resp, body)
}

func TestNotFoundKeepsMethodNotAllowed(t *testing.T) {
	e := newEnv(t)
	// A known path with the wrong method stays a 405, not a 404 page.
	resp, _ := e.post(t, e.op, "/events", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /events: %d, want 405", resp.StatusCode)
	}
}

func TestNotFoundFromHandlers(t *testing.T) {
	e := newEnv(t)
	resp, body := e.get(t, e.op, "/targets/999999")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "Page not found") {
		t.Errorf("missing target: %d, page = %v", resp.StatusCode, strings.Contains(body, "Page not found"))
	}
	// Anonymous visitors get the same page for an unknown status slug.
	resp, body = e.get(t, e.client(t), "/s/nope")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "Page not found") {
		t.Errorf("unknown slug: %d", resp.StatusCode)
	}
}

func TestRootStillServesOverview(t *testing.T) {
	e := newEnv(t)
	resp, body := e.get(t, e.op, "/")
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "Page not found") {
		t.Errorf("/: %d", resp.StatusCode)
	}
}

func readBody(resp *http.Response) string {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
