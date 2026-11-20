package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A QEMU handler Pitlane could register on this host: never the host's own
// architecture.
func testQEMU(name string) string {
	magic := "7f454c460201010000000000000000000200b700"
	if hostMachine() == "b700" {
		magic = "7f454c460201010000000000000000000200f300"
	}
	return qemuDef(name, magic)
}

// binfmtPool is a pool whose binfmt_misc is a mounted temporary directory.
func binfmtPool(t *testing.T) *Pool {
	t.Helper()
	p := newTestPool("owner")
	p.env = testHost(t)
	p.cfg.StateFile = filepath.Join(t.TempDir(), "slots.json")
	write(t, filepath.Join(p.env.binfmtDir, "register"), "")
	return p
}

func handlerState(t *testing.T, p *Pool, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(p.env.binfmtDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAuditBinfmt(t *testing.T) {
	def := testQEMU("qemu-aarch64")
	t.Run("not mounted: nothing to audit", func(t *testing.T) {
		p := newTestPool("owner")
		p.env = testHost(t)
		p.auditBinfmt(context.Background())
	})
	t.Run("an unexpected handler is removed", func(t *testing.T) {
		p := binfmtPool(t)
		p.binfmt = map[string]string{"qemu-aarch64": def}
		write(t, filepath.Join(p.env.binfmtDir, "qemu-aarch64"), def)
		write(t, filepath.Join(p.env.binfmtDir, "evil"), "enabled\n")
		p.auditBinfmt(context.Background())
		if got := handlerState(t, p, "evil"); got != "-1" {
			t.Fatalf("the handler registered by a job must be unregistered, holds %q", got)
		}
		if got := handlerState(t, p, "qemu-aarch64"); got != def || p.qemuWant || p.binfmt == nil {
			t.Fatalf("Pitlane's own handler must be left alone: %q want=%v", got, p.qemuWant)
		}
	})
	t.Run("a modified QEMU handler removes them all until the pool is idle", func(t *testing.T) {
		p := binfmtPool(t)
		p.binfmt = map[string]string{"qemu-aarch64": def, "qemu-riscv64": testQEMU("qemu-riscv64")}
		p.saveBinfmt(p.binfmt)
		write(t, filepath.Join(p.env.binfmtDir, "qemu-aarch64"), def+"tampered")
		write(t, filepath.Join(p.env.binfmtDir, "qemu-riscv64"), testQEMU("qemu-riscv64"))
		p.auditBinfmt(context.Background())
		for _, n := range []string{"qemu-aarch64", "qemu-riscv64"} {
			if got := handlerState(t, p, n); got != "-1" {
				t.Fatalf("%s holds %q: every handler must go, a job could have rewritten one", n, got)
			}
		}
		if p.binfmt != nil || !p.qemuWant {
			t.Fatalf("binfmt %v qemuWant %v: they must be registered again when idle", p.binfmt, p.qemuWant)
		}
		if _, err := os.Stat(p.binfmtStateFile()); err == nil {
			t.Fatal("handlers that are no longer trusted must not be persisted")
		}
	})
	t.Run("a handler that vanished counts as tampering", func(t *testing.T) {
		p := binfmtPool(t)
		p.binfmt = map[string]string{"qemu-aarch64": def}
		p.auditBinfmt(context.Background())
		if p.binfmt != nil || !p.qemuWant {
			t.Fatalf("binfmt %v qemuWant %v", p.binfmt, p.qemuWant)
		}
	})
	t.Run("QEMU off: nothing is trusted", func(t *testing.T) {
		p := binfmtPool(t)
		p.cfg.NoQEMU = true
		p.binfmt = map[string]string{"qemu-aarch64": def}
		write(t, filepath.Join(p.env.binfmtDir, "qemu-aarch64"), def)
		p.auditBinfmt(context.Background())
		if got := handlerState(t, p, "qemu-aarch64"); got != "-1" || p.qemuWant {
			t.Fatalf("handler %q qemuWant %v", got, p.qemuWant)
		}
	})
}

func TestReadAndClearBinfmt(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "register"), "")
	write(t, filepath.Join(dir, "status"), "enabled")
	write(t, filepath.Join(dir, "qemu-arm"), "def")
	if got := readBinfmt(dir); len(got) != 1 || got["qemu-arm"] != "def" {
		t.Fatalf("handlers %v: register and status are not handlers", got)
	}
	clearBinfmt(dir)
	if b, _ := os.ReadFile(filepath.Join(dir, "qemu-arm")); string(b) != "-1" {
		t.Fatalf("handler not unregistered: %q", b)
	}
	if got := readBinfmt(filepath.Join(dir, "missing")); len(got) != 0 {
		t.Fatalf("unreadable directory: %v", got)
	}
}

