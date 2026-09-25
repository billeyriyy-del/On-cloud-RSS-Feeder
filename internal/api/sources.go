package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"rssfeeder/internal/fetcher"
	"rssfeeder/internal/model"
	"rssfeeder/internal/storage"
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
	if src, ok := s.sourceFromPath(w, r); ok {
		writeJSON(w, http.StatusOK, src)
	}
}

// handleDiscover finds feeds for any URL a person might paste: a feed, a site,
// a YouTube channel, a subreddit, a GitHub repo, a Mastodon profile, an Apple
// Podcasts page… Body: {"url": "..."}
func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &req) {
		return
	}
	cands, err := s.disc.Discover(r.Context(), req.URL)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": cands})
}

// handleCreateSource subscribes. With a type ("rss"/"html") the URL is used as
// given. Without one, the URL is run through discovery and the best feed is
// subscribed; other candidates are returned as alternatives. If only an HTML
// page was found, nothing is created and the candidates are returned (422) so
// the client can confirm scraping.
func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL           string               `json:"url"`
		Type          model.SourceType     `json:"type"`
		Title         string               `json:"title"`
		FolderID      *int64               `json:"folder_id"`
		ScraperConfig *model.ScraperConfig `json:"scraper_config"`
		FetchFullText bool                 `json:"fetch_full_text"`
		PollInterval  int64                `json:"poll_interval"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "url required")
		return
	}
	src := &model.Source{
		Type: req.Type, URL: req.URL, Title: req.Title, FolderID: req.FolderID,
		ScraperConfig: req.ScraperConfig, FetchFullText: req.FetchFullText, PollInterval: req.PollInterval,
	}
	var alternatives []fetcher.Candidate
	switch req.Type {
	case model.SourceTypeRSS, model.SourceTypeHTML:
	case "":
		cands, err := s.disc.Discover(r.Context(), req.URL)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		best := cands[0]
		if best.Type == model.SourceTypeHTML {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": "no feed found; confirm with type \"html\" to follow this page by scraping", "candidates": cands,
			})
			return
		}
		src.Type, src.URL, src.Kind = best.Type, best.URL, best.Kind
		if src.Title == "" {
			src.Title = best.Title
		}
		alternatives = cands[1:]
	default:
		writeError(w, http.StatusBadRequest, "type must be 'rss', 'html', or omitted for discovery")
		return
	}
	if src.Title == "" {
		src.Title = src.URL
	}
	if src.PollInterval < 900 || src.PollInterval > 86400 {
		src.PollInterval = 3600
	}
	if src.FolderID != nil {
		if f, err := s.store.GetFolder(r.Context(), *src.FolderID); err != nil || f == nil {
			writeError(w, http.StatusBadRequest, "folder does not exist")
			return
		}
	}
	src.NextPollAt = time.Now().Unix() // a subscription the user just made should fill now

	if err := s.store.CreateSource(r.Context(), src); err != nil {
		if errors.Is(err, storage.ErrDuplicate) {
			existing, _ := s.store.GetSourceByURL(r.Context(), src.URL)
			writeJSON(w, http.StatusConflict, map[string]any{"error": "already subscribed", "source": existing})
			return
		}
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	s.kick()
	writeJSON(w, http.StatusCreated, map[string]any{"source": src, "alternatives": alternatives})
}

// handleUpdateSource applies a partial update. Send "folder_id": null to move
// a source to the top level, "is_dead": false to revive a dead feed.
func (s *Server) handleUpdateSource(w http.ResponseWriter, r *http.Request) {
	existing, ok := s.sourceFromPath(w, r)
	if !ok {
		return
	}
	var patch struct {
		Title         *string              `json:"title"`
		URL           *string              `json:"url"`
		FolderID      json.RawMessage      `json:"folder_id"`
		ScraperConfig *model.ScraperConfig `json:"scraper_config"`
		FetchFullText *bool                `json:"fetch_full_text"`
		PollInterval  *int64               `json:"poll_interval"`
		IsDead        *bool                `json:"is_dead"`
	}
	if !decode(w, r, &patch) {
		return
	}
	if patch.Title != nil && strings.TrimSpace(*patch.Title) != "" {
		existing.Title = strings.TrimSpace(*patch.Title)
	}
	if patch.URL != nil && strings.TrimSpace(*patch.URL) != "" {
		existing.URL = strings.TrimSpace(*patch.URL) // storage resets validators and push
	}
	if len(patch.FolderID) > 0 {
		if string(patch.FolderID) == "null" {
			existing.FolderID = nil
		} else {
			var id int64
			if err := json.Unmarshal(patch.FolderID, &id); err != nil {
				writeError(w, http.StatusBadRequest, "folder_id must be an integer or null")
				return
			}
			if f, err := s.store.GetFolder(r.Context(), id); err != nil || f == nil {
				writeError(w, http.StatusBadRequest, "folder does not exist")
				return
			}
			existing.FolderID = &id
		}
	}
	if patch.ScraperConfig != nil {
		existing.ScraperConfig = patch.ScraperConfig
	}
	if patch.FetchFullText != nil {
		existing.FetchFullText = *patch.FetchFullText
	}
	if patch.PollInterval != nil {
		if *patch.PollInterval < 900 || *patch.PollInterval > 86400 {
			writeError(w, http.StatusBadRequest, "poll_interval must be 900–86400 seconds")
			return
		}
		existing.PollInterval = *patch.PollInterval
	}
	revive := patch.IsDead != nil && !*patch.IsDead && existing.IsDead
	if err := s.store.UpdateSource(r.Context(), existing); err != nil {
		if errors.Is(err, storage.ErrDuplicate) {
			writeError(w, http.StatusConflict, "another source already uses that URL")
			return
		}
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	if revive {
		if err := s.store.ReviveSource(r.Context(), existing.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "db error")
			return
		}
	}
	s.kick()
	fresh, err := s.store.GetSource(r.Context(), existing.ID)
	if err != nil || fresh == nil {
		fresh = existing
	}
	writeJSON(w, http.StatusOK, fresh)
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	src, ok := s.sourceFromPath(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteSource(r.Context(), src.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	if s.sched != nil && s.sched.WebSub() != nil && src.WebSub.Hub != "" {
		go s.sched.WebSub().Unsubscribe(context.Background(), src)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRefresh is pull-to-refresh: poll now. Body (optional): {"source_id": n}.
// Sources polled in the last two minutes are skipped (conditional GETs make a
// refresh cheap, but publishers still deserve restraint).
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceID int64 `json:"source_id"`
	}
	if r.ContentLength != 0 {
		if !decode(w, r, &req) {
			return
		}
	}
	n, err := s.store.RequestPoll(r.Context(), req.SourceID, 120)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	s.kick()
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": n})
}

func (s *Server) kick() {
	if s.sched != nil {
		s.sched.Kick()
	}
}

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
