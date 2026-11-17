package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/store"
)

const msgTimeZoneSaved = "Time zone saved."

type zoneOption struct {
	Value, Label string
	Selected     bool
}

// zoneOptions lists the Account page's time zone choices: Automatic first,
// then every zone with its current offset. A saved zone that is not in the
// list (an alias) is added so the select shows it.
func zoneOptions(u store.User, now time.Time) []zoneOption {
	auto := "UTC"
	if store.LoadZone(u.DetectedTZ) != nil {
		auto = u.DetectedTZ
	}
	opts := []zoneOption{{Label: "Automatic (" + auto + ")", Selected: u.TimeZone == ""}}
	listed := false
	for _, name := range store.ZoneNames() {
		listed = listed || name == u.TimeZone
		opts = append(opts, zoneOption{Value: name, Label: zoneLabel(name, now), Selected: name == u.TimeZone})
	}
	if u.TimeZone != "" && !listed {
		opts = append(opts, zoneOption{Value: u.TimeZone, Label: zoneLabel(u.TimeZone, now), Selected: true})
	}
	return opts
}

// zoneLabel is "Asia/Kolkata (UTC+05:30)".
func zoneLabel(name string, now time.Time) string {
	return name + " (" + zoneOffset(store.LoadZone(name), now) + ")"
}

// accountTimeZone saves the user's explicit zone; an empty value returns them
// to automatic.
func (s *Server) accountTimeZone(w http.ResponseWriter, r *http.Request, a *authCtx) {
	name := r.FormValue("time_zone")
	if err := s.sys.SetTimeZone(r.Context(), a.User.ID, name); err != nil {
		d, derr := s.accountView(r, a)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		d.Error = err.Error()
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "account", d)
		return
	}
	s.resetStatus(a.WS) // the public status page is shown in the owner's zone
	redirect(w, r, "/account", msgTimeZoneSaved)
}

// accountDetectedZone records the zone the browser reports. It answers whether
// the user's effective zone changed, so the page reloads only when it must.
// Junk is rejected without being stored, and an explicit choice is never
// overwritten: only the detected value is.
func (s *Server) accountDetectedZone(w http.ResponseWriter, r *http.Request, a *authCtx) {
	if ok, retry := s.tzDetect.Take(strconv.FormatInt(a.User.ID, 10)); !ok {
		retryAfter(w, retry)
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]any{"error": "too many requests"})
		return
	}
	name := r.FormValue("tz")
	if store.LoadZone(name) == nil {
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"changed": false})
		return
	}
	changed := store.EffectiveZone(a.User.TimeZone, name).String() != a.User.Location().String()
	if name != a.User.DetectedTZ {
		if err := s.sys.SetDetectedZone(r.Context(), a.User.ID, name); err != nil {
			s.fail(w, err)
			return
		}
	}
	if changed {
		s.resetStatus(a.WS)
	}
	writeJSON(w, map[string]any{"changed": changed})
}
