package telegram

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/safepath"
)

// Command handlers map command names to their handlers.
// Handlers receive (ctx, bot, chatID, userID). userID is threaded so
// per-user authorization (e.g. /inbox project scoping) is possible
// without each handler re-deriving it from the chat.
var commands = map[string]func(context.Context, *Bot, int64, int64) string{
	"/start":   handleStart,
	"/help":    handleHelp,
	"/new":     handleNew,
	"/reset":   handleNew, // alias for /new
	"/project": handleProject,
	"/inbox":   handleInbox,
}

// handleInbox renders the operator's awaiting-input task list
// (Phase 28). Routes through Bot.renderInbox which queries the
// task repo for AWAITING_INPUT rows.
func handleInbox(ctx context.Context, b *Bot, chatID, userID int64) string {
	if b == nil {
		return "Bot unavailable."
	}
	out, err := b.renderInbox(ctx, userID)
	if err != nil {
		b.logger.Error().Err(err).Msg("/inbox: renderInbox failed")
		return "Failed to load inbox: " + err.Error()
	}
	return out
}

// handleStart is the onboarding wizard — 2026.6.0 SaaS-readiness
// feature 3 slice 2. Replaces the legacy prose welcome with a
// branching greeting tailored to where the user is:
//
//   - Active project already set        → short "welcome back" with the
//     project name and a /help hint.
//   - No active project, projects exist → welcome + inline project
//     picker so the user can switch
//     without typing a slug.
//   - No projects at all                → empty-state with a clear next
//     step pointing at the web
//     gallery, which is the
//     authoring surface.
//
// Returning "" suppresses the legacy text reply so it isn't posted
// alongside the picker keyboard / structured welcome.
func handleStart(ctx context.Context, b *Bot, chatID int64, _ int64) string {
	if b == nil {
		return "Welcome to vornik!"
	}
	if active := b.getActiveProject(chatID); active != "" {
		return fmt.Sprintf("Welcome back! Your active project is %s.\n\nAsk me anything in natural language, or send /help for the command list.", active)
	}
	projects := b.getProjectList()
	if len(projects) == 0 {
		// Empty-state. The web gallery is the canonical onboarding
		// path; we don't try to recreate it in Telegram because the
		// parameter forms don't fit the chat surface well.
		webURL := strings.TrimSpace(b.config.WebUIBaseURL)
		if webURL == "" {
			return "Welcome to vornik!\n\nYou don't have any projects yet. Create one from the web UI at /ui/projects/new, then come back and send /start again."
		}
		return fmt.Sprintf("Welcome to vornik!\n\nYou don't have any projects yet. You can create one from a template here: %s/ui/projects/new\n\nThen send /start again here.",
			strings.TrimRight(webURL, "/"))
	}
	// Projects exist → friendly intro + picker. Mirrors the
	// /project picker behaviour so returning users get the same
	// fast switch path.
	intro := "Welcome to vornik! Pick a project below to make it active, or send /help for the command list."
	if err := b.sendMessage(ctx, chatID, intro); err != nil {
		b.logger.Warn().Err(err).Int64("chat_id", chatID).
			Msg("telegram /start: intro send failed; picker may post without context")
	}
	if err := b.sendProjectPicker(ctx, chatID, projects); err != nil {
		b.logger.Warn().Err(err).Int64("chat_id", chatID).
			Msg("telegram /start: picker send failed; falling back to /project hint")
		return "Send /project to pick a project."
	}
	return ""
}

