package pool

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestConfigureResizesAndStartsSlots(t *testing.T) {
	p, _ := slotPool(4, SlotState{Index: 0, Status: statusReady, Owner: "acme"},
		SlotState{Index: 1, Status: statusReady}, SlotState{Index: 2, Status: statusBusy}, SlotState{Index: 3, Status: statusReady})
	routes := map[string]http.HandlerFunc{"DELETE /containers/pitlane-slot-3": replyStatus(http.StatusNoContent)}
	for i := range 4 {
		slotUpRoutes(routes, i)
	}
	api := newDockerAPI(t, p, routes)
	cfg := p.Config()
	cfg.Slots = 3
	if err := p.Configure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.slots[3]; ok {
		t.Fatal("an idle slot beyond the new count must be removed at once")
	}
	if !slices.Contains(api.called(), "DELETE /containers/pitlane-slot-3") {
		t.Fatalf("calls %v", api.called())
	}

	cfg.Slots = 1
	if err := p.Configure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !p.slots[2].retire || p.slots[2].Status != statusBusy {
		t.Fatalf("a busy slot beyond the count is retired when its job ends: %+v", p.slots[2])
	}

	cfg.Slots = 4
	if err := p.Configure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if p.slots[2].retire {
		t.Fatal("growing back must cancel the retirement")
	}
	if s := p.slots[3]; s == nil || s.Status != statusReady || s.Owner != UnknownOwner {
		t.Fatalf("a new slot starts with an unknown owner and comes up: %+v", s)
	}
}

// Changing the settings that live outside the slot table applies them.
func TestConfigureAppliesChanges(t *testing.T) {
	p, _ := slotPool(1, SlotState{Index: 0, Status: statusReady})
	b := newBroker()
	p.SetCache(b, 8091)
	pulled := make(chan string, 1)
	routes := map[string]http.HandlerFunc{
		"GET /images/ghcr.io/acme/runner:v2/json": replyStatus(http.StatusNotFound),
		"POST /images/create": func(w http.ResponseWriter, r *http.Request) {
			pulled <- r.URL.Query().Get("fromImage") + ":" + r.URL.Query().Get("tag")
			_, _ = w.Write([]byte(`{"status":"done"}`))
		},
		// the mirror
		"GET /networks/pitlane-mirror":          reply(map[string]string{"Id": "n"}),
		"GET /containers/pitlane-mirror/json":   reply(map[string]any{"Id": "m"}),
		"POST /containers/pitlane-mirror/start": replyStatus(http.StatusNoContent),
	}
	slotUpRoutes(routes, 0)
	api := newDockerAPI(t, p, routes)
	cfg := p.Config()
	cfg.ActionsCacheGB, cfg.ActionsCacheOwnerPct = 7, 40
	cfg.RunnerImage = "ghcr.io/acme/runner:v2"
	cfg.Mirror = true
	if err := p.Configure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if b.limit != 7<<30 || b.ownerPct != 40 {
		t.Fatalf("cache limits %d %d%%", b.limit, b.ownerPct)
	}
	if got := <-pulled; got != "ghcr.io/acme/runner:v2" {
		t.Fatalf("pulled %q", got)
	}
	if !slices.Contains(api.called(), "POST /containers/pitlane-mirror/start") {
		t.Fatalf("the mirror was not started: %v", api.called())
	}
}

func TestStartBringsSlotsUp(t *testing.T) {
	p, _ := slotPool(2)
	p.env = testHost(t)
	p.cfg.NoQEMU = true
	routes := map[string]http.HandlerFunc{}
	for _, img := range []string{p.cfg.DindImage, p.cfg.RunnerImage} {
		routes["GET /images/"+img+"/json"] = reply(map[string]string{"Id": "sha256:img"})
	}
	for i := range 2 {
		slotUpRoutes(routes, i)
	}
	newDockerAPI(t, p, routes)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stops the retry loop
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if p.FreeSlots() != 2 || p.qemuWant {
		t.Fatalf("free slots %d, qemuWant %v", p.FreeSlots(), p.qemuWant)
	}
}

