package scaler

import "time"

// Time sources of the manager. Production uses the real clock; tests set
// now, after and tick on the Manager to run without waiting.

// clock returns the current time.
func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// wait returns a channel that fires after d.
func (m *Manager) wait(d time.Duration) <-chan time.Time {
	if m.after != nil {
		return m.after(d)
	}
	return time.After(d)
}

// ticker returns a channel that fires every d, and the function that stops it.
func (m *Manager) ticker(d time.Duration) (<-chan time.Time, func()) {
	if m.tick != nil {
		return m.tick(d)
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// sleep blocks for d.
func (m *Manager) sleep(d time.Duration) { <-m.wait(d) }
