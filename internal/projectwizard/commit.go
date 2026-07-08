package projectwizard

import (
	"context"
	"encoding/json"
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
	// "/ui/projects/<id>/setup").
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

// ScaffoldProposer files the wizard's composed file set as a DRAFT
// control-plane scaffold proposal instead of writing it to disk. When wired
// (the production control-plane path), Commit routes through it: the operator
// reviews the diff, approves, and the gated apply engine creates the files
// atomically with rollback — so a generated project is reviewable-as-a-diff
// and rollbackable rather than direct-committed. Optional: a deployment
// without the proposal ledger (minimal CE) leaves it nil and Commit falls
// back to the direct-write ProjectWriter.
type ScaffoldProposer interface {
	// ProposeScaffold files a DRAFT scaffold proposal carrying the rendered
	// file set (keys are configs-root-relative paths, e.g. "projects/x.yaml",
	// "swarms/x-swarm.md") as create-ops. It does NOT write the files.
	// Returns the filed proposal's id and the operator-facing URL to review
	// it. A project-id collision (a create-op target already exists) is
	// surfaced as an error containing "already exists" so the commit handler
	// maps it to the same PROJECT_EXISTS response as the direct-write path.
	ProposeScaffold(ctx context.Context, projectID string, files map[string]string) (proposalID, url string, err error)
}

// CommitResult is the wizard service's return value on a
// successful commit. The URL is what the UI redirects to.
//
// On the ledger-gated path (ScaffoldProposer wired) no project exists yet:
// ProjectID is the pending project's id, ProposalID is the filed DRAFT, and
// URL points at the control-plane proposal review page. On the direct-write
// path ProposalID is empty and URL is the new project's setup page.
type CommitResult struct {
	SessionID  string `json:"session_id"`
	ProjectID  string `json:"project_id"`
	ProposalID string `json:"proposal_id,omitempty"`
	URL        string `json:"url"`
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
	// A commit needs SOME sink: the ledger proposer (production control-plane
	// path) or the direct-write ProjectWriter (minimal CE fallback).
	if w.Writer == nil && w.Proposer == nil {
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
		// Idempotent re-click. On the ledger path the project doesn't exist
		// yet (a DRAFT proposal is pending review), so its setup URL would
		// 404 — send the operator to the proposals review list instead. On
		// the direct-write path the project is live: return its setup URL.
		if w.Proposer != nil {
			return &CommitResult{
				SessionID: session.ID,
				ProjectID: *session.CommittedProjectID,
				URL:       controlPlaneProposalsURL,
			}, nil
		}
		return &CommitResult{
			SessionID: session.ID,
			ProjectID: *session.CommittedProjectID,
			URL:       "/ui/projects/" + *session.CommittedProjectID + "/setup",
		}, nil
	}
	if !session.ReadyToCommit {
		return nil, ErrNotReadyToCommit
	}

	// Wizard v2 sessions carry a structured composition (persisted by
	// Converse once a turn composes cleanly); everything else is the
	// legacy raw-proposal path. Split into two helpers — inlining both
	// here nests deep enough to bury the shared tail (session stamp +
	// metrics) below two independent validation ladders.
	// Render the project's file set (composition re-composes; legacy decodes
	// + re-validates the proposal). Neither path writes to disk here — the
	// shared land() step routes the files to the proposer or the writer.
	var (
		projectID string
		files     map[string]string
		commitErr error
	)
	if len(session.Composition) > 0 {
		projectID, files, commitErr = w.commitComposition(ctx, session.Composition)
	} else {
		projectID, files, commitErr = w.commitProposal(ctx, session)
	}
	if commitErr != nil {
		return nil, commitErr
	}

	url, proposalID, landErr := w.land(ctx, projectID, files)
	if landErr != nil {
		w.Metrics.recordCommit(commitOutcomeFailed)
		return nil, fmt.Errorf("projectwizard: %s project: %w", landVerb(w), landErr)
	}

	if err := w.Sessions.CommitTo(ctx, session.ID, projectID); err != nil {
		w.Metrics.recordCommit(commitOutcomeCreated)
		// The files were landed (proposal filed, or project written) but the
		// session-stamp failed. The work stands; the operator can click
		// commit again and the idempotent branch above returns a clean URL.
		// Don't unwind.
		return &CommitResult{
			SessionID:  session.ID,
			ProjectID:  projectID,
			ProposalID: proposalID,
			URL:        url,
		}, fmt.Errorf("projectwizard: stamp session: %w (commit landed)", err)
	}

	w.Metrics.recordCommit(commitOutcomeCreated)
	return &CommitResult{
		SessionID:  session.ID,
		ProjectID:  projectID,
		ProposalID: proposalID,
		URL:        url,
	}, nil
}

// controlPlaneProposalsURL is where the operator reviews a filed scaffold
// proposal (the control-plane hub's Proposals tab). The first-commit response
// deep-links to the specific proposal; the idempotent re-click falls back
// here since the proposal id isn't persisted on the session.
const controlPlaneProposalsURL = "/ui/admin/control-plane?section=proposals"

// land routes the rendered file set to its sink: the ledger proposer (files
// become a reviewable DRAFT scaffold proposal) when wired, else the
// direct-write ProjectWriter. Returns the operator-facing URL and, on the
// proposer path, the filed proposal id.
func (w *Wizard) land(ctx context.Context, projectID string, files map[string]string) (url, proposalID string, err error) {
	if len(files) == 0 {
		return "", "", errors.New("projectwizard: no files to commit")
	}
	if w.Proposer != nil {
		proposalID, reviewURL, perr := w.Proposer.ProposeScaffold(ctx, projectID, files)
		return reviewURL, proposalID, perr
	}
	// Direct-write fallback (no ledger). A lone project YAML keeps the
	// single-file Write path (its collision + redirect semantics, unchanged
	// from before the reroute); a multi-file set uses WriteFiles.
	if body, single := soleProjectYAML(files, projectID); single {
		u, werr := w.Writer.Write(ctx, projectID, []byte(body))
		return u, "", werr
	}
	mw, ok := w.Writer.(MultiFileProjectWriter)
	if !ok {
		return "", "", errors.New("projectwizard: single-file writer cannot land a multi-file project")
	}
	u, werr := mw.WriteFiles(ctx, projectID, files)
	return u, "", werr
}

// soleProjectYAML returns (body, true) when files is exactly one entry, the
// project's YAML — the single-file direct-write case.
func soleProjectYAML(files map[string]string, projectID string) (string, bool) {
	if len(files) != 1 {
		return "", false
	}
	body, ok := files[projectYAMLKey(projectID)]
	return body, ok
}

// landVerb labels the commit action for error messages: the ledger path
// "propose"s; the direct-write path "write"s. (The handler matches on
// "already exists" regardless, so this is purely for operator-facing text.)
func landVerb(w *Wizard) string {
	if w.Proposer != nil {
		return "propose"
	}
	return "write"
}

// projectYAMLKey is the configs-root-relative path of a project's YAML file,
// the key both the composition renderer and the single-file fallback use.
func projectYAMLKey(projectID string) string {
	return "projects/" + projectID + ".yaml"
}

// commitComposition re-runs the 3a Compose pipeline against a
// session's persisted wizard v2 composition and writes the result via
// the multi-file writer. Re-composing (rather than trusting a stale
// render) is the same defence-in-depth reason the legacy path
// re-validates: the template catalog or MCP inventory may have moved
// between the last /converse turn and this commit click. Returns the
// new project's ID and redirect URL.
func (w *Wizard) commitComposition(ctx context.Context, rawComposition []byte) (string, map[string]string, error) {
	var comp Composition
	if err := json.Unmarshal(rawComposition, &comp); err != nil {
		return "", nil, fmt.Errorf("projectwizard: decode composition: %w", err)
	}
	files, _, cerr := w.composeFromEnvelope(ctx, &Envelope{Composition: &comp})
	if cerr != nil {
		w.Metrics.recordCommit(commitOutcomeFailed)
		return "", nil, fmt.Errorf("projectwizard: re-compose failed: %w", cerr)
	}
	projectID := compositionProjectID(&comp)
	if projectID == "" {
		w.Metrics.recordCommit(commitOutcomeFailed)
		return "", nil, errors.New("projectwizard: composition missing projectId param")
	}
	if !isSafeProjectID(projectID) {
		w.Metrics.recordCommit(commitOutcomeFailed)
		return "", nil, fmt.Errorf("projectwizard: invalid projectId %q: use only letters, digits, '-' and '_'", projectID)
	}
	// A composition is inherently multi-file (project YAML + swarm.md). The
	// direct-write fallback needs a multi-file writer; the proposer path
	// handles any file set. Guard here only when there's no proposer.
	if w.Proposer == nil {
		if _, ok := w.Writer.(MultiFileProjectWriter); !ok {
			w.Metrics.recordCommit(commitOutcomeFailed)
			return "", nil, errors.New("projectwizard: writer does not support multi-file writes required to commit a composition")
		}
	}
	return projectID, files, nil
}

// commitProposal is the legacy (pre-wizard-v2) path: decode the
// session's raw proposal, re-validate it, and write it — template-
// anchored when a suggested_template resolves and the writer supports
// multi-file writes, else the proposal's own YAML. Returns the new
// project's ID and redirect URL.
func (w *Wizard) commitProposal(_ context.Context, session *persistence.ProjectWizardSession) (string, map[string]string, error) {
	if len(session.CurrentProposal) == 0 {
		return "", nil, ErrNoProposal
	}

	var proposal ProjectYAML
	if err := proposal.UnmarshalJSON(session.CurrentProposal); err != nil {
		return "", nil, fmt.Errorf("projectwizard: decode proposal: %w", err)
	}

	slug := session.SuggestedTemplate

	// Defence in depth — re-validate before rendering, the same way the
	// proposal was gated for ready_to_commit (template-anchored when a
	// template resolves; raw-proposal otherwise).
	if err := w.validateProposal(&proposal, slug); err != nil {
		return "", nil, fmt.Errorf("projectwizard: re-validation failed: %w", err)
	}

	projectID := ProposalProjectID(&proposal)
	if projectID == "" {
		return "", nil, errors.New("projectwizard: proposal missing projectId after validation (validator regression?)")
	}
	if !isSafeProjectID(projectID) {
		return "", nil, fmt.Errorf("projectwizard: invalid projectId %q: use only letters, digits, '-' and '_'", projectID)
	}

	files, err := w.renderProjectFiles(projectID, &proposal, slug)
	if err != nil {
		w.Metrics.recordCommit(commitOutcomeFailed)
		return "", nil, fmt.Errorf("projectwizard: render project: %w", err)
	}
	return projectID, files, nil
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

// renderProjectFiles produces the committed project's file set (configs-root-
// relative path → body), without writing. When a template resolves it is
// materialised from the template (project.yaml + swarm.md + …) with parameters
// derived from the proposal — identical to the gallery's create path, so the
// result is guaranteed to load and run. Otherwise it falls back to the
// proposal's own YAML as a single project file. The shared land() step then
// routes the files to the proposer or the writer.
//
// The multi-file template path is taken when a template resolves AND a sink
// can accept multiple files — the proposer always can; a direct writer must
// implement MultiFileProjectWriter. When only a single-file writer is wired
// (and no proposer), it falls back to the single-YAML render.
func (w *Wizard) renderProjectFiles(projectID string, proposal *ProjectYAML, templateSlug string) (map[string]string, error) {
	_, multiWriter := w.Writer.(MultiFileProjectWriter)
	canMultiFile := w.Proposer != nil || multiWriter
	if w.Templates != nil && templateSlug != "" && canMultiFile {
		if spec, ok := w.Templates.Lookup(templateSlug); ok {
			params := deriveTemplateParams(spec, proposal.Raw)
			files, err := w.Templates.Materialise(templateSlug, params)
			if err != nil {
				return nil, fmt.Errorf("materialise template %q: %w", templateSlug, err)
			}
			return files, nil
		}
	}
	yamlBody, err := RenderYAML(proposal)
	if err != nil {
		return nil, fmt.Errorf("render yaml: %w", err)
	}
	return map[string]string{projectYAMLKey(projectID): string(yamlBody)}, nil
}

// compositionProjectID pulls the projectId param out of a wizard v2
// composition. Mirrors ProposalProjectID for the legacy proposal
// path. Returns "" when the param is missing or empty.
func compositionProjectID(c *Composition) string {
	if c == nil {
		return ""
	}
	vals, ok := c.Params["projectId"]
	if !ok || len(vals) == 0 {
		return ""
	}
	return vals[0]
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
