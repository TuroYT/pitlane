package cache

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func newServer(t *testing.T, dir string) (*Server, *httptest.Server) {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	s, err := Open(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// jobToken builds an (unsigned) job token carrying branch scopes, like
// ACTIONS_RUNTIME_TOKEN.
func jobToken(scopes ...string) string {
	type ac struct {
		Scope      string
		Permission int
	}
	var list []ac
	for i, s := range scopes {
		p := 1
		if i == 0 {
			p = 3
		}
		list = append(list, ac{s, p})
	}
	b, _ := json.Marshal(list)
	payload, _ := json.Marshal(map[string]string{"ac": string(b)})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

type client struct {
	t    *testing.T
	base string // .../<token>/<owner>/<repo>/
	job  string
}

func (c client) do(method, path string, body io.Reader, hdr ...string) *http.Response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.job != "" {
		req.Header.Set("Authorization", "Bearer "+c.job)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func (c client) save(key, version string, data []byte) int {
	c.t.Helper()
	body, _ := json.Marshal(map[string]any{"key": key, "version": version, "cacheSize": len(data)})
	res := c.do("POST", "_apis/artifactcache/caches", bytes.NewReader(body))
	if res.StatusCode != http.StatusCreated {
		return res.StatusCode
	}
	var out struct{ CacheID int64 }
	_ = json.NewDecoder(res.Body).Decode(&out)
	id := fmt.Sprint(out.CacheID)
	// Two chunks, last one first, as the parallel client may do.
	half := len(data) / 2
	for _, r := range [][2]int{{half, len(data)}, {0, half}} {
		if r[0] == r[1] {
			continue
		}
		res := c.do("PATCH", "_apis/artifactcache/caches/"+id, bytes.NewReader(data[r[0]:r[1]]),
			"Content-Range", fmt.Sprintf("bytes %d-%d/*", r[0], r[1]-1), "Content-Type", "application/octet-stream")
		if res.StatusCode != http.StatusNoContent {
			c.t.Fatalf("upload chunk: %d", res.StatusCode)
		}
	}
	res = c.do("POST", "_apis/artifactcache/caches/"+id, strings.NewReader(fmt.Sprintf(`{"size":%d}`, len(data))))
	return res.StatusCode
}

// restore returns the restored key and content ("" when missing).
func (c client) restore(version string, keys ...string) (string, string) {
	c.t.Helper()
	res := c.do("GET", "_apis/artifactcache/cache?keys="+strings.Join(keys, ",")+"&version="+version, nil)
	if res.StatusCode == http.StatusNoContent {
		return "", ""
	}
	if res.StatusCode != http.StatusOK {
		c.t.Fatalf("lookup: %d", res.StatusCode)
	}
	var e cacheEntry
	_ = json.NewDecoder(res.Body).Decode(&e)
	dl, err := http.Get(e.ArchiveLocation) // no Authorization header, like the client
	if err != nil {
		c.t.Fatal(err)
	}
	defer dl.Body.Close()
	b, _ := io.ReadAll(dl.Body)
	if dl.StatusCode != http.StatusOK {
		c.t.Fatalf("download: %d", dl.StatusCode)
	}
	return e.CacheKey, string(b)
}

func newClient(t *testing.T, s *Server, ts *httptest.Server, runner string, id Identity, repo string, scopes ...string) client {
	tok := s.Register(runner, id)
	c := client{t: t, base: ts.URL + "/" + tok + "/" + repo + "/"}
	if len(scopes) > 0 {
		c.job = jobToken(scopes...)
	}
	return c
}

func TestSaveRestore(t *testing.T) {
	s, ts := newServer(t, "")
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api", "refs/heads/main")
	if code := c.save("npm-linux-abc", "v1", []byte("hello world")); code != http.StatusNoContent {
		t.Fatalf("save: %d", code)
	}
	if k, data := c.restore("v1", "npm-linux-abc"); k != "npm-linux-abc" || data != "hello world" {
		t.Fatalf("restore: %q %q", k, data)
	}
	if k, _ := c.restore("v2", "npm-linux-abc"); k != "" {
		t.Fatal("another version restored")
	}
	if code := c.save("npm-linux-abc", "v1", []byte("again")); code != http.StatusConflict {
		t.Fatalf("second save of an existing key: %d", code)
	}
	if st := s.Stats(); st.Entries != 1 || st.Bytes != 11 || st.Hits != 1 || st.Misses != 1 || st.Saves != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestMatching(t *testing.T) {
	s, ts := newServer(t, "")
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api", "refs/heads/main")
	for _, k := range []string{"deps-linux-1", "deps-linux-2", "deps-mac-1"} {
		if code := c.save(k, "v", []byte(k)); code != http.StatusNoContent {
			t.Fatal(code)
		}
		time.Sleep(2 * time.Millisecond) // distinct creation times
	}
	cases := []struct {
		keys []string
		want string
	}{
		{[]string{"deps-linux-1"}, "deps-linux-1"},                 // exact
		{[]string{"deps-linux-"}, "deps-linux-2"},                  // newest prefix
		{[]string{"deps-linux-9", "deps-mac-"}, "deps-mac-1"},      // restore key
		{[]string{"deps-linux-9", "deps-"}, "deps-mac-1"},          // newest of all
		{[]string{"deps-linux-1", "deps-mac-"}, "deps-linux-1"},    // primary key wins
		{[]string{"deps-l", "deps-linux-1"}, "deps-linux-2"},       // keys in order
		{[]string{"nothing", "other"}, ""},                         // miss
		{[]string{"Deps-linux-1"}, ""},                             // case sensitive
		{[]string{"deps-windows", "deps-linux-2x", "x"}, ""},       // no partial suffix
		{[]string{"deps-linux-2", "deps-linux-1"}, "deps-linux-2"}, // exact before
	}
	for _, tc := range cases {
		if k, _ := c.restore("v", tc.keys...); k != tc.want {
			t.Errorf("%v: got %q, want %q", tc.keys, k, tc.want)
		}
	}
}

func TestBranchScopes(t *testing.T) {
	s, ts := newServer(t, "")
	id := Identity{Owner: "acme"}
	main := newClient(t, s, ts, "pitlane-main", id, "acme/api", "refs/heads/main")
	feat := newClient(t, s, ts, "pitlane-feat", id, "acme/api", "refs/heads/feat", "refs/heads/main")
	if main.save("k-main", "v", []byte("main")) != http.StatusNoContent || feat.save("k-feat", "v", []byte("feat")) != http.StatusNoContent {
		t.Fatal("save")
	}
	if k, _ := feat.restore("v", "k-"); k != "k-feat" {
		t.Fatalf("feature branch should see its own entry first, got %q", k)
	}
	if k, _ := feat.restore("v", "k-main"); k != "k-main" {
		t.Fatalf("feature branch should fall back to the default branch, got %q", k)
	}
	if k, _ := main.restore("v", "k-feat"); k != "" {
		t.Fatal("the default branch restored a feature branch entry")
	}
	// The same key on two branches are two entries.
	if feat.save("k-main", "v", []byte("feat2")) != http.StatusNoContent {
		t.Fatal("same key on another branch")
	}
	if _, data := feat.restore("v", "k-main"); data != "feat2" {
		t.Fatalf("own branch first: %q", data)
	}
	// A read-only token cannot save.
	ro := main
	b, _ := json.Marshal([]map[string]any{{"Scope": "refs/heads/main", "Permission": 1}})
	payload, _ := json.Marshal(map[string]string{"ac": string(b)})
	ro.job = "x." + base64.RawURLEncoding.EncodeToString(payload) + ".y"
	if code := ro.save("ro", "v", []byte("x")); code != http.StatusForbidden {
		t.Fatalf("read-only save: %d", code)
	}
	if k, _ := ro.restore("v", "k-main"); k != "k-main" {
		t.Fatal("read-only restore")
	}
}

func TestOwnerIsolation(t *testing.T) {
	s, ts := newServer(t, "")
	acme := newClient(t, s, ts, "pitlane-acme", Identity{Owner: "acme"}, "acme/api")
	if acme.save("secret", "v", []byte("acme data")) != http.StatusNoContent {
		t.Fatal("save")
	}
	evil := newClient(t, s, ts, "pitlane-evil", Identity{Owner: "evil"}, "evil/x")
	if k, _ := evil.restore("v", "secret"); k != "" {
		t.Fatal("another owner restored the entry")
	}
	// Claiming the other owner's repository in the URL is refused.
	evilTok := s.Register("pitlane-evil", Identity{Owner: "evil"})
	for _, repo := range []string{"acme/api", "ACME/api", "acme/../acme/api", "evil/..", "evil/a%2Fb"} {
		res, err := http.Get(ts.URL + "/" + evilTok + "/" + repo + "/_apis/artifactcache/cache?keys=secret&version=v")
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusNoContent {
			t.Errorf("%s: %d", repo, res.StatusCode)
		}
	}
	// The download URL is useless with another runner's token.
	res := acme.do("GET", "_apis/artifactcache/cache?keys=secret&version=v", nil)
	var e cacheEntry
	_ = json.NewDecoder(res.Body).Decode(&e)
	acmeTok := strings.Split(strings.TrimPrefix(acme.base, ts.URL+"/"), "/")[0]
	stolen := strings.Replace(e.ArchiveLocation, acmeTok, evilTok, 1)
	stolen = strings.Replace(stolen, "/acme/api/", "/evil/x/", 1)
	if r, _ := http.Get(stolen); r.StatusCode != http.StatusNotFound {
		t.Fatalf("download with another owner's token: %d", r.StatusCode)
	}
	// Repository targets only reach their own repository.
	repoTok := s.Register("pitlane-repo", Identity{Owner: "acme", Repo: "web"})
	if r, _ := http.Get(ts.URL + "/" + repoTok + "/acme/api/_apis/artifactcache/cache?keys=secret&version=v"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("repository target reading another repository: %d", r.StatusCode)
	}
	// Repositories of one owner do not see each other's entries (GitHub semantics).
	other := newClient(t, s, ts, "pitlane-acme2", Identity{Owner: "acme"}, "acme/web")
	if k, _ := other.restore("v", "secret"); k != "" {
		t.Fatal("another repository restored the entry")
	}
}

func TestTokens(t *testing.T) {
	s, ts := newServer(t, "")
	tok := s.Register("pitlane-a", Identity{Owner: "acme"})
	get := func(tok string) int {
		res, err := http.Get(ts.URL + "/" + tok + "/acme/api/_apis/artifactcache/cache?keys=k&version=v")
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	if code := get(tok); code != http.StatusNoContent {
		t.Fatalf("valid token: %d", code)
	}
	for _, bad := range []string{"pitlane-a", "pitlane-a.", "pitlane-b." + strings.Split(tok, ".")[1], tok + "x", "." + tok} {
		if code := get(bad); code != http.StatusUnauthorized {
			t.Errorf("%q: %d", bad, code)
		}
	}
	// Re-registering (adoption after a restart) gives the same token.
	if s.Register("pitlane-a", Identity{Owner: "acme"}) != tok {
		t.Fatal("token changed")
	}
	s.Revoke("pitlane-a")
	if code := get(tok); code != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d", code)
	}
	// Tokens survive a restart (same key), but only for registered runners.
	s2, ts2 := newServer(t, s.dir)
	ts = ts2
	if code := get(tok); code != http.StatusUnauthorized {
		t.Fatalf("unregistered after restart: %d", code)
	}
	if s2.Register("pitlane-a", Identity{Owner: "acme"}) != tok {
		t.Fatal("token changed across restarts")
	}
}

func TestRevokeDropsUploads(t *testing.T) {
	s, ts := newServer(t, "")
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	res := c.do("POST", "_apis/artifactcache/caches", strings.NewReader(`{"key":"k","version":"v","cacheSize":3}`))
	if res.StatusCode != http.StatusCreated {
		t.Fatal(res.StatusCode)
	}
	// Another job cannot save the same key meanwhile.
	d := newClient(t, s, ts, "pitlane-b", Identity{Owner: "acme"}, "acme/api")
	if code := d.save("k", "v", []byte("abc")); code != http.StatusConflict {
		t.Fatalf("concurrent reserve: %d", code)
	}
	s.Revoke("pitlane-a")
	if code := d.save("k", "v", []byte("abc")); code != http.StatusNoContent {
		t.Fatalf("reserve after the first job ended: %d", code)
	}
	if left, _ := os.ReadDir(s.tmpDir()); len(left) != 0 {
		t.Fatalf("unfinished upload left on disk: %v", left)
	}
}

func TestUploadChecks(t *testing.T) {
	s, ts := newServer(t, "")
	s.SetLimit(100)
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	if code := c.save("big", "v", make([]byte, 101)); code != http.StatusBadRequest {
		t.Fatalf("over the limit: %d", code)
	}
	res := c.do("POST", "_apis/artifactcache/caches", strings.NewReader(`{"key":"k","version":"v","cacheSize":4}`))
	var out struct{ CacheID int64 }
	_ = json.NewDecoder(res.Body).Decode(&out)
	id := fmt.Sprint(out.CacheID)
	if r := c.do("PATCH", "_apis/artifactcache/caches/"+id, strings.NewReader("abcdef"), "Content-Range", "bytes 0-5/*"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("chunk beyond the reserved size: %d", r.StatusCode)
	}
	if r := c.do("PATCH", "_apis/artifactcache/caches/"+id, strings.NewReader("ab"), "Content-Range", "bytes 0-3/*"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("short chunk: %d", r.StatusCode)
	}
	other := newClient(t, s, ts, "pitlane-b", Identity{Owner: "acme"}, "acme/api")
	if r := other.do("PATCH", "_apis/artifactcache/caches/"+id, strings.NewReader("abcd"), "Content-Range", "bytes 0-3/*"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("another runner's upload: %d", r.StatusCode)
	}
	if r := c.do("POST", "_apis/artifactcache/caches/"+id, strings.NewReader(`{"size":4}`)); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("commit with a wrong size: %d", r.StatusCode)
	}
	if st := s.Stats(); st.Entries != 0 {
		t.Fatalf("incomplete entry committed: %+v", st)
	}
}

func TestEvictionLRU(t *testing.T) {
	dir := t.TempDir()
	s, ts := newServer(t, dir)
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	for _, k := range []string{"a", "b", "c"} {
		if c.save(k, "v", make([]byte, 40)) != http.StatusNoContent {
			t.Fatal("save")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if k, _ := c.restore("v", "a"); k != "a" { // a is now the most recently used
		t.Fatal("restore a")
	}
	s.SetLimit(100) // 120 bytes stored: b goes
	if k, _ := c.restore("v", "b"); k != "" {
		t.Fatal("b should have been evicted")
	}
	if c.save("d", "v", make([]byte, 40)) != http.StatusNoContent { // over again: c goes
		t.Fatal("save d")
	}
	if k, _ := c.restore("v", "c"); k != "" {
		t.Fatal("c should have been evicted")
	}
	if st := s.Stats(); st.Entries != 2 || st.Bytes != 80 || st.Evictions != 2 {
		t.Fatalf("stats: %+v", st)
	}
	// The disk pruning frees at least what it asks for.
	if freed := s.Evict(1); freed != 40 {
		t.Fatalf("evict: %d", freed)
	}
	// The index survives a restart, last-use order included.
	s2, ts2 := newServer(t, dir)
	c2 := newClient(t, s2, ts2, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	if st := s2.Stats(); st.Entries != 1 || st.Bytes != 40 {
		t.Fatalf("after restart: %+v", st)
	}
	if k, _ := c2.restore("v", "a", "d"); k == "" {
		t.Fatal("entry lost across restart")
	}
}

func TestLoadDropsBrokenEntries(t *testing.T) {
	dir := t.TempDir()
	s, ts := newServer(t, dir)
	c := newClient(t, s, ts, "pitlane-a", Identity{Owner: "acme"}, "acme/api")
	if c.save("a", "v", []byte("1234")) != http.StatusNoContent || c.save("b", "v", []byte("5678")) != http.StatusNoContent {
		t.Fatal("save")
	}
	var ids []int64
	for id := range s.entries {
		ids = append(ids, id)
	}
	if err := os.WriteFile(s.blobPath(ids[0]), []byte("truncated-or-longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(s.entryDir()+"/junk.txt", nil, 0o600)
	s2, _ := newServer(t, dir)
	if st := s2.Stats(); st.Entries != 1 || st.Bytes != 4 {
		t.Fatalf("after reload: %+v", st)
	}
	if _, err := os.Stat(s.blobPath(ids[0])); !os.IsNotExist(err) {
		t.Fatal("broken archive kept")
	}
}

func TestParallelUploads(t *testing.T) {
	s, ts := newServer(t, "")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			c := newClient(t, s, ts, fmt.Sprintf("pitlane-%d", i), Identity{Owner: "acme"}, "acme/api")
			data := bytes.Repeat([]byte{byte(i)}, 1000+i)
			if code := c.save(fmt.Sprintf("k%d", i), "v", data); code != http.StatusNoContent {
				t.Errorf("save %d: %d", i, code)
			}
			if _, got := c.restore("v", fmt.Sprintf("k%d", i)); got != string(data) {
				t.Errorf("restore %d", i)
			}
		})
	}
	wg.Wait()
}

func TestScopes(t *testing.T) {
	read, write, ok := scopes("Bearer " + jobToken("refs/pull/1/merge", "refs/heads/main"))
	if !ok || write != "refs/pull/1/merge" || strings.Join(read, ",") != "refs/pull/1/merge,refs/heads/main" {
		t.Fatalf("%v %q %v", read, write, ok)
	}
	for _, h := range []string{"", "Bearer", "Bearer a.b", "Bearer a.!!!.c", "Basic x"} {
		if read, write, ok := scopes(h); !ok || write != "" || len(read) != 1 || read[0] != "" {
			t.Errorf("%q: %v %q %v", h, read, write, ok)
		}
	}
}
