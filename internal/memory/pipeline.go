package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// PipelineConfig wires the pipeline's external dependencies.
// Quarantine repo is required; everything else is optional and
// nil-safe (skips the corresponding gate or stage).
type PipelineConfig struct {
	Quarantine      persistence.MemoryQuarantineRepository
	Epochs          persistence.CorpusEpochRepository
	ChunkExists     func(ctx context.Context, projectID, contentHash string) (bool, error)
	StampEpoch      func(ctx context.Context, projectID, artifactID, epochID string) error
	SecretsDetector secrets.Detector
	SecretsActions  map[string]secrets.Action
	// DenyPatterns seeds the substring deny-list PolicyMatchGate enforces.
	// It is published into the hot-reloadable gateOverrides snapshot at
	// NewPipeline time; thereafter the live value is swapped via UpdateGates
	// (config.yaml reload), so reading p.cfg.DenyPatterns post-construction
	// would be stale — the gate reads p.denyPatternsSnapshot() instead.
	// Substring (NOT regex), so ReDoS-immune by construction.
	DenyPatterns []string
	GateConfig   GateConfig
	// PromptInjectionAction sets the prompt-injection gate mode
	// (off|detect|quarantine). Carried at the top level (like
	// DenyPatterns) so it survives NewPipeline's GateConfig default-fill,
	// which only triggers when MinContent* are unset.
	PromptInjectionAction string
	// ClaimAuditDisabledProjects lists project IDs for which the
	// claim-audit (hallucination Phase-1) gate is skipped — a per-project
	// escape hatch for false-positive-prone projects.
	ClaimAuditDisabledProjects []string
	Logger                     zerolog.Logger
	Metrics                    *Metrics
	// AuditLookup resolves extracted claims against tool_audit_log
	// for the candidate's IngestExecutionID. nil-safe: when unset
	// (or when the candidate has no execution_id), the claim audit
	// gate auto-allows because there's nothing to verify against.
	// See https://docs.vornik.io
	AuditLookup AuditLookupFunc

	// Classifier, when non-nil AND ClassifierInlineFallback is true,
	// gives the ingest hot path a fall-back classification pass
	// for artifacts whose producer_role doesn't resolve to a known
	// class (empty role, role not in roleClassMap). Measure 3 of
	// the 2026-05-15 hardening: when the deterministic role-map
	// can't pin a class, run the LLM classifier inline rather than
	// stamping `unclassified` and waiting for the auto-backfill loop
	// to clean it up. Trades ingest latency for fresh class labels.
	// Nil-safe — when unset, ingest behaves as it did pre-fix.
	Classifier *Classifier
	// ClassifierInlineFallback gates whether the Classifier above
	// actually runs at ingest. Operators may wire a Classifier for
	// the backfill API but keep this off to avoid the per-artifact
	// LLM call. Defaults to false to preserve existing behaviour on
	// upgrade.
	ClassifierInlineFallback bool

	// CreateCompanionArtifact inserts a synthetic artifacts row for
	// a companion-deposited note before chunks are upserted.
	// project_memory_chunks.artifact_id has a FK to artifacts.id, so
	// the synthetic artifactID minted in IngestCompanionNote needs a
	// parent row in artifacts(id) before the chunk upsert runs. When
	// nil, IngestCompanionNote fails with a clear error.
	CreateCompanionArtifact func(ctx context.Context, projectID, artifactID, sourceName string, sizeBytes int64) error

	// RecordCompanionIngest writes one row to memory_ingest_audit
	// per IngestCompanionNote call regardless of gate decision.
	// Closes the audit-log gap LLD-22's "reuse queue.producer_role"
	// approach left open for companion-direct deposits (which bypass
	// the queue entirely). Nil-safe: when unset, ingest behaves as
	// it did pre-fix and no audit row is written. Stays nil-safe so
	// tests + lean deployments don't need to wire it.
	RecordCompanionIngest func(ctx context.Context, audit CompanionIngestAuditEvent) error

	// RecordAgentIngest writes one memory_ingest_audit row per Path B
	// (workflow-async / queue-drained agent) ingest, regardless of
	// gate decision. Path B is the bulk of project memory, but before
	// the 2026-05-29 audit (finding #4) only Path A (IngestCompanionNote
	// → RecordCompanionIngest) wrote audit rows — agent ingests left no
	// trail, and the memory-ingest-paths LLD §5 failure-mode tree
	// actively misrouted operators. Invoked from
	// IngestArtifactWithOptions for admitted / quarantined / rejected
	// candidates; companion-prefixed producer roles are skipped here
	// because IngestCompanionNote already audits them via
	// RecordCompanionIngest (avoids double-recording). Nil-safe: unset
	// = no audit row, behaves as pre-fix.
	// See https://docs.vornik.io § 4 & § 5.
	RecordAgentIngest func(ctx context.Context, audit AgentIngestAuditEvent) error
}

// Pipeline orchestrates the per-artifact ingest flow:
//
//	candidates → standard gates → (quarantine | publish)
//
// Phase 2 ships the deterministic gate stack + quarantine routing
// + role-based classification. Phase 3 adds epoch tagging at
// publish; Phase 4 layers LLM validator + supersession on top.
//
// One Pipeline instance is shared across all projects/workers —
// per-call state lives on the stack.
type Pipeline struct {
	cfg     PipelineConfig
	indexer *Indexer
	logger  zerolog.Logger
	// hotGates holds the ingest-gate knobs that are safe to hot-apply on
	// a config.yaml reload — prompt_injection_scan,
	// claim_audit_disabled_projects and deny_patterns. They're read
	// per-ingest, so a config reload swaps them via an atomic.Pointer rather
	// than rebuilding the pipeline (which would mean tearing down the embed
	// worker pool, indexer, classifier, etc. — none of which can hot-reload).
	// In-flight ingests that already loaded the old snapshot finish under it.
	// See UpdateGates.
	// Nil until NewPipeline (or UpdateGates) publishes the first snapshot;
	// readers guard for nil so test-constructed bare &Pipeline{} values are
	// safe.
	hotGates atomic.Pointer[gateOverrides]
}

