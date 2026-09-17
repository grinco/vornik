package memory

import (
	"context"
	"strings"
)

// Recurring-series identity — 2026-09-16-retrieval-recency-design.md §5.1.1.
//
// A `series_key` marks a chunk as one member of a recurring series (a daily
// news digest, a weekly job scan) so ranking can demote every member but the
// newest. §5.3 derives "is there a newer member" at query time; this file
// decides what the series IS.
//
// The key is DERIVED from the autonomy feed slug carried in the producing
// task's prompt, validated against the slugs the project declares. That looks
// like the `source_name` stem §5.3 rejects as a fragile operator-controlled
// string, and the distinction is not sturdiness — it is what each does when it
// breaks. The stem has an open vocabulary and groups WRONGLY on a bad value:
// the live store holds 219 distinct filenames for a single series, which would
// have yielded 219 single-member series and demoted nothing. The slug is
// checked against a closed declared list, so a bad value groups NOTHING.
//
// The stem fails wrong. The validated slug fails closed.
//
// MEASURED AGAINST THE LIVE STORE before shipping, because a rule that warns is
// only useful if its volume is survivable. Over the whole task history of the
// one feed-declaring project: the 7 declared slugs resolve 1,025 tasks, and
// exactly 4 undeclared slug-shaped prefixes account for 12 tasks that would
// warn (`europe-relocation`, which looks like a feed nobody declared, plus
// three ad-hoc prompts that happen to be slug-shaped). Twelve warnings across
// months is a signal an operator reads. The earlier three-case version of the
// table — before "project declares no feeds" was split out — produced 786.

// maxSlugLen bounds what can be read as a slug prefix. Long enough for the
// declared feeds (`prague-restaurants`, `welcometothejungle-cs`), short enough
// that a sentence beginning "Fix the bug: ..." cannot be mistaken for one.
const maxSlugLen = 40

// minSlugLen keeps a one- or two-letter prefix from becoming a series.
const minSlugLen = 3

// SeriesResolver answers "which recurring series does this task's output belong
// to", or "" for the overwhelming majority of tasks that are not part of one.
//
// An interface because the answer needs the task's prompt and the project's
// declared feeds, and internal/memory must not depend on the task store or the
// project registry to get them. The service container wires the real one.
type SeriesResolver interface {
	SeriesKeyFor(ctx context.Context, projectID, taskID string) (key string, warnUndeclared bool)
}

// SlugPrefix extracts a `<slug>: ` prompt prefix, or "".
//
// The shape is the autonomy scheduler's own convention, and `AutonomyFeed.Slug`
// documents it: "the `<slug>: ` prompt prefix identifying this feed's tasks".
// The scheduler matches on it to decide what is due, which is why reading it
// here adds no new fragile dependency — it reads one that already exists and
// already fails noisily when it breaks.
func SlugPrefix(prompt string) string {
	i := strings.Index(prompt, ": ")
	if i < minSlugLen || i > maxSlugLen {
		return ""
	}
	candidate := prompt[:i]
	// Lower-case, digits and dashes only. A slug is a registry key, not prose:
	// "Fix the bug: ..." and "UPPER: ..." must not resolve.
	for _, r := range candidate {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return ""
		}
	}
	return candidate
}

// deriveSeriesKey applies §5.1.1's table. warnUndeclared is true only for the
// one case a warning means anything.
//
//	| project declares feeds | prefix      | in feeds[] | key     | signal |
//	|------------------------|-------------|------------|---------|--------|
//	| yes                    | slug-shaped | yes        | set     | —      |
//	| yes                    | slug-shaped | no         | not set | WARN   |
//	| yes                    | other       | n/a        | not set | silent |
//	| no                     | any         | n/a        | not set | silent |
//
// The fourth row is not a special case bolted on — it is the difference between
// a stale registry and no registry. A project that declares nothing cannot have
// an *undeclared* slug, so warning there is a signal with no referent. It is
// also the difference between a usable warning and an unusable one: 786 live
// tasks across two projects run recurring series with no feeds block, and
// warning on each would bury the row above it.
func deriveSeriesKey(prompt string, declared []string, hasRegistry bool) (string, bool) {
	if !hasRegistry {
		return "", false
	}
	slug := SlugPrefix(prompt)
	if slug == "" {
		return "", false
	}
	for _, d := range declared {
		if d == slug {
			return slug, false
		}
	}
	// Slug-shaped, project declares feeds, this is not one of them. The
	// scheduler cannot notice — it matches prompt-prefix against
	// prompt-prefix and never reads the registry — so ranking is the only
	// place this shows up, and it must not show up silently.
	return "", true
}

// ResolveSeriesKey applies the precedence in §5.1.1: an explicitly declared key
// wins over a derived one, always.
//
// A producer that names its series has made a deliberate statement; derivation
// is an inference for producers that have not. Note the trust boundary this
// creates and that §5.1.1 states: the closed-list validation guards the
// INFERENCE path only, so a declared key is trusted unvalidated. That is
// deliberate — validating declarations would leave a non-autonomy producer
// unable to name a series the registry does not know — and the exposure is
// covered by warning when a recurring source's key CHANGES, not only when it
// goes missing.
func ResolveSeriesKey(declaredKey, prompt string, declaredSlugs []string, hasRegistry bool) (string, bool) {
	if declaredKey != "" {
		return declaredKey, false
	}
	return deriveSeriesKey(prompt, declaredSlugs, hasRegistry)
}
