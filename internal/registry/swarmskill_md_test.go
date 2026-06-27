package registry

import (
	"strings"
	"testing"
)

// makeTestSkill builds a small but realistic SwarmSkill covering
// the round-trip property the test suite leans on: a workflow with
// two agent steps + terminals, and the roles those steps reference.
func makeTestSkill() *SwarmSkill {
	return &SwarmSkill{
		Name:        "research-and-write",
		Description: "Two-step researcher → writer pipeline.",
		Version:     "1.0.0",
		Author:      "vadim",
		License:     "MIT",
		Workflow: &Workflow{
			ID:          "research",
			DisplayName: "Research and Write",
			Description: "Two-step researcher → writer pipeline.",
			Version:     "1.0.0",
			Entrypoint:  "research",
			Steps: map[string]WorkflowStep{
				"research": {
					Type:      "agent",
					Role:      "researcher",
					Prompt:    "Gather facts and sources.",
					OnSuccess: "write",
					OnFail:    "failed",
					Timeout:   "30m",
				},
				"write": {
					Type:      "agent",
					Role:      "writer",
					Prompt:    "Compose the report.",
					OnSuccess: "done",
					OnFail:    "failed",
					Timeout:   "15m",
				},
			},
			Terminals: map[string]WorkflowTerminal{
				"done":   {Status: "COMPLETED"},
				"failed": {Status: "FAILED", Message: "Research failed"},
			},
		},
		Roles: []SwarmRole{
			{
				Name:         "researcher",
				Description:  "Gathers facts and sources.",
				SystemPrompt: "You are a careful researcher.",
			},
			{
				Name:         "writer",
				Description:  "Composes the report.",
				SystemPrompt: "You are a concise writer.",
			},
		},
	}
}

func TestMarshalSwarmSkill_RoundTrip(t *testing.T) {
	skill := makeTestSkill()

	bytes1, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{})
	if err != nil {
		t.Fatalf("marshal 1: %v", err)
	}

	parsed, err := ParseSwarmSkill(bytes1, "round-trip.md")
	if err != nil {
		t.Fatalf("parse: %v\noutput was:\n%s", err, bytes1)
	}

	if parsed.Name != skill.Name {
		t.Errorf("name: got %q want %q", parsed.Name, skill.Name)
	}
	if parsed.Description != skill.Description {
		t.Errorf("description mismatch")
	}
	if parsed.Version != skill.Version {
		t.Errorf("version: got %q want %q", parsed.Version, skill.Version)
	}
	if parsed.Author != skill.Author {
		t.Errorf("author: got %q want %q", parsed.Author, skill.Author)
	}
	if parsed.Workflow == nil {
		t.Fatal("workflow was nil after parse")
	}
	if parsed.Workflow.ID != skill.Workflow.ID {
		t.Errorf("workflow id: got %q want %q", parsed.Workflow.ID, skill.Workflow.ID)
	}
	if parsed.Workflow.Steps["research"].Prompt != "Gather facts and sources." {
		t.Errorf("research prompt: got %q", parsed.Workflow.Steps["research"].Prompt)
	}
	if parsed.Workflow.Steps["write"].Prompt != "Compose the report." {
		t.Errorf("write prompt: got %q", parsed.Workflow.Steps["write"].Prompt)
	}
	if len(parsed.Roles) != 2 {
		t.Fatalf("roles: got %d want 2", len(parsed.Roles))
	}
	gotPrompts := map[string]string{}
	for _, r := range parsed.Roles {
		gotPrompts[r.Name] = r.SystemPrompt
	}
	if gotPrompts["researcher"] != "You are a careful researcher." {
		t.Errorf("researcher systemPrompt: got %q", gotPrompts["researcher"])
	}
	if gotPrompts["writer"] != "You are a concise writer." {
		t.Errorf("writer systemPrompt: got %q", gotPrompts["writer"])
	}

	// Second marshal of the parsed result should byte-equal the first.
	bytes2, err := MarshalSwarmSkill(parsed, MarshalSwarmSkillOpts{})
	if err != nil {
		t.Fatalf("marshal 2: %v", err)
	}
	if string(bytes1) != string(bytes2) {
		t.Errorf("marshal(parse(marshal(x))) != marshal(x):\n--- first ---\n%s\n--- second ---\n%s", bytes1, bytes2)
	}
}

func TestMarshalSwarmSkill_Standard_DropsVornikBlock(t *testing.T) {
	skill := makeTestSkill()
	out, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{Standard: true})
	if err != nil {
		t.Fatalf("marshal standard: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "metadata:") {
		t.Errorf("standard output should not contain 'metadata:' block:\n%s", s)
	}
	if strings.Contains(s, "vornik:") {
		t.Errorf("standard output should not contain 'vornik:' key:\n%s", s)
	}
	if !strings.Contains(s, "## Prompts") {
		t.Errorf("standard output should preserve '## Prompts' body section:\n%s", s)
	}
	if !strings.Contains(s, "## Role prompts") {
		t.Errorf("standard output should preserve '## Role prompts' body section:\n%s", s)
	}
	if !strings.Contains(s, "name: research-and-write") {
		t.Errorf("standard output should keep canonical 'name:' field:\n%s", s)
	}
}