func handleHelp(_ context.Context, b *Bot, chatID int64, _ int64) string {
	return `Available commands:

Session:
/new - Start a fresh session (clears history)
/context - Show session stats
/summarize - Compress history into a summary (saves tokens)
/save <name> - Park the active conversation under a name
/load [name] - Restore a saved conversation (no arg lists names)
/search <query> - Search across your saved conversations
/undo - Remove last exchange
/forget N - Drop last N messages
/pin <text> - Pin a persistent instruction

Behaviour:
/verbose silent|short|full - Control task-completion notifications

Projects:
/project [id] - Show or switch active project
/autopilot on|off - Enable/disable autonomous mode for active project
/inbox - List tasks awaiting your input (across projects)

Identity:
/link - Issue a code to consolidate this chat's profile with another channel
/link <code> - Claim a code from another channel; merges the two profiles

/help - Show this message

Ask me naturally:
- "list my projects"
- "create a task to fix the login bug"
- "what tasks are running?"
- "show me the artifacts for task X"`
}

func handleNew(_ context.Context, b *Bot, chatID int64, _ int64) string {
	b.resetConversation(chatID)
	b.saveConversations()
	return "New session started. How can I help you?"
}

func handleProject(ctx context.Context, b *Bot, chatID int64, _ int64) string {
	current := b.getActiveProject(chatID)
	// 2026.6.0 — when the user has no active project AND the bot
	// can list projects, send an inline-keyboard picker INSTEAD of
	// the legacy "use /project <id>" prose. The picker is a much
	// gentler onboarding step than "memorise this slug". Returning
	// "" here suppresses the legacy text reply so the picker isn't
	// followed by stale prose.
	if current == "" {
		projects := b.getProjectList()
		if len(projects) == 0 {
			return "No active project, and no projects are configured. Create one with `vornikctl init project <name>` or via /api/v1/projects/from-template."
		}
		if err := b.sendProjectPicker(ctx, chatID, projects); err != nil {
			b.logger.Warn().Err(err).Int64("chat_id", chatID).
				Msg("telegram /project: inline-keyboard picker send failed; falling back to text")
			// Fall back to the legacy prose response so the
			// operator still gets SOMETHING. Slug listing
			// helps them type /project <id> manually.
			slugs := make([]string, 0, len(projects))
			for _, p := range projects {
				if p != nil {
					slugs = append(slugs, p.ID)
				}
			}
			return "No active project. Available: " + strings.Join(slugs, ", ") + ". Use /project <id> to switch."
		}
		// Picker sent; suppress the legacy text reply.
		return ""
	}
	return fmt.Sprintf("Active project: %s", current)
}

// sendProjectPicker renders a project-list inline keyboard and
// posts it to chat. Each button's callback_data is
// "project:select:<projectId>" — the bot_callbacks dispatcher
// switches the active project when the user clicks. Cap the
// keyboard at 12 buttons (4 cols × 3 rows) to keep the rendered
// surface comfortable on mobile; ID overflow trips a fallback
// prose listing rather than truncating silently.
func (b *Bot) sendProjectPicker(ctx context.Context, chatID int64, projects []*registry.Project) error {
	const maxButtons = 12
	buttons := make([]Button, 0, min(maxButtons, len(projects)))
	for _, p := range projects {
		if p == nil {
			continue
		}
		data, err := EncodeCallback("project", "select", p.ID)
		if err != nil {
			// Project ID too long for callback data. Skip with
			// a warn — the user can still /project <id> manually.
			b.logger.Warn().Err(err).Str("project_id", p.ID).
				Msg("telegram /project: skipping over-long project ID in picker")
			continue
		}
		label := p.ID
		if p.DisplayName != "" {
			label = p.DisplayName
		}
		buttons = append(buttons, Button{Text: label, Data: data})
		if len(buttons) >= maxButtons {
			break
		}
	}
	if len(buttons) == 0 {
		return ErrEmptyMessage
	}
	kb := KeyboardGrid(2, buttons...)
	text := "Pick a project to make active:"
	if len(projects) > maxButtons {
		text += fmt.Sprintf("\n(Showing first %d of %d — use /project <id> for the rest.)", maxButtons, len(projects))
	}
	return b.sendMessageWithMarkup(ctx, chatID, text, &kb)
}

