package api

import (
	"strings"
	"time"

	"vornik.io/vornik/internal/outputguard"
)

// Ingress scanning for MCP tool results.
//
// outputguard is the control that flags injection attempts, credential leakage
// and encoded payloads in content on its way to a model. Until 2026-08-26 it
// had exactly two call sites — the daemon's own HTTP fetch tool (`query_api`)
// and the dispatcher chat path — and did NOT see MCP tool results. That is the
// path essentially all third-party content arrives on: Jira ticket bodies,
// Sentry error reports, pages a scraper MCP server returns, whatever an
// operator-supplied server chooses to send back.
//
// So the content least likely to be adversarial was scanned, and the content
// most likely to be adversarial was not — and the shortfall was invisible,
// because `secret_redaction_audit` simply had no rows for MCP calls, which
// reads exactly like "nothing needed redacting".
//
// PHASE 1 IS DETECT-ONLY. The scanned body is returned byte-identical; nothing
// is redacted. The failure mode of a scanner is false positives, and redaction
// that corrupts a legitimate tool result is worse than the gap it closes — so
// the metric soaks first and phase 2 decides redaction per rule class. See
// https://docs.vornik.io §4.
//
// Deliberately NOT a containment claim: an agent role with network egress can
// fetch content directly and never route it past this hook. That is
// `network: daemon-only`'s job (§2.3).

// guardObservation is one scan's outcome, handed to the sink. Carries the
// report rather than a pre-digested count so the sink decides what to record —
// metrics today, an audit row later, a test assertion in between.
type guardObservation struct {
	tool     string
	report   outputguard.Report
	duration time.Duration
}

// guardSink receives one observation per scanned result. Nil is a no-op.
type guardSink func(guardObservation)

// grantsToolPrefix is the daemon's own tool-grant catalogue. Its results are
// composed by the daemon, not by any third party.
const grantsToolPrefix = "mcp__vornik__grant_step_tools"

// provenanceForTool decides which rule classes apply to a tool's result.
//
// Everything is ThirdParty except the daemon's own grant catalogue. The
// precedent is `list_apis` (query-api provider-discovery design, review F4): a
// daemon-built list of names tripped injection-class rules on legitimate
// template syntax, so first-party content skips those. Secret-class rules run
// on everything regardless — first-party governs injection rules only.
//
// `document_*` is UNCONDITIONALLY ThirdParty even though the artifact layer
// routes per origin (FirstParty for origin=task_output). This seam sees a tool
// name and a result string, not the artifact row, so per-origin routing here
// would re-derive a decision that belongs one layer down. The failure direction
// is benign: over-scanning our own output changes nothing in phase 1 and can at
// worst redact something we wrote in phase 2. Under-scanning is the failure
// that matters.
func provenanceForTool(qualifiedName string) outputguard.Provenance {
	if strings.HasPrefix(qualifiedName, grantsToolPrefix) {
		return outputguard.ProvenanceFirstParty
	}
	return outputguard.ProvenanceThirdParty
}

// scanResult runs the guard over a tool result and reports what it found.
//
// Returns the body UNCHANGED — phase 1 is detect-only, and that is the property
// TestPhase1DoesNotAlterContent pins.
//
// Fails open on panic, for the reason the dispatcher's guard states: a
// malformed pattern that panicked would otherwise take down every tool call.
// The worst case must be "the guard didn't fire", never "the tool call failed".
func (c *ComposedMCPExecutor) scanResult(qualifiedName, body string) {
	if c == nil || c.GuardSink == nil || body == "" {
		// An empty result has nothing to scan and should not pay the fixed
		// cost; a nil sink is the unwired case (lean deployments, tests).
		return
	}
	defer func() { _ = recover() }()

	start := time.Now()
	rep := outputguard.ScanWithProvenance(body, provenanceForTool(qualifiedName))
	// Timed even when there is no finding: a clean scan still exercises the
	// full rule set, and the latency floor is exactly what the soak needs in
	// order to confirm or refute the design's §3 microbenchmark. Same reasoning
	// as the dispatcher's guard.
	c.GuardSink(guardObservation{
		tool:     qualifiedName,
		report:   rep,
		duration: time.Since(start),
	})
}
