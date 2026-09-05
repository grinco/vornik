package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// stepPromptFileName is the second output file the container writes beside
// result.json: the step's first model request in three parts, each with the
// container's own sha256 (step-prompt persistence design §3).
const stepPromptFileName = "step_prompt.json"

// stepPromptPart is one part as the container wrote it.
type stepPromptPart struct {
	SHA256 string `json:"sha256"`
	Body   string `json:"body"`
}

// stepPromptFile is step_prompt.json.
type stepPromptFile struct {
	System stepPromptPart `json:"system"`
	User   stepPromptPart `json:"user"`
	Tools  stepPromptPart `json:"tools"`
}

// readStepPromptFile reads the file if the container wrote one. Absent → nil,
// silently: that is every image built before the contract. Unparseable → nil
// with a log line: the prompt is not recorded, the step's outcome is
// unaffected. Never an error to the caller.
func readStepPromptFile(path string, logger *zerolog.Logger, executionID, stepID string) *stepPromptFile {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && logger != nil {
			logger.Warn().Err(err).Str("execution_id", executionID).Str("step", stepID).
				Msg("step prompt: could not read step_prompt.json — prompt not persisted for this step")
		}
		return nil
	}
	var f stepPromptFile
	if err := json.Unmarshal(raw, &f); err != nil {
		if logger != nil {
			logger.Warn().Err(err).Str("execution_id", executionID).Str("step", stepID).
				Msg("step prompt: step_prompt.json does not parse — prompt not persisted for this step")
		}
		return nil
	}
	return &f
}

// persistStepPrompt stores the three parts and returns the hashes of what
// was STORED — the repository (auditredact's decorator) redacts first and
// hashes after, so its hash is the one the outcome row must carry. A part's
// container-side hash that disagrees is counted, never fatal:
// reason=redacted when the bytes changed at the seam (a secret was scrubbed),
// reason=drift when they did not and the hashes still differ (the image and
// the daemon disagree about the file's shape). Absent file or nil repo → empty
// hashes.
func (e *Executor) persistStepPrompt(ctx context.Context, executionID, stepID string, f *stepPromptFile) persistence.StepPromptHashes {
	var out persistence.StepPromptHashes
	if f == nil || e.stepPromptRepo == nil {
		return out
	}
	save := func(part persistence.StepPromptPart, p stepPromptPart) string {
		if p.Body == "" {
			return ""
		}
		stored, err := e.stepPromptRepo.Save(ctx, part, p.Body)
		if err != nil {
			e.logger.Warn().Err(err).Str("execution_id", executionID).Str("step", stepID).Str("part", string(part)).
				Msg("step prompt: failed to persist part")
			return ""
		}
		if p.SHA256 != "" && p.SHA256 != stored {
			reason := "drift"
			if persistence.HashStepPrompt(p.Body) != stored {
				// The stored bytes are not the bytes the container wrote: the
				// seam redacted them. Expected whenever a secret was scrubbed.
				reason = "redacted"
			}
			if e.metrics != nil && e.metrics.PromptHashMismatchTotal != nil {
				e.metrics.PromptHashMismatchTotal.WithLabelValues(reason).Inc()
			}
			e.logger.Warn().Str("execution_id", executionID).Str("step", stepID).Str("part", string(part)).
				Str("reason", reason).Str("container_sha256", p.SHA256).Str("stored_sha256", stored).
				Msg("step prompt: container hash differs from the stored bytes' hash")
		}
		return stored
	}
	out.System = save(persistence.StepPromptSystem, f.System)
	out.User = save(persistence.StepPromptUser, f.User)
	out.Tools = save(persistence.StepPromptTools, f.Tools)
	return out
}
