package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// urlRe matches an http(s) URL up to the first whitespace, quote, or angle
// bracket — the shape URLs take inside JSON result bodies and prose.
var urlRe = regexp.MustCompile("https?://[^\\s\"'<>`\\\\]+")

// extractURLSpans returns byte spans covering the scheme+host+PATH of every
// http(s) URL in body, STOPPING at the query string ('?') or fragment ('#').
//
// Entropy findings inside these spans are exempt from redaction (see
// filterFindingsOutsidePathSpans — only entropy findings are dropped). URL
// path slugs/IDs are routinely high-entropy — job-portal offer URLs like
// startupjobs.cz/nabidka/<id> — but are NOT secrets; redacting them mangles
// the URL into "startupjobs.[REDACTED:entropy]", which the hallucination
// URL-grounding detector then flags as unsupported and FAILS the step (the
// 2026-06-20 startupjobs scan cascade). The query string and fragment are
// deliberately NOT exempt: tokens and signatures legitimately live there
// (?token=, presigned ?sig=...), so an entropy hit in the query still
// redacts, and every deterministic pattern (jwt, aws_*, generic_kv, …)
// redacts everywhere regardless of span.
func extractURLSpans(body []byte) []secrets.Span {
	var spans []secrets.Span
	for _, m := range urlRe.FindAllIndex(body, -1) {
		start, end := m[0], m[1]
		if i := bytes.IndexAny(body[start:end], "?#"); i >= 0 {
			end = start + i // trim query/fragment so secrets there stay scannable
		}
		if end > start {
			spans = append(spans, secrets.Span{Start: start, End: end})
		}
	}
	return spans
}

// pathFieldArrayRe matches the array body of every JSON key whose
// values are filesystem paths: produced_files, modified_files,
// cleanup_artifacts. The captured group is the array's interior so
// individual string elements can be located within it.
var pathFieldArrayRe = regexp.MustCompile(`"(?:produced_files|modified_files|cleanup_artifacts|input_files|inputFiles)"\s*:\s*\[([^\]]*)\]`)

// pathStringInArrayRe captures each string element in an array body —
// used after pathFieldArrayRe to walk the individual file paths.
var pathStringInArrayRe = regexp.MustCompile(`"([^"\\]*)"`)

// pathKeyRe matches every `"path": "<value>"` and `"sourcePath": "<value>"`
// occurrence — covers outputArtifacts[].path and inputArtifacts[].sourcePath
// where the path is a string field rather than an array element.
var pathKeyRe = regexp.MustCompile(`"(?:path|sourcePath|storagePath)"\s*:\s*"([^"\\]*)"`)

// extractPathFieldSpans returns byte spans covering every filesystem-
// path string value in `body`. Findings overlapping these spans must
// not be redacted — file paths frequently contain high-entropy
// substrings (hashes, timestamps, project IDs) that pattern-match
// the entropy detector but are NOT secrets; redacting them turns
// the path unreadable and breaks downstream verification
// (verifyClaimedFiles can't stat a file at /tmp/.../[REDACTED:entropy].pdf).
//
// Triggered by the 2026-05-18 janka CV-delivery cascade where the
// writer's produced_files paths got entropy-redacted, verification
// failed, the recover step ran, its lead emitted a plan that
// re-spawned writer, that writer's output got redacted again,
// and the whole task FAILED.
func extractPathFieldSpans(body []byte) []secrets.Span {
	var spans []secrets.Span
	// String arrays: produced_files / modified_files / etc.
	for _, m := range pathFieldArrayRe.FindAllSubmatchIndex(body, -1) {
		// m[2..3] = array interior. Walk each string element inside.
		interiorStart, interiorEnd := m[2], m[3]
		for _, sm := range pathStringInArrayRe.FindAllSubmatchIndex(body[interiorStart:interiorEnd], -1) {
			// sm[2..3] is the string's content (between the quotes).
			spans = append(spans, secrets.Span{
				Start: interiorStart + sm[2],
				End:   interiorStart + sm[3],
			})
		}
	}
	// Object fields: "path"/"sourcePath"/"storagePath" string values.
	for _, m := range pathKeyRe.FindAllSubmatchIndex(body, -1) {
		spans = append(spans, secrets.Span{Start: m[2], End: m[3]})
	}
	return spans
}

