package api

import (
	"encoding/json"
	"net/http"
	"time"

	"rssfeeder/internal/model"
)

// handleUpdateItemState implements PUT /items/{id}/state
// Body: { "is_read": bool, "is_starred": bool }
func (s *Server) handleUpdateItemState(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}

	var req struct {
		IsRead    *bool `json:"is_read"`
		IsStarred *bool `json:"is_starred"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	ctx := r.Context()
	st, err := s.store.GetItemState(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	now := time.Now().Unix()
	if req.IsRead != nil {
		st.IsRead = *req.IsRead
		if st.IsRead && st.ReadAt == nil {
			st.ReadAt = &now
		} else if !st.IsRead {
			st.ReadAt = nil
		}
	}
	if req.IsStarred != nil {
		st.IsStarred = *req.IsStarred
		if st.IsStarred && st.StarredAt == nil {
			st.StarredAt = &now
		} else if !st.IsStarred {
			st.StarredAt = nil
		}
	}
	st.ItemID = id

	if err := s.store.UpsertItemState(ctx, st); err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	// Return full state so clients can use last-write-wins logic.
	updated := &model.ItemState{
		ItemID:    st.ItemID,
		IsRead:    st.IsRead,
		IsStarred: st.IsStarred,
		ReadAt:    st.ReadAt,
		StarredAt: st.StarredAt,
		UpdatedAt: st.UpdatedAt,
	}
	writeJSON(w, http.StatusOK, updated)
}