// gateOverrides is the immutable, atomically-swapped snapshot of the
// hot-reloadable ingest-gate knobs.
type gateOverrides struct {
	// promptInjectionAction is the prompt-injection gate mode
	// ("" = off | "detect" | "quarantine").
	promptInjectionAction string
	// claimAuditDisabled is the set of project IDs for which the claim-audit
	// (hallucination Phase-1) gate is skipped — a per-project escape hatch for
	// false-positive-prone projects. Treated as read-only once published;
	// UpdateGates replaces the whole map rather than mutating it.
	claimAuditDisabled map[string]bool
	// denyPatterns is the substring deny-list PolicyMatchGate enforces.
	// Substring (NOT regex) by construction, so it is ReDoS-immune. Sourced
	// from memory.deny_patterns in config.yaml; empty entries are dropped at
	// snapshot-build time so the gate never wastes a Contains() on "". Treated
	// as read-only once published; UpdateGates replaces the whole slice.
	denyPatterns []string
}

// claimAuditDisabledFor reports whether the claim-audit gate is disabled
// for projectID. Skipping the AuditLookup leaves zero claim results, and
// ClaimAuditOverlapGate auto-allows on zero results — so the gate is
// effectively off for that project.
func (p *Pipeline) claimAuditDisabledFor(projectID string) bool {
	g := p.hotGates.Load()
	return g != nil && g.claimAuditDisabled[projectID]
}

// denyPatternsSnapshot returns the current hot-reloadable substring deny-list
// PolicyMatchGate enforces. Nil-safe: a bare &Pipeline{} (no published
// snapshot) reports an empty list, which PolicyMatchGate treats as a no-op.
func (p *Pipeline) denyPatternsSnapshot() []string {
	if g := p.hotGates.Load(); g != nil {
		return g.denyPatterns
	}
	return nil
}

// gateConfigSnapshot returns p.cfg.GateConfig with the hot-reloadable
// prompt-injection action overlaid from the current snapshot, so a config
// reload takes effect on the next ingest without rebuilding the pipeline.
func (p *Pipeline) gateConfigSnapshot() GateConfig {
	gc := p.cfg.GateConfig
	if g := p.hotGates.Load(); g != nil {
		gc.PromptInjectionAction = g.promptInjectionAction
	}
	return gc
}

// UpdateGates hot-swaps the reloadable ingest-gate knobs
// (prompt_injection_scan, claim_audit_disabled_projects, deny_patterns)
// without locking the ingest hot path or rebuilding the pipeline. Safe for
// concurrent ingests: the new snapshot is published atomically; ingests that
// already loaded the prior snapshot complete under it. action "" means the
// prompt-injection gate is off; an empty denyPatterns disables the deny-list.
// Called from the config-reload activator (see service.Container). The reload
// fails closed: the staged config is parsed + validated before this runs, so a
// bad config.yaml edit never reaches here and the live pipeline keeps the
// last-good snapshot.
func (p *Pipeline) UpdateGates(promptInjectionAction string, claimAuditDisabledProjects, denyPatterns []string) {
	p.hotGates.Store(newGateOverrides(promptInjectionAction, claimAuditDisabledProjects, denyPatterns))
}

// newGateOverrides builds an immutable snapshot, trimming/dropping empty
// project IDs and empty deny patterns (mirrors NewPipeline's original
// construction logic). Dropping empty deny entries keeps PolicyMatchGate from
// short-circuiting on "" (which strings.Contains treats as a universal match).
func newGateOverrides(promptInjectionAction string, claimAuditDisabledProjects, denyPatterns []string) *gateOverrides {
	disabled := make(map[string]bool, len(claimAuditDisabledProjects))
	for _, pid := range claimAuditDisabledProjects {
		if pid = strings.TrimSpace(pid); pid != "" {
			disabled[pid] = true
		}
	}
	deny := make([]string, 0, len(denyPatterns))
	for _, pat := range denyPatterns {
		if pat != "" {
			deny = append(deny, pat)
		}
	}
	return &gateOverrides{
		promptInjectionAction: promptInjectionAction,
		claimAuditDisabled:    disabled,
		denyPatterns:          deny,
	}
}

// SetMetrics attaches a Metrics instance after construction. The
// service container wires metrics late (after the pipeline is
// built) so a post-construction setter is the right shape; mirrors
// the indexer/searcher/worker pattern.
func (p *Pipeline) SetMetrics(m *Metrics) {
	if p == nil {
		return
	}
	p.cfg.Metrics = m
}

// SetClassifier wires the inline-fallback Classifier and its enable
// flag after construction. Mirrors SetMetrics: the pipeline is
// built before the chat-backed Classifier exists, so a post-
// construction setter avoids reordering the container's wiring.
// Passing a nil classifier or enabled=false explicitly disables the
// inline path (i.e. callers can flip the toggle off at runtime).
func (p *Pipeline) SetClassifier(cl *Classifier, enabled bool) {
	if p == nil {
		return
	}
	p.cfg.Classifier = cl
	p.cfg.ClassifierInlineFallback = enabled
}

// DryRunResult is the per-gate trail + final decision the
// inspector returns. Used by the UI's pipeline inspector to show
// operators exactly what would happen to a candidate without
// writing anything to the corpus.
type DryRunResult struct {
	// Final outcome — Allow / Redact / Quarantine / Reject.
	Final GateOutcome
	// Trail is the gate-by-gate history; first non-Allow entry
	// matches Final (RunStandardGates short-circuits on the
	// first non-Allow).
	Trail []GateOutcome
	// Class assigned by the role-based classifier.
	Class ContentClass
	// Policy bundle the chunk would have been stamped with.
	Policy ClassPolicy
	// RoleOfRecordEligible mirrors policy.RoleOfRecordEligible —
	// callers surface this as "would land verified" in the UI.
	RoleOfRecordEligible bool
	// PostRedactContent carries any post-secret-redaction content
	// so the operator can see what the corpus would actually have
	// stored. Empty when no redaction happened.
	PostRedactContent string
	// Claims is the pre-gate extraction the audit-overlap gate
	// would have inspected. Carries Found=true rows when DryRun
	// was given an executionID and the AuditLookup callback was
	// wired; otherwise Found is uniformly false (still useful for
	// regex tuning).
	Claims []ClaimMatch
}

