package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"uptimemon/internal/monitor"
	"uptimemon/internal/store"
)

// ---- maintenance windows ----

type maintForm struct {
	Title    string
	Starts   string // datetime-local value, empty for "now"
	Duration string
	Group    string
}

type maintenanceData struct {
	base
	Form     maintForm
	Active   []store.Maintenance
	Upcoming []store.Maintenance
	Past     []store.Maintenance
}

func (s *Server) maintenanceData(r *http.Request, a *authCtx, f maintForm) (maintenanceData, error) {
	ws := a.WS
	d := maintenanceData{base: s.base(r, a, "maintenance", "Maintenance"), Form: f}
	if d.Form.Duration == "" {
		d.Form.Duration = "1h"
	}
	all, err := ws.ListMaintenance(r.Context(), time.Time{}, 0)
	if err != nil {
		return d, err
	}
	for _, m := range all {
		switch {
		case m.Active(d.Now):
			d.Active = append(d.Active, m)
		case m.Upcoming(d.Now):
			d.Upcoming = append(d.Upcoming, m)
		default:
			d.Past = append(d.Past, m)
		}
	}
	// Past windows newest first; keep the list short.
	for i, j := 0, len(d.Past)-1; i < j; i, j = i+1, j-1 {
		d.Past[i], d.Past[j] = d.Past[j], d.Past[i]
	}
	if len(d.Past) > 30 {
		d.Past = d.Past[:30]
	}
	return d, nil
}

func (s *Server) maintenancePage(w http.ResponseWriter, r *http.Request, a *authCtx) {
	d, err := s.maintenanceData(r, a, maintForm{})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "maintenance", d)
}

// parseMaintForm validates the schedule form. The browser sends its UTC
// offset in minutes so a local wall-clock time can be pinned to an instant;
// without it the server's zone is used.
func parseMaintForm(r *http.Request, now time.Time) (maintForm, store.Maintenance, error) {
	f := maintForm{Title: strings.TrimSpace(r.FormValue("title")), Starts: strings.TrimSpace(r.FormValue("starts")),
		Duration: strings.TrimSpace(r.FormValue("duration")), Group: strings.TrimSpace(r.FormValue("group"))}
	m := store.Maintenance{Title: f.Title, Group: f.Group}
	if f.Title == "" {
		return f, m, fmt.Errorf("give the window a title, like \"Database upgrade\"")
	}
	if utf8.RuneCountInString(f.Title) > 80 {
		return f, m, fmt.Errorf("title must be 80 characters or fewer")
	}
	if len(f.Group) > 40 {
		return f, m, fmt.Errorf("group name must be 40 characters or fewer")
	}
	m.StartsAt = now
	if f.Starts != "" {
		var err error
		if off, perr := strconv.Atoi(r.FormValue("tz_offset")); perr == nil && off > -14*60 && off < 14*60 {
			m.StartsAt, err = time.Parse("2006-01-02T15:04", f.Starts)
			m.StartsAt = m.StartsAt.Add(time.Duration(off) * time.Minute)
		} else {
			m.StartsAt, err = time.ParseInLocation("2006-01-02T15:04", f.Starts, time.Local)
		}
		if err != nil {
			return f, m, fmt.Errorf("start time: pick a date and time")
		}
	}
	if f.Duration == "" {
		f.Duration = "1h"
	}
	d, err := time.ParseDuration(f.Duration)
	if err != nil || d < time.Minute || d > 30*24*time.Hour {
		return f, m, fmt.Errorf("duration: use a value between 1m and 720h, like 30m or 2h")
	}
	m.EndsAt = m.StartsAt.Add(d)
	if m.EndsAt.Before(now) {
		return f, m, fmt.Errorf("that window is already over")
	}
	return f, m, nil
}

func (s *Server) maintenanceCreate(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	f, m, err := parseMaintForm(r, s.now())
	if err == nil {
		m, err = ws.CreateMaintenance(r.Context(), m, s.now())
	}
	if err != nil {
		d, derr := s.maintenanceData(r, a, f)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		d.Error = err.Error()
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "maintenance", d)
		return
	}
	s.mon.Events.Publish(monitor.Event{WorkspaceID: ws.ID(), Kind: "maintenance"})
	s.resetStatus(r.Context(), ws)
	msg := "Maintenance started: alerts are paused until " + formatTS(m.EndsAt) + "."
	if m.Upcoming(s.now()) {
		msg = "Maintenance scheduled for " + formatTS(m.StartsAt) + "."
	}
	redirect(w, r, "/maintenance", msg)
}

// maintenanceEnd closes an active window now so alerting resumes at once.
func (s *Server) maintenanceEnd(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	m, ok := loadByID(s, w, r, ws.GetMaintenance)
	if !ok {
		return
	}
	if err := ws.EndMaintenance(r.Context(), m.ID, s.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	s.mon.Events.Publish(monitor.Event{WorkspaceID: ws.ID(), Kind: "maintenance"})
	s.resetStatus(r.Context(), ws)
	redirect(w, r, backTo(r, "/maintenance"), "Ended "+m.Title+". Alerts are live again.")
}

func (s *Server) maintenanceDelete(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	m, ok := loadByID(s, w, r, ws.GetMaintenance)
	if !ok {
		return
	}
	if err := ws.DeleteMaintenance(r.Context(), m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	s.mon.Events.Publish(monitor.Event{WorkspaceID: ws.ID(), Kind: "maintenance"})
	s.resetStatus(r.Context(), ws)
	redirect(w, r, "/maintenance", "Removed "+m.Title+".")
}

type apiMaint struct {
	ID       int64     `json:"id"`
	Title    string    `json:"title"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	Group    string    `json:"group,omitempty"`
	Active   bool      `json:"active"`
}

func (s *Server) apiMaintenance(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	now := s.now()
	list, err := ws.ListMaintenance(r.Context(), now, 0)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]apiMaint, 0, len(list))
	for _, m := range list {
		out = append(out, apiMaint{m.ID, m.Title, m.StartsAt, m.EndsAt, m.Group, m.Active(now)})
	}
	writeJSON(w, map[string]any{"generated_at": now.UTC(), "maintenance": out})
}
