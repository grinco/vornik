package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/verifier"
)

// writeBackstopArtifacts handles the "agent exited without writing
// the file the contract demanded" case. For every verifier
// configuration that declared an `artifact_pattern` and produced a
// "no artifact matched" violation, we write a stub artifact carrying:
//
//   - the failing verifier's name + detail (so the post-mortem
//     reads the contract, not just the symptom),
//   - a short audit summary (last few tool calls + their classified
//     block reason from the verifier engine) so the operator sees
//     WHY the agent gave up.
//
// The stub does NOT pass the current iteration's verifier — that
// already failed and is about to be returned to the caller. Its job
// is operator visibility on the next iteration and in the post-
// mortem. With #4 lowering scan_min_entries to min:1, the stub's
// single bullet also makes the next iteration's gate pass cleanly
// IF retries are permitted (terminal verifiers short-circuit per #2).
//
// Best-effort: any error along the way (no artifact store wired,
// disk full, etc.) is logged at Warn but never aborts the verifier
// flow.
func (e *Executor) writeBackstopArtifacts(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	stepID string,
	cfgs []verifier.Config,
	in verifier.Input,
	violations []verifier.Violation,
) {
	if e == nil || task == nil || execution == nil {
		return
	}
	// Match each "no artifact matched" violation back to its config
	// so we can read the artifact_pattern from the source.
	missingPatterns := collectMissingPatterns(cfgs, violations)
	if len(missingPatterns) == 0 {
		return
	}

	auditSummary := summariseAuditForBackstop(in.AuditEntries)
	for _, mp := range missingPatterns {
		name := stubFilenameFromPattern(mp.pattern, execution.ID)
		if name == "" {
			continue
		}
		// Skip if an artifact with this name already exists for the
		// task — this is the idempotency check, and also covers the
		// rare case where a second iteration's runVerifiers re-fires
		// against a stub already on disk.
		if alreadyHasArtifact(in.Artifacts, name) {
			continue
		}
		body := buildStubBody(mp.verifierName, mp.detail, auditSummary, task.ID, execution.ID)
		if err := e.persistStubArtifact(ctx, task, execution, stepID, name, body); err != nil {
			e.logger.Warn().
				Err(err).
				Str("execution_id", execution.ID).
				Str("step", stepID).
				Str("stub_name", name).
				Msg("verifier: backstop artifact write failed (non-fatal)")
		}
	}
}

// missingPatternHit pairs a violation's verifier identity with the
// artifact pattern it expected to find. Computed once per
// writeBackstopArtifacts call so the stub generator doesn't have to
// re-parse the violation detail.
type missingPatternHit struct {
	verifierName string
	pattern      string
	detail       string
}

// collectMissingPatterns walks the violation list, picking out the
// ones that report "no artifact matched pattern X" and joining each
// to its source Config so we can read artifact_pattern verbatim from
// the YAML rather than parsing the violation string. Type-driven
// instead of message-driven so a future translation / rewording of
// the violation detail won't break the backstop.
func collectMissingPatterns(cfgs []verifier.Config, violations []verifier.Violation) []missingPatternHit {
	out := make([]missingPatternHit, 0, len(violations))
	for _, v := range violations {
		if v.Severity == verifier.SeverityWarn {
			continue
		}
		// The two verifier kinds that demand a named artifact pattern.
		if v.Type != "artifact_min_entries" && v.Type != "artifact_non_empty" {
			continue
		}
		if !strings.Contains(strings.ToLower(v.Detail), "no artifact matched") {
			continue
		}
		// Match the violation's VerifierName back to its source config
		// so the pattern we read is the one the operator actually
		// declared.
		pattern := ""
		for _, c := range cfgs {
			if c.Name != v.VerifierName && string(c.Type) != v.VerifierName {
				continue
			}
			if p, ok := c.Params["artifact_pattern"].(string); ok {
				pattern = p
				break
			}
		}
		if pattern == "" {
			continue
		}
		out = append(out, missingPatternHit{
			verifierName: v.VerifierName,
			pattern:      pattern,
			detail:       v.Detail,
		})
	}
	return out
}

// stubFilenameFromPattern turns a glob like "scan-*.md" into a
// concrete filename like "scan-backstop-<exec_id-prefix>.md". Returns
// "" when the pattern shape is too ambiguous to render safely
// (multiple wildcards, no extension); the operator's verifier still
// fires, we just don't backstop.
func stubFilenameFromPattern(pattern, execID string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ""
	}
	// Single-star, single-extension patterns are the supported shape:
	// "scan-*.md", "*.patch", "report-*.json". Multi-star ("a-*-b-*.md")
	// and no-extension ("scan-*") cases are rejected.
	star := strings.Count(pattern, "*")
	if star != 1 {
		return ""
	}
	idx := strings.Index(pattern, "*")
	prefix := pattern[:idx]
	suffix := pattern[idx+1:]
	if !strings.HasPrefix(suffix, ".") {
		return ""
	}
	// Append a stable suffix so multiple stubs across iterations don't
	// stomp each other; truncate exec ID to 8 chars so the filename
	// stays human-readable.
	tag := execID
	if len(tag) > 8 {
		tag = tag[:8]
	}
	return fmt.Sprintf("%sbackstop-%s%s", prefix, tag, suffix)
}

