package api

import (
	"net/http"
	"strconv"
)

// handleSync returns a delta of all changes since the given cursor (unix seconds).
// GET /sync?since=:cursor
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	var since int64
	if v := r.URL.Query().Get("since"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be a unix timestamp")
			return
		}
		since = parsed
	}

	delta, err := s.store.GetSyncDelta(r.Context(), since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sync failed")
		return
	}
	writeJSON(w, http.StatusOK, delta)
}
