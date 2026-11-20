package update

import "testing"

func TestParseVersion(t *testing.T) {
	for _, s := range []string{"1.2.3", "v0.2.0", "1.0.0-rc.1", "1.0.0-alpha+001", "2.338.2"} {
		if _, err := ParseVersion(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"", "dev", "1.2", "01.2.3", "1.2.3-01", "1.2.3.4", "abc1234", "v1.2.3 "} {
		if s == "v1.2.3 " {
			continue // surrounding spaces are trimmed
		}
		if _, err := ParseVersion(s); err == nil {
			t.Errorf("%q parsed", s)
		}
	}
}

func TestCompareOrder(t *testing.T) {
	// semver.org §11 example, ascending.
	order := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2",
		"1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0", "10.0.0",
	}
	for i := 0; i < len(order); i++ {
		for j := 0; j < len(order); j++ {
			a, _ := ParseVersion(order[i])
			b, _ := ParseVersion(order[j])
			want := cmpInt(i, j)
			if got := Compare(a, b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
	a, _ := ParseVersion("v1.2.3+build.7")
	b, _ := ParseVersion("1.2.3")
	if Compare(a, b) != 0 {
		t.Error("build metadata and the v prefix must not matter")
	}
}

func TestReleaseVersion(t *testing.T) {
	for s, want := range map[string]bool{
		"0.2.0":                   true,
		"v0.2.0":                  true,
		"0.3.0-rc.1":              true,
		"dev":                     false,
		"a72ddb9":                 false,
		"v0.2.0-3-ga72ddb9":       false,
		"v0.2.0-dirty":            false,
		"v0.2.0-3-ga72ddb9-dirty": false,
		"":                        false,
	} {
		if _, ok := ReleaseVersion(s); ok != want {
			t.Errorf("ReleaseVersion(%q) = %v, want %v", s, ok, want)
		}
	}
}

func TestVersionString(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"1.2.3":              "1.2.3",
		"v1.2.3":             "1.2.3",
		"1.2.3-rc.1":         "1.2.3-rc.1",
		"1.2.3+build.5":      "1.2.3+build.5",
		"v1.2.3-rc.1+build5": "1.2.3-rc.1+build5",
		"0.0.0":              "0.0.0",
	} {
		v, err := ParseVersion(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String(); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
		if again, err := ParseVersion(v.String()); err != nil || Compare(v, again) != 0 {
			t.Errorf("%s does not round-trip: %v", in, err)
		}
	}
}

func TestIsPrerelease(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{"1.0.0": false, "1.0.0+b": false, "1.0.0-rc.1": true, "1.0.0-0": true} {
		v, _ := ParseVersion(in)
		if v.IsPrerelease() != want {
			t.Errorf("%s: %v", in, !want)
		}
	}
}

func TestComparePrereleaseIdentifiers(t *testing.T) {
	t.Parallel()
	tests := []struct{ a, b string }{
		{"1.0.0-1", "1.0.0-alpha"},       // numeric before alphanumeric
		{"1.0.0-alpha", "1.0.0-alpha.1"}, // a shorter set is lower
		{"1.0.0-2", "1.0.0-10"},          // numeric, not lexical
		{"1.0.0-rc.1", "1.0.0"},
		{"1.9.9", "2.0.0-rc.1"},
		{"1.0.0-a-b", "1.0.0-b"}, // hyphen inside an identifier
	}
	for _, tc := range tests {
		a, _ := ParseVersion(tc.a)
		b, _ := ParseVersion(tc.b)
		if Compare(a, b) != -1 || Compare(b, a) != 1 || Compare(a, a) != 0 {
			t.Errorf("%s < %s: %d %d", tc.a, tc.b, Compare(a, b), Compare(b, a))
		}
	}
}

func TestParseVersionBounds(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"1.2.3-", "1.2.3+", "1.2.3-rc..1", "1.2.3-rc.1+", "1.2.-3", "v", "v1", "1.2.3-0123", "1.2.3 rc"} {
		if _, err := ParseVersion(s); err == nil {
			t.Errorf("%q parsed", s)
		}
	}
	if v, err := ParseVersion("  v1.2.3\n"); err != nil || v.Major != 1 {
		t.Errorf("surrounding whitespace: %v %v", v, err)
	}
	if v, err := ParseVersion("1.2.3-0"); err != nil || len(v.Pre) != 1 {
		t.Errorf("a lone zero identifier is valid: %v %v", v, err)
	}
}
