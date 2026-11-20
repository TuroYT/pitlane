package cache

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reserve starts an upload and returns the status and the cache id.
func (c client) reserve(key, version string, size int64) (int, string) {
	c.t.Helper()
	body, _ := json.Marshal(map[string]any{"key": key, "version": version, "cacheSize": size})
	res := c.do("POST", "_apis/artifactcache/caches", bytes.NewReader(body))
	var out struct{ CacheID int64 }
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, fmt.Sprint(out.CacheID)
}

// roToken is a job token that may read the scope but not write it.
func roToken(scope string) string {
	b, _ := json.Marshal([]map[string]any{{"Scope": scope, "Permission": 1}})
	payload, _ := json.Marshal(map[string]string{"ac": string(b)})
	return "x." + base64.RawURLEncoding.EncodeToString(payload) + ".y"
}

func TestDirAndOpenErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Dir() != dir {
		t.Fatalf("Dir() = %q, want %q", s.Dir(), dir)
	}

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	keyDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(keyDir, "token.key"), 0o700); err != nil {
		t.Fatal(err)
	}
	entriesIsFile := t.TempDir()
	if err := os.WriteFile(filepath.Join(entriesIsFile, "entries"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"directory is a file":      file,
		"under a file":             filepath.Join(file, "sub"),
		"token key unreadable":     keyDir,
		"entries directory broken": entriesIsFile,
	} {
		if _, err := Open(dir, nil); err == nil {
			t.Errorf("%s: Open succeeded", name)
		}
	}
}

// A truncated key file is replaced, which invalidates the tokens issued with it.
func TestKeyRegeneratedWhenCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s1, _ := newServer(t, dir)
	id := Identity{Owner: "acme"}
	tok := s1.Register("r", id)
	keyFile := filepath.Join(dir, "token.key")
	if b, err := os.ReadFile(keyFile); err != nil || len(b) != 32 || fmt.Sprintf("%x", b) == strings.Repeat("00", 32) {
		t.Fatalf("key: %v %d", err, len(b))
	}
	if err := os.WriteFile(keyFile, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, _ := newServer(t, dir)
	if got := s2.Register("r", id); got == tok {
		t.Fatal("token unchanged although the key was replaced")
	}
	if b, _ := os.ReadFile(keyFile); len(b) != 32 {
		t.Fatalf("new key has %d bytes", len(b))
	}
	if st, err := os.Stat(keyFile); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key permissions: %v %v", st, err)
	}
}

func TestAuthFailures(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	good := s.Register("pitlane-a", Identity{Owner: "acme"})
	repoTarget := s.Register("pitlane-r", Identity{Owner: "acme", Repo: "web"})
	other, _ := newServer(t, "") // another key: its tokens are forged here
	forged := other.Register("pitlane-a", Identity{Owner: "acme"})
	noOwner := s.Register("pitlane-n", Identity{})

	tests := []struct {
		name string
		path string
		want int
	}{
		{"valid", good + "/acme/api", http.StatusNoContent},
		{"owner case-insensitive", good + "/ACME/api", http.StatusNoContent},
		{"other owner", good + "/evil/api", http.StatusForbidden},
		{"repo with bad characters", good + "/acme/a$b", http.StatusForbidden},
		{"repository target, own repo", repoTarget + "/acme/web", http.StatusNoContent},
		{"repository target, own repo case", repoTarget + "/acme/WEB", http.StatusNoContent},
		{"repository target, other repo", repoTarget + "/acme/api", http.StatusForbidden},
		{"identity without owner", noOwner + "/acme/api", http.StatusForbidden},
		{"token of another server", forged + "/acme/api", http.StatusUnauthorized},
		{"unknown runner", "ghost." + strings.Split(good, ".")[1] + "/acme/api", http.StatusUnauthorized},
		{"tampered signature", good[:len(good)-2] + "AA/acme/api", http.StatusUnauthorized},
		{"no signature", "pitlane-a/acme/api", http.StatusUnauthorized},
		{"empty token", "x/acme/api", http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := http.Get(ts.URL + "/" + tc.path + "/_apis/artifactcache/cache?keys=k&version=v")
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != tc.want {
				t.Fatalf("status %d, want %d", res.StatusCode, tc.want)
			}
		})
	}
}

