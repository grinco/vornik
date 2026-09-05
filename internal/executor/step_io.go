package executor

import (
	"context"

	"vornik.io/vornik/internal/persistence"
)

// stepIOMaxBytes is the ceiling on one boundary file the executor will
// persist (step-I/O persistence design §3): a part above it is not stored, the
// outcome row's hash stays empty, and StepIOSkippedTotal counts it with
// reason=too_large. 4 MiB is the order of a large canonical project context;
// a constant, not a knob, because the ceiling exists to bound the store, not
// to be tuned per deployment — an operator who hits it has a task.json that
// is itself the finding.
const stepIOMaxBytes = 4 << 20

// stepIOFiles carries the two files at the container boundary as the
// executor saw them: Input is what it wrote to task.json, Result is what it
// read back from result.json AFTER the result_json secrets checkpoint. Nil
// means the file did not exist for this step.
type stepIOFiles struct {
	Input  []byte
	Result []byte
}

// persistStepIO stores the two boundary files as content-addressed parts and
// returns the hashes of what was STORED — through the same redacting
// repository the prompt parts cross, so the hash names the redacted bytes.
// Absent parts store nothing; a part over the ceiling is counted, not
// stored; a failing store is a log line and an empty hash, never a step
// failure; a nil repository (the store not wired) yields empty hashes.
func (e *Executor) persistStepIO(ctx context.Context, executionID, stepID string, f stepIOFiles) persistence.StepPromptHashes {
	var out persistence.StepPromptHashes
	if e.stepPromptRepo == nil {
		return out
	}
	save := func(part persistence.StepPromptPart, body []byte) string {
		if len(body) == 0 {
			return ""
		}
		if len(body) > stepIOMaxBytes {
			if e.metrics != nil && e.metrics.StepIOSkippedTotal != nil {
				e.metrics.StepIOSkippedTotal.WithLabelValues(string(part), "too_large").Inc()
			}
			e.logger.Warn().Str("execution_id", executionID).Str("step", stepID).Str("part", string(part)).
				Int("bytes", len(body)).Int("ceiling", stepIOMaxBytes).
				Msg("step io: part over the ceiling — not persisted")
			return ""
		}
		stored, err := e.stepPromptRepo.Save(ctx, part, string(body))
		if err != nil {
			e.logger.Warn().Err(err).Str("execution_id", executionID).Str("step", stepID).Str("part", string(part)).
				Msg("step io: failed to persist part")
			return ""
		}
		return stored
	}
	out.Input = save(persistence.StepPromptInput, f.Input)
	out.Result = save(persistence.StepPromptResult, f.Result)
	return out
}
