package web

import (
	"fmt"
	"io/fs"
	"net/http"
)

type routeClass int

const (
	classPublic routeClass = iota + 1 // no auth; the zero value is invalid on purpose
	classAdmin                        // requireAdmin; M1d replaces it with user/workspace/operator
)

type route struct {
	method  string // "GET" or "POST"
	pattern string // ServeMux pattern without the method, e.g. "/targets/{id}"
	class   routeClass
	handler http.HandlerFunc
}

// routeTable is the single source of truth for every HTTP route.
func (s *Server) routeTable() []route {
	static, _ := fs.Sub(staticFS, "static")
	files := http.StripPrefix("/static/", cacheControl(http.FileServer(http.FS(static))))
	return []route{
		{"GET", "/static/", classPublic, files.ServeHTTP},
		{"GET", "/healthz", classPublic, s.healthz},
		{"GET", "/status", classPublic, s.statusPage},
		{"GET", "/login", classPublic, s.loginPage},
		{"POST", "/login", classPublic, s.loginPost},
		{"POST", "/logout", classPublic, s.logout},

		{"GET", "/{$}", classAdmin, s.overview},
		{"GET", "/events", classAdmin, s.events},
		{"GET", "/targets", classAdmin, s.monitorsPage},
		{"POST", "/targets", classAdmin, s.targetCreate},
		{"POST", "/targets/test", classAdmin, s.targetTest},
		{"GET", "/targets/{id}", classAdmin, s.targetPage},
		{"POST", "/targets/{id}", classAdmin, s.targetUpdate},
		{"POST", "/targets/{id}/delete", classAdmin, s.targetDelete},
		{"POST", "/targets/{id}/pause", classAdmin, s.targetPause},
		{"POST", "/targets/{id}/mute", classAdmin, s.targetMute},
		{"POST", "/targets/{id}/check", classAdmin, s.targetCheck},
		{"GET", "/incidents", classAdmin, s.incidentsPage},
		{"POST", "/incidents/{id}/ack", classAdmin, s.incidentAck},
		{"POST", "/incidents/{id}/note", classAdmin, s.incidentNote},
		{"GET", "/alerts", classAdmin, s.alertsPage},
		{"POST", "/alerts", classAdmin, s.alertsSave},
		{"POST", "/alerts/test", classAdmin, s.alertsTest},
		{"POST", "/status/settings", classAdmin, s.statusSettings},
		{"GET", "/maintenance", classAdmin, s.maintenancePage},
		{"POST", "/maintenance", classAdmin, s.maintenanceCreate},
		{"POST", "/maintenance/{id}/end", classAdmin, s.maintenanceEnd},
		{"POST", "/maintenance/{id}/delete", classAdmin, s.maintenanceDelete},
		{"GET", "/api/maintenance", classAdmin, s.apiMaintenance},
		{"GET", "/api/status", classAdmin, s.apiStatus},
		{"GET", "/api/targets/{id}/checks", classAdmin, s.apiChecks},
		{"GET", "/api/incidents", classAdmin, s.apiIncidents},
	}
}

// routes builds s.mux from routeTable, wrapping each handler by its class.
func (s *Server) routes() {
	m := http.NewServeMux()
	for _, r := range s.routeTable() {
		h := r.handler
		switch r.class {
		case classPublic:
		case classAdmin:
			h = s.requireAdmin(h)
		default:
			panic(fmt.Sprintf("web: route %s %s has no class", r.method, r.pattern))
		}
		if r.method == "POST" {
			h = sameOrigin(h)
		}
		m.HandleFunc(r.method+" "+r.pattern, h)
	}
	s.mux = m
}
