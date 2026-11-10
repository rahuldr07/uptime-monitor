package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"uptimemon/internal/store"
)

// ---- incidents ----

type incidentsData struct {
	base
	Targets   []store.Target
	Filter    store.IncidentFilter
	Incidents []store.Incident
}

func (s *Server) incidentsPage(w http.ResponseWriter, r *http.Request) {
	d := incidentsData{base: s.base(r, "incidents", "Incidents")}
	d.Filter.TargetID, _ = strconv.ParseInt(r.URL.Query().Get("target"), 10, 64)
	d.Filter.OngoingOnly = r.URL.Query().Get("ongoing") == "1"
	var err error
	if d.Targets, err = s.store.ListTargets(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	if d.Incidents, err = s.store.ListIncidents(r.Context(), d.Filter); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "incidents", d)
}

func (s *Server) incidentAck(w http.ResponseWriter, r *http.Request) {
	in, ok := loadByID(s, w, r, s.store.GetIncident)
	if !ok {
		return
	}
	if err := s.store.AcknowledgeIncident(r.Context(), in.ID, s.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, map[string]any{"ok": true, "message": "Acknowledged " + in.TargetName + " incident."})
		return
	}
	redirect(w, r, backTo(r, "/incidents"), "Acknowledged.")
}

func (s *Server) incidentNote(w http.ResponseWriter, r *http.Request) {
	in, ok := loadByID(s, w, r, s.store.GetIncident)
	if !ok {
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if utf8.RuneCountInString(note) > 500 {
		redirect(w, r, backTo(r, "/incidents"), "Note must be 500 characters or fewer.")
		return
	}
	if err := s.store.SetIncidentNote(r.Context(), in.ID, note); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, backTo(r, "/incidents"), "Note saved.")
}
