package server

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// handleWeb serves the embedded UI. Unknown paths get index.html (the UI
// routes them), except under /api/; hashed assets are cached for good.
func (s *Server) handleWeb(w http.ResponseWriter, r *http.Request) {
	if s.web == nil {
		http.Error(w, "UI not built", http.StatusNotFound)
		return
	}
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	if strings.HasPrefix(p, "api/") {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if _, err := fs.Stat(s.web, p); errors.Is(err, fs.ErrNotExist) {
		p = "index.html" // SPA routes
	}
	if strings.HasPrefix(p, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	// s.web is the embedded UI: nothing outside it can be served.
	http.ServeFileFS(w, r, s.web, p) //nolint:gosec // G703: embedded read-only FS
}
