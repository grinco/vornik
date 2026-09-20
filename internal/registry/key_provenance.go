package registry

import (
	_ "embed"
	"reflect"
	"sort"
	"strings"

	"vornik.io/vornik/internal/config"
)

// Per-key release provenance — loader-validator agreement design §13.2.
//
// ProjectConfigKeys answers "would my config load under this binary?", which is
// the upgrade question. It cannot answer "what changed in 2026.9.4?", because
// the schema carried no record of which release introduced each key. A `since:`
// tag on the field does, read through the SAME config.WalkLeaves the key set is
// already derived from — so the record cannot be hand-maintained into drift,
// which is the whole reason to prefer it over a manifest generated at release
// time (a manifest exists only if the release step ran).

//go:embed project_keys_baseline.txt
var projectKeysBaselineFile string

// KeySince is one config key and the release that introduced it.
type KeySince struct {
	Key string `json:"key"`
	// Since is the release the field declares, or "" for a key that predates
	// per-key provenance. Empty is a STATEMENT — "older than tracking" — not a
	// gap, and it is only legitimate for a key in the committed baseline.
	Since string `json:"since,omitempty"`
}

// ProjectConfigKeysSince returns every project config key with the release that
// introduced it, sorted by key.
func ProjectConfigKeysSince() []KeySince {
	out := make([]KeySince, 0, 256)
	config.WalkLeaves(reflect.ValueOf(Project{}), func(key string, f reflect.StructField, _ reflect.Value) {
		out = append(out, KeySince{Key: key, Since: strings.TrimSpace(f.Tag.Get("since"))})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ProjectKeysPredatingProvenance is the committed baseline: the keys that
// existed before `since:` tags did.
//
// It exists so new fields can be REQUIRED to carry a release without
// back-tagging 193 existing ones — the same ratchet shape as the LLD anchor
// ceiling, and for the same reason: a rule enforced by a file is a rule, and a
// rule that depends on remembering is not.
//
// It only ever shrinks. Back-tagging an old field removes its line; nothing is
// ever added.
func ProjectKeysPredatingProvenance() map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(projectKeysBaselineFile, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line] = true
	}
	return out
}

// KeysWithoutProvenance returns keys that neither declare a `since:` nor appear
// in the baseline — i.e. fields added after provenance shipped and not tagged.
// Empty is the passing state.
func KeysWithoutProvenance() []string {
	baseline := ProjectKeysPredatingProvenance()
	var missing []string
	for _, k := range ProjectConfigKeysSince() {
		if k.Since == "" && !baseline[k.Key] {
			missing = append(missing, k.Key)
		}
	}
	sort.Strings(missing)
	return missing
}

// ProjectKeysNewerThan returns the keys introduced AFTER the given release, by
// simple string comparison of the declared `since:` values.
//
// String comparison rather than semver parsing, deliberately: releases here are
// date-ordered (2026.9.4 < 2026.9.5 < 2026.10.0 fails, and that is the known
// cost) — so the caller is told to compare against a release it actually ran
// rather than to trust ordering across a major roll. A key with no `since:`
// predates tracking and is never "newer".
func ProjectKeysNewerThan(release string) []KeySince {
	release = strings.TrimSpace(release)
	var out []KeySince
	for _, k := range ProjectConfigKeysSince() {
		if k.Since != "" && (release == "" || k.Since > release) {
			out = append(out, k)
		}
	}
	return out
}
