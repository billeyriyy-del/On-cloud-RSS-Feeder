package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"rssfeeder/internal/fetcher"
	"rssfeeder/internal/model"
	"rssfeeder/internal/normalize"
	"rssfeeder/internal/storage"
)

// handleListItems serves timelines for the web client (and any thin client).
//
//	GET /items?filter=unread|starred|all&source_id=&folder_id=&kind=&since=&max_id=
//	          &order=newest|oldest&q=&content=1&limit=50&cursor=<opaque>
func (s *Server) handleListItems(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	q := storage.ItemQuery{
		SourceID:    qInt(r, "source_id"),
		FolderID:    qInt(r, "folder_id"),
		Kind:        model.ItemKind(v.Get("kind")),
		Since:       qInt(r, "since"),
		MaxID:       qInt(r, "max_id"),
		Oldest:      v.Get("order") == "oldest",
		Q:           strings.TrimSpace(v.Get("q")),
		WithContent: v.Get("content") == "1",
		Limit:       int(qInt(r, "limit")),
	}
	switch v.Get("filter") {
	case "", "unread":
		q.Unread = v.Get("filter") == "unread"
	case "starred":
		q.Starred = true
	case "all":
	default:
		writeError(w, http.StatusBadRequest, "filter must be unread, starred or all")
		return
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	if c := v.Get("cursor"); c != "" {
		if q.Q != "" {
			q.Offset, _ = strconv.Atoi(c)
		} else if pub, id, ok := strings.Cut(c, ":"); ok {
			q.AfterPub, _ = strconv.ParseInt(pub, 10, 64)
			q.AfterID, _ = strconv.ParseInt(id, 10, 64)
		}
	}
	items, err := s.store.ListItems(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	next := ""
	if len(items) == q.Limit {
		if q.Q != "" {
			next = strconv.Itoa(q.Offset + len(items))
		} else {
			last := items[len(items)-1]
			next = fmt.Sprintf("%d:%d", last.PublishedAt, last.ID)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

// handleSearch keeps the original ranked-search endpoint working.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "limit must be 1–200")
			return
		}
		limit = n
	}
	items, err := s.store.SearchItems(r.Context(), q, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// handleGetItem returns one item with content.
//
//	?full=1      extract the full article now if it has not been tried
//	?original=1  return the feed's own content even when full text exists
func (s *Server) handleGetItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	it, err := s.store.GetItem(ctx, id, r.URL.Query().Get("original") == "1")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	if it == nil {
		writeError(w, http.StatusNotFound, "item not found")
		return
	}
	resp := map[string]any{"item": it}
	if r.URL.Query().Get("full") == "1" && !it.HasFullText && s.sched != nil {
		status, _ := s.store.FullTextStatus(ctx, id)
		if status == "" || r.URL.Query().Get("retry") == "1" {
			fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := fetcher.FetchFullText(fctx, s.store, s.sched.Extractor(), id, it.URL)
			cancel()
			if err != nil {
				resp["full_text_error"] = err.Error()
			} else if fresh, err := s.store.GetItem(ctx, id, false); err == nil && fresh != nil {
				resp["item"] = fresh
			}
		} else if status == "failed" {
			resp["full_text_error"] = "extraction failed earlier; pass retry=1 to try again"
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSpeech returns an item as utterances for text-to-speech.
func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	it, err := s.store.GetItem(r.Context(), id, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	if it == nil {
		writeError(w, http.StatusNotFound, "item not found")
		return
	}
	intro := it.Title + "."
	if it.SourceTitle != "" {
		intro += " " + it.SourceTitle + "."
	}
	chunks := append([]string{intro}, normalize.SpeechChunks(it.ContentHTML, 600)...)
	writeJSON(w, http.StatusOK, map[string]any{
		"item_id": it.ID, "lang": it.Lang, "title": it.Title, "chunks": chunks, "reading_secs": it.ReadingSecs,
	})
}

// handleBulkState applies a batch of state changes (swipes, keyboard toggles,
// replayed offline actions, undo). Body: {"ops":[{"id":1,"is_read":true,"at":1727…}]}
func (s *Server) handleBulkState(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ops []model.StateOp `json:"ops"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Ops) == 0 || len(req.Ops) > 1000 {
		writeError(w, http.StatusBadRequest, "ops must contain 1–1000 operations")
		return
	}
	states, err := s.store.ApplyStateOps(r.Context(), req.Ops)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"states": states})
}

// handleUpdateItemState keeps PUT /items/{id}/state working.
// Body: {"is_read": bool, "is_starred": bool, "at": unix}
func (s *Server) handleUpdateItemState(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var op model.StateOp
	if !decode(w, r, &op) {
		return
	}
	op.ItemID = id
	states, err := s.store.ApplyStateOps(r.Context(), []model.StateOp{op})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	if len(states) == 0 {
		writeError(w, http.StatusNotFound, "item not found")
		return
	}
	writeJSON(w, http.StatusOK, states[0])
}

// handleMarkRead marks a scope read (or, with read=false and ids, undoes it).
// Body: {"source_id":0,"folder_id":0,"max_id":0,"before":0,"ids":[],"read":true,"at":0}
// Returns the ids changed so the client can offer undo.
func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceID int64   `json:"source_id"`
		FolderID int64   `json:"folder_id"`
		MaxID    int64   `json:"max_id"`
		Before   int64   `json:"before"`
		IDs      []int64 `json:"ids"`
		Read     *bool   `json:"read"`
		At       int64   `json:"at"`
	}
	if !decode(w, r, &req) {
		return
	}
	read := req.Read == nil || *req.Read
	if !read && len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "marking unread requires explicit ids")
		return
	}
	ids, err := s.store.MarkRead(r.Context(), storage.MarkScope{
		SourceID: req.SourceID, FolderID: req.FolderID, MaxID: req.MaxID, Before: req.Before,
		IDs: req.IDs, Read: read, At: req.At,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ids": ids, "count": len(ids)})
}
