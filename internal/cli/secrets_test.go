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

// TestAccumulateGlobal — the retention report's two global prunes
// (step_prompts, chat_system_prompts) are pruned BY REFERENCE across the whole
// database, so every per-project pass reports the same set. Summing them in
// preview turned six unreferenced prompt bodies into forty-eight across eight
// projects — caught by running `vornikctl retention` against the live
// deployment on 2026-09-04, hours after the column shipped.
func TestAccumulateGlobal(t *testing.T) {
	// Preview: eight passes, each measuring the same six bodies.
	got := 0
	for i := 0; i < 8; i++ {
		got = accumulateGlobal(got, 6, false)
	}
	if got != 6 {
		t.Errorf("preview over 8 projects = %d, want 6 (the same set measured 8 times)", got)
	}

	// Apply: the first pass removes them, the rest find nothing.
	got = accumulateGlobal(0, 6, true)
	for i := 0; i < 7; i++ {
		got = accumulateGlobal(got, 0, true)
	}
	if got != 6 {
		t.Errorf("apply over 8 projects = %d, want 6 (what was actually removed)", got)
	}
}
