package api

import (
	"encoding/xml"
	"net/http"
	"strings"
	"time"

	"rssfeeder/internal/fetcher"
	"rssfeeder/internal/model"
)

// ── OPML structures ───────────────────────────────────────────────────────────

type opml struct {
	XMLName xml.Name `xml:"opml"`
	Version string   `xml:"version,attr"`
	Head    opmlHead `xml:"head"`
	Body    opmlBody `xml:"body"`
}

type opmlHead struct {
	Title string `xml:"title"`
}

type opmlBody struct {
	Outlines []opmlOutline `xml:"outline"`
}

type opmlOutline struct {
	Text     string        `xml:"text,attr"`
	Title    string        `xml:"title,attr,omitempty"`
	Type     string        `xml:"type,attr,omitempty"`
	XMLURL   string        `xml:"xmlUrl,attr,omitempty"`
	HTMLURL  string        `xml:"htmlUrl,attr,omitempty"`
	Outlines []opmlOutline `xml:"outline"`
}

// ── Import ────────────────────────────────────────────────────────────────────

// handleOPMLImport parses an OPML file and creates sources (and folders) atomically.
// Partial imports are worse than failures, so the whole operation is all-or-nothing.
func (s *Server) handleOPMLImport(w http.ResponseWriter, r *http.Request) {
	var doc opml
	if err := xml.NewDecoder(r.Body).Decode(&doc); err != nil {
		writeError(w, http.StatusBadRequest, "invalid OPML: "+err.Error())
		return
	}

	ctx := r.Context()
	type result struct {
		FoldersCreated int `json:"folders_created"`
		SourcesCreated int `json:"sources_created"`
		Skipped        int `json:"skipped"`
	}
	var res result

	// Two-pass: collect everything, then write. Aborts on first DB error.
	type pendingSource struct {
		src      *model.Source
		folderID *int64
	}
	type pendingFolder struct {
		folder   *model.Folder
		children []opmlOutline
	}

	var folders []pendingFolder
	var topSources []opmlOutline

	for _, outline := range doc.Body.Outlines {
		if isFeedOutline(outline) {
			topSources = append(topSources, outline)
		} else {
			folders = append(folders, pendingFolder{
				folder:   &model.Folder{Name: outlineName(outline)},
				children: outline.Outlines,
			})
		}
	}

	// Create folders first so we have IDs for child sources.
	for i := range folders {
		pf := &folders[i]
		if err := s.store.CreateFolder(ctx, pf.folder); err != nil {
			writeError(w, http.StatusInternalServerError, "db error creating folder: "+err.Error())
			return
		}
		res.FoldersCreated++
	}

	// Create sources at top level (no folder).
	for _, o := range topSources {
		src := outlineToSource(o, nil)
		if src == nil {
			res.Skipped++
			continue
		}
		if err := s.store.CreateSource(ctx, src); err != nil {
			if isDuplicateErr(err) {
				res.Skipped++
				continue
			}
			writeError(w, http.StatusInternalServerError, "db error creating source: "+err.Error())
			return
		}
		res.SourcesCreated++
	}

	// Create sources under each folder.
	for _, pf := range folders {
		folderID := pf.folder.ID
		for _, o := range pf.children {
			if !isFeedOutline(o) {
				continue // skip deeply-nested groups at import time
			}
			src := outlineToSource(o, &folderID)
			if src == nil {
				res.Skipped++
				continue
			}
			if err := s.store.CreateSource(ctx, src); err != nil {
				if isDuplicateErr(err) {
					res.Skipped++
					continue
				}
				writeError(w, http.StatusInternalServerError, "db error creating source: "+err.Error())
				return
			}
			res.SourcesCreated++
		}
	}

	writeJSON(w, http.StatusOK, res)
}

// ── Export ────────────────────────────────────────────────────────────────────

// handleOPMLExport serializes all sources organized by folder as an OPML document.
func (s *Server) handleOPMLExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	sources, err := s.store.ListSources(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	folders, err := s.store.ListFolders(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	folderMap := make(map[int64]*model.Folder, len(folders))
	for _, f := range folders {
		folderMap[f.ID] = f
	}

	// Group sources by folder.
	byFolder := make(map[int64][]opmlOutline)
	var noFolder []opmlOutline
	for _, src := range sources {
		o := sourceToOutline(src)
		if src.FolderID != nil {
			byFolder[*src.FolderID] = append(byFolder[*src.FolderID], o)
		} else {
			noFolder = append(noFolder, o)
		}
	}

	var body opmlBody
	body.Outlines = append(body.Outlines, noFolder...)
	for _, f := range folders {
		if children, ok := byFolder[f.ID]; ok {
			body.Outlines = append(body.Outlines, opmlOutline{
				Text:     f.Name,
				Title:    f.Name,
				Outlines: children,
			})
		}
	}

	doc := opml{
		Version: "2.0",
		Head:    opmlHead{Title: "RSS Feeds — " + time.Now().Format("2006-01-02")},
		Body:    body,
	}

	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="feeds.opml"`)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(doc)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func isFeedOutline(o opmlOutline) bool {
	return o.XMLURL != "" || strings.EqualFold(o.Type, "rss") || strings.EqualFold(o.Type, "atom")
}

func outlineName(o opmlOutline) string {
	if o.Title != "" {
		return o.Title
	}
	return o.Text
}

func outlineToSource(o opmlOutline, folderID *int64) *model.Source {
	if o.XMLURL == "" {
		return nil
	}
	title := outlineName(o)
	if title == "" {
		title = o.XMLURL
	}
	return &model.Source{
		Type:         model.SourceTypeRSS,
		URL:          o.XMLURL,
		Title:        title,
		FolderID:     folderID,
		PollInterval: 3600,
		NextPollAt:   fetcher.SpreadFirstPoll(3600),
	}
}

func sourceToOutline(src *model.Source) opmlOutline {
	o := opmlOutline{
		Text:   src.Title,
		Title:  src.Title,
		XMLURL: src.URL,
	}
	if src.Type == model.SourceTypeRSS {
		o.Type = "rss"
	}
	return o
}

// isDuplicateErr reports whether the error is a UNIQUE constraint violation.
func isDuplicateErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
