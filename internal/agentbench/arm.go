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
// DEFINITION changes: old numbers become incomparable even when every other axis
// matches, and the arm key is what makes that refusal automatic rather than
// remembered.
//
// It is not the release version and it is not a build stamp. It moves only when
// the meaning of a number changes.
//
//	v1  first implementation
//	v2  2026-08-14. Four scoring changes in one day, each of which silently
//	    invalidates a v1 figure:
//	      - schema conformance excludes steps that produced NO output (a crashed
//	        container was previously counted as CONFORMANT)
//	      - grants are scored per EXECUTION, not per step
//	      - a trace with no grant activity is skipped rather than scored 0
//	      - tool names are compared in a bare form, so "functions.git_status"
//	        matches gold's "git_status"
//	v3  2026-08-14. Core-miss scoring is substitution-aware: a core tool counts
//	    as satisfied when the grant holds any tool of the same capability. The
//	    first baseline's five core misses were all the old rule misfiring —
//	    demanding git_status from a lead that granted run_shell, and
//	    read_many_files from one that granted file_read and grep. Operator
//	    ruling; see equivalence.go for why run_shell is a member of every
//	    CLI-expressible class.
//	v4  2026-08-17. Binary task completion is replaced as the paired quality
//	    metric by graded analyst-pinned case validation. Task repeats are
//	    averaged before pairing and the scoring-policy digest is an arm axis.
//	v5  2026-08-17. Release-gate tier semantics and the observed immutable
//	    agent-image set become arm axes. Task outcomes are journaled at the
//	    submitted-task/repeat unit for calibration.
//
// v1 was never bumped through any of those, which is the failure this comment
// exists to prevent: the mechanism refused nothing because nobody moved the
// number it keys on.
const HarnessVersion = "5"

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
	//
	// This is the model's IDENTITY, not the DEPLOYMENT serving it. GPU count,
	// tensor-parallel width, memory headroom and batching limits are all absent,
	// so two runs can share an identical arm key while one was served by a
	// throughput-starved deployment and the other was not.
	//
	// Not fixed here, because the key cannot capture it: the harness has no way
	// to observe a remote server's resource envelope — an OpenAI-compatible
	// /v1/models reports ids and nothing else. Adding a field would mean hashing
	// something unobservable.
	//
	// Resist the tempting simplification that "resources do not move logits, so
	// quality stays comparable". The premise is true and the conclusion does not
	// follow: faster inference fits MORE tool calls inside the same wall clock,
	// so which LIMIT terminates a step can change — a step that used to die on a
	// step timeout now runs long enough to exhaust its tool budget instead. The
	// outcome taxonomy shifts without the model behaving differently. Latency is
	// the obvious casualty; the outcome distribution is the subtle one.
	//
	// So it is an axis only an operator can attest to. Instance: 2026-08-20,
	// arms validation-report-write-first-v2 and -v3 straddle a vLLM redeploy
	// that changed resources only; the split is by arm NAME with the reason in
	// agentbench-runs/validation5/PROVENANCE-NOTE.md. A latency comparison
	// across arms should look for such a note and treat its absence as "unknown
	// deployment" rather than "same deployment".
	Models map[string]string `json:"models"`
	// AgentImages is the observed (role → immutable image IDs) map. Configured
	// tags are intent and can be retagged; only the IDs reported by the runtime
	// prove which agent loop executed the arm.
	AgentImages map[string]string `json:"agentImages"`

	// ContextPolicy is the thing under test: suppression set, advertisement
	// gating, grant ceiling, compaction settings. Free-form because the policy
	// surface changes faster than this struct should.
	ContextPolicy string `json:"contextPolicy"`

	// TaskSetSHA256 and GoldSHA256 pin what was run and what it was scored
	// against. A gold regeneration makes prior numbers incomparable, which is
	// exactly why §5.3 fences regeneration.
	TaskSetSHA256 string `json:"taskSetSha256"`
	GoldSHA256    string `json:"goldSha256,omitempty"`
	// ScoringPolicySHA256 pins the task-level quality contract separately from
	// the task body that the agents ran.
	ScoringPolicySHA256 string `json:"scoringPolicySha256,omitempty"`
	// TierPolicySHA256 pins which tasks are tripwires, scored gates, and
	// exploratory diagnostics without changing the identity of what agents ran.
	TierPolicySHA256 string `json:"tierPolicySha256"`

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
		{"agent_images", flattenModels(a.AgentImages)},
		{"context_policy", a.ContextPolicy},
		{"task_set_sha256", a.TaskSetSHA256},
		{"gold_sha256", a.GoldSHA256},
		{"scoring_policy_sha256", a.ScoringPolicySHA256},
		{"tier_policy_sha256", a.TierPolicySHA256},
		{"probes", strings.Join(sortedCopy(a.Probes), ",")},
	}
}

// ComparabilityAxes are the axis names CheckComparable reports, and the only
// values a pre-registration may nominate as its independent variable.
func ComparabilityAxes() []string {
	pairs := ArmFields{}.fieldPairs()
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p[0])
	}
	return out
}