// Unauthorised requests never reach the handlers, whatever the verb.
func TestAuthGuardsEveryRoute(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	tok := s.Register("pitlane-a", Identity{Owner: "acme"})
	base := ts.URL + "/" + tok + "/evil/api/_apis/artifactcache/"
	for _, r := range []struct{ method, path string }{
		{"GET", "cache?keys=k&version=v"}, {"GET", "caches?key=k"}, {"POST", "caches"},
		{"PATCH", "caches/1"}, {"POST", "caches/1"}, {"GET", "artifacts/1"},
	} {
		req, _ := http.NewRequest(r.method, base+r.path, strings.NewReader(`{}`))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s: %d, want 403", r.method, r.path, res.StatusCode)
		}
	}
}

func TestMethodsAndUnknownRoutes(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"DELETE", "_apis/artifactcache/caches", http.StatusMethodNotAllowed},
		{"PUT", "_apis/artifactcache/cache", http.StatusMethodNotAllowed},
		{"POST", "_apis/artifactcache/cache", http.StatusMethodNotAllowed},
		{"GET", "_apis/artifactcache/nothing", http.StatusNotFound},
		{"GET", "other", http.StatusNotFound},
	} {
		if res := c.do(tc.method, tc.path, nil); res.StatusCode != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, res.StatusCode, tc.want)
		}
	}
}

func TestLookupBadRequests(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	eleven := strings.TrimSuffix(strings.Repeat("k,", maxKeys+1), ",")
	for name, q := range map[string]string{
		"no query":       "",
		"no version":     "keys=k",
		"no keys":        "version=v",
		"blank keys":     "keys=,%20,&version=v",
		"too many keys":  "keys=" + eleven + "&version=v",
		"empty version":  "keys=k&version=",
		"only separator": "keys=,&version=v",
	} {
		if res := c.do("GET", "_apis/artifactcache/cache?"+q, nil); res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, res.StatusCode)
		}
	}
	// maxKeys is the limit, inclusive; blanks around keys are ignored.
	ten := strings.TrimSuffix(strings.Repeat("k,", maxKeys), ",")
	if res := c.do("GET", "_apis/artifactcache/cache?keys="+ten+"&version=v", nil); res.StatusCode != http.StatusNoContent {
		t.Errorf("%d keys: %d", maxKeys, res.StatusCode)
	}
	if st := s.Stats(); st.Misses != 1 {
		t.Fatalf("bad requests must not count as misses: %+v", st)
	}
}

