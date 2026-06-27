package service

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/retention"
	"vornik.io/vornik/internal/ui"
)

// retentionPreviewAdapter satisfies ui.RetentionPreviewer. Wraps
// retention.Sweeper.Preview + builds the per-project Policy by
// merging daemon defaults with the project's YAML overrides.
//
// Constructed at HTTP wire-up time. The sweeper itself is
// stateless (db handle + logger only), so this is safe alongside
// the background sweeper goroutine started in container_autonomy.go.
type retentionPreviewAdapter struct {
	sweeper       *retention.Sweeper
	defaults      config.RetentionConfig
	artifactsRoot string
	registry      *registry.Registry
}

func newRetentionPreviewAdapter(sweeper *retention.Sweeper, cfg config.RetentionConfig, artifactsRoot string, reg *registry.Registry) ui.RetentionPreviewer {
	if sweeper == nil {
		return nil
	}
	return &retentionPreviewAdapter{
		sweeper:       sweeper,
		defaults:      cfg,
		artifactsRoot: artifactsRoot,
		registry:      reg,
	}
}

func (a *retentionPreviewAdapter) Preview(ctx context.Context, projectID string) (ui.RetentionPreviewCounts, error) {
	if a == nil || a.sweeper == nil {
		return ui.RetentionPreviewCounts{}, fmt.Errorf("retention preview not configured")
	}
	if projectID == "" {
		return ui.RetentionPreviewCounts{}, fmt.Errorf("project id required")
	}
	policy := a.resolvePolicy(projectID)
	counts, err := a.sweeper.Preview(ctx, policy)
	if err != nil {
		return ui.RetentionPreviewCounts{}, err
	}
	return ui.RetentionPreviewCounts{
		TaskLLMUsage:  counts.TaskLLMUsage,
		ToolAudit:     counts.ToolAudit,
		Tasks:         counts.Tasks,
		Executions:    counts.Executions,
		Artifacts:     counts.Artifacts,
		ArtifactFiles: counts.ArtifactFiles,
		TaskMessages:  counts.TaskMessages,
		MemoryChunks:  counts.MemoryChunks,
	}, nil
}

// resolvePolicy merges the daemon-wide retention defaults with the
// project's YAML overrides. Same shape the background sweeper
// applies; centralised here so the preview shows exactly what the
// next sweep would do. Per-project zero falls through to defaults,
// per-project non-zero wins.
func (a *retentionPreviewAdapter) resolvePolicy(projectID string) retention.Policy {
	policy := retention.Policy{
		ProjectID:        projectID,
		TaskLLMUsageDays: a.defaults.TaskLLMUsageDays,
		ToolAuditDays:    a.defaults.ToolAuditDays,
		TasksDays:        a.defaults.TasksDays,
		ExecutionsDays:   a.defaults.ExecutionsDays,
		ArtifactsDays:    a.defaults.ArtifactsDays,
		TaskMessagesDays: a.defaults.TaskMessagesDays,
		MemoryChunksDays: a.defaults.MemoryChunksDays,
		ArtifactsRoot:    a.artifactsRoot,
	}
	if a.registry == nil {
		return policy
	}
	p := a.registry.GetProject(projectID)
	if p == nil {
		return policy
	}
	pr := p.Retention
	if pr.TaskLLMUsageDays > 0 {
		policy.TaskLLMUsageDays = pr.TaskLLMUsageDays
	}
	if pr.ToolAuditDays > 0 {
		policy.ToolAuditDays = pr.ToolAuditDays
	}
	if pr.TasksDays > 0 {
		policy.TasksDays = pr.TasksDays
	}
	if pr.ExecutionsDays > 0 {
		policy.ExecutionsDays = pr.ExecutionsDays
	}
	if pr.ArtifactsDays > 0 {
		policy.ArtifactsDays = pr.ArtifactsDays
	}
	if pr.TaskMessagesDays > 0 {
		policy.TaskMessagesDays = pr.TaskMessagesDays
	}
	if pr.MemoryChunksDays > 0 {
		policy.MemoryChunksDays = pr.MemoryChunksDays
	}
	return policy
}
