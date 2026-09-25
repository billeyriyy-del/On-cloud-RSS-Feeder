package api

import (
	"encoding/json"
	"net/http"
	"strings"

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
	if !decode(w, r, &f) {
		return
	}
	f.Name = strings.TrimSpace(f.Name)
	if f.Name == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}
	if f.ParentID != nil {
		if code, msg := s.checkParent(r, 0, *f.ParentID); code != 0 {
			writeError(w, code, msg)
			return
		}
	}
	if err := s.store.CreateFolder(r.Context(), &f); err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

// handleUpdateFolder renames and/or moves a folder ("parent_id": null moves it
// to the top level).
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
	var patch struct {
		Name     *string         `json:"name"`
		ParentID json.RawMessage `json:"parent_id"`
	}
	if !decode(w, r, &patch) {
		return
	}
	if patch.Name != nil && strings.TrimSpace(*patch.Name) != "" {
		existing.Name = strings.TrimSpace(*patch.Name)
	}
	if len(patch.ParentID) > 0 {
		if string(patch.ParentID) == "null" {
			existing.ParentID = nil
		} else {
			var pid int64
			if err := json.Unmarshal(patch.ParentID, &pid); err != nil {
				writeError(w, http.StatusBadRequest, "parent_id must be an integer or null")
				return
			}
			if code, msg := s.checkParent(r, id, pid); code != 0 {
				writeError(w, code, msg)
				return
			}
			existing.ParentID = &pid
		}
	}
	if err := s.store.UpdateFolder(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, existing)
}

// checkParent validates placing folder id (0 = new) under parent: the parent
// must exist, must not be inside the folder itself, and the whole subtree must
// stay within the depth limit.
func (s *Server) checkParent(r *http.Request, id, parent int64) (int, string) {
	ctx := r.Context()
	p, err := s.store.GetFolder(ctx, parent)
	if err != nil {
		return http.StatusInternalServerError, "db error"
	}
	if p == nil {
		return http.StatusBadRequest, "parent folder does not exist"
	}
	height := 0
	if id != 0 {
		inside, err := s.store.IsDescendant(ctx, id, parent)
		if err != nil {
			return http.StatusInternalServerError, "db error"
		}
		if inside {
			return http.StatusUnprocessableEntity, "a folder cannot be moved inside itself"
		}
		if height, err = s.store.SubtreeHeight(ctx, id); err != nil {
			return http.StatusInternalServerError, "db error"
		}
	}
	depth, err := s.store.FolderDepth(ctx, parent)
	if err != nil {
		return http.StatusInternalServerError, "db error"
	}
	if depth+1+height > maxFolderDepth {
		return http.StatusUnprocessableEntity, "maximum folder depth (3 levels) exceeded"
	}
	return 0, ""
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
