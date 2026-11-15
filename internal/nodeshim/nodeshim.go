// Package nodeshim makes the Pitlane binary act as the runner's Node.js when
// it is installed as `externals/node*/bin/node` (busybox style, see
// docs/design/actions-cache.md). The real binary is kept next to it as
// `node.pitlane`. Runner.Worker starts every JavaScript action with that path
// and sets ACTIONS_CACHE_SERVICE_V2 and the cache URLs itself, after the
// container's environment, so this is the only place where they can be
// changed without patching the runner.
//
// It only acts when Pitlane gave the runner container a cache URL
// (PITLANE_CACHE_URL, absent for strict jobs, when the cache is off, and
// inside job containers, which do not inherit it): @actions/cache then uses
// its v1 protocol against Pitlane, artifacts keep using ACTIONS_RESULTS_URL
// (GitHub). In every other case Node gets its environment unchanged.
//
// The environment is passed on byte for byte apart from those two variables.
// A shell wrapper cannot do that: POSIX shells drop the variables whose names
// are not identifiers, such as the `INPUT_FETCH-DEPTH` inputs of actions.
//
// The package only imports the standard library and does its work in init,
// so that the shim runs before the rest of Pitlane initialises.
package nodeshim

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Version changes whenever the shim's behaviour does, so that slots refresh
// their copy of the runner externals.
const Version = "2"

// Name is the file name the shim is installed under; Real is the suffix of
// the real binary next to it.
const (
	Name = "node"
	Real = ".pitlane"
)

func init() {
	exe, err := os.Executable() // /proc/self/exe: symlinks (nodejs -> node) resolved
	if err != nil {
		exe = os.Args[0] // no /proc: the path the runner used
	}
	if filepath.Base(exe) != Name {
		return
	}
	real := exe + Real
	err = syscall.Exec(real, os.Args, Env(os.Environ())) //nolint:gosec // G204: the real Node next to the shim, with the runner's own arguments
	fmt.Fprintf(os.Stderr, "pitlane node shim: run %s: %v\n", real, err)
	if os.IsNotExist(err) {
		os.Exit(127)
	}
	os.Exit(126)
}

// Env returns the environment for the real Node: env unchanged (order,
// duplicates and names that are not identifiers included), except that when
// PITLANE_CACHE_URL and GITHUB_REPOSITORY are both set and non-empty,
// ACTIONS_CACHE_URL points at Pitlane's cache for that repository and
// ACTIONS_CACHE_SERVICE_V2 is removed.
func Env(env []string) []string {
	cacheURL, repo := lookup(env, "PITLANE_CACHE_URL"), lookup(env, "GITHUB_REPOSITORY")
	if cacheURL == "" || repo == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "ACTIONS_CACHE_URL=") || strings.HasPrefix(kv, "ACTIONS_CACHE_SERVICE_V2=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "ACTIONS_CACHE_URL="+cacheURL+repo+"/")
}

// lookup returns the first value of key, like os.Getenv.
func lookup(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}
