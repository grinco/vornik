package ui

// Per-project web chat surface. GET /ui/projects/<id>/chat
// renders the page; POST /ui/projects/<id>/chat/messages
// dispatches one turn synchronously and re-renders with the
// reply appended.
//
// Session strategy: a UUID cookie ("vornik_chat_session") names
// the browser session; the in-memory webchat.SessionStore keeps
// per-cookie history for the daemon's lifetime. Daemon restart
// clears history — operators re-prompt, matching the GitHub App
// channel's contract.
//
// Streaming: out of scope for this slice. Each POST blocks the
// HTTP response until the dispatcher returns the final reply.
// Adding SSE later is additive — implement
// conversation.StreamingChannel on *webchat.Channel and the
// existing dispatcher.ChannelReceiver picks it up.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/sessionstore"
	"vornik.io/vornik/internal/webchat"
)

// chatSessionCookie names the cookie that ties a browser to its
// per-project chat history. The value is a random hex string; the
// daemon does NOT bind the cookie to an authenticated identity, so
// two browsers sharing an API key get independent chat sessions.
const chatSessionCookie = "vornik_chat_session"

var chatRandRead = rand.Read

// ChatDispatcher is the narrow dispatcher contract the chat UI
// handler depends on. *dispatcher.Agent implements Process /
// ProcessStreaming and therefore satisfies dispatcher.Doer;
// re-exporting that interface here lets the UI wire to it without
// importing dispatcher transitively in every test build.
type ChatDispatcher = dispatcher.Doer

// chatDispatchTimeout caps how long the UI waits for one chat
// turn to land. Mirrors the Telegram bot's default DispatchTimeout
// (5 minutes) — short enough that a stalled model fails visibly,
// long enough for the curated lead-model defaults.
const chatDispatchTimeout = 5 * time.Minute

// WithChatDispatcher wires the dispatcher backing the per-project
// chat surface. nil is allowed — the chat page then renders a
// "chat not configured" banner instead of returning 500.
func WithChatDispatcher(d ChatDispatcher) ServerOption {
	return func(s *Server) { s.chatDispatcher = d }
}

// WithChatSessionPersister wires the DB-backed persistence layer
// for every webchat SessionStore the UI lazily constructs. Nil
// keeps the pre-feature in-memory-only behaviour — a rolling
// restart drops the in-flight chat history, replicas can't pick
// each other's conversations up. Production wires this off
// c.repos.ChannelSessions.
func WithChatSessionPersister(p *sessionstore.Persister) ServerOption {
	return func(s *Server) { s.chatSessionPersister = p }
}

// WithChatContextBudget sets the token-window budget the webchat
// SessionStore uses to compute the per-turn context-budget tier.
// Zero (the default) leaves the tier surface disabled — no badge in
// the chat panel, no behavioural change to the dispatcher (deferred
// tool loading still kicks in once the catalog grows past its own
// threshold). Operators with a configured model context size pin
// this to that size so the four-tier signal is meaningful.
func WithChatContextBudget(tokens int) ServerOption {
	return func(s *Server) {
		if tokens > 0 {
			s.chatContextBudget = tokens
		}
	}
}

// WithWebUIBaseURL stamps the daemon's externally-reachable base
// URL on the UI server. Used by the chat surface to render
// deliverable links in assistant replies. Empty falls back to a
// relative-path rendering — clickable for browser users in the
// same origin; not portable outside.
func WithWebUIBaseURL(url string) ServerOption {
	return func(s *Server) { s.webUIBaseURL = strings.TrimRight(url, "/") }
}

// ChatPageData backs the chat template render. Title + CurrentPage
// mirror every other UI page so the shared nav template renders
// the right active link.
type ChatPageData struct {
	Title       string
	CurrentPage string

	ProjectID   string
	ProjectName string

	// History is the conversation so far, oldest-first. Each entry
	// has a Role ("user" / "assistant") and a Content string the
	// template renders with renderMarkdown.
	History []chat.Message

	// Error, when non-empty, renders an inline banner above the
	// composer. Used for both dispatcher errors and missing
	// configuration (chat-dispatcher unset).
	Error string

	// Disabled is true when no dispatcher is wired; the composer
	// is grayed out and the banner explains the deployment is
	// chat-disabled.
	Disabled bool

	// SessionID surfaces the cookie value to the template so a
	// hidden form input round-trips it (defence-in-depth: if a
	// browser drops the cookie mid-session, the form post still
	// names the right history bucket).
	SessionID string

	// ContextTier is the lowercase tier name ("peak" / "good" /
	// "degrading" / "poor") for this session at the moment the
	// page rendered. Empty when the deployment hasn't configured a
	// chatContextBudget — the template hides the badge in that
	// case so a "no signal" deployment matches the legacy chat
	// surface byte-for-byte.
	ContextTier string

	// ContextHeadroomPct is the remaining-budget percentage [0,
	// 100] paired with ContextTier. Surfaced in the badge tooltip
	// so the operator can read the exact number rather than guess
	// from the band's color. Zero when the deployment hasn't
	// configured a budget — same hide-the-badge contract as
	// ContextTier.
	ContextHeadroomPct int
}

