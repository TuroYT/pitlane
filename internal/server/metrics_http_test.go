package server

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/cache"
	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

func TestReadyzEndpoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		pingErr  error
		slots    []pool.SlotState
		wantCode int
		wantBody string
	}{
		{"ready", nil, []pool.SlotState{{Status: "busy"}}, http.StatusOK, "ready\n"},
		{"docker down", errors.New("dial unix /var/run/docker.sock"), []pool.SlotState{{Status: "ready"}}, http.StatusServiceUnavailable, "not ready: docker unreachable: dial unix"},
		{"no usable slot", nil, []pool.SlotState{{Status: "error"}}, http.StatusServiceUnavailable, "not ready: no slot is ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, fp, h := apiServer(t)
			fp.pingErr = tc.pingErr
			fp.setSlots(tc.slots)
			w := get(h, "/readyz", "") // public: no session
			if w.Code != tc.wantCode || !strings.HasPrefix(w.Body.String(), tc.wantBody) {
				t.Fatalf("got %d %q, want %d %q", w.Code, w.Body.String(), tc.wantCode, tc.wantBody)
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestMetricsEndpoint(t *testing.T) {
	t.Parallel()
	s, st, fp, h := apiServer(t)
	fp.setSlots([]pool.SlotState{{Status: "ready"}, {Status: "busy"}})
	fp.wipes = map[string]int64{pool.WipeStrict: 3}
	fp.setMem(pool.MemoryState{Enabled: true, Budget: 8 << 30, Predicted: 2 << 30, Committed: 3 << 30})
	fp.diskTotal, fp.diskFree, fp.diskErr = 100<<30, 40<<30, nil
	s.cfg.Version = "v9.9"
	now := time.Now().Unix()
	if err := st.UpsertJob(store.Job{TargetID: 1, JobID: "j1", RunID: 1, Owner: "acme", Repo: "api", RunnerName: "pitlane-a",
		QueuedAt: now - 60, StartedAt: now - 50, FinishedAt: now - 10, Result: "succeeded"}); err != nil {
		t.Fatal(err)
	}

	w := get(h, "/metrics", "alice") // signed in, no token configured
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain; version=0.0.4") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	for _, want := range []string{
		`pitlane_info{version="v9.9"} 1`,
		"pitlane_docker_up 1",
		`pitlane_slots{status="busy"} 1`,
		`pitlane_slot_wipes_total{reason="strict"} 3`,
		"pitlane_memory_budget_bytes 8589934592",
		`pitlane_jobs_completed{result="succeeded"} 1`,
		"pitlane_job_duration_seconds_count 1",
		"pitlane_job_wait_seconds_count 1",
		"pitlane_disk_free_bytes 42949672960",
	} {
		if !bodyHas(w, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, w.Body)
		}
	}
	if bodyHas(w, "pitlane_actions_cache_bytes") {
		t.Error("actions cache reported although it is not serving")
	}

	// Docker down and the actions cache serving.
	fp.pingErr = errors.New("down")
	fp.diskErr = errors.New("no disk")
	s.SetActionsCache(&cache.Server{})
	w = get(h, "/metrics", "vera")
	if !bodyHas(w, "pitlane_docker_up 0\n") || bodyHas(w, "pitlane_disk_free_bytes") || !bodyHas(w, "pitlane_actions_cache_bytes 0\n") {
		t.Errorf("docker down:\n%s", w.Body)
	}
}

func TestMetricsAuthorization(t *testing.T) {
	t.Parallel()
	const token = "0123456789abcdef-token"
	_, st, _, h := apiServer(t)
	req := func(remote string, header map[string]string, who string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/metrics", nil)
		r.RemoteAddr = remote
		for k, v := range header {
			r.Header.Set(k, v)
		}
		if who != "" {
			r.AddCookie(session(who))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// No token: sessions and loopback.
	for _, tc := range []struct {
		name, remote, who string
		hdr               map[string]string
		want              int
	}{
		{"remote anonymous", "203.0.113.5:1", "", nil, http.StatusUnauthorized},
		{"remote with a session", "203.0.113.5:1", "vera", nil, http.StatusOK},
		{"loopback", "127.0.0.1:1", "", nil, http.StatusOK},
		{"loopback behind a proxy", "127.0.0.1:1", "", map[string]string{"X-Real-Ip": "203.0.113.5"}, http.StatusUnauthorized},
		{"loopback with Forwarded", "127.0.0.1:1", "", map[string]string{"Forwarded": "for=203.0.113.5"}, http.StatusUnauthorized},
	} {
		if w := req(tc.remote, tc.hdr, tc.who); w.Code != tc.want {
			t.Errorf("no token, %s: %d, want %d", tc.name, w.Code, tc.want)
		}
	}
	if w := req("203.0.113.5:1", nil, ""); w.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 without WWW-Authenticate")
	}

	// With a token only the bearer token works: not a session, not loopback.
	if err := st.SetSecret(metricsTokenKey, token); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		hdr  map[string]string
		who  string
		want int
	}{
		{"no credentials", nil, "", http.StatusUnauthorized},
		{"session only", nil, "alice", http.StatusUnauthorized},
		{"right token", map[string]string{"Authorization": "Bearer " + token}, "", http.StatusOK},
		{"right token, spaces around", map[string]string{"Authorization": "Bearer  " + token + " "}, "", http.StatusOK},
		{"wrong token", map[string]string{"Authorization": "Bearer " + token + "x"}, "", http.StatusUnauthorized},
		{"prefix of the token", map[string]string{"Authorization": "Bearer " + token[:10]}, "", http.StatusUnauthorized},
		{"token without Bearer", map[string]string{"Authorization": token}, "", http.StatusUnauthorized},
		{"lower case scheme", map[string]string{"Authorization": "bearer " + token}, "", http.StatusOK},
		{"upper case scheme", map[string]string{"Authorization": "BEARER " + token}, "", http.StatusOK},
		{"lower case scheme, wrong token", map[string]string{"Authorization": "bearer " + token + "x"}, "", http.StatusUnauthorized},
		{"scheme with no token", map[string]string{"Authorization": "Bearer"}, "", http.StatusUnauthorized},
		{"basic scheme", map[string]string{"Authorization": "Basic " + token}, "", http.StatusUnauthorized},
	} {
		if w := req("127.0.0.1:1", tc.hdr, tc.who); w.Code != tc.want {
			t.Errorf("token set, %s: %d, want %d", tc.name, w.Code, tc.want)
		}
	}
	// A token that cannot be read fails closed.
	if err := st.Set(metricsTokenKey, "enc:"+base64.StdEncoding.EncodeToString(make([]byte, 64))); err != nil {
		t.Fatal(err)
	}
	if w := req("127.0.0.1:1", map[string]string{"Authorization": "Bearer anything"}, "alice"); w.Code != http.StatusUnauthorized {
		t.Errorf("unreadable token: %d, want 401", w.Code)
	}
}
