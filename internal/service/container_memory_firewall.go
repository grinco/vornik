package service

// Helpers for the Policy-Aware Memory Firewall's Container
// integration. The firewall's heavy lifting lives in
// internal/memoryfirewall/; this file just wires the lifecycle
// + config glue.

import (
	"context"
	"os"
	"strings"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/ui"
)

// memoryFirewallMode reads the daemon-level enforcement mode.
//
// Resolution order:
//  1. VORNIK_MEMORY_FIREWALL_MODE env var (ops-emergency
//     override; lands without touching config.yaml).
//  2. config.Memory.Firewall.Mode (Phase D YAML shape — not
//     yet defined; falls through to the default).
//  3. Default = "advisory".
//
// Default flipped 2026-05-29 from "off" to "advisory" as part
// of the "make firewall default behavior for new installations"
// change. Advisory means: evaluator runs, audit row written
// per chunk, blocked chunks STILL surface in results but
// carry a PolicyWarning string. No workflow breakage; full
// operator visibility into what the firewall would block under
// stricter modes. Operators who want pure-off can pin via the
// env var; operators ready for enforce mode flip it the same
// way.
func (c *Container) memoryFirewallMode() memoryfirewall.EnforcementMode {
	if c == nil {
		return memoryfirewall.EnforcementAdvisory
	}
	if env := strings.ToLower(strings.TrimSpace(envOrEmpty("VORNIK_MEMORY_FIREWALL_MODE"))); env != "" {
		return normalizeFirewallMode(env)
	}
	// Phase D YAML config (c.Config.Memory.Firewall.Mode)
	// lands in a follow-on. Until then, advisory is the
	// default — see commit message for the upgrade-behaviour
	// note.
	return memoryfirewall.EnforcementAdvisory
}

// memoryFirewallEditor returns the per-chunk policy-edit
// adapter for POST /api/v1/admin/memory/policy/chunks/{id}.
// Nil when memory isn't wired (SQLite-no-memory or
// memory-disabled deployments). Production wiring threads the
// underlying memory.Repository so the digest-recompute happens
// in-process before the UPDATE lands.
func (c *Container) memoryFirewallEditor() api.MemoryFirewallEditor {
	if c == nil || c.memoryManager == nil {
		return nil
	}
	return newMemoryFirewallEditorAdapter(c.memoryManager.Repository())
}

// uiFirewallEditor returns the UI-side firewall editor adapter
// for /ui/admin/memory/firewall/chunks/{id}. Same underlying
// *memory.Repository as the api adapter; lives in a separate
// ui-package interface so the UI doesn't import internal/api.
func (c *Container) uiFirewallEditor() ui.FirewallEditor {
	if c == nil || c.memoryManager == nil {
		return nil
	}
	return newUIFirewallEditorAdapter(c.memoryManager.Repository())
}

// memoryFirewallModeForProject resolves the per-project
// enforcement-mode override. Returns (mode, true) when the
// project authored a non-empty Firewall.Mode in its YAML;
// (zero, false) otherwise so the Searcher falls through to the
// daemon default. Registry lookup is in-memory; no DB call.
//
// Project mode is normalised via the same coerce-empty-to-
// advisory path the daemon default uses — operator typos
// ("STRICT" / "Off") get folded to the closest valid value.
func (c *Container) memoryFirewallModeForProject(projectID string) (memoryfirewall.EnforcementMode, bool) {
	if c == nil || c.Registry == nil || projectID == "" {
		return memoryfirewall.EnforcementOff, false
	}
	p := c.Registry.GetProject(projectID)
	if p == nil {
		return memoryfirewall.EnforcementOff, false
	}
	raw := strings.TrimSpace(p.Firewall.Mode)
	if raw == "" {
		return memoryfirewall.EnforcementOff, false
	}
	return normalizeFirewallMode(raw), true
}

// startMemoryFirewallWriter launches the audit writer's
// flusher goroutine. Called from Container.Run after the DB
// is fully wired (the writer's sink is a DB-backed repo).
// No-op when the writer wasn't wired (SQLite branch or
// firewall disabled).
func (c *Container) startMemoryFirewallWriter(ctx context.Context) {
	if c == nil || c.memoryFirewallWriter == nil {
		return
	}
	c.memoryFirewallWriter.Start(ctx)
	c.Logger.Info().Msg("memory firewall audit writer started")
}

// stopMemoryFirewallWriter drains pending rows + waits for
// the flusher goroutine to exit. Called during shutdown.
// Best-effort; the daemon's drain ctx bounds the wait.
func (c *Container) stopMemoryFirewallWriter() {
	if c == nil || c.memoryFirewallWriter == nil {
		return
	}
	c.memoryFirewallWriter.Stop()
}

// normalizeFirewallMode coerces operator-supplied strings to
// the typed mode enum. Empty / unknown values fall back to
// EnforcementAdvisory — matching the daemon default. Operators
// who want the firewall completely off must spell "off"
// explicitly.
func normalizeFirewallMode(s string) memoryfirewall.EnforcementMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "enforce":
		return memoryfirewall.EnforcementEnforce
	case "advisory":
		return memoryfirewall.EnforcementAdvisory
	case "off":
		return memoryfirewall.EnforcementOff
	case "":
		return memoryfirewall.EnforcementAdvisory
	default:
		return memoryfirewall.EnforcementAdvisory
	}
}

// envOrEmpty returns os.Getenv with a defensive empty-key guard.
func envOrEmpty(key string) string {
	if key == "" {
		return ""
	}
	return os.Getenv(key)
}

// memoryFirewallEditionGatePasses reports whether the container's edition
// includes the memory firewall AND the inner Postgres-repo gate is satisfied.
// This is the exact two-condition check that initScheduler evaluates; a
// dedicated predicate makes it directly unit-testable without invoking the
// full (DB-requiring) initScheduler path.
//
// Fidelity note: the test exercises this predicate directly. It does NOT
// invoke initScheduler end-to-end. The behavioural equivalence holds because
// initScheduler's wiring block is:
//
//	if !c.providers.MemoryFirewall { … omit … }
//	else if c.repos.MemoryPolicyEvaluations != nil { … wire … }
//
// which is identical to `providers.MemoryFirewall && repo != nil`.
func (c *Container) memoryFirewallEditionGatePasses() bool {
	if c == nil {
		return false
	}
	return c.providers.MemoryFirewall && c.repos != nil && c.repos.MemoryPolicyEvaluations != nil
}
