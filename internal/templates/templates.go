// Package templates implements the project-template gallery — the
// 2026.6.0 "no more empty YAML cliff" mitigation. A template is a
// per-slug directory containing a manifest plus one or more files
// to materialise via text/template substitution. The CLI's
// `vornikctl init project --template <slug>` and the web UI's
// /ui/projects/new gallery both call into this package.
//
// Layout convention:
//
//	configs/project-templates/<slug>/template.yaml
//	configs/project-templates/<slug>/<file>.tmpl
//	configs/project-templates/<slug>/<another>.tmpl
//
// Each `template.yaml` declares the operator-facing display name,
// the parameter form (typed inputs with patterns and defaults),
// and the file map (template files → output paths).
package templates

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/safepath"
)

// Manifest is the on-disk shape of a template.yaml. Operators
// author one per template slug; the loader parses every file in
// the templates root at startup so the UI can render the gallery
// without per-request disk reads.
type Manifest struct {
	// Slug is the directory name; populated by the loader.
	Slug string `yaml:"-"`

	// DisplayName surfaces in the gallery cards and CLI listings.
	DisplayName string `yaml:"displayName"`

	// Description is operator-facing prose. Markdown allowed.
	Description string `yaml:"description"`

	// Domain groups templates in the gallery (general, research,
	// code, trading). Empty defaults to "general". The gallery UI
	// renders a tab strip filtering on this field so the catalog
	// stays browsable as it grows past a handful of entries.
	Domain string `yaml:"domain,omitempty"`

	// Screenshot is an optional path relative to the template dir
	// for a preview image. Unused when empty; the gallery
	// renders a default placeholder.
	Screenshot string `yaml:"screenshot,omitempty"`

	// Parameters defines the form the user fills out before
	// materialisation. The order in this slice is preserved as
	// the rendering order in the UI form.
	Parameters []Parameter `yaml:"parameters"`

	// Files lists every file the materialiser writes when a user
	// applies this template. Paths in both `source` (relative to
	// the template dir) and `target` (relative to the daemon's
	// configs/ root) are substituted via text/template using the
	// collected parameters as context. Order doesn't matter
	// functionally but is preserved for deterministic output.
	Files []FileMap `yaml:"files"`
}

// Parameter declares one operator-facing form field.
type Parameter struct {
	// Name is the lookup key in templates (e.g. {{.projectId}}).
	Name string `yaml:"name"`

	// Type drives both form rendering and server-side validation.
	// Recognised: "string" | "enum" | "bool". Unknown values
	// default to "string".
	Type string `yaml:"type"`

	// Label is the human-readable form-field label.
	Label string `yaml:"label"`

	// Description (optional) is a hint shown under the input.
	Description string `yaml:"description,omitempty"`

	// Default is the pre-filled value. Validated against
	// Pattern/Options like any user-supplied value.
	Default string `yaml:"default,omitempty"`

	// Pattern (string types only) is a regex the input must
	// match. Anchors are auto-added — the test is FullMatch.
	Pattern string `yaml:"pattern,omitempty"`

	// Options (enum types only) is the closed set of values
	// accepted. The form renders as a select.
	Options []string `yaml:"options,omitempty"`

	// Required, when true, refuses an empty value at validation
	// time. String/enum parameters set this true when they have
	// no Default; bool parameters ignore it (their zero value
	// false is always valid).
	Required bool `yaml:"required,omitempty"`
}

// FileMap is one (source template, target output) pair.
type FileMap struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

// Catalog is an immutable view of the loaded templates. Built by
// Load() at startup; consumed read-only by the CLI/API/UI.
type Catalog struct {
	// dir is the templates root (kept so callers can resolve
	// per-file source paths without re-deriving).
	dir string

	// manifests indexed by slug for O(1) lookup. Slug order is
	// stabilised in List() via a sort, so callers iterate in a
	// deterministic order.
	manifests map[string]Manifest
}

