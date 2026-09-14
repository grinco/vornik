package registry

import (
	"strings"
	"testing"
)

// UPGRADE SAFETY, slice 1 (BACKLOG P1 2026-09-12).
//
// The motivating incident, 2026-09-10: `autonomy.feeds:` was written into a
// deployed project file while the running daemon predated the field. The loader
// is strict and fail-CLOSED, so the unknown key rejected the WHOLE FILE, the
// registry dropped the `assistant` project, and autonomy, chat and email went
// dark for ~30 minutes. Nothing about that surfaced as "the upgrade rejected a
// key" — it surfaced as "my project is gone".
//
// Fail-closed is not the bug (a permissions-bearing loader that fails open
// fails nothing and says nothing). The gap is that nothing carried the
// obligation fail-closed places on a deployer, and nothing diagnosed the
// resulting state. This is the diagnosis.

func TestProjectConfigKeys_EnumeratesTheSchemaFromTheCode(t *testing.T) {
	keys := ProjectConfigKeys()
	if len(keys) < 50 {
		t.Fatalf("ProjectConfigKeys returned %d keys; the project schema is far larger than that — the walk is not reaching nested sections", len(keys))
	}
	want := []string{"projectId", "autonomy.enabled", "forge.auto_review_on_push", "github_app.repo_allowlist"}
	for _, k := range want {
		if !containsString(keys, k) {
			t.Errorf("key %q missing from the inventory; a record of what this binary accepts that omits real keys is worse than none", k)
		}
	}
	// Derived from the struct tags, never hand-maintained: a key added to the
	// Go type must appear here with no second edit, or the record drifts from
	// the loader it is supposed to describe.
	if containsString(keys, "autonomy") {
		t.Error("sections are not keys; the inventory must list leaves")
	}
}

func containsString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// The incident's exact shape: a key this binary has never heard of. The
// diagnosis must name the version-skew reading, because that is what it
// almost always is, and must state the blast radius — the whole project, not
// the key.
func TestDiagnoseProjectDecodeError_UnknownKeyReadsAsVersionSkew(t *testing.T) {
	// The incident's key was `autonomy.feeds`, which this binary now KNOWS —
	// the field shipped. So the fixture uses a key from a hypothetical later
	// release instead: what is being pinned is the shape of the diagnosis for
	// "config ahead of binary", not one historical key.
	data := []byte("projectId: assistant\nautonomy:\n  feed_cadence_budget: 5\n")
	err := DecodeProjectStrict(data)
	if err == nil {
		t.Fatal("fixture must be rejected by the strict decoder, or it is not reproducing the incident")
	}

	d := DiagnoseProjectDecodeError("assistant.yaml", data, err)
	if len(d.UnknownKeys) == 0 {
		t.Fatalf("no unknown key extracted from %q", err)
	}
	if !containsString(d.UnknownKeys, "feed_cadence_budget") {
		t.Errorf("UnknownKeys = %v, want the offending key", d.UnknownKeys)
	}
	if d.ProjectID != "assistant" {
		t.Errorf("ProjectID = %q, want assistant — the operator needs to know WHICH project just vanished", d.ProjectID)
	}
	if !d.LikelyVersionSkew {
		t.Error("a key unknown under every spelling is version skew; saying otherwise sends the operator hunting for a typo")
	}
	detail := d.Detail()
	for _, want := range []string{"assistant", "whole", "feed_cadence_budget", "binary"} {
		if !strings.Contains(strings.ToLower(detail), strings.ToLower(want)) {
			t.Errorf("Detail() does not mention %q:\n%s", want, detail)
		}
	}
	// The remedy is the point. A diagnosis that names the fault and stops
	// leaves the operator exactly where the incident left them.
	for _, want := range []string{"Remedy", "deploy the binary", "FIRST", "Rolling BACK"} {
		if !strings.Contains(detail, want) {
			t.Errorf("Detail() must name the deploy ordering (%q missing):\n%s", want, detail)
		}
	}
}

// The other reading, and the one the operator CAN fix without deploying
// anything: the same concept under the wrong spelling. The 2026-08-27 design
// records two real instances (`dailySoftUSD` for `daily_soft_usd`, `id` for
// `projectId`), both of which lenient decoding had silently discarded.
func TestDiagnoseProjectDecodeError_MisspeltKeyIsNotVersionSkew(t *testing.T) {
	data := []byte("projectId: p1\nbudget:\n  dailySoftUSD: 100\n")
	err := DecodeProjectStrict(data)
	if err == nil {
		t.Fatal("fixture must be rejected: dailySoftUSD is not the schema's spelling")
	}

	d := DiagnoseProjectDecodeError("p1.yaml", data, err)
	if d.LikelyVersionSkew {
		t.Error("a key that matches a known one apart from spelling is a TYPO, not skew")
	}
	if got := d.Suggestions["dailySoftUSD"]; got != "daily_soft_usd" {
		t.Errorf("Suggestions[dailySoftUSD] = %q, want daily_soft_usd", got)
	}
	if !strings.Contains(d.Detail(), "daily_soft_usd") {
		t.Errorf("Detail() must carry the suggestion:\n%s", d.Detail())
	}
}

// A plain syntax error is neither skew nor a typo. It must not be dressed up
// as either — a diagnosis that guesses is worse than one that says "I could
// not tell", because the operator acts on it.
func TestDiagnoseProjectDecodeError_SyntaxErrorClaimsNothing(t *testing.T) {
	data := []byte("projectId: [unclosed\n")
	err := DecodeProjectStrict(data)
	if err == nil {
		t.Fatal("fixture must fail to parse")
	}
	d := DiagnoseProjectDecodeError("broken.yaml", data, err)
	if d.LikelyVersionSkew || len(d.UnknownKeys) != 0 {
		t.Errorf("a syntax error must not be reported as a schema mismatch: %+v", d)
	}
	if !strings.Contains(d.Detail(), "broken.yaml") {
		t.Errorf("Detail() must still name the file:\n%s", d.Detail())
	}
}

// The blast radius is the FILE, and the file is a whole project. A diagnosis
// that cannot name the project (because the file is too broken to read even
// the id) must say so rather than printing an empty name.
func TestDiagnoseProjectDecodeError_UnnameableProjectSaysSo(t *testing.T) {
	data := []byte("autonomy:\n  feed_cadence_budget: 5\n")
	err := DecodeProjectStrict(data)
	if err == nil {
		t.Fatal("fixture must be rejected")
	}
	d := DiagnoseProjectDecodeError("nameless.yaml", data, err)
	if d.ProjectID != "" {
		t.Fatalf("ProjectID = %q, want empty for a file with no projectId", d.ProjectID)
	}
	if !strings.Contains(d.Detail(), "nameless.yaml") {
		t.Errorf("Detail() must fall back to the file name:\n%s", d.Detail())
	}
}
