package assetschema

// Workflow schema — curated editable surface for registry.Workflow and
// its repeating steps{} MAP collection. The difference from the swarm's
// roles[] is map-key identity (KeyIsMapKey): a step's id is the YAML map
// key, not a struct field, so it's rendered/bound as a synthetic key.
//
// Storage split (WORKFLOW.md = frontmatter + body): a step's prompt is
// body-backed (## Prompts → ### <stepId>); everything else is frontmatter.
// The parser lets a frontmatter `prompt:` silently override the body, so
// the editor removes the inline key and keeps the body canonical.
//
// Deferred (still YAML-editable, tracked):
//   - terminals — end-state map; scoped out of P3 (steps editor).
//   - pedantic — *bool tri-state (nil = inherit) not expressible as a
//     true/false select.
//   - step gates / payload / params / initial_task.* — complex nested
//     blocks edited as a unit.

// WorkflowSchema returns the curated form schema for a workflow asset.
func WorkflowSchema() AssetSchema {
	return AssetSchema{
		Asset: "workflow",
		Sections: []Section{
			{
				Title: "Identity",
				Fields: []Field{
					{Path: "workflowId", Label: "Workflow ID", Kind: KindString, Required: true, ReadOnly: true, Help: "Unique identifier; matches the WORKFLOW.md filename. Renaming would orphan the file."},
					{Path: "displayName", Label: "Display name", Kind: KindString},
					{Path: "description", Label: "Description", Kind: KindString, Multiline: true, Help: "One-paragraph summary; required by the workflow-shape doctor check and shown in the picker."},
					{Path: "version", Label: "Version", Kind: KindString},
				},
			},
			{
				Title: "Execution",
				Fields: []Field{
					{Path: "entrypoint", Label: "Entrypoint", Kind: KindString, Required: true, Help: "The first step (or terminal) to execute. Must match a step/terminal id."},
					{Path: "maxStepVisits", Label: "Max step visits", Kind: KindInt, Default: "3", Help: "Per-step rework cap before the workflow fails."},
					{Path: "maxIterations", Label: "Max iterations", Kind: KindInt, Default: "20", Help: "Global cap on step transitions per execution."},
					{Path: "maxWallClock", Label: "Max wall-clock", Kind: KindDuration, Help: `Hard ceiling on one execution (e.g. "30m", "1h"). Empty = no cap.`},
					{Path: "cleanup_artifacts", Label: "Cleanup artifacts", Kind: KindStringList, Help: "Workspace-relative paths the executor deletes before the entrypoint runs."},
				},
			},
			{
				Title:    "Behavior",
				Advanced: true,
				Fields: []Field{
					{Path: "resume_after_children", Label: "Resume after children", Kind: KindBool, Help: "Opt a custom workflow into the strict-adaptive resume guard for delegated children."},
					{Path: "require_input_artifacts", Label: "Require input artifacts", Kind: KindBool, Help: "Reject companion delegations that arrive without input artifacts."},
					{Path: "ingest_input_artifacts", Label: "Ingest input artifacts", Kind: KindBool, Help: "Deterministically ingest the task's input artifacts into project RAG on completion."},
					{Path: "a2a.publish", Label: "Publish as A2A agent", Kind: KindBool, Help: "Make this workflow discoverable + reachable via the A2A agent endpoint."},
				},
			},
		},
		Collections: []Collection{
			{
				Path:        "steps",
				Singular:    "step",
				Title:       "Steps",
				Help:        "Workflow steps, keyed by id. Add / remove / rename via the id field; on_success / on_fail reference other step or terminal ids.",
				IDField:     "stepId",
				KeyIsMapKey: true,
				KeyLabel:    "Step ID",
				KeyHelp:     "The step's map key. Renaming = delete + re-add under the new key.",
				ItemSchema:  workflowStepItemSchema(),
				ItemDeferredPaths: []string{
					"gates",                 // []WorkflowGate — conditional transitions
					"payload",               // map[string]any — call_project input
					"params",                // map[string]any — spawn_project params
					"initial_task.workflow", // *WorkflowInitialTask
					"initial_task.payload",
				},
			},
		},
	}
}

