package projectwizard

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/registry"
)

// RegistryValidator runs a proposal through the production
// project-YAML parser + validator. This is the Phase B replacement
// for the Phase A permissive validator; once wired, the wizard
// refuses to mark a proposal ready_to_commit unless it would
// actually load into the daemon's project registry.
//
// The proposal map is marshalled to YAML and unmarshalled into a
// registry.Project so the operator sees the same errors they'd
// see editing the file by hand.
type RegistryValidator struct{}

// Validate marshals p.Raw to YAML, parses it into a
// registry.Project, then calls the registry's own Validate method.
// Any error along the way surfaces back to the operator.
func (RegistryValidator) Validate(p *ProjectYAML) error {
	if p == nil || len(p.Raw) == 0 {
		return errors.New("proposal is empty")
	}
	body, err := yaml.Marshal(p.Raw)
	if err != nil {
		return fmt.Errorf("yaml marshal: %w", err)
	}
	var project registry.Project
	if err := yaml.Unmarshal(body, &project); err != nil {
		return fmt.Errorf("yaml parse: %w", err)
	}
	if err := project.Validate("wizard-proposal.yaml"); err != nil {
		return err
	}
	return nil
}

// RenderYAML returns the proposal serialised as YAML bytes, ready
// for writing to disk via the existing project-ingestion path or
// rendering in the preview pane. Returns an error only when the
// raw map can't be marshalled — should never happen for a
// proposal that already passed Validate.
func RenderYAML(p *ProjectYAML) ([]byte, error) {
	if p == nil || len(p.Raw) == 0 {
		return nil, errors.New("proposal is empty")
	}
	return yaml.Marshal(p.Raw)
}

// ProposalProjectID pulls the projectId field out of the proposal
// map. Returns "" when missing — callers should validate via the
// registry parser first.
func ProposalProjectID(p *ProjectYAML) string {
	if p == nil || p.Raw == nil {
		return ""
	}
	if id, ok := p.Raw["projectId"].(string); ok {
		return id
	}
	return ""
}
