// Package web serves the production frontend compiled by Vite. Keeping the
// generated assets in this subpackage lets pigo-server ship as one binary while
// frontend development remains a normal, independent pnpm project.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist is the Vite build, which is not in git: build the frontend first
// (see cmd/pigo-server/README.md). "all:" takes in dist/.gitkeep, the one
// tracked file, so a fresh checkout still compiles — serving no pages.
//
//go:embed all:dist
var assets embed.FS

// Handler returns an SPA-aware static file handler. Real assets are served
// directly; unknown non-API paths fall back to index.html for client routing.
func Handler() http.Handler {
	dist, err := fs.Sub(assets, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "." || name == "" {
			serveIndex(w, r, dist)
			return
		}
		if info, statErr := fs.Stat(dist, name); statErr == nil && !info.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		serveIndex(w, r, dist)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, dist fs.FS) {
	content, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		http.Error(w, "embedded frontend is unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(content)
}
