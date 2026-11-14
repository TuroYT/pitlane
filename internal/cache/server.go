// Package cache serves the GitHub Actions cache from the Pitlane host, so
// that actions/cache (and the setup-* actions' `cache:`) restore and save at
// disk speed. It speaks the v1 protocol of @actions/cache (the
// `_apis/artifactcache` REST API, still used by GitHub Enterprise Server),
// which the client uses when ACTIONS_CACHE_URL is set and
// ACTIONS_CACHE_SERVICE_V2 is not; the pool's Node shim arranges that for the
// runners it starts. See docs/design/actions-cache.md.
//
// URLs carry the runner's token and the repository:
//
//	/<token>/<owner>/<repo>/_apis/artifactcache/...
//
// The token decides the owner; entries are stored per owner/repo and never
// shared across owners.
package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxKeyLen   = 512
	maxKeys     = 10
	maxChunk    = 256 << 20 // largest PATCH body accepted (the client sends 32 MB)
	uploadTTL   = 24 * time.Hour
	apiPrefix   = "_apis/artifactcache/"
	contentJSON = "application/json"
)

// Server is the actions cache server.
type Server struct {
	dir string
	log *slog.Logger
	key []byte

	mu        sync.Mutex
	runners   map[string]Identity // registered runners
	entries   map[int64]*entry
	pending   map[int64]*upload
	nextID    int64
	total     int64 // bytes of committed entries
	limit     int64 // 0 = unlimited
	hits      int64
	misses    int64
	saves     int64
	evictions int64
}

type upload struct {
	entry
	runner  string
	f       *os.File
	started time.Time
	limit   int64 // largest size accepted
}

// Open opens (or creates) the cache in dir.
func Open(dir string, log *slog.Logger) (*Server, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := loadKey(dir)
	if err != nil {
		return nil, fmt.Errorf("actions cache key: %w", err)
	}
	s := &Server{dir: dir, log: log, key: key, runners: map[string]Identity{},
		entries: map[int64]*entry{}, pending: map[int64]*upload{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	// IDs are never reused, even across restarts with an emptied directory.
	s.nextID = max(s.nextID, time.Now().UnixMilli())
	return s, nil
}

// Stats is a snapshot for the UI and metrics.
type Stats struct {
	Entries    int   `json:"entries"`
	Bytes      int64 `json:"bytes"`
	LimitBytes int64 `json:"limitBytes"`
	Hits       int64 `json:"hits"`
	Misses     int64 `json:"misses"`
	Saves      int64 `json:"saves"`
	Evictions  int64 `json:"evictions"`
}

// Stats returns the current size and the counters since start.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Entries: len(s.entries), Bytes: s.total, LimitBytes: s.limit, Hits: s.hits, Misses: s.misses,
		Saves: s.saves, Evictions: s.evictions}
}

// Handler returns the HTTP handler of the cache API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	const base = "/{token}/{owner}/{repo}/" + apiPrefix
	mux.HandleFunc("GET "+base+"cache", s.auth(s.handleLookup))
	mux.HandleFunc("GET "+base+"caches", s.auth(s.handleList))
	mux.HandleFunc("POST "+base+"caches", s.auth(s.handleReserve))
	mux.HandleFunc("PATCH "+base+"caches/{id}", s.auth(s.handleUpload))
	mux.HandleFunc("POST "+base+"caches/{id}", s.auth(s.handleCommit))
	mux.HandleFunc("GET "+base+"artifacts/{id}", s.auth(s.handleDownload))
	return mux
}

// call is an authenticated request.
type call struct {
	runner string
	ns     string
	prefix string // URL prefix up to and including the repository
}

