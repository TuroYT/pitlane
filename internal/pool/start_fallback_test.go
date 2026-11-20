package pool

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
)

// When Pitlane's runner image cannot be pulled and an older Pitlane image is
// on the host, Start uses it. Configure, which Start ends with, must not put
// the image that failed back.
func TestStartKeepsRunnerImageFallback(t *testing.T) {
	p, ops := slotPool(1)
	p.sops = dockerImages{ops, p}
	p.env = testHost(t)
	p.cfg.NoQEMU, p.cfg.Slots = true, 1
	want := runnerRepo + ":v0.1.0"
	// The default image is missing until the test says it was pulled; every
	// look at it is reported on looked.
	var pulled atomic.Bool
	looked := make(chan struct{}, 16)
	routes := map[string]http.HandlerFunc{
		"GET /images/" + p.cfg.DindImage + "/json": reply(map[string]string{"Id": "x"}),
		"GET /images/" + p.cfg.RunnerImage + "/json": func(w http.ResponseWriter, r *http.Request) {
			looked <- struct{}{}
			if pulled.Load() {
				reply(map[string]string{"Id": "sha256:default"})(w, r)
				return
			}
			replyStatus(http.StatusNotFound)(w, r)
		},
		"POST /images/create": replyStatus(http.StatusInternalServerError),
		"GET /images/json":    reply([]map[string]any{{"Id": "a", "RepoTags": []string{want}, "Created": 1}}),
	}
	slotUpRoutes(routes, 0)
	newDockerAPI(t, p, routes)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := p.Config().RunnerImage; got != want {
		t.Fatalf("runners would use %q, want the image found on the host: %q", got, want)
	}
	// A settings change that sends the default image back keeps the fallback.
	cfg := p.Config()
	cfg.RunnerImage = DefaultRunnerImage
	cfg.KeepBuildGB = 5
	if err := p.Configure(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := p.Config().RunnerImage; got != want {
		t.Fatalf("after a settings change: %q, want %q", got, want)
	}

	// Once the default image is on the host, Configure applies it again and
	// the fallback is forgotten for good. Its background pull check is the
	// last look at the image (Start 1, Configure 2, 3, 4, check 5).
	pulled.Store(true)
	cfg.KeepBuildGB = 6
	if err := p.Configure(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		<-looked
	}
	if got := p.Config().RunnerImage; got != DefaultRunnerImage {
		t.Fatalf("default image available again: runners use %q, want %q", got, DefaultRunnerImage)
	}
	p.mu.Lock()
	fb := p.runnerFallback
	p.mu.Unlock()
	if fb.from != "" {
		t.Fatalf("fallback still set: %+v", fb)
	}
	pulled.Store(false) // gone again: the cleared fallback is not used any more
	cfg.KeepBuildGB = 7
	if err := p.Configure(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := p.Config().RunnerImage; got != DefaultRunnerImage {
		t.Fatalf("after the fallback was dropped: %q, want %q", got, DefaultRunnerImage)
	}
}

// dockerImages answers the image lookups through Docker, the rest from the
// fake slot ops.
type dockerImages struct {
	*fakeSlotOps
	p *Pool
}

func (d dockerImages) imageID(ctx context.Context, ref string) (string, error) {
	return dockerSlotOps{d.p}.imageID(ctx, ref)
}