func TestList(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	id := Identity{Owner: "acme"}
	main := newClient(t, s, ts, "pitlane-main", id, "acme/api", "refs/heads/main")
	feat := newClient(t, s, ts, "pitlane-feat", id, "acme/api", "refs/heads/feat", "refs/heads/main")
	web := newClient(t, s, ts, "pitlane-web", id, "acme/web", "refs/heads/main")
	evil := newClient(t, s, ts, "pitlane-evil", Identity{Owner: "evil"}, "evil/api", "refs/heads/main")
	for c, data := range map[client]string{main: "main", feat: "feat", web: "web", evil: "evil"} {
		if code := c.save("k", "v", []byte(data)); code != http.StatusNoContent {
			t.Fatalf("save: %d", code)
		}
	}
	// A second version of the same key is listed too.
	if code := main.save("k", "v2", []byte("main2")); code != http.StatusNoContent {
		t.Fatal(code)
	}

	list := func(c client, key string) (int, []cacheEntry) {
		t.Helper()
		res := c.do("GET", "_apis/artifactcache/caches?key="+key, nil)
		var out struct {
			TotalCount     int          `json:"totalCount"`
			ArtifactCaches []cacheEntry `json:"artifactCaches"`
		}
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if out.TotalCount != len(out.ArtifactCaches) {
			t.Fatalf("totalCount %d for %d entries", out.TotalCount, len(out.ArtifactCaches))
		}
		return res.StatusCode, out.ArtifactCaches
	}
	for _, tc := range []struct {
		name string
		c    client
		key  string
		want int
	}{
		{"default branch sees both versions", main, "k", 2},
		{"feature branch sees its scope and the default branch", feat, "k", 3},
		{"another repository of the owner", web, "k", 1},
		{"another owner", evil, "k", 1},
		{"unknown key", main, "nope", 0},
		{"no key", main, "", 0},
		{"a prefix is not a match", main, "", 0},
	} {
		code, got := list(tc.c, tc.key)
		if code != http.StatusOK || len(got) != tc.want {
			t.Errorf("%s: %d, %d entries, want %d", tc.name, code, len(got), tc.want)
			continue
		}
		for _, e := range got {
			if e.CacheKey != tc.key || e.ArchiveLocation != "" || e.CreationTime == "" {
				t.Errorf("%s: unexpected entry %+v", tc.name, e)
			}
		}
	}
	// The listing is JSON even when empty (the client iterates over it).
	res := main.do("GET", "_apis/artifactcache/caches?key=nope", nil)
	var raw map[string]any
	_ = json.NewDecoder(res.Body).Decode(&raw)
	if raw["totalCount"] != float64(0) || res.Header.Get("Content-Type") != contentJSON {
		t.Fatalf("empty listing: %v %q", raw, res.Header.Get("Content-Type"))
	}
	// Listing does not count as a hit or a miss and is not an access (LRU).
	if st := s.Stats(); st.Hits != 0 || st.Misses != 0 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestReserveRejections(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api", "refs/heads/main")
	long := strings.Repeat("k", maxKeyLen+1)
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"invalid JSON", `{"key":`, http.StatusBadRequest},
		{"empty body", ``, http.StatusBadRequest},
		{"no key", `{"version":"v","cacheSize":1}`, http.StatusBadRequest},
		{"key with comma", `{"key":"a,b","version":"v","cacheSize":1}`, http.StatusBadRequest},
		{"key too long", `{"key":"` + long + `","version":"v","cacheSize":1}`, http.StatusBadRequest},
		{"no version", `{"key":"k","cacheSize":1}`, http.StatusBadRequest},
		{"version too long", `{"key":"k","version":"` + long + `","cacheSize":1}`, http.StatusBadRequest},
		{"zero size", `{"key":"k","version":"v","cacheSize":0}`, http.StatusBadRequest},
		{"negative size", `{"key":"k","version":"v","cacheSize":-5}`, http.StatusBadRequest},
		{"size is not a number", `{"key":"k","version":"v","cacheSize":"big"}`, http.StatusBadRequest},
		{"longest key accepted", `{"key":"` + strings.Repeat("k", maxKeyLen) + `","version":"v","cacheSize":1}`, http.StatusCreated},
	} {
		res := c.do("POST", "_apis/artifactcache/caches", strings.NewReader(tc.body))
		if res.StatusCode != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, res.StatusCode, tc.want)
		}
	}
	ro := c
	ro.job = roToken("refs/heads/main")
	if code, _ := ro.reserve("k", "v", 1); code != http.StatusForbidden {
		t.Fatalf("read-only job reserving: %d", code)
	}
}

// Past maxPending unfinished uploads a runner has to finish or abandon some.
func TestReserveTooManyPending(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	for i := range maxPending {
		if code, _ := c.reserve(fmt.Sprint("k", i), "v", 1); code != http.StatusCreated {
			t.Fatalf("reserve %d: %d", i, code)
		}
	}
	if code, _ := c.reserve("extra", "v", 1); code != http.StatusTooManyRequests {
		t.Fatalf("reservation over the per-runner limit: %d", code)
	}
	// The limit is per runner: another one is not affected.
	d := newClient(t, s, ts, "pitlane-b", Identity{Owner: "acme"}, "acme/api")
	if code, _ := d.reserve("extra", "v", 1); code != http.StatusCreated {
		t.Fatalf("other runner: %d", code)
	}
	// Ending the job frees the slots.
	s.Revoke("pitlane-a")
	c = newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	if code, _ := c.reserve("again", "v", 1); code != http.StatusCreated {
		t.Fatalf("after revoke: %d", code)
	}
}

