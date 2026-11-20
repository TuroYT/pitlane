package pool

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An adopted runner can sit in a slot far above the configured count (the
// slot count was lowered, or the label is odd); the dashboard must still
// show it, and in index order.
func TestSlotsListsHighIndexes(t *testing.T) {
	p := newTestPool("owner",
		SlotState{Index: 0, Status: "ready"},
		SlotState{Index: 1, Status: "ready"},
		SlotState{Index: 57, Status: "busy", Owner: "acme"},
		SlotState{Index: 20, Status: "busy", Owner: "acme"},
	)
	var got []int
	for _, s := range p.Slots() {
		got = append(got, s.Index)
	}
	want := []int{0, 1, 20, 57}
	if len(got) != len(want) {
		t.Fatalf("Slots lists %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Slots lists %v, want %v", got, want)
		}
	}
}

// slotUpRoutes answers Docker for a slot whose daemon already runs with the
// current definition (its network and container exist).
func slotUpRoutes(routes map[string]http.HandlerFunc, i int) {
	name := slotName(i)
	routes["GET /networks/"+name] = reply(map[string]string{"Id": "net"})
	routes["GET /containers/"+name+"/json"] = reply(map[string]any{
		"Id": "c", "Config": map[string]any{"Labels": map[string]string{LabelSpec: slotSpecVersion}}, "State": map[string]any{"Running": true},
	})
}

// slotPool is a pool whose slot volumes are already prepared and whose slot
// execs succeed.
func slotPool(n int, slots ...SlotState) (*Pool, *fakeSlotOps) {
	p := newTestPool("owner", slots...)
	ops := &fakeSlotOps{}
	p.sops = ops
	for i := range n {
		for _, v := range slotVolumes(i) {
			p.chowned[v] = true
		}
	}
	return p, ops
}

func TestStartSlot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		dirty      bool
		down       bool // Docker cannot reach the slot
		wantStatus string
		wantErr    string
	}{
		{"ready", statusStarting, false, false, statusReady, ""},
		{"a busy slot stays busy", statusBusy, false, false, statusBusy, ""},
		{"a dirty slot waits for its rebuild", statusStarting, true, false, statusError, "slot must be rebuilt before reuse"},
		{"docker fails", statusStarting, false, true, statusError, "create network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := slotPool(1, SlotState{Index: 0, Status: tc.status})
			p.slots[0].dirty = tc.dirty
			routes := map[string]http.HandlerFunc{}
			if tc.down {
				routes["GET /networks/pitlane-slot-0"] = replyStatus(http.StatusNotFound)
				routes["POST /networks/create"] = replyStatus(http.StatusInternalServerError)
			} else {
				slotUpRoutes(routes, 0)
			}
			newDockerAPI(t, p, routes)
			p.startSlot(context.Background(), 0)
			s := p.slots[0]
			if s.Status != tc.wantStatus || !strings.Contains(s.Error, tc.wantErr) {
				t.Fatalf("status %q error %q, want %q %q", s.Status, s.Error, tc.wantStatus, tc.wantErr)
			}
			if tc.wantStatus == statusReady {
				select {
				case <-p.Released():
				default:
					t.Fatal("a slot that came up must wake the scaler")
				}
			}
		})
	}
}

func TestTakeFailed(t *testing.T) {
	p := newTestPool("owner",
		SlotState{Index: 0, Status: statusError},
		SlotState{Index: 1, Status: statusError},
		SlotState{Index: 2, Status: statusError},
		SlotState{Index: 3, Status: statusReady},
	)
	p.slots[1].dirty = true
	p.slots[2].retire = true
	failed, dirty := p.takeFailed()
	if len(failed) != 1 || failed[0] != 0 || len(dirty) != 1 || dirty[0] != 1 {
		t.Fatalf("failed %v dirty %v, want [0] [1]", failed, dirty)
	}
	if p.slots[0].Status != statusStarting || p.slots[1].Status != statusStarting {
		t.Fatal("slots taken for a retry must be starting, so they are not taken twice")
	}
	if p.slots[2].Status != statusError || p.slots[3].Status != statusReady {
		t.Fatal("a retired slot or a ready one must be left alone")
	}
	if f, d := p.takeFailed(); len(f)+len(d) != 0 {
		t.Fatalf("taken twice: %v %v", f, d)
	}
}

