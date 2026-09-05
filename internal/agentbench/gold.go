package agentbench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Gold generation and its regeneration fence (§3.2, §5.3).
//
// Gold is a RECORDING, not an opinion: the per-run invoked set from each passing
// run of an unrestricted-ceiling arm. That sidesteps the objection membench
// flagged about a native gold set being authored by the party being measured —
// a wrong entry here is reviewable against the run that produced it rather than
// being a matter of taste.
//
// THE FENCE IS THE POINT. "Regenerated only when the task set changes" was a
// sentence, and a sentence does not stop an operator running `gold` after a
// config change and quietly replacing the ground truth. Pre-registration
// prevents post-hoc comparison shopping; this prevents PRE-hoc ground-truth
// corruption, which nothing else in the design addressed.

// Tools that must never enter ground truth, whatever an agent happened to call.
//
// Gold records tools USED, not tools REQUIRED (§3.2). That limitation is
// tolerable for ordinary workload tools — an agent reaching for `grep` it did not
// strictly need only makes coverage slightly generous. It is NOT tolerable for
// two categories, both found in the first real gold set and confirmed by review
// (review-20260814-d068):
//
//	META — the tool that DECIDES which tools a step gets. Recording
//	grant_step_tools as a tool the task "needed" makes the scoring circular: a
//	grant policy would be scored on whether it granted the grant-decider. It
//	appeared in 10 of 18 task paths.
//
//	SIDE-EFFECTING — a tool that changes state outside the task. `backlog_deposit`
//	writes to a project backlog; it appeared in dp-04-concurrency, whose prompt is
//	"write a bounded worker pool in a new scratch file". A benchmark task must
//	never REQUIRE a side effect, so a policy correctly refusing one must never be
//	penalised for it.
//
// Deliberately NOT here: memory_search and skill_fetch. Review flagged both as
// possible habit — they appeared in paths for self-contained tasks — and the
// operator's ruling (2026-08-14) was that they should be granted to every role
// everywhere instead, because both only read what the project already knows.
// That makes their presence in a path unremarkable rather than suspicious: a
// tool every role has is not evidence of anything, and a grant policy covers it
// for free. See agenttools.AlwaysGranted.
var goldExcludedTools = map[string]string{
	"grant_step_tools": "meta: decides the grant being scored, so including it is circular",
	"backlog_deposit":  "side-effecting: writes outside the task; no task may REQUIRE a side effect",
}

// ExcludedFromGold reports whether a tool must be kept out of ground truth, and
// why. Matching is on the bare name so a qualified spelling cannot smuggle one in.
func ExcludedFromGold(tool string) (string, bool) {
	reason, ok := goldExcludedTools[normaliseTool(tool)]
	return reason, ok
}

// filterGoldPath drops excluded tools from one recorded path.
func filterGoldPath(path []string) []string {
	out := make([]string, 0, len(path))
	for _, t := range path {
		if _, excluded := ExcludedFromGold(t); excluded {
			continue
		}
		out = append(out, t)
	}
	return out
}

// GoldManifest is a pinned gold set.
type GoldManifest struct {
	// TaskSetSHA256 is what the gold was generated FROM. The fence compares
	// against it.
	TaskSetSHA256 string `json:"taskSetSha256"`
	// Runs is how many unrestricted-ceiling runs contributed.
	Runs int `json:"runs"`
	// Entries is the per-task ground truth, sorted by task id so the manifest
	// is byte-stable.
	Entries []Gold `json:"entries"`
	// ReviewedBy records the operator who accepted this gold. Gold defines what
	// "correct" means for the gate, so it is not self-certifiable by the
	// harness that produced it.
	ReviewedBy string `json:"reviewedBy,omitempty"`
}

