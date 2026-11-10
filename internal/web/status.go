package web

import (
	"container/list"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
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
	Slug        string // owner view: the settings form's current values
	WSName      string
	Groups      []statusGroup
	Total       int
	Upcoming    []store.Maintenance // scheduled within the next 7 days
}

// statusCacheTTL bounds how often anonymous visitors can make the server
// rebuild the public status page, which runs several queries per monitor
// on the same database connection that records checks.
const statusCacheTTL = 10 * time.Second

const (
	// statusCacheMaxBytes caps the total size of cached page bodies.
	statusCacheMaxBytes = 32 << 20
	// statusMaxRebuilds caps page builds running at once across all keys.
	statusMaxRebuilds = 2
	// statusRatePerMinute is the anonymous request budget per client address.
	statusRatePerMinute = 120
)

// pageCache holds rendered pages for anonymous visitors. Keys are
// "<workspace id>|theme[|partial]". Its lock only guards the maps: it is never held
// while a page is built or written, so a slow client cannot stall others.
// The zero value is ready to use, with no byte limit.
type pageCache struct {
	mu       sync.Mutex
	maxBytes int              // 0 means unlimited
	total    int              // bytes in all cached bodies
	gens     map[int64]uint64 // per workspace; bumped by reset, older entries are stale
	entries  map[string]*cachedPage
	lru      *list.List // of *cachedPage holding a body; front = most recently used
	sem      chan struct{}
}

type cachedPage struct {
	key      string
	body     []byte
	at       time.Time
	gen      uint64
	building *flight       // non-nil while a rebuild runs
	elem     *list.Element // in pageCache.lru while body != nil
}

// flight is one rebuild. Callers that wait on it share its result, so a
// failing build is not retried by each waiter in turn.
type flight struct {
	done  chan struct{} // closed when the rebuild ends
	body  []byte
	err   error
	retry bool // the leader gave up (its context ended); waiters start over
}

var errBuildPanicked = errors.New("status page build panicked")

// wsOf is the workspace ID a cache key belongs to (0 for a malformed key).
func wsOf(key string) int64 {
	id, _, _ := strings.Cut(key, "|")
	n, _ := strconv.ParseInt(id, 10, 64)
	return n
}

// reset marks every cached page of one workspace stale, e.g. after an
// admin change.
func (c *pageCache) reset(wsID int64) {
	c.mu.Lock()
	if c.gens == nil {
		c.gens = map[int64]uint64{}
	}
	c.gens[wsID]++
	c.mu.Unlock()
}

// forget removes the cached pages of a deleted workspace. A page that is
// rebuilding stays until its build ends; bumping the generation (always, even
// for a workspace never reset) marks it stale so it is never served.
func (c *pageCache) forget(wsID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.entries {
		if wsOf(p.key) == wsID && p.building == nil {
			c.drop(p)
		}
	}
	if c.gens == nil {
		c.gens = map[int64]uint64{}
	}
	c.gens[wsID]++
}

// drop removes p from the cache; caller holds mu and p is not building.
func (c *pageCache) drop(p *cachedPage) {
	if p.elem != nil {
		c.lru.Remove(p.elem)
		p.elem = nil
		c.total -= len(p.body)
	}
	if c.entries[p.key] == p {
		delete(c.entries, p.key)
	}
}

// get returns the page for key, calling build when it is missing or stale.
// Only one caller builds a key at a time. Meanwhile other callers get the
// previous copy if there is one, or wait for that build's result (or ctx).
// At most statusMaxRebuilds builds run at once; a caller that cannot get a
// slot gets the stale copy if there is one, else waits for a slot.
func (c *pageCache) get(ctx context.Context, key string, build func() ([]byte, error)) ([]byte, error) {
	for {
		body, retry, err := c.try(ctx, key, build)
		if !retry {
			return body, err
		}
	}
}

