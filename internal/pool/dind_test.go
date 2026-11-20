package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

func TestDindSpec(t *testing.T) {
	cfg := Config{DindImage: "dind:test", MemoryMB: 512, Mirror: true}
	cc, hc, nc := dindSpec(cfg, 3)
	want := []string{"--host=tcp://127.0.0.1:2375", "--host=unix:///var/run/docker.sock", "--tls=false", "--max-concurrent-downloads=8", "--registry-mirror=http://pitlane-mirror:5000"}
	if !slices.Equal(cc.Cmd, want) || !slices.Equal(cc.Entrypoint, []string{"dind", "dockerd"}) {
		t.Fatalf("command line: %v %v", cc.Entrypoint, cc.Cmd)
	}
	if cc.Labels[LabelSpec] != slotSpecVersion || cc.Labels[LabelSlot] != "3" || cc.Hostname != "pitlane-slot-3" {
		t.Fatalf("labels/hostname: %v %s", cc.Labels, cc.Hostname)
	}
	if !hc.Privileged || hc.Memory != 512<<20 || hc.Tmpfs["/run"] == "" || len(hc.Mounts) != 3 || !hc.Mounts[2].ReadOnly {
		t.Fatalf("host config: %+v", hc)
	}
	if nc.EndpointsConfig["pitlane-slot-3"] == nil {
		t.Fatalf("network: %+v", nc.EndpointsConfig)
	}
	cc, hc, _ = dindSpec(Config{DindImage: "dind:test"}, 0)
	if len(cc.Cmd) != 4 || hc.Memory != 0 {
		t.Fatalf("no mirror, no limit: %v %d", cc.Cmd, hc.Memory)
	}
}

// ensureSlot through a fake Docker API and fake exec: a missing slot is
// created, started, waited for and its work directory chowned.
func TestEnsureSlotCreatesMissingSlot(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 0, Status: statusStarting})
	ops := &fakeSlotOps{}
	p.sops = ops
	for _, v := range slotVolumes(0) {
		p.chowned[v] = true // volumes already prepared
	}
	var created struct{ Cmd []string }
	api := newDockerAPI(t, p, map[string]http.HandlerFunc{
		"GET /networks/pitlane-slot-0":        reply(map[string]string{"Id": "n0"}),
		"GET /containers/pitlane-slot-0/json": replyStatus(http.StatusNotFound),
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Error(err)
			}
			reply(map[string]string{"Id": "c0"})(w, r)
		},
		"POST /containers/pitlane-slot-0/start": replyStatus(http.StatusNoContent),
	})
	if err := p.ensureSlot(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := api.called(); !slices.Contains(got, "POST /containers/create") || got[len(got)-1] != "POST /containers/pitlane-slot-0/start" {
		t.Fatalf("docker calls: %v", got)
	}
	if !slices.Contains(created.Cmd, "--max-concurrent-downloads=8") {
		t.Fatalf("dind command line: %v", created.Cmd)
	}
	if !slices.Equal(ops.scripts, []string{"docker info >/dev/null 2>&1", "chown 1001:1001 /home/runner/_work"}) {
		t.Fatalf("scripts: %q", ops.scripts)
	}
}
