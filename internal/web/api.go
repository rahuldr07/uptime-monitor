package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"uptimemon/internal/store"
)

// ---- live events ----

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	// The server's WriteTimeout would otherwise cut this long-lived stream.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		s.logger.Warn("sse: cannot clear write deadline", "err", err)
	}
	ch, unsub := s.mon.Events.Subscribe()
	defer unsub()
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// ---- JSON API ----

type apiTarget struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	URL         string     `json:"url"`
	Group       string     `json:"group,omitempty"`
	Status      string     `json:"status"`
	Since       *time.Time `json:"since,omitempty"`
	LastChecked *time.Time `json:"last_checked,omitempty"`
	LatencyMS   *int64     `json:"latency_ms,omitempty"`
	StatusCode  int        `json:"status_code,omitempty"`
	Error       string     `json:"error,omitempty"`
	Uptime24h   float64    `json:"uptime_24h"`
	Uptime7d    float64    `json:"uptime_7d"`
	Uptime30d   float64    `json:"uptime_30d"`
	Paused      bool       `json:"paused"`
	Muted       bool       `json:"muted"`
	Maintenance string     `json:"maintenance,omitempty"` // title of the window covering the target now
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	views, err := s.views(r.Context(), viewOpts{})
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]apiTarget, 0, len(views))
	for _, v := range views {
		st := v.State
		a := apiTarget{ID: st.Target.ID, Name: st.Target.Name, URL: st.Target.URL, Group: st.Target.Group, Status: st.Status.String(),
			Uptime24h: round2(v.Up24.UptimePercent()), Uptime7d: round2(v.Up7.UptimePercent()), Uptime30d: round2(v.Up30.UptimePercent()), Paused: st.Target.Paused, Muted: st.Target.Muted}
		if v.Maint != nil {
			a.Maintenance = v.Maint.Title
		}
		if !st.Since.IsZero() {
			t := st.Since
			a.Since = &t
		}
		if st.Last != nil {
			t := st.Last.CheckedAt
			a.LastChecked = &t
			l := st.Last.LatencyMS
			a.LatencyMS = &l
			a.StatusCode = st.Last.StatusCode
			a.Error = st.Last.Error
		}
		out = append(out, a)
	}
	writeJSON(w, map[string]any{"generated_at": s.now().UTC(), "targets": out})
}

func (s *Server) apiChecks(w http.ResponseWriter, r *http.Request) {
	t, _, ok := s.loadTarget(w, r)
	if !ok {
		return
	}
	span, ok := ranges[r.URL.Query().Get("range")]
	if !ok {
		span = 24 * time.Hour
	}
	checks, err := s.store.ChecksSince(r.Context(), t.ID, s.now().Add(-span))
	if err != nil {
		s.fail(w, err)
		return
	}
	type c struct {
		At         time.Time `json:"at"`
		OK         bool      `json:"ok"`
		StatusCode int       `json:"status_code"`
		LatencyMS  int64     `json:"latency_ms"`
		Error      string    `json:"error,omitempty"`
		Maint      bool      `json:"maintenance,omitempty"`
	}
	out := make([]c, 0, len(checks))
	for _, ch := range checks {
		out = append(out, c{ch.CheckedAt, ch.OK, ch.StatusCode, ch.LatencyMS, ch.Error, ch.Maintenance})
	}
	writeJSON(w, map[string]any{"target": t.Name, "checks": out})
}

func (s *Server) apiIncidents(w http.ResponseWriter, r *http.Request) {
	var f store.IncidentFilter
	f.TargetID, _ = strconv.ParseInt(r.URL.Query().Get("target"), 10, 64)
	f.OngoingOnly = r.URL.Query().Get("ongoing") == "1"
	list, err := s.store.ListIncidents(r.Context(), f)
	if err != nil {
		s.fail(w, err)
		return
	}
	type in struct {
		ID           int64      `json:"id"`
		Target       string     `json:"target"`
		TargetID     int64      `json:"target_id"`
		StartedAt    time.Time  `json:"started_at"`
		EndedAt      *time.Time `json:"ended_at,omitempty"`
		DurationS    int64      `json:"duration_s"`
		Cause        string     `json:"cause"`
		AlertedVia   string     `json:"alerted_via"`
		Acknowledged *time.Time `json:"acknowledged_at,omitempty"`
		Note         string     `json:"note,omitempty"`
	}
	now := s.now()
	out := make([]in, 0, len(list))
	for _, i := range list {
		out = append(out, in{i.ID, i.TargetName, i.TargetID, i.StartedAt, i.EndedAt, int64(i.Duration(now).Seconds()), i.Cause, i.Channels, i.AckedAt, i.Note})
	}
	writeJSON(w, map[string]any{"incidents": out})
}
