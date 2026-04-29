package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"rssfeeder/internal/storage"
)

// Config holds runtime settings for the API server.
type Config struct {
	JWTSecret string
	Password  string // plain-text password for the single user
}

// Server is the HTTP API.
type Server struct {
	cfg   Config
	store *storage.Store
}

// New creates a Server.
func New(cfg Config, store *storage.Store) *Server {
	return &Server{cfg: cfg, store: store}
}

// Handler returns the fully-wired chi router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Post("/auth/login", s.handleLogin)

	r.Group(func(r chi.Router) {
		r.Use(s.authMiddleware)

		// Sync
		r.Get("/sync", s.handleSync)

		// Sources
		r.Get("/sources", s.handleListSources)
		r.Post("/sources", s.handleCreateSource)
		r.Get("/sources/{id}", s.handleGetSource)
		r.Put("/sources/{id}", s.handleUpdateSource)
		r.Delete("/sources/{id}", s.handleDeleteSource)

		// Folders
		r.Get("/folders", s.handleListFolders)
		r.Post("/folders", s.handleCreateFolder)
		r.Put("/folders/{id}", s.handleUpdateFolder)
		r.Delete("/folders/{id}", s.handleDeleteFolder)

		// Items
		r.Put("/items/{id}/state", s.handleUpdateItemState)
		r.Get("/items/search", s.handleSearch)

		// OPML
		r.Post("/opml/import", s.handleOPMLImport)
		r.Get("/opml/export", s.handleOPMLExport)
	})

	return r
}

// ── Response helpers ──────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
