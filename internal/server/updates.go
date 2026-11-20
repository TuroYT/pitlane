package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// maxUpdatesAudit bounds the request body of an update mutation kept for the
// audit log. A longer body is still forwarded whole, without audit detail.
const maxUpdatesAudit = 4096

// handleUpdates serves the update API (internal/update) and records its
// successful mutations, with their JSON body, in the audit log.
func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	if s.upd == nil {
		writeErr(w, http.StatusNotFound, "updates are not available")
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		s.upd.ServeHTTP(w, r)
		return
	}
	// Mutations go to the audit log, with the preferences saved.
	var detail string
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxUpdatesAudit))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "cannot read the request body")
			return
		}
		if json.Valid(b) && len(b) > 2 {
			detail = string(b)
		}
		// Forward the whole body: the bytes read, then the rest.
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(b), r.Body), r.Body}
	}
	sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
	s.upd.ServeHTTP(sw, r)
	if sw.code < 400 {
		s.audit(r, "updates."+strings.TrimPrefix(r.URL.Path, "/api/updates/"), "", detail)
	}
}

// statusWriter remembers the status code of a response.
type statusWriter struct {
	http.ResponseWriter
	code int
}

// WriteHeader records code and sends it.
func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}
