package projectwizard

import "fmt"

// bundleIDs is the pre-registry shape-check result: the slugs the
// bundle declares for its project, swarm, and each workflow. Used
// both for slug-validity checking and for the live-collision check
// (design §5.2 step 1) before anything is rendered to disk.
type bundleIDs struct {
	ProjectID   string
	SwarmID     string
	WorkflowIDs []string
}

// shapeCheckBundle runs the cheap, pre-registry structural checks
// design §5.2 calls out as step 1: `1 <= len(Workflows) <= 2`, every
// declared ID present and slug-valid. It does NOT touch the registry
// (no cross-references, no file rendering) — that is staged
// validation's job. Returns the extracted IDs (for the caller's live-
// collision check) plus a list of plain-language error strings; a
// non-empty error list means the bundle must not proceed further.
func shapeCheckBundle(bundle *ComposedBundle) (bundleIDs, []string) {
	var errs []string
	var ids bundleIDs
	if bundle == nil {
		return ids, []string{"bundle is empty"}
	}

	ids.ProjectID = stringField(bundle.Project, "projectId")
	if ids.ProjectID == "" {
		errs = append(errs, "project.projectId is required")
	} else if !isSafeProjectID(ids.ProjectID) {
		errs = append(errs, fmt.Sprintf("project.projectId %q is not a valid slug", ids.ProjectID))
	}

	ids.SwarmID = stringField(bundle.Swarm, "swarmId")
	if ids.SwarmID == "" {
		errs = append(errs, "swarm.swarmId is required")
	} else if !isSafeProjectID(ids.SwarmID) {
		errs = append(errs, fmt.Sprintf("swarm.swarmId %q is not a valid slug", ids.SwarmID))
	}

	n := len(bundle.Workflows)
	if n < 1 {
		errs = append(errs, "at least one workflow is required")
	}
	if n > 2 {
		errs = append(errs, fmt.Sprintf("at most 2 workflows are allowed in v1, got %d", n))
	}
	seen := map[string]bool{}
	for i, wf := range bundle.Workflows {
		id := stringField(wf, "workflowId")
		if id == "" {
			errs = append(errs, fmt.Sprintf("workflows[%d].workflowId is required", i))
			continue
		}
		if !isSafeProjectID(id) {
			errs = append(errs, fmt.Sprintf("workflows[%d].workflowId %q is not a valid slug", i, id))
			continue
		}
		if seen[id] {
			errs = append(errs, fmt.Sprintf("duplicate workflowId %q in bundle", id))
			continue
		}
		seen[id] = true
		ids.WorkflowIDs = append(ids.WorkflowIDs, id)
	}

	return ids, errs
}

// liveEntityIDs is the set of project/swarm/workflow IDs already
// present in the live registry, used for the collision check.
// LoadFromPaths' later-wins merge would otherwise silently overwrite
// (rather than reject) a same-ID live entity, defeating detection —
// so this check runs BEFORE the staged merge (design §5.2 "no ID
// collisions with the live registry").
type liveEntityIDs struct {
	Projects  map[string]bool
	Swarms    map[string]bool
	Workflows map[string]bool
}

// collisionCheckBundle reports a plain-language error per bundle ID
// that already exists in the live registry.
func collisionCheckBundle(ids bundleIDs, live liveEntityIDs) []string {
	var errs []string
	if ids.ProjectID != "" && live.Projects[ids.ProjectID] {
		errs = append(errs, fmt.Sprintf("projectId %q already exists in the live registry", ids.ProjectID))
	}
	if ids.SwarmID != "" && live.Swarms[ids.SwarmID] {
		errs = append(errs, fmt.Sprintf("swarmId %q already exists in the live registry", ids.SwarmID))
	}
	for _, id := range ids.WorkflowIDs {
		if live.Workflows[id] {
			errs = append(errs, fmt.Sprintf("workflowId %q already exists in the live registry", id))
		}
	}
	return errs
}

// stringField reads a string value from a loose map[string]any,
// returning "" for a missing key or a non-string value.
func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
