package memory

// Phase B of the Policy-Aware Memory Firewall — wires the
// evaluator into the recall hot path. Adds RecallWithContext
// alongside the legacy Search / SearchWithOptions surfaces so
// existing callers stay shape-compatible. Operators opt in to
// firewall-aware retrieval via the new entry point.
//
// Design notes (mirrors LLD § "Retrieval-side wiring"):
//   - Evaluator runs against each result. Allowed chunks keep
//     their place + get a PolicyProof; blocked chunks under
//     EnforcementEnforce drop from the result set, under
//     EnforcementAdvisory carry a PolicyWarning string,
//     under EnforcementOff are unchanged.
//   - One audit row per chunk decision is queued on the
//     non-blocking writer goroutine.
//   - Policy columns are batch-loaded after the search query
//     runs (one extra indexed SELECT per recall). Avoids
//     restructuring the existing hybrid-search SQL.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/memoryfirewall"
)

// FirewallDeps bundles the Searcher's optional firewall wiring.
// Nil at construction time = firewall disabled (legacy behaviour).
// SetFirewall installs the deps post-construction so the
// container can wire them after the memory package is built.
type FirewallDeps struct {
	Evaluator *memoryfirewall.Evaluator
	Writer    *memoryfirewall.AuditWriter
	// EnforcementMode is the daemon-level default applied when
	// no per-project override resolves. Operators flip this via
	// VORNIK_MEMORY_FIREWALL_MODE.
	EnforcementMode memoryfirewall.EnforcementMode
	// ModeForProject resolves a per-project override (Phase D
	// follow-on of the firewall LLD). Nil = no per-project
	// resolver wired; daemon default applies to every recall.
	// Production wires a registry lookup that reads
	// ProjectFirewall.Mode; tests pass a static map-backed func.
	ModeForProject func(projectID string) (memoryfirewall.EnforcementMode, bool)
	// Metrics observes the recall-side firewall series
	// (decisions_total + eval_duration_seconds). Nil = metrics not
	// wired (SQLite / tests); every observe call is nil-safe. See
	// internal/memoryfirewall/metrics.go (LLD § Observability /
	// drift-mitigation §8.3).
	Metrics *memoryfirewall.Metrics
}

// SetFirewall installs the firewall dependencies. Nil deps =
// disable (back to legacy retrieval). Subsequent SetFirewall
// calls atomically replace the previous deps; concurrent
// recall calls see either the old or the new wiring, never a
// torn state.
func (s *Searcher) SetFirewall(deps *FirewallDeps) {
	if s == nil {
		return
	}
	s.firewall = deps
}

// SetFirewallMetrics wires the recall-side Prometheus metrics onto an
// already-installed FirewallDeps. Called after observability boots
// (the metrics registry isn't available at SetFirewall time). No-op
// when the firewall isn't wired. Concurrent recalls see either the
// old (nil) or new metrics — both are nil-safe at the observe sites.
func (s *Searcher) SetFirewallMetrics(m *memoryfirewall.Metrics) {
	if s == nil || s.firewall == nil {
		return
	}
	s.firewall.Metrics = m
}

// firewallDeps returns the active firewall config or nil.
// Helper so the recall path doesn't have to write the
// nil-and-mode check inline.
func (s *Searcher) firewallDeps() *FirewallDeps {
	if s == nil || s.firewall == nil {
		return nil
	}
	if s.firewall.Evaluator == nil {
		return nil
	}
	return s.firewall
}

