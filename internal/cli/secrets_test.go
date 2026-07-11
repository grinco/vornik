package cli

import "testing"

// TestNormalizeDuration — operators write "30d"; Go's ParseDuration has
// no day unit, so scan-history rewrites it to hours. Non-day inputs pass
// through untouched. Backlog item 2.
func TestNormalizeDuration(t *testing.T) {
	cases := map[string]string{
		"30d":  "720h",
		"1d":   "24h",
		"720h": "720h", // already valid — passthrough
		"45m":  "45m",  // passthrough
		"xd":   "xd",   // non-numeric day → passthrough (ParseDuration errors later)
		"":     "",
	}
	for in, want := range cases {
		if got := normalizeDuration(in); got != want {
			t.Errorf("normalizeDuration(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePositiveInt(t *testing.T) {
	if n, err := parsePositiveInt("30"); err != nil || n != 30 {
		t.Errorf("parsePositiveInt(30) = (%d, %v)", n, err)
	}
	if _, err := parsePositiveInt(""); err == nil {
		t.Error("empty string should error")
	}
	if _, err := parsePositiveInt("3x"); err == nil {
		t.Error("non-numeric should error")
	}
}
