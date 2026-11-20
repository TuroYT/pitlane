package update

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/client"
)

// fakeDocker is the few Docker API endpoints DockerImages uses.
type fakeDocker struct {
	mu         sync.Mutex
	image      map[string]any // body of GET /images/{ref}/json
	imageCode  int
	deps       string // content of Runner.Listener.deps.json ("" = no such file)
	createCode int
	created    []map[string]any
	removed    []string
}

func newDockerImages(t *testing.T, d *fakeDocker) DockerImages {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/v1.45")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
			if d.imageCode != 0 {
				w.WriteHeader(d.imageCode)
				_, _ = w.Write([]byte(`{"message":"No such image"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(d.image)
		case r.Method == http.MethodPost && path == "/containers/create":
			if d.createCode != 0 {
				w.WriteHeader(d.createCode)
				_, _ = w.Write([]byte(`{"message":"cannot create"}`))
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			d.created = append(d.created, body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"c0ffee","Warnings":[]}`))
		case r.Method == http.MethodGet && path == "/containers/c0ffee/archive":
			if d.deps == "" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Could not find the file"}`))
				return
			}
			stat, _ := json.Marshal(map[string]any{"name": "Runner.Listener.deps.json", "size": len(d.deps), "mode": 0o644})
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(stat))
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			_ = tw.WriteHeader(&tar.Header{Name: "Runner.Listener.deps.json", Mode: 0o644, Size: int64(len(d.deps))})
			_, _ = tw.Write([]byte(d.deps))
			_ = tw.Close()
			_, _ = w.Write(buf.Bytes())
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/containers/"):
			d.removed = append(d.removed, strings.TrimPrefix(path, "/containers/")+"?"+r.URL.RawQuery)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return DockerImages{Cli: cli, PullFunc: func(context.Context, string) error { return nil }}
}

func TestDockerImagesPull(t *testing.T) {
	t.Parallel()
	d := newDockerImages(t, &fakeDocker{})
	var pulled string
	d.PullFunc = func(_ context.Context, ref string) error { pulled = ref; return nil }
	if err := d.Pull(context.Background(), "ghcr.io/x/runner:v1"); err != nil || pulled != "ghcr.io/x/runner:v1" {
		t.Fatalf("%q %v", pulled, err)
	}
	d.PullFunc = func(context.Context, string) error { return errors.New("denied") }
	if err := d.Pull(context.Background(), "x"); err == nil || err.Error() != "denied" {
		t.Fatalf("pull error not propagated: %v", err)
	}
}

func TestDockerImagesInspect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ref     string
		digests []string
		want    string
	}{
		{"no digest (local image)", "pitlane-runner:dev", nil, ""},
		{"the digest of this repository wins", "ghcr.io/turoyt/pitlane-runner:latest",
			[]string{"docker.io/other/runner@sha256:aaa", "ghcr.io/turoyt/pitlane-runner@sha256:bbb"}, "ghcr.io/turoyt/pitlane-runner@sha256:bbb"},
		{"first digest when none matches", "ghcr.io/x/runner:latest", []string{"mirror/runner@sha256:111", "other/runner@sha256:222"}, "mirror/runner@sha256:111"},
		{"registry with a port keeps its host", "localhost:5000/runner", []string{"other/runner@sha256:1", "localhost:5000/runner@sha256:2"}, "localhost:5000/runner@sha256:2"},
		{"registry with a port and a tag", "localhost:5000/runner:v2", []string{"other/runner@sha256:1", "localhost:5000/runner@sha256:2"}, "localhost:5000/runner@sha256:2"},
		{"a longer repository name is not a match", "ghcr.io/x/runner:latest", []string{"ghcr.io/x/runner-extra@sha256:1"}, "ghcr.io/x/runner-extra@sha256:1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDockerImages(t, &fakeDocker{image: map[string]any{"Id": "sha256:imageid", "Created": "2026-11-01T00:00:00Z", "RepoDigests": tc.digests}})
			got, err := d.Inspect(context.Background(), tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != "sha256:imageid" || got.Created != "2026-11-01T00:00:00Z" || got.Digest != tc.want {
				t.Fatalf("%+v, want digest %q", got, tc.want)
			}
		})
	}
	t.Run("unknown image", func(t *testing.T) {
		t.Parallel()
		d := newDockerImages(t, &fakeDocker{imageCode: http.StatusNotFound})
		if _, err := d.Inspect(context.Background(), "nope"); err == nil {
			t.Fatal("no error for a missing image")
		}
	})
}

func TestDockerImagesRunnerVersion(t *testing.T) {
	t.Parallel()
	t.Run("reads the version without starting the container", func(t *testing.T) {
		t.Parallel()
		fd := &fakeDocker{deps: `{"targets":{"x":{"Runner.Listener/2.340.1":{}}}}`}
		d := newDockerImages(t, fd)
		v, err := d.RunnerVersion(context.Background(), "ghcr.io/x/runner:latest")
		if err != nil || v != "2.340.1" {
			t.Fatalf("%q %v", v, err)
		}
		fd.mu.Lock()
		defer fd.mu.Unlock()
		if len(fd.created) != 1 {
			t.Fatalf("created %d containers", len(fd.created))
		}
		c := fd.created[0]
		if c["Image"] != "ghcr.io/x/runner:latest" || len(c["Entrypoint"].([]any)) != 1 || c["Entrypoint"].([]any)[0] != "/bin/true" {
			t.Errorf("container config: %v", c)
		}
		labels, _ := c["Labels"].(map[string]any)
		if len(labels) == 0 {
			t.Error("the helper container is not labelled as Pitlane's (it would escape cleanup)")
		}
		if len(fd.removed) != 1 || !strings.HasPrefix(fd.removed[0], "c0ffee?") || !strings.Contains(fd.removed[0], "force=1") {
			t.Errorf("container not removed: %v", fd.removed)
		}
	})
	t.Run("not a runner image", func(t *testing.T) {
		t.Parallel()
		fd := &fakeDocker{}
		d := newDockerImages(t, fd)
		_, err := d.RunnerVersion(context.Background(), "alpine")
		if err == nil || !strings.Contains(err.Error(), "not found in alpine") {
			t.Fatalf("err = %v", err)
		}
		if len(fd.removed) != 1 {
			t.Errorf("container left behind: %v", fd.removed)
		}
	})
	t.Run("no version in the file", func(t *testing.T) {
		t.Parallel()
		d := newDockerImages(t, &fakeDocker{deps: `{"targets":{}}`})
		if _, err := d.RunnerVersion(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "version not found") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("container creation fails", func(t *testing.T) {
		t.Parallel()
		d := newDockerImages(t, &fakeDocker{createCode: http.StatusInternalServerError})
		if _, err := d.RunnerVersion(context.Background(), "x"); err == nil {
			t.Fatal("no error")
		}
	})
}
