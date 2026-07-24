package service

// Container wiring for taint-lineage tracking (taint-lineage-tracking-design.md).
// Resolves the effective enforcement mode (project override → daemon default)
// and builds the forge-write TaintReviewer the open_change_request handler
// consults. The pure classification/decision logic lives in
// internal/taintlineage; this is config + repo glue.

import (
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/taintlineage"
)

// taintDefaultMode returns the daemon-default enforcement mode string
// (off|advisory|enforce). The value was hard-validated at config load, so an
// error here is unreachable; default to the observe-first "advisory".
func (c *Container) taintDefaultMode() string {
	if c == nil {
		return "advisory"
	}
	mode, err := c.Config.TaintLineage.TaintLineageMode()
	if err != nil {
		return "advisory"
	}
	return mode
}

// taintModeForProject resolves the effective enforcement mode for a project:
// a non-empty per-project override wins, else the daemon default. Registry
// lookup is in-memory; no DB call. Invalid overrides coerce to advisory
// (fail-safe) — the daemon default was already validated at load.
func (c *Container) taintModeForProject(projectID string) taintlineage.Mode {
	override := ""
	if c != nil && c.Registry != nil && projectID != "" {
		if p := c.Registry.GetProject(projectID); p != nil {
			override = p.TaintLineage.Mode
		}
	}
	return taintlineage.EffectiveMode(override, c.taintDefaultMode())
}

// recordForgeTaintWrite routes a forge-surface taint outcome (flagged/parked)
// to the shared vornik_taint_writes_total counter, so both write surfaces land
// on ONE metric (single registration). Read lazily at call time: the counter is
// wired during HTTP setup, always before any task runs. No-op until wired.
func (c *Container) recordForgeTaintWrite(mode taintlineage.Mode, outcome string) {
	if c == nil || c.taintWriteMetrics == nil || c.taintWriteMetrics.WritesTotal == nil {
		return
	}
	c.taintWriteMetrics.WritesTotal.WithLabelValues(string(mode), "forge", outcome).Inc()
}

// newTaintReviewer builds the forge-write taint gate. Nil-safe on missing repos
// (the reviewer degrades to "no park", keeping the feature inert until fully
// wired).
func (c *Container) newTaintReviewer() *executor.TaintReviewer {
	if c == nil || c.repos == nil {
		return nil
	}
	return executor.NewTaintReviewer(
		c.repos.StepOutcomes,
		c.repos.Tasks,
		c.repos.Messages,
		c.taintModeForProject,
		c.recordForgeTaintWrite,
	)
}