// Load walks the templates root and parses every immediate
// subdirectory that contains a template.yaml. Subdirectories
// without a manifest are skipped silently — a developer-in-progress
// can leave a half-built template on disk without breaking the
// daemon. Malformed manifests return an error that names the slug
// so the operator can fix the offending file. Missing root
// directory is NOT an error — Load returns an empty Catalog so
// the daemon runs in deployments without templates installed.
func Load(dir string) (*Catalog, error) {
	c := &Catalog{dir: dir, manifests: map[string]Manifest{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return nil, fmt.Errorf("templates: read root %s: %w", dir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		slug := entry.Name()
		manifestPath := filepath.Join(dir, slug, "template.yaml")
		raw, rerr := os.ReadFile(manifestPath)
		if rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("templates: read manifest %s: %w", manifestPath, rerr)
		}
		var m Manifest
		if uerr := yaml.Unmarshal(raw, &m); uerr != nil {
			return nil, fmt.Errorf("templates: parse manifest for slug %q: %w", slug, uerr)
		}
		m.Slug = slug
		if m.Domain == "" {
			m.Domain = "general"
		}
		if verr := validateManifest(m); verr != nil {
			return nil, fmt.Errorf("templates: manifest %q is invalid: %w", slug, verr)
		}
		c.manifests[slug] = m
	}
	return c, nil
}

// List returns every loaded manifest in (domain, displayName)
// order — the natural gallery layout grouping for the UI. Returns
// a fresh slice on every call so the caller can sort/filter
// without mutating the catalog's internal state.
func (c *Catalog) List() []Manifest {
	if c == nil {
		return nil
	}
	out := make([]Manifest, 0, len(c.manifests))
	for _, m := range c.manifests {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Domain != out[j].Domain {
			return out[i].Domain < out[j].Domain
		}
		return out[i].DisplayName < out[j].DisplayName
	})
	return out
}

// Domains returns the distinct domain values across the loaded
// catalog, sorted alphabetically. Powers the gallery's tab strip;
// callers that filter by domain pass one of these back to List
// (via the higher-level UI) or use Get on individual slugs.
func (c *Catalog) Domains() []string {
	if c == nil {
		return nil
	}
	seen := make(map[string]struct{}, 4)
	for _, m := range c.manifests {
		seen[m.Domain] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// Get returns the manifest for a slug. Second return is false when
// the slug doesn't exist; callers must check it before passing the
// manifest to MaterialiseFiles.
func (c *Catalog) Get(slug string) (Manifest, bool) {
	if c == nil {
		return Manifest{}, false
	}
	m, ok := c.manifests[slug]
	return m, ok
}

// MaterialiseFiles renders every Files entry of `m` using `params`
// as the text/template context and returns the rendered output as
// a map keyed by the target path. The caller writes them to disk —
// MaterialiseFiles intentionally doesn't touch the filesystem so
// it's easy to dry-run (CLI --dry-run, API preview endpoints).
//
// Parameter values are validated against the manifest's Parameters
// declaration FIRST; an unknown parameter, a missing required
// value, a regex-pattern miss, or an enum out-of-list value all
// return a structured error (see ValidationError) so the caller
// can route them to a 400 response.
func (c *Catalog) MaterialiseFiles(m Manifest, params map[string]string) (map[string]string, error) {
	cleaned, err := ValidateParams(m, params)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(m.Files))
	for _, fm := range m.Files {
		// Render the target path the same way as the file body —
		// both go through text/template against `cleaned`. Allows
		// patterns like target: "configs/projects/{{.projectId}}.yaml".
		target, terr := renderInline(fm.Target, cleaned)
		if terr != nil {
			return nil, fmt.Errorf("render target %q: %w", fm.Target, terr)
		}
		if err := validateRelativeTarget(target); err != nil {
			return nil, fmt.Errorf("rendered target %q refused: %w", target, err)
		}
		sourcePath, serr := safepath.JoinUnder(filepath.Join(c.dir, m.Slug), fm.Source)
		if serr != nil {
			return nil, fmt.Errorf("source template %q refused: %w", fm.Source, serr)
		}
		raw, rerr := os.ReadFile(sourcePath)
		if rerr != nil {
			return nil, fmt.Errorf("read template source %q: %w", sourcePath, rerr)
		}
		body, berr := renderInline(string(raw), cleaned)
		if berr != nil {
			return nil, fmt.Errorf("render body %q: %w", sourcePath, berr)
		}
		out[target] = body
	}
	return out, nil
}

// SortedTargets returns rendered file targets in deterministic
// lexical order. MaterialiseFiles returns a map for convenient
// target->body lookup; callers that present or write the files use
// this helper so API responses, CLI dry-runs, and UI success views
// do not depend on Go's randomized map iteration order.
func SortedTargets(rendered map[string]string) []string {
	out := make([]string, 0, len(rendered))
	for target := range rendered {
		out = append(out, target)
	}
	sort.Strings(out)
	return out
}

// ValidationError carries the first parameter problem the
// validator hit. Callers route it to a 400 with the Field +
// Message text — both are operator-facing.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return fmt.Sprintf("parameter %q: %s", e.Field, e.Message)
}