// DryRun evaluates the gate stack against a candidate without any
// DB writes. Used by the operator UI's pipeline inspector to test
// whether arbitrary content would be admitted, redacted, or
// quarantined — and which gate would make the call.
//
// Same gate stack as IngestArtifact, including the project's
// secrets detector + class-based ttl + role_of_record check, so
// the dry-run reflects production behaviour exactly.
//
// ingestExecutionID is optional: when set and AuditLookup is
// wired, the inspector reflects the real audit-overlap verdict;
// when empty, claim extraction still runs (regex tuning) but
// every claim shows as Found=false, so the gate auto-quarantines.
// Operators tuning regex patterns should leave executionID empty;
// operators reproducing a production decision should supply it.
func (p *Pipeline) DryRun(
	projectID, sourceName, producerRole, content string,
) DryRunResult {
	return p.DryRunWithExecution(projectID, sourceName, producerRole, "", content)
}

// DryRunWithExecution is DryRun extended with an explicit
// execution_id for the audit-overlap gate. Kept separate so the
// existing DryRun signature stays a stable contract for the
// inspector adapter.
func (p *Pipeline) DryRunWithExecution(
	projectID, sourceName, producerRole, ingestExecutionID, content string,
) DryRunResult {
	class, policy := ClassifyByRole(producerRole)
	cand := &IngestCandidate{
		ProjectID:          projectID,
		SourceArtifactID:   "dry-run",
		SourceName:         sourceName,
		ProducerRole:       producerRole,
		IngestExecutionID:  ingestExecutionID,
		Content:            content,
		ProposedClass:      class,
		ProposedConfidence: policy.DefaultConfidence,
	}
	// Pre-resolve claims using the pipeline's AuditLookup if the
	// caller supplied an executionID. Without an executionID we
	// still extract claims so the inspector can show regex
	// matches — but the gate sees zero results and auto-allows.
	if ingestExecutionID != "" && p.cfg.AuditLookup != nil && !p.claimAuditDisabledFor(projectID) {
		extracted := ExtractClaims(content)
		if len(extracted) > 0 {
			results, lerr := p.cfg.AuditLookup(context.Background(), ingestExecutionID, extracted)
			if lerr == nil {
				cand.ClaimAuditResults = results
			} else {
				p.logger.Debug().Err(lerr).
					Str("execution_id", ingestExecutionID).
					Msg("DryRun: audit lookup failed; proceeding without overlap")
			}
		}
	}
	final, trail := RunStandardGates(cand, p.gateConfigSnapshot(), p.denyPatternsSnapshot(), 0, nil)
	res := DryRunResult{
		Final:                final,
		Trail:                trail,
		Class:                class,
		Policy:               policy,
		RoleOfRecordEligible: policy.RoleOfRecordEligible,
		Claims:               cand.ClaimAuditResults,
	}
	// When DryRun didn't pre-resolve (no executionID, or no
	// AuditLookup wired) still surface the bare extraction so the
	// inspector can show "what would be claimed" panels. Found
	// stays false on these.
	if len(res.Claims) == 0 {
		extracted := ExtractClaims(content)
		if len(extracted) > 0 {
			res.Claims = make([]ClaimMatch, len(extracted))
			for i, cl := range extracted {
				res.Claims[i] = ClaimMatch{Claim: cl}
			}
		}
	}
	// If the secret-scan gate redacted, expose the post-redact
	// content so the operator sees what'd be stored vs what they
	// pasted.
	for _, t := range trail {
		if t.Gate == GateSecretScan && t.Action == GateRedact {
			res.PostRedactContent = t.NewContent
			if res.PostRedactContent == "" {
				res.PostRedactContent = cand.Content // RunStandardGates applied in place
			}
		}
	}
	return res
}

// NewPipeline constructs a pipeline. Indexer is the existing chunk
// writer (carries its own DB repo); cfg supplies the gate dependencies.
func NewPipeline(indexer *Indexer, cfg PipelineConfig) *Pipeline {
	if cfg.GateConfig.MinContentChars == 0 && cfg.GateConfig.MinContentWords == 0 {
		cfg.GateConfig = DefaultGateConfig()
	}
	cfg.GateConfig.SecretsDetector = cfg.SecretsDetector
	cfg.GateConfig.SecretsActions = cfg.SecretsActions
	// Copy after the default-fill so the action survives (DefaultGateConfig
	// leaves PromptInjectionAction empty = off).
	if cfg.PromptInjectionAction != "" {
		cfg.GateConfig.PromptInjectionAction = cfg.PromptInjectionAction
	}
	logger := cfg.Logger
	// zerolog.Logger has unexported byte-slice state so it can't
	// be compared with ==. The Nop() default kicks in only when
	// the caller leaves Logger zero-valued; in practice every
	// production call site provides one.
	if logger.GetLevel() == zerolog.Disabled {
		logger = zerolog.Nop()
	}
	p := &Pipeline{cfg: cfg, indexer: indexer, logger: logger}
	// Publish the initial hot-reloadable snapshot. cfg.GateConfig.PromptInjectionAction
	// was already resolved from cfg.PromptInjectionAction above.
	p.hotGates.Store(newGateOverrides(cfg.GateConfig.PromptInjectionAction, cfg.ClaimAuditDisabledProjects, cfg.DenyPatterns))
	return p
}

// IngestArtifact is the single entry point the worker calls. It
// runs the gate stack, routes failures to quarantine, and on
// success delegates to the indexer (which chunks + queues for
// embedding). The chunk's lifecycle_state stays 'published' (the
// schema default) until Phase 3 tags it with an epoch.
//
// PerArtifact summary statistics flow back so the worker can record
// them (and Phase 3's snapshot step can roll them up).
type IngestStats struct {
	Admitted    int
	Quarantined int
	Rejected    int      // refused by gate; never stored
	Verified    int      // role_of_record fast-path stamps (Phase 4)
	Superseded  int      // older chunks marked superseded by this admit (Phase 4)
	GatesFailed []string // unique gate names that fired with non-allow
}

