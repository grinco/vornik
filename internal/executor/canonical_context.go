package executor

// Agent context-discovery hardening — Layer 1 (canonical-context
// pre-load). See https://docs.vornik.io
//
// Reads the project's PROJECT_CONTEXT.md + USER_GUIDANCE.md off disk
// at workspace-prep time so the agent gets them in task.json without
// burning 3-5 tool calls walking the workspace to find them. Naming-
// drift tolerant — both `.autonomy/` (current convention) and
// `autonomy/` (legacy) are recognised; both casings of the filenames
// resolve.

import (
	"errors"
	"fmt"
	"os"

	"vornik.io/vornik/internal/safepath"
)

// canonicalContextMaxBytes caps each pre-loaded file. A 200-line spec
// stuffed into PROJECT_CONTEXT.md would otherwise balloon task.json
// and push the LLM's context budget. Larger files are truncated with
// a trailing marker so the agent knows the source isn't complete.
const canonicalContextMaxBytes = 16 * 1024

// canonicalContextSources enumerates the directory naming conventions
// the resolver tries in order. First hit wins per field; mixed
// (both directories present) flags via the "mixed" source for the
// telemetry counter.
var canonicalContextSources = []string{".autonomy", "autonomy"}

// canonicalContextFilenames lists the two casings the resolver
// accepts for each context file. PROJECT_CONTEXT.md is the
// documented convention; project_context.md exists in older
// workspaces / test fixtures from before the convention settled.
var canonicalContextFilenames = map[string][]string{
	"project": {"PROJECT_CONTEXT.md", "project_context.md"},
	"user":    {"USER_GUIDANCE.md", "user_guidance.md"},
}

// CanonicalContext is the result of a per-task pre-load. Empty
// when the project doesn't use the autonomy convention — the
// caller falls back to the legacy behaviour (agent walks the
// workspace itself).
type CanonicalContext struct {
	// ProjectContext is the verbatim body of PROJECT_CONTEXT.md
	// (or the equivalent — see canonicalContextSources +
	// canonicalContextFilenames for the lookup order). Empty
	// when no such file exists. Capped at canonicalContextMaxBytes.
	ProjectContext string

	// UserGuidance is the verbatim body of USER_GUIDANCE.md.
	// Same shape + cap as ProjectContext.
	UserGuidance string

	// Source reports which convention resolved. One of:
	//   ""              — no canonical context found
	//   "dot_autonomy"  — pulled from .autonomy/ only
	//   "plain_autonomy"— pulled from autonomy/ only
	//   "mixed"         — both directories present; first-hit
	//                     path wins per field. Operator should
	//                     run `vornikctl workspace canonicalise`
	//                     to drop the legacy dir.
	Source string

	// Truncated is the list of file labels ("project" / "user")
	// whose content exceeded canonicalContextMaxBytes. Drives
	// the telemetry counter + lets the agent system prompt
	// surface a "partial" warning per field.
	Truncated []string
}

// Empty reports whether nothing was loaded — the convention
// hasn't been adopted by this project.
func (c CanonicalContext) Empty() bool {
	return c.ProjectContext == "" && c.UserGuidance == ""
}

// resolveCanonicalContext walks the candidate paths and returns the
// pre-loaded context. workspacePath is the project's workspace root
// (the same value the executor mounts into the agent container at
// /app/workspace). Empty workspacePath returns a zero-value result
// so callers don't have to nil-check.
//
// Resolution is best-effort: a missing file, an unreadable file, a
// symlinked file (rejected by safepath), or any I/O error during
// the read all degrade to "field absent". The agent still works —
// it just falls back to the filesystem walk it would have done
// pre-feature.
func resolveCanonicalContext(workspacePath string) CanonicalContext {
	var out CanonicalContext
	if workspacePath == "" {
		return out
	}

	// Track which source directories actually contributed a file.
	// Used to compute the "mixed" tag below.
	usedDot, usedPlain := false, false

	projContent, projTrunc, projSource := loadCanonicalFile(workspacePath, "project")
	if projContent != "" {
		out.ProjectContext = projContent
		if projTrunc {
			out.Truncated = append(out.Truncated, "project")
		}
		switch projSource {
		case ".autonomy":
			usedDot = true
		case "autonomy":
			usedPlain = true
		}
	}

	userContent, userTrunc, userSource := loadCanonicalFile(workspacePath, "user")
	if userContent != "" {
		out.UserGuidance = userContent
		if userTrunc {
			out.Truncated = append(out.Truncated, "user")
		}
		switch userSource {
		case ".autonomy":
			usedDot = true
		case "autonomy":
			usedPlain = true
		}
	}

	switch {
	case usedDot && usedPlain:
		out.Source = "mixed"
	case usedDot:
		out.Source = "dot_autonomy"
	case usedPlain:
		out.Source = "plain_autonomy"
	}
	return out
}