// ValidateParams checks user-supplied values against the manifest
// and returns a clean map suitable for text/template. Missing
// optional parameters are filled with their declared Default;
// missing required parameters return an error.
func ValidateParams(m Manifest, params map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(m.Parameters))
	declared := make(map[string]struct{}, len(m.Parameters))
	for _, p := range m.Parameters {
		declared[p.Name] = struct{}{}
		raw, ok := params[p.Name]
		raw = strings.TrimSpace(raw)
		if !ok || raw == "" {
			// Required parameters without a Default refuse.
			if p.Required && p.Default == "" {
				return nil, &ValidationError{Field: p.Name, Message: "required"}
			}
			raw = p.Default
		}
		// Type-specific checks.
		switch strings.ToLower(p.Type) {
		case "enum":
			if len(p.Options) == 0 {
				return nil, &ValidationError{Field: p.Name, Message: "enum parameter has no options declared in manifest"}
			}
			match := false
			for _, opt := range p.Options {
				if raw == opt {
					match = true
					break
				}
			}
			if !match {
				return nil, &ValidationError{Field: p.Name, Message: fmt.Sprintf("must be one of %v", p.Options)}
			}
		case "bool":
			lower := strings.ToLower(raw)
			if lower != "true" && lower != "false" && lower != "" {
				return nil, &ValidationError{Field: p.Name, Message: "must be 'true' or 'false'"}
			}
		default: // string
			if p.Pattern != "" {
				re, rerr := regexp.Compile("^(?:" + p.Pattern + ")$")
				if rerr != nil {
					return nil, &ValidationError{Field: p.Name, Message: fmt.Sprintf("manifest pattern is not a valid regex: %v", rerr)}
				}
				if !re.MatchString(raw) {
					return nil, &ValidationError{Field: p.Name, Message: fmt.Sprintf("must match pattern %s", p.Pattern)}
				}
			}
		}
		out[p.Name] = raw
	}
	// Refuse parameters the manifest doesn't know — defensive
	// against a malicious or stale client.
	for key := range params {
		if _, ok := declared[key]; !ok {
			return nil, &ValidationError{Field: key, Message: "unknown parameter for this template"}
		}
	}
	return out, nil
}

// renderInline runs `text/template` once with `params` as context.
// Centralised here so the source-path and source-body renders use
// the same delimiters + missing-key policy.
func renderInline(text string, params map[string]string) (string, error) {
	t, err := template.New("inline").Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, params); err != nil {
		return "", err
	}
	return b.String(), nil
}

// validateManifest catches the manifest-level invariants the
// loader can check without consulting any external state. Loaded
// at startup; a bad manifest fails the daemon's Load so operators
// see the error immediately rather than at first /ui/projects/new
// click.
func validateManifest(m Manifest) error {
	if m.DisplayName == "" {
		return errors.New("displayName is required")
	}
	if len(m.Files) == 0 {
		return errors.New("at least one files[] entry is required")
	}
	names := make(map[string]struct{}, len(m.Parameters))
	for _, p := range m.Parameters {
		if p.Name == "" {
			return errors.New("parameters[].name is required")
		}
		if _, dup := names[p.Name]; dup {
			return fmt.Errorf("duplicate parameter name %q", p.Name)
		}
		names[p.Name] = struct{}{}
		switch strings.ToLower(p.Type) {
		case "", "string", "enum", "bool":
		default:
			return fmt.Errorf("parameter %q: unknown type %q (expected string|enum|bool)", p.Name, p.Type)
		}
		if strings.EqualFold(p.Type, "enum") && len(p.Options) == 0 {
			return fmt.Errorf("parameter %q: enum type requires non-empty options", p.Name)
		}
	}
	for _, fm := range m.Files {
		if fm.Source == "" || fm.Target == "" {
			return errors.New("files[] entries require both source and target")
		}
		if strings.Contains(fm.Source, "..") || strings.HasPrefix(fm.Source, "/") {
			return fmt.Errorf("files[].source %q must be a relative path without parent-dir traversal", fm.Source)
		}
		if err := validateRelativeTarget(fm.Target); err != nil {
			return fmt.Errorf("files[].target %q must be a relative path inside configs: %w", fm.Target, err)
		}
	}
	return nil
}

func validateRelativeTarget(target string) error {
	target = strings.TrimSpace(target)
	if target == "" || target == "." {
		return errors.New("empty target")
	}
	if filepath.IsAbs(target) || strings.HasPrefix(target, "/") || strings.HasPrefix(target, `\`) {
		return errors.New("absolute paths are not allowed")
	}
	clean := filepath.Clean(target)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) ||
		strings.Contains(clean, string(filepath.Separator)+".."+string(filepath.Separator)) ||
		strings.HasSuffix(clean, string(filepath.Separator)+"..") {
		return errors.New("parent-dir traversal is not allowed")
	}
	if strings.Contains(target, "..") {
		return errors.New("parent-dir traversal is not allowed")
	}
	return nil
}
