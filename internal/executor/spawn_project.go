package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/templates"
)

// spawnTemplateCatalog is the narrow read-only slice of
// *templates.Catalog the spawn handler needs. Lets tests inject
// a fake without the whole filesystem-backed catalog.
type spawnTemplateCatalog interface {
	Get(slug string) (templates.Manifest, bool)
	MaterialiseFiles(m templates.Manifest, params map[string]string) (map[string]string, error)
}

// registryReloader triggers a stage-validate-activate cycle so
// a freshly-written project YAML becomes resolvable
// immediately. Matches the shape of *config.ConfigReloader.
type registryReloader interface {
	Reload() error
}

// errSpawnDisabled is returned by handleSpawnProjectStep when
// any of the required dependencies (feature flag, repo,
// catalog, configsDir) isn't wired. Same CROSS_PROJECT_DISABLED
// pattern as call_project — the workflow's on_fail branch can
// carry the workflow forward without exposing the surface.
var errSpawnDisabled = errors.New("spawn_project: inter-project orchestration disabled (set VORNIK_INTER_PROJECT_ENABLED=true and wire ProjectSpawnRepository + template catalog + configsDir)")

// spawnProjectResult mirrors callProjectResult's shape — small
// structured value the dispatch site uses to advance the
// workflow. spawn_project is FIRE-AND-FORGET from the caller's
// perspective (LLD §6.2) so there's no pause, no CPC; the
// caller proceeds to OnSuccess immediately after the spawn
// completes.
type spawnProjectResult struct {
	// SpawnedProject is the slug of the newly-materialised
	// project (or the existing one when the step short-circuits
	// on the idempotence path). Workflow downstream steps can
	// reference it via state.LastResult.
	SpawnedProject string
	// SpawnID is the project_spawns row id. Empty when the
	// step short-circuited (no new row inserted).
	SpawnID string
	// InitialTaskID is set when the workflow author declared
	// an initial_task block and the seed task was created.
	// Empty otherwise.
	InitialTaskID string
	// Skipped is true on the idempotent no-op path — the
	// spawned project already exists from a prior run.
	Skipped bool
}

