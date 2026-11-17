package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/store"
)

const msgTimeZoneSaved = "Time zone saved."

type zoneOption struct {
	Value, Label string
	Selected     bool
}

// zoneOptions lists the Account page's time zone choices: Automatic first,
// then every zone with its current offset. A saved alias selects its listed
// zone; a saved name that no longer loads counts as Automatic.
func zoneOptions(u store.User, now time.Time) []zoneOption {
	auto := "UTC"
	if store.LoadZone(u.DetectedTZ) != nil {
		auto = store.CanonicalZone(u.DetectedTZ)
	}
	chosen := ""
	if store.LoadZone(u.TimeZone) != nil {
		chosen = store.CanonicalZone(u.TimeZone)
	}
	opts := []zoneOption{{Label: "Automatic: " + zoneLabel(auto, now), Selected: chosen == ""}}
	for _, name := range store.ZoneNames() {
		opts = append(opts, zoneOption{Value: name, Label: zoneLabel(name, now), Selected: name == chosen})
	}
	return opts
}

// zoneLabel is "Asia/Kolkata (UTC+05:30)"; underscores read as spaces so the
// list can be searched by city.
func zoneLabel(name string, now time.Time) string {
	return strings.ReplaceAll(name, "_", " ") + " (" + zoneOffset(store.LoadZone(name), now) + ")"
}

// resetOwnedStatus drops the cached public status pages of every workspace the
// user owns: they are shown in the owner's zone.
func (s *Server) resetOwnedStatus(ctx context.Context, userID int64) {
	ids, err := s.sys.OwnedWorkspaceIDs(ctx, userID)
	if err != nil {
		s.logger.Error("list owned workspaces", "err", err)
	}
	for _, id := range ids {
		s.statusCache.reset(id)
	}
}

// accountTimeZone saves the user's explicit zone; an empty value returns them
// to automatic.
func (s *Server) accountTimeZone(w http.ResponseWriter, r *http.Request, a *authCtx) {
	fail := func(code int, msg string) {
		d, err := s.accountView(r, a)
		if err != nil {
			s.fail(w, err)
			return
		}
		d.Error = msg
		s.renderStatus(w, r, code, "account", d)
	}
	if ok, retry := s.tzDetect.Take("save:" + strconv.FormatInt(a.User.ID, 10)); !ok {
		retryAfter(w, retry)
		fail(http.StatusTooManyRequests, "Too many changes. Try again in a minute.")
		return
	}
	if err := s.sys.SetTimeZone(r.Context(), a.User.ID, r.FormValue("time_zone")); err != nil {
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.resetOwnedStatus(r.Context(), a.User.ID)
	redirect(w, r, "/account", msgTimeZoneSaved)
}

// accountDetectedZone records the zone the browser reports. It answers whether
// the user's effective zone changed, so the page reloads only when it must.
// Junk is rejected without being stored, and an explicit choice is never
// overwritten: only the detected value is.
func (s *Server) accountDetectedZone(w http.ResponseWriter, r *http.Request, a *authCtx) {
	if ok, retry := s.tzDetect.Take("detect:" + strconv.FormatInt(a.User.ID, 10)); !ok {
		retryAfter(w, retry)
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]any{"error": "too many requests"})
		return
	}
	name := r.FormValue("tz")
	if store.LoadZone(name) == nil {
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"changed": false})
		return
	}
	// Compare listed names: a browser that reports an alias (Asia/Calcutta)
	// for a zone already in use (Asia/Kolkata) changes nothing.
	zone := func(l *time.Location) string { return store.CanonicalZone(l.String()) }
	changed := zone(store.EffectiveZone(a.User.TimeZone, name)) != zone(a.User.Location())
	if name != a.User.DetectedTZ {
		if err := s.sys.SetDetectedZone(r.Context(), a.User.ID, name); err != nil {
			s.fail(w, err)
			return
		}
	}
	if changed {
		s.resetOwnedStatus(r.Context(), a.User.ID)
	}
	writeJSON(w, map[string]any{"changed": changed})
}