// Uploads nobody finished within uploadTTL stop holding their reservation.
func TestReserveDropsExpiredUploads(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	s.SetLimits(100, 100)
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	code, id := c.reserve("old", "v", 80)
	if code != http.StatusCreated {
		t.Fatal(code)
	}
	if code, _ := c.reserve("blocked", "v", 80); code != http.StatusTooManyRequests {
		t.Fatalf("while the first upload is fresh: %d", code)
	}
	s.mu.Lock()
	for _, u := range s.pending {
		u.started = time.Now().Add(-uploadTTL - time.Minute)
	}
	s.mu.Unlock()
	if code, _ := c.reserve("blocked", "v", 80); code != http.StatusCreated {
		t.Fatalf("after the first upload expired: %d", code)
	}
	if left, _ := os.ReadDir(s.tmpDir()); len(left) != 1 {
		t.Fatalf("expired upload kept on disk: %v", left)
	}
	// The expired upload is gone for its owner too.
	if r := c.do("PATCH", "_apis/artifactcache/caches/"+id, strings.NewReader("x"), "Content-Range", "bytes 0-0/*"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("upload to an expired reservation: %d", r.StatusCode)
	}
}

func TestReserveConflicts(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api", "refs/heads/main")
	if code, _ := c.reserve("k", "v", 1); code != http.StatusCreated {
		t.Fatal(code)
	}
	if code, _ := c.reserve("k", "v", 1); code != http.StatusConflict {
		t.Fatalf("same key and version in progress: %d", code)
	}
	if code, _ := c.reserve("k", "v2", 1); code != http.StatusCreated {
		t.Fatalf("another version: %d", code)
	}
	// Same key in another scope or another repository does not conflict.
	feat := newClient(t, s, ts, "pitlane-f", Identity{Owner: "acme"}, "acme/api", "refs/heads/feat")
	if code, _ := feat.reserve("k", "v", 1); code != http.StatusCreated {
		t.Fatalf("another scope: %d", code)
	}
	web := newClient(t, s, ts, "pitlane-w", Identity{Owner: "acme"}, "acme/web", "refs/heads/main")
	if code, _ := web.reserve("k", "v", 1); code != http.StatusCreated {
		t.Fatalf("another repository: %d", code)
	}
}

// A reservation over the owner's share is refused up front, and what an
// owner has reserved does not block another owner.
func TestReserveOwnerShare(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	s.SetLimits(100, 40)
	a := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	b := newClient(t, s, ts, "pitlane-b", Identity{Owner: "beta"}, "beta/api")
	if code, _ := a.reserve("big", "v", 41); code != http.StatusBadRequest {
		t.Fatalf("over the owner's share: %d", code)
	}
	if code, _ := a.reserve("limit", "v", 40); code != http.StatusCreated {
		t.Fatalf("exactly the share: %d", code)
	}
	if code, _ := a.reserve("more", "v", 1); code != http.StatusTooManyRequests {
		t.Fatalf("share already reserved: %d", code)
	}
	if code, _ := b.reserve("k", "v", 40); code != http.StatusCreated {
		t.Fatalf("other owner: %d", code)
	}
	// 80 bytes are reserved: a third owner's 40 would exceed the limit.
	c := newClient(t, s, ts, "pitlane-c", Identity{Owner: "gamma"}, "gamma/api")
	if code, _ := c.reserve("k", "v", 21); code != http.StatusTooManyRequests {
		t.Fatalf("global reservations over the limit: %d", code)
	}
}