// handleSpawnProjectStep is the executor handler for the
// `spawn_project` step type. See LLD §6.2.
//
// Lifecycle:
//  1. Validate feature flag + dependencies (repo + catalog +
//     configsDir).
//  2. Resolve template via the catalog. Unknown slug → step
//     fails with TEMPLATE_NOT_FOUND.
//  3. Check the parent project's AllowSpawn.Templates
//     allowlist. Denial → TEMPLATE_NOT_ALLOWED.
//  4. Check MaxSpawnsPerDay rate limit via
//     spawnRepo.CountForProjectSince. Exceeded →
//     SPAWN_LIMIT_EXCEEDED.
//  5. Resolve the spawned project's slug from params (key
//     "projectId" preferred, falls back to "name"). The
//     UNIQUE constraint on spawned_project requires uniqueness
//     across the registry.
//  6. Idempotence: if spawnRepo.GetBySpawnedProject already
//     has a row for this slug, log + proceed without re-
//     materialising. Lets retry-from-step / scheduler
//     recovery re-execute the workflow without double-
//     spawning.
//  7. Render the template via catalog.MaterialiseFiles
//     (stringified params).
//  8. Write rendered files atomically via the existing
//     WriteRenderedFilesExclusive primitive (O_EXCL + project-
//     id whitelist). Collision → PROJECT_EXISTS.
//  9. Insert project_spawns row. ErrDuplicateKey on the slug
//     UNIQUE is treated as the idempotence win, not an error.
//  10. Trigger registry reload (best-effort; the watcher's
//     poll picks it up regardless).
//  11. Create initial_task if declared.
func (e *Executor) handleSpawnProjectStep(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	currentStepID string,
	step *registry.WorkflowStep,
	stepResults map[string]json.RawMessage,
) (spawnProjectResult, error) {
	if !interProjectEnabled() || e.spawnRepo == nil || e.templateCatalog == nil || strings.TrimSpace(e.configsDir) == "" {
		return spawnProjectResult{}, errSpawnDisabled
	}

	// Resolve parent project — we need its AllowSpawn block.
	parentProj := e.workflows.GetProject(task.ProjectID)
	if parentProj == nil {
		return spawnProjectResult{}, fmt.Errorf("PROJECT_NOT_FOUND: parent project %q not in registry", task.ProjectID)
	}

	// Template allowlist gate. Closed by default; operators
	// opt in per-project via AllowSpawn.Templates.
	if !parentProj.AllowsSpawnTemplate(step.Template) {
		return spawnProjectResult{}, fmt.Errorf(
			"TEMPLATE_NOT_ALLOWED: project %q is not allowed to spawn from template %q (configure allowSpawn.templates)",
			task.ProjectID, step.Template,
		)
	}

	// Rate-limit check. Zero / negative disables the cap.
	if parentProj.AllowSpawn.MaxSpawnsPerDay > 0 {
		since := time.Now().Add(-24 * time.Hour)
		n, err := e.spawnRepo.CountForProjectSince(ctx, task.ProjectID, since)
		if err != nil {
			return spawnProjectResult{}, fmt.Errorf("spawn_project: count recent spawns: %w", err)
		}
		if n >= int64(parentProj.AllowSpawn.MaxSpawnsPerDay) {
			return spawnProjectResult{}, fmt.Errorf(
				"SPAWN_LIMIT_EXCEEDED: project %q has spawned %d projects in the past 24h (cap = %d)",
				task.ProjectID, n, parentProj.AllowSpawn.MaxSpawnsPerDay,
			)
		}
	}

	// Resolve ${outputs.<step>.<field>} references in the
	// step's params block — Phase D. interpolateOutputs walks
	// the map recursively; absent references resolve to "".
	// The result is a fresh map[string]any so workflow YAML
	// stays untouched (other executions of the same step see
	// the original).
	resolvedParams, _ := interpolateOutputs(step.Params, stepResults).(map[string]any)
	if resolvedParams == nil {
		resolvedParams = step.Params
	}

	// Spawned slug — taken from params. "projectId" preferred
	// (matches the registry's YAML field name), "name" as
	// fallback (matches the LLD's worked-example wording).
	spawnedSlug := resolveSpawnSlug(resolvedParams)
	if spawnedSlug == "" {
		return spawnProjectResult{}, errors.New("spawn_project: params must include either \"projectId\" or \"name\" (the spawned project's slug)")
	}

	// Idempotence check. A second execution of the same step
	// (retry-from-step, scheduler recovery) for a previously-
	// spawned project must be a no-op, not an error. The DB-
	// level UNIQUE constraint is the safety net; this check
	// gives a clean log + skip.
	if prior, err := e.spawnRepo.GetBySpawnedProject(ctx, spawnedSlug); err == nil && prior != nil {
		e.logger.Info().
			Str("task_id", task.ID).
			Str("spawned_project", spawnedSlug).
			Str("prior_spawn_id", prior.ID).
			Msg("spawn_project: idempotent skip — project already exists from prior spawn")
		return spawnProjectResult{
			SpawnedProject: spawnedSlug,
			Skipped:        true,
		}, nil
	}

	// Resolve the manifest. Unknown slug at this point is a
	// workflow-authoring bug (gate above wouldn't have fired
	// either) — surface PROJECT_NOT_FOUND-style error so the
	// on_fail branch can pattern-match.
	manifest, ok := e.templateCatalog.Get(step.Template)
	if !ok {
		return spawnProjectResult{}, fmt.Errorf("TEMPLATE_NOT_FOUND: template %q is not in the catalog", step.Template)
	}

	// Stringify params for text/template. v1 conversion: leave
	// strings as-is, JSON-marshal anything else (gives lists +
	// maps a sensible default representation). Phase C adds
	// ${outputs.x.y} resolution; v1 is literal pass-through.
	stringParams, err := stringifyParams(resolvedParams)
	if err != nil {
		return spawnProjectResult{}, fmt.Errorf("spawn_project: stringify params: %w", err)
	}

	rendered, err := e.templateCatalog.MaterialiseFiles(manifest, stringParams)
	if err != nil {
		return spawnProjectResult{}, fmt.Errorf("spawn_project: render template %q: %w", step.Template, err)
	}

	// Consent-bypass guard (security): a spawned child must NOT start out
	// accepting calls from its spawner. acceptCallsFrom is the callee's
	// consent gate; letting a spawn pre-grant the parent — via
	// caller-supplied template params injected into acceptCallsFrom, or a
	// template that statically lists the spawner — would let any
	// spawn-capable project mint a child it can immediately command,
	// bypassing the consent model. Cross-project consent between parent
	// and child must be granted deliberately AFTER spawn. (Inter-project
	// review batch 4, 2026-06-11.)
	if projYAML, ok := renderedProjectYAML(rendered, spawnedSlug); ok {
		var child registry.Project
		if uerr := yaml.Unmarshal([]byte(projYAML), &child); uerr == nil && child.AcceptsCallsFrom(task.ProjectID) {
			return spawnProjectResult{}, fmt.Errorf(
				"SPAWN_CONSENT_BYPASS: spawned project %q would accept calls from its spawner %q at spawn time — remove the spawner from acceptCallsFrom; grant cross-project consent deliberately after spawn",
				spawnedSlug, task.ProjectID,
			)
		}
	}

	written, err := templates.WriteRenderedFilesExclusive(e.configsDir, rendered)
	if err != nil {
		var exists *templates.ExistingTargetError
		if errors.As(err, &exists) {
			return spawnProjectResult{}, fmt.Errorf("PROJECT_EXISTS: rendered file %q already on disk — pick a different projectId / name param", exists.Target)
		}
		return spawnProjectResult{}, fmt.Errorf("spawn_project: write rendered files: %w", err)
	}

	// Persist the lineage row. Marshal the params map back to
	// JSON for storage so audits can answer "what params did
	// this spawn use".
	paramsJSON, _ := json.Marshal(resolvedParams)
	spawn := &persistence.ProjectSpawn{
		ParentTaskID:   task.ID,
		ParentProject:  task.ProjectID,
		ParentStepID:   currentStepID,
		SpawnedProject: spawnedSlug,
		TemplateSlug:   step.Template,
		Params:         paramsJSON,
	}
	if err := e.spawnRepo.Create(ctx, spawn); err != nil {
		// UNIQUE collision is the idempotence path — log + treat
		// as success. Any other error fails the step (the files
		// are already on disk; operator can clean up if needed).
		if errors.Is(err, persistence.ErrDuplicateKey) {
			e.logger.Warn().
				Str("task_id", task.ID).
				Str("spawned_project", spawnedSlug).
				Msg("spawn_project: project_spawns UNIQUE collision after WriteRenderedFilesExclusive (concurrent spawn race)")
			return spawnProjectResult{
				SpawnedProject: spawnedSlug,
				Skipped:        true,
			}, nil
		}
		return spawnProjectResult{}, fmt.Errorf("spawn_project: persist lineage row: %w", err)
	}

	// Trigger reload so the new project is immediately
	// resolvable. Best-effort — the file watcher catches the
	// new YAML on its next poll regardless.
	if e.registryReloader != nil {
		if rerr := e.registryReloader.Reload(); rerr != nil {
			e.logger.Warn().Err(rerr).
				Str("spawned_project", spawnedSlug).
				Msg("spawn_project: registry reload failed; project will be picked up by the next file-watcher poll")
		}
	}

	result := spawnProjectResult{
		SpawnedProject: spawnedSlug,
		SpawnID:        spawn.ID,
	}

	// Optional initial task. The seed task lands in the
	// spawned project's queue; the scheduler picks it up on
	// the next lease cycle.
	if step.InitialTask != nil {
		initialTaskID, ierr := e.seedInitialTask(ctx, task, spawnedSlug, step.InitialTask)
		if ierr != nil {
			e.logger.Warn().Err(ierr).
				Str("spawned_project", spawnedSlug).
				Msg("spawn_project: initial task creation failed (project was still materialised; operator can seed manually)")
		} else {
			result.InitialTaskID = initialTaskID
		}
	}

	e.logger.Info().
		Str("parent_task_id", task.ID).
		Str("parent_project", task.ProjectID).
		Str("spawned_project", spawnedSlug).
		Str("template", step.Template).
		Strs("files_written", written).
		Str("initial_task_id", result.InitialTaskID).
		Msg("spawn_project: project materialised")

	// Phase C observability: emit live event, bump metric, write
	// audit row. Best-effort + nil-safe.
	e.emitLive(ctx, execution.ID, livepubsub.KindProjectSpawned, livepubsub.ProjectSpawnedPayload{
		SpawnID:        spawn.ID,
		SpawnedProject: spawnedSlug,
		Template:       step.Template,
		InitialTaskID:  result.InitialTaskID,
		StepID:         currentStepID,
	})
	if e.metrics != nil {
		e.metrics.RecordProjectSpawn(task.ProjectID, step.Template)
	}
	e.recordSpawnAudit(ctx, task, currentStepID, spawn, result.InitialTaskID)

	return result, nil
}