// defaultDispatchTimeout is the fallback cap on how long the bot waits for
// the LLM to produce a reply. See BotConfig.DispatchTimeout for the
// per-deployment override.
const defaultDispatchTimeout = 5 * time.Minute

// effectiveDispatchTimeout picks the configured timeout or falls back to
// the default. Resolving this lazily (at call time, not at config parse)
// means config reload takes effect for the next message without a restart.
func (b *Bot) effectiveDispatchTimeout() time.Duration {
	if b.config.DispatchTimeout > 0 {
		return b.config.DispatchTimeout
	}
	return defaultDispatchTimeout
}

// HandleMessage processes an incoming message.
//
// Protocol responsibilities (auth, rate limiting, file download, command routing)
// are handled here. LLM interaction is delegated to the dispatcher via the inbox
// channel. The reply-wait runs in a separate goroutine so the poll loop is never
// blocked by a slow LLM call and can keep handling commands like /new.
func (b *Bot) HandleMessage(ctx context.Context, msg *Message) error {
	userLabel := strconv.FormatInt(msg.UserID, 10)

	b.logger.Info().
		Int64("chat_id", msg.ChatID).
		Int64("user_id", msg.UserID).
		Str("username", msg.Username).
		Int("text_len", len(msg.Text)).
		Msg("telegram message received")

	if b.metrics != nil {
		b.metrics.MessagesReceived.WithLabelValues(userLabel).Inc()
	}

	// Silent-drop guard. Must run BEFORE the auth check.
	//
	// Telegram emits service messages (forum_topic_created,
	// forum_topic_closed, member-join, message-pin, etc.) into the
	// chat whenever the bot or a user performs the corresponding
	// action. The bot's own createForumTopic / closeForumTopic
	// calls each generate one of these. They arrive via getUpdates
	// as a normal Message with text="" (no document, no caption)
	// and from.id set to whoever triggered the action — typically
	// the bot itself. Without this short-circuit the auth check
	// below treated those as inbound chat from an unauthorised
	// user and replied "You are not authorized to use this bot."
	// to the whole supergroup, once per forum-topic lifecycle
	// event. By the time we reach line ~1875 in HandleUpdate, any
	// real attachment has already set msg.Text to a placeholder
	// ("I've attached a file."), so an empty msg.Text here means
	// the update carries no actionable content. Drop silently.
	//
	// Voice messages are exempt — they arrive with empty Text and
	// fill it in via STT transcription further down. IsVoice is set
	// by HandleUpdate's voice/audio detection.
	if strings.TrimSpace(msg.Text) == "" && !msg.IsVoice {
		return nil
	}

	if !b.IsAllowed(msg.UserID) {
		return b.sendErrorMessage(ctx, msg.ChatID, "You are not authorized to use this bot.")
	}
	// Remember the chat→user mapping so the auto-resume path
	// (NotifyTaskCompleted → triggerFollowup) can rebuild the
	// per-user project scope when it fires later. Done after the
	// allowlist check so unauthorised callers don't pollute the map.
	b.recordChatUser(msg.ChatID, msg.UserID)
	if !b.CheckRateLimit(msg.UserID) {
		if b.metrics != nil {
			b.metrics.RateLimitsHit.Inc()
		}
		return b.sendErrorMessage(ctx, msg.ChatID, "Rate limit exceeded. Please try again later.")
	}

	// Voice MVP slice 3: when the inbound is a voice (or audio)
	// attachment AND a voice.STTProvider is wired, transcribe before
	// the rest of the message-routing pipeline runs. On STT failure
	// we reply humanely to the user and short-circuit — no
	// dispatcher / LLM spend on a payload we couldn't decode. The
	// tracker is updated so the next outbound Channel.Send replies
	// via TTS+sendVoice. See https://docs.vornik.io
	if msg.IsVoice && b.voice.STT != nil {
		hint := voiceImportHint(msg.VoiceHint)
		if humane, ok := b.handleVoiceAttachment(ctx, msg, hint); !ok {
			if humane != "" {
				return b.sendErrorMessage(ctx, msg.ChatID, humane)
			}
			return nil
		}
		// Successful transcribe — clear the FileID so the legacy
		// file-attachment downstream branch doesn't double-route
		// the audio. Audit / artifact persistence for the original
		// blob is a follow-up slice (caching, observability — slice 6).
		msg.FileID = ""
	} else if !msg.IsVoice {
		// Non-voice inbound resets the voice tracker so the next
		// reply on this chat is text (the user typed, so they want
		// to read).
		if b.voiceTracker != nil {
			b.voiceTracker.MarkText(msg.ChatID)
		}
	}

	// After voice transcription the message may still have empty
	// text (the user sent a genuinely silent recording). Drop
	// silently in that case — same posture as the legacy
	// service-message guard above.
	if strings.TrimSpace(msg.Text) == "" {
		return nil
	}

	// Phase 29: forum-topic reply routing. DB-backed
	// (chat_id, thread_id) → task lookup. Takes precedence over
	// the in-memory notifTracker because it's persistent (survives
	// restart) and unambiguous (no reliance on the operator using
	// the right swipe-to-reply gesture).
	if handled, _ := b.routeForumReplyIfApplicable(ctx, msg); handled {
		return nil
	}

	// Phase 28: per-task reply routing. When the operator replied
	// to a notification we tracked, the reply goes straight to
	// task_messages instead of the dispatcher LLM. Skip when the
	// text is a slash command (operators sometimes reply with
	// /help to a task notification expecting the bot help, not a
	// task directive).
	if msg.ReplyToMessageID != 0 && b.notifTracker != nil && !strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
		if taskID, projectID, ok := b.notifTracker.lookup(msg.ChatID, msg.ReplyToMessageID); ok {
			if handled, _ := b.routeReplyToTask(ctx, msg, taskID, projectID); handled {
				return nil
			}
		}
	}

	// Download file attachment if present. Two host destinations:
	//
	//   1. <projectWorkspacePath>/<project>/uploads/ when an active
	//      project is set — keeps user-uploaded inputs near the
	//      project so they survive across tasks and operators can
	//      inspect what was sent.
	//   2. os.TempDir() as a last resort.
	//
	// Both flow through the same dispatcher directive: pass the host
	// path via input_files on create_task. The executor stages every
	// input into the per-step workspace at /app/workspace/artifacts/in/
	// (security-allowlisting both /tmp and the project uploads dir),
	// rewrites the inputArtifacts path to the container-relative form,
	// and the agent's entrypoint detects image extensions there and
	// attaches them as multimodal content blocks. We deliberately do
	// NOT route via the /app/workspace/project bind mount — that mount
	// is the per-task git worktree, which doesn't include the
	// project-root uploads/ dir.
	if msg.FileID != "" {
		destDir := os.TempDir()
		if p := b.getActiveProject(msg.ChatID); p != "" && b.projectWorkspacePath != "" {
			safeProjectID, err := safepath.CleanPathComponent(p)
			if err != nil {
				b.logger.Warn().Err(err).Str("project_id", p).Msg("invalid active project for upload path")
			} else if joined, err := safepath.JoinUnder(b.projectWorkspacePath, safeProjectID, "uploads"); err != nil {
				b.logger.Warn().Err(err).Str("project_id", p).Msg("failed to build project upload path")
			} else {
				destDir = joined
			}
		}
		path, err := b.DownloadTelegramFile(ctx, msg.FileID, msg.FileName, destDir)
		if err != nil {
			b.logger.Warn().Err(err).Str("file_id", msg.FileID).Msg("failed to download telegram file")
		} else {
			msg.Text += fmt.Sprintf(
				"\n\n[SYSTEM: user attached file %q at host path %q. "+
					"When you call create_task, pass input_files: [%q]. "+
					"The executor stages attachments into /app/workspace/artifacts/in/ "+
					"inside the agent container — reference that path (not the host path) "+
					"in the task prompt you write. Image attachments are also forwarded "+
					"to vision-capable models as multimodal content automatically.]",
				msg.FileName, path, path,
			)
		}
	}

	// Handle bot commands.
	if strings.HasPrefix(msg.Text, "/") {
		parts := strings.Fields(msg.Text)
		cmd := parts[0]

		// /project <id> — switch project and pin to lead agent.
		if cmd == "/project" && len(parts) > 1 {
			projectID := parts[1]
			if b.registry != nil && b.registry.GetProject(projectID) == nil {
				return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("Unknown project '%s'. Use /project to list available projects.", projectID))
			}
			// Per-user project scoping. Deny with a clear message rather
			// than the generic "unknown project" — telling the operator
			// the project exists but is out of scope is more useful than
			// pretending it doesn't.
			if !b.UserCanAccessProject(msg.UserID, projectID) {
				return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf(
					"You are not authorized for project '%s'.", projectID))
			}
			b.setActiveProject(msg.ChatID, projectID)
			// Reset conversation — stale tool messages from the previous
			// persona confuse the new lead.
			b.resetConversation(msg.ChatID)
			b.saveConversations()

			// Check if the project has a lead agent.
			_, roleName := dispatcher.ResolveLeadPrompt(b.registry, projectID)
			if roleName != "" {
				return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("Switched to project '%s'. You are now talking to the %s.", projectID, roleName))
			}
			return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("Switched to project '%s'.", projectID))
		}

		// /context — show session stats.
		if cmd == "/context" {
			conv := b.getConversation(msg.ChatID)
			project := b.getActiveProject(msg.ChatID)
			if project == "" {
				project = "(none)"
			}
			age := time.Since(conv.CreatedAt()).Truncate(time.Second)
			pinned := len(conv.PinnedMessages())
			persona := "dispatcher"
			if _, roleName := dispatcher.ResolveLeadPrompt(b.registry, b.getActiveProject(msg.ChatID)); roleName != "" {
				persona = roleName
			}
			return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf(
				"Session context:\n"+
					"  Messages: %d\n"+
					"  Pinned: %d\n"+
					"  Est. tokens: ~%d\n"+
					"  Session age: %s\n"+
					"  Active project: %s\n"+
					"  Persona: %s",
				conv.Len(), pinned, conv.EstimateTokens(), age, project, persona))
		}

		// /undo — remove last user message + assistant response.
		if cmd == "/undo" {
			conv := b.getConversation(msg.ChatID)
			removed := conv.Undo()
			if removed == 0 {
				return b.sendMessage(ctx, msg.ChatID, "Nothing to undo.")
			}
			b.saveConversations()
			return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("Removed %d message(s).", removed))
		}

		// /forget N — drop last N messages.
		if cmd == "/forget" {
			if len(parts) < 2 {
				return b.sendMessage(ctx, msg.ChatID, "Usage: /forget <number>")
			}
			n, err := strconv.Atoi(parts[1])
			if err != nil || n <= 0 {
				return b.sendMessage(ctx, msg.ChatID, "Usage: /forget <number> (positive integer)")
			}
			conv := b.getConversation(msg.ChatID)
			dropped := conv.DropLast(n)
			b.saveConversations()
			return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("Dropped %d message(s). %d remaining.", dropped, conv.Len()))
		}

		// /pin <text> — pin a persistent instruction.
		if cmd == "/pin" {
			if len(parts) < 2 {
				return b.sendMessage(ctx, msg.ChatID, "Usage: /pin <message text>")
			}
			text := strings.TrimPrefix(msg.Text, "/pin ")
			conv := b.getConversation(msg.ChatID)
			conv.Pin(chat.Message{Role: "system", Content: text})
			b.saveConversations()
			return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("Pinned: %s\n\nThis instruction persists across /new resets.", text))
		}

		// /save <name> — persist the active conversation under a name
		// that /load can restore later. Useful for parking long threads
		// you want to revisit without running the working conversation
		// into trim/compaction limits.
		if cmd == "/save" {
			if len(parts) < 2 {
				return b.sendMessage(ctx, msg.ChatID, "Usage: /save <name>")
			}
			if b.config.SessionPath == "" {
				return b.sendMessage(ctx, msg.ChatID, "Session persistence is disabled (no session_path configured).")
			}
			name := parts[1]
			conv := b.getConversation(msg.ChatID)
			if conv.Len() == 0 {
				return b.sendMessage(ctx, msg.ChatID, "Nothing to save — this conversation is empty.")
			}
			if err := chat.SaveNamedConversation(b.config.SessionPath, msg.ChatID, name, conv); err != nil {
				return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("Save failed: %v", err))
			}
			return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("Saved %d message(s) as %q. Use /load %s to restore.", conv.Len(), name, name))
		}

		// /load <name> — replace the current conversation with a
		// previously-saved one. The current conversation is discarded;
		// /save it first if you want to come back to it.
		if cmd == "/load" {
			if len(parts) < 2 {
				names, err := chat.ListNamedSaves(b.config.SessionPath, msg.ChatID)
				if err != nil {
					return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("Failed to list saves: %v", err))
				}
				if len(names) == 0 {
					return b.sendMessage(ctx, msg.ChatID, "No saved conversations. Use /save <name> first.")
				}
				return b.sendMessage(ctx, msg.ChatID, "Saved conversations:\n"+strings.Join(names, "\n")+"\n\nUsage: /load <name>")
			}
			if b.config.SessionPath == "" {
				return b.sendMessage(ctx, msg.ChatID, "Session persistence is disabled (no session_path configured).")
			}
			name := parts[1]
			loaded, err := chat.LoadNamedConversation(b.config.SessionPath, msg.ChatID, name, b.config.MaxHistory)
			if err != nil {
				if os.IsNotExist(err) {
					return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("No saved conversation named %q.", name))
				}
				return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("Load failed: %v", err))
			}
			b.mu.Lock()
			b.conversations[msg.ChatID] = loaded
			b.mu.Unlock()
			b.saveConversations()
			return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("Loaded %q (%d message(s)). Prior conversation discarded.", name, loaded.Len()))
		}

		// /search <query> — 2026.7.0 F13. Free-text search
		// across the operator's named saves. Returns up to 5
		// ranked hits with a short snippet + /load pointer so
		// they can revisit an old thread without remembering
		// which name they used.
		if cmd == "/search" {
			if len(parts) < 2 {
				return b.sendMessage(ctx, msg.ChatID, "Usage: /search <query>")
			}
			if b.config.SessionPath == "" {
				return b.sendMessage(ctx, msg.ChatID, "Session persistence is disabled (no session_path configured), so there are no saves to search.")
			}
			query := strings.TrimSpace(strings.Join(parts[1:], " "))
			hits, err := chat.SearchSavedConversations(b.config.SessionPath, msg.ChatID, query, 5)
			if err != nil {
				return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("Search failed: %v", err))
			}
			if len(hits) == 0 {
				return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("No saved conversations matched %q. (Use /load with no argument to list every save.)", query))
			}
			var out strings.Builder
			fmt.Fprintf(&out, "Found %d match(es) for %q:\n\n", len(hits), query)
			for _, h := range hits {
				fmt.Fprintf(&out, "• %s (%d msgs, score %d)\n", h.Name, h.MessageCount, h.Score)
				if h.Snippet != "" {
					fmt.Fprintf(&out, "  %s\n", h.Snippet)
				}
				fmt.Fprintf(&out, "  → /load %s\n\n", h.Name)
			}
			return b.sendMessage(ctx, msg.ChatID, out.String())
		}

		// /summarize — compress conversation history into a summary via LLM.
		if cmd == "/summarize" {
			return b.handleSummarize(ctx, msg.ChatID)
		}

		// /verbose silent|short|full — control task-completion notification
		// verbosity for this chat. silent suppresses all task notifications
		// (the dispatcher's wait_for_task tool still works — that's an
		// internal channel). short shows a single line per terminal task.
		// full shows the existing rich notification.
		if cmd == "/verbose" {
			if len(parts) < 2 {
				return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("Notification verbosity is %q. Usage: /verbose silent|short|full", b.getVerbosity(msg.ChatID)))
			}
			mode := strings.ToLower(parts[1])
			switch mode {
			case "silent", "short", "full":
				b.setVerbosity(msg.ChatID, mode)
				b.saveConversations()
				return b.sendMessage(ctx, msg.ChatID, fmt.Sprintf("Notification verbosity set to %q.\n  silent: dispatcher uses task results internally; no chat pings\n  short:  one-line per terminal task (default)\n  full:   detailed status + artifact uploads", mode))
			default:
				return b.sendMessage(ctx, msg.ChatID, "Usage: /verbose silent|short|full")
			}
		}

		// /autopilot [on|off] — toggle autonomous mode for the active project.
		if cmd == "/autopilot" {
			return b.handleAutopilot(ctx, msg.ChatID, parts)
		}

		// /link [code] — operator-profile cross-channel linking
		// (Phase A finalisation). With no arg, issues an OTP
		// for the chat's canonical operator id. With an arg,
		// claims the OTP — the current chat's profile is folded
		// into the issuer's canonical and both speakers henceforth
		// resolve to the same row. See
		// https://docs.vornik.io
		if cmd == "/link" {
			return b.handleLinkCommand(ctx, msg.ChatID, parts)
		}

		if handler, ok := commands[cmd]; ok {
			response := handler(ctx, b, msg.ChatID, msg.UserID)
			// Empty response is a sentinel for "handler sent its
			// own reply (e.g. inline keyboard) — skip the legacy
			// sendMessage path". Without this guard the dispatcher
			// would emit ErrEmptyMessage for inline-keyboard
			// flows and log a spurious warning per /project
			// (no-args) tap.
			if response == "" {
				return nil
			}
			return b.sendMessage(ctx, msg.ChatID, response)
		}
	}

	receiver := b.Receiver()
	if receiver == nil {
		return b.sendErrorMessage(ctx, msg.ChatID, "Dispatcher is not configured.")
	}

	// Per-user project-scope enforcement on every turn: if the user
	// lost access to their pinned project (config reloaded, allowlist
	// narrowed, etc.), clear the pin and tell them. This stays inline
	// (before the receiver hop) so the SessionStore can trust that
	// any non-empty activeProjects[chatID] is still permitted; the
	// dispatcher's tool layer would otherwise see a stale project_id.
	activeProject := b.getActiveProject(msg.ChatID)
	if activeProject != "" && !b.UserCanAccessProject(msg.UserID, activeProject) {
		b.setActiveProject(msg.ChatID, "")
		_ = b.sendMessage(ctx, msg.ChatID, fmt.Sprintf(
			"Your access to project '%s' has been revoked. Returning to the default dispatcher.",
			activeProject))
		return nil
	}

	// Route inbound through the ConversationChannel receiver on a
	// goroutine so the poll loop keeps draining updates. The
	// SessionStore reads the bot's conversation state directly; do
	// NOT AddMessage here — the receiver appends the new user turn
	// itself, and a pre-emptive AddMessage would double it in the
	// dispatcher's history.
	go b.handleReceiverTurn(receiver, msg)
	return nil
}

