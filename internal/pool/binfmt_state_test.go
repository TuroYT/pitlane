package pool

import (
	"path/filepath"
	"testing"
)

func qemuDef(name, magic string) string {
	return "enabled\ninterpreter /usr/bin/" + name + "\nflags: POCF\noffset 0\nmagic " + magic + "\nmask ffff\n"
}

func TestRestorableBinfmt(t *testing.T) {
	arm := qemuDef("qemu-aarch64", "7f454c460201010000000000000000000200b700")
	if hostMachine() == "b700" {
		arm = qemuDef("qemu-aarch64", "7f454c460201010000000000000000000200f300") // not native on this host
	}
	riscv := qemuDef("qemu-riscv64", "7f454c4602010100000000000000000002001234")
	persisted := map[string]string{"qemu-aarch64": arm, "qemu-riscv64": riscv}

	if base, ok := restorableBinfmt(persisted, map[string]string{"qemu-aarch64": arm, "qemu-riscv64": riscv, "evil": "x"}); !ok || len(base) != 2 {
		t.Fatalf("unchanged handlers must be kept (extras are left to the audit): %v %v", base, ok)
	}
	if _, ok := restorableBinfmt(persisted, map[string]string{"qemu-aarch64": arm}); ok {
		t.Fatal("a handler removed while stopped must trust nothing")
	}
	if _, ok := restorableBinfmt(persisted, map[string]string{"qemu-aarch64": arm, "qemu-riscv64": riscv + "x"}); ok {
		t.Fatal("a handler modified while stopped must trust nothing")
	}
	bad := map[string]string{"qemu-aarch64": arm, "evil": qemuDef("evil", "1234")}
	if _, ok := restorableBinfmt(bad, bad); ok {
		t.Fatal("a persisted handler Pitlane would not register must trust nothing")
	}
	if _, ok := restorableBinfmt(nil, map[string]string{}); ok {
		t.Fatal("nothing persisted: nothing restored")
	}
}

func TestSaveBinfmt(t *testing.T) {
	p := newTestPool("owner")
	p.cfg.StateFile = filepath.Join(t.TempDir(), "slots.json")
	file := p.binfmtStateFile()
	p.saveBinfmt(map[string]string{"qemu-aarch64": "def"})
	if got := listDir(t, filepath.Dir(file)); len(got) != 1 || got[0] != "binfmt.json" {
		t.Fatalf("state not written: %v", got)
	}
	p.saveBinfmt(nil)
	if got := listDir(t, filepath.Dir(file)); len(got) != 0 {
		t.Fatalf("tampered handlers must not be trusted after a restart: %v", got)
	}
}