// RecallWithContext is the firewall-aware retrieval entry point
// that lets the caller supply a RequestContext (operator_id,
// role, purpose, trace_id). The legacy Search / SearchWithOptions
// surfaces now ALSO run through the firewall — they just pass an
// empty RequestContext, which under default policies allows
// everything but still writes the audit trail. So
// RecallWithContext is the right call from any path that has
// operator metadata to attach; Search() is the right call from
// any path that doesn't (yet).
//
// Three enforcement modes (daemon-level config, mode set at
// boot via VORNIK_MEMORY_FIREWALL_MODE):
//
//   - off       → every hit returned unchanged; audit row
//     still recorded so operators see what WOULD
//     have been blocked.
//   - advisory  → blocked chunks stay in the result but carry
//     a PolicyWarning string.
//   - enforce   → blocked chunks drop from the result entirely.
func (s *Searcher) RecallWithContext(
	ctx context.Context,
	projectID, query string,
	opts SearchOptions,
	reqCtx memoryfirewall.RequestContext,
) ([]SearchResult, error) {
	results, err := s.searchInternal(ctx, projectID, query, opts)
	if err != nil {
		return nil, err
	}
	return s.applyFirewall(ctx, projectID, results, reqCtx), nil
}

// applyFirewall is the post-search firewall pass shared by
// every recall entry point. Called with the caller's
// RequestContext (or an empty one from legacy Search /
// SearchWithOptions paths). Nil firewall = passthrough; the
// caller sees the original result slice.
//
// 2026-05-29 default-on change: the legacy Search /
// SearchWithOptions paths now also flow through this helper so
// the firewall's audit trail captures EVERY recall regardless
// of caller. RequestContext is empty (no operator_id / role /
// purpose); under default policies that allows everything but
// records the decision for operator visibility.
func (s *Searcher) applyFirewall(
	ctx context.Context,
	projectID string,
	results []SearchResult,
	reqCtx memoryfirewall.RequestContext,
) []SearchResult {
	fw := s.firewallDeps()
	if fw == nil || len(results) == 0 {
		return results
	}
	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.ChunkID)
	}
	policies, err := s.repo.LoadChunkPolicies(ctx, ids)
	if err != nil {
		// Policy load failed — degrade to legacy behaviour
		// rather than block all retrieval. The operator sees
		// the failure in the daemon logs.
		s.logger.Warn().Err(err).Msg("memoryfirewall: policy load failed, returning unfiltered results")
		return results
	}

	// Resolve effective enforcement mode for this project. Per-
	// project override wins; daemon default falls through. The
	// resolver is a single map read in production (registry
	// holds the ProjectFirewall struct in memory); no per-recall
	// DB call.
	mode := resolveMode(fw, projectID)

	enforced := make([]SearchResult, 0, len(results))
	evalStart := time.Now()
	evaluatedAt := evalStart.UTC()
	for _, r := range results {
		chunk := chunkFromPolicyRow(r.ChunkID, policies[r.ChunkID])
		decision, reason := fw.Evaluator.Decide(chunk, reqCtx)
		// memory_firewall_decisions_total{project, decision}. Nil-safe.
		fw.Metrics.ObserveDecision(projectID, decision)

		fw.Writer.Enqueue(memoryfirewall.EvaluationRow{
			ID:              newEvaluationRowID(),
			ProjectID:       projectID,
			TenantID:        reqCtx.TenantID,
			ChunkID:         r.ChunkID,
			RequestRole:     reqCtx.Role,
			RequestPurpose:  string(reqCtx.Purpose),
			RequestOperator: reqCtx.OperatorID,
			TraceID:         reqCtx.TraceID,
			Decision:        decision,
			PolicyDigest:    chunk.Digest,
			ReasonDetail:    reason,
			EvaluatedAt:     evaluatedAt,
		})

		switch mode {
		case memoryfirewall.EnforcementEnforce:
			if decision != memoryfirewall.DecisionAllow {
				continue // drop
			}
		case memoryfirewall.EnforcementAdvisory:
			if decision != memoryfirewall.DecisionAllow {
				r.PolicyWarning = string(decision) + ": " + reason
			}
		default: // EnforcementOff
		}

		r.PolicyProof = &PolicyProofWire{
			ChunkID:      r.ChunkID,
			Decision:     string(decision),
			EvaluatedAt:  evaluatedAt,
			PolicyDigest: chunk.Digest,
			RequestContext: PolicyProofRequestContext{
				TenantID:   reqCtx.TenantID,
				OperatorID: reqCtx.OperatorID,
				Role:       reqCtx.Role,
				Purpose:    string(reqCtx.Purpose),
				TraceID:    reqCtx.TraceID,
			},
		}
		enforced = append(enforced, r)
	}
	// memory_firewall_eval_duration_seconds{project} — one observation
	// per recall's firewall pass (covers the whole chunk set). Nil-safe.
	fw.Metrics.ObserveEval(projectID, time.Since(evalStart))
	return enforced
}

