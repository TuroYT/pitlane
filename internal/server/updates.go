package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

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
		b, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "cannot read the request body")
			return
		}
		if json.Valid(b) && len(b) > 2 {
			detail = string(b)
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
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

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}
