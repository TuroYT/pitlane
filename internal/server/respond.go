package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// writeJSON answers v as JSON with status 200.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr answers {"error": msg} with the status code.
func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// apiError is the body of an error response. Code and RequestID are only
// set for failures on Pitlane's side (see internalErr).
type apiError struct {
	Error     string `json:"error"`
	Code      string `json:"code,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// internalErr answers a request that failed on Pitlane's side (database,
// GitHub API): the client gets msg, a stable code and a request ID, while
// err itself only goes to the log, under the same request ID. msg must not
// carry any error text.
func (s *Server) internalErr(w http.ResponseWriter, r *http.Request, status int, code, msg string, err error) {
	id := requestID(r)
	if s.log != nil {
		s.log.Error(msg, "code", code, "request_id", id, "method", r.Method, "path", r.URL.Path, "status", status, "error", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", id)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{Error: fmt.Sprintf("%s (error %s, request %s)", msg, code, id), Code: code, RequestID: id})
}

// requestID is the X-Request-Id a reverse proxy set on the request, when it
// looks like one, else a new random ID.
func requestID(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" && len(v) <= 64 && strings.IndexFunc(v, func(c rune) bool {
		return c != '-' && c != '_' && c != '.' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z')
	}) < 0 {
		return v
	}
	return randomHex(8)
}

// badJSON answers a request body that is not the JSON expected.
func badJSON(w http.ResponseWriter) { writeErr(w, http.StatusBadRequest, "invalid JSON body") }

// readJSON decodes the JSON body into v. When it cannot, it answers 400
// "invalid JSON body" and returns false. Bound body with http.MaxBytesReader
// where it matters.
func readJSON(w http.ResponseWriter, body io.Reader, v any) bool {
	if err := json.NewDecoder(body).Decode(v); err != nil {
		badJSON(w)
		return false
	}
	return true
}

// randomHex returns n random bytes, hex encoded.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