// BeginEpoch creates a fresh epoch row for one ingest run. The
// pipeline tags every chunk admitted during this run with the
// returned epoch_id. Returns "" + nil when Epochs repo isn't wired
// (Phase 2 deployments without snapshot machinery still ingest;
// chunks just don't carry an epoch_id).
func (p *Pipeline) BeginEpoch(ctx context.Context, projectID, ingestExecutionID, notes string) (string, error) {
	if p == nil || p.cfg.Epochs == nil {
		return "", nil
	}
	ep := &persistence.CorpusEpoch{ProjectID: projectID, Notes: ptrStr(notes)}
	if ingestExecutionID != "" {
		ep.IngestExecutionID = ptrStr(ingestExecutionID)
	}
	if err := p.cfg.Epochs.CreateEpoch(ctx, ep); err != nil {
		return "", fmt.Errorf("BeginEpoch: %w", err)
	}
	return ep.ID, nil
}

// CloseAndActivateEpoch finalises an epoch row with summary counts
// and adds it to corpus_epochs_active so default search includes
// the chunks. When admittedTotal is 0 (every artifact was rejected
// or quarantined) we close the epoch but do NOT activate it — an
// empty epoch is operator-visible noise. nil-safe Epochs repo: no-op.
func (p *Pipeline) CloseAndActivateEpoch(ctx context.Context, projectID, epochID string, counts persistence.CorpusEpochCounts) error {
	if p == nil || p.cfg.Epochs == nil || epochID == "" {
		return nil
	}
	if err := p.cfg.Epochs.CloseEpoch(ctx, epochID, counts); err != nil {
		return fmt.Errorf("CloseEpoch: %w", err)
	}
	if counts.Admitted > 0 {
		if p.cfg.Metrics != nil {
			p.cfg.Metrics.EpochAdmittedChunksTotal.WithLabelValues(projectID).Add(float64(counts.Admitted))
		}
		if err := p.cfg.Epochs.Activate(ctx, projectID, epochID, "system", "ingest run completed"); err != nil {
			return fmt.Errorf("activate epoch: %w", err)
		}
	}
	return nil
}

// IngestArtifactOptions carries optional per-call overrides for
// IngestArtifactWithOptions. Empty struct value = "use the existing
// defaults", which is what plain IngestArtifact passes.
type IngestArtifactOptions struct {
	// ClassOverride, when non-empty, replaces the ClassifyByRole
	// verdict for this candidate. Used by IngestCompanionNote to
	// honour caller-supplied `class` on the remember() MCP tool.
	// Empty leaves the role-map classifier in charge.
	ClassOverride ContentClass

	// TTLOverride, when non-nil, replaces the class-policy default
	// TTL on admitted chunks. Used by IngestCompanionNote for the
	// remember() `ttl_days` arg. Zero duration means "no expiry"
	// (matches the policy convention). Nil = use class default.
	TTLOverride *time.Duration

	// RepoScope partitions this deposit within the project's RAG
	// (migration 75). Empty = uncategorized; "*" = cross-cutting;
	// any other string = a repo token. Threads through to the
	// IngestCandidate and lands on the resulting chunks.
	RepoScope string
}

// IngestArtifact runs one source artifact through the pipeline with
// no overrides. Thin wrapper over IngestArtifactWithOptions kept for
// the queue-drained agent path that always wants ClassifyByRole +
// per-class default TTL.
func (p *Pipeline) IngestArtifact(
	ctx context.Context,
	projectID, taskID, artifactID, sourceName string,
	producerRole string,
	ingestExecutionID string,
	content string,
	sourceSizeBytes int64,
	epochID string,
) (IngestStats, error) {
	return p.IngestArtifactWithOptions(
		ctx,
		projectID, taskID, artifactID, sourceName,
		producerRole, ingestExecutionID,
		content, sourceSizeBytes, epochID,
		IngestArtifactOptions{},
	)
}

