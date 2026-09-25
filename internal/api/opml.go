package api

import (
	"encoding/xml"
	"errors"
	"net/http"
	"strings"
	"time"

	"rssfeeder/internal/fetcher"
	"rssfeeder/internal/model"
	"rssfeeder/internal/storage"

	"golang.org/x/net/html/charset"
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

// handleOPMLImport creates folders and sources from an OPML file. Folders are
// matched by name so re-importing is harmless, nesting is kept up to the depth
// limit (deeper groups are flattened into their deepest allowed ancestor), and
// feeds already subscribed are counted as skipped. First polls are spread over
// the next hour so a 300-feed import does not stampede.
func (s *Server) handleOPMLImport(w http.ResponseWriter, r *http.Request) {
	var doc opml
	dec := xml.NewDecoder(r.Body)
	dec.Strict = false
	dec.CharsetReader = charset.NewReaderLabel
	if err := dec.Decode(&doc); err != nil {
		writeError(w, http.StatusBadRequest, "invalid OPML: "+err.Error())
		return
	}
	ctx := r.Context()
	var res struct {
		FoldersCreated int `json:"folders_created"`
		SourcesCreated int `json:"sources_created"`
		Skipped        int `json:"skipped"`
	}
	existing, err := s.store.ListFolders(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	type fkey struct {
		parent int64
		name   string
	}
	folders := map[fkey]int64{}
	for _, f := range existing {
		var p int64
		if f.ParentID != nil {
			p = *f.ParentID
		}
		folders[fkey{p, strings.ToLower(f.Name)}] = f.ID
	}

	var walk func(outlines []opmlOutline, parent *int64, depth int) error
	walk = func(outlines []opmlOutline, parent *int64, depth int) error {
		for _, o := range outlines {
			if isFeedOutline(o) {
				src := outlineToSource(o, parent)
				if src == nil {
					res.Skipped++
					continue
				}
				if err := s.store.CreateSource(ctx, src); err != nil {
					if errors.Is(err, storage.ErrDuplicate) {
						res.Skipped++
						continue
					}
					return err
				}
				res.SourcesCreated++
				continue
			}
			if len(o.Outlines) == 0 {
				continue
			}
			if depth > maxFolderDepth { // too deep: keep feeds in the current folder
				if err := walk(o.Outlines, parent, depth); err != nil {
					return err
				}
				continue
			}
			var pid int64
			if parent != nil {
				pid = *parent
			}
			name := strings.TrimSpace(outlineName(o))
			if name == "" {
				name = "Imported"
			}
			id, ok := folders[fkey{pid, strings.ToLower(name)}]
			if !ok {
				f := &model.Folder{Name: name, ParentID: parent}
				if err := s.store.CreateFolder(ctx, f); err != nil {
					return err
				}
				id = f.ID
				folders[fkey{pid, strings.ToLower(name)}] = id
				res.FoldersCreated++
			}
			fid := id
			if err := walk(o.Outlines, &fid, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(doc.Body.Outlines, nil, 0); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "import stopped: " + err.Error(), "partial": res})
		return
	}
	s.kick()
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

	// Group sources and child folders by parent, then build the tree.
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
	children := make(map[int64][]*model.Folder)
	var roots []*model.Folder
	for _, f := range folders {
		if f.ParentID != nil {
			children[*f.ParentID] = append(children[*f.ParentID], f)
		} else {
			roots = append(roots, f)
		}
	}
	var build func(f *model.Folder, depth int) opmlOutline
	build = func(f *model.Folder, depth int) opmlOutline {
		o := opmlOutline{Text: f.Name, Title: f.Name}
		if depth < 8 {
			for _, c := range children[f.ID] {
				o.Outlines = append(o.Outlines, build(c, depth+1))
			}
		}
		o.Outlines = append(o.Outlines, byFolder[f.ID]...)
		return o
	}
	var body opmlBody
	for _, f := range roots {
		if o := build(f, 0); len(o.Outlines) > 0 {
			body.Outlines = append(body.Outlines, o)
		}
	}
	body.Outlines = append(body.Outlines, noFolder...)

	doc := opml{
		Version: "2.0",
		Head:    opmlHead{Title: "Noema subscriptions — " + time.Now().Format("2006-01-02")},
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
		Text:    src.Title,
		Title:   src.Title,
		XMLURL:  src.URL,
		HTMLURL: src.SiteURL,
		Type:    "rss",
	}
	if src.Type == model.SourceTypeHTML {
		// Other readers cannot subscribe to a scraped page; keep it as a link.
		o.Type, o.XMLURL, o.HTMLURL = "link", "", src.URL
	}
	return o
}
