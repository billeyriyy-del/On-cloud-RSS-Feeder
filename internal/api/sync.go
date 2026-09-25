package api

import (
	"net/http"
	"strconv"

	"rssfeeder/internal/storage"
)

// handleSync returns the changes after a cursor.
//
//	GET /sync?cursor=<n>&limit=<1..1000>
//
// Loop while has_more is true, storing cursor after applying each page. If
// reset is true, wipe the local cache and start again from cursor=0.
// For compatibility, ?since=<unix seconds> is still accepted: it is mapped to
// a cursor that cannot skip anything changed at or after that second.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var cursor int64
	raw := q.Get("cursor")
	if raw == "" {
		raw = q.Get("since")
	}
	if raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "cursor must be a non-negative integer")
			return
		}
		cursor = n
	}
	if q.Get("cursor") == "" && cursor >= storage.LegacySinceThreshold {
		c, err := s.store.CursorForTimestamp(r.Context(), cursor)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "sync failed")
			return
		}
		cursor = c
	}
	limit := 500
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, "limit must be 1–1000")
			return
		}
		limit = n
	}
	delta, err := s.store.GetSyncDelta(r.Context(), cursor, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sync failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, delta)
}

func (s *Server) handleCounts(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetCounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, c)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