// resolveMode resolves the effective enforcement mode for a
// project: the per-project override wins, the daemon default falls
// through. Extracted so applyFirewall and RecentWithContext share
// one resolution path — both the recall and recent-memory surfaces
// must reach the SAME mode for a given project, otherwise a chunk
// blocked by recall could still leak through recent_memory.
func resolveMode(fw *FirewallDeps, projectID string) memoryfirewall.EnforcementMode {
	mode := fw.EnforcementMode
	if fw.ModeForProject != nil {
		if pm, ok := fw.ModeForProject(projectID); ok {
			mode = pm
		}
	}
	return mode
}

// RecentWithContext is the firewall-aware entry point for the
// companion `recent_memory` digest. It wraps
// Repository.ListRecentChunksWithOptions and runs every returned
// row through the SAME firewall decision the recall path uses
// (LoadChunkPolicies → resolveMode → Evaluator.Decide → audit
// Enqueue → enforce-drop / advisory-annotate). Without this the
// recent_memory tool was a firewall bypass: a chunk that recall
// drops under enforce (credentials, refuted, expired policy) would
// still surface verbatim in the recent-memory digest.
//
// Nil firewall (or nil evaluator) = passthrough, identical to the
// legacy ListRecentChunksWithOptions result, so deployments that
// haven't wired the firewall see no behaviour change. Under
// EnforcementEnforce non-Allow rows are dropped; under
// EnforcementAdvisory they remain but carry a PolicyWarning; under
// EnforcementOff every row is returned unchanged. One audit row is
// queued per row decision regardless of mode, matching applyFirewall.
func (s *Searcher) RecentWithContext(
	ctx context.Context,
	projectID string,
	limit int,
	repoScope string,
	strictScope, onlyUntagged bool,
	reqCtx memoryfirewall.RequestContext,
) ([]RecentChunkRow, error) {
	if s == nil || s.repo == nil {
		return nil, nil
	}
	rows, err := s.repo.ListRecentChunksWithOptions(ctx, projectID, limit, repoScope, strictScope, onlyUntagged)
	if err != nil {
		return nil, err
	}
	fw := s.firewallDeps()
	if fw == nil || len(rows) == 0 {
		return rows, nil
	}

	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ChunkID)
	}
	policies, perr := s.repo.LoadChunkPolicies(ctx, ids)
	if perr != nil {
		// Policy load failed — degrade to legacy behaviour rather
		// than block the whole digest. Mirrors applyFirewall's
		// fail-open-with-warning posture; the operator sees it in
		// the daemon logs.
		s.logger.Warn().Err(perr).Msg("memoryfirewall: recent-memory policy load failed, returning unfiltered digest")
		return rows, nil
	}

	mode := resolveMode(fw, projectID)
	out := make([]RecentChunkRow, 0, len(rows))
	evalStart := time.Now()
	evaluatedAt := evalStart.UTC()
	for _, r := range rows {
		chunk := chunkFromPolicyRow(r.ChunkID, policies[r.ChunkID])
		decision, reason := fw.Evaluator.Decide(chunk, reqCtx)
		fw.Metrics.ObserveDecision(projectID, decision)
		fw.Writer.Enqueue(memoryfirewall.EvaluationRow{
			ID:              newEvaluationRowID(),
			ProjectID:       projectID,
			TenantID:        reqCtx.TenantID,
			ChunkID:         r.ChunkID,
			RequestRole:     reqCtx.Role,
			RequestPurpose:  string(reqCtx.Purpose),
			RequestOperator: reqCtx.OperatorID,
			TraceID:         reqCtx.TraceID,
			Decision:        decision,
			PolicyDigest:    chunk.Digest,
			ReasonDetail:    reason,
			EvaluatedAt:     evaluatedAt,
		})

		switch mode {
		case memoryfirewall.EnforcementEnforce:
			if decision != memoryfirewall.DecisionAllow {
				continue // drop — same as recall under enforce
			}
		case memoryfirewall.EnforcementAdvisory:
			if decision != memoryfirewall.DecisionAllow {
				r.PolicyWarning = string(decision) + ": " + reason
			}
		default: // EnforcementOff
		}
		out = append(out, r)
	}
	fw.Metrics.ObserveEval(projectID, time.Since(evalStart))
	return out, nil
}

