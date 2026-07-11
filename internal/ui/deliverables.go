// Deliverable-first completion (task 2.4, narrated-execution-design.md
// §5.8) — the final task of the Narrated Execution feature. A COMPLETED
// task leads with its deliverable: the final execution's OUTPUT-class
// artifacts, rendered as cards (name, type label, the existing download/
// preview affordance) ABOVE the technical execution record, plus a
// "Send to chat" action per card that resolves the task's originating
// channel via the SAME internal/chatorigin chain task 2.3's chat push
// uses (chatorigin.Resolve) — so a future channel-resolution change
// updates one place, not two. Falls back to the plain completion
// narration text (the last story line) when the execution produced no
// OUTPUT artifact.
//
// Per the design's own non-goal ("No new artifact rendering engine.
// Deliverable cards elevate the existing artifact chips + preview; new
// previewers are out of scope") the "preview" IS the existing chip
// affordance (icon + name + download link) — this file does not add a
// thumbnail/rendering engine, only a name-derived type label and a
// card layout around the existing download link.

package ui

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"vornik.io/vornik/internal/chatorigin"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// DeliverableCard is the task-detail page's view of one OUTPUT artifact,
// pre-formatted for the template (no filtering/classification logic
// belongs in the template layer).
type DeliverableCard struct {
	ID          string
	Name        string
	TypeLabel   string
	SizeBytes   int64
	DownloadURL string
}

// buildDeliverableCards filters execArtifacts down to the OUTPUT class
// (excluding INPUT/INTERMEDIATE/SNAPSHOT/LOG/METADATA — those aren't the
// deliverable, design §5.8) and maps each to a DeliverableCard, preserving
// the repo's return order. nil entries are skipped defensively.
func buildDeliverableCards(execArtifacts []*persistence.Artifact) []DeliverableCard {
	var cards []DeliverableCard
	for _, a := range execArtifacts {
		if a == nil || a.ArtifactClass != persistence.ArtifactClassOutput {
			continue
		}
		var size int64
		if a.SizeBytes != nil {
			size = *a.SizeBytes
		}
		cards = append(cards, DeliverableCard{
			ID:          a.ID,
			Name:        a.Name,
			TypeLabel:   classifyArtifactType(a.Name, a.MimeType),
			SizeBytes:   size,
			DownloadURL: "/ui/artifacts/" + a.ID,
		})
	}
	return cards
}

// codeExtensions are name suffixes classified as "Code" when the mime
// type doesn't already say so (agents commonly write code artifacts with
// no/generic mime type recorded).
var codeExtensions = []string{
	".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".rb", ".java", ".rs",
	".sh", ".yaml", ".yml", ".json", ".sql", ".c", ".cpp", ".h",
}

// classifyArtifactType returns a short, human-readable type label for a
// deliverable card's badge, derived from the artifact's declared mime
// type (preferred) falling back to its file extension. Never errors —
// an unrecognised combination just returns "File".
func classifyArtifactType(name string, mimeType *string) string {
	mt := ""
	if mimeType != nil {
		mt = strings.ToLower(strings.TrimSpace(*mimeType))
	}
	lowerName := strings.ToLower(name)
	switch {
	case strings.HasPrefix(mt, "image/"):
		return "Image"
	case mt == "application/pdf":
		return "PDF"
	case strings.Contains(mt, "spreadsheet") || strings.HasSuffix(lowerName, ".csv") || strings.HasSuffix(lowerName, ".xlsx"):
		return "Spreadsheet"
	case strings.Contains(mt, "zip") || strings.Contains(mt, "tar") || strings.Contains(mt, "gzip") ||
		strings.HasSuffix(lowerName, ".zip") || strings.HasSuffix(lowerName, ".tar") || strings.HasSuffix(lowerName, ".gz"):
		return "Archive"
	case strings.Contains(mt, "json") || strings.Contains(mt, "yaml") || hasCodeExtension(lowerName):
		return "Code"
	case strings.HasPrefix(mt, "text/") || strings.HasSuffix(lowerName, ".md") || strings.HasSuffix(lowerName, ".txt"):
		return "Text"
	default:
		return "File"
	}
}