// Validate refuses a manifest whose task-set digest is not a digest.
//
// The field is compared against a real sha256 by the gold fence, so a
// malformed value can only ever produce a refusal at use time — but it can be
// WRITTEN, and on 2026-08-14 one was: dev-swarm-gold-v1.json shipped with
// captured `vornikctl bench` help text in taskSetSha256, because the producing
// script piped a help message through `awk '{print $1}'` and nobody looked at
// the shape. That took the tool-grant probe dark for five days.
//
// Checked at construction and at merge rather than only at load, because the
// cheapest place to reject ground truth that cannot be ground truth is before
// it reaches a file somebody commits.
func (m GoldManifest) Validate() error {
	if err := validateTaskSetDigest(m.TaskSetSHA256); err != nil {
		return err
	}
	return nil
}

// ValidateTaskSetDigest is the exported form, so a COMMAND can apply this
// precondition during argument parsing instead of at write time.
//
// That ordering is the whole point. On 2026-08-23 a `--topup` run was given
// `topup0-<hash>`, executed all 13 tasks, completed 13 of 13 over 3h40m, and
// was refused when the manifest was built — with nothing salvageable, because
// `bench agent gold` runs the tasks itself and rescore needs a journal the
// failed command never wrote. The value was knowable before the first
// container started. A run that spends hours before it can discover it will
// refuse to record the result has turned a cheap failure into an expensive one.
func ValidateTaskSetDigest(digest string) error { return validateTaskSetDigest(digest) }

