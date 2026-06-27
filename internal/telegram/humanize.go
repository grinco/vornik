package telegram

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// humanizeTaskMessage turns the raw `result.message` field from a
// completed agent step into a Telegram-friendly summary.
//
// Why it exists: roles that emit structured JSON (reviewer:
// `{"approved": ..., "feedback": "..."}`, scout: `{"summary":
// "...", "next_steps": [...]}`, etc.) put the entire JSON blob in
// `message`. The Telegram bot used to paste that verbatim, leaving
// operators staring at unformatted JSON when all they want is the
// summary the model wrote.
//
// Strategy:
//  1. Try to parse as a JSON object.
//  2. If parsed, extract a prose summary from the most-likely
//     fields (summary > message > feedback > body > content >
//     details > response > error > reason). Decorate with status
//     glyphs when an "approved" or "status" boolean/string is
//     present.
//  3. If no prose field matches, format the top-level keys as a
//     short bullet list ("• approved: true · • score: 95").
//  4. If the input isn't JSON (plain text, markdown), pass it
//     through unchanged.
//
// Truncation is the caller's job — the helper produces the cleanest
// possible string and lets NotifyTaskCompleted's existing length
// cap apply to the result. That keeps the helper testable without
// the truncation noise.
func humanizeTaskMessage(message string) string {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return ""
	}

	// JSON object detection. A candidate must start with `{` and
	// parse cleanly; everything else (markdown, plain text, JSON
	// arrays, scalars) passes through.
	if !strings.HasPrefix(trimmed, "{") {
		return trimmed
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil || len(obj) == 0 {
		return trimmed
	}

	prefix := decorateStatus(obj)
	prose := extractProseField(obj)
	if prose != "" {
		if prefix != "" {
			return prefix + "\n\n" + prose
		}
		return prose
	}

	// No obvious prose field. Fall back to a compact top-level
	// bullet list so the operator can at least see what the agent
	// produced rather than reading the JSON. Keys sorted for
	// stable output across runs.
	bullets := summarizeFields(obj)
	if bullets == "" {
		// Defensive: empty fields → return the original JSON so
		// the operator can debug. Better that than a misleading
		// blank notification.
		return trimmed
	}
	if prefix != "" {
		return prefix + "\n\n" + bullets
	}
	return bullets
}

// decorateStatus inspects well-known status fields and returns a
// short headline glyph + label. Empty when nothing matches.
//
// Priority: approved bool > status string (commonly "ok" / "fail" /
// "rejected") > error/reason string (presence implies failure).
// Only the first match fires; mixed signals fall through to the
// approved-bool branch first since reviewer agents are the most
// frequent JSON-emitter.
func decorateStatus(obj map[string]any) string {
	if v, ok := obj["approved"]; ok {
		if b, isBool := v.(bool); isBool {
			if b {
				return "✓ Approved"
			}
			return "✗ Rejected"
		}
	}
	if v, ok := obj["status"]; ok {
		if s, isStr := v.(string); isStr && s != "" {
			s = strings.ToLower(s)
			switch s {
			case "ok", "success", "succeeded", "passed", "approved", "completed":
				return "✓ " + cap1(s)
			case "fail", "failed", "rejected", "error", "denied":
				return "✗ " + cap1(s)
			default:
				return "• " + cap1(s)
			}
		}
	}
	return ""
}

// extractProseField walks the priority list and returns the first
// non-empty string value found. Trims surrounding whitespace.
// Keeps the priority list short — adding more fields here just
// means more possible "true name" of the summary, which encourages
// agents to be inconsistent. The current list is the empirical
// "what reviewer/scout/coder roles actually emit" set.
func extractProseField(obj map[string]any) string {
	for _, key := range []string{
		"summary",
		"message",
		"feedback",
		"body",
		"content",
		"details",
		"response",
		"error",
		"reason",
	} {
		if v, ok := obj[key]; ok {
			if s, isStr := v.(string); isStr {
				s = strings.TrimSpace(s)
				if s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// summarizeFields produces a compact bullet list of top-level keys
// + their values, used when no prose field is available. Skips
// keys whose values are the structural noise downstream consumers
// don't care about (toolAudit, usage, diagnostics — these are
// machine-only). Long string values are truncated so a single
// pathological field doesn't blow out the rendered message.
func summarizeFields(obj map[string]any) string {
	skip := map[string]bool{
		"toolAudit":       true,
		"usage":           true,
		"diagnostics":     true,
		"outputArtifacts": true,
		"delegatedTasks":  true,
		"exit_code":       true,
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		if skip[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if len(keys) == 0 {
		return ""
	}

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("• ")
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(formatValue(obj[k]))
	}
	return b.String()
}

// formatValue renders one map value as a compact human string.
// Long strings get truncated; nested objects/arrays collapse to a
// shape descriptor so the bullet list stays scannable.
func formatValue(v any) string {
	switch val := v.(type) {
	case nil:
		return "null"
	case bool:
		if val {
			return "true"
		}
		return "false"
	case string:
		s := strings.TrimSpace(val)
		// Truncation must be rune-aware: agent outputs from
		// non-English deployments contain multi-byte
		// characters (CJK, emoji, Cyrillic), and slicing at
		// byte offset 120 in the middle of one would emit
		// invalid UTF-8 — Telegram's API rejects messages
		// containing invalid UTF-8 entirely. Convert to runes,
		// trim, convert back.
		const maxRunes = 120
		if r := []rune(s); len(r) > maxRunes {
			s = string(r[:maxRunes]) + "…"
		}
		return s
	case float64:
		// JSON numbers come back as float64. Print integer-shaped
		// values without the trailing zero so "score: 95" not
		// "score: 95.000000".
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%g", val)
	case []any:
		return fmt.Sprintf("[%d items]", len(val))
	case map[string]any:
		return fmt.Sprintf("{%d fields}", len(val))
	default:
		return fmt.Sprintf("%v", v)
	}
}

// cap1 capitalises the first rune so "approved" → "Approved".
// ASCII-only — agent statuses are conventionally English.
func cap1(s string) string {
	if s == "" {
		return s
	}
	first := s[0]
	if first >= 'a' && first <= 'z' {
		first -= 'a' - 'A'
	}
	return string(first) + s[1:]
}
