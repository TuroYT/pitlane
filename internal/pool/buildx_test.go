package pool

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const uuidBuilderVolume = "buildx_buildkit_builder-3f1c2a9e-7b4d-4e21-9c0a-5d6e7f8a9b0c0_state"

func TestReusableBuildkitVolume(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"buildx_buildkit_ci0_state", true},
		{"buildx_buildkit_my-builder0_state", true},
		{"buildx_buildkit_builder0_state", true},
		{"buildx_buildkit_builder-ci0_state", true},
		{"buildx_buildkit_builder10_state", true}, // builder "builder1"
		{uuidBuilderVolume, false},
		{"buildx_buildkit_builder-3F1C2A9E-7B4D-4E21-9C0A-5D6E7F8A9B0C0_state", true}, // not what setup-buildx-action draws
		{"buildx_buildkit_ci1_state", false},                                          // not a node name ending in 0
		{"buildx_buildkit_0_state", false},                                            // no builder name
		{"buildx_buildkit_ci0_state_x", false},                                        // not the state volume
		{"pitlane-cache-acme", false},
		{"3c5e9a7d1b2f", false}, // anonymous volume
		{"", false},
	} {
		if got := reusableBuildkitVolume(tc.name); got != tc.want {
			t.Errorf("reusableBuildkitVolume(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReleaseScriptKeepsNamedBuildkitState(t *testing.T) {
	s := releaseScript(20)
	if strings.Contains(s, "volume prune") {
		t.Fatal("releaseScript still prunes every volume")
	}
	if !strings.Contains(s, removeVolumesScript) {
		t.Fatal("releaseScript does not remove the job's volumes")
	}
	rm := strings.Index(s, "docker rm -f")
	vols := strings.Index(s, "docker volume rm")
	if rm < 0 || vols < rm {
		t.Fatal("volumes must be removed after the containers that use them")
	}
	if !strings.HasSuffix(s, `left=$(docker ps -aq) && test -z "$left"`) {
		t.Fatal("releaseScript lost its fail-closed container check")
	}
}

// The volume filters run against a stub docker command: these tests prove
// the shell and grep side, not Docker itself.
func TestRemoveVolumesScriptSparesNamedBuilders(t *testing.T) {
	removed := runVolumeScript(t, removeVolumesScript,
		"buildx_buildkit_ci0_state", uuidBuilderVolume, "3c5e9a7d1b2f", "job-data", "buildx_buildkit_ci0_state_x")
	want := []string{"3c5e9a7d1b2f", uuidBuilderVolume, "buildx_buildkit_ci0_state_x", "job-data"}
	if strings.Join(removed, ",") != strings.Join(want, ",") {
		t.Fatalf("removed %v, want %v", removed, want)
	}
}

func TestRemoveVolumesScriptNoVolumes(t *testing.T) {
	if removed := runVolumeScript(t, removeVolumesScript); len(removed) != 0 {
		t.Fatalf("removed %v with no volume", removed)
	}
}

func TestRemoveBuildkitStateScriptRemovesEveryBuilder(t *testing.T) {
	removed := runVolumeScript(t, removeBuildkitStateScript,
		"buildx_buildkit_ci0_state", uuidBuilderVolume, "job-data")
	want := []string{uuidBuilderVolume, "buildx_buildkit_ci0_state"}
	if strings.Join(removed, ",") != strings.Join(want, ",") {
		t.Fatalf("removed %v, want %v", removed, want)
	}
}

// runVolumeScript runs script with sh and a stub docker listing vols, and
// returns the volumes it removed, sorted (want lists are sorted too).
func runVolumeScript(t *testing.T, script string, vols ...string) []string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh:", err)
	}
	dir := t.TempDir()
	list := filepath.Join(dir, "volumes")
	out := filepath.Join(dir, "removed")
	content := strings.Join(vols, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(list, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
case "$1 $2" in
"volume ls") cat "$STUB_VOLUMES" ;;
"volume rm") shift 2; printf '%s\n' "$@" >>"$STUB_REMOVED" ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_VOLUMES="+list, "STUB_REMOVED="+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	removed := strings.Fields(string(b))
	sort.Strings(removed)
	return removed
}
