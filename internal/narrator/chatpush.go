// Package narrator implements the Narrated Execution worker. This file
// adds the chat push (task 2.3, https://docs.vornik.io
// narrated-execution-design.md §5.7 + §9 Q3): opt-in per project, pushes a
// COARSER subset of already-produced narration lines (milestone lines
// only — by default step-completed + completion) to the task's
// originating chat channel. Reuses the exact resolve-and-send chain
// internal/steering's Notifier uses, via internal/chatorigin, so the two
// callers can't drift (companion review finding 5/8). Makes NO extra LLM
// calls: the pushed text is always the line emitLine already composed (or,
// for the completion push, that text led by the task's OUTPUT artifacts).
package narrator

import (
	"context"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/chatorigin"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// TaskGetter resolves a task by ID. Alias of chatorigin.TaskGetter,
// satisfied by persistence.TaskRepository. The chat push needs the full
// Task (ChatTurnID / ParentTaskID) for a line's bare taskID —
// executionState only tracks the id string.
type TaskGetter = chatorigin.TaskGetter

// ArtifactLister is the narrow read the completion push needs to lead with
// the task's deliverable (design §5.7 point 4). Satisfied by
// *artifacts.Store's List method. Nil ⇒ the completion push falls back to
// the plain completion narration text with no deliverable lead-in.
type ArtifactLister interface {
	List(ctx context.Context, projectID, executionID string) ([]*persistence.Artifact, error)
}

// ProjectNarratorSettings is the per-project opt-in/opt-out the chat push
// consults (registry.Project.Narrator — see that type's doc comment). The
// zero value is the safe default: chat push off, narration on.
type ProjectNarratorSettings struct {
	// ChatPush opts this project into pushing milestone lines to chat.
	ChatPush bool
	// NoNarration suppresses ALL narration (not just chat push) for this
	// project — design §9 Q3.
	NoNarration bool
}

// projectSettings resolves st's per-project settings via n.ProjectSettings,
// defaulting to the zero value (chat push off, narration on) when no
// resolver is wired — so an un-configured Narrator behaves exactly as
// before task 2.3 shipped.
func (n *Narrator) projectSettings(projectID string) ProjectNarratorSettings {
	if n.ProjectSettings == nil {
		return ProjectNarratorSettings{}
	}
	return n.ProjectSettings(projectID)
}

// isMilestoneKind reports whether kind is eligible for the chat push — a
// COARSER cadence than the UI story, which shows every line (design §5.7
// point 2). n.ChatMilestoneKinds (config.NarratorConfig.ChatMilestoneKinds)
// overrides the default of [step_completed, completion] when non-empty.
func (n *Narrator) isMilestoneKind(kind triggerKind) bool {
	if len(n.ChatMilestoneKinds) == 0 {
		return kind == triggerStepCompleted || kind == triggerCompletion
	}
	for _, k := range n.ChatMilestoneKinds {
		if triggerKind(k) == kind {
			return true
		}
	}
	return false
}

// pushChatMilestone is emitLine's chat-push hook, called AFTER a line has
// been persisted and published (never before — it only ever pushes a line
// that already exists in storage/on the bus). Best-effort and non-fatal,
// like steering's NotifySteeringRequired: every skip/failure path logs (at
// most) and returns, never propagating an error to the caller. Pushes NO
// extra LLM call — row.Text is exactly the text emitLine already composed.
func (n *Narrator) pushChatMilestone(ctx context.Context, kind triggerKind, st *executionState, row *persistence.ExecutionNarration) {
	if !n.isMilestoneKind(kind) {
		return
	}
	if !n.projectSettings(st.projectID).ChatPush {
		return
	}
	if n.Tasks == nil || n.Audit == nil || n.Resolver == nil {
		return
	}
	task, err := n.Tasks.Get(ctx, st.taskID)
	if err != nil || task == nil {
		return
	}
	// SHARED with internal/steering's Notifier via internal/chatorigin — a
	// future channel-resolution change updates this one call, not two
	// duplicated chains (companion review finding 5/8).
	res, ok := chatorigin.Resolve(ctx, task, n.Tasks, n.Audit, n.Resolver)
	if !ok {
		// Not chat-originated (UI/API/autonomy), or the resolved channel
		// isn't wired for outbound (web-chat/a2a) — never an error.
		return
	}

	text := row.Text
	var attachments []conversation.Attachment
	if kind == triggerCompletion {
		// Deliverable-led completion push (design §5.7 point 4): lead with
		// the task's OUTPUT artifacts instead of a bare "task done".
		text, attachments = n.completionMessage(ctx, st, row)
	}

	msg := conversation.ChannelMessage{SessionID: res.SessionID, Text: text, Attachments: attachments}
	// Email's Send needs an addressable recipient + subject, recovered from
	// the durable audit row — same precedent as steering/notifier.go.
	if res.ChannelName == "email" && res.AuditRow != nil {
		if to := chatorigin.EmailAddrFromUserID(res.AuditRow.UserID); to != "" {
			msg.ChannelSpecific = map[string]string{
				"to":      to,
				"subject": "vornik: a task update",
			}
		}
	}

	if _, err := res.Channel.Send(ctx, msg); err != nil {
		n.Logger.Warn().Err(err).Str("task_id", st.taskID).Str("channel", res.ChannelName).
			Msg("narrator: chat push send failed")
		return
	}
	n.Logger.Debug().Str("task_id", st.taskID).Str("channel", res.ChannelName).Str("kind", string(kind)).
		Msg("narrator: pushed milestone narration to originating chat")
}

// completionMessage builds the deliverable-led completion text +
// attachments (design §5.7 point 4 / §5.8): "Here's your <thing>:" plus the
// completion narration line, followed by a UI deep link. Channels that
// support attachments get the artifacts via Attachment.ArtifactID;
// text-only channels still get the name list in the text itself. Falls
// back to the plain completion text (row.Text, no attachments) when no
// artifact store is wired or the execution produced no OUTPUT artifacts —
// e.g. a task whose only deliverable IS the narration text.
func (n *Narrator) completionMessage(ctx context.Context, st *executionState, row *persistence.ExecutionNarration) (string, []conversation.Attachment) {
	outputs := n.outputArtifacts(ctx, st)
	if len(outputs) == 0 {
		return row.Text, nil
	}

	b := &strings.Builder{}
	if len(outputs) == 1 {
		fmt.Fprintf(b, "Here's your %s:", outputs[0].Name)
	} else {
		fmt.Fprintf(b, "Here's what this task produced (%d files):", len(outputs))
		for _, a := range outputs {
			fmt.Fprintf(b, "\n- %s", a.Name)
		}
	}
	b.WriteString("\n" + row.Text)
	if n.BaseURL != "" {
		// Canonical UI task-detail route, matching steering's deep link
		// convention (internal/steering/notifier.go composeText).
		fmt.Fprintf(b, "\nOpen it: %s/ui/tasks/%s", strings.TrimRight(n.BaseURL, "/"), st.taskID)
	}

	attachments := make([]conversation.Attachment, 0, len(outputs))
	for _, a := range outputs {
		mime := ""
		if a.MimeType != nil {
			mime = *a.MimeType
		}
		attachments = append(attachments, conversation.Attachment{Name: a.Name, ArtifactID: a.ID, MimeType: mime})
	}
	return b.String(), attachments
}

// outputArtifacts lists st's execution's OUTPUT-class artifacts (never
// INPUT/INTERMEDIATE/SNAPSHOT/LOG/METADATA — those aren't the deliverable).
// Returns nil (not an error) when no artifact store is wired, the list call
// fails, or there are simply no OUTPUT artifacts.
func (n *Narrator) outputArtifacts(ctx context.Context, st *executionState) []*persistence.Artifact {
	if n.Artifacts == nil {
		return nil
	}
	arts, err := n.Artifacts.List(ctx, st.projectID, st.executionID)
	if err != nil {
		n.Logger.Debug().Err(err).Str("execution_id", st.executionID).Msg("narrator: outputArtifacts list failed; completion push falls back to plain text")
		return nil
	}
	outputs := make([]*persistence.Artifact, 0, len(arts))
	for _, a := range arts {
		if a != nil && a.ArtifactClass == persistence.ArtifactClassOutput {
			outputs = append(outputs, a)
		}
	}
	return outputs
}
