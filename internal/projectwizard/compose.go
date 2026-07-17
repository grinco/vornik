package projectwizard

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/templates"
)

// Addon is one typed composition mutation the wizard's LLM proposes.
// Type selects the applier; Args is the verbatim JSON object (including
// type) so each applier decodes its own fields. See
// https://docs.vornik.io §3b.
type Addon struct {
	Type string
	Args json.RawMessage
}

// UnmarshalJSON captures the discriminator (type) and keeps the whole
// object as Args for the per-applier decode.
func (a *Addon) UnmarshalJSON(b []byte) error {
	var disc struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &disc); err != nil {
		return err
	}
	a.Type = disc.Type
	a.Args = append(a.Args[:0], b...)
	return nil
}

// MarshalJSON round-trips Args verbatim — it already holds the
// complete addon object (captured by UnmarshalJSON, including
// "type"), so re-marshaling must reproduce that object rather than
// re-deriving one from Type alone, which would silently drop every
// other field (interval, goal, name, …).
//
// This matters because Composition — and therefore its Addons — gets
// marshaled twice on the wizard v2 path: once by Converse to persist
// session.Composition, and (bug found while wiring Commit to consume
// it, T6) a second time would be needed if Commit re-derived Args.
// Without this method, the default struct marshaler emits
// {"Type":...,"Args":{...}} (capitalised Go field names, Args
// double-nested), and decoding that back via UnmarshalJSON's
// case-insensitive "type" match recovers the discriminator but
// stores the *whole* mis-shapen object as Args — so an applier like
// scheduleApplier looks for "interval" at the wrong nesting level and
// fails with a bogus "not a positive Go duration".
func (a Addon) MarshalJSON() ([]byte, error) {
	if len(a.Args) > 0 {
		return a.Args, nil
	}
	return json.Marshal(struct {
		Type string `json:"type"`
	}{Type: a.Type})
}

// DeclaredSecret is a secret the composition says the project needs.
// It becomes a doctor to-do (never blocks commit).
type DeclaredSecret struct {
	Name  string
	Label string
}

// composedProject is the mutable unit threaded through the appliers
// after template materialisation and before validation.
type composedProject struct {
	Project *registry.Project
	Swarm   *registry.Swarm
	Secrets []DeclaredSecret
}

// AddonApplier mutates cp in place. A returned error (ideally a
// *ComposeError) names the offending field and aborts the pipeline.
type AddonApplier interface {
	Apply(cp *composedProject, args json.RawMessage) error
}

// MultiMaterialiser renders a template's files with multi-value params.
// Satisfied in production by catalogTemplateSource (over
// templates.Catalog.MaterialiseFilesMulti).
type MultiMaterialiser interface {
	MaterialiseMulti(slug string, params map[string][]string, resolver templates.OptionsResolver) (map[string]string, error)
}

// ComposeDeps carries the engine's collaborators.
type ComposeDeps struct {
	Templates MultiMaterialiser
	Resolver  templates.OptionsResolver
	// KnownMCP is the set of MCP server names configured on the daemon
	// (from the MCP registry snapshot). The mcp_server applier rejects
	// any server not in this set.
	KnownMCP map[string]bool
	// TemplateMeta returns a base template's declared params and its
	// declared autonomy block, read from the template files. Consumed by
	// normalizeComposition for param repair (§4.2) and base-autonomy
	// reconciliation (§3.2). Nil is tolerated: normalization then skips
	// base-aware behavior and derives only projectId.
	TemplateMeta TemplateMetaLookup
}

// TemplateMetaLookup resolves a base template slug to its declared params
// and autonomy block. ok=false for an unknown slug.
type TemplateMetaLookup func(slug string) (params []TemplateParam, base BaseAutonomy, ok bool)

// ComposeError is a structured composition failure. AddonIndex/AddonType
// are set for applier failures; AddonIndex = -1 for
// materialise/parse/validate failures.
type ComposeError struct {
	AddonIndex int
	AddonType  string
	Field      string
	Message    string
}

func (e *ComposeError) Error() string {
	if e.AddonIndex < 0 {
		if e.Field != "" {
			return fmt.Sprintf("composition: %s: %s", e.Field, e.Message)
		}
		return "composition: " + e.Message
	}
	return fmt.Sprintf("addon[%d] %s: field %q: %s", e.AddonIndex, e.AddonType, e.Field, e.Message)
}

// ComposeInput is one composition request: a base template, its
// multi-value params, and the ordered addons to apply.
type ComposeInput struct {
	TemplateSlug string
	Params       map[string][]string
	Addons       []Addon
}

