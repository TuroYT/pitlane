package nodeshim

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// The test binary doubles as the real Node: run as `node.pitlane` it prints
// the arguments and environment it received (see installShim).
func TestMain(m *testing.M) {
	if b := filepath.Base(os.Args[0]); b == Name || b == "nodejs" {
		_ = json.NewEncoder(os.Stdout).Encode(map[string][]string{"args": os.Args, "env": os.Environ()})
		os.Exit(3) // checked by the caller: the exit code goes through the shim
	}
	os.Exit(m.Run())
}

func TestEnvKeepsEverythingWithoutPitlaneCache(t *testing.T) {
	env := []string{"INPUT_INCLUDE-HIDDEN-FILES=false", "INPUT_A.B=1", "EMPTY=", "EQ=a=b==c", "NL=line1\nline2",
		"ACTIONS_CACHE_SERVICE_V2=True", "ACTIONS_CACHE_URL=https://github/", "GITHUB_REPOSITORY=acme/api", "DUP=1", "DUP=2", "noequals"}
	for _, extra := range [][]string{nil, {"PITLANE_CACHE_URL="}} {
		in := append(slices.Clone(env), extra...)
		if got := Env(slices.Clone(in)); !slices.Equal(got, in) {
			t.Fatalf("PITLANE_CACHE_URL unset or empty changed the environment:\n got %q\nwant %q", got, in)
		}
	}
	// No repository: no URL to build, nothing changes either.
	in := []string{"PITLANE_CACHE_URL=http://x/", "ACTIONS_CACHE_SERVICE_V2=True"}
	if got := Env(slices.Clone(in)); !slices.Equal(got, in) {
		t.Fatalf("no GITHUB_REPOSITORY: %q", got)
	}
}

func TestEnvRedirectsTheCacheOnly(t *testing.T) {
	in := []string{"INPUT_INCLUDE-HIDDEN-FILES=false", "ACTIONS_CACHE_SERVICE_V2=True", "INPUT_A.B=1", "EMPTY=",
		"ACTIONS_CACHE_URL=https://github/", "EQ=a=b==c", "NL=line1\nline2", "PITLANE_CACHE_URL=http://x/t/",
		"GITHUB_REPOSITORY=acme/api", "ACTIONS_RESULTS_URL=https://results/", "ACTIONS_CACHE_URL_X=keep", "DUP=1", "DUP=2", "noequals"}
	want := []string{"INPUT_INCLUDE-HIDDEN-FILES=false", "INPUT_A.B=1", "EMPTY=", "EQ=a=b==c", "NL=line1\nline2",
		"PITLANE_CACHE_URL=http://x/t/", "GITHUB_REPOSITORY=acme/api", "ACTIONS_RESULTS_URL=https://results/",
		"ACTIONS_CACHE_URL_X=keep", "DUP=1", "DUP=2", "noequals", "ACTIONS_CACHE_URL=http://x/t/acme/api/"}
	if got := Env(in); !slices.Equal(got, want) {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

// installShim installs the test binary as dir/node (the shim) in front of
// itself as dir/node.pitlane (the "real" Node, see TestMain).
func installShim(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "node24", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, Name), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(dir, Name+Real)); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, Name)
}

// The shim runs before anything else and execs the real Node with the same
// arguments (argv[0] included) and exit code, and the environment byte for
// byte apart from the cache variables.
func TestShimExec(t *testing.T) {
	node := installShim(t)
	if err := os.Symlink(Name, filepath.Join(filepath.Dir(node), "nodejs")); err != nil {
		t.Fatal(err)
	}
	base := []string{"INPUT_INCLUDE-HIDDEN-FILES=false", "INPUT_FETCH-DEPTH=0", "INPUT_A.B=1", "EMPTY=", "EQ=a=b==c",
		"NL=line1\nline2", "ACTIONS_CACHE_SERVICE_V2=True", "ACTIONS_CACHE_URL=https://github/", "GITHUB_REPOSITORY=acme/api"}
	for _, tc := range []struct {
		name, path string
		extra      []string
		want       []string
	}{
		{"without Pitlane cache", node, nil, base},
		{"with Pitlane cache", node, []string{"PITLANE_CACHE_URL=http://x/"}, []string{"INPUT_INCLUDE-HIDDEN-FILES=false",
			"INPUT_FETCH-DEPTH=0", "INPUT_A.B=1", "EMPTY=", "EQ=a=b==c", "NL=line1\nline2", "GITHUB_REPOSITORY=acme/api",
			"PITLANE_CACHE_URL=http://x/", "ACTIONS_CACHE_URL=http://x/acme/api/"}},
		{"through a symlink", filepath.Join(filepath.Dir(node), "nodejs"), nil, base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{tc.path, "-e", "a b", "", "--x=y\nz"}
			cmd := &exec.Cmd{Path: tc.path, Args: args, Env: append(slices.Clone(base), tc.extra...)}
			out, err := cmd.Output()
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
				t.Fatalf("exit: %v (%s)", err, out)
			}
			var got map[string][]string
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if !slices.Equal(got["args"], args) {
				t.Fatalf("args:\n got %q\nwant %q", got["args"], args)
			}
			if !slices.Equal(got["env"], tc.want) {
				t.Fatalf("env:\n got %q\nwant %q", got["env"], tc.want)
			}
		})
	}
}

// Without the real binary, the shim fails loudly instead of running Pitlane.
func TestShimWithoutRealNode(t *testing.T) {
	node := installShim(t)
	if err := os.Remove(node + Real); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--version").CombinedOutput() //nolint:gosec // G204: test
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 127 {
		t.Fatalf("exit: %v (%s)", err, out)
	}
}