func (s *Server) auth(h func(http.ResponseWriter, *http.Request, call)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runner, id, ok := s.identify(r.PathValue("token"))
		if !ok {
			writeErr(w, http.StatusUnauthorized, "invalid or expired Pitlane cache token")
			return
		}
		owner, repo := r.PathValue("owner"), r.PathValue("repo")
		ns, ok := namespace(id, owner, repo)
		if !ok {
			writeErr(w, http.StatusForbidden, "this job may not use the cache of "+owner+"/"+repo)
			return
		}
		h(w, r, call{runner: runner, ns: ns, prefix: "/" + r.PathValue("token") + "/" + owner + "/" + repo + "/"})
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", contentJSON)
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", contentJSON)
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// cacheEntry is the v1 ArtifactCacheEntry.
type cacheEntry struct {
	CacheKey        string `json:"cacheKey"`
	Scope           string `json:"scope"`
	CacheVersion    string `json:"cacheVersion,omitempty"`
	CreationTime    string `json:"creationTime"`
	ArchiveLocation string `json:"archiveLocation,omitempty"`
}

func (e *entry) wire() cacheEntry {
	return cacheEntry{CacheKey: e.Key, Scope: e.Scope, CacheVersion: e.Version, CreationTime: e.Created.UTC().Format(time.RFC3339)}
}

func (s *Server) handleLookup(w http.ResponseWriter, r *http.Request, c call) {
	q := r.URL.Query()
	version := q.Get("version")
	var keys []string
	for k := range strings.SplitSeq(q.Get("keys"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	if version == "" || len(keys) == 0 || len(keys) > maxKeys {
		writeErr(w, http.StatusBadRequest, "keys and version are required")
		return
	}
	read, _, _ := scopes(r.Header.Get("Authorization"))
	s.mu.Lock()
	e := s.matchLocked(c.ns, version, keys, read)
	if e == nil {
		s.misses++
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.hits++
	now := time.Now()
	e.used = now
	out := e.wire()
	id := e.ID
	s.mu.Unlock()
	// The archive mtime is the last-use time across restarts (LRU).
	_ = os.Chtimes(s.blobPath(id), now, now)
	out.ArchiveLocation = baseURL(r) + c.prefix + apiPrefix + "artifacts/" + strconv.FormatInt(id, 10)
	writeJSON(w, http.StatusOK, out)
}

// baseURL is the scheme and host the client used.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// handleList answers the client's diagnostics listing (debug logging only).
func (s *Server) handleList(w http.ResponseWriter, r *http.Request, c call) {
	key := r.URL.Query().Get("key")
	read, _, _ := scopes(r.Header.Get("Authorization"))
	var out []cacheEntry
	s.mu.Lock()
	for _, e := range s.entries {
		if e.NS == c.ns && e.Key == key && slices.Contains(read, e.Scope) {
			out = append(out, e.wire())
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"totalCount": len(out), "artifactCaches": out})
}

func (s *Server) handleReserve(w http.ResponseWriter, r *http.Request, c call) {
	var in struct {
		Key       string `json:"key"`
		Version   string `json:"version"`
		CacheSize int64  `json:"cacheSize"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if in.Key == "" || len(in.Key) > maxKeyLen || strings.Contains(in.Key, ",") || in.Version == "" || len(in.Version) > maxKeyLen || in.CacheSize < 0 {
		writeErr(w, http.StatusBadRequest, "invalid key or version")
		return
	}
	_, scope, canWrite := scopes(r.Header.Get("Authorization"))
	if !canWrite {
		writeErr(w, http.StatusForbidden, "this job may not save caches")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limit > 0 && in.CacheSize > s.limit {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("cache size of %d bytes is over the %d bytes limit of the Pitlane cache", in.CacheSize, s.limit))
		return
	}
	now := time.Now()
	for id, u := range s.pending {
		if now.Sub(u.started) > uploadTTL {
			s.dropUploadLocked(id, u)
		}
	}
	same := func(e *entry) bool {
		return e.NS == c.ns && e.Key == in.Key && e.Version == in.Version && e.Scope == scope
	}
	for _, e := range s.entries {
		if same(e) {
			writeErr(w, http.StatusConflict, "cache entry "+in.Key+" already exists")
			return
		}
	}
	for _, u := range s.pending {
		if same(&u.entry) {
			writeErr(w, http.StatusConflict, "cache entry "+in.Key+" is being saved by another job")
			return
		}
	}
	s.nextID++
	id := s.nextID
	f, err := os.OpenFile(s.partPath(id), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		s.log.Error("actions cache: create upload", "error", err)
		writeErr(w, http.StatusInternalServerError, "cannot create the upload")
		return
	}
	limit := s.limit
	if in.CacheSize > 0 {
		limit = in.CacheSize
	}
	s.pending[id] = &upload{entry: entry{ID: id, NS: c.ns, Key: in.Key, Version: in.Version, Scope: scope},
		runner: c.runner, f: f, started: now, limit: limit}
	writeJSON(w, http.StatusCreated, map[string]int64{"cacheId": id})
}

// dropUploadLocked abandons an unfinished upload. Callers hold s.mu.
func (s *Server) dropUploadLocked(id int64, u *upload) {
	delete(s.pending, id)
	_ = u.f.Close()
	_ = os.Remove(s.partPath(id))
}

// ownUpload returns the caller's unfinished upload id.
func (s *Server) ownUpload(r *http.Request, c call) (int64, *upload, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.pending[id]
	if !ok || u.runner != c.runner || u.NS != c.ns {
		return 0, nil, false
	}
	return id, u, true
}

// parseRange parses "bytes start-end/*".
func parseRange(h string) (start, end int64, err error) {
	rest, ok := strings.CutPrefix(h, "bytes ")
	if !ok {
		return 0, 0, errors.New("bad Content-Range")
	}
	rng, _, _ := strings.Cut(rest, "/")
	a, b, ok := strings.Cut(rng, "-")
	if !ok {
		return 0, 0, errors.New("bad Content-Range")
	}
	if start, err = strconv.ParseInt(a, 10, 64); err != nil {
		return 0, 0, err
	}
	if end, err = strconv.ParseInt(b, 10, 64); err != nil {
		return 0, 0, err
	}
	if start < 0 || end < start || end-start+1 > maxChunk {
		return 0, 0, errors.New("bad Content-Range")
	}
	return start, end, nil
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, c call) {
	_, u, ok := s.ownUpload(r, c)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such upload")
		return
	}
	start, end, err := parseRange(r.Header.Get("Content-Range"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if u.limit > 0 && end >= u.limit {
		writeErr(w, http.StatusBadRequest, "chunk beyond the reserved size")
		return
	}
	n := end - start + 1
	// Parallel chunks write at their own offsets (pwrite), so they need no lock.
	written, err := io.Copy(io.NewOffsetWriter(u.f, start), io.LimitReader(r.Body, n))
	if err != nil || written != n {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("chunk incomplete: %d of %d bytes", written, n))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request, c call) {
	id, u, ok := s.ownUpload(r, c)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such upload")
		return
	}
	var in struct {
		Size int64 `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[id] != u { // revoked meanwhile
		writeErr(w, http.StatusNotFound, "no such upload")
		return
	}
	st, err := u.f.Stat()
	if err != nil || st.Size() != in.Size || in.Size <= 0 {
		s.dropUploadLocked(id, u)
		writeErr(w, http.StatusBadRequest, "uploaded size does not match")
		return
	}
	delete(s.pending, id)
	e := u.entry
	e.Size, e.Created, e.used = in.Size, time.Now(), time.Now()
	err = u.f.Close()
	if err == nil {
		err = os.Rename(s.partPath(id), s.blobPath(id))
	}
	if err == nil {
		err = s.writeMeta(&e)
	}
	if err != nil {
		s.log.Error("actions cache: commit", "error", err)
		_ = os.Remove(s.partPath(id))
		s.removeFiles(id)
		writeErr(w, http.StatusInternalServerError, "cannot store the cache entry")
		return
	}
	s.entries[id] = &e
	s.total += e.Size
	s.saves++
	if s.limit > 0 {
		s.evictLocked(s.limit)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, c call) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such entry")
		return
	}
	s.mu.Lock()
	e, ok := s.entries[id]
	ok = ok && e.NS == c.ns
	var created time.Time
	if ok {
		created = e.Created
	}
	s.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "no such entry")
		return
	}
	f, err := os.Open(s.blobPath(id))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such entry") // evicted meanwhile
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	// An open file survives its eviction: the download completes.
	http.ServeContent(w, r, "", created, f)
}
