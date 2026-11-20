package pool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// The QEMU handlers Pitlane registered (their exact binfmt_misc definitions)
// are persisted next to the slot state, in Pitlane's data directory, which
// jobs cannot reach. binfmt_misc registrations outlive Pitlane, so without
// this a restart would forget which handlers are its own: the first audit
// would remove all of them as unexpected and register them again.

// binfmtStateFile is where the trusted handlers are kept ("" = nowhere).
func (p *Pool) binfmtStateFile() string {
	f := p.Config().StateFile
	if f == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(f), "binfmt.json")
}

// saveBinfmt persists the trusted handlers; nil removes the file, so that
// after a restart nothing is trusted (fail closed).
func (p *Pool) saveBinfmt(base map[string]string) {
	file := p.binfmtStateFile()
	if file == "" {
		return
	}
	if len(base) > 0 {
		if err := writeJSONFile(file, base); err == nil {
			return
		} else {
			p.log.Error("cannot persist the QEMU handlers: they will be registered again after a restart", "file", file, "error", err)
		}
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		p.log.Error("cannot remove the QEMU handler state: remove it before restarting Pitlane", "file", file, "error", err)
	}
}

// restorableBinfmt returns the persisted handlers when they can be trusted
// again: each one is a handler Pitlane registers (trustedQEMU) and is still
// registered with exactly the same definition. Any difference (a handler
// changed or removed while Pitlane was stopped) trusts nothing, and the
// handlers are then removed and registered again as on a first start.
// Handlers that are present but not persisted are not trusted: the audit
// removes them.
func restorableBinfmt(persisted, current map[string]string) (map[string]string, bool) {
	if len(persisted) == 0 || len(trustedQEMU(persisted)) != len(persisted) {
		return nil, false
	}
	for name, def := range persisted {
		if current[name] != def {
			return nil, false
		}
	}
	return persisted, true
}

// restoreBinfmt runs at Start, before any slot is handed out.
func (p *Pool) restoreBinfmt(ctx context.Context) {
	file := p.binfmtStateFile()
	if file == "" || p.Config().NoQEMU {
		p.saveBinfmt(nil)
		return
	}
	dir := p.host().binfmtDir
	if !binfmtMounted(dir) {
		return // not mounted: ensureQEMU mounts it and registers
	}
	b, err := os.ReadFile(file) //nolint:gosec // G304: path derived from Pitlane's own data directory
	if err != nil {
		return
	}
	var persisted map[string]string
	if err := json.Unmarshal(b, &persisted); err != nil {
		p.log.Warn("QEMU handler state unreadable, registering the handlers again", "file", file, "error", err)
		return
	}
	base, ok := restorableBinfmt(persisted, readBinfmt(dir))
	if !ok {
		p.log.Warn("QEMU handlers changed while Pitlane was stopped: removing and registering them again")
		return
	}
	p.mu.Lock()
	p.binfmt, p.qemuWant = base, false
	p.mu.Unlock()
	p.log.Info("QEMU handlers registered by the previous run kept", "handlers", len(base))
	p.auditBinfmt(ctx) // anything else registered meanwhile goes now
}
