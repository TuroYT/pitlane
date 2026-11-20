package pool

import (
	"encoding/json"
	"errors"
	"os"
)

// persistedSlot is what survives a Pitlane restart. It lives in Pitlane's
// data directory, which jobs cannot reach, so it can be trusted.
type persistedSlot struct {
	Owner     string `json:"owner"`
	TargetID  int64  `json:"targetId"`
	Externals string `json:"externals"` // runner image ID copied into the externals volume
	LastUsed  int64  `json:"lastUsed,omitempty"`
}

// loadState restores the persisted slot ownership, before the pool is
// shared. An unreadable file restores nothing (fail closed): every slot then
// has an unknown owner and is wiped before its first job.
func (p *Pool) loadState() {
	b, err := os.ReadFile(p.cfg.StateFile)
	if err != nil {
		return
	}
	if err := json.Unmarshal(b, &p.restored); err != nil {
		p.log.Error("slot state unreadable, every slot will be wiped before its first job", "file", p.cfg.StateFile, "error", err)
		p.restored = map[int]persistedSlot{}
	}
	if pruneRetired(p.restored, p.cfg.Slots, p.slots) > 0 {
		p.saveLocked()
	}
	for i, st := range p.restored {
		if st.Externals != "" {
			p.externals[i] = st.Externals
		}
	}
}

// pruneRetired drops the persisted state of slots beyond the configured
// count that no longer exist (left by a former, higher slot count) and
// returns how many were dropped. Forgetting a slot is fail closed: if it
// comes back, its owner is unknown and it is wiped before its first job.
func pruneRetired(state map[int]persistedSlot, slots int, live map[int]*slot) int {
	n := 0
	for i := range state {
		if i < slots || live[i] != nil {
			continue
		}
		delete(state, i)
		n++
	}
	return n
}

// saveLocked persists slot ownership. Callers hold p.mu.
func (p *Pool) saveLocked() {
	if p.cfg.StateFile == "" {
		return
	}
	// Start from the previous state so slots being retired keep their last
	// owner; those beyond the slot count are dropped once gone (pruneRetired).
	out := map[int]persistedSlot{}
	for i, st := range p.restored {
		out[i] = st
	}
	for i, s := range p.slots {
		out[i] = persistedSlot{Owner: s.Owner, TargetID: s.TargetID, Externals: p.externals[i], LastUsed: s.LastUsed}
	}
	pruneRetired(out, p.cfg.Slots, p.slots)
	p.restored = out
	if err := writeJSONFile(p.cfg.StateFile, out); err != nil {
		// Fail closed: a stale file could restore an owner that no longer
		// matches the slot's data, and skip its wipe. Without a file every
		// slot restores with an unknown owner and is wiped.
		p.log.Error("cannot persist slot ownership, removing the state file: slots will be wiped after a restart", "file", p.cfg.StateFile, "error", err)
		if rerr := os.Remove(p.cfg.StateFile); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			p.log.Error("cannot remove the stale slot state file: remove it before restarting Pitlane", "file", p.cfg.StateFile, "error", rerr)
		}
	}
}

// writeJSONFile atomically replaces a 0600 JSON file.
func writeJSONFile(file string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := file + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, file)
}