func TestStartFailsWithoutDindImage(t *testing.T) {
	p, _ := slotPool(2)
	newDockerAPI(t, p, map[string]http.HandlerFunc{
		"GET /images/" + p.cfg.DindImage + "/json": replyStatus(http.StatusNotFound),
		"POST /images/create":                      replyStatus(http.StatusInternalServerError),
	})
	err := p.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pull "+p.cfg.DindImage) {
		t.Fatalf("Start: %v", err)
	}
}

// When Pitlane's runner image cannot be pulled, the order of preference is a
// Pitlane image already on the host, then GitHub's bare one; an image chosen
// in the settings has no fallback.
func TestEnsureRunnerImageFallbacks(t *testing.T) {
	missing := func(img string) map[string]http.HandlerFunc {
		return map[string]http.HandlerFunc{"GET /images/" + img + "/json": replyStatus(http.StatusNotFound)}
	}
	pullFails := replyStatus(http.StatusInternalServerError)
	t.Run("pulled", func(t *testing.T) {
		p := newTestPool("owner")
		newDockerAPI(t, p, map[string]http.HandlerFunc{"GET /images/" + DefaultRunnerImage + "/json": reply(map[string]string{"Id": "x"})})
		if got, err := p.ensureRunnerImage(context.Background(), DefaultRunnerImage); err != nil || got != DefaultRunnerImage {
			t.Fatalf("%q %v", got, err)
		}
	})
	t.Run("a Pitlane image already on the host", func(t *testing.T) {
		p := newTestPool("owner")
		routes := missing(DefaultRunnerImage)
		routes["POST /images/create"] = pullFails
		routes["GET /images/json"] = reply([]map[string]any{
			{"Id": "a", "RepoTags": []string{runnerRepo + ":v0.9.0"}, "Created": 10},
			{"Id": "b", "RepoTags": []string{runnerRepo + ":v1.0.0", "other:tag"}, "Created": 30},
			{"Id": "c", "RepoTags": []string{runnerRepo + ":v0.5.0"}, "Created": 20},
		})
		newDockerAPI(t, p, routes)
		if got, err := p.ensureRunnerImage(context.Background(), DefaultRunnerImage); err != nil || got != runnerRepo+":v1.0.0" {
			t.Fatalf("must use the most recent local Pitlane image: %q %v", got, err)
		}
	})
	t.Run("GitHub's bare image as a last resort", func(t *testing.T) {
		p := newTestPool("owner")
		routes := missing(DefaultRunnerImage)
		routes["GET /images/"+OfficialRunnerImage+"/json"] = reply(map[string]string{"Id": "x"})
		routes["POST /images/create"] = pullFails
		routes["GET /images/json"] = reply([]map[string]any{})
		newDockerAPI(t, p, routes)
		if got, err := p.ensureRunnerImage(context.Background(), DefaultRunnerImage); err != nil || got != OfficialRunnerImage {
			t.Fatalf("%q %v", got, err)
		}
	})
	t.Run("nothing available", func(t *testing.T) {
		p := newTestPool("owner")
		routes := missing(DefaultRunnerImage)
		routes["GET /images/"+OfficialRunnerImage+"/json"] = replyStatus(http.StatusNotFound)
		routes["POST /images/create"] = pullFails
		routes["GET /images/json"] = replyStatus(http.StatusInternalServerError)
		newDockerAPI(t, p, routes)
		if _, err := p.ensureRunnerImage(context.Background(), DefaultRunnerImage); err == nil {
			t.Fatal("no image at all must be an error")
		}
	})
	t.Run("a chosen image has no fallback", func(t *testing.T) {
		p := newTestPool("owner")
		routes := missing("my/runner:1")
		routes["POST /images/create"] = pullFails
		api := newDockerAPI(t, p, routes)
		if _, err := p.ensureRunnerImage(context.Background(), "my/runner:1"); err == nil {
			t.Fatal("the chosen image failed to pull: that must be reported")
		}
		if slices.Contains(api.called(), "GET /images/json") {
			t.Fatal("looked for a fallback to an image chosen in the settings")
		}
	})
}