// ChatPage handles GET /ui/projects/<id>/chat. Ensures the
// session cookie is set, loads the per-session history, and
// renders the page.
func (s *Server) ChatPage(w http.ResponseWriter, r *http.Request, projectID string) {
	data := s.chatPageData(r, projectID)
	s.ensureChatCookie(w, r, &data)
	s.render(w, "chat.html", data)
}

// ChatPostMessage handles POST /ui/projects/<id>/chat/messages.
// Reads the operator's prompt from the form, dispatches a single
// turn synchronously, and re-renders the chat page with the
// updated history.
func (s *Server) ChatPostMessage(w http.ResponseWriter, r *http.Request, projectID string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	data := s.chatPageData(r, projectID)
	s.ensureChatCookie(w, r, &data)

	if data.Disabled {
		// No dispatcher — explain and re-render. Don't pretend
		// the message landed.
		s.render(w, "chat.html", data)
		return
	}
	if prompt == "" {
		data.Error = "Prompt is empty — type something to send."
		s.render(w, "chat.html", data)
		return
	}
	if data.ProjectID == "" || data.ProjectName == "" && s.projectReg != nil && s.projectReg.GetProject(projectID) == nil {
		data.Error = "Project not found in the registry."
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "chat.html", data)
		return
	}

	store := s.chatStoreFor(projectID)
	channel := webchat.New(projectID, conversation.Speaker{
		ID:          "web-chat:" + data.SessionID,
		DisplayName: "Web operator",
	})
	receiver := &dispatcher.ChannelReceiver{
		Channel:  channel,
		Agent:    s.chatDispatcher,
		Sessions: store,
		ResultPostprocessor: func(result dispatcher.Result) string {
			// Append the deliverable-links block when the
			// dispatcher's turn referenced a task that produced
			// files. The dispatcher doesn't currently propagate
			// produced_files through the Result — we read off the
			// artifact repo via the executor's existing telegram
			// notify path. For the synchronous web chat turn,
			// produced files belong to a separate task surface
			// (the dispatcher submits work asynchronously). When
			// the future event-driven webchat lifecycle lands, the
			// task-complete deliverable surfacing will share the
			// telegram path; for now, the postprocessor is a
			// passthrough that future code can extend without
			// touching the receiver wiring.
			return result.Text
		},
	}

	ctx, cancel := context.WithTimeout(r.Context(), chatDispatchTimeout)
	defer cancel()

	inbound := conversation.ChannelMessage{
		Source:    channel.Name(),
		SessionID: data.SessionID,
		SpeakerID: data.SessionID,
		Text:      prompt,
		Timestamp: time.Now(),
	}
	if err := receiver.Receive(ctx, inbound); err != nil {
		data.Error = "Chat dispatch failed: " + err.Error()
	}
	// Refresh history from the store so the rendered page shows
	// the user's new prompt + the assistant reply.
	data.History = store.History(data.SessionID)
	s.render(w, "chat.html", data)
}

// chatPageData builds the render struct with project info +
// history. Honoured both on GET and on the POST re-render.
func (s *Server) chatPageData(r *http.Request, projectID string) ChatPageData {
	data := ChatPageData{
		Title:       "Chat: " + projectID,
		CurrentPage: "projects",
		ProjectID:   projectID,
	}
	if s.chatDispatcher == nil {
		data.Disabled = true
		data.Error = "Chat is not configured on this deployment (no chat dispatcher wired)."
	}
	if s.projectReg != nil {
		if p := s.projectReg.GetProject(projectID); p != nil {
			data.ProjectName = p.DisplayName
			if data.ProjectName == "" {
				data.ProjectName = p.ID
			}
		}
	}
	cookie, err := r.Cookie(chatSessionCookie)
	if err == nil && cookie.Value != "" {
		data.SessionID = cookie.Value
		store := s.chatStoreFor(projectID)
		data.History = store.History(data.SessionID)
	}
	// Tier badge: surface the current band + exact headroom for
	// the rendered history. Only fires when the deployment
	// configured a budget — otherwise the template hides the
	// badge and the chat surface matches the legacy byte-for-byte.
	if s.chatContextBudget > 0 {
		used := chatEstimateTokens(data.History)
		tier := chat.TierFromUsage(used, s.chatContextBudget)
		data.ContextTier = tier.String()
		data.ContextHeadroomPct = int(chat.HeadroomPct(used, s.chatContextBudget))
	}
	return data
}