func TestMaybeRegisterQEMU(t *testing.T) {
	idle := func(t *testing.T, runners []map[string]string) (*Pool, *dockerAPI) {
		p := binfmtPool(t)
		p.slots[0] = &slot{SlotState: SlotState{Index: 0, Status: statusReady}}
		p.qemuWant = true
		list := []map[string]any{}
		for _, l := range runners {
			list = append(list, map[string]any{"Id": "r", "Labels": l})
		}
		api := newDockerAPI(t, p, map[string]http.HandlerFunc{
			"GET /containers/json":                 reply(list),
			"GET /images/" + binfmtImage + "/json": reply(map[string]string{"Id": "x"}),
			"POST /containers/create":              reply(map[string]string{"Id": "b1"}),
			"POST /containers/b1/start": func(w http.ResponseWriter, _ *http.Request) {
				// The helper container registers the emulators.
				write(t, filepath.Join(p.env.binfmtDir, "qemu-aarch64"), testQEMU("qemu-aarch64"))
				write(t, filepath.Join(p.env.binfmtDir, "evil"), "enabled\n")
				w.WriteHeader(http.StatusNoContent)
			},
			"POST /containers/b1/wait": reply(map[string]int{"StatusCode": 0}),
			"DELETE /containers/b1":    replyStatus(http.StatusNoContent),
		})
		return p, api
	}
	t.Run("registers what Pitlane trusts when no job runs", func(t *testing.T) {
		p, _ := idle(t, nil)
		p.maybeRegisterQEMU(context.Background())
		if len(p.binfmt) != 1 || p.binfmt["qemu-aarch64"] == "" || p.qemuWant || p.qemuBusy {
			t.Fatalf("binfmt %v qemuWant %v qemuBusy %v", p.binfmt, p.qemuWant, p.qemuBusy)
		}
		b, _ := os.ReadFile(p.binfmtStateFile())
		var saved map[string]string
		if err := json.Unmarshal(b, &saved); err != nil || len(saved) != 1 {
			t.Fatalf("persisted handlers %q %v", b, err)
		}
		select {
		case <-p.Released():
		default:
			t.Fatal("slots handed out again: the scaler must be woken up")
		}
	})
	t.Run("a runner container that is alive postpones it", func(t *testing.T) {
		p, api := idle(t, []map[string]string{{LabelRole: "runner"}})
		p.maybeRegisterQEMU(context.Background())
		if !p.qemuWant || p.qemuBusy || len(p.binfmt) != 0 {
			t.Fatalf("qemuWant %v qemuBusy %v binfmt %v", p.qemuWant, p.qemuBusy, p.binfmt)
		}
		for _, c := range api.called() {
			if strings.HasPrefix(c, "POST /containers/create") {
				t.Fatal("registered while a runner is alive")
			}
		}
	})
	t.Run("a busy slot postpones it", func(t *testing.T) {
		p, api := idle(t, nil)
		p.slots[0].Status = statusBusy
		p.maybeRegisterQEMU(context.Background())
		if !p.qemuWant || len(api.called()) != 0 {
			t.Fatalf("qemuWant %v calls %v", p.qemuWant, api.called())
		}
	})
	t.Run("nothing wanted, or QEMU off", func(t *testing.T) {
		p, api := idle(t, nil)
		p.qemuWant = false
		p.maybeRegisterQEMU(context.Background())
		p.qemuWant, p.cfg.NoQEMU = true, true
		p.maybeRegisterQEMU(context.Background())
		if len(api.called()) != 0 {
			t.Fatalf("calls %v", api.called())
		}
	})
	t.Run("the listing fails: retried later", func(t *testing.T) {
		p, _ := idle(t, nil)
		newDockerAPI(t, p, map[string]http.HandlerFunc{"GET /containers/json": replyStatus(http.StatusInternalServerError)})
		p.maybeRegisterQEMU(context.Background())
		if !p.qemuWant || p.qemuBusy {
			t.Fatalf("qemuWant %v qemuBusy %v", p.qemuWant, p.qemuBusy)
		}
	})
}

