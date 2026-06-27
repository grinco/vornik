package projectwizard

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/registry"
)

// TemplateSource is the narrow seam the wizard uses to anchor a
// committed proposal on a vetted project template — the same catalog
// the /ui/projects/new gallery renders. The wizard's LLM proposal
// supplies *parameters* (projectId, displayName, topic); the
// template supplies the file structure (project.yaml + swarm.md +
// any others). A committed wizard project is therefore guaranteed to
// load and run exactly like a gallery one, instead of depending on
// the LLM to hand-author a valid swarmId / defaultWorkflowId / swarm
// file — which it never did, so every proposal failed registry
// validation on "swarmId is required" and ready_to_commit could
// never flip.
//
// Production wires an adapter over templates.Catalog; tests inject a
// fake.
type TemplateSource interface {
	// Lookup returns the template's spec (declared parameters) and
	// whether the slug exists in the catalog.
	Lookup(slug string) (TemplateSpec, bool)
	// Materialise validates params against the manifest and renders
	// every template file, keyed by target path (relative to the
	// daemon configs root). Mirrors templates.Catalog.MaterialiseFiles
	// — including filling declared Defaults for omitted parameters.
	Materialise(slug string, params map[string]string) (map[string]string, error)
}

// TemplateSpec is the catalog metadata the wizard needs to derive a
// parameter map from an LLM proposal without coupling projectwizard
// to the templates package's concrete types.
type TemplateSpec struct {
	Slug   string
	Params []TemplateParamSpec
}

// TemplateParamSpec mirrors the subset of templates.Parameter the
// wizard needs for proposal→params derivation: the lookup name, the
// type (so enum values can be screened), and the closed option set
// for enums.
type TemplateParamSpec struct {
	Name    string
	Type    string
	Options []string
}

// deriveTemplateParams maps an LLM proposal's raw map onto the
// template's declared parameters. For each declared parameter that
// has a scalar value under the same key in the proposal, the value
// is carried through; everything else is omitted so the template's
// own ValidateParams fills the declared Default. Enum values that
// aren't in the declared option set are dropped (the default wins)
// so an LLM that invents an out-of-range value can't make an
// otherwise-valid proposal fail to materialise.
func deriveTemplateParams(spec TemplateSpec, raw map[string]any) map[string]string {
	params := make(map[string]string, len(spec.Params))
	for _, p := range spec.Params {
		v, ok := raw[p.Name]
		if !ok {
			continue
		}
		s := coerceScalar(v)
		if s == "" {
			continue
		}
		if strings.EqualFold(p.Type, "enum") && !optionAllowed(s, p.Options) {
			continue // out-of-range LLM value — let the template default win
		}
		params[p.Name] = s
	}
	return params
}

func optionAllowed(v string, options []string) bool {
	for _, o := range options {
		if o == v {
			return true
		}
	}
	return false
}

// coerceScalar renders a JSON-decoded scalar as the string the
// template engine expects. Non-scalar values (maps, slices) return
// "" so they're skipped — a template parameter is always a scalar.
// Numbers arrive as float64 because the proposal is decoded via
// encoding/json into map[string]any.
func coerceScalar(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return ""
	}
}

// validateRenderedProject finds the rendered project YAML in a
// materialised file set and runs it through the registry validator,
// so the wizard only flips ready_to_commit when the template would
// actually load. The project file is the target under "projects/"
// ending ".yaml" (the catalog's FileMap target convention).
func validateRenderedProject(files map[string]string) error {
	body, ok := projectYAMLBody(files)
	if !ok {
		return errors.New("template produced no project YAML")
	}
	var project registry.Project
	if err := yaml.Unmarshal([]byte(body), &project); err != nil {
		return fmt.Errorf("rendered project YAML parse: %w", err)
	}
	if err := project.Validate("wizard-proposal.yaml"); err != nil {
		return err
	}
	return nil
}

// projectYAMLBody returns the rendered project-file body from a
// materialised file set (the target under projects/ ending .yaml).
func projectYAMLBody(files map[string]string) (string, bool) {
	for target, body := range files {
		if isProjectTarget(target) {
			return body, true
		}
	}
	return "", false
}

func isProjectTarget(target string) bool {
	t := strings.ReplaceAll(target, "\\", "/")
	return strings.HasPrefix(t, "projects/") && strings.HasSuffix(t, ".yaml")
}
