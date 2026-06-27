// Package workflowhealing implements the Self-Healing Workflow
// Genome v1 trial runner, scorecard, and promotion-gate logic. The
// detector + trigger ledger shipped under the Black Box arc; this
// package builds the candidate-evaluation layer on top of it.
//
// This file carries the genome-hash helper — the deterministic
// fingerprint of a workflow's structure used for the
// baseline_genome_hash / candidate_genome_hash columns on a healing
// candidate. The hash is the same SHA-256-prefix the executor uses
// to detect WORKFLOW_DRIFT (registry.Workflow.Hash), so a candidate
// hash is directly comparable to the live workflow's hash.
package workflowhealing

import (
	"fmt"

	"vornik.io/vornik/internal/registry"
)

// GenomeHash returns the stable structural fingerprint of a parsed
// workflow. It delegates to registry.Workflow.Hash so the value is
// identical to the revision pin the executor stamps on every
// execution — two proposals with different whitespace/comments but
// identical structure hash the same; any step/transition/budget
// change produces a different hash.
//
// A nil workflow hashes to the empty string (the same contract as
// registry.Workflow.Hash), so callers can treat "" as "no genome".
func GenomeHash(wf *registry.Workflow) string {
	return wf.Hash()
}

// GenomeHashFromMarkdown parses a WORKFLOW.md document and returns
// its genome hash. This is the path the candidate generator takes to
// fingerprint a proposal's ProposalYAML: the proposal carries the
// full WORKFLOW.md text, so parse-then-hash yields the candidate
// genome hash. The filename is forwarded into parse errors so a
// malformed proposal is traceable.
//
// A parse failure is returned to the caller rather than swallowed —
// an unparseable candidate genome must not be fingerprinted as if it
// were valid, or the promotion gate would compare against a bogus
// hash.
func GenomeHashFromMarkdown(content []byte, filename string) (string, error) {
	wf, err := registry.ParseWorkflowMarkdown(content, filename)
	if err != nil {
		return "", fmt.Errorf("workflowhealing: genome hash: %w", err)
	}
	return GenomeHash(wf), nil
}
