package memory

import "testing"

// Series-key derivation — 2026-09-16-retrieval-recency-design.md §5.1.1.
//
// The key identifies a recurring series so that ranking can demote every member
// but the newest (§5.3). It is DERIVED from the autonomy feed slug that a task's
// prompt carries, validated against the slugs the project declares.
//
// Why a prompt prefix at all, given §5.3 rejects the source_name stem as an
// operator-controlled fragile string: the two differ in what they do when they
// break, not in how sturdy they are. The stem has an open vocabulary and groups
// WRONGLY on a bad value — the live store holds 219 distinct filenames for one
// series, which would have produced 219 single-member series. The slug is
// checked against a closed declared list, so a bad value groups NOTHING. The
// stem fails wrong; the slug fails closed.
//
// The four cases below are the design's table, and the fourth exists because a
// three-case version warned on 786 live tasks from two projects that run
// recurring series without declaring a feed registry — drowning the very signal
// the warning is added to provide.

func TestDeriveSeriesKey_DeclaredSlugIsUsed(t *testing.T) {
	key, warn := deriveSeriesKey(
		"czech-news: Refresh project memory for the czech-news feed.",
		[]string{"czech-news", "prague-events"}, true)
	if key != "czech-news" {
		t.Errorf("key = %q, want czech-news", key)
	}
	if warn {
		t.Error("a declared slug must not warn")
	}
}

// The case the warning exists for: the prompt convention held, the registry
// moved. The scheduler never consults feeds[], so it keeps working and only
// ranking would notice — silently, without this.
func TestDeriveSeriesKey_UndeclaredSlugFailsClosedAndWarns(t *testing.T) {
	key, warn := deriveSeriesKey(
		"europe-relocation: refresh the feed.",
		[]string{"czech-news"}, true)
	if key != "" {
		t.Errorf("key = %q, want empty — an undeclared prefix must not become a series", key)
	}
	if !warn {
		t.Error("an undeclared slug on a feed-declaring project must WARN; failing closed " +
			"silently is the failure this rule exists to prevent")
	}
}

// 786 live tasks in janka and vornik-marketing take this path. Warning on them
// would fire a signal with no referent — a project that declares nothing cannot
// have an *undeclared* slug — and would bury the case above.
func TestDeriveSeriesKey_NoRegistryIsSilent(t *testing.T) {
	key, warn := deriveSeriesKey(
		"linkedin-jobs-cz: scan for new postings.", nil, false)
	if key != "" {
		t.Errorf("key = %q, want empty with no registry to validate against", key)
	}
	if warn {
		t.Error("a project that declares no feeds must be SILENT: the absence of a registry " +
			"is not a stale registry, and 786 live tasks take this path")
	}
}

func TestDeriveSeriesKey_OrdinaryPromptIsSilent(t *testing.T) {
	for _, prompt := range []string{
		"Refresh project memory for the czech-news feed.", // no prefix at all
		"Review this design doc for architectural issues.",
		"",                                      // empty
		"Fix the bug: the parser drops a colon", // colon, but not a leading slug
		"UPPER: not a slug",                     // slugs are lower-case
		"a: too short to be a slug",
	} {
		key, warn := deriveSeriesKey(prompt, []string{"czech-news"}, true)
		if key != "" || warn {
			t.Errorf("prompt %q: key=%q warn=%v — an ordinary task must be silent", prompt, key, warn)
		}
	}
}

// §5.1.1 precedence: derivation fills a gap, it never overrides a declaration.
func TestResolveSeriesKey_DeclarationWinsOverDerivation(t *testing.T) {
	got, warn := ResolveSeriesKey("operator-set",
		"czech-news: refresh", []string{"czech-news"}, true)
	if got != "operator-set" {
		t.Errorf("key = %q, want the explicitly declared value to win", got)
	}
	if warn {
		t.Error("an explicit declaration must not warn about the derivation it displaced")
	}
}

func TestResolveSeriesKey_DerivesWhenNothingDeclared(t *testing.T) {
	got, _ := ResolveSeriesKey("", "czech-news: refresh", []string{"czech-news"}, true)
	if got != "czech-news" {
		t.Errorf("key = %q, want czech-news derived when no explicit key is set", got)
	}
}
