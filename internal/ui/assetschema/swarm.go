package assetschema

// Swarm schema — the curated editable surface for registry.Swarm and its
// repeating roles[] collection. The drift-guard (swarm_test.go) keeps
// both struct surfaces honest: every Swarm leaf is a field or the roles
// collection key; every SwarmRole leaf is an item field or a conscious
// ItemDeferredPaths entry.
//
// Storage split (SWARM.md = YAML frontmatter + Markdown body):
//   - Swarm displayName / leadRole / rolePrelude and all role
//     frontmatter (model, runtime, permissions, …) live in the
//     frontmatter and patch through the yaml.Node patcher.
//   - A role's systemPrompt lives in the body (## Role prompts →
//     ### <name>) and routes through ReplaceSwarmRolePrompts — hence
//     Backing: BackingBody.
//
// Deferred (still YAML-editable, tracked so they aren't "missing"):
//   - outputSchema.* + injectSchemaIntoPrompt — the structured-output
//     contract; a recursive JSON-schema shape edited as a unit.
//   - plausibilityRules — a list of conditional rule objects.
//   - runtime.envVars — a free-form string map.

// SwarmSchema returns the curated form schema for a swarm asset.
func SwarmSchema() AssetSchema {
	return AssetSchema{
		Asset: "swarm",
		Sections: []Section{
			{
				Title: "Identity",
				Fields: []Field{
					{Path: "swarmId", Label: "Swarm ID", Kind: KindString, Required: true, ReadOnly: true, Help: "Unique identifier; matches the SWARM.md filename. Renaming would orphan the file."},
					{Path: "displayName", Label: "Display name", Kind: KindString},
					{Path: "leadRole", Label: "Lead role", Kind: KindString, Help: "Name of the role that acts as the lead/planner. Must match one of the roles below."},
					{Path: "rolePrelude", Label: "Role prelude", Kind: KindString, Multiline: true, Help: "Operator text prepended to every role's system prompt (after the built-in safety prelude)."},
				},
			},
		},
		Collections: []Collection{
			{
				Path:       "roles",
				Singular:   "role",
				Title:      "Roles",
				Help:       "Agent roles in this swarm. Order matters for display; each role is matched by name when patching.",
				IDField:    "name",
				Ordered:    true,
				ItemSchema: swarmRoleItemSchema(),
				ItemDeferredPaths: []string{
					// Structured-output contract — recursive JSON-schema
					// shape; edited as a unit via raw YAML.
					"outputSchema.type", "outputSchema.version", "outputSchema.properties",
					"outputSchema.required", "outputSchema.enum", "outputSchema.items",
					"outputSchema.minLength", "outputSchema.plausibility",
					"injectSchemaIntoPrompt",
					// Conditional plausibility rule objects.
					"plausibilityRules",
					// Free-form container env map.
					"runtime.envVars",
				},
			},
		},
	}
}

// swarmRoleItemSchema is the per-item form for one registry.SwarmRole.
// Field paths are relative to the role element (e.g. "runtime.image").
func swarmRoleItemSchema() AssetSchema {
	return AssetSchema{
		Asset: "swarmRole",
		Sections: []Section{
			{
				Title: "Role",
				Fields: []Field{
					{Path: "name", Label: "Name", Kind: KindString, Required: true, Help: "Unique within the swarm; the identity key for patching."},
					{Path: "description", Label: "Description", Kind: KindString, Help: "One-line summary; shown to the lead as a role catalog for adaptive workflows."},
					{Path: "count", Label: "Count", Kind: KindInt, Default: "1", Help: "Number of agents for this role."},
					{Path: "runtimePolicy", Label: "Runtime policy", Kind: KindEnum, Enum: []string{"ephemeral", "warm"}, Help: "Container lifecycle. Empty = daemon default."},
					{Path: "aliases", Label: "Aliases", Kind: KindStringList, Help: "Alternate names the lead's plan may use that resolve to this role."},
				},
			},
			{
				Title: "Model",
				Fields: []Field{
					{Path: "model", Label: "Model", Kind: KindString, Help: "Overrides the daemon model for this role. Empty = daemon default."},
					{Path: "modelFallback", Label: "Model fallback", Kind: KindString, Help: "Backup model on a model-shaped failure (schema/plausibility/iteration). Prefer a different vendor."},
					{Path: "maxTokens", Label: "Max output tokens", Kind: KindInt, Help: "0 = daemon/model default."},
					{Path: "contextSize", Label: "Context size", Kind: KindInt, Help: "0 = daemon/model default."},
					{Path: "responseFormat", Label: "Response format", Kind: KindString, Help: `Gateway output constraint. "json_object" enforces a parseable JSON envelope; empty = free-form.`},
				},
			},
			{
				Title: "Prompt",
				Fields: []Field{
					// Lives in the SWARM.md body, not the frontmatter.
					{Path: "systemPrompt", Label: "System prompt", Kind: KindString, Backing: BackingBody, Help: "Role's system message. Prepended with the built-in safety prelude + the swarm rolePrelude at runtime."},
				},
			},
			{
				Title:    "Output contract",
				Advanced: true,
				Help:     "Lightweight output checks. The richer outputSchema block stays in raw YAML.",
				Fields: []Field{
					{Path: "requiredOutputKeys", Label: "Required output keys", Kind: KindStringList, Help: "Top-level result.json keys the role must emit."},
					{Path: "shapeRetryHint", Label: "Shape-retry hint", Kind: KindString, Help: "Role-specific corrective text appended when a shape retry fires."},
				},
			},
			{
				Title:    "Runtime",
				Advanced: true,
				Fields: []Field{
					{Path: "runtime.image", Label: "Container image", Kind: KindString, Help: "Image for this role's agent container."},
					{Path: "runtime.cpu", Label: "CPU limit", Kind: KindString, Help: `e.g. "2".`},
					{Path: "runtime.memory", Label: "Memory limit", Kind: KindString, Help: `e.g. "4Gi".`},
					{Path: "runtime.network", Label: "Network mode", Kind: KindEnum, Enum: []string{"host", "none", "daemon-only"}, Help: "Empty = permissive default (rootless egress)."},
				},
			},
			{
				Title:    "Permissions",
				Advanced: true,
				Fields: []Field{
					{Path: "permissions.allowedTools", Label: "Allowed tools", Kind: KindStringList, Help: "Agent runtime tools this role may use."},
					{Path: "permissions.autonomousTaskCreation", Label: "Autonomous task creation", Kind: KindBool},
					{Path: "permissions.delegationAllowed", Label: "Delegation allowed", Kind: KindBool},
					{Path: "permissions.maxDelegations", Label: "Max delegations", Kind: KindInt},
				},
			},
		},
	}
}