// CheckComparableExcept is CheckComparable with a declared independent variable.
//
// Without it the benchmark cannot do the one thing it exists for. Comparing two
// RELEASES means the binary differs by definition, and CheckComparable refuses
// any difference — so `bench agent compare` refused every release comparison,
// which is what the README advertises it for.
//
// The axes must be DECLARED IN THE PRE-REGISTRATION, before the runs. That is
// the whole safeguard: choosing after the fact which difference to forgive is
// how a comparison becomes a press release. Every axis not named still refuses.
func CheckComparableExcept(a, b ArmFields, allowed []string) error {
	if len(allowed) == 0 {
		return CheckComparable(a, b)
	}
	permitted := set(allowed)
	// Forgiving everything is not a comparison. Refused explicitly rather than
	// left to produce a meaningless clean verdict.
	if len(permitted) >= len(ComparabilityAxes()) {
		return fmt.Errorf("pre-registration declares every axis as independent; "+
			"that compares nothing — declare the one variable under test, from: %s",
			strings.Join(ComparabilityAxes(), ", "))
	}
	for _, axis := range allowed {
		if !containsString(ComparabilityAxes(), axis) {
			return fmt.Errorf("pre-registration declares unknown independent axis %q; known axes: %s",
				axis, strings.Join(ComparabilityAxes(), ", "))
		}
	}
	var refused []string
	for i, pa := range a.fieldPairs() {
		pb := b.fieldPairs()[i]
		if pa[1] != pb[1] && !permitted[pa[0]] {
			refused = append(refused, fmt.Sprintf("%s (%q vs %q)", pa[0], pa[1], pb[1]))
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("arms %q and %q differ on axes the pre-registration did NOT "+
			"declare independent: %s", a.Name, b.Name, strings.Join(refused, ", "))
	}
	return nil
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
	return strings.TrimSpace(a.HarnessVersion) == "" || a.BinarySHA256 == "" ||
		a.ConfigSHA256 == "" || len(a.Models) == 0 || len(a.AgentImages) == 0 ||
		strings.TrimSpace(a.ContextPolicy) == "" || a.TaskSetSHA256 == "" ||
		a.TierPolicySHA256 == "" || len(a.Probes) == 0
}

// CheckMergeable is CheckComparable for BATCHES OF ONE ARM.
//
// Models and AgentImages are OBSERVED — a batch records only the roles its own
// tasks invoked — so batches of one arm legitimately differ in role COVERAGE.
// Observed 2026-08-29: a completed 30-task arm refused to merge because batch 0
// never ran analyst or tester while batch 1 did, though every shared role held
// an identical value. Unlike the task-derived axes, these cannot be made
// whole-set at build time: nothing knows which roles a task will invoke until
// it runs.
//
// So coverage may differ and VALUES may not. A role present in both must agree;
// roles present in one are unioned by the caller. Every other axis is compared
// exactly as CheckComparable does — the tolerance is confined to the two
// observed maps and must not leak into the axes that define the experiment.
//
// This is deliberately NOT a relaxation of CheckComparable, which still governs
// diffing two independent arms, where a differing model set is a different
// experiment and must refuse.
func CheckMergeable(a, b ArmFields) error {
	if err := sharedRolesAgree("models", a.Models, b.Models); err != nil {
		return err
	}
	if err := sharedRolesAgree("agent_images", a.AgentImages, b.AgentImages); err != nil {
		return err
	}
	// Compare every other axis with the observed maps restricted to the roles
	// both saw, so coverage cannot mask a real difference elsewhere.
	an, bn := a, b
	an.Models, bn.Models = restrictToShared(a.Models, b.Models)
	an.AgentImages, bn.AgentImages = restrictToShared(a.AgentImages, b.AgentImages)
	return CheckComparable(an, bn)
}

// sharedRolesAgree refuses when a role observed in BOTH maps holds different
// values — a model swapped mid-arm, or one batch run against another image.
func sharedRolesAgree(axis string, a, b map[string]string) error {
	for role, av := range a {
		if bv, ok := b[role]; ok && av != bv {
			return fmt.Errorf("arms disagree on %s for role %q (%q vs %q); "+
				"batches of one arm must have measured the same system",
				axis, role, av, bv)
		}
	}
	return nil
}

// restrictToShared returns both maps reduced to the keys they have in common.
func restrictToShared(a, b map[string]string) (map[string]string, map[string]string) {
	ra := make(map[string]string, len(a))
	rb := make(map[string]string, len(b))
	for k, v := range a {
		if bv, ok := b[k]; ok {
			ra[k] = v
			rb[k] = bv
		}
	}
	return ra, rb
}

// unionObservedRoles merges the observed role maps of two mergeable arms.
//
// The merged arm must describe every role that actually ran, or a rolled-up
// figure would silently cover fewer roles than the pass exercised. Safe only
// after CheckMergeable, which has already refused any shared-role disagreement.
func unionObservedRoles(a, b ArmFields) ArmFields {
	out := a
	out.Models = unionMaps(a.Models, b.Models)
	out.AgentImages = unionMaps(a.AgentImages, b.AgentImages)
	return out
}

func unionMaps(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
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