func hasCodeExtension(lowerName string) bool {
	for _, ext := range codeExtensions {
		if strings.HasSuffix(lowerName, ext) {
			return true
		}
	}
	return false
}

// DeliverableMetrics holds the "Send to chat" Prometheus counter (design
// §5.9: vornik_deliverable_sends_total{channel}). Follows the exact
// idiom internal/ui/integrations_metrics.go established: a struct of
// *prometheus.CounterVec + a constructor that MustRegisters + nil-safe
// Record methods, built by the CONTAINER (not a registry-taking
// ServerOption) so the two-pass initHTTPServer re-entry never double-
// registers the same collector name (see integrations_metrics.go's doc
// comment on the 2026-06-06 "TWO-PASS TRAP" incident).
type DeliverableMetrics struct {
	SendsTotal *prometheus.CounterVec
}

// NewDeliverableMetrics creates and registers the deliverable-send
// counter on reg.
func NewDeliverableMetrics(reg *prometheus.Registry) *DeliverableMetrics {
	m := &DeliverableMetrics{
		SendsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Name:      "deliverable_sends_total",
			Help:      "Deliverable-first completion \"send to chat\" actions, by destination channel.",
		}, []string{"channel"}),
	}
	reg.MustRegister(m.SendsTotal)
	return m
}

// RecordSend bumps the per-channel send counter. Nil-safe — a Server
// built without WithDeliverableMetrics (most tests, and pass 1 of the
// two-pass HTTP init) just skips the increment.
func (m *DeliverableMetrics) RecordSend(channel string) {
	if m == nil || m.SendsTotal == nil || channel == "" {
		return
	}
	m.SendsTotal.WithLabelValues(channel).Inc()
}

// WithDeliverableMetrics wires an already-constructed DeliverableMetrics
// onto the Server. A nil m is a harmless no-op.
func WithDeliverableMetrics(m *DeliverableMetrics) ServerOption {
	return func(s *Server) { s.deliverableMetrics = m }
}

// WithChannelResolver wires the chatorigin.ChannelResolver the "send to
// chat" handler uses to turn a resolved channel name ("telegram",
// "email", "slack") into a live conversation.Channel. Same collaborator
// internal/narrator's chat push (task 2.3) uses via the container's
// containerChannelResolver — sharing the adapter, not the field.
func WithChannelResolver(r chatorigin.ChannelResolver) ServerOption {
	return func(s *Server) { s.channelResolver = r }
}

// WithChatAuditRepository wires the chat-audit lookup "send to chat"
// needs to resolve a task's originating channel (chatorigin.Resolve's
// ChatAuditLookup). Deliberately separate from WithAdminChatAuditRepository
// (server.go's adminChatAudit field) — that option is only appended by
// the container on Admin-provider (EE) builds' admin wiring block, while
// deliverable sends must work on any build the narrator/chat-push
// feature is wired on. Both options may point at the same underlying
// repository; this one just doesn't inherit the admin gate's wiring
// condition.
func WithChatAuditRepository(lookup chatorigin.ChatAuditLookup) ServerOption {
	return func(s *Server) { s.chatAudit = lookup }
}

