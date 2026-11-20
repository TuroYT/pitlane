package pool

import (
	"context"
	"net/http"
	"testing"
)

// When Pitlane's runner image cannot be pulled and an older Pitlane image is
// on the host, Start uses it. Configure, which Start ends with, must not put
// the image that failed back.
func TestStartKeepsRunnerImageFallback(t *testing.T) {
	p, _ := slotPool(1)
	p.env = testHost(t)
	p.cfg.NoQEMU = true
	want := runnerRepo + ":v0.1.0"
	routes := map[string]http.HandlerFunc{
		"GET /images/" + p.cfg.DindImage + "/json":   reply(map[string]string{"Id": "x"}),
		"GET /images/" + p.cfg.RunnerImage + "/json": replyStatus(http.StatusNotFound),
		"POST /images/create":                        replyStatus(http.StatusInternalServerError),
		"GET /images/json":                           reply([]map[string]any{{"Id": "a", "RepoTags": []string{want}, "Created": 1}}),
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
}