func (c *pageCache) try(ctx context.Context, key string, build func() ([]byte, error)) (body []byte, retry bool, err error) {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*cachedPage{}
		c.lru = list.New()
		c.sem = make(chan struct{}, statusMaxRebuilds)
	}
	p := c.entries[key]
	if p == nil {
		p = &cachedPage{key: key}
		c.entries[key] = p
	}
	wsID := wsOf(key)
	if p.elem != nil {
		c.lru.MoveToFront(p.elem)
	}
	if p.body != nil && p.gen == c.gens[wsID] && time.Since(p.at) < statusCacheTTL {
		body := p.body
		c.mu.Unlock()
		return body, false, nil
	}
	if f := p.building; f != nil {
		stale := p.body
		c.mu.Unlock()
		if stale != nil {
			return stale, false, nil // serve stale while one rebuild runs
		}
		select {
		case <-f.done:
			return f.body, f.retry && ctx.Err() == nil, f.err
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	got := false
	select {
	case c.sem <- struct{}{}:
		got = true
	default:
	}
	if !got && p.body != nil {
		stale := p.body
		c.mu.Unlock()
		return stale, false, nil // all rebuild slots are busy
	}
	f := &flight{done: make(chan struct{})}
	p.building = f
	gen := c.gens[wsID]
	c.mu.Unlock()
	if !got {
		select {
		case c.sem <- struct{}{}:
		case <-ctx.Done():
			c.mu.Lock()
			f.err, f.retry = ctx.Err(), true
			p.building = nil
			if p.body == nil {
				c.drop(p)
			}
			close(f.done)
			c.mu.Unlock()
			return nil, false, ctx.Err()
		}
	}
	body, err = c.rebuild(p, gen, f, build)
	return body, false, err
}

func (c *pageCache) rebuild(p *cachedPage, gen uint64, f *flight, build func() ([]byte, error)) (body []byte, err error) {
	finished := false
	defer func() {
		c.mu.Lock()
		<-c.sem
		if !finished {
			// build panicked: keep the previous copy and fail the waiters;
			// the panic itself continues to net/http, which logs it.
			body, err = nil, errBuildPanicked
		}
		p.building = nil
		if err == nil && body != nil {
			c.total += len(body) - len(p.body)
			p.body, p.at, p.gen = body, time.Now(), gen
			if p.elem == nil {
				p.elem = c.lru.PushFront(p)
			} else {
				c.lru.MoveToFront(p.elem)
			}
			c.evict()
		} else if p.body == nil {
			c.drop(p)
		}
		f.body, f.err = body, err
		close(f.done)
		c.mu.Unlock()
	}()
	body, err = build()
	finished = true
	return body, err
}

// evict drops the least recently used pages until the total fits; caller
// holds mu. A page that is rebuilding is never evicted.
func (c *pageCache) evict() {
	for el := c.lru.Back(); el != nil && c.maxBytes > 0 && c.total > c.maxBytes; {
		p := el.Value.(*cachedPage)
		el = el.Prev()
		if p.building == nil {
			c.drop(p)
		}
	}
}

// buildCtx is the context for a shared page build. It keeps the leader
// request's values but not its cancellation, so one visitor leaving cannot
// fail the build for every visitor waiting on it.
func buildCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
}

