package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// entry is a committed cache archive. Entries are immutable.
type entry struct {
	ID      int64     `json:"id"`
	NS      string    `json:"ns"` // "owner/repo", lowercase
	Key     string    `json:"key"`
	Version string    `json:"version"`
	Scope   string    `json:"scope"` // branch scope from the job token ("" when unknown)
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
	used    time.Time // last restore (archive mtime on disk)
}

func (s *Server) entryDir() string { return filepath.Join(s.dir, "entries") }
func (s *Server) tmpDir() string   { return filepath.Join(s.dir, "tmp") }

func (s *Server) metaPath(id int64) string {
	return filepath.Join(s.entryDir(), strconv.FormatInt(id, 10)+".json")
}

func (s *Server) blobPath(id int64) string {
	return filepath.Join(s.entryDir(), strconv.FormatInt(id, 10)+".bin")
}

func (s *Server) partPath(id int64) string {
	return filepath.Join(s.tmpDir(), strconv.FormatInt(id, 10)+".part")
}

// load rebuilds the index from disk. Unfinished uploads are dropped, and so
// is any entry whose metadata or archive is missing or inconsistent.
func (s *Server) load() error {
	if err := os.RemoveAll(s.tmpDir()); err != nil {
		return err
	}
	for _, d := range []string{s.entryDir(), s.tmpDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	files, err := os.ReadDir(s.entryDir())
	if err != nil {
		return err
	}
	seen := map[int64]bool{}
	for _, f := range files {
		name := f.Name()
		base, ext, _ := strings.Cut(name, ".")
		id, err := strconv.ParseInt(base, 10, 64)
		if err != nil || (ext != "json" && ext != "bin") {
			_ = os.Remove(filepath.Join(s.entryDir(), name))
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		e, err := s.readEntry(id)
		if err != nil {
			s.log.Warn("dropping unreadable actions cache entry", "id", id, "error", err)
			s.removeFiles(id)
			continue
		}
		s.entries[e.ID] = e
		s.total += e.Size
		s.nextID = max(s.nextID, e.ID)
	}
	return nil
}

func (s *Server) readEntry(id int64) (*entry, error) {
	b, err := os.ReadFile(s.metaPath(id))
	if err != nil {
		return nil, err
	}
	var e entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	if e.ID != id || e.NS == "" || e.Key == "" {
		return nil, errors.New("invalid metadata")
	}
	st, err := os.Stat(s.blobPath(id))
	if err != nil {
		return nil, err
	}
	if st.Size() != e.Size {
		return nil, fmt.Errorf("archive is %d bytes, metadata says %d", st.Size(), e.Size)
	}
	e.used = st.ModTime()
	return &e, nil
}

func (s *Server) removeFiles(id int64) {
	_ = os.Remove(s.metaPath(id))
	_ = os.Remove(s.blobPath(id))
}

// writeMeta atomically writes an entry's metadata.
func (s *Server) writeMeta(e *entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp := s.metaPath(e.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.metaPath(e.ID))
}

// removeLocked deletes an entry. Callers hold s.mu.
func (s *Server) removeLocked(e *entry) {
	delete(s.entries, e.ID)
	s.total -= e.Size
	s.removeFiles(e.ID)
}

// lruLocked returns the entries, least recently used first.
func (s *Server) lruLocked() []*entry {
	out := make([]*entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].used.Equal(out[j].used) {
			return out[i].used.Before(out[j].used)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// evictLocked removes least recently used entries until at most limit bytes
// are stored, and returns the bytes freed. Callers hold s.mu.
func (s *Server) evictLocked(limit int64) int64 {
	if s.total <= limit {
		return 0
	}
	var freed int64
	for _, e := range s.lruLocked() {
		if s.total <= limit {
			break
		}
		freed += e.Size
		s.removeLocked(e)
		s.evictions++
	}
	return freed
}

// DefaultOwnerPercent is the share of the cache size one owner may use by
// default.
const DefaultOwnerPercent = 50

// ownerOf is the owner of a namespace ("owner/repo").
func ownerOf(ns string) string {
	owner, _, _ := strings.Cut(ns, "/")
	return owner
}

// ownerLimitLocked is the most one owner may store: its share of the limit,
// at least one byte; 0 when there is no limit. Callers hold s.mu.
func (s *Server) ownerLimitLocked() int64 {
	if s.limit <= 0 {
		return 0
	}
	return max(1, s.limit/100*int64(s.ownerPct)+s.limit%100*int64(s.ownerPct)/100)
}

// evictOwnerLocked removes the least recently used entries of an owner,
// never keep, until the owner stores at most limit bytes. Callers hold s.mu.
func (s *Server) evictOwnerLocked(owner string, limit int64, keep int64) {
	var stored int64
	for _, e := range s.entries {
		if ownerOf(e.NS) == owner {
			stored += e.Size
		}
	}
	for _, e := range s.lruLocked() {
		if stored <= limit {
			return
		}
		if e.ID == keep || ownerOf(e.NS) != owner {
			continue
		}
		stored -= e.Size
		s.removeLocked(e)
		s.evictions++
	}
}

// enforceLimitsLocked brings every owner back within its share, evicting
// its own least recently used entries (never keep, the entry just saved: an
// owner keeps at least that one), then the whole cache within the limit,
// least recently used first. Callers hold s.mu.
func (s *Server) enforceLimitsLocked(keep int64) {
	if s.limit <= 0 {
		return
	}
	if ownerLimit := s.ownerLimitLocked(); ownerLimit < s.limit {
		owners := map[string]bool{}
		for _, e := range s.entries {
			owners[ownerOf(e.NS)] = true
		}
		for o := range owners {
			s.evictOwnerLocked(o, ownerLimit, keep)
		}
	}
	s.evictLocked(s.limit)
}

// Evict removes least recently used entries until at least n bytes are
// freed (or nothing is left) and returns the bytes freed. The low-disk
// pruning calls it.
func (s *Server) Evict(n int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	freed := s.evictLocked(max(0, s.total-n))
	if freed > 0 {
		s.log.Info("evicted actions cache entries for disk space", "freedMB", freed>>20)
	}
	return freed
}

// SetLimit sets the maximum stored bytes (0 = no limit) and evicts down to it.
func (s *Server) SetLimit(bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limit = bytes
	s.enforceLimitsLocked(0)
}

// SetOwnerShare sets the share of the limit one owner may use, in percent
// (1 to 100), and evicts down to it.
func (s *Server) SetOwnerShare(percent int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownerPct = min(100, max(1, percent))
	s.enforceLimitsLocked(0)
}

// match finds the entry to restore, as GitHub does: for each readable scope
// in order, for each key in order, an exact match first, then the newest
// entry whose key starts with it. Callers hold s.mu.
func (s *Server) matchLocked(ns, version string, keys, scopes []string) *entry {
	for _, scope := range scopes {
		for _, k := range keys {
			var exact, prefix *entry
			for _, e := range s.entries {
				if e.NS != ns || e.Version != version || e.Scope != scope || !strings.HasPrefix(e.Key, k) {
					continue
				}
				if e.Key == k && newer(e, exact) {
					exact = e
				}
				if newer(e, prefix) {
					prefix = e
				}
			}
			if exact != nil {
				return exact
			}
			if prefix != nil {
				return prefix
			}
		}
	}
	return nil
}

func newer(e, than *entry) bool {
	return than == nil || e.Created.After(than.Created) || (e.Created.Equal(than.Created) && e.ID > than.ID)
}
