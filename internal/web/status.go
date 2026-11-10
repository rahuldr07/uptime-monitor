package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"uptimemon/internal/store"
)

// ---- status page ----

type statusGroup struct {
	Name    string
	Targets []targetView
}

type statusData struct {
	base
	StatusTitle string
	StatusDesc  string
	ShareURL    string
	Groups      []statusGroup
	Total       int
	Upcoming    []store.Maintenance // scheduled within the next 7 days
}

// statusCacheTTL bounds how often anonymous visitors can make the server
// rebuild the public status page, which runs several queries per monitor
// on the same database connection that records checks.
const statusCacheTTL = 10 * time.Second

// pageCache holds rendered pages for anonymous visitors. Its lock only
// guards the map: it is never held while a page is built or written, so a
// slow client cannot stall other visitors.
type pageCache struct {
	mu      sync.Mutex
	gen     uint64 // bumped by reset; entries from older generations are stale
	entries map[string]*cachedPage
}

type cachedPage struct {
	body     []byte
	at       time.Time
	gen      uint64
	building *flight // non-nil while a rebuild runs
}

// flight is one rebuild. Callers that wait on it share its result, so a
// failing build is not retried by each waiter in turn.
type flight struct {
	done chan struct{} // closed when the rebuild ends
	body []byte
	err  error
}

var errBuildPanicked = errors.New("status page build panicked")

// reset marks every cached page stale, e.g. after an admin change.
func (c *pageCache) reset() {
	c.mu.Lock()
	c.gen++
	c.mu.Unlock()
}

// get returns the page for key, calling build when it is missing or stale.
// Only one caller builds a key at a time. Meanwhile other callers get the
// previous copy if there is one, or wait for that build's result (or ctx).
func (c *pageCache) get(ctx context.Context, key string, build func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*cachedPage{}
	}
	p := c.entries[key]
	if p == nil {
		p = &cachedPage{}
		c.entries[key] = p
	}
	if p.body != nil && p.gen == c.gen && time.Since(p.at) < statusCacheTTL {
		body := p.body
		c.mu.Unlock()
		return body, nil
	}
	if f := p.building; f != nil {
		stale := p.body
		c.mu.Unlock()
		if stale != nil {
			return stale, nil // serve stale while one rebuild runs
		}
		select {
		case <-f.done:
			return f.body, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	p.building = f
	gen := c.gen
	c.mu.Unlock()
	return c.rebuild(p, gen, f, build)
}

func (c *pageCache) rebuild(p *cachedPage, gen uint64, f *flight, build func() ([]byte, error)) (body []byte, err error) {
	finished := false
	defer func() {
		c.mu.Lock()
		if !finished {
			// build panicked: keep the previous copy and fail the waiters;
			// the panic itself continues to net/http, which logs it.
			body, err = nil, errBuildPanicked
		}
		if err == nil && body != nil {
			p.body, p.at, p.gen = body, time.Now(), gen
		}
		f.body, f.err = body, err
		p.building = nil
		close(f.done)
		c.mu.Unlock()
	}()
	body, err = build()
	finished = true
	return body, err
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	ws := s.ws(r)
	// Admins see live data and the settings form; everyone else shares a
	// briefly cached copy per theme and render mode. Anonymous visitors never
	// bypass the cache, whatever headers they send.
	if s.isAdmin(r) {
		// The admin view carries the settings form, so unlike the public page
		// it must not be framed, not even by a same-site origin that would
		// receive the SameSite=Lax session cookie.
		w.Header().Set("Content-Security-Policy", cspBase+"frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		d, err := s.statusData(r, ws)
		if err != nil {
			s.fail(w, err)
			return
		}
		s.render(w, r, "status", d)
		return
	}
	// Both parts take a handful of fixed values, so visitors cannot grow the map.
	key := themeFrom(r)
	if r.Header.Get("X-Partial") == "1" {
		key += "|partial"
	}
	body, err := s.statusCache.get(r.Context(), key, func() ([]byte, error) {
		d, err := s.statusData(r, ws)
		if err != nil {
			return nil, err
		}
		return s.renderBytes(r, "status", d)
	})
	if err != nil {
		if r.Context().Err() == nil {
			s.fail(w, err)
		}
		return
	}
	writeHTML(w, http.StatusOK, body)
}

func (s *Server) statusData(r *http.Request, ws *store.WS) (statusData, error) {
	d := statusData{base: s.base(r, ws, "status", "Status")}
	d.StatusTitle, d.StatusDesc = s.statusMeta(r.Context(), ws)
	d.Title = d.StatusTitle
	scheme := "http"
	if s.isHTTPS(r) {
		scheme = "https"
	}
	d.ShareURL = scheme + "://" + r.Host + "/status"
	views, err := s.views(r, ws, viewOpts{days: true})
	if err != nil {
		return d, err
	}
	d.Total = len(views)
	sched, err := ws.ListMaintenance(r.Context(), d.Now, 20)
	if err != nil {
		return d, err
	}
	for _, m := range sched {
		if m.Upcoming(d.Now) && m.StartsAt.Before(d.Now.Add(7*24*time.Hour)) {
			d.Upcoming = append(d.Upcoming, m)
		}
	}
	byGroup := map[string]*statusGroup{}
	for _, v := range views {
		g := v.State.Target.Group
		if byGroup[g] == nil {
			byGroup[g] = &statusGroup{Name: g}
		}
		byGroup[g].Targets = append(byGroup[g].Targets, v)
	}
	for _, g := range byGroup {
		d.Groups = append(d.Groups, *g)
	}
	sort.Slice(d.Groups, func(i, j int) bool {
		if (d.Groups[i].Name == "") != (d.Groups[j].Name == "") {
			return d.Groups[i].Name == "" // ungrouped first
		}
		return d.Groups[i].Name < d.Groups[j].Name
	})
	return d, nil
}

func (s *Server) statusMeta(ctx context.Context, ws *store.WS) (title, desc string) {
	title, _ = ws.GetSetting(ctx, "status_title")
	desc, _ = ws.GetSetting(ctx, "status_desc")
	if title == "" {
		title = "Service status"
	}
	return title, desc
}

func (s *Server) statusSettings(w http.ResponseWriter, r *http.Request) {
	ws := s.ws(r)
	title := strings.TrimSpace(r.FormValue("title"))
	desc := strings.TrimSpace(r.FormValue("description"))
	if len(title) > 80 || len(desc) > 200 {
		redirect(w, r, "/status", "Title or description too long.")
		return
	}
	if err := ws.SetSetting(r.Context(), "status_title", title); err != nil {
		s.fail(w, err)
		return
	}
	if err := ws.SetSetting(r.Context(), "status_desc", desc); err != nil {
		s.fail(w, err)
		return
	}
	s.statusCache.reset()
	redirect(w, r, "/status", "Status page saved.")
}
