package pool

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

func TestRunnerNames(t *testing.T) {
	p := newTestPool("owner")
	var filter string
	newDockerAPI(t, p, map[string]http.HandlerFunc{"GET /containers/json": func(w http.ResponseWriter, r *http.Request) {
		filter = r.URL.Query().Get("filters")
		reply([]map[string]any{
			{"Id": "a", "Names": []string{"/pitlane-a"}, "Labels": map[string]string{LabelRunner: "pitlane-a"}},
			{"Id": "b", "Names": []string{"/"}, "Labels": map[string]string{LabelRunner: "pitlane-b"}},
		})(w, r)
	}})
	got, err := p.RunnerNames(context.Background())
	if err != nil || len(got) != 2 || !got["pitlane-a"] || !got["pitlane-b"] {
		t.Fatalf("names %v %v", got, err)
	}
	if !strings.Contains(filter, LabelRole+"=runner") || !strings.Contains(filter, LabelManaged+"=true") {
		t.Fatalf("only Pitlane's runner containers may be listed, filter %q", filter)
	}
	newDockerAPI(t, p, map[string]http.HandlerFunc{"GET /containers/json": replyStatus(http.StatusInternalServerError)})
	if _, err := p.RunnerNames(context.Background()); err == nil {
		t.Fatal("a failed listing must be reported: an empty answer would make every runner look orphaned")
	}
}

func TestRunnerContainersAdoptsSlots(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 0, Status: statusReady}, SlotState{Index: 1, Status: statusReady})
	newDockerAPI(t, p, map[string]http.HandlerFunc{"GET /containers/json": reply([]map[string]any{
		{"Id": "a", "State": "running", "Created": 100, "Labels": map[string]string{LabelSlot: "1", LabelTarget: "7", LabelOwner: "acme", LabelRunner: "pitlane-a", LabelStrict: "false"}},
		{"Id": "b", "State": "exited", "Created": 200, "Labels": map[string]string{LabelRunner: "pitlane-b"}},
	})})
	got, err := p.RunnerContainers(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("orphans %+v %v", got, err)
	}
	if o := got[0]; o.ID != "a" || o.Slot != 1 || o.TargetID != 7 || !o.Running || o.Strict || o.Created.Unix() != 100 {
		t.Fatalf("first orphan %+v", o)
	}
	if o := got[1]; !o.Untrusted || o.Slot != -1 || o.Running {
		t.Fatalf("a container with no labels is untrusted and in no slot: %+v", o)
	}
	if p.slots[1].Status != statusBusy || p.slots[0].Status != statusReady {
		t.Fatalf("only the adopted runner's slot is marked busy: %+v %+v", p.slots[0].SlotState, p.slots[1].SlotState)
	}
	newDockerAPI(t, p, map[string]http.HandlerFunc{"GET /containers/json": replyStatus(http.StatusInternalServerError)})
	if _, err := p.RunnerContainers(context.Background()); err == nil {
		t.Fatal("a failed listing must be reported")
	}
}

// adoptContainer creates the slot of a runner whose slot is not in the table
// yet (the slot count was lowered since).
func TestAdoptContainerCreatesMissingSlot(t *testing.T) {
	p := newTestPool("owner")
	p.now = func() time.Time { return time.Unix(500, 0) }
	p.adoptContainer("a", map[string]string{LabelSlot: "9", LabelTarget: "3", LabelOwner: "acme", LabelRunner: "pitlane-a", LabelStrict: "false"}, "running", 0)
	s := p.slots[9]
	if s == nil || s.Status != statusBusy || s.Owner != "acme" || s.TargetID != 3 || s.Runner != "pitlane-a" || s.Since != 500 {
		t.Fatalf("adopted slot %+v", s)
	}
	p.cfg.Slots = 2
	if got := p.Slots(); len(got) != 1 || got[0].Index != 9 {
		t.Fatalf("the adopted slot must show in the list: %+v", got)
	}
}

