package executor

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// tool_credentials.go — credential carryover capture (step 3).
// See https://docs.vornik.io
//
// When a trusted tool (operator-configured) returns an access credential in
// its structured output (e.g. a PageDrop viewing password), the daemon
// captures it deterministically — no LLM — into task_credentials so it can be
// surfaced code-formatted + copyable instead of being redacted as a generic
// secret. A value matching a STRONG credential pattern is never captured.

// ToolCredentialMapping is the executor-local form of a
// config.ToolCredentialConfig (the executor stays decoupled from
// internal/config). The wiring converts config → this at construction.
//
// Two extraction modes: a JSON dotted-path (CredentialField, for tools that
// return a JSON object) or a text regexp (CredRE, one capture group, for tools
// that return human-readable prose like PageDrop). CredRE takes precedence.
type ToolCredentialMapping struct {
	// Tool is the tool-name prefix whose output carries the credential.
	Tool string
	// CredentialField is a dotted path into the tool result JSON.
	CredentialField string
	// ArtifactField is an optional dotted path to the URL it unlocks.
	ArtifactField string
	// CredRE / ArtRE are compiled text-extraction patterns (capture group 1 =
	// value). Non-nil selects text mode over the JSON field path. Compiled by
	// the wiring (invalid patterns are dropped there, with a log).
	CredRE *regexp.Regexp
	ArtRE  *regexp.Regexp
	// Label is the operator-facing name ("viewing password").
	Label string
}

// WithToolCredentials wires the capture mappings + the store. Both must be
// non-empty/non-nil for capture to run; either unset disables it (default).
func WithToolCredentials(mappings []ToolCredentialMapping, repo persistence.TaskCredentialRepository) Option {
	return func(e *Executor) {
		e.toolCredentialMappings = mappings
		e.taskCredentialRepo = repo
	}
}

// captureToolCredential extracts and stores an operator-facing credential from
// one tool-audit entry's raw output, if the tool matches a configured mapping.
// Best-effort: any miss (no mapping, unparseable output, absent field,
// strong-pattern value, store error) logs and returns without failing the step.
func (e *Executor) captureToolCredential(ctx context.Context, task *persistence.Task, execution *persistence.Execution, tool, rawOutput string) {
	if len(e.toolCredentialMappings) == 0 || e.taskCredentialRepo == nil {
		return
	}
	m, ok := e.matchCredentialMapping(tool)
	if !ok {
		return
	}
	var (
		value       string
		artifactURL string
	)
	if m.CredRE != nil {
		// Text mode: extract from the tool's human-readable output.
		text := toolResultText(rawOutput)
		value = firstSubmatch(m.CredRE, text)
		if m.ArtRE != nil {
			artifactURL = firstSubmatch(m.ArtRE, text)
		}
	} else {
		// JSON mode: dotted-path into the structured result.
		obj, ok := parseToolResultJSON(rawOutput)
		if !ok {
			e.logger.Debug().Str("tool", tool).Str("execution_id", execution.ID).
				Msg("tool-credential: output not parseable as JSON — no capture")
			return
		}
		value, _ = lookupDottedPath(obj, m.CredentialField)
		if m.ArtifactField != "" {
			artifactURL, _ = lookupDottedPath(obj, m.ArtifactField)
		}
	}
	if value == "" {
		e.logger.Debug().Str("tool", tool).
			Msg("tool-credential: credential value absent/empty — no capture")
		return
	}
	// Strong-pattern denylist: even from a trusted tool + correct mapping, a
	// value matching a strong, prefix-anchored credential pattern (openai/
	// anthropic/github/aws/jwt/…) is refused — those always redact. Only the
	// heuristic (generic_kv/entropy) shape, which a viewing password takes, is
	// eligible. Uses the redactor's own registry (no ad-hoc list here).
	if e.secretsDetector != nil {
		if len(dropHeuristicFindings(e.secretsDetector.Scan([]byte(value)))) > 0 {
			e.logger.Debug().Str("tool", tool).
				Msg("tool-credential: value matches a strong secret pattern — refusing capture")
			return
		}
	}
	cred := &persistence.TaskCredential{
		TaskID:      task.ID,
		ExecutionID: execution.ID,
		Tool:        tool,
		Label:       m.Label,
		Value:       value,
		ArtifactURL: artifactURL,
	}
	if err := e.taskCredentialRepo.Upsert(ctx, cred); err != nil {
		e.logger.Error().Err(err).Str("tool", tool).Str("execution_id", execution.ID).
			Msg("tool-credential: failed to store captured credential")
		return
	}
	e.logger.Info().
		Str("tool", tool).
		Str("field", m.CredentialField).
		Bool("has_artifact_url", artifactURL != "").
		Str("task_id", task.ID).
		Str("execution_id", execution.ID).
		Msg("tool-credential: captured operator-facing credential")
}