func TestMarshalSwarmSkill_RequiredFields(t *testing.T) {
	cases := []struct {
		name    string
		skill   *SwarmSkill
		wantErr string
	}{
		{"nil", nil, "nil"},
		{"missing name", &SwarmSkill{Description: "d", Version: "1.0", Workflow: &Workflow{ID: "w", Entrypoint: "s"}}, "name is required"},
		{"missing description", &SwarmSkill{Name: "n", Version: "1.0", Workflow: &Workflow{ID: "w", Entrypoint: "s"}}, "description is required"},
		{"missing version", &SwarmSkill{Name: "n", Description: "d", Workflow: &Workflow{ID: "w", Entrypoint: "s"}}, "version is required"},
		{"missing workflow", &SwarmSkill{Name: "n", Description: "d", Version: "1.0"}, "workflow is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := MarshalSwarmSkill(tc.skill, MarshalSwarmSkillOpts{})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("got err=%v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseSwarmSkill_RequiredFields(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			"missing name",
			"---\ndescription: d\nversion: 1.0\n---\n",
			"required field 'name'",
		},
		{
			"missing description",
			"---\nname: foo\nversion: 1.0\n---\n",
			"required field 'description'",
		},
		{
			"missing version",
			"---\nname: foo\ndescription: d\n---\n",
			"required field 'version'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSwarmSkill([]byte(tc.content), tc.name+".md")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("got err=%v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseSwarmSkill_UnknownSchemaVersion(t *testing.T) {
	content := `---
name: foo
description: d
version: 1.0
metadata:
  vornik:
    schema_version: 99
---
`
	_, err := ParseSwarmSkill([]byte(content), "v99.md")
	if err == nil || !strings.Contains(err.Error(), "schema_version 99") {
		t.Errorf("got err=%v, want 'schema_version 99'", err)
	}
}

func TestParseSwarmSkill_UnknownStepInPromptsBody(t *testing.T) {
	content := `---
name: foo
description: d
version: 1.0
metadata:
  vornik:
    schema_version: 1
    workflow:
      workflowId: w
      entrypoint: real
      steps:
        real:
          type: agent
          role: r
    roles:
      - name: r
---

## Prompts

### nonexistent

This step is not declared.
`
	_, err := ParseSwarmSkill([]byte(content), "unknown.md")
	if err == nil || !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("got err=%v, want mention of 'nonexistent'", err)
	}
}

func TestParseSwarmSkill_UnknownRoleInRolePromptsBody(t *testing.T) {
	content := `---
name: foo
description: d
version: 1.0
metadata:
  vornik:
    schema_version: 1
    workflow:
      workflowId: w
      entrypoint: real
      steps:
        real:
          type: agent
          role: r
          prompt: do it
    roles:
      - name: r
---

## Role prompts

### ghost

This role doesn't exist.
`
	_, err := ParseSwarmSkill([]byte(content), "ghost.md")
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("got err=%v, want mention of 'ghost'", err)
	}
}

func TestParseSwarmSkill_StepReferencesUnknownRole(t *testing.T) {
	content := `---
name: foo
description: d
version: 1.0
metadata:
  vornik:
    schema_version: 1
    workflow:
      workflowId: w
      entrypoint: s
      steps:
        s:
          type: agent
          role: ghost-role
          prompt: do it
    roles:
      - name: r
        systemPrompt: hi
---
`
	_, err := ParseSwarmSkill([]byte(content), "step-role.md")
	if err == nil || !strings.Contains(err.Error(), "ghost-role") {
		t.Errorf("got err=%v, want mention of 'ghost-role'", err)
	}
}

func TestParseSwarmSkill_BodySectionsPreserved(t *testing.T) {
	// Prose sections other than Prompts / Role prompts must NOT
	// cause parse errors — they're documentation.
	content := `---
name: foo
description: d
version: 1.0
metadata:
  vornik:
    schema_version: 1
    workflow:
      workflowId: w
      entrypoint: s
      steps:
        s:
          type: agent
          role: r
          prompt: do it
    roles:
      - name: r
        systemPrompt: hi
---

# Foo

## Overview

This is a freeform overview.

## When to use

For testing the parser.
`
	skill, err := ParseSwarmSkill([]byte(content), "overview.md")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if skill.Workflow == nil || skill.Workflow.Steps["s"].Prompt != "do it" {
		t.Errorf("expected inline frontmatter prompt to be preserved, got %#v", skill.Workflow)
	}
}

func TestMarshalSwarmSkill_DeterministicStepOrder(t *testing.T) {
	skill := makeTestSkill()
	out1, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Re-marshal a few times; output bytes must be identical
	// regardless of Go's map-iteration randomisation.
	for i := 0; i < 10; i++ {
		out2, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{})
		if err != nil {
			t.Fatalf("marshal iter %d: %v", i, err)
		}
		if string(out1) != string(out2) {
			t.Errorf("non-deterministic output on iter %d:\n%s\n---\n%s", i, out1, out2)
			break
		}
	}
}

func TestParseSwarmSkill_StandardFileHasNoVornikBlock(t *testing.T) {
	// A --standard export drops metadata.vornik.*. The parser
	// accepts the file but Workflow/Roles stay nil so the
	// importer can refuse it loudly.
	skill := makeTestSkill()
	out, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{Standard: true})
	if err != nil {
		t.Fatalf("marshal standard: %v", err)
	}
	// The body section MUST NOT have stale `## Prompts` /
	// `## Role prompts` content that would orphan-reference
	// missing structural data on parse.
	parsed, err := ParseSwarmSkill(out, "standard.md")
	if err == nil {
		t.Fatal("expected parse to refuse standard file with body prompt sections")
	}
	if !strings.Contains(err.Error(), "exported with --standard") {
		t.Errorf("got err=%v, expected mention of --standard", err)
	}
	_ = parsed
}

func TestParseSwarmSkill_MissingClosingFrontmatter(t *testing.T) {
	content := "---\nname: foo\ndescription: d\nversion: 1.0\n"
	_, err := ParseSwarmSkill([]byte(content), "noclose.md")
	if err == nil || !strings.Contains(err.Error(), "missing closing frontmatter marker") {
		t.Errorf("got err=%v, want 'missing closing frontmatter marker'", err)
	}
}