func TestEnsureSlotVariants(t *testing.T) {
	t.Run("a stopped slot is started", func(t *testing.T) {
		p, ops := slotPool(1, SlotState{Index: 0, Status: statusStarting})
		routes := map[string]http.HandlerFunc{
			"GET /networks/pitlane-slot-0":          reply(map[string]string{"Id": "n"}),
			"GET /containers/pitlane-slot-0/json":   reply(map[string]any{"Id": "c", "Config": map[string]any{"Labels": map[string]string{LabelSpec: slotSpecVersion}}, "State": map[string]any{"Running": false}}),
			"POST /containers/pitlane-slot-0/start": replyStatus(http.StatusNoContent),
		}
		newDockerAPI(t, p, routes)
		if err := p.ensureSlot(context.Background(), 0); err != nil || len(ops.scripts) != 2 {
			t.Fatalf("err %v scripts %q", err, ops.scripts)
		}
	})
	t.Run("a start that fails", func(t *testing.T) {
		p, _ := slotPool(1, SlotState{Index: 0, Status: statusStarting})
		routes := map[string]http.HandlerFunc{
			"GET /networks/pitlane-slot-0":          reply(map[string]string{"Id": "n"}),
			"GET /containers/pitlane-slot-0/json":   reply(map[string]any{"Id": "c", "Config": map[string]any{"Labels": map[string]string{LabelSpec: slotSpecVersion}}, "State": map[string]any{"Running": false}}),
			"POST /containers/pitlane-slot-0/start": replyStatus(http.StatusInternalServerError),
		}
		newDockerAPI(t, p, routes)
		if err := p.ensureSlot(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "start dind") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("inspect fails", func(t *testing.T) {
		p, _ := slotPool(1, SlotState{Index: 0, Status: statusStarting})
		newDockerAPI(t, p, map[string]http.HandlerFunc{
			"GET /networks/pitlane-slot-0":        reply(map[string]string{"Id": "n"}),
			"GET /containers/pitlane-slot-0/json": replyStatus(http.StatusInternalServerError),
		})
		if err := p.ensureSlot(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "inspect dind") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("create fails", func(t *testing.T) {
		p, _ := slotPool(1, SlotState{Index: 0, Status: statusStarting})
		newDockerAPI(t, p, map[string]http.HandlerFunc{
			"GET /networks/pitlane-slot-0":        reply(map[string]string{"Id": "n"}),
			"GET /containers/pitlane-slot-0/json": replyStatus(http.StatusNotFound),
			"POST /containers/create":             replyStatus(http.StatusInternalServerError),
		})
		if err := p.ensureSlot(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "create dind") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("a running slot joins the mirror", func(t *testing.T) {
		p, _ := slotPool(1, SlotState{Index: 0, Status: statusStarting})
		p.cfg.Mirror = true
		routes := map[string]http.HandlerFunc{
			"POST /networks/pitlane-mirror/disconnect": replyStatus(http.StatusNotFound),
			"POST /networks/pitlane-slot-0/connect":    replyStatus(http.StatusConflict), // already connected
		}
		slotUpRoutes(routes, 0)
		api := newDockerAPI(t, p, routes)
		if err := p.ensureSlot(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(api.called(), "POST /networks/pitlane-slot-0/connect") {
			t.Fatalf("calls %v", api.called())
		}
	})
}

// A slot created by an older Pitlane is recreated, unless a runner is using
// it: it is then only marked, and recreated once its job is released.
func TestUpgradeDind(t *testing.T) {
	outdated := reply(map[string]any{"Id": "c", "Config": map[string]any{"Labels": map[string]string{LabelSpec: "1"}}, "State": map[string]any{"Running": true}})
	base := func() map[string]http.HandlerFunc {
		routes := map[string]http.HandlerFunc{"GET /networks/pitlane-slot-0": reply(map[string]string{"Id": "n"})}
		slotUpRoutes(routes, 0)
		routes["GET /containers/pitlane-slot-0/json"] = outdated
		return routes
	}
	t.Run("a runner uses it", func(t *testing.T) {
		p, _ := slotPool(1, SlotState{Index: 0, Status: statusBusy})
		routes := base()
		routes["GET /containers/json"] = reply([]map[string]string{{"Id": "r"}})
		api := newDockerAPI(t, p, routes)
		if err := p.ensureSlot(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if !p.slots[0].upgrade || slices.Contains(api.called(), "DELETE /containers/pitlane-slot-0") {
			t.Fatalf("upgrade %v calls %v: a slot running a job must not be killed", p.slots[0].upgrade, api.called())
		}
	})
	t.Run("when unsure nothing is killed", func(t *testing.T) {
		p, _ := slotPool(1, SlotState{Index: 0, Status: statusBusy})
		routes := base()
		routes["GET /containers/json"] = replyStatus(http.StatusInternalServerError)
		api := newDockerAPI(t, p, routes)
		if err := p.ensureSlot(context.Background(), 0); err != nil || !p.slots[0].upgrade || slices.Contains(api.called(), "DELETE /containers/pitlane-slot-0") {
			t.Fatalf("err %v upgrade %v calls %v", err, p.slots[0].upgrade, api.called())
		}
	})
	t.Run("idle: recreated now", func(t *testing.T) {
		p, _ := slotPool(1, SlotState{Index: 0, Status: statusStarting})
		var mu sync.Mutex
		removed := false
		routes := base()
		routes["GET /containers/json"] = reply([]map[string]string{})
		routes["DELETE /containers/pitlane-slot-0"] = func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			removed = true
			mu.Unlock()
			replyStatus(http.StatusNoContent)(w, r)
		}
		routes["GET /containers/pitlane-slot-0/json"] = func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			gone := removed
			mu.Unlock()
			if gone {
				replyStatus(http.StatusNotFound)(w, r)
				return
			}
			outdated(w, r)
		}
		routes["POST /containers/create"] = reply(map[string]string{"Id": "c2"})
		routes["POST /containers/pitlane-slot-0/start"] = replyStatus(http.StatusNoContent)
		api := newDockerAPI(t, p, routes)
		if err := p.ensureSlot(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(api.called(), "POST /containers/create") {
			t.Fatalf("calls %v", api.called())
		}
	})
	t.Run("removal fails", func(t *testing.T) {
		p, _ := slotPool(1, SlotState{Index: 0, Status: statusStarting})
		routes := base()
		routes["GET /containers/json"] = reply([]map[string]string{})
		routes["DELETE /containers/pitlane-slot-0"] = replyStatus(http.StatusInternalServerError)
		newDockerAPI(t, p, routes)
		if err := p.ensureSlot(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "remove outdated dind") {
			t.Fatalf("err %v", err)
		}
	})
}

func TestWaitDindStopsWhenCanceled(t *testing.T) {
	p, ops := slotPool(1)
	ops.code = 1 // the daemon never answers
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.waitDind(ctx, "pitlane-slot-0"); err == nil || len(ops.scripts) == 0 {
		t.Fatalf("a canceled wait must stop with an error: %v", err)
	}
}

func TestDockerHelpers(t *testing.T) {
	t.Run("EnsureImage", func(t *testing.T) {
		p := newTestPool("owner")
		var pulls int
		newDockerAPI(t, p, map[string]http.HandlerFunc{
			"GET /images/a:1/json": reply(map[string]string{"Id": "x"}),
			"GET /images/b:1/json": replyStatus(http.StatusNotFound),
			"POST /images/create": func(w http.ResponseWriter, _ *http.Request) {
				pulls++
				_, _ = w.Write([]byte(`{"status":"done"}`))
			},
		})
		ctx := context.Background()
		if err := p.EnsureImage(ctx, "a:1", false); err != nil || pulls != 0 {
			t.Fatalf("a present image is not pulled: %v %d", err, pulls)
		}
		if err := p.EnsureImage(ctx, "a:1", true); err != nil || pulls != 1 {
			t.Fatalf("forced: %v %d", err, pulls)
		}
		if err := p.EnsureImage(ctx, "b:1", false); err != nil || pulls != 2 {
			t.Fatalf("a missing image is pulled: %v %d", err, pulls)
		}
	})
	t.Run("EnsureImage pull error", func(t *testing.T) {
		p := newTestPool("owner")
		newDockerAPI(t, p, map[string]http.HandlerFunc{
			"GET /images/b:1/json": replyStatus(http.StatusNotFound),
			"POST /images/create":  replyStatus(http.StatusInternalServerError),
		})
		if err := p.EnsureImage(context.Background(), "b:1", false); err == nil || !strings.Contains(err.Error(), "pull b:1") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("ensureNetwork", func(t *testing.T) {
		p := newTestPool("owner")
		newDockerAPI(t, p, map[string]http.HandlerFunc{
			"GET /networks/new":     replyStatus(http.StatusNotFound),
			"POST /networks/create": reply(map[string]string{"Id": "n"}),
		})
		if err := p.ensureNetwork(context.Background(), "new", false); err != nil {
			t.Fatal(err)
		}
		newDockerAPI(t, p, map[string]http.HandlerFunc{
			"GET /networks/race":    replyStatus(http.StatusNotFound),
			"POST /networks/create": replyStatus(http.StatusConflict), // created meanwhile
			"GET /networks/bad":     replyStatus(http.StatusNotFound),
		})
		if err := p.ensureNetwork(context.Background(), "race", false); err != nil {
			t.Fatalf("a network created by someone else is fine: %v", err)
		}
		newDockerAPI(t, p, map[string]http.HandlerFunc{
			"GET /networks/bad":     replyStatus(http.StatusNotFound),
			"POST /networks/create": replyStatus(http.StatusInternalServerError),
		})
		if err := p.ensureNetwork(context.Background(), "bad", false); err == nil {
			t.Fatal("a network that cannot be created must be reported")
		}
	})
	t.Run("ensureVolume", func(t *testing.T) {
		p := newTestPool("owner")
		routes := map[string]http.HandlerFunc{
			"GET /volumes/v":              replyStatus(http.StatusNotFound),
			"POST /volumes/create":        reply(map[string]string{"Name": "v"}),
			"POST /containers/create":     reply(map[string]string{"Id": "init"}),
			"POST /containers/init/start": replyStatus(http.StatusNoContent),
			"POST /containers/init/wait":  reply(map[string]int{"StatusCode": 0}),
		}
		api := newDockerAPI(t, p, routes)
		if err := p.ensureVolume(context.Background(), "v"); err != nil || !p.chowned["v"] {
			t.Fatalf("err %v chowned %v", err, p.chowned["v"])
		}
		n := len(api.called())
		if err := p.ensureVolume(context.Background(), "v"); err != nil || len(api.called()) != n {
			t.Fatal("a prepared volume is not prepared again")
		}
	})
	t.Run("ensureVolume failures", func(t *testing.T) {
		for name, routes := range map[string]map[string]http.HandlerFunc{
			"create volume":  {"GET /volumes/v": replyStatus(http.StatusNotFound), "POST /volumes/create": replyStatus(http.StatusInternalServerError)},
			"prepare volume": {"GET /volumes/v": reply(map[string]string{"Name": "v"}), "POST /containers/create": replyStatus(http.StatusInternalServerError)},
			"start":          {"GET /volumes/v": reply(map[string]string{"Name": "v"}), "POST /containers/create": reply(map[string]string{"Id": "init"}), "POST /containers/init/start": replyStatus(http.StatusInternalServerError)},
		} {
			p := newTestPool("owner")
			newDockerAPI(t, p, routes)
			if err := p.ensureVolume(context.Background(), "v"); err == nil || p.chowned["v"] {
				t.Fatalf("%s: err %v chowned %v: a volume that could not be prepared must not be marked prepared", name, err, p.chowned["v"])
			}
		}
	})
	t.Run("Wait", func(t *testing.T) {
		p := newTestPool("owner")
		newDockerAPI(t, p, map[string]http.HandlerFunc{
			"POST /containers/ok/wait":  reply(map[string]int{"StatusCode": 7}),
			"POST /containers/bad/wait": replyStatus(http.StatusInternalServerError),
		})
		if got := p.Wait(context.Background(), "ok"); got != 7 {
			t.Fatalf("exit code %d", got)
		}
		if got := p.Wait(context.Background(), "bad"); got != -1 {
			t.Fatalf("a wait that fails reports -1, got %d", got)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if got := p.Wait(ctx, "ok"); got != -1 {
			t.Fatalf("canceled wait: %d", got)
		}
	})
}

func TestEnsureMirror(t *testing.T) {
	cfg := Config{Mirror: true}
	t.Run("off", func(t *testing.T) {
		p := newTestPool("owner")
		if err := p.ensureMirror(context.Background(), Config{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("created and started", func(t *testing.T) {
		p := newTestPool("owner")
		api := newDockerAPI(t, p, map[string]http.HandlerFunc{
			"GET /networks/pitlane-mirror":          reply(map[string]string{"Id": "n"}),
			"GET /containers/pitlane-mirror/json":   replyStatus(http.StatusNotFound),
			"GET /images/" + mirrorImage + "/json":  reply(map[string]string{"Id": "x"}),
			"GET /volumes/" + mirrorVolume:          reply(map[string]string{"Name": mirrorVolume}),
			"POST /containers/create":               reply(map[string]string{"Id": "m"}),
			"POST /containers/init/start":           replyStatus(http.StatusNoContent),
			"POST /containers/init/wait":            reply(map[string]int{"StatusCode": 0}),
			"POST /containers/pitlane-mirror/start": replyStatus(http.StatusNoContent),
		})
		p.chowned[mirrorVolume] = true
		if err := p.ensureMirror(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(api.called(), "POST /containers/pitlane-mirror/start") {
			t.Fatalf("calls %v", api.called())
		}
	})
	t.Run("create fails", func(t *testing.T) {
		p := newTestPool("owner")
		newDockerAPI(t, p, map[string]http.HandlerFunc{
			"GET /networks/pitlane-mirror":         reply(map[string]string{"Id": "n"}),
			"GET /containers/pitlane-mirror/json":  replyStatus(http.StatusNotFound),
			"GET /images/" + mirrorImage + "/json": reply(map[string]string{"Id": "x"}),
			"POST /containers/create":              replyStatus(http.StatusInternalServerError),
		})
		p.chowned[mirrorVolume] = true
		if err := p.ensureMirror(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "registry mirror") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("an existing mirror is only started", func(t *testing.T) {
		p := newTestPool("owner")
		api := newDockerAPI(t, p, map[string]http.HandlerFunc{
			"GET /networks/pitlane-mirror":          reply(map[string]string{"Id": "n"}),
			"GET /containers/pitlane-mirror/json":   reply(map[string]any{"Id": "m"}),
			"POST /containers/pitlane-mirror/start": replyStatus(http.StatusNotModified),
		})
		if err := p.ensureMirror(context.Background(), cfg); err != nil || slices.Contains(api.called(), "POST /containers/create") {
			t.Fatalf("err %v calls %v", err, api.called())
		}
	})
}