func TestParseRange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in         string
		start, end int64
		ok         bool
	}{
		{"bytes 0-9/*", 0, 9, true},
		{"bytes 5-5/*", 5, 5, true},
		{"bytes 10-19/100", 10, 19, true},
		{"bytes 0-9", 0, 9, true},
		{fmt.Sprintf("bytes 0-%d/*", maxChunk-1), 0, maxChunk - 1, true},
		{fmt.Sprintf("bytes 0-%d/*", maxChunk), 0, 0, false}, // one byte over
		{"", 0, 0, false},
		{"0-9/*", 0, 0, false},
		{"items 0-9/*", 0, 0, false},
		{"bytes 9/*", 0, 0, false},
		{"bytes a-9/*", 0, 0, false},
		{"bytes 0-b/*", 0, 0, false},
		{"bytes 9-0/*", 0, 0, false},
		{"bytes -1-5/*", 0, 0, false},
		{"bytes 0-/*", 0, 0, false},
		{"bytes -/*", 0, 0, false},
		{"bytes 99999999999999999999-1/*", 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			start, end, err := parseRange(tc.in)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if tc.ok && (start != tc.start || end != tc.end) {
				t.Fatalf("got %d-%d, want %d-%d", start, end, tc.start, tc.end)
			}
		})
	}
}

