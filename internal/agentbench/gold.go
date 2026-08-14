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

	byTask := map[string][][]string{}
	attempted := map[string]bool{}
	for _, r := range runs {
		attempted[r.TaskID] = true
		if !r.Passed {
			continue
		}
		byTask[r.TaskID] = append(byTask[r.TaskID], sortedCopy(dedupe(r.Invoked)))
	}

	m := GoldManifest{TaskSetSHA256: taskSetSHA256, Runs: runCount}
	for taskID := range attempted {
		paths := byTask[taskID]
		switch {
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
