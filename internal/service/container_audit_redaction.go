package service

import (
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
	if _, already := c.repos.ToolAudit.(*auditredact.Repo); already {
		// Idempotent: the observability rebuild re-runs this after replacing
		// the repo handles, and a double wrap would scan every row twice.
		return
	}
	if !c.Config.Secrets.Enabled {
		// The operator disabled scanning outright; initScheduler already warns
		// loudly about that. Leaving the repo undecorated keeps the bypass in
		// one place rather than half-applying it here.
		return
	}
	detector, actions, err := buildSecretsDetector(c.Config.Secrets)
	if err != nil || detector == nil {
		c.Logger.Error().Err(err).Msg("secrets: tool-audit redaction NOT wired — detector failed to construct; audit rows may persist plaintext credentials")
		return
	}
	c.repos.ToolAudit = auditredact.New(
		c.repos.ToolAudit,
		detector,
		actions,
		c.repos.SecretRedaction,
		c.Config.Secrets.TrustedOutputTools,
		&c.Logger,
	)
	c.Logger.Info().
		Str("action", string(secrets.ResolveAction(secrets.CheckpointToolAudit, actions))).
		Msg("secrets: tool-audit redaction wired at the repository seam — every writer is covered")
}