// DeliverableSend handles POST /tasks/{taskID}/artifacts/{artifactID}/send
// — the "send to chat" action on a deliverable card. Resolves the task's
// originating channel via chatorigin.Resolve and delivers the artifact:
// attachment-capable channels (today: email) get it via
// conversation.Attachment.ArtifactID; text-only channels (Telegram,
// Slack — see internal/telegram and internal/slack's Send, which do not
// read msg.Attachments) still get a usable download link in the message
// text, mirroring internal/narrator/chatpush.go's completionMessage.
//
// Always renders an HTML fragment (the "deliverableSendResult" partial)
// for the HTMX inline confirmation — never a redirect — including the
// "not chat-originated" and "send failed" cases, so the button's target
// div always gets SOME feedback rather than going silent.
func (s *Server) DeliverableSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.taskRepo == nil || s.artifactRepo == nil {
		http.Error(w, "task/artifact repositories not configured", http.StatusServiceUnavailable)
		return
	}

	taskID, artifactID, ok := parseDeliverableSendPath(r.URL.Path)
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	task, err := s.taskRepo.Get(ctx, taskID)
	if err != nil || task == nil {
		http.NotFound(w, r)
		return
	}
	// Multi-tenant scope gate — same pattern as TaskConversationAction /
	// TaskCancel: the route carries no project segment, so the check
	// happens after the load.
	if !s.uiRequireProjectScope(w, r, task.ProjectID) {
		return
	}

	artifact, err := s.artifactRepo.Get(ctx, artifactID)
	if err != nil || artifact == nil {
		http.NotFound(w, r)
		return
	}
	// Defense against sending an artifact that doesn't belong to this
	// task, or isn't a deliverable (OUTPUT) at all — a crafted artifact
	// ID from a different task/project must not be sendable by guessing
	// the ID, and INPUT/INTERMEDIATE artifacts are never "the
	// deliverable" (design §5.8).
	if artifact.TaskID == nil || *artifact.TaskID != taskID || artifact.ArtifactClass != persistence.ArtifactClassOutput {
		http.NotFound(w, r)
		return
	}

	res, ok := chatorigin.Resolve(ctx, task, s.taskRepo, s.chatAudit, s.channelResolver)
	if !ok {
		s.renderDeliverableSendResult(w, deliverableSendResult{
			Message: "This task wasn't started from a chat channel — nothing to send to.",
		})
		return
	}

	mime := ""
	if artifact.MimeType != nil {
		mime = *artifact.MimeType
	}
	text := fmt.Sprintf("Here's your %s:", artifact.Name)
	if s.webUIBaseURL != "" {
		text += "\n" + strings.TrimRight(s.webUIBaseURL, "/") + "/ui/artifacts/" + artifact.ID
	}
	msg := conversation.ChannelMessage{
		SessionID:   res.SessionID,
		Text:        text,
		Attachments: []conversation.Attachment{{Name: artifact.Name, ArtifactID: artifact.ID, MimeType: mime}},
	}
	// Email recovers to/subject from the durable audit row — same
	// precedent as steering/notifier.go and chatpush.go.
	if res.ChannelName == "email" && res.AuditRow != nil {
		if to := chatorigin.EmailAddrFromUserID(res.AuditRow.UserID); to != "" {
			msg.ChannelSpecific = map[string]string{
				"to":      to,
				"subject": "vornik: your deliverable",
			}
		}
	}

	if _, sendErr := res.Channel.Send(ctx, msg); sendErr != nil {
		s.logger.Warn().Err(sendErr).Str("task_id", taskID).Str("artifact_id", artifactID).
			Str("channel", res.ChannelName).Msg("deliverable send: channel Send failed")
		s.renderDeliverableSendResult(w, deliverableSendResult{
			Message: "Send failed: " + sendErr.Error(),
		})
		return
	}

	s.deliverableMetrics.RecordSend(res.ChannelName)
	s.renderDeliverableSendResult(w, deliverableSendResult{Ok: true, Channel: res.ChannelName})
}

// deliverableSendResult is the "deliverableSendResult" partial's data.
type deliverableSendResult struct {
	Ok      bool
	Channel string
	Message string
}

func (s *Server) renderDeliverableSendResult(w http.ResponseWriter, data deliverableSendResult) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "deliverableSendResult", data); err != nil {
		s.logger.Error().Err(err).Msg("renderDeliverableSendResult: template failed")
	}
}

// parseDeliverableSendPath extracts (taskID, artifactID) from
// "/tasks/{taskID}/artifacts/{artifactID}/send", tolerating a leading
// "/ui" the same way TaskConversationAction does (defensive — the
// subtree handler normally strips it before this runs).
func parseDeliverableSendPath(path string) (taskID, artifactID string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/ui")
	trimmed = strings.TrimPrefix(trimmed, "/tasks/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] != "artifacts" || parts[2] == "" || parts[3] != "send" {
		return "", "", false
	}
	return parts[0], parts[2], true
}
