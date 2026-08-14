package agentbench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Run journal and pre-registration (§3.2, §7).
//
// THE JOURNAL HOLDS VERDICTS, NOT ROW IDS. tool_audit_log keeps 30 days, so a
// journal that recorded "see execution X" would decay into a pointer at nothing.
// Probes score at run time and their output is what is written down. The
// consequence is stated plainly rather than worked around: historical
// RE-SCORING is not available, and §4's rollup reads this file rather than
// re-querying the ledger.
//
// PRE-REGISTRATION IS A PRECONDITION, NOT A CONVENTION. Choosing what to compare
// after seeing results is the line between a benchmark and a press release, and
// it is the one rule in §7 that is worth nothing if it depends on discipline. So
// a run refuses to start without a committed pre-registration, and its blob hash
// is written into the journal so a report can print it beside every figure.

// PreRegistration is what an operator commits BEFORE a run.
type PreRegistration struct {
	// Arms names the arms being compared. Two or more, because a
	// pre-registration for a single arm registers no comparison.
	Arms []string `json:"arms"`
	// Metric is the one figure this run is commissioned to move.
	Metric string `json:"metric"`
	// TargetDelta is the effect size the run intends to resolve.
	TargetDelta float64 `json:"targetDelta"`
	// SigmaD and SigmaN are the measured noise floor and the n it came from.
	SigmaD float64 `json:"sigmaD"`
	SigmaN int     `json:"sigmaN"`
	// ComputedPairs is the n the formula demands. Recorded here so a run cannot
	// later claim a different requirement.
	ComputedPairs int `json:"computedPairs"`
	// Rationale is why this comparison is worth spending on. Free text, but
	// required: a pre-registration nobody had to think about registers nothing.
	Rationale string `json:"rationale"`
}

// Validate refuses a pre-registration that does not commit to anything.
func (p PreRegistration) Validate() error {
	if len(p.Arms) < 2 {
		return fmt.Errorf("pre-registration needs at least two arms: registering a single "+
			"arm commits to no comparison, got %d", len(p.Arms))
	}
	seen := map[string]bool{}
	for _, a := range p.Arms {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("pre-registration names an empty arm")
		}
		if seen[a] {
			return fmt.Errorf("pre-registration names arm %q twice: an arm compared with "+
				"itself resolves nothing", a)
		}
		seen[a] = true
	}
	if strings.TrimSpace(p.Metric) == "" {
		return fmt.Errorf("pre-registration names no metric: without one, any figure that " +
			"moved can be presented as the intended finding")
	}
	if p.TargetDelta <= 0 {
		return fmt.Errorf("pre-registration sets no target delta: a run with no intended " +
			"effect size cannot be underpowered, which is the point of declaring one")
	}
	if strings.TrimSpace(p.Rationale) == "" {
		return fmt.Errorf("pre-registration has no rationale: one nobody had to think " +
			"about registers nothing")
	}
	return nil
}

// Hash is the pre-registration's identity, printed beside every published
// figure so a reader can check the claim against what was registered.
func (p PreRegistration) Hash() (string, error) {
	blob, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal pre-registration: %w", err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

// RunManifest identifies a run.
type RunManifest struct {
	RunID string    `json:"runId"`
	Arm   ArmFields `json:"arm"`
	// ArmKey and ArmPartial are denormalised so a reader does not have to
	// recompute the key to know whether the run is comparable.
	ArmKey     string `json:"armKey"`
	ArmPartial bool   `json:"armPartial"`

	PreRegistrationHash string          `json:"preRegistrationHash"`
	PreRegistration     PreRegistration `json:"preRegistration"`

	Power PowerCheck `json:"power"`

	// Untrustworthy marks a run whose figures may not be read as results, with
	// the reason. Set rather than refused when the run has already happened:
	// discarding the data would also discard the evidence of why it is
	// untrustworthy.
	Untrustworthy       bool   `json:"untrustworthy,omitempty"`
	UntrustworthyReason string `json:"untrustworthyReason,omitempty"`
}

// Journal is a completed run: its manifest, its per-execution records, and the
// probe verdicts scored while the traces still existed.
type Journal struct {
	Manifest RunManifest       `json:"manifest"`
	Records  []ExecutionRecord `json:"records"`
}

// Write serialises the journal.
func (j Journal) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(j); err != nil {
		return fmt.Errorf("write journal: %w", err)
	}
	return nil
}

// ReadJournal parses a journal.
func ReadJournal(r io.Reader) (Journal, error) {
	var j Journal
	if err := json.NewDecoder(r).Decode(&j); err != nil {
		return Journal{}, fmt.Errorf("read journal: %w", err)
	}
	return j, nil
}

// Rollup computes this run's customer figures from the journal alone.
//
// Never re-queries the ledger: that is the whole reason verdicts are journaled
// rather than referenced, and a rollup that reached for tool_audit_log would
// silently produce partial numbers once the retention window passed.
func (j Journal) Rollup() Rollup {
	return BuildRollup(j.Manifest.Arm.Name, j.Records)
}

// CheckReadable refuses to present a journal's figures as results when the run
// that produced them was not sound.
//
// Returns the reason rather than a bare error so a report can print the warning
// BEFORE the table — a degraded run's figures must not be readable without it.
func (j Journal) CheckReadable() error {
	if j.Manifest.Untrustworthy {
		return fmt.Errorf("this run is marked untrustworthy: %s", j.Manifest.UntrustworthyReason)
	}
	if j.Manifest.PreRegistrationHash == "" {
		return fmt.Errorf("this run has no pre-registration: what it was commissioned to " +
			"compare cannot be established after the fact, so its figures are exploratory " +
			"and may not be published")
	}
	if j.Manifest.ArmPartial {
		return fmt.Errorf("this run's arm key is PARTIAL — an unidentified binary, config " +
			"or task set means comparability is unverified, which is not the same as " +
			"verified-identical")
	}
	return j.Manifest.Power.Refuse()
}

// CompareJournals refuses to diff two runs that are not comparable, and reports
// whether the observed difference clears the floor the runs can actually
// resolve.
//
// A difference below the floor is INCONCLUSIVE and says so, carrying the number
// with it (§5.5): suppressing a real measurement would be its own dishonesty,
// and what inconclusive forbids is the claim, not the figure.
func CompareJournals(a, b Journal, observedDelta float64) (string, error) {
	if err := CheckComparable(a.Manifest.Arm, b.Manifest.Arm); err != nil {
		return "", err
	}
	floor := a.Manifest.Power.ResolvableDelta
	if bf := b.Manifest.Power.ResolvableDelta; bf > floor {
		// The weaker run governs: a comparison is only as resolvable as its
		// least-powered side.
		floor = bf
	}
	if floor > 0 && observedDelta < floor {
		return fmt.Sprintf("delta = %.4f (INCONCLUSIVE, floor %.4f)", observedDelta, floor), nil
	}
	return fmt.Sprintf("delta = %.4f (floor %.4f)", observedDelta, floor), nil
}
