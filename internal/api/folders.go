package api

import (
	"encoding/json"
	"net/http"

	"rssfeeder/internal/model"
)

const maxFolderDepth = 2 // 0-indexed: depth 2 means 3 levels (root, child, grandchild)

func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	folders, err := s.store.ListFolders(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, folders)
}

func (s *Server) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	var f model.Folder
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if f.Name == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}
	if f.ParentID != nil {
		depth, err := s.store.FolderDepth(r.Context(), *f.ParentID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db error")
			return
		}
		// A child of a folder at depth d is at depth d+1.
		if depth+1 > maxFolderDepth {
			writeError(w, http.StatusUnprocessableEntity, "maximum folder depth (3 levels) exceeded")
			return
		}
	}
	if err := s.store.CreateFolder(r.Context(), &f); err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) handleUpdateFolder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	existing, err := s.store.GetFolder(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "folder not found")
		return
	}

	var patch model.Folder
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if patch.Name != "" {
		existing.Name = patch.Name
	}
	if patch.ParentID != nil {
		// Re-check depth after reparenting.
		depth, err := s.store.FolderDepth(r.Context(), *patch.ParentID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db error")
			return
		}
		if depth+1 > maxFolderDepth {
			writeError(w, http.StatusUnprocessableEntity, "maximum folder depth (3 levels) exceeded")
			return
		}
		existing.ParentID = patch.ParentID
	}

	if err := s.store.UpdateFolder(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) handleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteFolder(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
