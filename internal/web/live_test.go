package web

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/password"
)

func (e *env) openEvents(t *testing.T) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.ts.URL+"/events", nil)
	e.authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestEventsClosesOnRevoke(t *testing.T) {
	old := eventsRecheck
	eventsRecheck = 50 * time.Millisecond
	t.Cleanup(func() { eventsRecheck = old })
	e := newEnv(t)
	resp := e.openEvents(t)
	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "retry:") {
		t.Fatalf("no stream preamble: %q", sc.Text())
	}
	u, _, err := e.st.UserByEmail(context.Background(), opEmail)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.DeleteUserSessions(context.Background(), u.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		io.Copy(io.Discard, resp.Body)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream still open after the session was revoked")
	}
}

func TestEventsStreamCap(t *testing.T) {
	e := newEnv(t)
	var open []*http.Response
	for i := 0; i < 5; i++ {
		resp := e.openEvents(t)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: %d", i+1, resp.StatusCode)
		}
		open = append(open, resp)
	}
	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/events", nil)
	e.authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("6th stream: %d", resp.StatusCode)
	}
	// Closing one stream must release its slot.
	open[0].Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/events", nil)
		e.authorize(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		code := resp.StatusCode
		resp.Body.Close()
		if code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot not released after closing a stream: %d", code)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAPILimitPerSession(t *testing.T) {
	e := newEnv(t)
	for i := 1; i <= 120; i++ {
		if resp, _ := e.get(t, e.op, "/api/status"); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	resp, body := e.get(t, e.op, "/api/status")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") ||
		!strings.Contains(body, `"error": "too many requests"`) {
		t.Fatalf("121st: %d %q %q", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	if resp, _ := e.get(t, e.signIn(t, opEmail, testSecret), "/api/status"); resp.StatusCode != http.StatusOK {
		t.Fatalf("a new session must not be limited: %d", resp.StatusCode)
	}
}

func TestStatusOwnerViewOnlyForOwner(t *testing.T) {
	e := newEnv(t)
	const marker = "Status page settings"
	if _, body := e.get(t, e.op, "/s/default"); !strings.Contains(body, marker) {
		t.Fatal("owner should see the settings form")
	}
	bob, _ := e.account(t, "bob@example.com")
	resp, body := e.get(t, bob, "/s/default")
	if resp.StatusCode != http.StatusOK || strings.Contains(body, marker) {
		t.Fatalf("another account must get the anonymous page: %d", resp.StatusCode)
	}
}

func TestStatusShareURL(t *testing.T) {
	e0 := newEnv(t)
	_, body := e0.get(t, e0.op, "/s/default")
	if !strings.Contains(body, `<span class="mono">http://127.0.0.1:`) || !strings.Contains(body, "/s/default</span>") {
		t.Fatalf("without base_url the share link follows the request: %s", body)
	}
	e := newEnvConfig(t, password.New(password.Params{Memory: 64, Time: 1, Threads: 1}), `{"base_url": "https://status.example.com"}`)
	if _, body := e.get(t, e.op, "/s/default"); !strings.Contains(body, `<span class="mono">https://status.example.com/s/default</span>`) {
		t.Fatalf("base_url must be the share link origin: %s", body)
	}
}
