package api

import (
	"context"
	"encoding/json"
	"expvar"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"rssfeeder/internal/fetcher"
	"rssfeeder/internal/storage"
	"rssfeeder/internal/voice"
)

// Config holds runtime settings for the API server.
type Config struct {
	JWTSecret string
	Password  string        // the single user's password
	TokenTTL  time.Duration // default 90 days
	// TrustProxy honours X-Forwarded-For / X-Real-IP for client addresses.
	// Enable only behind a reverse proxy that sets them; otherwise any client
	// could spoof its address and dodge the per-IP login limit.
	TrustProxy bool
	Location   *time.Location // for "today" in voice queries
	// Static serves the embedded web client (nil disables it).
	Static http.Handler
}

// Server is the HTTP API.
type Server struct {
	cfg    Config
	store  *storage.Store
	sched  *fetcher.Scheduler
	disc   *fetcher.Discoverer
	agent  *voice.Agent
	logins *loginLimiter

	failDelay time.Duration // pause after a wrong password
}

// New creates a Server. sched may be nil in tests (no polling, no push).
func New(cfg Config, store *storage.Store, sched *fetcher.Scheduler) *Server {
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = 90 * 24 * time.Hour
	}
	s := &Server{
		cfg:    cfg,
		store:  store,
		sched:  sched,
		disc:   fetcher.NewDiscoverer(),
		logins: newLoginLimiter(),

		failDelay: 400 * time.Millisecond,
	}
	s.agent = &voice.Agent{Store: store, Loc: cfg.Location}
	if sched != nil {
		ex := sched.Extractor()
		s.agent.FullText = func(ctx context.Context, id int64, url string) error {
			return fetcher.FetchFullText(ctx, store, ex, id, url)
		}
	}
	return s
}

// Handler returns the fully-wired router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	if s.cfg.TrustProxy {
		r.Use(middleware.RealIP)
	}
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5, "application/json", "text/html", "text/css", "application/javascript",
		"text/javascript", "image/svg+xml", "text/xml", "application/manifest+json", "text/plain"))
	r.Use(securityHeaders)

	r.Get("/healthz", s.handleHealth)
	r.Post("/auth/login", s.handleLogin)
	r.Post("/auth/logout", s.handleLogout)

	if s.sched != nil && s.sched.WebSub() != nil {
		ws := s.sched.WebSub()
		r.Get("/websub/{id}", func(w http.ResponseWriter, r *http.Request) {
			if id, ok := pathID(w, r); ok {
				ws.HandleVerify(w, r, id)
			}
		})
		r.Post("/websub/{id}", func(w http.ResponseWriter, r *http.Request) {
			if id, ok := pathID(w, r); ok {
				ws.HandlePush(w, r, id)
			}
		})
	}

	r.Group(func(r chi.Router) {
		r.Use(s.authMiddleware)
		r.Use(jsonBodyLimit)

		r.Get("/auth/check", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })

		// Sync (native clients) and counts (badges, sidebars, voice)
		r.Get("/sync", s.handleSync)
		r.Get("/counts", s.handleCounts)

		// Sources
		r.Post("/discover", s.handleDiscover)
		r.Get("/sources", s.handleListSources)
		r.Post("/sources", s.handleCreateSource)
		r.Post("/sources/refresh", s.handleRefresh)
		r.Get("/sources/{id}", s.handleGetSource)
		r.Put("/sources/{id}", s.handleUpdateSource)
		r.Delete("/sources/{id}", s.handleDeleteSource)

		// Folders
		r.Get("/folders", s.handleListFolders)
		r.Post("/folders", s.handleCreateFolder)
		r.Put("/folders/{id}", s.handleUpdateFolder)
		r.Delete("/folders/{id}", s.handleDeleteFolder)

		// Items
		r.Get("/items", s.handleListItems)
		r.Get("/items/search", s.handleSearch)
		r.Post("/items/state", s.handleBulkState)
		r.Post("/items/mark-read", s.handleMarkRead)
		r.Get("/items/{id}", s.handleGetItem)
		r.Put("/items/{id}/state", s.handleUpdateItemState)
		r.Get("/items/{id}/speech", s.handleSpeech)

		// Voice agent
		r.Get("/briefing", s.handleBriefing)
		r.Post("/ask", s.handleAsk)

		// OPML
		r.Post("/opml/import", s.handleOPMLImport)
		r.Get("/opml/export", s.handleOPMLExport)

		// Observability
		r.Handle("/debug/vars", expvar.Handler())
	})

	if s.cfg.Static != nil {
		r.NotFound(s.cfg.Static.ServeHTTP)
	}
	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// securityHeaders applies to everything, including the web client. Article
// HTML is sanitised server-side; the CSP is the second line of defence.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: http: data: blob:; media-src 'self' https: http: blob:; "+
			"style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// jsonBodyLimit caps request bodies (OPML files included) at 5 MB.
func jsonBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, fetcher.MaxBody)
		}
		next.ServeHTTP(w, r)
	})
}

// ── Response helpers ──────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

func qInt(r *http.Request, key string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return n
}
