package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestLeadRoleSystemPromptDoesNotContainWrongShape walks every
// distributed swarm YAML — both the runnable swarm configs under
// configs/swarms/ and the templates under internal/cli/presets/ that
// `vornikctl init swarm --template` ships — and asserts that no
// "lead-class" role's systemPrompt contains a literal example with
// the wrong-shape `{"plan": [{...}]}` signature.
//
// Why this matters: parsePlanSteps expects
// {"plan":{"steps":["role"],"rationale":"..."}, "message":"..."} and
// the executor injects that authoritative format spec into the
// lead's prompt at runtime (see plan_step.go:672-686). A role
// systemPrompt that prints a contradicting example puts two specs
// in front of the model — the failure mode pinned in
// TestParsePlanSteps_RegressionWrongShape. This test catches the
// regression class at config time so a future copy-paste from an
// outdated README or PR can't reintroduce it silently.
//
// Lead-class detection: any role with `requiredOutputKeys` containing
// "plan". That covers every swarm preset we ship today (lead role
// names vary by template — "lead", "coordinator", "router", etc. —
// but the requiredOutputKeys signature is invariant).
func TestLeadRoleSystemPromptDoesNotContainWrongShape(t *testing.T) {
	// Pattern: a JSON-style "plan" key whose value starts with `[`,
	// optionally with whitespace. Tolerant of single or double quotes
	// and inner whitespace so a future operator typing
	//   'plan' : [ { ...
	// still trips the lint.
	wrongShape := regexp.MustCompile(`["']plan["']\s*:\s*\[\s*\{`)

	repoRoot := repoRootFromTest(t)
	scanRoots := []string{
		filepath.Join(repoRoot, "configs", "swarms"),
		filepath.Join(repoRoot, "internal", "cli", "presets"),
	}

	checked := 0
	leadRolesChecked := 0
	for _, root := range scanRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			// Only SWARM.md; skip backups, JSON, etc.
			// (YAML format was removed for swarms 2026-05-17.)
			if !strings.HasSuffix(path, ".md") {
				return nil
			}
			if strings.Contains(path, ".bak") {
				return nil
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			checked++

			// Parse the SWARM.md frontmatter as the swarm doc. Files
			// without frontmatter (e.g. README.md) are silently
			// skipped — they're not swarms.
			frontmatter, _, fmErr := splitMarkdownFrontmatter(data)
			if fmErr != nil {
				return nil
			}
			var doc swarmDocForLint
			if err := yaml.Unmarshal(frontmatter, &doc); err != nil {
				return nil
			}
			for _, role := range doc.Roles {
				if !roleHasRequiredKey(role.RequiredOutputKeys, "plan") {
					continue
				}
				leadRolesChecked++
				if wrongShape.MatchString(role.SystemPrompt) {
					t.Errorf("%s role %q systemPrompt contains the wrong-shape "+
						"`{\"plan\": [{...}]}` example. parsePlanSteps expects "+
						"`{\"plan\":{\"steps\":[]string}}` — see "+
						"TestParsePlanSteps_RegressionWrongShape and "+
						"plan_step.go:672 for the canonical spec.",
						relPath(repoRoot, path), role.Name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}

	// Sanity: the test is only useful if it's actually inspecting
	// something. Catch a future tree refactor that moves the .md
	// files out from under us before the assertion silently passes.
	if checked == 0 {
		t.Fatalf("no SWARM.md files inspected — expected lint to walk %v", scanRoots)
	}
	if leadRolesChecked == 0 {
		t.Fatalf("walked %d YAMLs but found zero lead-class roles "+
			"(roles with requiredOutputKeys containing \"plan\"). "+
			"The lint is no-op; verify the swarm tree still "+
			"defines lead roles the way this test detects them.", checked)
	}
}

// TestRoleSystemPromptsDoNotConflictWithWorkflowStepShape is the
// self-extending generalisation of the four named role lints
// (analyst/coder/tester/reviewer) that previously lived here.
//
// Scope note: this lint reads the raw YAML's `requiredOutputKeys`
// field. Roles that have migrated to `outputSchema:` (item 6 phase 1
// of https://docs.vornik.io) leave that legacy
// field unset, and the lint won't see them. That's intended for
// phase 1 — the Validate-time derivation populates the in-memory
// SwarmRole with the legacy fields for downstream consumers, but the
// YAML's `requiredOutputKeys` source remains empty and the schema
// itself lives in a structured field where there's no inline example
// to lint against. Phase 2 (renderSchemaForPrompt + role migration)
// removes the regression class entirely for migrated roles. Each
// of those lints checked one specific key with the same logic; a
// future role we ship (e.g. an "auditor" with
// `requiredOutputKeys: [audit]`) needed a sixth test function or
// it wasn't covered.
//
// New shape, scoped to the actual conflict class:
//
//  1. Walk every workflow YAML, build a set of (role, requiredKey)
//     pairs where the step's `prompt` field pins a literal
//     `"key": {` shape. These are the call sites that compete
//     with the role's systemPrompt for the model's attention.
//  2. Walk every shipped swarm + preset YAML. For each role, for
//     each entry K in `requiredOutputKeys`, fail if BOTH:
//     - the role's systemPrompt contains `"K":\s*\{`, AND
//     - some workflow step uses this role with a `"K":\s*\{`
//     shape in its prompt.
//
// This captures the analyst/coder/tester/reviewer/writer regression
// class precisely without false-positiving roles that are invoked
// only via the adaptive lead-plan delegation (feasibility, scout,
// architect) — those have no per-step prompt to compete with the
// role-level example, so the example is fine for now. Once the
// `outputSchema` field lands (item 6 of
// https://docs.vornik.io), this lint will
// tighten to disallow any inline shape example regardless of
// workflow usage.
//
// The lead's wrong-shape `"plan":\s*\[` regression has its own
// dedicated lint above — different failure shape (array vs.
// object), same root cause. Keeping both lints means a future
// operator who manages to introduce only one of the two patterns
// still gets caught.
func TestRoleSystemPromptsDoNotConflictWithWorkflowStepShape(t *testing.T) {
	repoRoot := repoRootFromTest(t)

	// Step 1: scan workflows, accumulate (role, key) pairs whose
	// step prompts pin a literal `"key": {` shape.
	workflowConflicts := scanWorkflowStepShapePins(t, repoRoot)
	if len(workflowConflicts) == 0 {
		t.Fatalf("walked workflows but found zero step prompts pinning "+
			"a literal `\"K\": {` shape — the lint can't fire. Verify "+
			"%s still contains workflow YAMLs and at least one of "+
			"them still pins shapes (dev-pipeline.yaml is the canonical "+
			"example).",
			filepath.Join(repoRoot, "configs", "workflows"))
	}

	// Step 2: scan swarms + presets, fail when a role's systemPrompt
	// pins a shape for a key that workflowConflicts says is also pinned
	// at a step prompt.
	scanRoots := []string{
		filepath.Join(repoRoot, "configs", "swarms"),
		filepath.Join(repoRoot, "internal", "cli", "presets"),
	}

	patternCache := map[string]*regexp.Regexp{}
	patternFor := func(key string) *regexp.Regexp {
		if r, ok := patternCache[key]; ok {
			return r
		}
		r := regexp.MustCompile(`["']` + regexp.QuoteMeta(key) + `["']\s*:\s*\{`)
		patternCache[key] = r
		return r
	}

	yamlsChecked := 0
	for _, root := range scanRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".md") {
				return nil
			}
			if strings.Contains(path, ".bak") {
				return nil
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			yamlsChecked++

			frontmatter, _, fmErr := splitMarkdownFrontmatter(data)
			if fmErr != nil {
				return nil
			}
			var doc swarmDocForLint
			if err := yaml.Unmarshal(frontmatter, &doc); err != nil {
				return nil
			}
			for _, role := range doc.Roles {
				for _, entry := range role.RequiredOutputKeys {
					key := strings.TrimSpace(entry)
					if i := strings.Index(key, ":"); i > 0 {
						key = strings.TrimSpace(key[:i])
					}
					// Dotted paths address nested fields; the
					// role's prompt rarely pins those literally
					// and the leaf-segment match would false-
					// positive on common identifiers.
					if key == "" || strings.Contains(key, ".") {
						continue
					}
					if _, conflicts := workflowConflicts[workflowConflictKey{Role: role.Name, Key: key}]; !conflicts {
						continue
					}
					if patternFor(key).MatchString(role.SystemPrompt) {
						t.Errorf("%s role %q systemPrompt contains a literal "+
							"`\"%s\": {...}` example AND at least one workflow "+
							"step using this role pins a different `\"%s\": {...}` "+
							"shape in its `prompt` field. Two contracts in front "+
							"of the model — drop the role-level example and let "+
							"the workflow step's prompt be authoritative. See "+
							"commit history on configs/swarms/dev-swarm.yaml "+
							"for the fix pattern.",
							relPath(repoRoot, path), role.Name, key, key)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}

	if yamlsChecked == 0 {
		t.Fatalf("walked swarm + preset roots but inspected zero YAMLs — "+
			"expected lint to walk %v", scanRoots)
	}
}

// workflowConflictKey identifies a (role, top-level key) pair whose
// shape is pinned by at least one workflow step's `prompt` field.
type workflowConflictKey struct {
	Role string
	Key  string
}

// scanWorkflowStepShapePins walks configs/workflows/, parses each
// YAML, and returns the set of (role, key) pairs where some step
// using that role has a `prompt:` containing a literal
// `"key":\s*\{` example. The returned set is the input to the
// role-side lint above.
//
// Tolerant of multiple workflow YAMLs and steps; absence of either
// produces an empty set (the lint then fails its own sanity check).
func scanWorkflowStepShapePins(t *testing.T, repoRoot string) map[workflowConflictKey]struct{} {
	t.Helper()
	root := filepath.Join(repoRoot, "configs", "workflows")
	out := map[workflowConflictKey]struct{}{}
	// Match `"identifier":\s*\{` — same regex shape as the role-side
	// lint, applied to step prompts. The captured group is the key
	// name so we can build the (role, key) pair.
	keyPattern := regexp.MustCompile(`["']([a-zA-Z_][a-zA-Z0-9_]*)["']\s*:\s*\{`)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		frontmatter, body, fmErr := splitMarkdownFrontmatter(data)
		if fmErr != nil {
			return nil
		}
		var doc workflowDocForLint
		if err := yaml.Unmarshal(frontmatter, &doc); err != nil {
			return nil
		}
		// In the WORKFLOW.md shape, step prompts live in body
		// `### <step-id>` subsections under `## Prompts` rather
		// than inline in the frontmatter. Pull each subsection's
		// prose and pair it with the role from the frontmatter.
		bodyPrompts := extractStepPromptsFromBody(string(body))
		for stepID, step := range doc.Steps {
			role := step.Role
			if role == "" {
				continue
			}
			promptText := step.Prompt
			if promptText == "" {
				promptText = bodyPrompts[stepID]
			}
			if promptText == "" {
				continue
			}
			for _, m := range keyPattern.FindAllStringSubmatch(promptText, -1) {
				out[workflowConflictKey{Role: role, Key: m[1]}] = struct{}{}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

// workflowDocForLint mirrors the minimal subset of the workflow YAML
// that the lint cares about. Workflow YAMLs in this repo encode steps
// either as a map (dev-pipeline, simple-workflow) or via gates; we
// only need (role, prompt) per step to detect shape pinning.
type workflowDocForLint struct {
	Steps map[string]struct {
		Role   string `yaml:"role"`
		Prompt string `yaml:"prompt"`
	} `yaml:"steps"`
}

// swarmDocForLint is the minimal subset of the swarm-config YAML
// shape this lint cares about. Kept separate from registry.SwarmRole
// so a future field rename in the registry struct doesn't silently
// break this test's parse — and the registry package isn't pulled
// in just to read two strings.
type swarmDocForLint struct {
	Roles []struct {
		Name               string   `yaml:"name"`
		RequiredOutputKeys []string `yaml:"requiredOutputKeys"`
		SystemPrompt       string   `yaml:"systemPrompt"`
	} `yaml:"roles"`
}

// roleHasRequiredKey returns whether the role's requiredOutputKeys
// list contains the named top-level key. The schema supports
// `name:type` entries — strip the type suffix before the comparison
// so future schema enrichment doesn't silently drop the match.
//
// Used by the lead-class lint (key="plan") and the analyst-class
// lint (key="analysis"); same shape, different role-class signature.
func roleHasRequiredKey(keys []string, want string) bool {
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if i := strings.Index(k, ":"); i > 0 {
			k = strings.TrimSpace(k[:i])
		}
		if k == want {
			return true
		}
	}
	return false
}

// repoRootFromTest walks up from the test's working directory until
// it finds a go.mod, so the test runs reliably regardless of
// `go test` invocation cwd. Falls back to t.Fatalf rather than
// returning an error: a missing go.mod means we're being run from
// somewhere unexpected and any path the test computes would be wrong.
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate go.mod walking up from %s", dir)
	return ""
}

func relPath(root, full string) string {
	if rel, err := filepath.Rel(root, full); err == nil {
		return rel
	}
	return full
}

// splitMarkdownFrontmatter peels a `---`-delimited YAML
// frontmatter block off the head of a SWARM.md / WORKFLOW.md
// file. Returns the YAML bytes (without the markers) and the
// body bytes after the closing marker. Mirrors
// internal/registry/mdfrontmatter.go's helper but inlined here
// to keep this lint test free of cross-package internals.
func splitMarkdownFrontmatter(content []byte) (frontmatter, body []byte, err error) {
	const marker = "---"
	trimmed := strings.TrimLeft(string(content), " \t\r\n")
	if !strings.HasPrefix(trimmed, marker) {
		return nil, nil, fmt.Errorf("no frontmatter marker")
	}
	rest := strings.TrimPrefix(trimmed, marker)
	if len(rest) == 0 || (rest[0] != '\n' && rest[0] != '\r') {
		return nil, nil, fmt.Errorf("marker not on own line")
	}
	rest = strings.TrimLeft(rest, "\r\n")
	idx := strings.Index(rest, "\n"+marker)
	if idx < 0 {
		// Allow `---` at the very end of the file without a
		// preceding newline (unlikely but harmless).
		if strings.HasSuffix(rest, marker) {
			return []byte(rest[:len(rest)-len(marker)]), nil, nil
		}
		return nil, nil, fmt.Errorf("no closing marker")
	}
	frontmatter = []byte(rest[:idx+1]) // include trailing newline
	body = []byte(strings.TrimPrefix(rest[idx+1+len(marker):], "\n"))
	return frontmatter, body, nil
}

// extractStepPromptsFromBody walks the Markdown body of a
// WORKFLOW.md and returns a map of step-id → prompt body for
// every `### <step-id>` subheading inside the `## Prompts`
// section. Mirrors internal/registry/mdfrontmatter.go's
// extractSections; inlined here to keep the lint test free of
// cross-package internals.
func extractStepPromptsFromBody(body string) map[string]string {
	out := map[string]string{}
	if body == "" {
		return out
	}
	const promptsHeading = "## Prompts"
	lines := strings.Split(body, "\n")
	var (
		inTarget bool
		curID    string
		curBody  []string
	)
	flush := func() {
		if curID == "" {
			return
		}
		out[curID] = strings.TrimSpace(strings.Join(curBody, "\n"))
		curID = ""
		curBody = nil
	}
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "## ") && !strings.HasPrefix(trim, "### ") {
			flush()
			inTarget = trim == promptsHeading
			continue
		}
		if !inTarget {
			continue
		}
		if strings.HasPrefix(trim, "### ") {
			flush()
			curID = strings.TrimSpace(strings.TrimPrefix(trim, "###"))
			continue
		}
		if curID == "" {
			continue
		}
		curBody = append(curBody, line)
	}
	flush()
	return out
}