func TestRebuildSlot(t *testing.T) {
	t.Run("everything removed then started again empty", func(t *testing.T) {
		p, _ := slotPool(1, SlotState{Index: 0, Status: statusStarting, Owner: "acme", TargetID: 4})
		p.slots[0].dirty = true
		p.cfg.StateFile = filepath.Join(t.TempDir(), "slots.json")
		routes := map[string]http.HandlerFunc{"DELETE /containers/pitlane-slot-0": replyStatus(http.StatusNoContent)}
		for _, v := range slotVolumes(0) {
			routes["DELETE /volumes/"+v] = replyStatus(http.StatusNoContent)
		}
		routes["GET /containers/json"] = reply([]any{}) // containers still using its volumes
		slotUpRoutes(routes, 0)
		// The volumes are created again and prepared for the runner user.
		for _, v := range slotVolumes(0) {
			routes["GET /volumes/"+v] = reply(map[string]string{"Name": v})
		}
		routes["POST /containers/create"] = reply(map[string]string{"Id": "init"})
		routes["POST /containers/init/start"] = replyStatus(http.StatusNoContent)
		routes["POST /containers/init/wait"] = reply(map[string]int{"StatusCode": 0})
		api := newDockerAPI(t, p, routes)
		p.rebuildSlot(context.Background(), 0)
		s := p.slots[0]
		if s.Status != statusReady || s.dirty || s.Owner != "" || s.TargetID != 0 {
			t.Fatalf("rebuilt slot: %+v dirty=%v", s.SlotState, s.dirty)
		}
		if n := strings.Count(strings.Join(api.called(), ","), "DELETE /volumes/"); n != 3 {
			t.Fatalf("removed %d volumes, want its 3: %v", n, api.called())
		}
		b, _ := os.ReadFile(p.cfg.StateFile)
		if !strings.Contains(string(b), `"owner":""`) {
			t.Fatalf("the emptied slot must be persisted: %s", b)
		}
	})
	t.Run("a volume that cannot be removed keeps the previous owner", func(t *testing.T) {
		p, _ := slotPool(1, SlotState{Index: 0, Status: statusStarting, Owner: "acme", TargetID: 4})
		p.externals[0] = "sha"
		newDockerAPI(t, p, map[string]http.HandlerFunc{
			"GET /containers/json":                     reply([]any{}),
			"DELETE /containers/pitlane-slot-0":        replyStatus(http.StatusNotFound),
			"DELETE /volumes/pitlane-slot-0-docker":    replyStatus(http.StatusNoContent),
			"DELETE /volumes/pitlane-slot-0-work":      replyStatus(http.StatusConflict),
			"DELETE /volumes/pitlane-slot-0-externals": replyStatus(http.StatusNoContent),
		})
		p.rebuildSlot(context.Background(), 0)
		s := p.slots[0]
		if s.Status != statusError || !strings.HasPrefix(s.Error, "rebuild: ") || !s.dirty || s.Owner != "acme" {
			t.Fatalf("failed rebuild: %+v dirty=%v", s.SlotState, s.dirty)
		}
		if p.externals[0] != "sha" {
			t.Fatal("a slot that is not clean keeps its externals record")
		}
	})
	t.Run("a container that cannot be removed", func(t *testing.T) {
		p, _ := slotPool(1, SlotState{Index: 0, Status: statusStarting, Owner: "acme"})
		newDockerAPI(t, p, map[string]http.HandlerFunc{"DELETE /containers/pitlane-slot-0": replyStatus(http.StatusInternalServerError)})
		err := p.resetSlot(context.Background(), 0)
		if err == nil || !strings.Contains(err.Error(), "remove dind") || !p.slots[0].dirty || p.slots[0].Owner != "acme" {
			t.Fatalf("err %v slot %+v", err, p.slots[0])
		}
	})
}

