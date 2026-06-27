package apikey

import (
	"strings"
	"testing"
)

// TestGenerate_ShapeAndPrefix — every freshly-minted key starts
// with the canonical prefix, embeds the project ID verbatim, and
// carries a random tail long enough to defeat brute force. Pinning
// the shape stops a future refactor from accidentally producing
// tokens that no longer match Parse's expectations.
func TestGenerate_ShapeAndPrefix(t *testing.T) {
	key, err := Generate("assistant")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// New keys embed the non-reversible ShortProjectTag, NOT the raw
	// project name (enumeration hardening). The raw project must not
	// appear in the key.
	if strings.Contains(key, "assistant") {
		t.Errorf("key %q leaks the raw project name", key)
	}
	wantPrefix := "sk-vornik-" + ShortProjectTag("assistant") + "."
	if !strings.HasPrefix(key, wantPrefix) {
		t.Errorf("key %q does not have prefix %q", key, wantPrefix)
	}
	random := strings.TrimPrefix(key, wantPrefix)
	// base64.RawURLEncoding of 24 bytes = 32 chars (no padding).
	if len(random) != 32 {
		t.Errorf("random tail length = %d, want 32", len(random))
	}
}

// TestGenerate_UniquePerCall — two consecutive Generate calls
// against the same project must produce distinct keys (different
// random tails). A regression here would mean keys collide and
// revocation can't disambiguate.
func TestGenerate_UniquePerCall(t *testing.T) {
	a, _ := Generate("p")
	b, _ := Generate("p")
	if a == b {
		t.Errorf("two Generate calls produced identical key %q", a)
	}
}

// TestGenerate_RejectsEmptyProject — empty project string is
// nonsensical and must error rather than mint a malformed key.
func TestGenerate_RejectsEmptyProject(t *testing.T) {
	if _, err := Generate(""); err != ErrEmptyProject {
		t.Errorf("Generate(\"\") err = %v, want ErrEmptyProject", err)
	}
}

// TestGenerate_AllowsHyphenInProject — project IDs elsewhere in
// vornik commonly use kebab-case. API keys must support those IDs
// rather than rejecting valid projects at creation time.
func TestGenerate_AllowsHyphenInProject(t *testing.T) {
	key, err := Generate("foo-bar")
	if err != nil {
		t.Fatalf("Generate(\"foo-bar\"): %v", err)
	}
	pid, _, err := Parse(key)
	if err != nil {
		t.Fatalf("Parse(%q): %v", key, err)
	}
	// pid is the tag, not the raw project, but it must resolve back to
	// the project via MatchesProject.
	if !MatchesProject(pid, "foo-bar") {
		t.Errorf("parsed segment %q does not match project foo-bar", pid)
	}
}

// TestMatchesProject_NewAndLegacy pins the dual-format cross-check: the
// new tag prefix and the legacy raw-projectID prefix both resolve to the
// project, and a wrong project does not.
func TestMatchesProject_NewAndLegacy(t *testing.T) {
	tag := ShortProjectTag("assistant")
	if !MatchesProject(tag, "assistant") {
		t.Errorf("new tag %q should match assistant", tag)
	}
	// Legacy keys embedded the raw project ID.
	if !MatchesProject("assistant", "assistant") {
		t.Error("legacy raw-projectID segment should match")
	}
	if MatchesProject(tag, "other-project") {
		t.Error("tag must not match a different project")
	}
	if MatchesProject("", "assistant") {
		t.Error("empty segment must not match")
	}
}

// TestShortProjectTag_StableAndOpaque — the tag is deterministic, 12 hex
// chars, and does not contain the raw project name.
func TestShortProjectTag_StableAndOpaque(t *testing.T) {
	a := ShortProjectTag("trading")
	b := ShortProjectTag("trading")
	if a != b {
		t.Errorf("tag not stable: %q vs %q", a, b)
	}
	if len(a) != 12 {
		t.Errorf("tag length = %d, want 12", len(a))
	}
	if strings.Contains(a, "trading") {
		t.Errorf("tag %q leaks the project name", a)
	}
	if ShortProjectTag("trading") == ShortProjectTag("research") {
		t.Error("distinct projects must produce distinct tags")
	}
}

// TestParse_RoundTripsGeneratedKeys — the contract Auth depends on:
// what Generate produces, Parse must split back into the same
// (project, random) pair.
func TestParse_RoundTripsGeneratedKeys(t *testing.T) {
	for _, project := range []string{"assistant", "snake", "test_project", "foo-bar", "a"} {
		key, err := Generate(project)
		if err != nil {
			t.Fatalf("Generate(%q): %v", project, err)
		}
		pid, rnd, err := Parse(key)
		if err != nil {
			t.Errorf("Parse(%q): %v", key, err)
			continue
		}
		// Parse returns the embedded tag; it must resolve to the project.
		if !MatchesProject(pid, project) {
			t.Errorf("Parse(%q) segment = %q, does not match project %q", key, pid, project)
		}
		if rnd == "" {
			t.Errorf("Parse(%q) random is empty", key)
		}
	}
}

// TestParse_RejectsMalformed — each branch of the prefix /
// project / random validation must report the specific error so
// operator-facing diagnostics can distinguish "wrong product" from
// "truncated key".
func TestParse_RejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want error
	}{
		{"empty", "", ErrMalformed},
		{"no-prefix", "Bearer abc", ErrWrongPrefix},
		{"prefix-only", "sk-vornik-", ErrMalformed},
		{"empty-project", "sk-vornik-.abc", ErrEmptyProject},
		{"empty-random", "sk-vornik-foo.", ErrEmptyRandom},
		{"prefix-with-trailing-hyphen-only", "sk-vornik-foo", ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Parse(tc.key)
			if err != tc.want {
				t.Errorf("Parse(%q) err = %v, want %v", tc.key, err, tc.want)
			}
		})
	}
}

// TestHash_DeterministicAndDistinct — Hash must produce identical
// digests for identical input (otherwise the DB lookup misses) and
// distinct digests for distinct input (otherwise two keys collide
// into one row). These properties are what gives AuthMiddleware
// its security: the timing-oracle defence reduces to a single
// 64-char hex equality.
func TestHash_DeterministicAndDistinct(t *testing.T) {
	a := Hash("sk-vornik-foo-aaa")
	b := Hash("sk-vornik-foo-aaa")
	c := Hash("sk-vornik-foo-aab")
	if a != b {
		t.Errorf("Hash is non-deterministic: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("Hash collision between distinct keys: %q", a)
	}
	// sha256 hex is always 64 chars.
	if len(a) != 64 {
		t.Errorf("Hash length = %d, want 64 (sha256 hex)", len(a))
	}
}

// TestDisplayPrefix_TruncatesAtFixedLength — the UI uses
// DisplayPrefix to render `sk-vornik-Ab` in tables without exposing
// the secret. Truncation must be stable across calls.
func TestDisplayPrefix_TruncatesAtFixedLength(t *testing.T) {
	key := "sk-vornik-assistant-abcdefghijklmnopqrstuvwxyzABCDEF"
	got := DisplayPrefix(key)
	if got != key[:PrefixDisplayLen] {
		t.Errorf("DisplayPrefix(%q) = %q, want %q", key, got, key[:PrefixDisplayLen])
	}
	// Short inputs (shorter than the cap) return verbatim — degrade
	// gracefully rather than panic on slice OOB.
	short := "sk"
	if got := DisplayPrefix(short); got != short {
		t.Errorf("DisplayPrefix(%q) = %q, want %q", short, got, short)
	}
}
