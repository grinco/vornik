package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vornik.io/vornik/internal/promptblock"
)

// L1 of the agent-quality benchmark: per-source byte attribution of a composed
// agent system prompt
// (https://docs.vornik.io §3.1).
//
// WHY BYTES AND NOT TOKENS (§3.1a). A token count moves when a tokenizer
// version does, which would make the strongest gate in the benchmark depend on
// a dependency nobody is watching. Bytes are exact and tokenizer-independent.
// Tokens are still the number a reader wants, so they are reported downstream
// and labelled with the tokenizer that produced them — but they are not what
// fails CI.
//
// WHY THIS RUNS THE PRODUCTION COMPOSERS. A dedicated L1 composer would gate a
// copy: change the real chain and the golden still passes, which is the precise
// failure this gate exists to catch. So AttributePromptCost calls the same
// composeSystemPromptWith* functions agent_input_context.go calls, in the same
// order, and attributes each step by the length it added.
//
// WHY THAT IS SAFE. The composers are pure by signature — value parameters, a
// string return, no I/O — and the file-reading lives in the RESOLVERS
// (canonical_context.go reads PROJECT_CONTEXT.md; resolveSkillIndex queries the
// skill store). L1 supplies resolved values from a pinned fixture instead of
// calling the resolvers, so nothing here reaches live state. Two laws in
// prompt_cost_test.go enforce both halves of that: composer purity, and a
// hydration path that reads only from the fixture root.

const (
	// promptCostFixtureRoot is the ONLY path this file may read. Law 2 in
	// prompt_cost_test.go asserts that, because a loader free to read elsewhere
	// is how live state reaches the golden without tripping the composer law.
	promptCostFixtureRoot = "testdata/promptcost"

	// promptCostHashFile pins each fixture's content. Editing a fixture without
	// updating its hash fails, because every golden underneath it moves too.
	promptCostHashFile = "testdata/promptcost/fixtures.sha256"
)

// PromptCostFixture is a fully-resolved composition input: everything the
// production chain would have fetched, supplied as values.
//
// Every field is a value type on purpose. There is no store, no client and no
// context here, so there is no way for this struct to carry a live handle.
type PromptCostFixture struct {
	// Role names the swarm role this prompt is composed for.
	Role string `json:"role"`
	// Swarm names the owning swarm. Part of the golden's identity: the same
	// role in two swarms may suppress different blocks.
	Swarm string `json:"swarm"`
	// RolePrompt is the role's own system prompt, before any block is appended.
	RolePrompt string `json:"rolePrompt"`
	// SuppressedGuidanceBlocks mirrors the swarm's suppressedGuidanceBlocks.
	// Advisory blocks named here are omitted; the invariant block is not,
	// whatever this says (see suppressesGuidanceBlock).
	SuppressedGuidanceBlocks []string `json:"suppressedGuidanceBlocks,omitempty"`
	// ToolGrantAvailable mirrors whether grant_step_tools is advertised, which
	// gates the tool-budget block.
	ToolGrantAvailable bool `json:"toolGrantAvailable"`
	// WorktreeGitReadOnly mirrors whether the project is mounted as a git
	// worktree, whose main .git the runtime mounts read-only — the condition
	// the workspace-git block describes.
	WorktreeGitReadOnly bool `json:"worktreeGitReadOnly"`
	// CanonicalContext is the resolved pre-load, supplied rather than read.
	CanonicalContext CanonicalContext `json:"canonicalContext"`
	// Skills is the resolved learned-skill index, supplied rather than queried.
	Skills []SkillIndexEntry `json:"skills,omitempty"`
}

// PromptCostSource is one attributed contributor, in composition order.
type PromptCostSource struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

// PromptCost is the attribution of one composed prompt.
//
// Sources are ordered as composed, and their Bytes sum exactly to TotalBytes —
// an unattributed byte is a byte nobody is accountable for, so the closure is
// asserted rather than assumed.
type PromptCost struct {
	Role       string             `json:"role"`
	Swarm      string             `json:"swarm,omitempty"`
	Sources    []PromptCostSource `json:"sources"`
	TotalBytes int                `json:"totalBytes"`
}

// Source returns the named contributor's attribution.
func (c PromptCost) Source(name string) (PromptCostSource, bool) {
	for _, s := range c.Sources {
		if s.Name == name {
			return s, true
		}
	}
	return PromptCostSource{}, false
}

// sourceRolePrompt names the role's own prompt in the attribution. The injected
// blocks use their promptblock names, so this one needs an identifier that
// cannot collide with a block an operator might try to suppress.
const sourceRolePrompt = "role-prompt"

// sourceSkillIndex names the learned-skill index. Deliberately absent from
// injectedBlocks (it is rendered per execution and has no fixed size to
// budget), but it is still a real cost and is attributed here.
const sourceSkillIndex = "skill-index"