// seedInitialTask creates the optional seed task in the spawned
// project. Best-effort — failure here doesn't roll back the
// spawn (the project is on disk and the lineage row is
// committed); operator can create the task manually.
func (e *Executor) seedInitialTask(
	ctx context.Context,
	parentTask *persistence.Task,
	spawnedProjectID string,
	initial *registry.WorkflowInitialTask,
) (string, error) {
	if initial == nil {
		return "", nil
	}
	payload := map[string]any{
		"context": map[string]any{
			"prompt": "spawned-project initial task — see args.spawn_payload",
		},
		"args": map[string]any{
			"spawn_payload": initial.Payload,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal initial task payload: %w", err)
	}
	taskID := persistence.GenerateID("task")
	var wfID *string
	if w := strings.TrimSpace(initial.Workflow); w != "" {
		wfID = &w
	}
	now := time.Now()
	t := &persistence.Task{
		ID:             taskID,
		ProjectID:      spawnedProjectID,
		WorkflowID:     wfID,
		ParentTaskID:   &parentTask.ID,
		CreationSource: persistence.TaskCreationSourceDelegation,
		Status:         persistence.TaskStatusQueued,
		Priority:       50,
		Payload:        body,
		Attempt:        1,
		MaxAttempts:    3,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := e.taskRepo.Create(ctx, t); err != nil {
		return "", err
	}
	return taskID, nil
}

// resolveSpawnSlug picks the spawned project's slug from the
// step params. Preference: "projectId" (matches the YAML field
// name in the spawned project config), then "name" (matches
// the LLD's worked example). Returns "" when neither is
// present or both are non-string types.
// renderedProjectYAML returns the rendered project-config file content
// for the spawned slug. Template targets render to
// configs/projects/<slug>.yaml, so match that suffix first; fall back to
// the first projects/*.yaml in the set for templates that name the file
// differently. Returns ("", false) when no project file is present.
func renderedProjectYAML(rendered map[string]string, slug string) (string, bool) {
	want := "projects/" + slug + ".yaml"
	var fallback string
	var haveFallback bool
	for target, content := range rendered {
		if strings.HasSuffix(target, want) {
			return content, true
		}
		if !haveFallback && strings.Contains(target, "projects/") && strings.HasSuffix(target, ".yaml") {
			fallback, haveFallback = content, true
		}
	}
	return fallback, haveFallback
}

func resolveSpawnSlug(params map[string]any) string {
	for _, key := range []string{"projectId", "name"} {
		if v, ok := params[key]; ok {
			if s, sok := v.(string); sok {
				if t := strings.TrimSpace(s); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// stringifyParams converts the YAML-decoded map[string]any
// into the map[string]string the template renderer expects.
// Strings pass through verbatim; numbers + bools convert via
// fmt.Sprint; everything else (lists, nested maps) JSON-
// marshals so the template author can echo them into the
// output via {{.<key>}} (the rendered form is the JSON
// literal — workflow authors can wrap with `| safe` or
// `tojson`-style pipes as their template manifest declares).
func stringifyParams(in map[string]any) (map[string]string, error) {
	if len(in) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		switch tv := v.(type) {
		case nil:
			out[k] = ""
		case string:
			out[k] = tv
		case bool:
			if tv {
				out[k] = "true"
			} else {
				out[k] = "false"
			}
		case int, int32, int64, float32, float64:
			out[k] = fmt.Sprint(tv)
		default:
			b, err := json.Marshal(tv)
			if err != nil {
				return nil, fmt.Errorf("param %q: %w", k, err)
			}
			out[k] = string(b)
		}
	}
	return out, nil
}