func TestRemoveSlotKeepsVolumesAndForgetsState(t *testing.T) {
	p, _ := slotPool(2, SlotState{Index: 0, Status: statusReady, Owner: "acme"}, SlotState{Index: 5, Status: statusReady, Owner: "acme"})
	p.cfg.StateFile = filepath.Join(t.TempDir(), "slots.json")
	p.externals[5] = "sha"
	var forced string
	api := newDockerAPI(t, p, map[string]http.HandlerFunc{"DELETE /containers/pitlane-slot-5": func(w http.ResponseWriter, r *http.Request) {
		forced = r.URL.RawQuery
		replyStatus(http.StatusNoContent)(w, r)
	}})
	p.removeSlot(context.Background(), 5)
	if _, ok := p.slots[5]; ok {
		t.Fatal("slot still listed")
	}
	if _, ok := p.externals[5]; ok {
		t.Fatal("externals record kept")
	}
	if strings.Contains(forced, "v=1") || !strings.Contains(forced, "force=1") {
		t.Fatalf("the container must be force-removed without its volumes (its cache is kept): %q", forced)
	}
	for _, c := range api.called() {
		if strings.HasPrefix(c, "DELETE /volumes") {
			t.Fatalf("removed a volume of a retired slot: %s", c)
		}
	}
	// The slot is beyond the configured count (2): a gone slot is forgotten.
	b, _ := os.ReadFile(p.cfg.StateFile)
	if strings.Contains(string(b), `"5"`) {
		t.Fatalf("a retired slot that is gone must be forgotten: %s", b)
	}
}

func TestDropSlotCache(t *testing.T) {
	p := newTestPool("owner")
	routes := map[string]http.HandlerFunc{"GET /containers/json": reply([]any{})}
	for _, v := range slotVolumes(1) {
		routes["DELETE /volumes/"+v] = replyStatus(http.StatusNotFound) // already gone
	}
	api := newDockerAPI(t, p, routes)
	if err := p.DropSlotCache(context.Background(), 1); err != nil || len(api.called()) != 6 { // per volume: its users, then the volume
		t.Fatalf("missing volumes are not an error: %v %v", err, api.called())
	}
	routes["DELETE /volumes/pitlane-slot-1-docker"] = replyStatus(http.StatusInternalServerError)
	if err := p.DropSlotCache(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "pitlane-slot-1-docker") {
		t.Fatalf("a volume that cannot be removed must be reported: %v", err)
	}
}

func TestFreeSlotsAndMarkRunner(t *testing.T) {
	p := newTestPool("owner",
		SlotState{Index: 0, Status: statusReady},
		SlotState{Index: 1, Status: statusReady},
		SlotState{Index: 2, Status: statusBusy},
		SlotState{Index: 3, Status: statusReady},
	)
	p.slots[3].retire = true
	if n := p.FreeSlots(); n != 2 {
		t.Fatalf("free slots %d, want 2 (a retired one does not count)", n)
	}
	p.MarkRunner(2, "pitlane-abc")
	p.MarkRunner(99, "ignored")
	if p.slots[2].Runner != "pitlane-abc" || len(p.slots) != 4 {
		t.Fatalf("runner not recorded: %+v", p.slots[2].SlotState)
	}
	p.setStatus(2, statusReady, "")
	if p.slots[2].Runner != "" {
		t.Fatal("a slot that becomes ready has no runner")
	}
}

// A retired slot is shown as retiring; a leased one carries its memory.
func TestSlotsReportsRetiringAndMemory(t *testing.T) {
	p := newTestPool("owner", SlotState{Index: 0, Status: statusBusy}, SlotState{Index: 1, Status: statusReady})
	p.slots[1].retire = true
	p.reserved = map[int]int64{0: 3 << 30}
	got := p.Slots()
	if got[1].Status != statusRetiring {
		t.Fatalf("retired slot shown as %q", got[1].Status)
	}
	if got[0].Predicted != 3<<30 || got[1].Predicted != 0 {
		t.Fatalf("predicted %d %d", got[0].Predicted, got[1].Predicted)
	}
}