// muxFrames encodes the stdout and stderr texts in Docker's multiplexed log
// format.
func muxFrames(stdout, stderr string) []byte {
	var b bytes.Buffer
	for _, f := range []struct {
		stream byte
		text   string
	}{{1, stdout}, {2, stderr}} {
		hdr := make([]byte, 8)
		hdr[0] = f.stream
		binary.BigEndian.PutUint32(hdr[4:], uint32(len(f.text)))
		b.Write(hdr)
		b.WriteString(f.text)
	}
	return b.Bytes()
}

func TestLogs(t *testing.T) {
	p := newTestPool("owner")
	var follow string
	newDockerAPI(t, p, map[string]http.HandlerFunc{
		"GET /containers/c1/logs": func(w http.ResponseWriter, r *http.Request) {
			follow = r.URL.Query().Get("follow")
			_, _ = w.Write(muxFrames("out\n", "err\n"))
		},
		"GET /containers/gone/logs": replyStatus(http.StatusNotFound),
	})
	var w bytes.Buffer
	if err := p.Logs(context.Background(), "c1", true, &w); err != nil || w.String() != "out\nerr\n" {
		t.Fatalf("logs %q %v", w.String(), err)
	}
	if follow != "1" && follow != "true" {
		t.Fatalf("follow=%q", follow)
	}
	if err := p.Logs(context.Background(), "gone", false, &w); err == nil {
		t.Fatal("logs of a missing container must be an error")
	}
}

func TestStopRunner(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"stopped", http.StatusNoContent, false},
		{"already stopped", http.StatusNotModified, false},
		{"gone", http.StatusNotFound, false},
		{"being removed", http.StatusConflict, false},
		{"daemon error", http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPool("owner")
			var query string
			newDockerAPI(t, p, map[string]http.HandlerFunc{"POST /containers/c1/stop": func(w http.ResponseWriter, r *http.Request) {
				query = r.URL.RawQuery
				replyStatus(tc.status)(w, r)
			}})
			err := p.StopRunner(context.Background(), "c1", 30*time.Second)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err %v", err)
			}
			if !strings.Contains(query, "signal=SIGTERM") || !strings.Contains(query, "t=30") {
				t.Fatalf("SIGTERM first, then SIGKILL after the grace period: %q", query)
			}
		})
	}
}

// A runner whose cache volumes cannot be prepared is not started; one whose
// slot cannot reach the host gets no cache URL but still runs.
func TestStartRunnerVolumeAndCacheEnv(t *testing.T) {
	spec := RunnerSpec{Name: "pitlane-a", JITConfig: "jit", TargetID: 1, Owner: "acme", Repo: "acme/app", Lease: Lease{Slot: 0}}
	t.Run("volume failure", func(t *testing.T) {
		p := newTestPool("owner", SlotState{Index: 0, Status: statusBusy})
		api := newDockerAPI(t, p, map[string]http.HandlerFunc{
			"GET /volumes/pitlane-cache-acme": replyStatus(http.StatusNotFound),
			"POST /volumes/create":            replyStatus(http.StatusInternalServerError),
		})
		if _, err := p.StartRunner(context.Background(), spec); err == nil || slices.Contains(api.called(), "POST /containers/create") {
			t.Fatalf("err %v calls %v", err, api.called())
		}
	})
	for _, tc := range []struct {
		name    string
		gateway http.HandlerFunc
		wantURL string
	}{
		{"gateway", reply(map[string]any{"IPAM": map[string]any{"Config": []map[string]string{{"Gateway": "172.20.0.1"}}}}), "PITLANE_CACHE_URL=http://172.20.0.1:8091/tok-pitlane-a/"},
		{"no route to the host", replyStatus(http.StatusNotFound), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPool("owner", SlotState{Index: 0, Status: statusBusy})
			p.env = testHost(t)
			p.cfg.ActionsCache = true
			p.chowned["pitlane-cache-acme"], p.chowned["pitlane-toolcache-acme"] = true, true
			b := newBroker()
			p.SetCache(b, 8091)
			var created container.Config
			newDockerAPI(t, p, map[string]http.HandlerFunc{
				"GET /networks/pitlane-slot-0": tc.gateway,
				"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
					_ = json.NewDecoder(r.Body).Decode(&created)
					reply(map[string]string{"Id": "c1"})(w, r)
				},
				"POST /containers/c1/start": replyStatus(http.StatusNoContent),
			})
			id, err := p.StartRunner(context.Background(), spec)
			if err != nil || id != "c1" {
				t.Fatalf("StartRunner: %q %v", id, err)
			}
			got := ""
			for _, e := range created.Env {
				if strings.HasPrefix(e, "PITLANE_CACHE_URL=") {
					got = e
				}
			}
			if got != tc.wantURL {
				t.Fatalf("cache env %q, want %q", got, tc.wantURL)
			}
			if _, registered := b.reg["pitlane-a"]; registered != (tc.wantURL != "") {
				t.Fatalf("token registered=%v with url %q", registered, tc.wantURL)
			}
			if p.slots[0].Runner != "pitlane-a" {
				t.Fatal("the slot must show its runner")
			}
		})
	}
}