// IngestArtifactWithOptions runs one source artifact through the
// pipeline with optional per-call overrides for proposed class and
// TTL. Returns IngestStats describing what happened. Returns error
// only on infrastructure failure (DB unreachable, etc.); gate
// decisions are recorded in stats, not surfaced as errors.
//
// epochID is non-empty when BeginEpoch returned a real ID; the
// pipeline tags admitted chunks with it via the StampEpoch hook.
func (p *Pipeline) IngestArtifactWithOptions(
	ctx context.Context,
	projectID, taskID, artifactID, sourceName string,
	producerRole string,
	ingestExecutionID string,
	content string,
	sourceSizeBytes int64,
	epochID string,
	opts IngestArtifactOptions,
) (IngestStats, error) {
	stats := IngestStats{}
	if p == nil {
		return stats, errors.New("pipeline is nil")
	}
	if p.indexer == nil {
		return stats, errors.New("pipeline has no indexer")
	}

	// Phase 2: artifact = single candidate. Phase 4 may sub-divide
	// into per-section candidates so the validator can score
	// chunks individually rather than the whole artifact.
	//
	// Caller-supplied ClassOverride wins over ClassifyByRole. Used
	// by IngestCompanionNote (remember() MCP tool's `class` arg) so
	// the host LLM can deposit a note as `spec` / `decision` / etc.
	// rather than the role-default `companion_note`.
	class, _ := ClassifyByRole(producerRole)
	if opts.ClassOverride != "" {
		class = opts.ClassOverride
	}
	// Measure 3 (2026-05-15): when the role-map can't pin a class
	// (empty role / role not in the map → Unclassified) AND the
	// operator opted into inline LLM fallback, run the classifier
	// here so the chunk lands with a real class rather than
	// "unclassified". Failures fall back silently to the original
	// Unclassified verdict — the auto-backfill loop will retry
	// asynchronously. We avoid the call entirely when the role-map
	// produced a usable class so the inline path costs nothing for
	// the happy case.
	if class == ClassUnclassified && p.cfg.ClassifierInlineFallback && p.cfg.Classifier != nil {
		// Per-chunk ID isn't available yet (the indexer mints
		// chunk IDs downstream), so usage attribution stamps the
		// artifact ID instead. Empty artifactID → recordUsage
		// skips persistence; the LLM call still runs.
		llmClass, lerr := p.cfg.Classifier.Classify(ctx, content, sourceName, producerRole, projectID, artifactID)
		if lerr == nil && llmClass != "" && llmClass != ClassUnclassified {
			class = llmClass
			p.logger.Debug().
				Str("project_id", projectID).
				Str("source_name", sourceName).
				Str("producer_role", producerRole).
				Str("class", string(class)).
				Msg("inline classifier fallback: chunk classified")
		} else if lerr != nil {
			// Warn (not Debug) so a misconfigured classifier — the
			// most likely cause of every inline call failing — is
			// visible in the daemon log without raising the global
			// level. The chunk still ingests as unclassified; the
			// auto-backfill loop will retry later.
			p.logger.Warn().Err(lerr).
				Str("project_id", projectID).
				Str("source_name", sourceName).
				Msg("inline classifier fallback: LLM error; storing chunk as unclassified for backfill")
		}
	}
	cand := &IngestCandidate{
		ProjectID:          projectID,
		SourceArtifactID:   artifactID,
		SourceName:         sourceName,
		ProducerRole:       producerRole,
		IngestExecutionID:  ingestExecutionID,
		Content:            content,
		ProposedClass:      class,
		ProposedConfidence: DefaultClassPolicies[class].DefaultConfidence,
		TTLOverride:        opts.TTLOverride,
		RepoScope:          opts.RepoScope,
	}

	// Path B audit (finding #4 / mitigation plan §7.3).
	// see LLD § https://docs.vornik.io § 4 & § 5
	// (the Path B audit-row contract + failure-mode tree). Record one
	// memory_ingest_audit row for every agent ingest — admitted,
	// quarantined, or rejected — via a defer so all three terminal
	// branches below are covered without threading the call through
	// each return site. Companion-prefixed roles are skipped:
	// IngestCompanionNote (Path A) already audits them via
	// RecordCompanionIngest, and this function is the shared
	// implementation it calls — recording here too would double-write.
	// Decision is derived from the final stats (same helper the
	// companion path uses); on an infra error the stats stay zero and
	// the row reads "rejected", matching Path A's behavior.
	if p.cfg.RecordAgentIngest != nil && !strings.HasPrefix(producerRole, "companion:") {
		defer func() {
			decision, gateFailed := classifyIngestDecision(stats)
			ev := AgentIngestAuditEvent{
				ProjectID:         projectID,
				TaskID:            taskID,
				ActorKind:         "agent",
				ActorID:           producerRole,
				SourceName:        sourceName,
				ContentHash:       hashContent(content),
				ContentBytes:      int64(len(content)),
				ProposedClass:     string(cand.ProposedClass),
				Decision:          decision,
				GateFailed:        gateFailed,
				ChunksAdmitted:    stats.Admitted,
				IngestExecutionID: ingestExecutionID,
				RepoScope:         opts.RepoScope,
			}
			if recErr := p.cfg.RecordAgentIngest(ctx, ev); recErr != nil {
				p.logger.Warn().
					Err(recErr).
					Str("project_id", projectID).
					Str("producer_role", producerRole).
					Str("decision", decision).
					Msg("memory_ingest_audit (Path B) write failed (ingest unaffected)")
			}
		}()
	}

	dedupFn := func(pid, hash string) (bool, error) {
		if p.cfg.ChunkExists == nil {
			return false, nil
		}
		return p.cfg.ChunkExists(ctx, pid, hash)
	}

	// Phase 17: pre-resolve claims against tool_audit_log so the
	// claim-audit-overlap gate can score by reading from the
	// candidate. Best-effort — when the lookup fails or no
	// execution_id is available, the gate falls back to allow.
	// The gate itself stays pure (no I/O) so it's testable.
	if p.cfg.AuditLookup != nil && cand.IngestExecutionID != "" && !p.claimAuditDisabledFor(projectID) {
		extracted := ExtractClaims(cand.Content)
		if len(extracted) > 0 {
			results, lerr := p.cfg.AuditLookup(ctx, cand.IngestExecutionID, extracted)
			if lerr != nil {
				p.logger.Warn().Err(lerr).
					Str("project_id", projectID).
					Str("artifact_id", artifactID).
					Str("execution_id", cand.IngestExecutionID).
					Int("claims", len(extracted)).
					Msg("pipeline: claim audit lookup failed; gate falls back to allow")
			} else {
				cand.ClaimAuditResults = results
			}
		}
	}

	final, trail := RunStandardGates(cand, p.gateConfigSnapshot(), p.denyPatternsSnapshot(), sourceSizeBytes, dedupFn)

	// Track which gates had non-allow outcomes for observability.
	gatesFiredSeen := make(map[string]bool)
	for _, g := range trail {
		if g.Action != GateAllow && !gatesFiredSeen[string(g.Gate)] {
			gatesFiredSeen[string(g.Gate)] = true
			stats.GatesFailed = append(stats.GatesFailed, string(g.Gate))
		}
	}

	switch final.Action {
	case GateReject:
		stats.Rejected++
		p.logger.Debug().
			Str("project_id", projectID).
			Str("artifact_id", artifactID).
			Str("gate", string(final.Gate)).
			Str("detail", final.Detail).
			Msg("pipeline: candidate rejected (gate refused; not quarantined)")
		if p.cfg.Metrics != nil {
			p.cfg.Metrics.PipelineRejectsTotal.WithLabelValues(projectID, string(final.Gate)).Inc()
		}
		return stats, nil

	case GateQuarantine:
		stats.Quarantined++
		if err := p.recordQuarantine(ctx, cand, final); err != nil {
			// Quarantine write failure is bad — operator loses
			// visibility — but doesn't block other ingests. Log
			// loud, return the stats (caller's MarkDone path
			// continues so we don't loop on the same artifact).
			p.logger.Error().
				Err(err).
				Str("project_id", projectID).
				Str("artifact_id", artifactID).
				Str("gate", string(final.Gate)).
				Msg("pipeline: failed to write quarantine row")
		}
		if p.cfg.Metrics != nil {
			p.cfg.Metrics.PipelineQuarantinedTotal.WithLabelValues(projectID, string(final.Gate)).Inc()
		}
		return stats, nil

	case GateAllow:
		// Pass-through to existing indexer. The indexer chunks +
		// inserts; chunks land at lifecycle_state='published' (the
		// schema default), validation_status='unverified', the
		// content_class column carries our classification.
		//
		// Phase 2 keeps using the existing IngestText surface so
		// chunk creation remains a single path. Phase 4 extends
		// the indexer with class/confidence/expires_at parameters
		// so chunks carry the policy-derived metadata.
		if err := p.indexer.IngestText(ctx, projectID, taskID, artifactID, sourceName, cand.Content); err != nil {
			return stats, fmt.Errorf("pipeline: indexer.IngestText: %w", err)
		}
		// Best-effort backfill of the per-class metadata onto the
		// just-written chunks. The index is on (project_id,
		// content_hash) which we already have. Fail-soft so a
		// transient backfill error doesn't unwind the ingest.
		if backfillErr := p.backfillClassMetadata(ctx, cand); backfillErr != nil {
			p.logger.Warn().Err(backfillErr).
				Str("project_id", projectID).
				Str("artifact_id", artifactID).
				Msg("pipeline: backfill class metadata failed (non-fatal)")
		}
		// Phase 3: stamp the epoch_id on the just-written chunks
		// so the search-side filter through corpus_epochs_active
		// includes them. Same fail-soft posture as the policy
		// backfill — a stamping error doesn't unwind the ingest;
		// the chunks just stay legacy/null-epoch and remain
		// searchable until operators promote them.
		if epochID != "" && p.cfg.StampEpoch != nil {
			if stampErr := p.cfg.StampEpoch(ctx, projectID, artifactID, epochID); stampErr != nil {
				p.logger.Warn().Err(stampErr).
					Str("project_id", projectID).
					Str("artifact_id", artifactID).
					Str("epoch_id", epochID).
					Msg("pipeline: epoch stamp failed (non-fatal)")
			}
		}
		// Phase 4: role_of_record shortcut. When the producer
		// role's class is role-of-record-eligible (decision /
		// spec by default), chunks land verified directly —
		// bypassing the LLM validator. Mirrors MDM's
		// system-of-record pattern: the workflow gate that
		// admitted the chunk (e.g. reviewer-approved commit) is
		// itself the verification.
		policy := DefaultClassPolicies[cand.ProposedClass]
		if policy.RoleOfRecordEligible && p.indexer != nil {
			if vErr := p.indexer.MarkVerifiedByArtifact(ctx, projectID, artifactID, cand.ProducerRole); vErr != nil {
				p.logger.Warn().Err(vErr).
					Str("project_id", projectID).
					Str("artifact_id", artifactID).
					Msg("pipeline: role_of_record verify-stamp failed (non-fatal)")
			} else {
				stats.Verified++
				// Audit the validator bypass: these chunks reached
				// 'verified' on role-of-record eligibility, not the LLM
				// validator. The counter lets operators quantify how much
				// of the verified corpus skipped validation.
				if p.cfg.Metrics != nil {
					p.cfg.Metrics.PipelineRoleOfRecordVerifiedTotal.
						WithLabelValues(projectID, string(cand.ProposedClass), cand.ProducerRole).Inc()
				}
			}
		}
		// Phase 4: same-source supersession. Supersession is scoped
		// to the same task_id so independent tasks that happen to
		// write the same filename (e.g. research.md) don't wipe each
		// other's chunks. Only a re-run of the same task suppresses
		// its own previous output. epochID rides along as restore
		// provenance so rolling back THIS epoch un-supersedes the
		// prior version (migration 89; rollback x supersession fix).
		if p.indexer != nil {
			if n, sErr := p.indexer.SupersedeBySameSource(ctx, projectID, string(cand.ProposedClass), sourceName, taskID, artifactID, epochID); sErr != nil {
				p.logger.Warn().Err(sErr).
					Str("project_id", projectID).
					Str("source_name", sourceName).
					Msg("pipeline: same-source supersede failed (non-fatal)")
			} else {
				stats.Superseded += n
			}
		}
		stats.Admitted++
		if p.cfg.Metrics != nil {
			p.cfg.Metrics.PipelineAdmittedTotal.WithLabelValues(projectID, string(class)).Inc()
			if final.ShadowSignal {
				// ADVISORY-ONLY. The shadow signal is computed (Phase 17
				// ClaimAuditOverlapGate, partial claim-audit overlap) and
				// surfaced here via PipelineShadowSignalTotal, but the
				// Phase-5 Pillar-4 shadow lifecycle (lifecycle_state=
				// 'shadow' routing + operator review queue) is DEFERRED /
				// not implemented — see https://docs.vornik.io
				// memory-hardening-phase5-design.md §5 (status banner) and
				// AUDIT-2026-05-30-lld-drift-consolidated.md §3.5
				// (R2/R9/R10). So a flagged chunk is still admitted at
				// lifecycle_state='published'; the signal is observability,
				// not a gate. This metric is the deliberate, documented
				// surface for the otherwise-dropped signal.
				//
				// Walk the trail; first hit wins. Phase 17 only sets it
				// from ClaimAuditOverlapGate, but the loop keeps the metric
				// label correct if a future source sets it too.
				signalGate := string(GateClaimAuditOverlap)
				for _, t := range trail {
					if t.ShadowSignal {
						signalGate = string(t.Gate)
						break
					}
				}
				p.cfg.Metrics.PipelineShadowSignalTotal.WithLabelValues(projectID, signalGate).Inc()
				p.logger.Info().
					Str("project_id", projectID).
					Str("artifact_id", artifactID).
					Str("gate", signalGate).
					Msg("pipeline: admitted with shadow signal (advisory-only; shadow lifecycle routing deferred — see memory-hardening-phase5-design.md §5)")
			}
		}
		return stats, nil
	}

	// Unreachable in normal operation — RunStandardGates always
	// returns one of the four actions above.
	return stats, fmt.Errorf("pipeline: unhandled gate action %d", final.Action)
}

