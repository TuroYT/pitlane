// Package update checks for new Pitlane releases and new GitHub runner
// versions, installs Pitlane releases in place (systemd / plain binary
// installs) and keeps the runner image fresh.
package update

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is a parsed semantic version (https://semver.org), with an optional
// leading "v" as in Git tags.
type Version struct {
	Major, Minor, Patch int
	Pre                 []string // pre-release identifiers ("rc", "1")
	Build               string
}

var semverRe = regexp.MustCompile(`^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

// ParseVersion parses "1.2.3", "v1.2.3-rc.1+build".
func ParseVersion(s string) (Version, error) {
	m := semverRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, fmt.Errorf("not a semantic version: %q", s)
	}
	var v Version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		v.Pre = strings.Split(m[4], ".")
		for _, id := range v.Pre {
			if isNum(id) && len(id) > 1 && id[0] == '0' {
				return Version{}, fmt.Errorf("not a semantic version: %q (leading zero)", s)
			}
		}
	}
	v.Build = m[5]
	return v, nil
}

// IsPrerelease reports whether the version has pre-release identifiers.
func (v Version) IsPrerelease() bool { return len(v.Pre) > 0 }

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}

func isNum(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Compare returns -1, 0 or 1 following semver precedence (build metadata is
// ignored; a pre-release sorts before its release).
func Compare(a, b Version) int {
	if c := cmpInt(a.Major, b.Major); c != 0 {
		return c
	}
	if c := cmpInt(a.Minor, b.Minor); c != 0 {
		return c
	}
	if c := cmpInt(a.Patch, b.Patch); c != 0 {
		return c
	}
	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0:
		return 1
	case len(b.Pre) == 0:
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		x, y := a.Pre[i], b.Pre[i]
		if x == y {
			continue
		}
		xn, yn := isNum(x), isNum(y)
		switch {
		case xn && yn:
			xi, _ := strconv.Atoi(x)
			yi, _ := strconv.Atoi(y)
			return cmpInt(xi, yi)
		case xn:
			return -1 // numeric identifiers sort before alphanumeric ones
		case yn:
			return 1
		case x < y:
			return -1
		default:
			return 1
		}
	}
	return cmpInt(len(a.Pre), len(b.Pre))
}

// gitDescribeRe matches what `git describe` appends to a tag ("3-gabc1234").
var gitDescribeRe = regexp.MustCompile(`^\d+-g[0-9a-f]{4,}$`)

// ReleaseVersion parses the version Pitlane was built with and reports
// whether it is a published release. Development builds ("dev", a bare
// commit, `git describe` output such as v0.2.0-3-gabc1234 or -dirty) are not:
// they cannot be compared meaningfully with releases.
func ReleaseVersion(s string) (Version, bool) {
	v, err := ParseVersion(s)
	if err != nil {
		return Version{}, false
	}
	pre := strings.Join(v.Pre, ".")
	if strings.Contains(pre, "dirty") || gitDescribeRe.MatchString(strings.TrimSuffix(pre, "-dirty")) {
		return Version{}, false
	}
	return v, true
}
