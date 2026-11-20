package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// errRestart asks main to start the (just updated) binary again.
var errRestart = errors.New("restart requested")

// reexec replaces the process with the binary now at its path, keeping the
// PID (systemd keeps tracking it) and the arguments. Busy runners are Docker
// containers, not children, so they keep running and are adopted on start.
func reexec(log *slog.Logger) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err == nil {
		log.Info("restarting into the updated binary", "path", exe)
		// The path is our own binary, just replaced by a verified release.
		err = syscall.Exec(exe, os.Args, os.Environ()) //nolint:gosec // G204: own executable
	}
	// Exec failed: exit non-zero so the service manager starts us again.
	log.Error("re-exec failed, exiting so the service manager restarts Pitlane", "error", err)
	os.Exit(1)
}
