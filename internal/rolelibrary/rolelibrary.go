// Package rolelibrary implements the curated role-archetype library
// that grounds the NL automation composer's tier-3 synthesis (see
// https://docs.vornik.io §5.3).
//
// The composer never mints a role from nothing: it SELECTS and
// PARAMETERISES archetypes drawn from this library. Each archetype
// declares the MAXIMUM tool allowlist a composed role may carry —
// composed roles may subset it, never exceed it — which is why the
// library is a security asset, not a tuning knob, and ships with a
// mandatory doctor check (see doctor.go).
//
// File format (configs/role-library/*.md) mirrors the SWARM.md
// convention (registry/swarm_md.go): a `---`-delimited YAML
// frontmatter block carrying the structured metadata, followed by a
// Markdown body that IS the role's system prompt. Unlike SWARM.md,
// each file holds exactly ONE archetype and the whole body is the
// prompt (no `## Role prompts` / `### <role>` sub-sectioning) —
// distinct schema, distinct type (RoleArchetype, NOT SwarmRole).
package rolelibrary

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// LibraryDirName is the canonical sub-directory (under the configs
// root) that holds the role-archetype Markdown files. Public-repo
// placement per design §11 Q4.
const LibraryDirName = "role-library"

// Valid modelTier values. The composer assigns a tier; the server
// resolves tier → concrete model using the operator's routing
// preferences (design §5.3), so model policy stays server-owned.
const (
	ModelTierTrivial  = "trivial"
	ModelTierStandard = "standard"
	ModelTierComplex  = "complex"
)

// ArchetypeRuntime mirrors the subset of a role's runtime the library
// pins: container sizing + the per-call token ceiling. Values are the
// same string/int shapes the swarm runtime uses so a composed role can
// carry them through unchanged.
type ArchetypeRuntime struct {
	CPU       string `yaml:"cpu" json:"cpu"`
	Memory    string `yaml:"memory" json:"memory"`
	MaxTokens int    `yaml:"maxTokens" json:"maxTokens"`
}

// RoleArchetype is one curated role template. The frontmatter fields
// are the contract the library doctor check (§5.3) validates; the
// Prompt is the Markdown body, a Go text/template with `{{.param}}`
// splice points limited to the declared PromptParams.
type RoleArchetype struct {
	// ArchetypeID is a unique slug ("researcher", "writer", …).
	ArchetypeID string `yaml:"archetypeId" json:"archetypeId"`
	// DisplayName is the human-facing label.
	DisplayName string `yaml:"displayName" json:"displayName"`
	// Description is a one-line summary shown in the composer grounding.
	Description string `yaml:"description" json:"description"`
	// Tools is the MAXIMUM allowlist: composed roles may subset it,
	// never exceed it. Growing this list is a security-review-worthy
	// change (the doctor check flags broad lists loudly).
	Tools []string `yaml:"tools" json:"tools"`
	// RequiredOutputKeys names the top-level keys the role's structured
	// output MUST carry. Must be non-empty strings.
	RequiredOutputKeys []string `yaml:"requiredOutputKeys" json:"requiredOutputKeys"`
	// Runtime pins container sizing + token ceiling.
	Runtime ArchetypeRuntime `yaml:"runtime" json:"runtime"`
	// ModelTier is trivial|standard|complex.
	ModelTier string `yaml:"modelTier" json:"modelTier"`
	// PromptParams names the splice points the Prompt body may use.
	PromptParams []string `yaml:"promptParams" json:"promptParams"`

	// Prompt is the Markdown body (the system prompt). Not a
	// frontmatter field — populated from the file body by the parser.
	Prompt string `yaml:"-" json:"prompt"`

	// SourceFile is the file this archetype was parsed from, for
	// doctor findings + error messages. Not persisted.
	SourceFile string `yaml:"-" json:"-"`
}

const frontmatterMarker = "---"

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// ParseArchetype decodes one role-library Markdown file into a
// RoleArchetype. Contract (cribbed from registry.ParseSwarmMarkdown):
//   - File starts with `---` frontmatter (BOM / leading whitespace
//     tolerated).
//   - Frontmatter closes with `---` on its own line.
//   - Frontmatter yaml.Unmarshals into the archetype metadata.
//   - Everything after the closing marker is the prompt body.
//
// Structural validity (tools known, splice points declared, …) is NOT
// checked here — that is the doctor check's job (CheckLibrary). Parse
// only fails on malformed frontmatter so a broken file surfaces at
// load, and the doctor check can still report on a parseable-but-
// invalid archetype.
func ParseArchetype(content []byte, filename string) (*RoleArchetype, error) {
	fm, body, err := splitFrontmatter(content, filename)
	if err != nil {
		return nil, err
	}
	var a RoleArchetype
	if err := yaml.Unmarshal(fm, &a); err != nil {
		return nil, fmt.Errorf("role-library %s: yaml frontmatter parse: %w", filename, err)
	}
	a.Prompt = strings.TrimSpace(string(body))
	a.SourceFile = filename
	return &a, nil
}

// splitFrontmatter peels the leading `---`-delimited block off the
// file, returning the YAML bytes (without markers) and the body.
// Tolerates a UTF-8 BOM and leading whitespace before the opener.
func splitFrontmatter(content []byte, filename string) (frontmatter, body []byte, err error) {
	content = bytes.TrimPrefix(content, utf8BOM)
	trimmed := bytes.TrimLeft(content, " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte(frontmatterMarker)) {
		return nil, nil, fmt.Errorf("role-library %s: missing opening frontmatter marker (file must start with '---')", filename)
	}
	rest := bytes.TrimPrefix(trimmed, []byte(frontmatterMarker))
	if len(rest) == 0 || (rest[0] != '\n' && rest[0] != '\r') {
		return nil, nil, fmt.Errorf("role-library %s: opening frontmatter marker must be on its own line", filename)
	}
	// Find the closing marker: a line that is exactly "---".
	lines := bytes.Split(rest, []byte("\n"))
	var fmBuf bytes.Buffer
	closed := false
	var bodyLines [][]byte
	for i, line := range lines {
		if i == 0 {
			// `rest` began right after the opening "---"; the first
			// element is the remainder of that opening line (usually
			// empty). Skip it.
			continue
		}
		if !closed && strings.TrimRight(string(line), "\r") == frontmatterMarker {
			closed = true
			continue
		}
		if closed {
			bodyLines = append(bodyLines, line)
		} else {
			fmBuf.Write(line)
			fmBuf.WriteByte('\n')
		}
	}
	if !closed {
		return nil, nil, fmt.Errorf("role-library %s: missing closing frontmatter marker ('---')", filename)
	}
	return fmBuf.Bytes(), bytes.Join(bodyLines, []byte("\n")), nil
}

// Load reads every *.md file in <configsDir>/role-library into a
// slice of archetypes, sorted by ArchetypeID for stable output. A
// missing directory returns an empty slice + nil error (the composer
// feature-doctor prereq reports "no entries" separately). Parse
// errors abort the load so a malformed file can't be silently
// skipped.
func Load(configsDir string) ([]*RoleArchetype, error) {
	dir := filepath.Join(configsDir, LibraryDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read role-library dir: %w", err)
	}
	var out []*RoleArchetype
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if strings.EqualFold(e.Name(), "README.md") {
			// The library's own security-model documentation, not an
			// archetype — see configs/role-library/README.md. Skipped
			// here (not just "not an archetype") so it can live
			// alongside the *.md archetypes without frontmatter.
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read role-library file %s: %w", e.Name(), err)
		}
		a, err := ParseArchetype(data, e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArchetypeID < out[j].ArchetypeID })
	return out, nil
}