// AttributePromptCost composes the fixture through the production chain and
// attributes each step by the bytes it added.
//
// The order below mirrors buildAgentContextMap exactly. It must: the
// attribution is a delta of prompt length at each step, so a reordering here
// that did not happen there would misattribute silently.
func AttributePromptCost(f PromptCostFixture) PromptCost {
	opts := &agentInputOpts{
		SystemPrompt:             f.RolePrompt,
		SuppressedGuidanceBlocks: f.SuppressedGuidanceBlocks,
		ToolGrantAvailable:       f.ToolGrantAvailable,
		WorktreeGitReadOnly:      f.WorktreeGitReadOnly,
	}
	suppressed := opts.suppressesGuidanceBlock

	cost := PromptCost{Role: f.Role, Swarm: f.Swarm}
	sp := f.RolePrompt
	if len(sp) > 0 {
		cost.Sources = append(cost.Sources, PromptCostSource{
			Name:  sourceRolePrompt,
			Bytes: len(sp),
		})
	}

	// add applies one composition step and attributes the growth. A step that
	// adds nothing contributes no source line, so a suppressed block is absent
	// from the attribution rather than present at zero — the difference matters
	// when reading a golden diff.
	add := func(name string, next string) {
		if grew := len(next) - len(sp); grew > 0 {
			cost.Sources = append(cost.Sources, PromptCostSource{Name: name, Bytes: grew})
		}
		sp = next
	}

	if !suppressed(promptblock.CanonicalContext) {
		add(promptblock.CanonicalContext, composeSystemPromptWithCanonicalContext(sp, f.CanonicalContext))
	}
	add(sourceSkillIndex, composeSystemPromptWithSkillIndex(sp, f.Skills))
	if !suppressed(promptblock.ToolBudget) {
		add(promptblock.ToolBudget, composeSystemPromptWithToolGrant(sp, f.ToolGrantAvailable))
	}
	// Unconditional, matching agent_input_context.go: the reporting-integrity
	// gate runs on every step of every deployment, so suppressing the block
	// would remove the warning and not the rule.
	add(promptblock.ReportingIntegrity, composeSystemPromptWithClaimVerification(sp))
	// Gated on the deployment fact, not on suppression — mirrors
	// agent_input_context.go. A non-worktree project adds nothing here, so the
	// attribution keeps summing to the composed prompt in both shapes.
	add(promptblock.WorkspaceGit, composeSystemPromptWithWorkspaceGit(sp, f.WorktreeGitReadOnly))

	cost.TotalBytes = len(sp)
	return cost
}

// --- fixture hydration (law 2 covers everything below) ----------------------

// FixtureNames lists the pinned fixtures, sorted so the golden run order is
// stable.
func FixtureNames() ([]string, error) {
	entries, err := os.ReadDir(promptCostFixtureRoot)
	if err != nil {
		return nil, fmt.Errorf("read fixture root: %w", err)
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".fixture.json") {
			continue
		}
		names = append(names, strings.TrimSuffix(n, ".fixture.json"))
	}
	sort.Strings(names)
	return names, nil
}

// LoadFixture reads one pinned fixture. The path is built from the fixture
// root and a bare name, so a name carrying separators cannot escape it.
func LoadFixture(name string) (PromptCostFixture, error) {
	if strings.ContainsAny(name, `/\.`) {
		return PromptCostFixture{}, fmt.Errorf("fixture name %q must be bare", name)
	}
	data, err := os.ReadFile(filepath.Join(promptCostFixtureRoot, name+".fixture.json"))
	if err != nil {
		return PromptCostFixture{}, fmt.Errorf("read fixture %q: %w", name, err)
	}
	var f PromptCostFixture
	if err := json.Unmarshal(data, &f); err != nil {
		return PromptCostFixture{}, fmt.Errorf("parse fixture %q: %w", name, err)
	}
	return f, nil
}

// HashFixture returns a fixture file's sha256, hex-encoded.
func HashFixture(name string) (string, error) {
	if strings.ContainsAny(name, `/\.`) {
		return "", fmt.Errorf("fixture name %q must be bare", name)
	}
	data, err := os.ReadFile(filepath.Join(promptCostFixtureRoot, name+".fixture.json"))
	if err != nil {
		return "", fmt.Errorf("read fixture %q: %w", name, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// LoadFixtureHashes reads the pin file: one "<sha256>  <name>" line per
// fixture, mirroring sha256sum's output so it can be regenerated with it.
func LoadFixtureHashes() (map[string]string, error) {
	data, err := os.ReadFile(promptCostHashFile)
	if err != nil {
		return nil, fmt.Errorf("read hash pin: %w", err)
	}
	pinned := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("malformed pin line %q: want '<sha256>  <name>'", line)
		}
		pinned[strings.TrimSuffix(fields[1], ".fixture.json")] = fields[0]
	}
	return pinned, nil
}
