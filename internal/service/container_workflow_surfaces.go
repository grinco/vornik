package service

// Shared workflow-surface helpers.
//
// These outlived internal/memetic's deletion (WP9 step 3, 2026-09-16). They
// were defined beside the architect adapter because the architect was their
// first caller, but none of them is architect-specific: the proposals UI
// reads the on-disk workflow for its diff panel, the impact panel reads the
// same telemetry rollup, and the instinct metrics are shared across the
// worker, the executor and the recovery resolver.
//
// Keeping them here rather than in the deleted file is the whole point of
// the separation: what went away is the LLM proposer, not the surfaces that
// happened to sit next to it.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/safepath"
	"vornik.io/vornik/internal/workflowtelemetry"
)

// fsWorkflowSource implements memetic.WorkflowSource by reading
// <configDir>/workflows/<id>.md from disk. configDir is the
// deployed configs tree (~/.config/vornik/configs in dev), NOT the
// source tree, so the daemon always reads what the operator
// promoted — matches the broader two-trees discipline.
type fsWorkflowSource struct {
	configDir string
}

func (s *fsWorkflowSource) Load(_ context.Context, workflowID string) ([]byte, error) {
	if workflowID == "" {
		return nil, fmt.Errorf("fsWorkflowSource: empty workflowID")
	}
	if s.configDir == "" {
		return nil, fmt.Errorf("fsWorkflowSource: configDir not set")
	}
	// Anchored join via the canonical helper. workflowID is operator-supplied
	// at the admin endpoint, so JoinUnderRel defends against `..` traversal,
	// symlink escape, AND an absolute workflowID (which JoinUnder would collapse
	// under workflowsDir rather than reject), pinning the path inside workflowsDir.
	candidate, err := safepath.JoinUnderRel(filepath.Join(s.configDir, "workflows"), workflowID+".md")
	if err != nil {
		return nil, fmt.Errorf("fsWorkflowSource: workflowID escapes workflows directory: %w", err)
	}
	data, err := os.ReadFile(candidate)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// sharedInstinctMetrics returns the process-wide *observability.InstinctMetrics,
// creating it once against the observability registry and caching it on the
// Container (TRACK ARCH-METRIC). observability.NewInstinctMetrics registers
// collectors via promauto, which PANICS on a duplicate registration — so every
// consumer (worker, executor, recovery resolver, workflow architect) must share
// this one instance. Returns nil when observability is disabled (no registry);
// callers are nil-safe. Safe to call from the second initHTTPServer pass (where
// the registry already exists) and again from wireComponentMetrics — the second
// call returns the cached value rather than re-registering.
func (c *Container) sharedInstinctMetrics() *observability.InstinctMetrics {
	if c == nil {
		return nil
	}
	if c.instinctMetrics != nil {
		return c.instinctMetrics
	}
	reg := c.observabilityRegistry()
	if reg == nil {
		return nil
	}
	c.instinctMetrics = observability.NewInstinctMetrics(reg)
	return c.instinctMetrics
}

// workflowRollupSource bridges *workflowtelemetry.Service to
// memetic.TelemetrySource. Distinct from workflowTelemetryAdapter
// because the api-package adapter returns any (for JSON); memetic
// needs the typed *Rollup.
type workflowRollupSource struct {
	svc *workflowtelemetry.Service
}

func (m *workflowRollupSource) ForWorkflow(ctx context.Context, workflowID string, since time.Time) (*workflowtelemetry.Rollup, error) {
	return m.svc.ForWorkflow(ctx, workflowID, since)
}