// alreadyHasArtifact returns true when an artifact with the given
// name is already in the verifier input set. Used to idempotently
// skip stub creation on subsequent iterations.
func alreadyHasArtifact(arts []*persistence.Artifact, name string) bool {
	for _, a := range arts {
		if a == nil {
			continue
		}
		if a.Name == name {
			return true
		}
	}
	return false
}

// summariseAuditForBackstop produces a compact bullet list of the
// final 5 tool-audit entries, formatted for the stub artifact body.
// Empty input → empty string so the stub still renders cleanly.
func summariseAuditForBackstop(entries []*persistence.ToolAuditEntry) string {
	if len(entries) == 0 {
		return "(no tool audit entries recorded for this step)"
	}
	limit := 5
	start := len(entries) - limit
	if start < 0 {
		start = 0
	}
	var b strings.Builder
	for i := start; i < len(entries); i++ {
		ent := entries[i]
		if ent == nil {
			continue
		}
		fmt.Fprintf(&b, "- `%s`: input=%s output=%s\n",
			ent.ToolName,
			truncForStub(strings.TrimSpace(ent.ToolInput), 200),
			truncForStub(strings.TrimSpace(ent.ToolOutput), 200),
		)
	}
	return b.String()
}

func truncForStub(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// buildStubBody renders the stub artifact's markdown body. Format
// mirrors the scan-*.md template's expected shape (one bullet list)
// so artifact_min_entries with min:1 passes on the next iteration:
// the verifier counts the "- " block-report bullet as a list item.
func buildStubBody(verifierName, detail, auditSummary, taskID, execID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Backstop scan record\n\n")
	fmt.Fprintf(&b, "_This artifact was generated automatically because the agent exited without writing the expected scan file. The verifier `%s` flagged the missing artifact._\n\n",
		verifierName)
	fmt.Fprintf(&b, "## Why\n\n")
	fmt.Fprintf(&b, "- %s\n", detail)
	fmt.Fprintf(&b, "\n## Recent tool calls\n\n")
	if strings.TrimSpace(auditSummary) == "" {
		b.WriteString("(no tool audit recorded)\n")
	} else {
		b.WriteString(auditSummary)
	}
	fmt.Fprintf(&b, "\n## Metadata\n\n")
	fmt.Fprintf(&b, "- task_id: %s\n", taskID)
	fmt.Fprintf(&b, "- execution_id: %s\n", execID)
	fmt.Fprintf(&b, "- generated_at: %s\n", time.Now().UTC().Format(time.RFC3339))
	return b.String()
}

// persistStubArtifact writes the stub body to disk and records it in
// the artifact repo. Uses the artifact store when available
// (production wiring) and falls back to a direct artifactRepo.Create
// against a path in the task's artifact directory.
//
// Path discipline: the stub lives inside the executor's artifact
// scratch dir for the execution. We DO NOT try to drop it inside the
// project's source tree — that's the agent's responsibility, not
// ours. The artifact row carries StoragePath so the verifier engine
// (and the post-mortem UI) can read it back.
func (e *Executor) persistStubArtifact(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	stepID string,
	name string,
	body string,
) error {
	dir := e.backstopArtifactDir(execution.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("backstop mkdir %s: %w", dir, err)
	}
	hostPath := filepath.Join(dir, name)
	if err := os.WriteFile(hostPath, []byte(body), 0o644); err != nil {
		return fmt.Errorf("backstop write %s: %w", hostPath, err)
	}

	if e.artifactStore != nil {
		_, err := e.artifactStore.Store(ctx, task.ProjectID, execution.ID, task.ID, name, hostPath)
		if err != nil {
			return fmt.Errorf("backstop store: %w", err)
		}
		return nil
	}
	if e.artifactRepo == nil {
		return nil
	}
	art := &persistence.Artifact{
		ID:            generateArtifactID(execution.ID),
		ProjectID:     task.ProjectID,
		ExecutionID:   &execution.ID,
		Name:          name,
		ArtifactClass: persistence.ArtifactClassOutput,
		StoragePath:   hostPath,
		Origin:        persistence.ArtifactOriginTaskOutput,
	}
	if task.ID != "" {
		art.TaskID = &task.ID
	}
	return e.artifactRepo.Create(ctx, art)
}

// backstopArtifactDir returns the directory where the stub markdown
// file lives. Anchored under the system temp dir so the stub is
// reachable both from the daemon (writing) and from anything that
// later reads the artifact's StoragePath. Per-execution sub-dir so
// concurrent executions can't stomp each other's stubs.
func (e *Executor) backstopArtifactDir(executionID string) string {
	return filepath.Join(os.TempDir(), "vornik-backstop", executionID)
}