// filterFindingsOutsidePathSpans drops every finding whose byte
// range overlaps any path span. Cheap O(N*M) since both lists are
// short (single-digit counts in typical result.json bodies).
func filterFindingsOutsidePathSpans(findings []secrets.Finding, spans []secrets.Span) []secrets.Finding {
	if len(spans) == 0 || len(findings) == 0 {
		return findings
	}
	out := findings[:0]
	for _, f := range findings {
		skip := false
		// Only ENTROPY findings are path-exempt. They legitimately
		// collide with hash/timestamp-bearing path segments, and
		// redacting those breaks verifyClaimedFiles (the 2026-05-18
		// janka regression). Every deterministic, prefix-anchored
		// pattern (aws_*, github_pat, jwt, connection_string,
		// generic_kv, private_key_block, …) MUST still redact even
		// inside a path field — those shapes never occur in a real
		// filesystem path, so exempting them gave an agent a labeled
		// channel to smuggle a secret past redaction via a path-shaped
		// JSON value.
		if f.Type == secrets.FindingTypeEntropy {
			for _, s := range spans {
				if f.Start < s.End && f.End > s.Start {
					skip = true
					break
				}
			}
		}
		if !skip {
			out = append(out, f)
		}
	}
	return out
}

// ErrSecretLeakBlocked is returned by scanResultForSecrets when the
// configured action for the result.json checkpoint is Block and the
// detector found at least one secret-shaped value. The caller maps
// this to a SECRET_LEAK failure class via the classifier.
var ErrSecretLeakBlocked = errors.New("secret_leak: agent output contains secret-shaped value(s) and the result_json checkpoint is in block mode")

// scanResultForSecrets applies the secrets layer to result.json
// before any downstream consumer reads it. The action is resolved
// from the executor's per-checkpoint policy map (or compiled
// defaults when unconfigured) — Redact substitutes findings with
// typed markers, Detect logs without modifying, and Block returns
// ErrSecretLeakBlocked alongside the (still-redacted) bytes so the
// caller can fail the step with class SECRET_LEAK without losing
// the redacted body for downstream audit.
//
// Returns the (possibly-redacted) bytes the rest of the pipeline
// should treat as canonical. When the layer is disabled (nil
// detector) the input is returned unchanged with a nil error.
func (e *Executor) scanResultForSecrets(ctx context.Context, task *persistence.Task, execution *persistence.Execution, stepID string, body []byte) ([]byte, error) {
	if e.secretsDetector == nil || len(body) == 0 {
		return body, nil
	}
	findings := e.secretsDetector.Scan(body)
	// Drop findings that fall inside known file-path fields. Paths
	// frequently contain entropy-shaped substrings (hashes, dated
	// suffixes) but they aren't secrets, and redacting them turns
	// downstream verification (file does not exist at /tmp/.../
	// [REDACTED:entropy].pdf) into a hard step failure.
	// Exempt entropy findings inside filesystem-path fields AND inside URL
	// scheme/host/path spans — both routinely carry high-entropy non-secret
	// segments whose redaction breaks downstream consumers (file verification;
	// URL-grounding hallucination checks). Query strings stay scannable.
	exemptSpans := append(extractPathFieldSpans(body), extractURLSpans(body)...)
	findings = filterFindingsOutsidePathSpans(findings, exemptSpans)
	if len(findings) == 0 {
		return body, nil
	}
	action := secrets.ResolveAction(secrets.CheckpointResultJSON, e.secretsActions)
	counts := secrets.CountByType(findings)
	logEvent := e.logger.Warn().
		Str("execution_id", execution.ID).
		Str("task_id", task.ID).
		Str("step", stepID).
		Str("checkpoint", secrets.CheckpointResultJSON).
		Str("action", string(action)).
		Int("findings", len(findings)).
		Interface("by_type", counts)

	switch action {
	case secrets.ActionRedact:
		logEvent.Msg("secrets: result.json scanned — redacting findings before persist")
		return secrets.Redact(body, findings), nil
	case secrets.ActionBlock:
		// Phase 2: SECRET_LEAK failure class is wired. Return the
		// redacted body alongside the sentinel error so the caller
		// can persist a clean result row + fail the step. The
		// downstream classifier maps "secret_leak: ..." to
		// TaskFailureClassSecretLeak.
		logEvent.Msg("secrets: result.json scanned — BLOCK enforced, step will fail with SECRET_LEAK")
		return secrets.Redact(body, findings), fmt.Errorf("%w: %d finding(s)", ErrSecretLeakBlocked, len(findings))
	default: // ActionDetect
		logEvent.Msg("secrets: result.json scanned — detect-only, body left intact")
		return body, nil
	}
}

