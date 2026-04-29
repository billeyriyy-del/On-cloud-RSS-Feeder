package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"rssfeeder/internal/fetcher"
	"rssfeeder/internal/model"
)

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.store.ListSources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, srcs)
}

func (s *Server) handleGetSource(w http.ResponseWriter, r *http.Request) {
	src, ok := s.sourceFromPath(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, src)
}

func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	var src model.Source
	if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if src.URL == "" {
		writeError(w, http.StatusBadRequest, "url required")
		return
	}
	if src.Type != model.SourceTypeRSS && src.Type != model.SourceTypeHTML {
		writeError(w, http.StatusBadRequest, "type must be 'rss' or 'html'")
		return
	}
	if src.Type == model.SourceTypeHTML && src.ScraperConfig == nil {
		writeError(w, http.StatusBadRequest, "scraper_config required for html sources")
		return
	}
	if src.PollInterval == 0 {
		src.PollInterval = 3600
	}
	// Spread first poll across the interval to prevent thundering herd.
	src.NextPollAt = fetcher.SpreadFirstPoll(src.PollInterval)

	if err := s.store.CreateSource(r.Context(), &src); err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusCreated, src)
}

func (s *Server) handleUpdateSource(w http.ResponseWriter, r *http.Request) {
	existing, ok := s.sourceFromPath(w, r)
	if !ok {
		return
	}
	var patch model.Source
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	// Apply patch fields that were provided.
	if patch.Title != "" {
		existing.Title = patch.Title
	}
	if patch.FolderID != nil {
		existing.FolderID = patch.FolderID
	}
	if patch.ScraperConfig != nil {
		existing.ScraperConfig = patch.ScraperConfig
	}
	if patch.PollInterval > 0 {
		existing.PollInterval = patch.PollInterval
	}

	if err := s.store.UpdateSource(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteSource(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (s *Server) sourceFromPath(w http.ResponseWriter, r *http.Request) (*model.Source, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return nil, false
	}
	src, err := s.store.GetSource(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return nil, false
	}
	if src == nil {
		writeError(w, http.StatusNotFound, "source not found")
		return nil, false
	}
	return src, true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}