// sendErrorMessage sends an error message to the user.
func (b *Bot) sendErrorMessage(ctx context.Context, chatID int64, text string) error {
	return b.sendMessage(ctx, chatID, text)
}

// handleAutopilot handles the /autopilot [on|off] command.
// With no argument it reports the current state.
// With on|off it enables or disables the autonomous loop for the active project.
func (b *Bot) handleAutopilot(ctx context.Context, chatID int64, parts []string) error {
	projectID := b.getActiveProject(chatID)
	if projectID == "" {
		return b.sendMessage(ctx, chatID, "No active project. Use /project <id> to select one first.")
	}

	b.mu.RLock()
	mgr := b.autonomyMgr
	b.mu.RUnlock()

	if mgr == nil {
		return b.sendMessage(ctx, chatID, "Autonomous mode is not available (chat client not configured).")
	}

	// No argument — report current state.
	if len(parts) < 2 {
		status := "off"
		if mgr.IsAutonomyEnabled(projectID) {
			status = "on"
		}
		return b.sendMessage(ctx, chatID, fmt.Sprintf("Autopilot for project '%s' is %s.\nUsage: /autopilot on|off", projectID, status))
	}

	switch strings.ToLower(parts[1]) {
	case "on", "true", "1":
		if err := mgr.EnableProject(projectID); err != nil {
			return b.sendMessage(ctx, chatID, fmt.Sprintf("Failed to enable autopilot: %v", err))
		}
		return b.sendMessage(ctx, chatID, fmt.Sprintf(
			"Autopilot enabled for project '%s'.\nThe swarm will now evaluate the project goal and schedule tasks autonomously.",
			projectID,
		))
	case "off", "false", "0":
		if err := mgr.DisableProject(projectID); err != nil {
			return b.sendMessage(ctx, chatID, fmt.Sprintf("Failed to disable autopilot: %v", err))
		}
		return b.sendMessage(ctx, chatID, fmt.Sprintf("Autopilot disabled for project '%s'.", projectID))
	default:
		return b.sendMessage(ctx, chatID, "Usage: /autopilot on|off")
	}
}