// Compose materialises the base template, parses the project + swarm,
// applies the addons in order, then re-serialises and cross-validates
// the whole (registry validation of the project + a swarm
// marshal→parse→Validate round-trip). It returns the final file set
// ready to write (project YAML + swarm markdown, with any other
// template files passed through untouched) plus the declared secrets.
// A *ComposeError names the failing addon or stage.
func Compose(in ComposeInput, deps ComposeDeps) (map[string]string, []DeclaredSecret, error) {
	if deps.Templates == nil {
		return nil, nil, &ComposeError{AddonIndex: -1, Message: "no template source"}
	}
	files, err := deps.Templates.MaterialiseMulti(in.TemplateSlug, in.Params, deps.Resolver)
	if err != nil {
		return nil, nil, &ComposeError{AddonIndex: -1, Field: "materialise", Message: err.Error()}
	}
	// A template that emits more than one candidate project YAML or swarm
	// markdown makes findBySuffix's pick nondeterministic (map iteration
	// order). Fail clearly instead of arbitrarily selecting one.
	if countBySuffix(files, "projects/", ".yaml") > 1 || countBySuffix(files, "swarms/", ".md") > 1 {
		return nil, nil, &ComposeError{AddonIndex: -1, Field: "template",
			Message: "template emitted multiple project (or swarm) files; expected exactly one project YAML and at most one swarm markdown"}
	}
	projTarget, projBody, ok := findBySuffix(files, "projects/", ".yaml")
	if !ok {
		return nil, nil, &ComposeError{AddonIndex: -1, Message: "template produced no project YAML"}
	}
	var project registry.Project
	if err := yaml.Unmarshal([]byte(projBody), &project); err != nil {
		return nil, nil, &ComposeError{AddonIndex: -1, Field: "project", Message: "parse: " + err.Error()}
	}
	cp := &composedProject{Project: &project}

	swTarget, swBody, hasSwarm := findBySuffix(files, "swarms/", ".md")
	if hasSwarm {
		sw, perr := registry.ParseSwarmMarkdown([]byte(swBody), swTarget)
		if perr != nil {
			return nil, nil, &ComposeError{AddonIndex: -1, Field: "swarm", Message: "parse: " + perr.Error()}
		}
		cp.Swarm = sw
	}

	reg := newApplierRegistry(deps)
	for i, addon := range in.Addons {
		applier, known := reg[addon.Type]
		if !known {
			return nil, nil, &ComposeError{AddonIndex: i, AddonType: addon.Type, Field: "type",
				Message: "unknown addon type"}
		}
		if err := applier.Apply(cp, addon.Args); err != nil {
			// Stamp the index onto a ComposeError; wrap others.
			var ce *ComposeError
			if errors.As(err, &ce) {
				ce.AddonIndex = i
				if ce.AddonType == "" {
					ce.AddonType = addon.Type
				}
				return nil, nil, ce
			}
			return nil, nil, &ComposeError{AddonIndex: i, AddonType: addon.Type, Message: err.Error()}
		}
	}

	// Re-serialise the mutated project + swarm back into the file set.
	outProj, err := yaml.Marshal(cp.Project)
	if err != nil {
		return nil, nil, &ComposeError{AddonIndex: -1, Field: "project", Message: "re-marshal: " + err.Error()}
	}
	files[projTarget] = string(outProj)
	if cp.Swarm != nil {
		outSwarm, merr := registry.MarshalSwarmMarkdown(cp.Swarm)
		if merr != nil {
			return nil, nil, &ComposeError{AddonIndex: -1, Field: "swarm", Message: "re-marshal: " + merr.Error()}
		}
		files[swTarget] = string(outSwarm)
	}

	// Cross-cutting validation of the composed whole.
	if verr := cp.Project.Validate(projTarget); verr != nil {
		return nil, nil, &ComposeError{AddonIndex: -1, Field: "project", Message: "validation: " + verr.Error()}
	}
	if cp.Swarm != nil {
		// Round-trip the re-serialised swarm to catch a mutation that
		// produced un-parseable markdown, then validate.
		reparsed, rerr := registry.ParseSwarmMarkdown([]byte(files[swTarget]), swTarget)
		if rerr != nil {
			return nil, nil, &ComposeError{AddonIndex: -1, Field: "swarm", Message: "round-trip parse: " + rerr.Error()}
		}
		if verr := reparsed.Validate(swTarget); verr != nil {
			return nil, nil, &ComposeError{AddonIndex: -1, Field: "swarm", Message: "validation: " + verr.Error()}
		}
	}
	return files, cp.Secrets, nil
}

// findBySuffix returns the first file whose target has the given prefix
// and suffix (path-separator normalised). Callers that must reject
// multiple matches deterministically should check countBySuffix first.
func findBySuffix(files map[string]string, prefix, suffix string) (target, body string, ok bool) {
	for t, b := range files {
		norm := filepath.ToSlash(t)
		if strings.HasPrefix(norm, prefix) && strings.HasSuffix(norm, suffix) {
			return t, b, true
		}
	}
	return "", "", false
}

// countBySuffix counts files whose target has the given prefix and
// suffix (path-separator normalised).
func countBySuffix(files map[string]string, prefix, suffix string) int {
	n := 0
	for t := range files {
		norm := filepath.ToSlash(t)
		if strings.HasPrefix(norm, prefix) && strings.HasSuffix(norm, suffix) {
			n++
		}
	}
	return n
}

// newApplierRegistry builds the six appliers keyed by addon type,
// injecting deps where an applier needs them (mcp_server needs KnownMCP).
func newApplierRegistry(deps ComposeDeps) map[string]AddonApplier {
	return map[string]AddonApplier{
		"mcp_server": mcpServerApplier{known: deps.KnownMCP},
		// autonomy: normalizer-emitted only (mergeAutonomyAddons); NOT in
		// the LLM addon vocabulary/grounding.
		"autonomy":           autonomyApplier{},
		"schedule":           scheduleApplier{},
		"rag_source":         ragSourceApplier{},
		"chat_tools":         chatToolsApplier{},
		"role_prompt_append": rolePromptAppendApplier{},
		"secret_requirement": secretRequirementApplier{},
	}
}