func TestUploadRejections(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	code, id := c.reserve("k", "v", 10)
	if code != http.StatusCreated {
		t.Fatal(code)
	}
	patch := func(c client, id, rng, body string) int {
		t.Helper()
		hdr := []string{}
		if rng != "" {
			hdr = append(hdr, "Content-Range", rng)
		}
		return c.do("PATCH", "_apis/artifactcache/caches/"+id, strings.NewReader(body), hdr...).StatusCode
	}
	for _, tc := range []struct {
		name, id, rng, body string
		want                int
	}{
		{"missing Content-Range", id, "", "x", http.StatusBadRequest},
		{"malformed Content-Range", id, "bytes x-y/*", "x", http.StatusBadRequest},
		{"inverted range", id, "bytes 5-2/*", "x", http.StatusBadRequest},
		{"last byte past the reservation", id, "bytes 0-10/*", "0123456789a", http.StatusBadRequest},
		{"range starting past the reservation", id, "bytes 10-10/*", "x", http.StatusBadRequest},
		{"body shorter than the range", id, "bytes 0-4/*", "012", http.StatusBadRequest},
		{"non-numeric id", "abc", "bytes 0-0/*", "x", http.StatusNotFound},
		{"unknown id", "1", "bytes 0-0/*", "x", http.StatusNotFound},
		{"valid", id, "bytes 0-4/*", "01234", http.StatusNoContent},
		{"body longer than the range is cut", id, "bytes 5-6/*", "56789", http.StatusNoContent},
	} {
		if got := patch(c, tc.id, tc.rng, tc.body); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	// Chunks sent for another owner's upload, even with the id, are not found.
	evil := newClient(t, s, ts, "pitlane-e", Identity{Owner: "evil"}, "evil/api")
	if got := patch(evil, id, "bytes 0-0/*", "x"); got != http.StatusNotFound {
		t.Fatalf("another owner's upload: %d", got)
	}
	// Same owner, other repository: the upload belongs to acme/api.
	web := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/web")
	if got := patch(web, id, "bytes 0-0/*", "x"); got != http.StatusNotFound {
		t.Fatalf("another repository's upload: %d", got)
	}
}

func TestCommitRejections(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	commit := func(c client, id, body string) int {
		t.Helper()
		return c.do("POST", "_apis/artifactcache/caches/"+id, strings.NewReader(body)).StatusCode
	}
	upload := func(key string, data string) string {
		t.Helper()
		code, id := c.reserve(key, "v", int64(len(data)))
		if code != http.StatusCreated {
			t.Fatal(code)
		}
		if r := c.do("PATCH", "_apis/artifactcache/caches/"+id, strings.NewReader(data), "Content-Range", fmt.Sprintf("bytes 0-%d/*", len(data)-1)); r.StatusCode != http.StatusNoContent {
			t.Fatal(r.StatusCode)
		}
		return id
	}

	if got := commit(c, "abc", `{"size":1}`); got != http.StatusNotFound {
		t.Errorf("non-numeric id: %d", got)
	}
	if got := commit(c, "42", `{"size":1}`); got != http.StatusNotFound {
		t.Errorf("unknown id: %d", got)
	}
	id := upload("a", "abcd")
	for _, tc := range []struct{ name, body string }{
		{"invalid JSON", `{`},
		{"empty body", ``},
	} {
		if got := commit(c, id, tc.body); got != http.StatusBadRequest {
			t.Errorf("%s: %d", tc.name, got)
		}
	}
	// Invalid JSON leaves the upload alive; a wrong size ends it.
	if got := commit(c, id, `{"size":5}`); got != http.StatusBadRequest {
		t.Errorf("wrong size: %d", got)
	}
	if got := commit(c, id, `{"size":4}`); got != http.StatusNotFound {
		t.Errorf("commit after a refused one: %d", got)
	}
	if left, _ := os.ReadDir(s.tmpDir()); len(left) != 0 {
		t.Errorf("refused upload left on disk: %v", left)
	}
	if got := commit(c, upload("z", "zz"), `{"size":0}`); got != http.StatusBadRequest {
		t.Errorf("zero size: %d", got)
	}
	// Another owner cannot finish somebody else's upload.
	id = upload("b", "abcd")
	evil := newClient(t, s, ts, "pitlane-e", Identity{Owner: "evil"}, "evil/api")
	if got := commit(evil, id, `{"size":4}`); got != http.StatusNotFound {
		t.Errorf("another owner committing: %d", got)
	}
	// The unfinished upload of a revoked runner cannot be committed.
	s.Revoke("pitlane-a")
	if got := commit(c, id, `{"size":4}`); got != http.StatusUnauthorized {
		t.Errorf("revoked token: %d", got)
	}
	if st := s.Stats(); st.Entries != 0 || st.Saves != 0 {
		t.Errorf("stats: %+v", st)
	}
}

// If the archive cannot be stored the client gets a 500 and nothing is left.
func TestCommitStorageFailure(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	code, id := c.reserve("k", "v", 3)
	if code != http.StatusCreated {
		t.Fatal(code)
	}
	if r := c.do("PATCH", "_apis/artifactcache/caches/"+id, strings.NewReader("abc"), "Content-Range", "bytes 0-2/*"); r.StatusCode != http.StatusNoContent {
		t.Fatal(r.StatusCode)
	}
	if err := os.RemoveAll(s.entryDir()); err != nil { // the rename target is gone
		t.Fatal(err)
	}
	if r := c.do("POST", "_apis/artifactcache/caches/"+id, strings.NewReader(`{"size":3}`)); r.StatusCode != http.StatusInternalServerError {
		t.Fatalf("commit: %d", r.StatusCode)
	}
	if st := s.Stats(); st.Entries != 0 || st.Bytes != 0 || st.Saves != 0 {
		t.Fatalf("stats: %+v", st)
	}
	if left, _ := os.ReadDir(s.tmpDir()); len(left) != 0 {
		t.Fatalf("upload left on disk: %v", left)
	}
}

// If the temporary directory is unusable the reservation fails cleanly.
func TestReserveStorageFailure(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	if err := os.RemoveAll(s.tmpDir()); err != nil {
		t.Fatal(err)
	}
	if code, _ := c.reserve("k", "v", 3); code != http.StatusInternalServerError {
		t.Fatalf("reserve: %d", code)
	}
	if code, _ := c.reserve("k", "v", 3); code != http.StatusInternalServerError {
		t.Fatalf("a failed reservation must not leave a pending upload (conflict): %d", code)
	}
}

func TestDownload(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	if code := c.save("k", "v", []byte("0123456789")); code != http.StatusNoContent {
		t.Fatal(code)
	}
	var id string
	for k := range s.entries {
		id = fmt.Sprint(k)
	}
	for _, tc := range []struct {
		name, id string
		want     int
	}{
		{"non-numeric", "abc", http.StatusNotFound},
		{"unknown", "1", http.StatusNotFound},
		{"valid", id, http.StatusOK},
	} {
		if res := c.do("GET", "_apis/artifactcache/artifacts/"+tc.id, nil); res.StatusCode != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, res.StatusCode, tc.want)
		}
	}
	// The client downloads in ranges.
	res := c.do("GET", "_apis/artifactcache/artifacts/"+id, nil, "Range", "bytes=2-4")
	var got bytes.Buffer
	_, _ = got.ReadFrom(res.Body)
	if res.StatusCode != http.StatusPartialContent || got.String() != "234" {
		t.Fatalf("range: %d %q", res.StatusCode, got.String())
	}
	// Another repository of the same owner cannot read it by id.
	web := newClient(t, s, ts, "pitlane-w", Identity{Owner: "acme"}, "acme/web")
	if res := web.do("GET", "_apis/artifactcache/artifacts/"+id, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("another repository: %d", res.StatusCode)
	}
	// An archive that disappeared from disk (pruned) is a 404, not a 500.
	for k := range s.entries {
		_ = os.Remove(s.blobPath(k))
	}
	if res := c.do("GET", "_apis/artifactcache/artifacts/"+id, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing archive: %d", res.StatusCode)
	}
}