// validateTaskSetDigest requires exactly 64 lowercase hex characters — the
// shape hex.EncodeToString produces for a sha256, and nothing else. Lowercase
// is required rather than folded: the fence compares digests by string
// equality, so an uppercase copy of the right hash still refuses, and
// accepting it here would move that surprise later.
func validateTaskSetDigest(digest string) error {
	if digest == "" {
		return fmt.Errorf("gold manifest taskSetSha256 is empty: it must name the task set the gold was recorded from")
	}
	if len(digest) != 64 {
		return fmt.Errorf("gold manifest taskSetSha256 is not a sha256: got %d characters, want 64 (value begins %q)",
			len(digest), shortHash(digest))
	}
	for i := 0; i < len(digest); i++ {
		c := digest[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return fmt.Errorf("gold manifest taskSetSha256 is not a sha256: character %d is %q, want lowercase hex (value begins %q)",
			i, string(c), shortHash(digest))
	}
	return nil
}

// SHA256 is the manifest's identity, used as the arm key's gold_sha256.
func (m GoldManifest) SHA256() (string, error) {
	blob, err := json.Marshal(m.canonical())
	if err != nil {
		return "", fmt.Errorf("marshal gold manifest: %w", err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

// canonical returns the manifest with every collection sorted, so an equal gold
// set always hashes alike regardless of the order runs happened to arrive in.
func (m GoldManifest) canonical() GoldManifest {
	out := GoldManifest{
		TaskSetSHA256: m.TaskSetSHA256,
		Runs:          m.Runs,
		ReviewedBy:    m.ReviewedBy,
		Entries:       make([]Gold, 0, len(m.Entries)),
	}
	for _, e := range m.Entries {
		paths := make([][]string, 0, len(e.Paths))
		for _, p := range e.Paths {
			paths = append(paths, sortedCopy(p))
		}
		sort.Slice(paths, func(i, j int) bool {
			return fmt.Sprint(paths[i]) < fmt.Sprint(paths[j])
		})
		out.Entries = append(out.Entries, Gold{
			TaskID:         e.TaskID,
			Paths:          paths,
			Excluded:       e.Excluded,
			ExcludedReason: e.ExcludedReason,
		})
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		return out.Entries[i].TaskID < out.Entries[j].TaskID
	})
	return out
}

// Lookup returns one task's ground truth.
func (m GoldManifest) Lookup(taskID string) (Gold, bool) {
	for _, e := range m.Entries {
		if e.TaskID == taskID {
			return e, true
		}
	}
	return Gold{}, false
}

// UnrestrictedRun is one task's result under the unrestricted-ceiling arm.
type UnrestrictedRun struct {
	TaskID string
	Passed bool
	// Invoked is the tool set this run used. Meaningful only when Passed.
	Invoked []string
	// ErrorText is why it failed, so a HARNESS failure is not mistaken for
	// evidence about the task. Without it, a dirty benchmark workspace reads as
	// "the model cannot do this" and the task is dropped from gold on that basis.
	ErrorText string
}

// BuildGold turns unrestricted-ceiling runs into a pinned manifest.
//
// A task the arm never passed is EXCLUDED, not omitted silently: no ground truth
// exists for it, and we cannot distinguish "the policy was too tight" from "the
// task is infeasible for this model". Keeping it would measure the model's
// ceiling and report the result as a policy finding. The exclusion is recorded
// so it is visible in review rather than being an absence nobody notices.
func BuildGold(taskSetSHA256 string, runs []UnrestrictedRun, runCount int) (GoldManifest, error) {
	if taskSetSHA256 == "" {
		return GoldManifest{}, fmt.Errorf("refusing to build gold with no task-set hash: " +
			"the regeneration fence has nothing to compare against, so the gold could be " +
			"silently rebuilt against a different task set")
	}
	// A digest that is not a digest fences against nothing either, and unlike
	// an empty one it looks populated. See validateTaskSetDigest.
	if err := validateTaskSetDigest(taskSetSHA256); err != nil {
		return GoldManifest{}, fmt.Errorf("refusing to build gold: %w", err)
	}

	byTask := map[string][][]string{}
	attempted := map[string]bool{}
	// harnessOnly[t] stays true while every failure for t was the harness's own.
	harnessOnly := map[string]bool{}
	for _, r := range runs {
		if !attempted[r.TaskID] {
			attempted[r.TaskID] = true
			harnessOnly[r.TaskID] = true
		}
		if !r.Passed {
			if ClassifyFailure(false, r.ErrorText) != FailureHarness {
				harnessOnly[r.TaskID] = false
			}
			continue
		}
		harnessOnly[r.TaskID] = false
		byTask[r.TaskID] = append(byTask[r.TaskID], sortedCopy(filterGoldPath(dedupe(r.Invoked))))
	}

	m := GoldManifest{TaskSetSHA256: taskSetSHA256, Runs: runCount}
	for taskID := range attempted {
		paths := byTask[taskID]
		switch {
		case len(paths) == 0 && harnessOnly[taskID]:
			// Every failure was OURS. That is not evidence about the task, so the
			// exclusion says what actually happened rather than implying the
			// model could not do it.
			m.Entries = append(m.Entries, Gold{
				TaskID:   taskID,
				Excluded: true,
				ExcludedReason: "not measured: every run failed inside the harness " +
					"(see the run journal); re-run before treating this as a task the arm cannot pass",
			})
		case len(paths) == 0:
			m.Entries = append(m.Entries, Gold{
				TaskID:         taskID,
				Excluded:       true,
				ExcludedReason: "the unrestricted-ceiling arm never passed this task",
			})
		case allEmpty(paths):
			// A task needing no tools cannot exercise a grant policy, and
			// scoring it would report a perfect coverage nobody earned.
			m.Entries = append(m.Entries, Gold{
				TaskID:         taskID,
				Excluded:       true,
				ExcludedReason: "passing runs invoked no tools: nothing for a grant policy to get right",
			})
		default:
			m.Entries = append(m.Entries, Gold{TaskID: taskID, Paths: paths})
		}
	}
	return m.canonical(), nil
}

func allEmpty(paths [][]string) bool {
	for _, p := range paths {
		if len(p) > 0 {
			return false
		}
	}
	return true
}

// CheckRegeneration refuses to rebuild gold against an unchanged task set.
//
// Rebuilding is permitted only when the task set genuinely changed. To rebuild
// against the same set an operator must delete the pinned manifest, which is a
// REVIEWABLE DIFF — it lands in a commit and shows up in review — rather than an
// invisible re-run. The fence's job is to make ground-truth corruption
// detectable, not impossible: nothing stops someone who can also edit the
// harness, and no design can. This stops the silent version.
func CheckRegeneration(pinned *GoldManifest, currentTaskSetSHA256 string) error {
	if currentTaskSetSHA256 == "" {
		return fmt.Errorf("refusing to regenerate gold: the current task set has no hash, " +
			"so 'has it changed' cannot be answered")
	}
	if pinned == nil {
		return nil // nothing pinned yet: the first build is always permitted.
	}
	if pinned.TaskSetSHA256 == currentTaskSetSHA256 {
		return fmt.Errorf("refusing to regenerate gold: the task set is unchanged (%s). "+
			"Regenerating against an unchanged set replaces the ground truth the gate "+
			"measures against, which makes every prior number incomparable and every "+
			"future one unfalsifiable. If that is genuinely intended, delete the pinned "+
			"manifest — a reviewable diff — and rebuild",
			shortHash(pinned.TaskSetSHA256))
	}
	return nil
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// MergeGold combines per-batch manifests into one.
//
// WHY BATCHING NEEDS THIS. A 54-run pass is hours long, and losing it to a
// session drop or a capacity blip means re-spending all of it. Running in small
// batches caps that loss to one batch — but only if the partial manifests can be
// combined into the single pinned gold the fence and the arm key expect.
//
// Merge rules, in the order they matter:
//
//   - A task with recorded paths in ANY batch keeps them, and paths from several
//     batches accumulate. More runs of the same task is exactly what repeats are
//     for, so a later batch adds routes rather than replacing them.
//   - An exclusion survives only if NO batch recorded a path. A task excluded in
//     one batch and passed in another was measurable; treating it as excluded
//     would drop ground truth we actually have.
//   - Batches must agree on the task-set hash. Merging across task sets would
//     produce a manifest that pins nothing.
func MergeGold(manifests ...GoldManifest) (GoldManifest, error) {
	var out GoldManifest
	byTask := map[string]Gold{}
	runs := 0

	for _, m := range manifests {
		if m.TaskSetSHA256 == "" {
			return GoldManifest{}, fmt.Errorf("refusing to merge a manifest with no task-set hash")
		}
		if err := m.Validate(); err != nil {
			return GoldManifest{}, fmt.Errorf("refusing to merge a manifest: %w", err)
		}
		if out.TaskSetSHA256 == "" {
			out.TaskSetSHA256 = m.TaskSetSHA256
		} else if out.TaskSetSHA256 != m.TaskSetSHA256 {
			return GoldManifest{}, fmt.Errorf("refusing to merge manifests from different task "+
				"sets (%s vs %s): the result would pin neither",
				shortHash(out.TaskSetSHA256), shortHash(m.TaskSetSHA256))
		}
		runs += m.Runs

		for _, e := range m.Entries {
			// Filter at merge as well as at build: the per-batch manifests stay
			// the RAW record of what ran, and the merged gold is the cleaned
			// measuring stick. Keeping both means a later question about what an
			// agent actually did is still answerable.
			filtered := make([][]string, 0, len(e.Paths))
			for _, p := range e.Paths {
				if fp := filterGoldPath(p); len(fp) > 0 {
					filtered = append(filtered, fp)
				}
			}
			e.Paths = filtered

			prev, seen := byTask[e.TaskID]
			switch {
			case !seen:
				byTask[e.TaskID] = e
			case len(e.Paths) > 0:
				// Accumulate routes; a pass anywhere clears an exclusion.
				merged := append(append([][]string{}, prev.Paths...), e.Paths...)
				byTask[e.TaskID] = Gold{TaskID: e.TaskID, Paths: merged}
			case len(prev.Paths) == 0:
				// Both excluded — keep the first reason.
				byTask[e.TaskID] = prev
			}
		}
	}

	out.Runs = runs
	for _, e := range byTask {
		out.Entries = append(out.Entries, e)
	}
	return out.canonical(), nil
}
