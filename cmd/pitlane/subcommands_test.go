package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"version", "--version", "-v"} {
		sys, stdout, stderr := testSystem(t, nil)
		if code := sys.main(context.Background(), []string{"pitlane", arg}); code != 0 {
			t.Errorf("%s: exit code %d", arg, code)
		}
		if got, want := stdout.String(), "pitlane "+version+"\n"; got != want {
			t.Errorf("%s: stdout %q, want %q", arg, got, want)
		}
		if stderr.String() != "" {
			t.Errorf("%s: stderr %q", arg, stderr.String())
		}
	}
}

func TestSetupTokenCommand(t *testing.T) {
	t.Parallel()
	withToken := t.TempDir()
	if err := os.WriteFile(filepath.Join(withToken, "setup-token"), []byte("tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	for _, tc := range []struct {
		name       string
		env        map[string]string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"directory argument", nil, []string{withToken}, 0, "tok-123\n", ""},
		{"PITLANE_DATA", map[string]string{"PITLANE_DATA": withToken}, nil, 0, "tok-123\n", ""},
		{"argument wins over PITLANE_DATA", map[string]string{"PITLANE_DATA": withToken}, []string{empty}, 1, "", "no setup token: setup is already complete"},
		{"no token", nil, []string{empty}, 1, "", "no setup token: setup is already complete (or Pitlane never started)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sys, stdout, stderr := testSystem(t, tc.env)
			code := sys.main(context.Background(), append([]string{"pitlane", "setup-token"}, tc.args...))
			if code != tc.wantCode {
				t.Errorf("exit code %d, want %d", code, tc.wantCode)
			}
			if stdout.String() != tc.wantStdout {
				t.Errorf("stdout %q, want %q", stdout.String(), tc.wantStdout)
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr %q, want %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

// Without an argument the default data directory is read: on a test
// machine it holds no token.
func TestSetupTokenDefaultDirectory(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat(filepath.Join(defaultDataDir, "setup-token")); err == nil {
		t.Skip("this machine has a Pitlane setup token")
	}
	sys, _, stderr := testSystem(t, nil)
	if code := sys.setupToken(nil); code != 1 || !strings.Contains(stderr.String(), "no setup token") {
		t.Errorf("exit code %d, stderr %q", code, stderr.String())
	}
}

func TestCacheForwardCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		args       []string
		cancelled  bool
		wantCode   int
		wantStderr string
	}{
		{"no arguments", nil, false, 2, "usage: pitlane cache-forward <listen> <target>\n"},
		{"one argument", []string{"127.0.0.1:0"}, false, 2, "usage:"},
		{"too many arguments", []string{"a", "b", "c"}, false, 2, "usage:"},
		{"bad listen address", []string{"127.0.0.1:notaport", "127.0.0.1:1"}, false, 1, "msg=fatal"},
		// The signal context is already done: Forward stops at once.
		{"stops on signal", []string{"127.0.0.1:0", "127.0.0.1:1"}, true, 0, "forwarding the actions cache"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sys, stdout, stderr := testSystem(t, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			code := sys.main(ctx, append([]string{"pitlane", "cache-forward"}, tc.args...))
			if code != tc.wantCode {
				t.Errorf("exit code %d, want %d (stderr %q)", code, tc.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr %q, want %q", stderr.String(), tc.wantStderr)
			}
			if stdout.String() != "" {
				t.Errorf("stdout %q", stdout.String())
			}
		})
	}
}
