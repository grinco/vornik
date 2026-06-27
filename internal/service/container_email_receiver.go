package service

import (
	"context"
	"io"
	"sync"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/sessionstore"
)

// emailFileSender is the per-thread dispatcher.FileSender adapter for email.
// It binds the email channel + the thread root so the dispatcher's file tools
// (send_artifact, render_document) deliver a file as a threaded email
// attachment — the email realisation of the channel-agnostic file-delivery
// seam.
type emailFileSender struct {
	ch        *email.Channel
	sessionID string
}

func (e emailFileSender) SendArtifactFile(ctx context.Context, fileName string, content io.Reader, caption string) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	_, err = e.ch.SendFile(ctx, e.sessionID, fileName, data, caption)
	return err
}

// emailSessionStore implements dispatcher.SessionStore for the
// email ConversationChannel (slice 3 of the email rollout — the
// slice-2 work landed the channel itself, this layer plugs it
// into the dispatcher). Mirrors githubSessionStore's shape: one
// in-memory history map keyed on the channel's SessionID (the
// email thread root) plus a constant project resolver so every
// session stays inside the project that owns this channel.
//
// Multi-project routing: one emailSessionStore per channel, each
// pinned to its own project. The container's lifecycle code in
// container.go constructs one store-and-receiver pair per
// EmailChannels[i].
//
// Daemon restart clears the in-memory history; the persisted
// authoritative state is the IMAP mailbox itself. Subsequent
// inbound mail on the same thread starts a fresh dispatcher turn
// — operators rely on the email subject line + the thread root
// to carry context across restarts, not in-process state.
type emailSessionStore struct {
	registry  *registry.Registry
	projectID string
	// channel is the email channel this store's sessions reply through.
	// Used to build the per-thread FileSender so send_artifact can deliver
	// files as email attachments. Nil disables the file tools.
	channel *email.Channel

	mu      sync.Mutex
	history map[string][]chat.Message

	// persister (optional) write-through-persists session state
	// to Postgres so a daemon restart or replica failover doesn't
	// drop conversations. Nil keeps the pre-feature in-memory-only
	// behaviour for tests + opt-out deployments.
	persister *sessionstore.Persister
}

// SetPersister wires the DB-backed persistence layer. Channel
// kind is "email"; the (kind, session_id) composite PK keeps
// thread-root message-ids isolated from other channels' ids.
func (s *emailSessionStore) SetPersister(p *sessionstore.Persister) {
	s.persister = p
}

// newEmailSessionStore constructs a per-channel session store
// pinned to the supplied project. projectID may be empty in
// degenerate test wiring; production always supplies one because
// the channel is one-project-per-instance.
func newEmailSessionStore(reg *registry.Registry, projectID string, ch *email.Channel) *emailSessionStore {
	return &emailSessionStore{
		registry:  reg,
		projectID: projectID,
		channel:   ch,
		history:   make(map[string][]chat.Message),
	}
}

// Load returns the per-session conversation snapshot for the
// dispatcher. History is copied (not aliased) so a concurrent
// Append on a different SessionID doesn't race the dispatcher.
func (s *emailSessionStore) Load(ctx context.Context, msg conversation.ChannelMessage) (dispatcher.Session, error) {
	s.mu.Lock()
	history := append([]chat.Message(nil), s.history[msg.SessionID]...)
	s.mu.Unlock()

	// Cache miss → fall back to DB (replica failover / daemon restart).
	if len(history) == 0 && s.persister != nil {
		if persisted, _, found, err := s.persister.Load(ctx, msg.SessionID); err == nil && found && len(persisted) > 0 {
			history = persisted
			s.mu.Lock()
			s.history[msg.SessionID] = append([]chat.Message(nil), persisted...)
			s.mu.Unlock()
		}
	}

	sess := dispatcher.Session{
		History:       history,
		ActiveProject: s.projectID,
	}
	// Bind the per-thread file sender so send_artifact / render_document can
	// deliver files as threaded email attachments on this conversation.
	if s.channel != nil {
		sess.FileSender = emailFileSender{ch: s.channel, sessionID: msg.SessionID}
	}
	if s.registry == nil || s.projectID == "" {
		return sess, nil
	}
	project := s.registry.GetProject(s.projectID)
	if project == nil {
		return sess, nil
	}
	sess.AvailableProjects = s.registry.ListProjects()
	// Email channel is one-project-per-channel; restrict the
	// dispatcher's project-scoped tools to that project so an
	// inbound message can't route create_task into an unrelated
	// project from another email channel.
	sess.AllowedProjects = []string{s.projectID}

	if leadPrompt, _ := dispatcher.ResolveLeadPrompt(s.registry, s.projectID); leadPrompt != "" {
		swarm := s.registry.GetSwarm(project.SwarmID)
		sess.LeadSystemPrompt = dispatcher.BuildLeadSystemPrompt(project, swarm, leadPrompt, sess.AvailableProjects)
	}
	return sess, nil
}

// Append replaces the session's history with the dispatcher's
// post-turn Messages slice. Mirrors githubSessionStore.Append's
// reasoning — Result.Messages is documented as "full updated
// conversation history" so a replace is the right operation.
func (s *emailSessionStore) Append(ctx context.Context, msg conversation.ChannelMessage, result dispatcher.Result) error {
	if len(result.Messages) == 0 {
		// Defensive: empty post-turn result would clear history.
		// Skip rather than wipe state on a dispatcher error path.
		return nil
	}
	s.mu.Lock()
	updated := append([]chat.Message(nil), result.Messages...)
	s.history[msg.SessionID] = updated
	s.mu.Unlock()

	if s.persister != nil {
		_ = s.persister.Save(ctx, msg.SessionID, s.projectID, updated)
	}
	return nil
}

// snapshotHistory returns a copy of the stored history for a
// session. Test seam; production code reads through Load.
func (s *emailSessionStore) snapshotHistory(sessionID string) []chat.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]chat.Message(nil), s.history[sessionID]...)
}

// Compile-time guard: emailSessionStore satisfies the dispatcher
// SessionStore contract.
var _ dispatcher.SessionStore = (*emailSessionStore)(nil)