// chatEstimateTokens is the UI-side mirror of webchat's
// estimateTokens helper — same chars/4 heuristic. Pulled here so the
// chat page renderer doesn't need to import the webchat package's
// internal helper (which would couple the package's public API to a
// private estimator detail).
func chatEstimateTokens(msgs []chat.Message) int {
	const charsPerTokenEstimate = 4
	var n int
	for _, m := range msgs {
		n += len(m.Content) / charsPerTokenEstimate
	}
	return n
}

// ensureChatCookie sets the session cookie on the response when
// the request didn't carry one. Idempotent — repeated calls with
// the same request reuse the existing cookie value.
//
// HttpOnly: prevents JS-side fingerprinting / theft. SameSite=Lax:
// blocks cross-site cookie forwarding to limit CSRF surface; the
// POST handler is same-origin in practice. Secure flag NOT set —
// production deployments terminate TLS at a reverse proxy; the
// daemon doesn't know whether the incoming request was upgraded.
// The proxy is responsible for stripping the cookie on non-HTTPS
// flows.
func (s *Server) ensureChatCookie(w http.ResponseWriter, r *http.Request, data *ChatPageData) {
	if data.SessionID != "" {
		return
	}
	id := newChatSessionID()
	if id == "" {
		data.Error = "Chat session could not be created securely."
		data.Disabled = true
		return
	}
	data.SessionID = id
	http.SetCookie(w, &http.Cookie{
		Name:     chatSessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// 30 days — long enough for an operator to come back to a
		// half-finished investigation across a weekend; short
		// enough that abandoned sessions eventually clear from
		// the in-memory store on browser side.
		MaxAge: 30 * 24 * 3600,
	})
	_ = r // r reserved for future TLS / forwarded-proto checks.
}

// chatStoreFor returns the per-project SessionStore, allocating it
// on first use. Stores are project-scoped so two browsers on
// different projects can't accidentally read each other's
// history. The deployment-wide chatContextBudget is stamped onto
// each new store so the per-turn tier computation has the budget
// it needs without re-resolving config on every Load.
func (s *Server) chatStoreFor(projectID string) *webchat.SessionStore {
	s.chatStoresMu.Lock()
	defer s.chatStoresMu.Unlock()
	if s.chatStores == nil {
		s.chatStores = map[string]*webchat.SessionStore{}
	}
	store := s.chatStores[projectID]
	if store == nil {
		store = webchat.NewSessionStore(s.projectReg, projectID)
		store.ContextBudget = s.chatContextBudget
		if s.chatSessionPersister != nil {
			store.SetPersister(s.chatSessionPersister)
		}
		s.chatStores[projectID] = store
	}
	return store
}

// newChatSessionID returns a 32-character hex string suitable
// for use as a cookie value. crypto/rand provides the entropy;
// fallback to time-based on rand failure is intentionally absent
// (a working crypto/rand is required for cookie security).
func newChatSessionID() string {
	var b [16]byte
	if _, err := chatRandRead(b[:]); err != nil {
		// Fail closed. A predictable chat-session cookie can expose
		// another browser's in-memory chat history; no fallback has
		// enough entropy when crypto/rand is unavailable.
		return ""
	}
	return hex.EncodeToString(b[:])
}

// renderDeliverableLinksForWebChat is the web-chat counterpart to
// the telegram bot's renderDeliverableLinks helper. Exposed as a
// function (not a method on Server) so unit tests don't need a
// full Server to exercise it. Reads the project's artifact set
// from the supplied repo and renders the "Produced files:" block
// using the daemon's WebUIBaseURL. Currently unused — the
// dispatcher's web-chat reply doesn't carry produced_files
// metadata back to the handler, so the chat surface relies on
// the operator running a follow-up task to see the file.
// Kept here as the integration point for the eventual streaming/
// event-driven wiring.
func renderDeliverableLinksForWebChat(baseURL, projectID string, names []string) string {
	links := conversation.BuildDeliverableLinks(baseURL, projectID, names)
	return conversation.RenderDeliverableLinks(links)
}
