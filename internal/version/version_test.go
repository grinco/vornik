package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestNormalizeEdition(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{EditionEnterprise, EditionEnterprise},
		{"enterprise", "enterprise"},
		{EditionCommunity, EditionCommunity},
		{"community", "community"},
		{"", EditionCommunity},           // empty → fail-safe to community
		{"garbage", EditionCommunity},    // unknown → fail-safe to community
		{"Enterprise", EditionCommunity}, // case-sensitive: only exact match counts
	}
	for _, c := range cases {
		if got := NormalizeEdition(c.in); got != c.want {
			t.Errorf("NormalizeEdition(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if DefaultEdition != EditionCommunity {
		t.Errorf("DefaultEdition = %q, want %q", DefaultEdition, EditionCommunity)
	}
}

func TestBuildLine(t *testing.T) {
	// Normalizes the edition and renders a stable one-liner for any program.
	got := BuildLine("vornik", "1.2.3", "2026-06-24", "enterprise")
	want := "vornik 1.2.3 (built 2026-06-24, enterprise edition)"
	if got != want {
		t.Errorf("BuildLine enterprise = %q, want %q", got, want)
	}
	// Unknown/unstamped edition is normalized inside BuildLine.
	got = BuildLine("vornikctl version", "dev", "unknown", "")
	want = "vornikctl version dev (built unknown, community edition)"
	if got != want {
		t.Errorf("BuildLine default = %q, want %q", got, want)
	}
}

// Regression, 2026-08-15. A daemon built with only the edition ldflag (and no
// -X main.Version) reported version.Default — then "2026.4.5" — in the UI footer, four months
// after that release shipped. It was the third occurrence of the same shape
// (see also 2026-07-30 and 2026-08-03), and each earlier fix relied on the
// operator remembering to build through the Makefile.
//
// The property under test is that an UNSTAMPED build identifies itself from the
// VCS metadata Go embeds unconditionally, so no discipline is required for the
// reported version to be truthful.
func TestVCSFromSettings(t *testing.T) {
	const fullRev = "eae1a72aa6b5b8fa1531a70e2b82eaacb7380e18"

	t.Run("clean tree yields short revision and a plain date", func(t *testing.T) {
		rev, date, dirty, ok := vcsFromSettings([]debug.BuildSetting{
			{Key: "vcs.revision", Value: fullRev},
			{Key: "vcs.time", Value: "2026-08-15T08:53:49Z"},
			{Key: "vcs.modified", Value: "false"},
		})
		if !ok {
			t.Fatal("VCS metadata present but not recognised")
		}
		if rev != "eae1a72aa6b5" {
			t.Errorf("rev = %q, want the 12-char short form", rev)
		}
		if date != "2026-08-15" {
			t.Errorf("date = %q, want the date without the time component", date)
		}
		if dirty {
			t.Error("vcs.modified=false must not report dirty")
		}
	})

	t.Run("modified tree is flagged", func(t *testing.T) {
		_, _, dirty, ok := vcsFromSettings([]debug.BuildSetting{
			{Key: "vcs.revision", Value: fullRev},
			{Key: "vcs.modified", Value: "true"},
		})
		if !ok || !dirty {
			t.Errorf("ok=%v dirty=%v; a modified tree must be reported", ok, dirty)
		}
	})

	t.Run("a time without a revision is not usable", func(t *testing.T) {
		// Nothing traceable: refuse rather than invent a version from a date.
		if _, _, _, ok := vcsFromSettings([]debug.BuildSetting{
			{Key: "vcs.time", Value: "2026-08-15T08:53:49Z"},
		}); ok {
			t.Error("a build with no revision must not count as identifiable")
		}
	})

	t.Run("no VCS settings at all", func(t *testing.T) {
		if _, _, _, ok := vcsFromSettings(nil); ok {
			t.Error("an archive build has nothing to report")
		}
	})
}

func TestResolve(t *testing.T) {
	t.Run("a stamped release passes through untouched", func(t *testing.T) {
		// The Makefile path. Whatever VCS data the test binary carries must not
		// override an explicit -X main.Version.
		ver, date := Resolve("2026.8.4", "2026-08-15")
		if ver != "2026.8.4" || date != "2026-08-15" {
			t.Errorf("Resolve stamped = (%q, %q), want the injected values", ver, date)
		}
	})

	t.Run("an unstamped build never reports a release-shaped version", func(t *testing.T) {
		// This is the whole point: the old code returned "2026.4.5" here, which
		// is indistinguishable in a footer from a correct build.
		ver, _ := Resolve(Default, UnknownBuildDate)
		if IsStamped(ver) {
			t.Fatalf("unstamped build reported %q, which reads as a release", ver)
		}
		if ver == "2026.4.5" {
			t.Fatal("the stale constant is back")
		}
		// `go test` builds from the source tree, so VCS data is present and the
		// version must name the commit rather than fall back to the constant.
		if _, _, _, ok := vcsInfo(); ok && !strings.HasPrefix(ver, devPrefix) {
			t.Errorf("version = %q, want the %q + commit form", ver, devPrefix)
		}
	})

	t.Run("empty injection is treated as unstamped, not as a version", func(t *testing.T) {
		ver, _ := Resolve("", "")
		if ver == "" || IsStamped(ver) {
			t.Errorf("Resolve(\"\") = %q; want a non-empty, non-release marker", ver)
		}
	})
}

func TestIsStamped(t *testing.T) {
	cases := map[string]bool{
		"2026.8.4":                            true,
		"2026.8.4-3-gabcdef":                  true,
		Default:                               false,
		"":                                    false,
		"   ":                                 false,
		devPrefix + "eae1a72aa6b5":            false,
		devPrefix + "eae1a72aa6b5" + ".dirty": false,
	}
	for in, want := range cases {
		if got := IsStamped(in); got != want {
			t.Errorf("IsStamped(%q) = %v, want %v", in, got, want)
		}
	}
}
