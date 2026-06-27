package projectwizard

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// ProjectWriter is the narrow interface the Wizard uses to land a
// committed proposal on disk. Production wires an adapter around
// the existing project-ingestion pipeline (filesystem write into
// configsDir + registry hot-reload); tests inject an in-memory
// recorder.
//
// The writer is responsible for refusing collisions: if a project
// with the same ID already exists, return a non-nil error. The
// Wizard surfaces that to the operator without committing the
// session.
type ProjectWriter interface {
	// Write persists a new project from the wizard's YAML output.
	// projectID is the parsed projectId field; yaml is the
	// fully-marshalled project.yaml body. Returns the operator-
	// facing URL to redirect to on success (typically
	// "/ui/projects/<id>").
	Write(ctx context.Context, projectID string, yaml []byte) (string, error)
}

// MultiFileProjectWriter is the optional extension a ProjectWriter
// implements when it can land a whole rendered file set (project
// YAML + swarm.md + any other template files) in one collision-
// refusing write. The template-anchored commit path uses it so a
// wizard project is materialised exactly like a gallery one. A
// writer that implements only ProjectWriter still works — the commit
// falls back to writing the proposal's own YAML as a single file.
type MultiFileProjectWriter interface {
	// WriteFiles writes every (target → body) entry below the configs
	// root, refusing if any target already exists, and returns the
	// operator-facing redirect URL for the new project. Targets are
	// relative paths the renderer produced (e.g. "projects/x.yaml",
	// "swarms/x-swarm.md").
	WriteFiles(ctx context.Context, projectID string, files map[string]string) (string, error)
}

// CommitResult is the wizard service's return value on a
// successful commit. The URL is what the UI redirects to.
type CommitResult struct {
	SessionID string `json:"session_id"`
	ProjectID string `json:"project_id"`
	URL       string `json:"url"`
}

// ErrNotReadyToCommit — the session's most recent envelope has
// ready_to_commit=false. The commit handler refuses; the operator
// either keeps chatting until the wizard signals ready, or
// abandons via the "Edit YAML" escape hatch.
var ErrNotReadyToCommit = errors.New("projectwizard: session not ready to commit")

// ErrNoProposal — the session has no proposal yet. The very first
// turn typically doesn't produce one; the commit endpoint
// short-circuits with this when called too early.
var ErrNoProposal = errors.New("projectwizard: session has no proposal")

// ErrWriterUnwired — the wizard wasn't constructed with a
// ProjectWriter. Handler surfaces as 503.
var ErrWriterUnwired = errors.New("projectwizard: project writer not wired")

