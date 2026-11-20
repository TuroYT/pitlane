package pool

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestDockerSlotOpsRemoveVolume(t *testing.T) {
	for _, tc := range []struct {
		status  int
		wantErr bool
	}{{http.StatusNoContent, false}, {http.StatusNotFound, false}, {http.StatusInternalServerError, true}} {
		p := newTestPool("owner")
		var force string
		newDockerAPI(t, p, map[string]http.HandlerFunc{"DELETE /volumes/pitlane-strict-a-cache": func(w http.ResponseWriter, r *http.Request) {
			force = r.URL.Query().Get("force")
			replyStatus(tc.status)(w, r)
		}})
		err := dockerSlotOps{p}.removeVolume(context.Background(), "pitlane-strict-a-cache")
		if (err != nil) != tc.wantErr {
			t.Errorf("status %d: err %v", tc.status, err)
		}
		if force != "1" && force != "true" {
			t.Errorf("a strict job's volume must be removed forcibly, force=%q", force)
		}
	}
}

func TestDockerSlotOpsImageID(t *testing.T) {
	p := newTestPool("owner")
	newDockerAPI(t, p, map[string]http.HandlerFunc{
		"GET /images/runner:1/json": reply(map[string]string{"Id": "sha256:abc"}),
		"GET /images/gone:1/json":   replyStatus(http.StatusNotFound),
	})
	ops := dockerSlotOps{p}
	if id, err := ops.imageID(context.Background(), "runner:1"); err != nil || id != "sha256:abc" {
		t.Fatalf("%q %v", id, err)
	}
	if _, err := ops.imageID(context.Background(), "gone:1"); err == nil || !strings.Contains(err.Error(), "inspect image gone:1") {
		t.Fatalf("err %v", err)
	}
}

func TestDockerSlotOpsRecreate(t *testing.T) {
	p, _ := slotPool(1, SlotState{Index: 0, Status: statusReady})
	removed := false
	routes := map[string]http.HandlerFunc{
		"DELETE /containers/pitlane-slot-0": func(w http.ResponseWriter, r *http.Request) {
			removed = true
			replyStatus(http.StatusNotFound)(w, r) // already gone: not a failure
		},
	}
	slotUpRoutes(routes, 0)
	api := newDockerAPI(t, p, routes)
	if err := (dockerSlotOps{p}).recreate(context.Background(), 0); err != nil || !removed {
		t.Fatalf("err %v removed %v calls %v", err, removed, api.called())
	}
}

func TestDockerSlotOpsWipe(t *testing.T) {
	p, _ := slotPool(1, SlotState{Index: 0, Status: statusBusy, Owner: "acme"})
	routes := map[string]http.HandlerFunc{"DELETE /containers/pitlane-slot-0": replyStatus(http.StatusInternalServerError)}
	newDockerAPI(t, p, routes)
	if err := (dockerSlotOps{p}).wipe(context.Background(), 0); err == nil || !p.slots[0].dirty || p.slots[0].Owner != "acme" {
		t.Fatalf("a wipe that cannot remove the daemon must fail and leave the slot dirty: %v", err)
	}
}

func TestCopyExternals(t *testing.T) {
	routes := func() map[string]http.HandlerFunc {
		return map[string]http.HandlerFunc{
			"POST /containers/create":    reply(map[string]string{"Id": "x1"}),
			"PUT /containers/x1/archive": replyStatus(http.StatusOK),
			"POST /containers/x1/start":  replyStatus(http.StatusNoContent),
			"POST /containers/x1/wait":   reply(map[string]int{"StatusCode": 0}),
			"DELETE /containers/x1":      replyStatus(http.StatusNoContent),
		}
	}
	newPool := func(t *testing.T) (*Pool, *bytes.Buffer) {
		p := newTestPool("owner")
		p.env = testHost(t)
		write(t, p.env.selfExe, "#!pitlane")
		var logs bytes.Buffer
		p.log = slog.New(slog.NewTextHandler(&logs, nil))
		return p, &logs
	}
	ctx := context.Background()
	t.Run("copied, helper container removed", func(t *testing.T) {
		p, _ := newPool(t)
		api := newDockerAPI(t, p, routes())
		if err := (dockerSlotOps{p}).copyExternals(ctx, 1, "runner:1"); err != nil {
			t.Fatal(err)
		}
		if got := api.called(); got[len(got)-1] != "DELETE /containers/x1" || !slices.Contains(got, "PUT /containers/x1/archive") {
			t.Fatalf("calls %v", got)
		}
	})
	t.Run("without the shim the copy still happens", func(t *testing.T) {
		p, logs := newPool(t)
		p.env.selfExe, p.env.executable = "/nonexistent", func() (string, error) { return "", errors.New("no executable") }
		api := newDockerAPI(t, p, routes())
		if err := (dockerSlotOps{p}).copyExternals(ctx, 1, "runner:1"); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(api.called(), "PUT /containers/x1/archive") || !strings.Contains(logs.String(), "cannot install the Node shim") {
			t.Fatalf("calls %v logs %s", api.called(), logs)
		}
	})
	for name, tc := range map[string]struct {
		mod  func(map[string]http.HandlerFunc)
		want string
	}{
		"create fails": {func(r map[string]http.HandlerFunc) {
			r["POST /containers/create"] = replyStatus(http.StatusInternalServerError)
		}, "copy runner externals into slot 1"},
		"start fails": {func(r map[string]http.HandlerFunc) {
			r["POST /containers/x1/start"] = replyStatus(http.StatusInternalServerError)
		}, "copy runner externals into slot 1"},
		"copy fails": {func(r map[string]http.HandlerFunc) {
			r["POST /containers/x1/wait"] = reply(map[string]int{"StatusCode": 3})
		}, "exit code 3"},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := newPool(t)
			r := routes()
			tc.mod(r)
			newDockerAPI(t, p, r)
			if err := (dockerSlotOps{p}).copyExternals(ctx, 1, "runner:1"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}

func TestOpsDefaultsToDocker(t *testing.T) {
	p := newTestPool("owner")
	if _, ok := p.ops().(dockerSlotOps); !ok {
		t.Fatalf("ops() = %T", p.ops())
	}
	p.sops = &fakeSlotOps{}
	if _, ok := p.ops().(*fakeSlotOps); !ok {
		t.Fatal("a fake must replace the real ops")
	}
}

func TestDockerSlotOpsExec(t *testing.T) {
	p := newTestPool("owner")
	routes, f := execRoutes(t, func(_, _ string) (string, int) { return "ok", 0 }, "pitlane-slot-1")
	newDockerAPI(t, p, routes)
	out, code, err := dockerSlotOps{p}.exec(context.Background(), "pitlane-slot-1", "true")
	if err != nil || code != 0 || out != "ok" || len(f.ran()) != 1 {
		t.Fatalf("%q %d %v", out, code, err)
	}
}
