// Package service — slice 4F of the ConversationChannel rollout:
// the github.TaskCreator adapter.
//
// The github channel package owns the inbound webhook handler and
// the TaskCreator interface (see internal/github/channel.go ~L142),
// but it stays decoupled from persistence + registry. This file is
// the service-container adapter that bridges the channel into the
// daemon's task pipeline. It mirrors the conventions of the
// existing webhook task creator (internal/api/webhook_handlers.go
// createWebhookTask) so a label-driven GitHub task and a
// /api/v1/webhooks/{project}/{source} task land as comparable rows:
//
//   - same persistence.Task fields populated (project_id, payload,
//     idempotency_key, status=QUEUED, attempt=1, max_attempts=3)
//   - same persistence.TaskRepository.Create method
//   - same idempotency_key column — `github-app:<X-GitHub-Delivery>`
//     for the GitHub path vs `webhook:<source>:<event_id>` for the
//     generic path. Both flow through GetByIdempotencyKey for the
//     duplicate-delivery short-circuit.
//
// The adapter is constructed by initHTTPServer in container_http.go
// after the GitHub channel itself is built and pinned to a
// project; the channel was already nil-checking cfg.TaskCreator so
// existing single-project deployments without the github_app block
// behave unchanged.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// githubTaskCreatorSource is the payload-embedded source tag used
// to identify GitHub-App-driven tasks downstream (UI filters,
// /tasks?source= search). Matches the design doc's "source key"
// convention.
const githubTaskCreatorSource = "github_app"

// pullRequestReviewTaskType is the fixed task type for opened-PR
// review tasks. The matching swarm workflow lives in the operator
// config; the type string here is the lookup key.
const pullRequestReviewTaskType = "review"

// githubTaskCreator implements github.TaskCreator. It converts a
// TaskCreationEvent into a persistence.Task and inserts it via the
// shared TaskRepository — the scheduler picks up the QUEUED row on
// its next poll, no separate enqueue is required (autonomy follows
// the same pattern — see internal/autonomy/manager.go
// createAutonomousTask).
//
// The struct is constructed once at boot and is safe for concurrent
// use: the only mutable state is taskRepo, which is itself
// thread-safe per the TaskRepository contract.
type githubTaskCreator struct {
	taskRepo  persistence.TaskRepository
	project   *registry.Project
	taskLabel map[string]string // optional label→task_type mapping, empty falls back to label name
	logger    zerolog.Logger
}

// newGitHubTaskCreator builds an adapter pinned to a single
// project. labelMap is optional — when nil/empty, issues.labeled
// tasks default their type to the matched label name.
func newGitHubTaskCreator(
	taskRepo persistence.TaskRepository,
	project *registry.Project,
	labelMap map[string]string,
	logger zerolog.Logger,
) *githubTaskCreator {
	return &githubTaskCreator{
		taskRepo:  taskRepo,
		project:   project,
		taskLabel: labelMap,
		logger:    logger,
	}
}