// workflowStepItemSchema is the per-item form for one registry.WorkflowStep.
// Paths are relative to the step value (the map key is the synthetic
// IDField, handled by the collection, not a field here).
func workflowStepItemSchema() AssetSchema {
	return AssetSchema{
		Asset: "workflowStep",
		Sections: []Section{
			{
				Title: "Step",
				Fields: []Field{
					{Path: "type", Label: "Type", Kind: KindString, Required: true, Help: "agent | gate | approval | plan | system | call_project | spawn_project | a2a_call | forge.post_review …"},
					{Path: "role", Label: "Role", Kind: KindString, Help: "Swarm role that performs an agent step."},
					{Path: "on_success", Label: "On success", Kind: KindString, Help: "Next step/terminal id on success."},
					{Path: "on_fail", Label: "On fail", Kind: KindString, Help: "Step/terminal id on failure. Empty = fail the execution."},
					{Path: "timeout", Label: "Timeout", Kind: KindDuration, Help: `Per-step wall-clock (e.g. "30m"). Empty = no per-step cap.`},
				},
			},
			{
				Title: "Prompt",
				Fields: []Field{
					// Lives in the WORKFLOW.md body (## Prompts → ### id).
					{Path: "prompt", Label: "Prompt", Kind: KindString, Backing: BackingBody, Help: "Instruction for an agent step. Stored in the body; required for agent steps."},
				},
			},
			{
				Title:    "Retry",
				Advanced: true,
				Fields: []Field{
					{Path: "retryPolicy.maxRetries", Label: "Max retries", Kind: KindInt},
					{Path: "retryPolicy.backoff", Label: "Backoff", Kind: KindString, Help: `e.g. "5s", "exponential".`},
				},
			},
			{
				Title:    "Routing & handlers",
				Advanced: true,
				Help:     "Type-specific fields. Leave blank unless the step type uses them.",
				Fields: []Field{
					{Path: "handler", Label: "Handler", Kind: KindString, Help: "SystemHandler name for system-typed steps (e.g. rag.index)."},
					{Path: "delegated_workflow", Label: "Delegated workflow", Kind: KindString, Help: "Pins the workflow delegated tasks from this step run under."},
					{Path: "gating_reviews", Label: "Gating reviews", Kind: KindBool, Help: "On forge.post_review: post a real APPROVE/REQUEST_CHANGES review."},
					{Path: "cancel_on_timeout", Label: "Cancel callee on timeout", Kind: KindBool, Help: "call_project: cascade-cancel the callee on timeout."},
					{Path: "target_project", Label: "Target project", Kind: KindString, Help: "call_project: callee project id."},
					{Path: "target_workflow", Label: "Target workflow", Kind: KindString, Help: "call_project: callee workflow id."},
					{Path: "expect.schema", Label: "Expect schema", Kind: KindString, Help: "call_project: result-envelope schema name."},
					{Path: "template", Label: "Template", Kind: KindString, Help: "spawn_project: project-template slug."},
					{Path: "agent_url", Label: "Agent URL", Kind: KindString, Help: "a2a_call: partner agent endpoint."},
					{Path: "api_key_env", Label: "API key env", Kind: KindString, Help: "a2a_call: env var holding the X-API-Key."},
				},
			},
		},
	}
}

// WorkflowDeferredPaths are registry.Workflow top-level leaves not given a
// structured form yet (tracked so they aren't confused with "missing").
var WorkflowDeferredPaths = []string{
	// End-state map — scoped out of the P3 steps editor.
	"terminals",
	// *bool tri-state (nil = inherit project/task) — a true/false select
	// can't express "unset"; edit via raw YAML.
	"pedantic",
}
