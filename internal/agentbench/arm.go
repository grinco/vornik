package agentbench

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/membench"
)

// Arms and comparability (§5.5).
//
// An arm is one configuration under test. Two runs may only be diffed when they
// agree on every axis that could move a number — and the key REFUSES a mismatch
// rather than warning about one, because a warning on a comparison is a warning
// nobody reads twice.
//
// The key algorithm is membench's, imported rather than reimplemented (§6.1).
// The FIELDS are this benchmark's own: a memory run's embedder and recall params
// say nothing about an agent run, and carrying them as empty strings would put
// misleading axes on every manifest.

// HarnessVersion is this package's scoring contract. Bump it when a probe's
// definition changes: old numbers become incomparable even when every other axis
// matches, and the arm key is what makes that refusal automatic rather than
// remembered.
const HarnessVersion = "1"

// ArmFields enumerates every axis that makes two agent-benchmark runs
// incomparable.
//
// Enumerated explicitly rather than derived from a config struct, so adding a
// knob without deciding whether it affects comparability is a visible omission
// rather than a silent default — the same discipline membench applies.
type ArmFields struct {
	// HarnessVersion is this package's contract version. A scoring change makes
	// old numbers incomparable even when everything else matches.
	HarnessVersion string `json:"harnessVersion"`
	// Name is the operator's label for the arm. Not part of the key: renaming an
	// arm must not make it incomparable with itself.
	Name string `json:"name"`

	// BinarySHA256 identifies the daemon under test. Published beside results;
	// the config hash is not (§7).
	BinarySHA256 string `json:"binarySha256"`
	// ConfigSHA256 is the resolved config the daemon actually ran with — not the
	// file on disk, which may not be the tree the daemon reads.
	ConfigSHA256 string `json:"configSha256"`

	// Models is the (role → model) map, flattened deterministically. A model
	// swap on one role changes cost and accuracy and must split the key.
	Models map[string]string `json:"models"`

	// ContextPolicy is the thing under test: suppression set, advertisement
	// gating, grant ceiling, compaction settings. Free-form because the policy
	// surface changes faster than this struct should.
	ContextPolicy string `json:"contextPolicy"`

	// TaskSetSHA256 and GoldSHA256 pin what was run and what it was scored
	// against. A gold regeneration makes prior numbers incomparable, which is
	// exactly why §5.3 fences regeneration.
	TaskSetSHA256 string `json:"taskSetSha256"`
	GoldSHA256    string `json:"goldSha256,omitempty"`

	// Probes lists the probes that scored the run, sorted. A run scored by two
	// probes is not a superset of one scored by three — the third may have
	// failed executions the others tolerated.
	Probes []string `json:"probes"`
}

// fieldPairs returns the key's inputs in a fixed order. One source of truth for
// both hashing and diffing, so the two can never disagree about which fields
// matter.
//
// Name is deliberately absent: an arm renamed is the same experiment.
func (a ArmFields) fieldPairs() [][2]string {
	return [][2]string{
		{"harness_version", a.HarnessVersion},
		{"binary_sha256", a.BinarySHA256},
		{"config_sha256", a.ConfigSHA256},
		{"models", flattenModels(a.Models)},
		{"context_policy", a.ContextPolicy},
		{"task_set_sha256", a.TaskSetSHA256},
		{"gold_sha256", a.GoldSHA256},
		{"probes", strings.Join(sortedCopy(a.Probes), ",")},
	}
}

// Key is this arm's comparability key, computed by membench's implementation.
func (a ArmFields) Key() string {
	return membench.ComparabilityKeyOf(a.fieldPairs())
}

// Partial reports a key that does not cover everything it should.
//
// A partial key means comparability is UNVERIFIED, which is not the same as
// verified-identical and must be surfaced as such. An unknown binary or config
// is the common cause: two runs against different daemons would otherwise key
// alike and compare clean.
func (a ArmFields) Partial() bool {
	return a.BinarySHA256 == "" || a.ConfigSHA256 == "" || a.TaskSetSHA256 == ""
}

// CheckComparable refuses a diff between runs that do not agree, naming every
// differing axis rather than the first — an operator who fixes one difference
// and re-runs only to hit the next has been sent round a loop.
func CheckComparable(a, b ArmFields) error {
	if a.Key() == b.Key() {
		return nil
	}
	diffs := membench.DiffComparabilityPairs(a.fieldPairs(), b.fieldPairs())
	if len(diffs) == 0 {
		// Keys differ but no enumerated field does: fieldPairs has drifted out
		// of sync with the key. Report it rather than claiming comparability.
		return fmt.Errorf("arm keys differ but no enumerated field does — " +
			"fieldPairs() is out of sync with Key()")
	}
	return fmt.Errorf("arms %q and %q are not comparable; differing: %s",
		a.Name, b.Name, strings.Join(diffs, ", "))
}

// flattenModels renders the role→model map deterministically. Map iteration
// order is random in Go, so hashing the map directly would give one arm a
// different key on every run.
func flattenModels(models map[string]string) string {
	if len(models) == 0 {
		return ""
	}
	roles := make([]string, 0, len(models))
	for role := range models {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	var b strings.Builder
	for i, role := range roles {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(role)
		b.WriteByte('=')
		b.WriteString(models[role])
	}
	return b.String()
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TaskSetDigest identifies the task set a run used.
//
// Order-independent and length-prefixed, mirroring membench's CorpusDigest for
// the same reasons: the order tasks happen to be listed in is not a property of
// the set, and a rename must not be able to compensate for an edit.
//
// This is also the value §5.3's regeneration fence compares against, so a task
// set that changed by one character produces a different digest and permits a
// gold regeneration that an unchanged one refuses.
func TaskSetDigest(taskIDs []string, bodies map[string]string) string {
	if len(taskIDs) == 0 {
		return ""
	}
	ids := sortedCopy(taskIDs)
	h := sha256.New()
	for _, id := range ids {
		body := bodies[id]
		_, _ = fmt.Fprintf(h, "%d:%s%d:%s", len(id), id, len(body), body)
	}
	return hex.EncodeToString(h.Sum(nil))
}
