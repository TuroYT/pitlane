package pool

import "regexp"

// A buildx builder of the docker-container driver (what
// docker/setup-buildx-action creates) runs BuildKit in a container of the
// slot's daemon and keeps its cache in the volume
// buildx_buildkit_<builder>0_state ("0" is the index of its only node). The
// container is removed after every job, but a builder created again with the
// same name mounts the same volume, so keeping the volume keeps the cache.
// setup-buildx-action itself removes the volume at the end of the job unless
// the workflow sets keep-state: true (or cleanup: false).
//
// Both patterns are POSIX extended regular expressions that RE2 reads the
// same way: the release script greps with them, Go matches with them.
const (
	// buildkitStateERE matches the state volume of any buildx builder.
	buildkitStateERE = `^buildx_buildkit_.+0_state$`
	// throwawayBuildkitStateERE matches the state volume of a builder named
	// by setup-buildx-action when the workflow gives no name: builder-<uuid
	// v4>. A new uuid is drawn for every job, so the volume is never reused.
	throwawayBuildkitStateERE = `^buildx_buildkit_builder-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}0_state$`
)

var (
	buildkitStateRE          = regexp.MustCompile(buildkitStateERE)
	throwawayBuildkitStateRE = regexp.MustCompile(throwawayBuildkitStateERE)
)

// reusableBuildkitVolume reports whether the volume name is the BuildKit
// state of a builder with a chosen name, which a later job can reuse.
func reusableBuildkitVolume(name string) bool {
	return buildkitStateRE.MatchString(name) && !throwawayBuildkitStateRE.MatchString(name)
}

// removeVolumesScript removes every volume of the slot's daemon except the
// BuildKit state of named builders (see reusableBuildkitVolume). It must run
// once the containers are gone: docker volume rm refuses a volume in use.
const removeVolumesScript = `v=$(docker volume ls -q)
{ printf '%s\n' "$v" | grep -Ev '` + buildkitStateERE + `'
  printf '%s\n' "$v" | grep -E '` + throwawayBuildkitStateERE + `'
} | xargs -r docker volume rm >/dev/null 2>&1`

// removeBuildkitStateScript removes the BuildKit state of every buildx
// builder, named or not. Disk reclaim runs it on idle slots, where no builder
// container is left.
const removeBuildkitStateScript = `docker volume ls -q | grep -E '` + buildkitStateERE + `' | xargs -r docker volume rm >/dev/null`
