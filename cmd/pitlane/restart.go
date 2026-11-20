package main

import (
	"errors"
	"log/slog"
	"path/filepath"
)

// errRestart asks main to start the (just updated) binary again.
var errRestart = errors.New("restart requested")

// reexec replaces the process with the binary now at its path, keeping the
// PID (systemd keeps tracking it) and the arguments. Busy runners are Docker
// containers, not children, so they keep running and are adopted on start.
// It only returns when exec failed, with the exit code that makes the
// service manager start Pitlane again.
func (s system) reexec(log *slog.Logger, args []string) int {
	exe, err := s.executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err == nil {
		log.Info("restarting into the updated binary", "path", exe)
		// The path is our own binary, just replaced by a verified release.
		err = s.exec(exe, args, s.environ())
	}
	log.Error("re-exec failed, exiting so the service manager restarts Pitlane", "error", err)
	return 1
}
