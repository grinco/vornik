package service

import (
	"context"
	"sync"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/sessionstore"
)

// projectResolver maps a GitHub channel SessionID to the vornik
// project ID that owns it. Multi-installation mode wires this to
// the channel's ProjectForSession method so every dispatcher turn
// runs inside the right project's scope; single-installation mode
// returns a constant projectID for every session.
type projectResolver interface {
	ProjectForSession(sessionID string) string
}

// constantProjectResolver implements projectResolver for the
// back-compat single-installation case where every session
// belongs to the same project.
type constantProjectResolver struct {
	projectID string
}

func (c constantProjectResolver) ProjectForSession(_ string) string { return c.projectID }

// githubSessionStore implements dispatcher.SessionStore for the
// GitHub App channel (slice 4E of the ConversationChannel
// rollout). Each session is one GitHub issue or PR; conversation
// history is held in memory keyed on the channel's SessionID
// (`owner/repo#issues/N` / `owner/repo#pulls/N`). Daemon restart
// clears the history — the persisted authoritative state is the
// comment thread on GitHub itself, so a restart simply means the
// next `@vornik` mention starts a fresh dispatcher turn.
//
// Multi-installation mode: the project that owns a session is
// resolved per-call via the projectResolver, which the channel
// pins on the first inbound delivery. Each session therefore
// stays scoped to its own project for its lifetime — an issue in
// project A never accidentally exposes project B's tools.
//
// Single-installation mode: the resolver is a constant returning
// the one configured project ID — behaviour identical to the
// pre-multi-installation code path.
type githubSessionStore struct {
	registry *registry.Registry
	resolver projectResolver

	mu      sync.Mutex
	history map[string][]chat.Message

	// persister (optional) DB-backs the session. GitHub already
	// has the comment thread as source-of-truth, so DB persistence
	// is mainly to avoid replaying history through the LLM on
	// every rolling restart. Nil = pre-feature behaviour.
	persister *sessionstore.Persister
}

// SetPersister wires the DB-backed persistence layer.
func (s *githubSessionStore) SetPersister(p *sessionstore.Persister) {
	s.persister = p
}

// newGitHubSessionStore constructs the single-installation
// back-compat session store. Every session is pinned to
// projectID; daemon-wide registry powers the lead-prompt + project
// list resolution.
func newGitHubSessionStore(reg *registry.Registry, projectID string) *githubSessionStore {
	return newGitHubSessionStoreWithResolver(reg, constantProjectResolver{projectID: projectID})
}

// newGitHubSessionStoreWithResolver is the multi-installation
// constructor — wire the channel's ProjectForSession method as
// the resolver so each session's dispatcher turn runs inside the
// project that originally received its first event.
func newGitHubSessionStoreWithResolver(reg *registry.Registry, resolver projectResolver) *githubSessionStore {
	if resolver == nil {
		resolver = constantProjectResolver{}
	}
	return &githubSessionStore{
		registry: reg,
		resolver: resolver,
		history:  make(map[string][]chat.Message),
	}
}

// Load returns the per-session conversation snapshot the
// dispatcher consumes. History is copied (not aliased) so a
// concurrent Append on a different SessionID doesn't race the
// dispatcher's read of this Session's slice.
func (s *githubSessionStore) Load(ctx context.Context, msg conversation.ChannelMessage) (dispatcher.Session, error) {
	s.mu.Lock()
	history := append([]chat.Message(nil), s.history[msg.SessionID]...)
	s.mu.Unlock()

	if len(history) == 0 && s.persister != nil {
		if persisted, _, found, err := s.persister.Load(ctx, msg.SessionID); err == nil && found && len(persisted) > 0 {
			history = persisted
			s.mu.Lock()
			s.history[msg.SessionID] = append([]chat.Message(nil), persisted...)
			s.mu.Unlock()
		}
	}

	projectID := s.resolver.ProjectForSession(msg.SessionID)
	sess := dispatcher.Session{
		History:       history,
		ActiveProject: projectID,
	}
	if s.registry == nil || projectID == "" {
		return sess, nil
	}

	project := s.registry.GetProject(projectID)
	if project == nil {
		return sess, nil
	}
	sess.AvailableProjects = s.registry.ListProjects()
	// GitHub session is pinned to one project — restrict the
	// dispatcher's project-scoped tools to that project so an
	// `@vornik` mention can't accidentally route create_task into
	// an unrelated project.
	sess.AllowedProjects = []string{projectID}

	if leadPrompt, _ := dispatcher.ResolveLeadPrompt(s.registry, projectID); leadPrompt != "" {
		swarm := s.registry.GetSwarm(project.SwarmID)
		sess.LeadSystemPrompt = dispatcher.BuildLeadSystemPrompt(project, swarm, leadPrompt, sess.AvailableProjects)
	}
	return sess, nil
}

// Append replaces the session's history with the dispatcher's
// post-turn Messages slice (which already includes the user turn
// and the assistant reply / tool turns). Replacing rather than
// appending mirrors how dispatcher.Result is intended to be
// consumed — `Result.Messages contains the full updated
// conversation history` per its docstring.
func (s *githubSessionStore) Append(ctx context.Context, msg conversation.ChannelMessage, result dispatcher.Result) error {
	if len(result.Messages) == 0 {
		// Defensive: an empty post-turn result would clear history.
		// Skip rather than wipe state on a dispatcher error path.
		return nil
	}
	s.mu.Lock()
	updated := append([]chat.Message(nil), result.Messages...)
	s.history[msg.SessionID] = updated
	s.mu.Unlock()

	if s.persister != nil {
		projectID := s.resolver.ProjectForSession(msg.SessionID)
		_ = s.persister.Save(ctx, msg.SessionID, projectID, updated)
	}
	return nil
}

// snapshotHistory returns a copy of the stored history for a
// session. Test seam; production code reads through Load.
func (s *githubSessionStore) snapshotHistory(sessionID string) []chat.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]chat.Message(nil), s.history[sessionID]...)
}

// Compile-time guard: githubSessionStore satisfies the
// dispatcher SessionStore contract.
var _ dispatcher.SessionStore = (*githubSessionStore)(nil)
