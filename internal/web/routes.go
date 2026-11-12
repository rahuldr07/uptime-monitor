package web

import (
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// apiRatePerMinute is the JSON API budget per session.
const apiRatePerMinute = 120

// apiLimited counts each request against the caller's session and answers
// JSON 429 once the budget is spent.
func (s *Server) apiLimited(h func(http.ResponseWriter, *http.Request, *authCtx)) func(http.ResponseWriter, *http.Request, *authCtx) {
	return func(w http.ResponseWriter, r *http.Request, a *authCtx) {
		if ok, retry := s.apiLimiter.Take(a.sessionHash); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
			writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "too many requests"})
			return
		}
		h(w, r, a)
	}
}

type routeClass int

const (
	classPublic    routeClass = iota + 1 // no auth; the zero value is invalid on purpose
	classSetup                           // only exists while no account does
	classGuest                           // signed-in users are sent to /
	classUser                            // any signed-in user
	classWorkspace                       // a signed-in user acting on their own workspace
)

type route struct {
	method  string // "GET" or "POST"
	pattern string // ServeMux pattern without the method, e.g. "/targets/{id}"
	class   routeClass
	handler http.HandlerFunc                                   // public, setup and guest routes
	authed  func(http.ResponseWriter, *http.Request, *authCtx) // user and workspace routes
}

// routeTable is the single source of truth for every HTTP route.
func (s *Server) routeTable() []route {
	static, _ := fs.Sub(staticFS, "static")
	files := http.StripPrefix("/static/", cacheControl(http.FileServer(http.FS(static))))
	return []route{
		{"GET", "/static/", classPublic, files.ServeHTTP, nil},
		{"GET", "/healthz", classPublic, s.healthz, nil},
		{"GET", "/status", classPublic, s.statusRedirect, nil},
		{"GET", "/s/{slug}", classPublic, s.statusPage, nil},
		{"GET", "/verify", classPublic, s.verifyPage, nil},
		{"POST", "/verify", classPublic, s.verifyPost, nil},
		{"POST", "/verify/resend", classPublic, s.verifyResend, nil},
		{"GET", "/setup", classSetup, s.setupPage, nil},
		{"POST", "/setup", classSetup, s.setupPost, nil},
		{"GET", "/login", classGuest, s.loginPage, nil},
		{"POST", "/login", classGuest, s.loginPost, nil},
		{"GET", "/signup", classGuest, s.signupPage, nil},
		{"POST", "/signup", classGuest, s.signupPost, nil},
		{"GET", "/reset", classGuest, s.resetPage, nil},
		{"POST", "/reset", classGuest, s.resetPost, nil},
		{"POST", "/logout", classUser, nil, s.logout},
		{"GET", "/account", classUser, nil, s.accountPage},
		{"POST", "/account/password", classUser, nil, s.accountPassword},
		{"POST", "/account/signout-all", classUser, nil, s.accountSignOutAll},
		{"POST", "/account/delete", classUser, nil, s.accountDelete},

		{"GET", "/{$}", classWorkspace, nil, s.overview},
		{"GET", "/events", classWorkspace, nil, s.events},
		{"GET", "/targets", classWorkspace, nil, s.monitorsPage},
		{"POST", "/targets", classWorkspace, nil, s.targetCreate},
		{"POST", "/targets/test", classWorkspace, nil, s.targetTest},
		{"GET", "/targets/{id}", classWorkspace, nil, s.targetPage},
		{"POST", "/targets/{id}", classWorkspace, nil, s.targetUpdate},
		{"POST", "/targets/{id}/delete", classWorkspace, nil, s.targetDelete},
		{"POST", "/targets/{id}/pause", classWorkspace, nil, s.targetPause},
		{"POST", "/targets/{id}/mute", classWorkspace, nil, s.targetMute},
		{"POST", "/targets/{id}/check", classWorkspace, nil, s.targetCheck},
		{"GET", "/incidents", classWorkspace, nil, s.incidentsPage},
		{"POST", "/incidents/{id}/ack", classWorkspace, nil, s.incidentAck},
		{"POST", "/incidents/{id}/note", classWorkspace, nil, s.incidentNote},
		{"GET", "/alerts", classWorkspace, nil, s.alertsPage},
		{"POST", "/alerts", classWorkspace, nil, s.alertsSave},
		{"POST", "/alerts/test", classWorkspace, nil, s.alertsTest},
		{"POST", "/status/settings", classWorkspace, nil, s.statusSettings},
		{"GET", "/maintenance", classWorkspace, nil, s.maintenancePage},
		{"POST", "/maintenance", classWorkspace, nil, s.maintenanceCreate},
		{"POST", "/maintenance/{id}/end", classWorkspace, nil, s.maintenanceEnd},
		{"POST", "/maintenance/{id}/delete", classWorkspace, nil, s.maintenanceDelete},
		{"GET", "/api/maintenance", classWorkspace, nil, s.apiMaintenance},
		{"GET", "/api/status", classWorkspace, nil, s.apiStatus},
		{"GET", "/api/targets/{id}/checks", classWorkspace, nil, s.apiChecks},
		{"GET", "/api/incidents", classWorkspace, nil, s.apiIncidents},
	}
}

// routes builds s.mux from routeTable, wrapping each handler by its class.
func (s *Server) routes() {
	m := http.NewServeMux()
	for _, r := range s.routeTable() {
		h := r.handler
		switch r.class {
		case classPublic:
		case classSetup:
			inner := h
			h = func(w http.ResponseWriter, req *http.Request) {
				if !s.setupMode.Load() {
					http.NotFound(w, req)
					return
				}
				inner(w, req)
			}
		case classGuest:
			inner := h
			h = func(w http.ResponseWriter, req *http.Request) {
				if _, ok := s.currentAuth(req); ok {
					http.Redirect(w, req, "/", http.StatusSeeOther)
					return
				}
				inner(w, req)
			}
		case classUser, classWorkspace:
			if r.authed == nil {
				panic(fmt.Sprintf("web: route %s %s has no authed handler", r.method, r.pattern))
			}
			authed := r.authed
			if r.class == classWorkspace && strings.HasPrefix(r.pattern, "/api/") {
				authed = s.apiLimited(authed)
			}
			h = s.requireUser(authed)
		default:
			panic(fmt.Sprintf("web: route %s %s has no class", r.method, r.pattern))
		}
		if h == nil {
			panic(fmt.Sprintf("web: route %s %s has no handler", r.method, r.pattern))
		}
		if r.method == "POST" {
			h = sameOrigin(h)
		}
		m.HandleFunc(r.method+" "+r.pattern, h)
	}
	s.mux = m
}
