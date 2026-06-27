// Package forge implements the deterministic, daemon-side system-step handlers
// that publish an agent's work to a code-hosting forge without any LLM running
// git or forge CLIs:
//
//   - "forge.open_change_request" — pushes the task's committed branch and opens
//     a pull/merge request (idempotent).
//   - "forge.post_review" — posts the reviewer agent's prose as a review/note.
//
// Both resolve the project's forgeapi.ForgeProvider from config and dispatch — no
// provider name appears here, so GitHub/GitLab/Gitea behave identically. The git
// + forge mutations run in the daemon (writable .git); the LLM only authors
// content. See https://docs.vornik.io
package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
)

// ProviderResolver resolves a project's configured forge provider. Wired by the
// service container (which holds the project registry + a provider cache); a
// narrow interface here keeps the handler test doubles trivial.
type ProviderResolver interface {
	ForgeProvider(ctx context.Context, projectID string) (forgeapi.ForgeProvider, error)
}

// PublishSource locates what to publish for a task: the daemon-side git dir to
// push from and the commit sha the executor produced for this task (the merged
// worktree). Wired by the service container; faked in tests.
type PublishSource interface {
	PublishSource(ctx context.Context, task *persistence.Task) (gitDir, sha string, err error)
}

// forgePayload is the narrow shape the handlers read out of the task payload.
// The typed ForgeJob is accepted at the top level (`forge_job`, where the
// github channel's task creator stamps it — keeping the existing `context`
// map[string]string intact) OR under `context.forge_job` (a future channel that
// builds a richer context object). Top-level wins when both are present.
type forgePayload struct {
	ForgeJob *forgeapi.ForgeJob `json:"forge_job"`
	Context  struct {
		ForgeJob *forgeapi.ForgeJob `json:"forge_job"`
	} `json:"context"`
}

// forgeJobFromTask extracts the typed ForgeJob, erroring clearly when the
// upstream step didn't record one (a workflow-wiring bug, not a transient fault).
func forgeJobFromTask(task *persistence.Task, handler string) (*forgeapi.ForgeJob, error) {
	if task == nil {
		return nil, fmt.Errorf("%s: task is nil", handler)
	}
	var pl forgePayload
	if len(task.Payload) > 0 {
		_ = json.Unmarshal(task.Payload, &pl)
	}
	j := pl.ForgeJob
	if j == nil {
		j = pl.Context.ForgeJob
	}
	if j == nil {
		return nil, fmt.Errorf("%s: no forge job on task — forge_job (or context.forge_job) must be set by the channel/intake step", handler)
	}
	if j.Repo == "" || j.Number == 0 {
		return nil, fmt.Errorf("%s: forge job missing repo/number (%+v)", handler, *j)
	}
	return j, nil
}

// branchForJob is the deterministic publish branch for a job: a function of the
// issue number + whether it's a feature, so re-runs produce the same name and
// the forge-side idempotency (lookup by head) holds.
func branchForJob(j forgeapi.ForgeJob) string {
	verb := "fix"
	if isFeature(j) {
		verb = "feat"
	}
	return fmt.Sprintf("%s/issue-%d", verb, j.Number)
}

// isFeature reports whether the job is an enhancement/feature (→ draft CR) vs a
// bug fix, by label.
func isFeature(j forgeapi.ForgeJob) bool {
	for _, l := range j.Labels {
		switch strings.ToLower(strings.TrimSpace(l)) {
		case "enhancement", "feature", "feature-request":
			return true
		}
	}
	return false
}

// titleForJob / bodyForJob template the CR title+body from the issue. Never
// LLM-authored — deterministic so the same job always yields the same CR.
func titleForJob(j forgeapi.ForgeJob) string {
	verb := "Fix"
	if isFeature(j) {
		verb = "Implement"
	}
	if t := strings.TrimSpace(j.Title); t != "" {
		return fmt.Sprintf("%s #%d: %s", verb, j.Number, t)
	}
	return fmt.Sprintf("%s #%d", verb, j.Number)
}

func bodyForJob(j forgeapi.ForgeJob) string {
	b := fmt.Sprintf("Closes #%d.\n\n", j.Number)
	if s := strings.TrimSpace(j.Body); s != "" {
		if len(s) > 600 {
			s = s[:600] + "…"
		}
		b += "**Requested:** " + s + "\n\n"
	}
	b += "_Opened automatically by vornik. Review the diff against the request above before merging._"
	return b
}