// chunkFromPolicyRow lifts a ChunkPolicyRow into the firewall's
// Chunk shape. Handles the NULL columns (legacy chunks) by
// falling back to DefaultPolicyForSource — the lazy-backfill
// path from the LLD's Phase A description.
func chunkFromPolicyRow(chunkID string, row ChunkPolicyRow) memoryfirewall.Chunk {
	source := memoryfirewall.ProvenanceUnknown
	if row.ProvenanceSource != "" {
		source = memoryfirewall.ProvenanceSource(row.ProvenanceSource)
	}
	// If the chunk has NO policy columns set (legacy), the
	// effective policy is DefaultPolicyForSource. Otherwise we
	// build the policy from the loaded columns.
	hasPolicyData := row.SensitivityTier != "" ||
		row.ProvenanceSource != "" ||
		len(row.PermittedRoles) > 0 ||
		len(row.AllowedPurposes) > 0 ||
		row.FirewallExpiresAt != nil ||
		row.TenantID != ""

	var policy memoryfirewall.Policy
	if hasPolicyData {
		policy = memoryfirewall.Policy{
			Provenance: memoryfirewall.Provenance{
				Source:     source,
				ProducerID: row.ProvenanceProducer,
				TrustLevel: row.ProvenanceTrust,
				SourceURL:  row.ProvenanceURL,
			},
			Sensitivity:     memoryfirewall.SensitivityTier(row.SensitivityTier),
			ExpiresAt:       row.FirewallExpiresAt,
			TenantID:        row.TenantID,
			PermittedRoles:  row.PermittedRoles,
			AllowedPurposes: purposesFromStrings(row.AllowedPurposes),
		}
	} else {
		policy = memoryfirewall.DefaultPolicyForSource(source, row.ContentClass)
	}
	// Apply the classifier bridge — read-time policy
	// adjustment for credentials / refuted chunks.
	policy = memoryfirewall.ApplyClassifierSignal(policy, row.ContentClass, row.ValidationStatus)

	// Use the stored digest when present; recompute if
	// classifier bridge changed the policy or the chunk is
	// on default policy.
	digest := row.PolicyDigest
	if digest == "" {
		digest = memoryfirewall.PolicyDigest(policy)
	}
	return memoryfirewall.Chunk{
		ID:     chunkID,
		Policy: policy,
		Digest: digest,
	}
}

func purposesFromStrings(in []string) []memoryfirewall.Purpose {
	if len(in) == 0 {
		return nil
	}
	out := make([]memoryfirewall.Purpose, 0, len(in))
	for _, s := range in {
		out = append(out, memoryfirewall.Purpose(s))
	}
	return out
}

// newEvaluationRowID returns a fresh evaluation row ID. 16
// random bytes hex-encoded; enough entropy to avoid collisions
// even at high recall throughput.
func newEvaluationRowID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "ev_" + hex.EncodeToString(b[:])
}

// silence unused-import warning if zerolog ends up unused in
// future refactors; the file's main use is via s.logger.
var _ = zerolog.Nop