// loadCanonicalFile tries every (dir, filename) pair in precedence
// order and returns the first hit. Returns (content, truncated,
// sourceDir) where sourceDir is the directory name that
// contributed (without the leading dot).
func loadCanonicalFile(workspacePath, label string) (string, bool, string) {
	for _, dir := range canonicalContextSources {
		for _, name := range canonicalContextFilenames[label] {
			body, truncated, ok := tryReadCanonical(workspacePath, dir, name)
			if !ok {
				continue
			}
			return body, truncated, dir
		}
	}
	return "", false, ""
}

// tryReadCanonical attempts one (dir, filename) pair under
// workspacePath. Returns (body, truncated, ok). The safepath
// join rejects path traversal + symlinks pointing outside the
// workspace.
func tryReadCanonical(workspacePath, dir, name string) (string, bool, bool) {
	target, err := safepath.JoinUnder(workspacePath, dir, name)
	if err != nil {
		return "", false, false
	}
	info, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, false
		}
		// Stat error other than "not exist" — log via the caller's
		// audit channel would be nice but the helper is pure;
		// return absent and move on.
		return "", false, false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// Symlinks are refused defensively. A future workspace
		// canonicaliser may want to allow them, but v1 keeps the
		// lookup straight-forward.
		return "", false, false
	}
	if !info.Mode().IsRegular() {
		return "", false, false
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "", false, false
	}
	if len(data) > canonicalContextMaxBytes {
		truncated := truncateWithMarker(data, canonicalContextMaxBytes)
		return truncated, true, true
	}
	return string(data), false, true
}

// truncateWithMarker cuts data to maxBytes and appends a marker
// noting the original size. The marker lives outside the maxBytes
// budget intentionally — readers shouldn't lose more content to
// the warning.
func truncateWithMarker(data []byte, maxBytes int) string {
	if len(data) <= maxBytes {
		return string(data)
	}
	dropped := len(data) - maxBytes
	return string(data[:maxBytes]) + fmt.Sprintf("\n\n...[truncated %d bytes]\n", dropped)
}

// canonicalContextSystemPromptBlock is the guidance the agent
// system prompt gains when the pre-load populated something.
// Wording is deliberately concrete about the JSON paths the
// agent should consult — the LLM follows literal field names
// more reliably than abstract instructions.
const canonicalContextSystemPromptBlock = `
You have canonical project context already loaded in your task.json:

* context.projectContext — the project's PROJECT_CONTEXT.md
  (mission, scope, source-of-truth links).
* context.userGuidance — operator preferences and standing rules
  (what to prefer, what to avoid).

ALWAYS read these fields BEFORE running file_read or glob to
discover project structure or operator preferences. They are
the same files you'd find under .autonomy/ — pre-loaded so you
don't waste tool calls.

When the operator's prompt references a fact "in project
context" or "in the spec", consult these fields first. If the
fact isn't there, refuse to fabricate; respond that the spec
is missing the named fact.
`

// composeSystemPromptWithCanonicalContext appends the canonical-
// context guidance block to the role's system prompt. Empty
// role prompt + empty canonical context returns "" (caller's
// guard already skips the field in that case); empty role
// prompt + non-empty canonical context emits just the block;
// non-empty role prompt + canonical context emits the role
// prompt followed by the block.
//
// The block is appended (not prepended) so the role's identity
// instructions still come first — the agent's primary contract
// is "be the X role"; the context-discovery rules are a
// secondary "and while you're at it, save tool calls."
func composeSystemPromptWithCanonicalContext(rolePrompt string, ctx CanonicalContext) string {
	if ctx.Empty() {
		return rolePrompt
	}
	if rolePrompt == "" {
		return canonicalContextSystemPromptBlock
	}
	return rolePrompt + "\n\n" + canonicalContextSystemPromptBlock
}
