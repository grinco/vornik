package apikey

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestHash_NeverReturnsRawKey is the core one-way property: the digest
// AuthMiddleware persists must never equal (or contain) the secret it
// was derived from. A regression where Hash echoes its input would
// store plaintext keys in api_keys.key_hash.
func TestHash_NeverReturnsRawKey(t *testing.T) {
	key, err := Generate("trading")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	h := Hash(key)
	if h == key {
		t.Fatal("Hash returned the raw key verbatim")
	}
	if strings.Contains(h, key) {
		t.Errorf("Hash output %q contains the raw key %q", h, key)
	}
	// The random tail is the secret part; it must not survive into the
	// hash either.
	_, random, err := Parse(key)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if strings.Contains(h, random) {
		t.Errorf("Hash output %q leaks the random secret %q", h, random)
	}
}

// TestHash_OutputIsLowercaseHex pins the storage format: a 64-char
// lowercase hex string. AuthMiddleware's constant-time guarantee relies
// on comparing fixed-width hex; an encoding drift (e.g. base64) would
// silently break the single-equality lookup and any DB column width
// assumption.
func TestHash_OutputIsLowercaseHex(t *testing.T) {
	h := Hash("sk-vornik-foo.abc")
	if len(h) != 64 {
		t.Fatalf("Hash length = %d, want 64", len(h))
	}
	if h != strings.ToLower(h) {
		t.Errorf("Hash output is not lowercase: %q", h)
	}
	if _, err := hex.DecodeString(h); err != nil {
		t.Errorf("Hash output %q is not valid hex: %v", h, err)
	}
}

// TestHash_EmptyInputStillHashes — Hash must be total: even the empty
// string produces the canonical 64-char sha256 hex rather than panicking
// or returning "". Defends the auth path against a truncated/blank token
// reaching Hash before validation.
func TestHash_EmptyInputStillHashes(t *testing.T) {
	h := Hash("")
	// sha256("") is a well-known constant.
	const want = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if h != want {
		t.Errorf("Hash(\"\") = %q, want %q", h, want)
	}
}

// TestShortProjectTag_IsHexOnly — the tag is embedded in keys and grepped
// from logs; it must be pure lowercase hex (the first 12 chars of a
// sha256 hex digest). A non-hex char would mean the digest encoding
// changed and the documented "recompute the tag to correlate" workflow
// would break.
func TestShortProjectTag_IsHexOnly(t *testing.T) {
	for _, p := range []string{"assistant", "trading", "foo-bar", "a", "x_y"} {
		tag := ShortProjectTag(p)
		if len(tag) != 12 {
			t.Errorf("tag for %q = %q, length = %d, want 12", p, tag, len(tag))
		}
		if _, err := hex.DecodeString(tag); err != nil {
			t.Errorf("tag %q for project %q is not hex: %v", tag, p, err)
		}
		if tag != strings.ToLower(tag) {
			t.Errorf("tag %q is not lowercase", tag)
		}
	}
}

// TestShortProjectTag_EmptyProject — ShortProjectTag has no guard against
// an empty project (only Generate does), so it must still return the
// canonical 12-hex prefix of sha256("") rather than an empty/short
// string. Pins that the function is total and content-addressed.
func TestShortProjectTag_EmptyProject(t *testing.T) {
	tag := ShortProjectTag("")
	const fullEmptySha = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if tag != fullEmptySha[:12] {
		t.Errorf("ShortProjectTag(\"\") = %q, want %q", tag, fullEmptySha[:12])
	}
}

// TestMatchesProject_CrossProjectMatrix exhaustively checks that a tag
// minted for one project never authorises a different project, and that
// each project's own tag (and legacy raw ID) accepts. This is the
// defense-in-depth cross-check that scopes a request; a false accept
// here would let a key bound to project A pass the tag gate for B.
func TestMatchesProject_CrossProjectMatrix(t *testing.T) {
	projects := []string{"assistant", "trading", "research", "foo-bar"}
	for _, owner := range projects {
		tag := ShortProjectTag(owner)
		for _, candidate := range projects {
			want := candidate == owner
			if got := MatchesProject(tag, candidate); got != want {
				t.Errorf("MatchesProject(tag(%q), %q) = %v, want %v", owner, candidate, got, want)
			}
			// Legacy raw-projectID segment behaves the same way.
			if got := MatchesProject(owner, candidate); got != want {
				t.Errorf("MatchesProject(rawID %q, %q) = %v, want %v", owner, candidate, got, want)
			}
		}
	}
}

// TestMatchesProject_TagCollisionIsNotAuthBypass documents and pins the
// security model from apikey.go: the tag is only a cross-check. Even if
// two distinct projects shared a tag, MatchesProject keys off the
// project ID passed in by the caller (which comes from the authoritative
// DB row found via LookupActiveByHash), so presenting project A's tag
// while the row says project B must still reject. We can't force a real
// sha256 collision, but we can assert the binding direction: a segment
// equal to project A's tag does not match project B regardless of how
// the segment was obtained.
func TestMatchesProject_TagCollisionIsNotAuthBypass(t *testing.T) {
	tagA := ShortProjectTag("project-a")
	// Sanity: tagA genuinely matches A.
	if !MatchesProject(tagA, "project-a") {
		t.Fatal("precondition: tagA must match project-a")
	}
	// The authoritative row says project-b; the presented tag is A's.
	// MatchesProject must reject — the tag alone cannot grant access to a
	// project whose own tag/ID it does not equal.
	if MatchesProject(tagA, "project-b") {
		t.Error("project A's tag must not satisfy the cross-check for project B")
	}
}