// scanToolAuditForSecrets scans a single tool-audit entry's input
// and output before persistence. Default action for this
// checkpoint is Detect — the audit log's job is to record what the
// agent actually did, and silent rewrites would mask the very
// thing operators audit for. Operators can override to Redact via
// secrets.yaml when they trust the detector enough to accept the
// loss of audit fidelity.
//
// Returns the (possibly-redacted) input and output. Detect-mode
// returns the originals plus a log line; Block degrades to
// Redact (refusing to persist the audit row would lose more
// signal than the redaction does).
func (e *Executor) scanToolAuditForSecrets(execution *persistence.Execution, stepID, tool, input, output string) (string, string) {
	if e.secretsDetector == nil {
		return input, output
	}
	inputFindings := e.secretsDetector.Scan([]byte(input))
	outputFindings := e.secretsDetector.Scan([]byte(output))
	if len(inputFindings) == 0 && len(outputFindings) == 0 {
		return input, output
	}
	action := secrets.ResolveAction(secrets.CheckpointToolAudit, e.secretsActions)
	combined := append([]secrets.Finding{}, inputFindings...)
	combined = append(combined, outputFindings...)
	counts := secrets.CountByType(combined)
	logEvent := e.logger.Warn().
		Str("execution_id", execution.ID).
		Str("step", stepID).
		Str("tool", tool).
		Str("checkpoint", secrets.CheckpointToolAudit).
		Str("action", string(action)).
		Int("input_findings", len(inputFindings)).
		Int("output_findings", len(outputFindings)).
		Interface("by_type", counts)

	switch action {
	case secrets.ActionRedact:
		logEvent.Msg("secrets: tool audit scanned — redacting before persist")
		return string(secrets.Redact([]byte(input), inputFindings)),
			string(secrets.Redact([]byte(output), outputFindings))
	case secrets.ActionBlock:
		// Block-on-audit degrades to Redact: dropping the audit
		// row hurts observability more than redaction does, AND
		// Phase 1 doesn't enforce block anywhere yet (Phase 2
		// brings the SECRET_LEAK failure class). Make both
		// degradations explicit in the log.
		logEvent.Msg("secrets: tool audit — BLOCK ACTION NOT YET ENFORCED, degraded to redact")
		return string(secrets.Redact([]byte(input), inputFindings)),
			string(secrets.Redact([]byte(output), outputFindings))
	default: // ActionDetect (default for this checkpoint)
		logEvent.Msg("secrets: tool audit scanned — detect-only, raw input/output retained")
		return input, output
	}
}

// scanContainerLogsForSecrets is the read-time scan used when
// surfacing the last 50 lines of container output on a failed-task
// error. The actual container log is NOT modified — only the bytes
// we display. Container logs frequently include `printenv` output
// from poorly-prompted shell tool calls; redaction at this layer
// is the last line of defence before the operator's screen.
func (e *Executor) scanContainerLogsForSecrets(ctx context.Context, execution *persistence.Execution, stepID string, body []byte) []byte {
	if e.secretsDetector == nil || len(body) == 0 {
		return body
	}
	findings := e.secretsDetector.Scan(body)
	if len(findings) == 0 {
		return body
	}
	action := secrets.ResolveAction(secrets.CheckpointContainerLogs, e.secretsActions)
	counts := secrets.CountByType(findings)
	logEvent := e.logger.Warn().
		Str("execution_id", execution.ID).
		Str("step", stepID).
		Str("checkpoint", secrets.CheckpointContainerLogs).
		Str("action", string(action)).
		Int("findings", len(findings)).
		Interface("by_type", counts)

	switch action {
	case secrets.ActionRedact:
		logEvent.Msg("secrets: container log scanned — redacting before display")
		return secrets.Redact(body, findings)
	case secrets.ActionBlock:
		// Block on container logs degrades to Redact: refusing
		// to display the failure log to the operator hurts more
		// than it helps, AND Phase 1 doesn't enforce block
		// anywhere yet. Make the degradation visible in logs so
		// an operator who configured block doesn't misread this
		// as enforcement.
		logEvent.Msg("secrets: container log — BLOCK ACTION NOT YET ENFORCED, degraded to redact")
		return secrets.Redact(body, findings)
	default: // ActionDetect
		logEvent.Msg("secrets: container log scanned — detect-only")
		return body
	}
}