// Commit takes a ready-to-commit session, re-validates the
// proposal (defence in depth), writes it via the ProjectWriter,
// and stamps the session terminal. Returns the new project's ID
// + redirect URL.
//
// Re-validation is intentional: between the last /converse turn
// and the operator clicking commit, the daemon's registry might
// have been mutated by a parallel operator, OR the wizard's
// validator might have been upgraded. Both windows are tiny, but
// a stale ready_to_commit is the kind of corner case that ruins
// the operator's day if missed.
func (w *Wizard) Commit(ctx context.Context, sessionID, operatorID string) (*CommitResult, error) {
	if w == nil || w.Sessions == nil {
		return nil, errors.New("projectwizard: not fully wired")
	}
	if w.Writer == nil {
		return nil, ErrWriterUnwired
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("projectwizard: session id required")
	}
	if strings.TrimSpace(operatorID) == "" {
		return nil, errors.New("projectwizard: operator id required")
	}

	session, err := w.Sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("projectwizard: load session: %w", err)
	}
	if session == nil {
		return nil, persistence.ErrNotFound
	}
	if session.OperatorID != operatorID {
		// Cross-operator commit attempt — treat as not-found so
		// the response shape doesn't leak the existence of another
		// operator's session.
		return nil, persistence.ErrNotFound
	}
	if session.CommittedProjectID != nil {
		// Idempotent re-click: return the existing project's URL
		// so the UI redirect lands cleanly.
		return &CommitResult{
			SessionID: session.ID,
			ProjectID: *session.CommittedProjectID,
			URL:       "/ui/projects/" + *session.CommittedProjectID,
		}, nil
	}
	if !session.ReadyToCommit {
		return nil, ErrNotReadyToCommit
	}
	if len(session.CurrentProposal) == 0 {
		return nil, ErrNoProposal
	}

	var proposal ProjectYAML
	if err := proposal.UnmarshalJSON(session.CurrentProposal); err != nil {
		return nil, fmt.Errorf("projectwizard: decode proposal: %w", err)
	}

	slug := session.SuggestedTemplate

	// Defence in depth — re-validate before writing, the same way the
	// proposal was gated for ready_to_commit (template-anchored when a
	// template resolves; raw-proposal otherwise).
	if err := w.validateProposal(&proposal, slug); err != nil {
		return nil, fmt.Errorf("projectwizard: re-validation failed: %w", err)
	}

	projectID := ProposalProjectID(&proposal)
	if projectID == "" {
		return nil, errors.New("projectwizard: proposal missing projectId after validation (validator regression?)")
	}
	if !isSafeProjectID(projectID) {
		return nil, fmt.Errorf("projectwizard: invalid projectId %q: use only letters, digits, '-' and '_'", projectID)
	}

	url, err := w.writeProject(ctx, projectID, &proposal, slug)
	if err != nil {
		w.Metrics.recordCommit(commitOutcomeFailed)
		return nil, fmt.Errorf("projectwizard: write project: %w", err)
	}

	if err := w.Sessions.CommitTo(ctx, session.ID, projectID); err != nil {
		w.Metrics.recordCommit(commitOutcomeCreated)
		// Project file was written but the session-stamp failed.
		// Project still works (it's on disk); the operator can
		// click commit again and the idempotent branch above
		// returns the same URL. Don't unwind the write.
		return &CommitResult{
			SessionID: session.ID,
			ProjectID: projectID,
			URL:       url,
		}, fmt.Errorf("projectwizard: stamp session: %w (project was created)", err)
	}

	w.Metrics.recordCommit(commitOutcomeCreated)
	return &CommitResult{
		SessionID: session.ID,
		ProjectID: projectID,
		URL:       url,
	}, nil
}

// Cancel terminally cancels an in-progress session, freeing the
// operator's active-session slot (the cap counts only uncommitted,
// un-cancelled rows). Mirrors Commit's opening ownership / state
// checks. Refuses a committed session with ErrSessionCommitted;
// cancelling an already-cancelled session is an idempotent success
// (the repo's Cancel returns nil in that case).
func (w *Wizard) Cancel(ctx context.Context, sessionID, operatorID string) error {
	if w == nil || w.Sessions == nil {
		return errors.New("projectwizard: not fully wired")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("projectwizard: session id required")
	}
	if strings.TrimSpace(operatorID) == "" {
		return errors.New("projectwizard: operator id required")
	}

	session, err := w.Sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("projectwizard: load session: %w", err)
	}
	if session == nil {
		return persistence.ErrNotFound
	}
	if session.OperatorID != operatorID {
		// Cross-operator cancel attempt — treat as not-found so the
		// response shape doesn't leak another operator's session.
		return persistence.ErrNotFound
	}
	if session.CommittedProjectID != nil {
		return ErrSessionCommitted
	}

	if err := w.Sessions.Cancel(ctx, sessionID, operatorID); err != nil {
		return err
	}
	w.Metrics.recordCommit(commitOutcomeCancelled)
	return nil
}

// writeProject lands the committed project on disk. When a template
// resolves and the writer supports multi-file writes, the project is
// materialised from the template (project.yaml + swarm.md + …) with
// parameters derived from the proposal — identical to the gallery's
// create path, so the result is guaranteed to load and run. Without
// a resolvable template (or a multi-file writer), it falls back to
// writing the proposal's own YAML as a single project file.
func (w *Wizard) writeProject(ctx context.Context, projectID string, proposal *ProjectYAML, templateSlug string) (string, error) {
	if w.Templates != nil && templateSlug != "" {
		if spec, ok := w.Templates.Lookup(templateSlug); ok {
			if mw, isMulti := w.Writer.(MultiFileProjectWriter); isMulti {
				params := deriveTemplateParams(spec, proposal.Raw)
				files, err := w.Templates.Materialise(templateSlug, params)
				if err != nil {
					return "", fmt.Errorf("materialise template %q: %w", templateSlug, err)
				}
				return mw.WriteFiles(ctx, projectID, files)
			}
		}
	}
	yamlBody, err := RenderYAML(proposal)
	if err != nil {
		return "", fmt.Errorf("render yaml: %w", err)
	}
	return w.Writer.Write(ctx, projectID, yamlBody)
}

func isSafeProjectID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}
