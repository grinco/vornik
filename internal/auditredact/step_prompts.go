package auditredact

import (
	"context"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// StepPrompts decorates a persistence.StepPromptRepository so every part of a
// step's model input is scanned and redacted BEFORE it is stored, at the one
// seam every writer passes through (step-prompt persistence design §5). The
// hash the inner repository returns is the sha256 of the redacted bytes, so
// the stored hash names the stored body.
//
// Same reasoning as Repo: a prompt carries whatever reached the model — skill
// text, tool-credential carryover, third-party content a prior step fetched —
// and a scan at each writer is a scan the next writer forgets. Every code path
// that rebuilds the repository set must re-apply this decoration; that was the
// live lesson of 2026-08-20.
type StepPrompts struct {
	inner    persistence.StepPromptRepository
	detector secrets.Detector
	audit    persistence.SecretRedactionAuditRepository
	logger   *zerolog.Logger
}

// NewStepPrompts wraps inner. A nil detector is a pass-through, so CE paths
// and tests that never wire secret scanning still persist prompts.
func NewStepPrompts(inner persistence.StepPromptRepository, detector secrets.Detector, audit persistence.SecretRedactionAuditRepository, logger *zerolog.Logger) *StepPrompts {
	return &StepPrompts{inner: inner, detector: detector, audit: audit, logger: logger}
}

// RedactText scans body and returns it with every finding replaced by a
// [REDACTED:type] marker, plus the per-type counts. The body-level entry point
// beside Repo.Log's entry-level one; the detector is the same.
func (s *StepPrompts) RedactText(body string) (string, map[string]int) {
	if s == nil || s.detector == nil || body == "" {
		return body, nil
	}
	findings := s.detector.Scan([]byte(body))
	if len(findings) == 0 {
		return body, nil
	}
	return string(secrets.Redact([]byte(body), findings)), secrets.CountByType(findings)
}

// Save redacts, then stores; the returned hash is of what was stored.
func (s *StepPrompts) Save(ctx context.Context, part persistence.StepPromptPart, body string) (string, error) {
	clean, counts := s.RedactText(body)
	if len(counts) > 0 && s.logger != nil {
		s.logger.Warn().Str("part", string(part)).Interface("by_type", counts).
			Str("checkpoint", secrets.CheckpointToolAudit).
			Msg("secrets: step prompt scanned — redacting before persist")
	}
	s.record(ctx, counts)
	return s.inner.Save(ctx, part, clean)
}

// Get delegates unchanged: bodies are stored redacted.
func (s *StepPrompts) Get(ctx context.Context, hash string) (*persistence.StepPrompt, error) {
	return s.inner.Get(ctx, hash)
}

// PruneUnreferenced delegates unchanged.
func (s *StepPrompts) PruneUnreferenced(ctx context.Context) (int64, error) {
	return s.inner.PruneUnreferenced(ctx)
}

func (s *StepPrompts) record(ctx context.Context, counts map[string]int) {
	if s.audit == nil || len(counts) == 0 {
		return
	}
	events := make([]persistence.SecretRedactionEvent, 0, len(counts))
	for findingType, n := range counts {
		if n > 0 {
			events = append(events, persistence.SecretRedactionEvent{
				Checkpoint: secrets.CheckpointToolAudit, FindingType: findingType, Count: n, Source: "live",
			})
		}
	}
	if len(events) == 0 {
		return
	}
	if err := s.audit.Record(ctx, events); err != nil && s.logger != nil {
		s.logger.Warn().Err(err).Msg("secrets: recording step-prompt redaction events failed")
	}
}

var _ persistence.StepPromptRepository = (*StepPrompts)(nil)
