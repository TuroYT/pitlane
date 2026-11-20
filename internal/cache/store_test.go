package cache

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeEntryFiles stores an entry the way a commit does, then lets the test
// damage it.
func writeEntryFiles(t *testing.T, s *Server, meta string, id int64, blob string) {
	t.Helper()
	if err := os.MkdirAll(s.entryDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if meta != "" {
		if err := os.WriteFile(s.metaPath(id), []byte(meta), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if blob != "" {
		if err := os.WriteFile(s.blobPath(id), []byte(blob), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// load keeps only entries whose metadata and archive agree, and says so
// instead of failing the start.
func TestLoadValidation(t *testing.T) {
	t.Parallel()
	good := func(id int64, size int) string {
		b, _ := json.Marshal(entry{ID: id, NS: "acme/api", Key: "k", Version: "v", Size: int64(size)})
		return string(b)
	}
	tests := []struct {
		name string
		meta string
		blob string
		keep bool
	}{
		{"consistent", good(7, 4), "abcd", true},
		{"missing archive", good(7, 4), "", false},
		{"missing metadata", "", "abcd", false},
		{"archive size differs", good(7, 4), "abc", false},
		{"invalid JSON", "{", "abcd", false},
		{"id differs from the file name", good(8, 4), "abcd", false},
		{"no namespace", `{"id":7,"key":"k","size":4}`, "abcd", false},
		{"no key", `{"id":7,"ns":"acme/api","size":4}`, "abcd", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s, err := Open(dir, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			writeEntryFiles(t, s, tc.meta, 7, tc.blob)
			s2, err := Open(dir, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			st := s2.Stats()
			if tc.keep != (st.Entries == 1) || (tc.keep && st.Bytes != 4) {
				t.Fatalf("stats: %+v", st)
			}
			_, metaErr := os.Stat(s2.metaPath(7))
			_, blobErr := os.Stat(s2.blobPath(7))
			if !tc.keep && (metaErr == nil || blobErr == nil) {
				t.Fatalf("leftovers of a dropped entry: meta=%v blob=%v", metaErr, blobErr)
			}
			if tc.keep && s2.nextID < 7 {
				t.Fatalf("nextID %d reuses a stored id", s2.nextID)
			}
		})
	}
}

// Unfinished uploads and stray files do not survive a restart, and new ids
// stay above anything stored.
func TestLoadCleansUp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.tmpDir(), "5.part"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"notanumber.json", "5.txt", "9.json.tmp", "12.bin.bak"} {
		if err := os.WriteFile(filepath.Join(s.entryDir(), name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	future := time.Now().Add(time.Hour).UnixMilli()
	meta, _ := json.Marshal(entry{ID: future, NS: "acme/api", Key: "k", Version: "v", Size: 1})
	writeEntryFiles(t, s, string(meta), future, "x")

	s2, err := Open(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(s2.tmpDir()); len(left) != 0 {
		t.Errorf("unfinished uploads survived: %v", left)
	}
	left, _ := os.ReadDir(s2.entryDir())
	var names []string
	for _, f := range left {
		names = append(names, f.Name())
	}
	if want := strconv.FormatInt(future, 10); strings.Join(names, ",") != want+".bin,"+want+".json" {
		t.Errorf("entries directory: %v", names)
	}
	if s2.nextID < future {
		t.Errorf("nextID %d is not above the stored id %d", s2.nextID, future)
	}
}

func TestLRUOrder(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_700_000_000, 0)
	for id, used := range map[int64]time.Time{
		3: base.Add(2 * time.Second),
		1: base.Add(time.Second),
		2: base.Add(time.Second), // same time as 1: the older id goes first
		4: base,
	} {
		s.entries[id] = &entry{ID: id, used: used, Size: 1}
	}
	var got []int64
	for _, e := range s.lruLocked() {
		got = append(got, e.ID)
	}
	if want := []int64{4, 1, 2, 3}; !equalInts(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

func equalInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Eviction takes an owner's own oldest entries first, never the entry just
// saved, and leaves the other owners alone.
func TestEnforceLimitsEvictionOrder(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_700_000_000, 0)
	add := func(id int64, ns string, size int64, age time.Duration) {
		s.entries[id] = &entry{ID: id, NS: ns, Key: "k", Size: size, used: base.Add(age)}
		s.total += size
	}
	add(1, "acme/api", 30, 0)
	add(2, "beta/api", 30, time.Second)
	add(3, "acme/web", 30, 2*time.Second)
	add(4, "acme/api", 30, 3*time.Second) // the entry just saved, the newest
	s.limit, s.ownerPct = 100, 50         // 50 bytes per owner

	s.mu.Lock()
	s.enforceLimitsLocked(1) // keep entry 1 although it is the oldest
	s.mu.Unlock()

	// acme stores 90 bytes for a 50 bytes share. Entry 1 is kept, so its
	// other entries go, oldest first (3 then 4), and beta is untouched.
	for id, want := range map[int64]bool{1: true, 2: true, 3: false, 4: false} {
		if _, ok := s.entries[id]; ok != want {
			t.Errorf("entry %d present = %v, want %v", id, ok, want)
		}
	}
	if s.total != 60 || s.evictions != 2 {
		t.Fatalf("total %d, evictions %d", s.total, s.evictions)
	}
}

func TestEvictNothingToFree(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Evict(1 << 20); got != 0 {
		t.Fatalf("empty cache freed %d", got)
	}
	if got := s.Evict(-5); got != 0 {
		t.Fatalf("negative request freed %d", got)
	}
}

func TestSetLimitsClampsPercent(t *testing.T) {
	t.Parallel()
	s := &Server{entries: map[int64]*entry{}}
	for in, want := range map[int]int{-10: 1, 0: 1, 1: 1, 50: 50, 100: 100, 250: 100} {
		s.SetLimits(1000, in)
		if s.ownerPct != want {
			t.Errorf("percent %d stored as %d, want %d", in, s.ownerPct, want)
		}
	}
}

func TestScopesVariants(t *testing.T) {
	t.Parallel()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	acs := func(v any) string {
		b, _ := json.Marshal(v)
		return "Bearer h." + enc(map[string]string{"ac": string(b)}) + ".s"
	}
	type perm = map[string]any
	tests := []struct {
		name     string
		header   string
		read     string
		write    string
		canWrite bool
	}{
		{"padded payload", "Bearer h." + enc(map[string]string{"ac": `[{"Scope":"s","Permission":3}]`}) + "==.s", "s", "s", true},
		{"payload is not JSON", "Bearer h." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".s", "", "", true},
		{"no ac claim", "Bearer h." + enc(map[string]string{"x": "y"}) + ".s", "", "", true},
		{"ac is not JSON", "Bearer h." + enc(map[string]string{"ac": "nope"}) + ".s", "", "", true},
		{"empty ac list", acs([]perm{}), "", "", true},
		{"read-only", acs([]perm{{"Scope": "a", "Permission": 1}}), "a", "", false},
		{"no permission at all", acs([]perm{{"Scope": "a", "Permission": 0}}), "", "", false},
		{"write-only scope", acs([]perm{{"Scope": "a", "Permission": 2}}), "", "a", true},
		{"own ref first then base", acs([]perm{{"Scope": "feat", "Permission": 3}, {"Scope": "main", "Permission": 1}}), "feat,main", "feat", true},
		{"writable scope listed after readable ones", acs([]perm{{"Scope": "main", "Permission": 1}, {"Scope": "feat", "Permission": 3}}), "feat,main", "feat", true},
		{"duplicate scopes are read once", acs([]perm{{"Scope": "a", "Permission": 3}, {"Scope": "a", "Permission": 1}}), "a", "a", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			read, write, ok := scopes(tc.header)
			if strings.Join(read, ",") != tc.read || write != tc.write || ok != tc.canWrite {
				t.Fatalf("got %q %q %v, want %q %q %v", read, write, ok, tc.read, tc.write, tc.canWrite)
			}
		})
	}
}

func TestForwardListenError(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := Forward(context.Background(), ln.Addr().String(), "127.0.0.1:1", slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("Forward on a busy address succeeded")
	}
}

// When the target is down the client's connection is closed, not left hanging.
func TestForwardTargetUnreachable(t *testing.T) {
	t.Parallel()
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := dead.Addr().String()
	_ = dead.Close()

	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()
	go func() {
		c, err := front.Accept()
		if err == nil {
			relay(c, target, slog.New(slog.DiscardHandler))
		}
	}()
	c, err := net.Dial("tcp", front.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := io.ReadAll(c); err != nil || len(n) != 0 {
		t.Fatalf("read %q: %v", n, err)
	}
}