func TestLookupArchiveLocation(t *testing.T) {
	t.Parallel()
	s, ts := newServer(t, "")
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	if code := c.save("k", "v", []byte("x")); code != http.StatusNoContent {
		t.Fatal(code)
	}
	res := c.do("GET", "_apis/artifactcache/cache?keys=k&version=v", nil)
	var e cacheEntry
	if err := json.NewDecoder(res.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	want := ts.URL + "/" + strings.TrimPrefix(c.base, ts.URL+"/") + "_apis/artifactcache/artifacts/"
	if !strings.HasPrefix(e.ArchiveLocation, want) {
		t.Fatalf("archive location %q does not start with %q", e.ArchiveLocation, want)
	}
	if e.CacheKey != "k" || e.CacheVersion != "v" {
		t.Fatalf("entry: %+v", e)
	}
	if _, err := time.Parse(time.RFC3339, e.CreationTime); err != nil {
		t.Fatalf("creation time %q: %v", e.CreationTime, err)
	}
}

func TestBaseURL(t *testing.T) {
	t.Parallel()
	plain := httptest.NewRequest("GET", "http://cache.example:8080/x", nil)
	if got := baseURL(plain); got != "http://cache.example:8080" {
		t.Fatalf("plain: %q", got)
	}
	secure := httptest.NewRequest("GET", "https://cache.example/x", nil)
	secure.TLS = &tls.ConnectionState{}
	if got := baseURL(secure); got != "https://cache.example" {
		t.Fatalf("TLS: %q", got)
	}
}

func TestNamespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		id          Identity
		owner, repo string
		want        string
		ok          bool
	}{
		{"same owner", Identity{Owner: "acme"}, "acme", "api", "acme/api", true},
		{"lower-cased", Identity{Owner: "Acme"}, "ACME", "Api", "acme/api", true},
		{"repository target", Identity{Owner: "acme", Repo: "api"}, "acme", "API", "acme/api", true},
		{"dots, dashes and underscores", Identity{Owner: "a"}, "a", "my_repo-1.x", "a/my_repo-1.x", true},
		{"other owner", Identity{Owner: "acme"}, "evil", "api", "", false},
		{"owner prefix", Identity{Owner: "acme"}, "acme2", "api", "", false},
		{"other repository", Identity{Owner: "acme", Repo: "web"}, "acme", "api", "", false},
		{"no owner in the identity", Identity{}, "", "api", "", false},
		{"empty owner", Identity{Owner: "acme"}, "", "api", "", false},
		{"empty repo", Identity{Owner: "acme"}, "acme", "", "", false},
		{"dot repo", Identity{Owner: "acme"}, "acme", ".", "", false},
		{"dot dot repo", Identity{Owner: "acme"}, "acme", "..", "", false},
		{"slash in repo", Identity{Owner: "acme"}, "acme", "a/b", "", false},
		{"space in repo", Identity{Owner: "acme"}, "acme", "a b", "", false},
		{"repo too long", Identity{Owner: "acme"}, "acme", strings.Repeat("r", 101), "", false},
		{"longest repo", Identity{Owner: "acme"}, "acme", strings.Repeat("r", 100), "acme/" + strings.Repeat("r", 100), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := namespace(tc.id, tc.owner, tc.repo)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("namespace(%+v, %q, %q) = %q, %v; want %q, %v", tc.id, tc.owner, tc.repo, got, ok, tc.want, tc.ok)
			}
		})
	}
}
