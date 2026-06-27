package version

import "testing"

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