// TestMatchesProject_EmptyInputs — both an empty claimed segment and an
// empty target must reject. An empty segment short-circuits to false;
// an empty target's tag is a fixed hex string that an empty segment can
// never equal. Guards against a blank/truncated key sliding through.
func TestMatchesProject_EmptyInputs(t *testing.T) {
	if MatchesProject("", "assistant") {
		t.Error("empty segment must not match a real project")
	}
	if MatchesProject("", "") {
		t.Error("empty segment must not match empty project")
	}
	// A non-empty segment against an empty project only matches if it
	// equals the empty-project tag (legacy raw "" is impossible since
	// Generate rejects it). A normal tag must not.
	if MatchesProject(ShortProjectTag("assistant"), "") {
		t.Error("assistant's tag must not match an empty project")
	}
}

// TestParse_RandomTailWithURLSafeChars — base64.RawURLEncoding emits '-'
// and '_', and Parse uses LastIndex on '.' to find the separator. This
// pins that a random tail containing those chars round-trips and that the
// separator split is unambiguous (project segment must contain no '.').
func TestParse_RandomTailWithURLSafeChars(t *testing.T) {
	// Hand-craft a key whose random tail contains both URL-safe specials.
	key := Prefix + "-" + ShortProjectTag("p") + "." + "ab-cd_ef-GH_12"
	pid, random, err := Parse(key)
	if err != nil {
		t.Fatalf("Parse(%q): %v", key, err)
	}
	if !MatchesProject(pid, "p") {
		t.Errorf("parsed segment %q does not match project p", pid)
	}
	if random != "ab-cd_ef-GH_12" {
		t.Errorf("random = %q, want ab-cd_ef-GH_12", random)
	}
}

// TestParse_LastSeparatorWins — if a key somehow carries more than one
// '.', Parse uses LastIndex, so everything up to the final dot is the
// project segment and only the final field is the random tail. Pinning
// this prevents a refactor to strings.Index (first dot) that would
// silently truncate the project segment and mis-scope auth.
func TestParse_LastSeparatorWins(t *testing.T) {
	key := Prefix + "-" + "weird.project" + "." + "rnd123"
	pid, random, err := Parse(key)
	if err != nil {
		t.Fatalf("Parse(%q): %v", key, err)
	}
	if pid != "weird.project" {
		t.Errorf("projectID = %q, want %q (LastIndex must win)", pid, "weird.project")
	}
	if random != "rnd123" {
		t.Errorf("random = %q, want rnd123", random)
	}
}

// TestParse_WrongPrefixVariants — anything not starting with the exact
// "sk-vornik-" prefix is rejected with ErrWrongPrefix, including a key
// that merely contains the prefix mid-string or has a near-miss prefix.
// AuthMiddleware collapses this to UNAUTHORIZED, but the package-level
// error must stay specific for diagnostics.
func TestParse_WrongPrefixVariants(t *testing.T) {
	cases := []string{
		"sk-swarm-foo.abc",   // missing 'd'
		"SK-VORNIK-foo.abc",  // wrong case
		"xsk-vornik-foo.abc", // prefix not at start
		" sk-vornik-foo.abc", // leading space
		"sk-vornikfoo.abc",   // missing the hyphen after prefix
	}
	for _, key := range cases {
		if _, _, err := Parse(key); err != ErrWrongPrefix {
			t.Errorf("Parse(%q) err = %v, want ErrWrongPrefix", key, err)
		}
	}
}

// TestDisplayPrefix_BoundaryLengths — DisplayPrefix returns the input
// verbatim when it is shorter than OR equal to PrefixDisplayLen, and
// truncates only when strictly longer. The == boundary is the easy
// off-by-one to break, and getting it wrong would either panic on a
// short key or expose an extra secret char.
func TestDisplayPrefix_BoundaryLengths(t *testing.T) {
	exact := strings.Repeat("x", PrefixDisplayLen)
	if got := DisplayPrefix(exact); got != exact {
		t.Errorf("DisplayPrefix(len==cap) = %q, want %q", got, exact)
	}
	longer := strings.Repeat("y", PrefixDisplayLen+1)
	got := DisplayPrefix(longer)
	if len(got) != PrefixDisplayLen {
		t.Errorf("DisplayPrefix(len>cap) length = %d, want %d", len(got), PrefixDisplayLen)
	}
	if got != longer[:PrefixDisplayLen] {
		t.Errorf("DisplayPrefix(len>cap) = %q, want %q", got, longer[:PrefixDisplayLen])
	}
}

// TestGenerate_OnlyRandomPortionDiffers — across two calls for the same
// project, the tag portion (everything up to the separator) is identical
// and only the random tail differs. This pins that the project binding
// is stable per project while the secret is fresh per key — exactly the
// property revocation/correlation relies on.
func TestGenerate_OnlyRandomPortionDiffers(t *testing.T) {
	a, err := Generate("revocable")
	if err != nil {
		t.Fatalf("Generate a: %v", err)
	}
	b, err := Generate("revocable")
	if err != nil {
		t.Fatalf("Generate b: %v", err)
	}
	if a == b {
		t.Fatal("two keys for same project were identical")
	}
	pidA, randA, err := Parse(a)
	if err != nil {
		t.Fatalf("Parse a: %v", err)
	}
	pidB, randB, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse b: %v", err)
	}
	if pidA != pidB {
		t.Errorf("tag portion differs across calls: %q vs %q", pidA, pidB)
	}
	if pidA != ShortProjectTag("revocable") {
		t.Errorf("tag portion %q != ShortProjectTag(\"revocable\")", pidA)
	}
	if randA == randB {
		t.Error("random tails must differ across calls")
	}
	// Distinct keys must also hash distinctly (no DB row collision).
	if Hash(a) == Hash(b) {
		t.Error("distinct keys produced identical hashes")
	}
}