func TestEnsureQEMUFailures(t *testing.T) {
	base := func(t *testing.T) (*Pool, map[string]http.HandlerFunc) {
		p := binfmtPool(t)
		return p, map[string]http.HandlerFunc{
			"GET /images/" + binfmtImage + "/json": reply(map[string]string{"Id": "x"}),
			"POST /containers/create":              reply(map[string]string{"Id": "b1"}),
			"POST /containers/b1/start":            replyStatus(http.StatusNoContent),
			"POST /containers/b1/wait":             reply(map[string]int{"StatusCode": 0}),
			"DELETE /containers/b1":                replyStatus(http.StatusNoContent),
		}
	}
	for name, tc := range map[string]struct {
		mod  func(map[string]http.HandlerFunc)
		off  bool
		want string // calls that must not happen
	}{
		"QEMU off": {off: true, want: "POST /containers/create"},
		"image unavailable": {mod: func(r map[string]http.HandlerFunc) {
			r["GET /images/"+binfmtImage+"/json"] = replyStatus(http.StatusNotFound)
			r["POST /images/create"] = replyStatus(http.StatusInternalServerError)
		}, want: "POST /containers/create"},
		"create fails": {mod: func(r map[string]http.HandlerFunc) {
			r["POST /containers/create"] = replyStatus(http.StatusInternalServerError)
		}, want: "POST /containers/b1/start"},
		"helper fails": {mod: func(r map[string]http.HandlerFunc) {
			r["POST /containers/b1/wait"] = reply(map[string]int{"StatusCode": 1})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			p, routes := base(t)
			p.cfg.NoQEMU = tc.off
			if tc.mod != nil {
				tc.mod(routes)
			}
			api := newDockerAPI(t, p, routes)
			p.ensureQEMU(context.Background())
			if len(p.binfmt) != 0 {
				t.Fatalf("handlers trusted after a failed registration: %v", p.binfmt)
			}
			if tc.want != "" && strings.Contains(strings.Join(api.called(), ","), tc.want) {
				t.Fatalf("calls %v", api.called())
			}
		})
	}
}

func TestRestoreBinfmtStates(t *testing.T) {
	def := testQEMU("qemu-aarch64")
	t.Run("QEMU off forgets what was persisted", func(t *testing.T) {
		p := binfmtPool(t)
		p.cfg.NoQEMU = true
		p.saveBinfmt(map[string]string{"qemu-aarch64": def})
		p.restoreBinfmt(context.Background())
		if _, err := os.Stat(p.binfmtStateFile()); err == nil {
			t.Fatal("persisted handlers kept with QEMU off")
		}
	})
	t.Run("no state file", func(t *testing.T) {
		p := binfmtPool(t)
		p.cfg.StateFile = ""
		p.qemuWant = true
		p.restoreBinfmt(context.Background())
		if !p.qemuWant {
			t.Fatal("without persisted state the handlers are registered")
		}
	})
	t.Run("not mounted", func(t *testing.T) {
		p := newTestPool("owner")
		p.env = testHost(t)
		p.cfg.StateFile = filepath.Join(t.TempDir(), "slots.json")
		p.qemuWant = true
		p.restoreBinfmt(context.Background())
		if !p.qemuWant || p.binfmt != nil {
			t.Fatalf("qemuWant %v binfmt %v", p.qemuWant, p.binfmt)
		}
	})
	t.Run("nothing persisted, or unreadable, or changed", func(t *testing.T) {
		for name, content := range map[string]*string{"nothing persisted": nil, "unreadable": ptr("{not json"), "changed": ptr(`{"qemu-aarch64":"other"}`)} {
			p := binfmtPool(t)
			write(t, filepath.Join(p.env.binfmtDir, "qemu-aarch64"), def)
			if content != nil {
				write(t, p.binfmtStateFile(), *content)
			}
			p.qemuWant = true
			p.restoreBinfmt(context.Background())
			if !p.qemuWant || p.binfmt != nil {
				t.Fatalf("%s: qemuWant %v binfmt %v: nothing may be trusted", name, p.qemuWant, p.binfmt)
			}
		}
	})
	t.Run("unchanged handlers are kept and the rest audited", func(t *testing.T) {
		p := binfmtPool(t)
		write(t, filepath.Join(p.env.binfmtDir, "qemu-aarch64"), def)
		write(t, filepath.Join(p.env.binfmtDir, "evil"), "enabled\n")
		b, _ := json.Marshal(map[string]string{"qemu-aarch64": def})
		write(t, p.binfmtStateFile(), string(b))
		p.qemuWant = true
		p.restoreBinfmt(context.Background())
		if p.qemuWant || len(p.binfmt) != 1 {
			t.Fatalf("qemuWant %v binfmt %v", p.qemuWant, p.binfmt)
		}
		if got := handlerState(t, p, "evil"); got != "-1" {
			t.Fatalf("a handler registered while Pitlane was stopped must go: %q", got)
		}
	})
}

func TestSaveBinfmtWriteFailure(t *testing.T) {
	p := newTestPool("owner")
	p.cfg.StateFile = filepath.Join(t.TempDir(), "missing-dir", "slots.json")
	p.saveBinfmt(map[string]string{"qemu-aarch64": "x"}) // cannot be written: nothing is left behind
	if _, err := os.Stat(p.binfmtStateFile()); err == nil {
		t.Fatal("state file exists")
	}
	p.cfg.StateFile = ""
	p.saveBinfmt(map[string]string{"x": "y"}) // nowhere to save
}
