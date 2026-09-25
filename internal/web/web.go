// Package web embeds Noema's browser client: a dependency-free progressive
// web app (installable, offline-capable) served from the same binary.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var files embed.FS

// Handler serves the client. Unknown paths fall back to index.html so deep
// links (/item/123) work; hashed assets are cached, the shell is revalidated.
func Handler() http.Handler {
	root, _ := fs.Sub(files, "static")
	fileServer := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(root, p); err != nil {
			if strings.Contains(path.Base(p), ".") {
				http.NotFound(w, r) // a missing asset, not a client route
				return
			}
			r.URL.Path = "/"
			p = "index.html"
		}
		switch {
		case p == "sw.js" || p == "index.html" || p == "manifest.webmanifest":
			w.Header().Set("Cache-Control", "no-cache")
		default:
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		if p == "sw.js" {
			w.Header().Set("Service-Worker-Allowed", "/")
		}
		fileServer.ServeHTTP(w, r)
	})
}