// recordQuarantine inserts the per-chunk quarantine row.
func (p *Pipeline) recordQuarantine(ctx context.Context, c *IngestCandidate, final GateOutcome) error {
	if p.cfg.Quarantine == nil {
		return errors.New("no quarantine repo configured")
	}
	role := c.ProducerRole
	cls := string(c.ProposedClass)
	exec := c.IngestExecutionID
	det := final.Detail
	item := &persistence.MemoryQuarantineItem{
		ProjectID:        c.ProjectID,
		SourceArtifactID: c.SourceArtifactID,
		ProducerRole:     ptrStr(role),
		Content:          c.Content,
		ContentHash:      c.ContentHash,
		ProposedClass:    ptrStr(cls),
		FailedGate:       string(final.Gate),
		FailureDetail:    ptrStr(det),
		QuarantinedAt:    time.Now().UTC(),
	}
	if exec != "" {
		item.IngestExecutionID = ptrStr(exec)
	}
	return p.cfg.Quarantine.Insert(ctx, item)
}

// backfillClassMetadata stamps the per-class confidence + class
// + expires_at on chunks just written by the indexer. The indexer
// inserts with schema defaults (lifecycle='published',
// validation_status='unverified', class='unclassified') because
// it predates Phase 2's policy-aware writer; we patch the metadata
// in a follow-up UPDATE keyed by (project_id, content_hash).
//
// Phase 4 will move this into the indexer itself (new IngestText
// signature), eliminating the second round-trip.
func (p *Pipeline) backfillClassMetadata(ctx context.Context, c *IngestCandidate) error {
	pol := DefaultClassPolicies[c.ProposedClass]
	ttl := pol.TTL
	if c.TTLOverride != nil {
		// Caller-supplied override beats the class default. Zero
		// duration means "no expiry" — matches the policy convention
		// where TTL=0 produces a nil expires_at on the chunk.
		ttl = *c.TTLOverride
	}
	var expiresAt *time.Time
	if ttl > 0 {
		t := time.Now().Add(ttl).UTC()
		expiresAt = &t
	}
	// The indexer's repository.UpsertChunks writes one row per
	// chunk under (project_id, artifact_id). PatchPolicyByArtifact
	// stamps metadata on every chunk for the artifact in one
	// statement. The legacy→unverified flip rule lives there.
	return p.indexer.PatchPolicyByArtifact(ctx, c.ProjectID, c.SourceArtifactID,
		string(c.ProposedClass),
		c.ProposedConfidence,
		c.ProducerRole,
		c.IngestExecutionID,
		expiresAt,
		c.RepoScope,
	)
}

func ptrStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// classifyIngestDecision derives the (decision, gate_failed) pair
// for a companion-ingest audit row from IngestStats. One IngestStats
// represents one companion deposit (single artifact, single
// candidate), so Admitted+Quarantined+Rejected is always 0 or 1
// — except for an empty-content rejection which leaves all three
// at 0 and counts as "rejected" with no gate name (the candidate
// never entered the gate stack). gate_failed picks the last
// recorded failure when the stats list multiple (the dominant
// reason in practice — gates run in order and short-circuit on
// the first non-allow outcome for a single candidate).
func classifyIngestDecision(stats IngestStats) (decision, gateFailed string) {
	switch {
	case stats.Admitted > 0:
		return "admitted", ""
	case stats.Quarantined > 0:
		decision = "quarantined"
	default:
		decision = "rejected"
	}
	if n := len(stats.GatesFailed); n > 0 {
		gateFailed = stats.GatesFailed[n-1]
	}
	return decision, gateFailed
}

// hashContent returns the hex-encoded sha256 of content. Used as
// the dedup key on memory_ingest_audit rows so an operator can
// query "every deposit attempt of THIS content" across keys/time.
// Mirrors the content_hash convention used on project_memory_chunks.
func hashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// CompanionIngestResult is what IngestCompanionNote returns. Pipes
// the pipeline's IngestStats out unchanged so the MCP layer can map
// them to the `remember` tool's response envelope, plus the
// synthetic artifactID stamped on every chunk from this deposit so
// the caller can echo it back to the client for traceability.
type CompanionIngestResult struct {
	Stats      IngestStats
	ArtifactID string // synthetic "companion:<key_id>:<rand>"
}

// CompanionIngestAuditEvent is the audit record handed to the
// PipelineConfig.RecordCompanionIngest hook on every companion-
// direct deposit attempt, regardless of gate decision. The
// memory→persistence boundary stays a plain Go struct (no DB
// import in internal/memory) so the audit table can evolve without
// rippling. Decision is one of "admitted" | "quarantined" |
// "rejected"; GateFailed is non-empty only when Decision != "admitted".
type CompanionIngestAuditEvent struct {
	ProjectID      string
	ActorKind      string // "companion:<client_kind>"
	ActorID        string // api_keys.id
	SourceName     string
	ContentHash    string
	ContentBytes   int64
	ProposedClass  string
	Decision       string
	GateFailed     string
	ChunksAdmitted int
	// RepoScope partitions this deposit within the project's RAG
	// (migration 75). Empty / "*" / repo token — see candidate doc.
	RepoScope string
}

// AgentIngestAuditEvent is the audit record handed to
// PipelineConfig.RecordAgentIngest on every Path B (queue-drained
// agent) ingest attempt, regardless of gate decision. Mirrors
// CompanionIngestAuditEvent's shape so both paths map onto the same
// memory_ingest_audit row; ActorKind is the constant "agent" and
// ActorID carries the producer role. TaskID + IngestExecutionID are
// added (companion deposits have neither) for trace assembly.
// Decision is one of "admitted" | "quarantined" | "rejected".
type AgentIngestAuditEvent struct {
	ProjectID         string
	TaskID            string
	ActorKind         string // always "agent"
	ActorID           string // producer role
	SourceName        string
	ContentHash       string
	ContentBytes      int64
	ProposedClass     string
	Decision          string
	GateFailed        string
	ChunksAdmitted    int
	IngestExecutionID string
	RepoScope         string
}

