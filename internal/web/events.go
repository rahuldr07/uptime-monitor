package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const maxStreamsPerUser = 5

// eventsRecheck is how often an open stream re-validates its session.
var eventsRecheck = 60 * time.Second

// sseWriteTimeout bounds each write to a stream.
const sseWriteTimeout = 30 * time.Second

// openStream reserves one of the user's stream slots.
func (s *Server) openStream(userID int64) bool {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if s.streams[userID] >= maxStreamsPerUser {
		return false
	}
	if s.streams == nil {
		s.streams = map[int64]int{}
	}
	s.streams[userID]++
	return true
}

func (s *Server) closeStream(userID int64) {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if s.streams[userID]--; s.streams[userID] <= 0 {
		delete(s.streams, userID)
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, a *authCtx) {
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if !s.openStream(a.User.ID) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "too many streams", http.StatusTooManyRequests)
		return
	}
	defer s.closeStream(a.User.ID)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	// The server's WriteTimeout would otherwise cut this long-lived stream, so
	// each write gets its own deadline instead: a stalled client then fails
	// the write and releases its slot.
	rc := http.NewResponseController(w)
	write := func(format string, args ...any) error {
		if err := rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err != nil {
			s.logger.Warn("sse: cannot set write deadline", "err", err)
		}
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return err
		}
		return rc.Flush()
	}
	wsID := a.WS.ID()
	ch, unsub := s.mon.Events.Subscribe()
	defer unsub()
	if err := write("retry: 3000\n\n"); err != nil {
		return
	}
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	recheck := time.NewTicker(eventsRecheck)
	defer recheck.Stop()
	raw := s.sessionToken(r)
	for {
		select {
		case <-recheck.C:
			// A revoked or expired session, or a disabled user, ends the stream.
			if _, err := s.sys.SessionAuth(r.Context(), raw, s.now()); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if ev.WorkspaceID == 0 || ev.WorkspaceID != wsID {
				continue
			}
			b, _ := json.Marshal(ev)
			if err := write("data: %s\n\n", b); err != nil {
				return
			}
		case <-ping.C:
			if err := write(": ping\n\n"); err != nil {
				return
			}
		}
	}
}