// Create implements github.TaskCreator. Returns an error when the
// adapter is structurally unusable (nil task repo) or when the
// underlying Create fails for a reason other than a duplicate key
// — duplicates are quietly resolved via GetByIdempotencyKey so a
// retried delivery is a no-op rather than a hard failure.
func (g *githubTaskCreator) Create(ctx context.Context, ev github.TaskCreationEvent) error {
	if g == nil {
		return errors.New("github task creator: nil receiver")
	}
	if g.taskRepo == nil {
		return errors.New("github task creator: task repository is not configured")
	}
	if g.project == nil {
		return errors.New("github task creator: no project pinned to the github channel")
	}

	// Resolve task type first — the kind dispatch decides whether a
	// label-mapping lookup applies. Empty result here means the
	// event is malformed (issues.labeled without a label, etc.) —
	// surface as an error so the channel's Warn log captures the
	// rejected delivery.
	taskType, err := g.resolveTaskType(ev)
	if err != nil {
		return err
	}

	// Idempotency short-circuit: if a row already exists for this
	// delivery, return nil without re-creating. Matches the
	// createWebhookTask convention (api/webhook_handlers.go:226).
	if ev.IdempotencyKey != "" {
		existing, err := g.taskRepo.GetByIdempotencyKey(ctx, g.project.ID, ev.IdempotencyKey)
		if err == nil && existing != nil {
			g.logger.Debug().
				Str("project_id", g.project.ID).
				Str("idempotency_key", ev.IdempotencyKey).
				Str("task_id", existing.ID).
				Msg("github task creator: duplicate delivery — returning existing task")
			return nil
		}
	}

	priority := g.project.DefaultPriority
	// Route per event kind: an opened PR runs the review workflow; an issue runs
	// the issue→change-request router. Both fall back to reply_workflow_id /
	// the project default when not separately configured.
	// See https://docs.vornik.io (Config Surface).
	var workflowID string
	if ev.Kind == "pull_request.opened" {
		workflowID = g.project.GitHubApp.EffectivePRReviewWorkflowID(g.project.DefaultWorkflowID)
	} else {
		workflowID = g.project.GitHubApp.EffectiveReplyWorkflowID(g.project.DefaultWorkflowID)
	}
	payload, err := marshalGitHubTaskPayload(ev, taskType, priority, workflowID)
	if err != nil {
		return fmt.Errorf("marshal github task payload: %w", err)
	}

	now := time.Now()
	idempotencyKey := ev.IdempotencyKey
	task := &persistence.Task{
		ID:             persistence.GenerateID("task"),
		ProjectID:      g.project.ID,
		CreationSource: persistence.TaskCreationSourceUser,
		Status:         persistence.TaskStatusQueued,
		Priority:       priority,
		Payload:        payload,
		Attempt:        1,
		MaxAttempts:    3,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if workflowID != "" {
		wf := workflowID
		task.WorkflowID = &wf
	}
	if idempotencyKey != "" {
		task.IdempotencyKey = &idempotencyKey
	}

	if err := g.taskRepo.Create(ctx, task); err != nil {
		// Duplicate-key races: another delivery slipped in between
		// the lookup above and the insert. Re-resolve and treat as
		// success so GitHub's retry doesn't bounce off this layer.
		if idempotencyKey != "" {
			if existing, getErr := g.taskRepo.GetByIdempotencyKey(ctx, g.project.ID, idempotencyKey); getErr == nil && existing != nil {
				return nil
			}
		}
		return fmt.Errorf("create github task: %w", err)
	}

	g.logger.Info().
		Str("project_id", g.project.ID).
		Str("task_id", task.ID).
		Str("task_type", taskType).
		Str("kind", ev.Kind).
		Str("repo", ev.Repo).
		Int("number", ev.Number).
		Str("idempotency_key", idempotencyKey).
		Msg("github task creator: task created")
	return nil
}

// resolveTaskType maps an event onto a task_type string:
//   - pull_request.opened → fixed "review"
//   - issues.labeled      → labelMap[matched-label] when set,
//     otherwise the first label on the
//     event (the matched label, per the
//     channel's issueLabels helper)
//
// Returns an error when the event carries neither a usable label
// nor a mapping. The channel logs the error; HTTP response stays
// 200 (handled upstream).
func (g *githubTaskCreator) resolveTaskType(ev github.TaskCreationEvent) (string, error) {
	switch ev.Kind {
	case "pull_request.opened":
		return pullRequestReviewTaskType, nil
	case "issues.labeled":
		if len(ev.Labels) == 0 {
			return "", errors.New("issues.labeled event has no labels")
		}
		// The channel puts the matched label first; honour that.
		matched := ev.Labels[0]
		if g.taskLabel != nil {
			if mapped, ok := g.taskLabel[matched]; ok && strings.TrimSpace(mapped) != "" {
				return mapped, nil
			}
		}
		if strings.TrimSpace(matched) == "" {
			return "", errors.New("issues.labeled event has an empty matched label")
		}
		return matched, nil
	default:
		return "", fmt.Errorf("unsupported event kind %q", ev.Kind)
	}
}

// githubTaskPayload is the JSON shape persisted in tasks.payload
// for GitHub-App-driven tasks. The dispatcher / role agents read
// `taskType` + `context.prompt` via the same extractPrompt helper
// every webhook + autonomy path uses, so existing consumers see a
// familiar shape; the `source` + GitHub-specific fields ride
// alongside for future routing / UI filters.
type githubTaskPayload struct {
	TaskType       string            `json:"taskType"`
	Priority       int               `json:"priority,omitempty"`
	WorkflowID     string            `json:"workflowId,omitempty"`
	IdempotencyKey string            `json:"idempotencyKey,omitempty"`
	Source         string            `json:"source"`
	Context        map[string]string `json:"context,omitempty"`
	GitHub         githubPayloadMeta `json:"github"`
	// ForgeJob is the provider-neutral classification the forge.* system steps
	// read (top-level so the string-only Context map stays intact). Lets the
	// deterministic publish/review steps run without parsing free text.
	ForgeJob *forge.ForgeJob `json:"forge_job,omitempty"`
}

// githubPayloadMeta carries the per-delivery context every
// downstream consumer (UI, dispatcher tools, post-mortem) needs to
// link a task back to its origin issue / PR. Mirrors
// github.TaskCreationEvent verbatim — no transformation, just a
// serialisation boundary.
type githubPayloadMeta struct {
	Kind           string   `json:"kind"`
	Repo           string   `json:"repo"`
	Number         int      `json:"number"`
	Labels         []string `json:"labels,omitempty"`
	Title          string   `json:"title,omitempty"`
	Body           string   `json:"body,omitempty"`
	SenderLogin    string   `json:"sender_login,omitempty"`
	InstallationID int64    `json:"installation_id,omitempty"`
	SessionID      string   `json:"session_id,omitempty"`
}

// marshalGitHubTaskPayload serialises an event into the JSON bytes
// stored in tasks.payload. The prompt sentence embedded in
// context.prompt is the canonical "what the agent reads first"
// field — see internal/dispatcher/grounding.go extractPrompt for
// the consumer side.
func marshalGitHubTaskPayload(ev github.TaskCreationEvent, taskType string, priority int, workflowID string) ([]byte, error) {
	prompt := buildGitHubPrompt(ev)
	p := githubTaskPayload{
		TaskType:       taskType,
		Priority:       priority,
		WorkflowID:     workflowID,
		IdempotencyKey: ev.IdempotencyKey,
		Source:         githubTaskCreatorSource,
		Context:        map[string]string{"prompt": prompt},
		GitHub: githubPayloadMeta{
			Kind:           ev.Kind,
			Repo:           ev.Repo,
			Number:         ev.Number,
			Labels:         append([]string(nil), ev.Labels...),
			Title:          ev.Title,
			Body:           ev.Body,
			SenderLogin:    ev.SenderLogin,
			InstallationID: ev.InstallationID,
			SessionID:      ev.SessionID,
		},
		ForgeJob: forgeJobFromEvent(ev),
	}
	return json.Marshal(p)
}

// forgeJobFromEvent builds the provider-neutral ForgeJob the forge.* system
// steps consume from a GitHub TaskCreationEvent. Provider is "github" (this is
// the GitHub channel); Action + IsChangeRequest derive from the event kind.
func forgeJobFromEvent(ev github.TaskCreationEvent) *forge.ForgeJob {
	action := ""
	isCR := false
	switch ev.Kind {
	case "issues.labeled":
		action = "labeled"
	case "pull_request.opened":
		action = "opened"
		isCR = true
	}
	return &forge.ForgeJob{
		Provider:        forge.ProviderGitHub,
		Repo:            ev.Repo,
		Action:          action,
		Number:          ev.Number,
		Labels:          append([]string(nil), ev.Labels...),
		DefaultBranch:   ev.DefaultBranch,
		IsChangeRequest: isCR,
		Title:           ev.Title,
		Body:            ev.Body,
	}
}

// buildGitHubPrompt assembles the operator-facing prompt sentence
// the agent runtime reads via extractPrompt. Keeps the formatting
// minimal (title + body) so downstream role prompts stay in control
// of presentation; the title-only path falls back to a synthetic
// "PR #N" / "issue #N" sentence so an empty-title delivery still
// produces a usable string.
func buildGitHubPrompt(ev github.TaskCreationEvent) string {
	title := strings.TrimSpace(ev.Title)
	body := strings.TrimSpace(ev.Body)
	subject := title
	if subject == "" {
		switch ev.Kind {
		case "pull_request.opened":
			subject = fmt.Sprintf("PR #%d in %s", ev.Number, ev.Repo)
		case "issues.labeled":
			subject = fmt.Sprintf("issue #%d in %s", ev.Number, ev.Repo)
		default:
			subject = fmt.Sprintf("%s #%d in %s", ev.Kind, ev.Number, ev.Repo)
		}
	}
	if body == "" {
		return subject
	}
	return subject + "\n\n" + body
}

// Compile-time guard: githubTaskCreator satisfies the channel's
// TaskCreator contract. Catches API drift on either side at build
// time rather than the first delivery.
var _ github.TaskCreator = (*githubTaskCreator)(nil)