// With Pitlane in a container, runners reach the cache through a forwarder
// that joins each slot's network.
func TestCacheHostThroughForwarder(t *testing.T) {
	setup := func(t *testing.T) (*Pool, map[string]http.HandlerFunc) {
		p := newTestPool("owner")
		p.env = testHost(t)
		write(t, p.env.dockerEnv, "")
		p.env.hostname = func() (string, error) { return "abc", nil }
		p.env.executable = func() (string, error) { return "/pitlane", nil }
		p.SetCache(newBroker(), 8091)
		return p, map[string]http.HandlerFunc{
			"GET /containers/abc/json": reply(map[string]any{"Id": "abcfull", "Name": "/pitlane", "Image": "img", "Config": map[string]any{},
				"NetworkSettings": map[string]any{"Networks": map[string]any{"compose": map[string]any{"IPAddress": "10.1.0.2"}}}}),
			"GET /containers/pitlane-cache-forward/json": reply(map[string]any{"Id": "f", "Config": map[string]any{"Labels": map[string]string{LabelSpec: "abcfull/8091"}},
				"State": map[string]any{"Running": true}, "NetworkSettings": map[string]any{"Networks": map[string]any{"pitlane-slot-0": map[string]any{"IPAddress": "10.9.0.5"}}}}),
			"POST /networks/pitlane-slot-0/connect": replyStatus(http.StatusOK),
		}
	}
	t.Run("address on the slot network", func(t *testing.T) {
		p, routes := setup(t)
		newDockerAPI(t, p, routes)
		if host, err := p.cacheHost(context.Background(), 0); err != nil || host != "10.9.0.5" {
			t.Fatalf("host %q %v", host, err)
		}
	})
	t.Run("already connected", func(t *testing.T) {
		p, routes := setup(t)
		routes["POST /networks/pitlane-slot-0/connect"] = replyStatus(http.StatusConflict)
		newDockerAPI(t, p, routes)
		if host, err := p.cacheHost(context.Background(), 0); err != nil || host != "10.9.0.5" {
			t.Fatalf("host %q %v", host, err)
		}
	})
	t.Run("connect fails", func(t *testing.T) {
		p, routes := setup(t)
		routes["POST /networks/pitlane-slot-0/connect"] = replyStatus(http.StatusInternalServerError)
		newDockerAPI(t, p, routes)
		if _, err := p.cacheHost(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "connect the actions cache forwarder") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("forwarder cannot be inspected", func(t *testing.T) {
		p, routes := setup(t)
		n := 0
		inspect := routes["GET /containers/pitlane-cache-forward/json"]
		routes["GET /containers/pitlane-cache-forward/json"] = func(w http.ResponseWriter, r *http.Request) {
			if n++; n > 1 { // the first call is ensureForwarder's
				replyStatus(http.StatusInternalServerError)(w, r)
				return
			}
			inspect(w, r)
		}
		newDockerAPI(t, p, routes)
		if _, err := p.cacheHost(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "inspect the actions cache forwarder") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("no address", func(t *testing.T) {
		p, routes := setup(t)
		routes["GET /containers/pitlane-cache-forward/json"] = reply(map[string]any{"Id": "f", "Config": map[string]any{"Labels": map[string]string{LabelSpec: "abcfull/8091"}},
			"State": map[string]any{"Running": true}})
		newDockerAPI(t, p, routes)
		if _, err := p.cacheHost(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "no address") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("forwarder cannot be created", func(t *testing.T) {
		p, routes := setup(t)
		routes["GET /containers/pitlane-cache-forward/json"] = replyStatus(http.StatusNotFound)
		routes["POST /containers/create"] = replyStatus(http.StatusInternalServerError)
		newDockerAPI(t, p, routes)
		if _, err := p.cacheHost(context.Background(), 0); err == nil {
			t.Fatal("a forwarder that cannot be created must be reported")
		}
	})
}

func TestLookupSelfFailures(t *testing.T) {
	for name, setup := range map[string]func(h *hostEnv, routes map[string]http.HandlerFunc){
		"no host name": func(h *hostEnv, _ map[string]http.HandlerFunc) {
			h.hostname = func() (string, error) { return "", errors.New("no") }
		},
		"no executable": func(h *hostEnv, _ map[string]http.HandlerFunc) {
			h.executable = func() (string, error) { return "", errors.New("no") }
		},
		"unknown to Docker": func(_ *hostEnv, routes map[string]http.HandlerFunc) {
			routes["GET /containers/abc/json"] = replyStatus(http.StatusNotFound)
		},
		"no usable network": func(_ *hostEnv, routes map[string]http.HandlerFunc) {
			routes["GET /containers/abc/json"] = reply(map[string]any{"Id": "x", "Name": "/p", "Config": map[string]any{},
				"NetworkSettings": map[string]any{"Networks": map[string]any{"pitlane-slot-0": map[string]any{"IPAddress": "10.0.0.2"}}}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newTestPool("owner")
			p.env = testHost(t)
			write(t, p.env.dockerEnv, "")
			p.env.hostname = func() (string, error) { return "abc", nil }
			p.env.executable = func() (string, error) { return "/pitlane", nil }
			routes := map[string]http.HandlerFunc{"GET /containers/abc/json": reply(map[string]any{})}
			setup(p.env, routes)
			newDockerAPI(t, p, routes)
			if self := p.selfContainer(context.Background()); self != nil {
				t.Fatalf("self %+v: Pitlane must fall back to the slot gateway", self)
			}
		})
	}
}

// TailJobOutput streams the job's pages, then stops when the runner is gone.
func TestTailJobOutput(t *testing.T) {
	const tailCmd = "tail -c +1 '" + pagesDir + "/a.log' | head -c 5"
	run := func(listing string, tail map[string]string) func(_, script string) (string, int) {
		return func(_, script string) (string, int) {
			if script == listPagesScript {
				return listing, 0
			}
			if out, ok := tail[script]; ok {
				return out, 0
			}
			return "", 1
		}
	}
	t.Run("streams pages until the writer stops it", func(t *testing.T) {
		p := newTestPool("owner")
		routes, _ := execRoutes(t, run("1.0 5 a.log\n", map[string]string{tailCmd: "hello"}), "c1")
		newDockerAPI(t, p, routes)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := &cancelWriter{cancel: cancel}
		if err := p.TailJobOutput(ctx, "c1", w); !errors.Is(err, context.Canceled) || w.buf.String() != "hello" {
			t.Fatalf("err %v output %q", err, w.buf.String())
		}
	})
	t.Run("a runner that finished ends the stream", func(t *testing.T) {
		p := newTestPool("owner")
		routes, _ := execRoutes(t, func(_, _ string) (string, int) { return "", 1 }, "c1")
		routes["GET /containers/c1/json"] = reply(map[string]any{"Id": "c1", "State": map[string]any{"Running": false}})
		newDockerAPI(t, p, routes)
		var w bytes.Buffer
		if err := p.TailJobOutput(context.Background(), "c1", &w); err != nil || w.Len() != 0 {
			t.Fatalf("err %v output %q", err, w.String())
		}
	})
	t.Run("a broken writer ends the stream", func(t *testing.T) {
		p := newTestPool("owner")
		routes, _ := execRoutes(t, run("1.0 5 a.log\n", map[string]string{tailCmd: "hello"}), "c1")
		newDockerAPI(t, p, routes)
		if err := p.TailJobOutput(context.Background(), "c1", errWriter{}); err == nil {
			t.Fatal("a write error must end the stream")
		}
	})
}

type cancelWriter struct {
	buf    bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelWriter) Write(b []byte) (int, error) {
	w.cancel()
	return w.buf.Write(b)
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }

func TestRunning(t *testing.T) {
	p := newTestPool("owner")
	newDockerAPI(t, p, map[string]http.HandlerFunc{
		"GET /containers/up/json":   reply(map[string]any{"Id": "up", "State": map[string]any{"Running": true}}),
		"GET /containers/down/json": reply(map[string]any{"Id": "down", "State": map[string]any{"Running": false}}),
		"GET /containers/err/json":  replyStatus(http.StatusInternalServerError),
	})
	for id, want := range map[string]bool{"up": true, "down": false, "err": false} {
		if got := p.running(context.Background(), id); got != want {
			t.Errorf("running(%s) = %v", id, got)
		}
	}
}

func TestCopyPages(t *testing.T) {
	routes := func(t *testing.T, p *Pool, out string, code int) {
		r, _ := execRoutes(t, func(_, _ string) (string, int) { return out, code }, "c1")
		newDockerAPI(t, p, r)
	}
	t.Run("only what a page gained is copied, each page up to its announced size", func(t *testing.T) {
		p := newTestPool("owner")
		routes(t, p, "world", 0)
		var w bytes.Buffer
		tl := &jobTail{p: p, id: "c1", w: &w, offsets: map[string]int{"a.log": 5}}
		done, err := tl.copyPages(context.Background(), []page{{"a.log", 10}, {"b.log", 0}, {"c.log", 3}})
		if done || err != nil || tl.offsets["a.log"] != 10 || tl.offsets["c.log"] != 3 || w.String() != "world"+"wor" {
			t.Fatalf("done %v err %v offsets %v output %q", done, err, tl.offsets, w.String())
		}
	})
	t.Run("a page that cannot be read is skipped", func(t *testing.T) {
		p := newTestPool("owner")
		routes(t, p, "", -1)
		tl := &jobTail{p: p, id: "c1", w: io.Discard, offsets: map[string]int{}}
		if done, err := tl.copyPages(context.Background(), []page{{"a.log", 10}}); done || err != nil || tl.offsets["a.log"] != 0 {
			t.Fatalf("done %v err %v offsets %v", done, err, tl.offsets)
		}
	})
	t.Run("a page's output is capped to what the listing announced", func(t *testing.T) {
		p := newTestPool("owner")
		routes(t, p, "0123456789-too-much", 0)
		var w bytes.Buffer
		tl := &jobTail{p: p, id: "c1", w: &w, offsets: map[string]int{}}
		_, _ = tl.copyPages(context.Background(), []page{{"a.log", 10}})
		if w.String() != "0123456789" || tl.offsets["a.log"] != 10 {
			t.Fatalf("output %q offsets %v", w.String(), tl.offsets)
		}
	})
	t.Run("a stream is truncated at its maximum", func(t *testing.T) {
		p := newTestPool("owner")
		routes(t, p, "x", 0)
		var w bytes.Buffer
		tl := &jobTail{p: p, id: "c1", w: &w, offsets: map[string]int{}, total: tailTotalMax - 1}
		done, err := tl.copyPages(context.Background(), []page{{"a.log", 1}, {"b.log", 1}})
		if !done || err != nil || !strings.Contains(w.String(), "output truncated") || strings.Count(w.String(), "x") != 1 {
			t.Fatalf("done %v err %v output %q", done, err, w.String())
		}
	})
}