const maxCompanionNoteTTLDays = 3650

// IngestCompanionNote ingests inline content deposited via the
// `remember` MCP tool (LLD 22). Thin wrapper over IngestArtifact
// that synthesises the companion-shaped provenance:
//
//   - producer_role = "companion:<client_kind>"   — drives
//     ClassifyByRole → ClassCompanionNote and the gate-pipeline
//     companion carve-out.
//   - artifactID    = "companion_<ts>_<rand>"     — synthetic.
//     project_memory_chunks.artifact_id is FK-constrained to
//     artifacts(id), so the wrapper first inserts a synthetic
//     artifacts row via CreateCompanionArtifact before any chunk
//     upserts run. Per-call uniqueness means PatchPolicyByArtifact
//     and SupersedeBySameSource scope to this deposit only.
//   - ingestExecutionID = "companion_<ts>_<rand>" — synthetic;
//     consumed by the audit-trail / claim-audit gate which
//     auto-allows when no extracted claims are present.
//
// `sourceName` is what the caller passed verbatim — typically
// already prefixed with `companion:<client_kind>` by the MCP
// handler. `clientKind` and `keyID` flow into the synthetic IDs
// so audit log queries can trace a chunk back to its origin key.
//
// Returns the pipeline stats unchanged. Zero stats with `Admitted
// == 0 && Quarantined == 0 && Rejected == 0` means "content was
// empty after gates trimmed it" — pipeline treats that as a no-op.
func (p *Pipeline) IngestCompanionNote(
	ctx context.Context,
	projectID, clientKind, keyID, sourceName, content string,
	class ContentClass,
	ttlDays int,
	repoScope string,
) (CompanionIngestResult, error) {
	if p == nil {
		return CompanionIngestResult{}, errors.New("pipeline is nil")
	}
	if projectID == "" || clientKind == "" || keyID == "" {
		return CompanionIngestResult{}, errors.New("IngestCompanionNote: project_id, client_kind and key_id are required")
	}
	if sourceName == "" {
		sourceName = "companion:" + clientKind + ":note"
	}
	if ttlDays > maxCompanionNoteTTLDays {
		return CompanionIngestResult{}, fmt.Errorf("IngestCompanionNote: ttl_days must be <= %d", maxCompanionNoteTTLDays)
	}

	artifactID := persistence.GenerateID("companion")
	ingestExec := persistence.GenerateID("companion")
	producerRole := "companion:" + clientKind

	// Insert the synthetic artifacts row before any chunk upsert.
	// project_memory_chunks.artifact_id FK to artifacts(id) is
	// enforced by the schema; the chunk upsert would otherwise die
	// with a foreign-key violation.
	if p.cfg.CreateCompanionArtifact == nil {
		return CompanionIngestResult{}, errors.New("IngestCompanionNote: PipelineConfig.CreateCompanionArtifact is required")
	}
	if err := p.cfg.CreateCompanionArtifact(ctx, projectID, artifactID, sourceName, int64(len(content))); err != nil {
		return CompanionIngestResult{}, fmt.Errorf("IngestCompanionNote: create artifact row: %w", err)
	}

	// Resolve caller-supplied class + ttl_days into pipeline options.
	// LLD-22 § "Tool surface": `class` defaults to "" (the role-map
	// classifier picks `companion_note` from the `companion:` role
	// prefix); `ttl_days` <= 0 means "use the class policy default".
	// `ttl_days` > 0 produces a per-deposit TTL override on the
	// candidate; explicit zero (passed as a sentinel from a caller
	// that genuinely wants "no expiry") is currently coerced to
	// the default — the MCP schema validates `ttl_days >= 1` so the
	// zero-means-no-expiry interpretation is reachable only from
	// in-process callers, and none rely on it today.
	opts := IngestArtifactOptions{ClassOverride: class, RepoScope: repoScope}
	if ttlDays > 0 {
		d := time.Duration(ttlDays) * 24 * time.Hour
		opts.TTLOverride = &d
	}

	// taskID is empty: companion deposits are not tied to a task.
	// epochID is empty: the live-search path picks up legacy-epoch
	// chunks via the OR-null filter, so companion-origin chunks are
	// queryable from the moment they land.
	stats, err := p.IngestArtifactWithOptions(
		ctx,
		projectID,
		"", // taskID
		artifactID,
		sourceName,
		producerRole,
		ingestExec,
		content,
		int64(len(content)),
		"", // epochID
		opts,
	)

	// Audit row regardless of decision, IngestArtifact error or not.
	// Companion-direct deposits bypass project_ingest_queue, so this
	// is the only per-call trail for the "what did key X deposit"
	// query. Nil-safe; failures here are logged-only — auditing
	// shouldn't fail the operator-visible deposit. See the
	// PipelineConfig.RecordCompanionIngest docstring.
	if p.cfg.RecordCompanionIngest != nil {
		decision, gateFailed := classifyIngestDecision(stats)
		event := CompanionIngestAuditEvent{
			ProjectID:      projectID,
			ActorKind:      producerRole,
			ActorID:        keyID,
			SourceName:     sourceName,
			ContentHash:    hashContent(content),
			ContentBytes:   int64(len(content)),
			Decision:       decision,
			GateFailed:     gateFailed,
			ChunksAdmitted: stats.Admitted,
			RepoScope:      repoScope,
		}
		if recErr := p.cfg.RecordCompanionIngest(ctx, event); recErr != nil {
			p.logger.Warn().
				Err(recErr).
				Str("project_id", projectID).
				Str("actor_id", keyID).
				Str("decision", decision).
				Msg("memory_ingest_audit write failed (deposit succeeded)")
		}
	}

	return CompanionIngestResult{Stats: stats, ArtifactID: artifactID}, err
}