// statusLimited answers 429 and reports true when the client has used up its
// anonymous status page budget. Every call counts, allowed or not.
func (s *Server) statusLimited(w http.ResponseWriter, r *http.Request) bool {
	key := clientKey(r, s.cfg.ProxyHops())
	ok, retry := s.statusLimiter.Allow(key)
	s.statusLimiter.Hit(key)
	if ok {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	io.WriteString(w, "too many requests")
	return true
}

// statusRedirect sends /status to the signed-in user's status page, or to
// the operator's (workspace 1) for everyone else.
func (s *Server) statusRedirect(w http.ResponseWriter, r *http.Request) {
	ws := s.sys.Workspace(operatorWorkspaceID)
	if a, ok := s.currentAuth(r); ok {
		ws = a.WS
	}
	info, err := ws.Info(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/s/"+info.Slug, http.StatusFound)
}

// statusURL is the path of ws's public status page.
func (s *Server) statusURL(ctx context.Context, ws *store.WS) string {
	info, err := ws.Info(ctx)
	if err != nil {
		return "/status"
	}
	return "/s/" + info.Slug
}

// resetStatus invalidates the cached public pages of ws.
func (s *Server) resetStatus(ws *store.WS) {
	s.statusCache.reset(ws.ID())
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	// Anonymous visitors are rate limited before any lookup.
	a, signedIn := s.currentAuth(r)
	if !signedIn && s.statusLimited(w, r) {
		return
	}
	info, err := s.sys.WorkspaceBySlug(r.Context(), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	ws := s.sys.Workspace(info.ID)
	// The owner sees live data and the settings form; everyone else shares a
	// briefly cached copy per theme and render mode. Anonymous visitors never
	// bypass the cache, whatever headers they send.
	if signedIn {
		if info.ID == a.Workspace.ID {
			s.statusOwner(w, r, a, info)
			return
		}
		if s.statusLimited(w, r) {
			return
		}
	}
	// Both parts take a handful of fixed values, so visitors cannot grow the map.
	theme, partial := themeFrom(r), r.Header.Get("X-Partial") == "1"
	key := strconv.FormatInt(info.ID, 10) + "|" + theme
	if partial {
		key += "|partial"
	}
	body, err := s.statusCache.get(r.Context(), key, func() ([]byte, error) {
		bctx, cancel := buildCtx(r.Context())
		defer cancel()
		d, err := s.statusData(bctx, ws, theme, partial)
		if err != nil {
			return nil, err
		}
		return s.renderPage("status", partial, d)
	})
	if err != nil {
		if r.Context().Err() == nil {
			s.fail(w, err)
		}
		return
	}
	writeHTML(w, http.StatusOK, body)
}

// statusOwner renders the live page with the settings form.
func (s *Server) statusOwner(w http.ResponseWriter, r *http.Request, a *authCtx, info store.Workspace) {
	s.statusOwnerCode(w, r, a, info, http.StatusOK, "")
}

// statusOwnerCode renders the owner view with code, showing errMsg when set.
func (s *Server) statusOwnerCode(w http.ResponseWriter, r *http.Request, a *authCtx, info store.Workspace, code int, errMsg string) {
	ws := a.WS
	// The owner view carries the settings form, so unlike the public page
	// it must not be framed, not even by a same-site origin that would
	// receive the SameSite=Lax session cookie.
	w.Header().Set("Content-Security-Policy", cspBase+"frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	d, err := s.statusData(r.Context(), ws, themeFrom(r), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.base = s.base(r, a, "status", d.StatusTitle)
	d.Error = errMsg
	d.Slug, d.WSName = info.Slug, info.Name
	scheme := "http"
	if s.isHTTPS(r) {
		scheme = "https"
	}
	d.ShareURL = scheme + "://" + r.Host + "/s/" + info.Slug
	s.renderStatus(w, r, code, "status", d)
}

// statusData builds the anonymous view of ws. It needs no request, and
// nothing request-specific enters it, so the result is safe to cache.
func (s *Server) statusData(ctx context.Context, ws *store.WS, theme string, partial bool) (statusData, error) {
	d := statusData{base: s.baseFor(ctx, ws, "status", "Status", theme, false)}
	d.StatusTitle, d.StatusDesc = s.statusMeta(ctx, ws)
	d.Title = d.StatusTitle
	views, err := s.views(ctx, ws, viewOpts{days: true})
	if err != nil {
		return d, err
	}
	d.Total = len(views)
	sched, err := ws.ListMaintenance(ctx, d.Now, 20)
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

const (
	msgBadSlug   = "Use 3–40 lowercase letters, digits and dashes."
	msgSlugTaken = "That address is taken."
)

func (s *Server) statusSettings(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	title := strings.TrimSpace(r.FormValue("title"))
	desc := strings.TrimSpace(r.FormValue("description"))
	if len(title) > 80 || len(desc) > 200 {
		redirect(w, r, s.statusURL(r.Context(), ws), "Title or description too long.")
		return
	}
	info, err := ws.Info(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	slug := strings.TrimSpace(r.FormValue("slug"))
	name := strings.TrimSpace(r.FormValue("name"))
	newSlug := slug != "" && slug != info.Slug
	newName := name != "" && name != info.Name
	switch {
	case newSlug && !store.ValidSlug(slug):
		s.statusOwnerCode(w, r, a, info, http.StatusUnprocessableEntity, msgBadSlug)
		return
	case newName && !validWorkspaceName(name):
		s.statusOwnerCode(w, r, a, info, http.StatusUnprocessableEntity, msgWorkspaceLen)
		return
	}
	if newSlug {
		switch err := ws.SetSlug(r.Context(), slug); {
		case errors.Is(err, store.ErrSlugTaken):
			s.statusOwnerCode(w, r, a, info, http.StatusUnprocessableEntity, msgSlugTaken)
			return
		case errors.Is(err, store.ErrInvalidSlug):
			s.statusOwnerCode(w, r, a, info, http.StatusUnprocessableEntity, msgBadSlug)
			return
		case err != nil:
			s.fail(w, err)
			return
		}
	}
	if newName {
		if err := ws.Rename(r.Context(), name); err != nil {
			s.fail(w, err)
			return
		}
	}
	if err := ws.SetSetting(r.Context(), "status_title", title); err != nil {
		s.fail(w, err)
		return
	}
	if err := ws.SetSetting(r.Context(), "status_desc", desc); err != nil {
		s.fail(w, err)
		return
	}
	s.resetStatus(ws)
	redirect(w, r, s.statusURL(r.Context(), ws), "Status page saved.")
}

// operatorWorkspaceID is the workspace of the instance operator, created
// first; /status points anonymous visitors at it.
const operatorWorkspaceID int64 = 1