// matchCredentialMapping returns the first mapping whose Tool is a prefix of
// tool (same prefix-anchored match as isTrustedOutputTool).
func (e *Executor) matchCredentialMapping(tool string) (ToolCredentialMapping, bool) {
	for _, m := range e.toolCredentialMappings {
		if toolNameMatchesPrefix(tool, m.Tool) {
			return m, true
		}
	}
	return ToolCredentialMapping{}, false
}

// toolResultText returns the human-readable text of a tool result for regexp
// extraction: the first type:"text" item's text when the output is an MCP
// content envelope, else the raw output verbatim (PageDrop returns prose, not
// JSON).
func toolResultText(raw string) string {
	raw = strings.TrimSpace(raw)
	var top map[string]any
	if err := json.Unmarshal([]byte(raw), &top); err == nil {
		if content, ok := top["content"].([]any); ok {
			for _, item := range content {
				mi, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := mi["type"].(string); t != "text" {
					continue
				}
				if text, ok := mi["text"].(string); ok {
					return text
				}
			}
		}
	}
	return raw
}

// firstSubmatch returns capture group 1 of re against s, or "" when there's no
// match or no group.
func firstSubmatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// parseToolResultJSON parses a tool's raw output into a result object, handling
// two MCP shapes: a top-level JSON object, and the
// {"content":[{"type":"text","text":"<json>"}]} envelope (the FIRST text item's
// text is parsed as the result; malformed inner JSON → no result).
func parseToolResultJSON(raw string) (map[string]any, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		return nil, false
	}
	if inner, ok := unwrapContentEnvelope(top); ok {
		return inner, true
	}
	return top, true
}

// unwrapContentEnvelope returns the JSON object embedded in the first
// type:"text" item of an MCP content envelope, if top is one. Returns false
// when top is not a content envelope or the first text item isn't a JSON
// object (malformed → no-op, per the design).
func unwrapContentEnvelope(top map[string]any) (map[string]any, bool) {
	content, ok := top["content"].([]any)
	if !ok || len(content) == 0 {
		return nil, false
	}
	for _, item := range content {
		mi, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := mi["type"].(string); t != "text" {
			continue
		}
		text, ok := mi["text"].(string)
		if !ok {
			continue
		}
		var inner map[string]any
		if err := json.Unmarshal([]byte(text), &inner); err == nil {
			return inner, true
		}
		return nil, false // first text item's JSON is malformed — no capture
	}
	return nil, false
}

// lookupDottedPath walks a dotted path (e.g. "data.password") into obj and
// returns the leaf as a string. No array indexing; a missing key at any level
// returns false.
func lookupDottedPath(obj map[string]any, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	var cur any = obj
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[part]
		if !ok {
			return "", false
		}
	}
	return stringifyScalar(cur)
}

// stringifyScalar renders a JSON scalar as a string. Non-scalars (objects,
// arrays, null) return false — a credential is a scalar.
func stringifyScalar(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(x), true
	default:
		return "", false
	}
}
