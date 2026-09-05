package service

import (
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/secrets"
)

// initToolAuditRedaction wraps the tool-audit repository so every writer's rows
// are scanned before they are persisted.
//
// MUST run after the repos exist and BEFORE any consumer captures
// c.repos.ToolAudit — the same ordering constraint initLogship documents for
// its audit decorators. A consumer that grabbed the undecorated repo first
// would write raw rows and nothing would say so.
//
// Why a decorator at all: three writers reach tool_audit_log (the realtime
// POST handler, the post-step batch persist, the companion roll-up) and only
// the batch one scanned. Because the repository INSERT is
// ON CONFLICT (id) DO NOTHING and the two agent-side writers share the
// agent-supplied audit_id, the UNREDACTED realtime row landed first and won,
// silently discarding the redacted one. See
// https://docs.vornik.io
func (c *Container) initToolAuditRedaction() {
	if c.repos == nil || c.repos.ToolAudit == nil {
		return
	}
	base := c.repos.ToolAudit
	if existing, already := base.(*auditredact.Repo); already {
		// Idempotent: the observability rebuild re-runs this after replacing
		// the repo handles, and a double wrap would scan every row twice.
		//
		// A BYPASS wrapper is the exception — it must be upgradeable. This runs
		// once at container.go:834 and again after the rebuild, and a
		// pass-through pinned by the first call would keep the fleet writing
		// unscanned rows while the counter honestly reported a bypass the
		// second call should have fixed.
		if existing.BypassReason() == auditredact.ReasonNone {
			return
		}
		// Upgrading: wrap what the bypass wrapped, not the bypass itself.
		// Nesting would count each row twice — skipped by the inner wrapper and
		// scanned by the outer one — corrupting the denominator.
		//
		// The replaced bypass is NOT orphaned for counting purposes. Consumers
		// that captured it (initScheduler, initDispatcher) keep writing through
		// it, and it holds a reference to the same shared holder, so its rows
		// keep landing in the same series. That is the whole reason the holder
		// is shared rather than per-instance.
		base = existing.Inner()
	}

	// The census holder is created ONCE and shared by every decorator instance.
	// On postgres there are two live ones for the life of the daemon: the
	// scheduler and dispatcher capture the Repo built at container.go:834, and
	// the rebuild at :1421 builds another that only the re-run initHTTPServer
	// picks up. Per-instance counters would cover roughly half the rows while
	// presenting as complete. See the auditredact.Metrics doc comment.
	if c.auditRedactMetrics == nil {
		c.auditRedactMetrics = auditredact.NewMetrics()
	}

	// A bypass is a decorator carrying a reason, never an absent decorator:
	// otherwise the object that would count the fail-open is never built, and
	// the bypass announces itself exactly once, in a boot log that scrolls
	// (Finding C, docs/audits/2026-08-26-silent-controls-audit.md).
	wrap := func(reason auditredact.Reason) {
		repo := auditredact.NewBypassed(base, reason, &c.Logger)
		repo.SetMetrics(c.auditRedactMetrics)
		c.repos.ToolAudit = repo
	}

	if !c.Config.Secrets.Enabled {
		// The operator disabled scanning outright; initScheduler already warns
		// loudly about that. The rows it lets through are still counted.
		wrap(auditredact.ReasonSecretsDisabled)
		return
	}
	detector, actions, err := buildSecretsDetector(c.Config.Secrets)
	if err != nil || detector == nil {
		c.Logger.Error().Err(err).Msg("secrets: tool-audit redaction NOT wired — detector failed to construct; audit rows may persist plaintext credentials")
		wrap(auditredact.ReasonDetectorUnavailable)
		return
	}
	wired := auditredact.New(
		base,
		detector,
		actions,
		c.repos.SecretRedaction,
		c.Config.Secrets.TrustedOutputTools,
		&c.Logger,
	)
	wired.SetMetrics(c.auditRedactMetrics)
	c.repos.ToolAudit = wired
	// The step-prompt store goes through the same detector at the same kind
	// of seam (step-prompt persistence design §5): every part of a step's
	// model input is scanned before it is stored, hash taken after. Same
	// rule as above — any path that rebuilds c.repos must re-apply this.
	if c.repos.StepPrompts != nil {
		c.repos.StepPrompts = auditredact.NewStepPrompts(c.repos.StepPrompts, detector, c.repos.SecretRedaction, &c.Logger)
	}
	// The chat audit store is the third instance of the same seam
	// (chat-audit retention and redaction design §3.2): two writers reach it
	// — the dispatcher's turn audit and the chat proxy's — and neither
	// scanned. Idempotent for the same reason the tool-audit wrap is: the
	// observability rebuild re-runs this, and a double wrap would scan every
	// row twice.
	if c.repos.ChatAudit != nil {
		if _, already := c.repos.ChatAudit.(*auditredact.ChatAudit); !already {
			c.repos.ChatAudit = auditredact.NewChatAudit(c.repos.ChatAudit, detector, c.repos.SecretRedaction, &c.Logger)
		}
	}
	c.Logger.Info().
		Str("action", string(secrets.ResolveAction(secrets.CheckpointToolAudit, actions))).
		Msg("secrets: tool-audit redaction wired at the repository seam — every writer is covered")
}

// toolAuditRedactionState reports how the seam was wired, for the
// tool_audit_redaction doctor check: the bypass reason (empty when the seam is
// live) and the resolved action.
//
// It reads the DECORATOR, not the config. A config saying secrets.enabled=true
// is a statement of intent; whether rows are actually being scanned is a
// property of the object in the write path, and the gap between the two is the
// whole subject of the check.
func (c *Container) toolAuditRedactionState() (auditredact.Reason, string) {
	if c.repos == nil {
		return auditredact.ReasonDetectorNil, ""
	}
	repo, ok := c.repos.ToolAudit.(*auditredact.Repo)
	if !ok {
		return auditredact.ReasonDetectorNil, ""
	}
	if reason := repo.BypassReason(); reason != auditredact.ReasonNone {
		return reason, ""
	}
	return auditredact.ReasonNone, string(secrets.ResolveAction(secrets.CheckpointToolAudit, repo.Actions()))
}

// exchangeRedactor is the recorder's seam (llm-exchange record/replay design
// §4): the same detector the step-prompt store was decorated with, exposed as
// a function so the API server never holds the decorator itself. Nil when
// the step-prompt store was not decorated (no detector configured), in which
// case the recorder stores bodies as they are — the same position step
// prompts are in on such a deployment.
func (c *Container) exchangeRedactor() api.ExchangeRedactor {
	sp, ok := c.repos.StepPrompts.(*auditredact.StepPrompts)
	if !ok || sp == nil {
		return nil
	}
	return func(body string) (string, int) {
		out, counts := sp.RedactText(body)
		n := 0
		for _, v := range counts {
			n += v
		}
		return out, n
	}
}