// handleSummarize calls the LLM to compress the conversation history into a
// single summary message, replacing the full history. This reduces token usage
// while preserving conversational context.
func (b *Bot) handleSummarize(ctx context.Context, chatID int64) error {
	if b.llmClient == nil {
		return b.sendMessage(ctx, chatID, "LLM client not available.")
	}

	conv := b.getConversation(chatID)
	msgs := conv.GetMessages()
	if len(msgs) == 0 {
		return b.sendMessage(ctx, chatID, "Nothing to summarize — conversation is empty.")
	}

	// Build a compact transcript for the LLM to summarize.
	var transcript strings.Builder
	for _, m := range msgs {
		role := m.Role
		if role == "tool" {
			role = "tool_result"
		}
		if m.Content != "" {
			fmt.Fprintf(&transcript, "%s: %s\n\n", role, m.Content)
		}
	}

	summarizeMessages := []chat.Message{
		{
			Role: "system",
			Content: "You are a conversation summarizer. Given a chat transcript, produce a concise summary " +
				"(2-5 sentences) capturing the key topics discussed, decisions made, and current context. " +
				"Write in third person past tense. Do not add opinions or extra commentary.",
		},
		{
			Role:    "user",
			Content: "Summarize this conversation:\n\n" + transcript.String(),
		},
	}

	_ = b.sendMessage(ctx, chatID, "Summarizing conversation...")

	summarizeCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	resp, err := b.llmClient.Complete(summarizeCtx, summarizeMessages)
	if err != nil {
		b.logger.Warn().Err(err).Int64("chat_id", chatID).Msg("summarize LLM call failed")
		return b.sendMessage(ctx, chatID, fmt.Sprintf("Summarization failed: %v", err))
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
		return b.sendMessage(ctx, chatID, "Summarization produced an empty response.")
	}

	summary := resp.Choices[0].Message.Content

	// Replace full history with a single summary assistant message.
	conv.ReplaceMessages([]chat.Message{
		{Role: "assistant", Content: "[Conversation summary]\n" + summary},
	})
	b.saveConversations()

	before := len(msgs)
	return b.sendMessage(ctx, chatID, fmt.Sprintf(
		"Summarized %d messages into 1.\n\nSummary:\n%s", before, summary,
	))
}
