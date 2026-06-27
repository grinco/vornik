// Package telegram provides a Telegram bot integration for vornik.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/idfmt"
	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/safepath"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/sessionstore"
)

// Common errors.
var (
	ErrEmptyToken        = errors.New("bot token cannot be empty")
	ErrUserNotAllowed    = errors.New("user not allowed")
	ErrRateLimitExceeded = errors.New("rate limit exceeded")
	ErrEmptyMessage      = errors.New("message text cannot be empty")
)

// AutonomyController manages per-project autonomous task creation.
// Implemented by autonomy.Manager.
type AutonomyController interface {
	EnableProject(projectID string) error
	DisableProject(projectID string) error
	IsAutonomyEnabled(projectID string) bool
}

const telegramLongPollTimeout = 30 * time.Second
const telegramHTTPTimeout = 35 * time.Second

// UserAccess mirrors config.UserAccess but is accepted via the
// BotConfig map so the telegram package doesn't depend on config.
// Converter in service/container.go copies the fields verbatim.
type UserAccess struct {
	Allowed  bool
	Projects []string
}

// Wildcard reports unrestricted project access (Projects contains "*").
func (u UserAccess) Wildcard() bool {
	for _, p := range u.Projects {
		if p == "*" {
			return true
		}
	}
	return false
}

// CanAccessProject answers whether the user may /project into or
// otherwise see projectID. Wildcard users match any id; scoped users
// match only exact entries in Projects.
func (u UserAccess) CanAccessProject(projectID string) bool {
	if !u.Allowed {
		return false
	}
	if u.Wildcard() {
		return true
	}
	for _, p := range u.Projects {
		if p == projectID {
			return true
		}
	}
	return false
}

// BotConfig holds Telegram bot configuration.
type BotConfig struct {
	Token             string
	AllowedUsers      map[int64]UserAccess
	RateLimit         int           // requests per minute per user
	MaxHistory        int           // hard message-count cap per conversation
	MaxHistoryTokens  int           // soft token budget for trim; 0 disables
	MaxToolIterations int           // dispatcher tool-call loop cap; 0 = default (10)
	SessionPath       string        // path for conversation persistence (empty = disabled)
	SessionTTL        time.Duration // auto-expire idle sessions; 0 = disabled
	// DispatchTimeout caps how long the bot waits for the LLM to produce
	// a reply to one Telegram message. Zero defaults to 5 minutes — short
	// enough that a stalled model fails visibly, long enough for the
	// default-curated agent/coordinator models. Configurable because some
	// deployments route through slower upstreams (BAG → Bedrock with
	// thinking enabled) where 5m isn't enough.
	DispatchTimeout time.Duration
	// DispatcherProjectID, when set, pins every dispatcher LLM
	// usage row's project_id to a single project (typically a
	// dedicated assistant project). Without this, every chat
	// turn's cost lands on whichever project the conversation is
	// pinned to, mixing chat overhead into per-project spend
	// dashboards on projects that may have no automation enabled.
	// Active project still drives MCP / memory / budget /
	// autonomy — only billing attribution moves.
	DispatcherProjectID string
	// WebUIBaseURL (optional) is the externally-reachable base URL
	// of this daemon's web UI — e.g. "https://vornik.example.com".
	// The 2026.6.0 /start onboarding wizard uses it to render a
	// clickable link to /ui/projects/new for new users without
	// projects yet. Empty falls back to a relative-path hint that
	// works for self-hosted operators.
	WebUIBaseURL string
}

// Message represents a Telegram message.
type Message struct {
	ID       int64
	ChatID   int64
	UserID   int64
	Username string
	Text     string
	FileID   string // Telegram file_id if a document was attached
	FileName string // Original file name
	// ReplyToMessageID is the message_id this message is a reply
	// to (Telegram's "swipe to reply" feature). Used by the
	// conversational task lifecycle (Phase 28) to route replies
	// to specific tasks via notifMessageMap.
	ReplyToMessageID int64
	// MessageThreadID is non-zero when the message was posted
	// inside a Telegram Forum Topic. Phase 29's forum routing
	// uses it to look up the task whose topic this message
	// belongs to and route the reply to task_messages without
	// requiring a reply-to-message gesture from the operator.
	MessageThreadID int64
	// IsVoice marks the inbound as a voice (or audio) attachment so
	// HandleMessage routes it through the STT provider instead of
	// the document/photo path. Set by HandleUpdate when the upstream
	// payload had a non-empty voice or audio field. Also serialised
	// onto ChannelMessage.ChannelSpecific["voice.inbound"] on
	// successful transcription so downstream consumers can branch
	// on it.
	IsVoice bool
	// VoiceHint carries the inbound MIME / sample-rate signal so the
	// STT provider can short-circuit its container probe. Zero
	// value is fine (the provider treats it as a hint, not a
	// constraint).
	VoiceHint voiceHint
	// VoiceTranscript captures the STT-emitted transcript so
	// downstream translation (MessageToChannelMessage) can stamp
	// the voice.* tags on ChannelSpecific. Populated by
	// handleVoiceAttachment on success; zero value otherwise.
	VoiceTranscript voiceTranscript
}

// voiceTranscript mirrors voice.Transcript at the package boundary.
// Kept local so this file doesn't import the voice package — the
// voice integration lives in voice.go.
type voiceTranscript struct {
	Text       string
	Language   string
	DurationMs int64
	Confidence float64
}

// voiceHint mirrors voice.Hint at the package boundary so this file
// doesn't import the voice package (the voice integration lives in
// voice.go in this package).
type voiceHint struct {
	MimeType     string
	SampleRateHz int
}

// TelegramDocument represents a file attachment.
type TelegramDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// TelegramPhotoSize represents one resolution of a photo.
type TelegramPhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size,omitempty"`
}

// Update represents a Telegram update (webhook payload).
type Update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		ID int64 `json:"message_id"`
		// MessageThreadID is non-zero for messages posted inside
		// a Forum Topic. Phase 29 uses it to route replies to
		// the task whose topic this thread belongs to.
		MessageThreadID int64 `json:"message_thread_id,omitempty"`
		Chat            struct {
			ID int64 `json:"id"`
		}
		From struct {
			ID       int64  `json:"id"`
			Username string `json:"username,omitempty"`
		}
		Text     string              `json:"text"`
		Document *TelegramDocument   `json:"document,omitempty"`
		Photo    []TelegramPhotoSize `json:"photo,omitempty"`
		// Voice / Audio are populated for inbound voice messages and
		// general audio attachments respectively. The MVP voice
		// pipeline (slice 3) treats both as transcribable inputs and
		// routes them through the STT provider. Either field being
		// non-nil and non-empty FileID overrides the Document /
		// Photo branches in HandleUpdate.
		Voice          *TelegramVoice `json:"voice,omitempty"`
		Audio          *TelegramAudio `json:"audio,omitempty"`
		Caption        string         `json:"caption,omitempty"`
		ReplyToMessage *struct {
			ID int64 `json:"message_id"`
		} `json:"reply_to_message,omitempty"`
	} `json:"message"`
	// CallbackQuery is set when the update originates from an
	// inline-keyboard button click (added 2026.6.0 for the
	// SaaS-readiness Telegram surface). One Update carries
	// either a Message OR a CallbackQuery, never both.
	CallbackQuery *struct {
		ID   string `json:"id"`
		From struct {
			ID       int64  `json:"id"`
			Username string `json:"username,omitempty"`
		} `json:"from"`
		Message *struct {
			ID   int64 `json:"message_id"`
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
		Data string `json:"data"`
	} `json:"callback_query,omitempty"`
}

// SendMessageRequest is the request body for sending a message.
type SendMessageRequest struct {
	ChatID      int64                 `json:"chat_id"`
	Text        string                `json:"text"`
	ParseMode   string                `json:"parse_mode,omitempty"`
	ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// AnswerCallbackQueryRequest is the request body for Telegram's
// answerCallbackQuery API. Sent in response to every
// CallbackQuery update to acknowledge the click — without it
// the user's Telegram client shows the button as "loading"
// for ~15 seconds before timing out. Text (when set) shows as a
// transient toast at the top of the chat.
type AnswerCallbackQueryRequest struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
	ShowAlert       bool   `json:"show_alert,omitempty"`
}

// SendMessageResponse is the response from sendMessage API.
type SendMessageResponse struct {
	OK     bool `json:"ok"`
	Result struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		}
	} `json:"result"`
	Description string `json:"description,omitempty"`
}

// GetUpdatesResponse is the response from getUpdates API.
type GetUpdatesResponse struct {
	OK     bool     `json:"ok"`
	Result []Update `json:"result"`
}

// Bot is a Telegram bot client.
type Bot struct {
	config     BotConfig
	httpClient *http.Client

	// receiver is the ConversationChannel-driven dispatcher entry
	// point. Wired by the service container at boot via SetReceiver;
	// HandleMessage and triggerFollowup route every dispatcher-bound
	// turn through it. A nil receiver means the bot has no LLM hop
	// (degraded boot / minimal tests) — HandleMessage returns a
	// "dispatcher not configured" reply in that case.
	receiver        conversation.Receiver
	memorySearcher  dispatcher.MemorySearcher
	memoryCorrector dispatcher.MemoryCorrector
	auditRepo       dispatcher.AuditRepository         // optional; enables audit log for dispatcher tool calls
	llmUsageRepo    persistence.TaskLLMUsageRepository // optional; enables budget enforcement in create_task
	pricingTable    *pricing.Table                     // optional; enables cost calculation on dispatcher usage rows
	rateLimiter     ratelimit.ProjectLimiter           // optional; enables rate-limit enforcement in create_task
	defaultModel    string                             // VORNIK_LLM_MODEL fallback used by dispatcher cost forecast
	llmClient       chat.Provider                      // LLM client used for in-bot operations like /summarize
	compactor       chat.Compactor                     // optional; enables read-path conversation compaction (else legacy truncation)
	// intentJudgeRepo persists two-tier judge verdicts. nil
	// disables the judge entirely (heuristic + LLM both skipped).
	intentJudgeRepo persistence.IntentVerdictRepository
	// intentJudgeModel pins the LLM refiner's model. Empty leaves
	// the chat router's default in place — small OSS classifiers
	// (gpt-oss-20b, gemma-4-26b) are recommended.
	intentJudgeModel string

	mu             sync.RWMutex
	conversations  map[int64]*chat.Conversation
	activeProjects map[int64]string
	receiverLocks  map[int64]*sync.Mutex
	rateLimits     map[int64]*rateLimitEntry

	// sessionPersister (optional) DB-backs the in-memory
	// conversations + activeProjects maps. Wired via
	// WithSessionPersister at bot construction; nil keeps the
	// pre-feature in-memory-only behaviour for tests + opt-out
	// deployments. Channel kind on the row is "telegram"; the
	// session_id is the chat_id stringified.
	//
	// Writes happen at SessionStore.Append (write-through after
	// the in-memory side effects) + resetConversation (delete).
	// Reads happen at SessionStore.Load via hydrateSession when
	// the in-memory cache is empty for the chat.
	sessionPersister *sessionstore.Persister

	// leaderGate guards the long-poll loop in multi-replica
	// deployments. When set, pollLoop only calls getUpdates while
	// IsLeader() reports true — otherwise it idles, waiting for
	// the lease to flip. Nil (single-process default) leaves the
	// gate open. Satisfied by *leaderelection.Elector; the narrow
	// interface lives next to the bot so this file doesn't pull
	// the leaderelection package into its import set.
	leaderGate LeaderGate

	// pollerStateRepo persists the long-poll offset watermark
	// across daemon restarts + replica failover. nil keeps the
	// pre-feature in-memory-only behaviour (offset resets to 0 on
	// restart; Telegram replays queued updates). Wired via
	// WithPollerStateRepository.
	pollerStateRepo persistence.TelegramPollerStateRepository

	// pollerBotID is the bot identifier used as the key in the
	// telegram_poller_state table. Defaults to the bot's
	// @username; multi-bot deployments override via
	// WithPollerStateRepository to disambiguate.
	pollerBotID string

	// verbosity is the per-chat notification verbosity preference.
	// Values: "silent" (no task-completion pings; dispatcher's
	// wait_for_task still works), "short" (one-liner per task),
	// "full" (rich notification — the legacy default). Empty
	// string is treated as "full" so deployments without
	// /verbose configured see the historical behaviour.
	verbosity   map[int64]string
	mcpManager  dispatcher.MCPExecutor // MCP tool routing (optional)
	autonomyMgr AutonomyController     // optional; enables /autopilot command

	// secretsDetector (optional) scans every outbound chat message
	// and redacts findings before they hit the wire. Backstop for
	// anything earlier checkpoints missed (autonomy summaries,
	// memory_search results that cited a leaked .env, error texts
	// from a misconfigured project, etc.). Nil disables the layer
	// — preserves the pre-feature behaviour for opt-out
	// deployments.
	secretsDetector secrets.Detector

	// voice (optional) wires STT + TTS providers for inbound voice
	// transcription and outbound voice synthesis. Both halves are
	// independently nilable — see VoiceProviders. Wired via
	// WithVoiceProviders. voiceTracker remembers per-chat which
	// session is currently in voice-reply mode (latest inbound was a
	// voice message); read by Channel.Send to route replies through
	// sendVoice when appropriate. Both are nil-safe for boot paths
	// that skip the WithVoiceProviders option.
	voice        VoiceProviders
	voiceTracker *voiceInboundTracker

	// repos and registry are kept so the bot can pass them to the dispatcher
	// and for getProjectList (used when building Request.Projects).
	taskRepo      persistence.TaskRepository
	execRepo      persistence.ExecutionRepository
	artifactRepo  persistence.ArtifactRepository
	artifactStore dispatcher.InputArtifactStore // optional; enables snapshotting input_files into durable storage
	watcherRepo   persistence.TaskWatcherRepository
	registry      *registry.Registry
	// Phase 28 — conversational task lifecycle. taskMessageRepo
	// + rescheduler enable per-task reply routing. notifTracker
	// maps notification message_id → task_id so replies can find
	// the right thread. All nil-safe.
	taskMessageRepo persistence.TaskMessageRepository
	rescheduler     Rescheduler
	notifTracker    *taskNotifTracker

	// Operator-profile cross-channel linking (Phase A finalisation,
	// 2026-05-25). When all three are wired, the /link command is
	// enabled — it issues OTPs against the dispatcher's singleton
	// store and finalises links via the merge primitive in
	// internal/dispatcher.PerformOperatorLink. Nil-safe; missing
	// repos disable the command with a helpful operator message.
	operatorProfiles      persistence.OperatorProfileRepository
	operatorIdentityLinks persistence.OperatorIdentityLinkRepository
	profileUseAudit       persistence.ProfileUseAuditRepository

	// Phase 29 — Telegram Forum Topics. forumChatID is the
	// supergroup that receives per-task topics. threadRepo
	// persists the (chat_id, thread_id) → task_id mapping so
	// inbound forum replies route to the right task across bot
	// restarts. forumIconColor is the topic icon palette value
	// (see config.TelegramConfig.ForumTopicIconColor).
	//
	// All three are nil/zero-safe — when forumChatID == 0 or
	// threadRepo == nil, forum features are disabled and the
	// bot keeps its pre-Phase-29 behaviour. forumCreateLock
	// serialises createForumTopic calls per task_id to avoid
	// orphan topics when two notifications race; the DB
	// UNIQUE(chat_id, thread_id) is the durable guard.
	forumChatID      int64
	forumIconColor   int
	threadRepo       persistence.TelegramThreadRepository
	forumCreateMu    sync.Mutex
	forumCreateInFly map[string]chan struct{}
	// forumSentArtifacts dedupes per-(thread_id, artifact_id) so
	// a task that notifies twice (e.g. AWAITING_INPUT then
	// COMPLETED — lead_handoff.go and workflow.go both fire
	// NotifyTaskCompleted) doesn't upload the same file twice
	// into the group thread. 2026-05-16 bug fix. In-memory only;
	// daemon restart wipes the set, which is the right tradeoff
	// (common case fixed, rare cross-restart re-send no worse
	// than today). Guarded by forumMu alongside the set itself.
	forumMu            sync.Mutex
	forumSentArtifacts map[int64]map[string]struct{}

	baseURL              string
	logger               zerolog.Logger
	metrics              *Metrics
	projectWorkspacePath string

	// budgetAlertMu guards budgetAlertsSent. One alert per
	// (projectID, period, level, period-key) — period-key is the
	// local-TZ day (YYYY-MM-DD) or month (YYYY-MM). Map survives for
	// the bot's lifetime; restart clears it, so first breach after
	// a restart re-alerts (acceptable — operators want to know).
	budgetAlertMu    sync.Mutex
	budgetAlertsSent map[string]struct{}

	// followupMu guards pendingFollowups and chatUsers — both are
	// touched from the dispatcher tool path (via RegisterFollowup)
	// AND the executor's NotifyTaskCompleted path, so a dedicated
	// mutex avoids a deadlock with the wider bot.mu.
	followupMu sync.Mutex
	// pendingFollowups maps task_id → context for auto-resuming the
	// chat conversation when the task reaches a terminal status.
	// Populated by RegisterFollowup when create_task is called with
	// await_completion=true. Drained by NotifyTaskCompleted.
	pendingFollowups map[string]followupContext
	// chatUsers remembers the most recent telegram user_id for each
	// chat_id so the auto-resume path can rebuild the per-user
	// project scope (lead prompt, project list, allowed projects).
	// Personal chats have chat_id == user_id, so this map is only
	// load-bearing for group chats; we still populate it
	// unconditionally for symmetry.
	chatUsers map[int64]int64
	// chatTurnOutcomes accumulates terminal task outcomes by their
	// originating dispatcher turn (Task.ChatTurnID, set by the
	// dispatcher's create_task tool from the per-turn context). When
	// several tasks scheduled in one turn terminate while the
	// dispatcher is mid-reply on a parent turn, they pile up here
	// and get delivered as a SINGLE coalesced synthetic turn once
	// the chat lock frees up — rather than firing N separate
	// turns, one per task. See 2026-05-21 watchlist incident.
	chatTurnOutcomes map[string][]taskOutcome
	// chatTurnDelivering tracks turn ids that already have a
	// delivery goroutine in flight, so a second completion for the
	// same turn just appends to chatTurnOutcomes rather than
	// spawning a duplicate deliverer.
	chatTurnDelivering map[string]bool

	// Fill-notification debouncer. Trading_fills rows for the
	// same order arriving within fillDebounceWindow are
	// collapsed into a single Telegram message — a partially-
	// filled order doesn't spam the operator with one message
	// per leg. State is per-(project, order_id); buffers and
	// timers cleared on flush.
	fillNotifyMu     sync.Mutex
	fillNotifyBuf    map[string][]*persistence.TradingFill
	fillNotifyTimers map[string]*time.Timer

	running  bool
	stopChan chan struct{}
	wg       sync.WaitGroup
}

// fillDebounceWindow is the trailing-edge debounce duration for
// per-order fill notifications. Each new fill on the same order
// resets the timer; once no new fill arrives for this long, the
// buffered fills are aggregated into one message and sent.
const fillDebounceWindow = 2 * time.Second

// rateLimitEntry tracks rate limit state for a user.
type rateLimitEntry struct {
	mu          sync.Mutex
	count       int
	windowStart time.Time
}

// BotOption configures the bot.
type BotOption func(*Bot)

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(hc *http.Client) BotOption {
	return func(b *Bot) {
		b.httpClient = hc
	}
}

// WithTaskRepository sets the task repository.
func WithTaskRepository(repo persistence.TaskRepository) BotOption {
	return func(b *Bot) {
		b.taskRepo = repo
	}
}

// WithIntentJudgeRepository wires the two-tier intent judge's
// verdict persistence layer. When set, the dispatcher fires the
// heuristic tier before every tool call and (when the chat
// client is wired) the async LLM refiner; both verdicts persist
// to intent_verdicts for calibration. nil disables both tiers.
//
// model is the LLM model id the refiner uses; empty leaves the
// chat router's default in place (recommended: a small OSS
// classifier).
func WithIntentJudgeRepository(repo persistence.IntentVerdictRepository, model string) BotOption {
	return func(b *Bot) {
		b.intentJudgeRepo = repo
		b.intentJudgeModel = model
	}
}

// Rescheduler is the narrow interface the per-task reply router
// uses to wake the scheduler after a re-queue. Same shape as the
// api/ui rescheduler — separate type to avoid cross-package imports.
type Rescheduler interface {
	Wake()
}

// WithTaskMessageRepository wires the conversational task
// lifecycle backing for reply routing (Phase 28).
func WithTaskMessageRepository(repo persistence.TaskMessageRepository) BotOption {
	return func(b *Bot) {
		b.taskMessageRepo = repo
	}
}

// WithOperatorLinkRepositories wires the per-operator profile +
// identity-link + profile-use-audit repos into the bot so the
// `/link` slash command can issue OTPs and finalise cross-
// channel merges. All three are required for /link to function;
// any nil disables the command with a clear operator message.
//
// See https://docs.vornik.io (Phase A
// /link slash command).
func WithOperatorLinkRepositories(
	profiles persistence.OperatorProfileRepository,
	links persistence.OperatorIdentityLinkRepository,
	audit persistence.ProfileUseAuditRepository,
) BotOption {
	return func(b *Bot) {
		b.operatorProfiles = profiles
		b.operatorIdentityLinks = links
		b.profileUseAudit = audit
	}
}

// WithSessionPersister DB-backs the bot's per-chat conversation
// + active-project state via the shared channel_sessions table.
// Nil keeps the pre-feature in-memory-only behaviour so tests +
// opt-out deployments work unchanged.
//
// When wired, every SessionStore.Append writes the post-turn
// history through to Postgres and SessionStore.Load rehydrates
// the in-memory cache on the first inbound message after a
// daemon restart or replica failover.
func WithSessionPersister(p *sessionstore.Persister) BotOption {
	return func(b *Bot) {
		b.sessionPersister = p
	}
}

// LeaderGate is the narrow contract pollLoop uses to skip
// getUpdates calls on non-leader daemons. Satisfied by
// *leaderelection.Elector. The cheap IsLeader() bit gates the
// idle loop; pollLoop additionally type-asserts the gate to
// leaderelection.EpochVerifier for the fail-closed B1 fence, so a
// gate exposing only IsLeader() keeps pre-fence behaviour.
type LeaderGate interface {
	IsLeader() bool
}

// WithLeaderGate gates the long-poll loop on the supplied leader
// elector. Non-leader daemons idle (sleep + retry on a short
// interval) instead of polling Telegram, eliminating the
// double-reply problem in a multi-replica deployment. Nil
// (single-process default) leaves the loop running on every
// daemon — the legacy behaviour.
//
// Must be combined with WithPollerStateRepository for fully
// duplicate-free behaviour; the gate alone protects steady-
// state but a leader-failover would reset the in-memory offset
// to 0 and replay queued updates.
func WithLeaderGate(g LeaderGate) BotOption {
	return func(b *Bot) {
		b.leaderGate = g
	}
}

// WithPollerStateRepository persists the long-poll offset
// watermark to Postgres so leader-failover resumes from the
// last-confirmed offset rather than replaying queued updates.
// botID is the key into the telegram_poller_state table;
// pass the bot's @username for single-bot deployments or an
// operator-supplied label for multi-bot ones. Empty botID
// disables persistence (same as passing a nil repo).
func WithPollerStateRepository(repo persistence.TelegramPollerStateRepository, botID string) BotOption {
	return func(b *Bot) {
		b.pollerStateRepo = repo
		b.pollerBotID = botID
	}
}

// WithRescheduler wires the scheduler-wake hook used after
// per-task replies re-queue a task.
func WithRescheduler(r Rescheduler) BotOption {
	return func(b *Bot) {
		b.rescheduler = r
	}
}

// WithForumChatID enables Telegram Forum Topic routing for tasks.
// chatID must be a supergroup with Topics enabled in Telegram.
// iconColor is one of the Telegram palette ints (see
// config.TelegramConfig.ForumTopicIconColor); invalid values cause
// the bot to omit the field when creating topics.
//
// When chatID == 0, forum features are disabled.
func WithForumChatID(chatID int64, iconColor int) BotOption {
	return func(b *Bot) {
		b.forumChatID = chatID
		b.forumIconColor = iconColor
	}
}

// WithTelegramThreadRepository wires the Postgres-backed
// (chat_id, thread_id) → task_id map. Required for forum routing —
// without it, b.forumEnabled() returns false even when
// WithForumChatID is set.
func WithTelegramThreadRepository(r persistence.TelegramThreadRepository) BotOption {
	return func(b *Bot) {
		b.threadRepo = r
	}
}

// WithExecutionRepository sets the execution repository.
func WithExecutionRepository(repo persistence.ExecutionRepository) BotOption {
	return func(b *Bot) {
		b.execRepo = repo
	}
}

// WithLogger sets the logger for the bot.
func WithLogger(logger zerolog.Logger) BotOption {
	return func(b *Bot) {
		b.logger = logger
	}
}

// WithArtifactRepository sets the artifact repository for file sending.
func WithArtifactRepository(repo persistence.ArtifactRepository) BotOption {
	return func(b *Bot) { b.artifactRepo = repo }
}

// WithArtifactStore wires a store that snapshots user-supplied input
// files (Telegram uploads) into durable artifact storage when the
// dispatcher creates a task. Without it, input_files in the task
// payload point at the original host path, which is fine for retries
// while the source still exists but loses uploads after /tmp reaping
// or workspace cleanup.
func WithArtifactStore(s dispatcher.InputArtifactStore) BotOption {
	return func(b *Bot) { b.artifactStore = s }
}

// WithProjectWorkspacePath sets the base path for per-project workspaces (file uploads).
func WithProjectWorkspacePath(path string) BotOption {
	return func(b *Bot) { b.projectWorkspacePath = path }
}

// SetMetrics updates the Prometheus metrics on an already-created bot.
// Used when observability is initialized after the bot is created.
func (b *Bot) SetMetrics(m *Metrics) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.metrics = m
}

// WithRegistry sets the project registry for tool use.
func WithRegistry(reg *registry.Registry) BotOption {
	return func(b *Bot) {
		b.registry = reg
	}
}

// WithTaskWatcherRepository sets the watcher repo for completion notifications.
func WithTaskWatcherRepository(repo persistence.TaskWatcherRepository) BotOption {
	return func(b *Bot) {
		b.watcherRepo = repo
	}
}

// WithMCPManager sets the MCP tool executor for the dispatcher.
func WithMCPManager(m dispatcher.MCPExecutor) BotOption {
	return func(b *Bot) {
		b.mcpManager = m
	}
}

// WithAuditRepository enables tool-call audit logging for the dispatcher.
func WithAuditRepository(repo dispatcher.AuditRepository) BotOption {
	return func(b *Bot) {
		b.auditRepo = repo
	}
}

// WithLLMUsageRepository enables per-project budget enforcement on the
// create_task dispatcher tool.
func WithLLMUsageRepository(repo persistence.TaskLLMUsageRepository) BotOption {
	return func(b *Bot) {
		b.llmUsageRepo = repo
	}
}

// WithPricing wires the model pricing table so dispatcher LLM-usage rows
// get cost_usd populated. Without it, dispatcher rows still record tokens
// but cost aggregates will under-count — the rows will appear with
// cost_usd=0 in the spend panel.
func WithPricing(t *pricing.Table) BotOption {
	return func(b *Bot) {
		b.pricingTable = t
	}
}

// WithRateLimiter wires the shared task-creation rate limiter so the
// dispatcher's create_task tool enforces per-minute/per-hour caps
// alongside autonomy and the API.
func WithRateLimiter(l ratelimit.ProjectLimiter) BotOption {
	return func(b *Bot) {
		b.rateLimiter = l
	}
}

// WithDefaultModel pins the daemon's VORNIK_LLM_MODEL fallback so
// the dispatcher's cost forecast can resolve roles whose swarm
// config doesn't override the model. Optional — empty disables
// the pricing fallback path for unmodelled steps.
func WithDefaultModel(model string) BotOption {
	return func(b *Bot) {
		b.defaultModel = model
	}
}

// WithCompactor enables read-path conversation compaction: overflow turns
// that don't fit the history token budget are condensed into one deterministic
// topic gist instead of being dropped. Unset → legacy truncation. The service
// container passes this only when chat.compaction.enabled is true.
func WithCompactor(c chat.Compactor) BotOption {
	return func(b *Bot) {
		b.compactor = c
	}
}

// tuneConversation applies the configured history controls to a conversation:
// the soft token budget and, when wired, the read-path compactor. Centralised
// so every conversation-creation/load site stays consistent.
func (b *Bot) tuneConversation(conv *chat.Conversation) {
	if conv == nil {
		return
	}
	if b.config.MaxHistoryTokens > 0 {
		conv.SetMaxTokens(b.config.MaxHistoryTokens)
	}
	if b.compactor != nil {
		conv.SetCompactor(b.compactor)
	}
}

// WithMemorySearcher gives the dispatcher direct access to project memory
// so the memory_search tool can answer user questions without scheduling
// a research task. When unset, memory_search reports the subsystem as
// disabled and the dispatcher falls back to its task-scheduling path.
func WithMemorySearcher(m dispatcher.MemorySearcher) BotOption {
	return func(b *Bot) {
		b.memorySearcher = m
	}
}

// WithMemoryCorrector enables the memory_correct dispatcher tool — the
// LLM calls it when the user says "fact X is wrong, it's actually Y" to
// refute matching memory chunks and store the correction as a verified
// chunk. Without this wiring the tool surfaces a "not enabled" message.
func WithMemoryCorrector(c dispatcher.MemoryCorrector) BotOption {
	return func(b *Bot) {
		b.memoryCorrector = c
	}
}

// SetAutonomyManager wires the autonomy manager into an already-created bot.
// Called after initAutonomy completes, since the manager is initialized after the bot.
func (b *Bot) SetAutonomyManager(mgr AutonomyController) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.autonomyMgr = mgr
}

// registerBotMenu sets the Telegram bot command menu via setMyCommands API.
// botMenuCommands is the curated set advertised to Telegram clients via
// setMyCommands. It is intentionally a SUBSET of all handled commands (see
// handleHelp for the full list) — but every entry MUST be a real, handled
// command. TestBotMenuCommandsAreDocumented enforces that, after a dead
// `streaming` entry once shipped advertised-but-unhandled (it fell through to
// the LLM as a chat message).
func botMenuCommands() []map[string]string {
	return []map[string]string{
		{"command": "new", "description": "Start a fresh session"},
		{"command": "context", "description": "Show session stats"},
		{"command": "summarize", "description": "Compress history into a summary (saves tokens)"},
		{"command": "undo", "description": "Remove last exchange"},
		{"command": "forget", "description": "Drop last N messages"},
		{"command": "pin", "description": "Pin a persistent instruction"},
		{"command": "project", "description": "Show or switch project"},
		{"command": "autopilot", "description": "Enable/disable autonomous mode for active project"},
		{"command": "inbox", "description": "List tasks awaiting your input"},
		{"command": "help", "description": "Show available commands"},
	}
}

func (b *Bot) registerBotMenu(ctx context.Context) {
	commands := botMenuCommands()
	body, _ := json.Marshal(map[string]any{"commands": commands})
	url := fmt.Sprintf("%s/setMyCommands", b.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		b.logger.Warn().Err(err).Msg("failed to create setMyCommands request")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		b.logger.Warn().Err(err).Msg("failed to register bot commands")
		return
	}
	_ = resp.Body.Close()
	b.logger.Info().Int("commands", len(commands)).Msg("telegram bot menu registered")
}

// Receiver returns the ConversationChannel receiver wired to this
// bot. Nil before the service container's wireTelegramReceiver
// runs; HandleMessage refuses dispatcher-bound messages until set.
func (b *Bot) Receiver() conversation.Receiver {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.receiver
}

// SetReceiver wires a conversation.Receiver into the bot.
// HandleMessage's non-slash branch and the auto-resume follow-up
// route every dispatcher-bound turn through it. Setting nil
// disables dispatcher-bound chat (HandleMessage returns "not
// configured" until a receiver lands).
func (b *Bot) SetReceiver(r conversation.Receiver) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.receiver = r
}

// WatchTask registers the given chat to receive a notification when the task completes.
func (b *Bot) WatchTask(taskID string, chatID int64) {
	if b.watcherRepo == nil {
		return
	}
	_ = b.watcherRepo.Watch(context.Background(), taskID, chatID)
}

// followupContext records the chat that asked for an auto-resume
// after a specific task completes. ProjectID is only stored for
// logging — the chat's active-project state (which may have moved
// on while the task ran) wins on actual resume.
type followupContext struct {
	chatID    int64
	projectID string
	createdAt time.Time
}

// taskOutcome captures everything triggerFollowup needs to build a
// synthetic resume turn for one task. Bundled in chatTurnOutcomes
// so several outcomes from the same dispatcher turn can be delivered
// in a single synthetic turn (coalescing).
type taskOutcome struct {
	task    *persistence.Task
	success bool
	message string
	fu      followupContext
}

// RegisterFollowup records that the given chat is waiting for the
// named task to finish so the dispatcher can resume the
// conversation with the task's result. Implements
// dispatcher.FollowupRegistrar — wired to the dispatcher in NewBot.
//
// Idempotent: re-registering the same task is harmless (the second
// call wins, which is the right behaviour if the same chat
// schedules the same task twice in quick succession).
func (b *Bot) RegisterFollowup(chatID int64, taskID, projectID string) {
	if b == nil || taskID == "" || chatID == 0 {
		return
	}
	b.followupMu.Lock()
	if b.pendingFollowups == nil {
		b.pendingFollowups = make(map[string]followupContext)
	}
	b.pendingFollowups[taskID] = followupContext{
		chatID:    chatID,
		projectID: projectID,
		createdAt: time.Now(),
	}
	b.followupMu.Unlock()
	b.logger.Info().
		Str("task_id", taskID).
		Int64("chat_id", chatID).
		Str("project", projectID).
		Msg("follow-up registered: chat will auto-resume on task completion")
}

// findActiveDescendant walks the task's children (and their
// children) and returns the ID of the most recent
// non-terminal descendant, or empty string if every
// descendant has already terminated. Used by triggerFollowup
// to defer the dispatcher resume until the chain reaches a
// leaf — route handoffs don't block the parent, so the
// parent's terminal status arrives before the children
// finish producing the real artifacts.
//
// Walk semantics: BFS from the parent. Stop at the first
// non-terminal task we find (most-recent at that level wins
// because the children list is ordered newest-first by the
// repo). The bound is generous (32 nodes) — production
// hierarchies are flat enough that this is sufficient,
// but the bound prevents a pathological cycle from
// spinning forever.
//
// Errors are swallowed back to "" so the caller falls
// through to the legacy resume — losing a notification is
// worse than over-firing.
func (b *Bot) findActiveDescendant(parentID string) string {
	if b == nil || b.taskRepo == nil || parentID == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	const maxWalk = 32
	queue := []string{parentID}
	visited := map[string]struct{}{parentID: {}}
	for len(queue) > 0 && len(visited) < maxWalk {
		head := queue[0]
		queue = queue[1:]
		children, err := b.taskRepo.GetChildren(ctx, head)
		if err != nil {
			return ""
		}
		for _, child := range children {
			if child == nil {
				continue
			}
			if _, seen := visited[child.ID]; seen {
				continue
			}
			visited[child.ID] = struct{}{}
			if !isTerminalTaskStatus(child.Status) {
				return child.ID
			}
			queue = append(queue, child.ID)
		}
	}
	return ""
}

// isTerminalTaskStatus is a local helper because the
// persistence package doesn't expose this predicate. Kept
// in sync with the TaskStatus* constants — adding a new
// terminal status means updating both this and the
// scheduler.
func isTerminalTaskStatus(s persistence.TaskStatus) bool {
	switch s {
	case persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled:
		return true
	}
	return false
}

// claimFollowup atomically pops and returns the followup for taskID,
// or returns ok=false if none was registered. Done as a single
// critical section so two concurrent NotifyTaskCompleted calls
// (e.g. retry race) can't both fire the auto-resume.
func (b *Bot) claimFollowup(taskID string) (followupContext, bool) {
	b.followupMu.Lock()
	defer b.followupMu.Unlock()
	fu, ok := b.pendingFollowups[taskID]
	if ok {
		delete(b.pendingFollowups, taskID)
	}
	return fu, ok
}

// recordChatUser remembers the userID associated with a chatID so
// the auto-resume path can rebuild the per-user project scope. Called
// from HandleMessage on every inbound message.
func (b *Bot) recordChatUser(chatID, userID int64) {
	if b == nil || chatID == 0 || userID == 0 {
		return
	}
	b.followupMu.Lock()
	if b.chatUsers == nil {
		b.chatUsers = make(map[int64]int64)
	}
	b.chatUsers[chatID] = userID
	b.followupMu.Unlock()
}

// userIDForChat returns the most recent user_id seen for this
// chat_id, falling back to chatID itself when the bot hasn't seen
// a message from this chat yet (which is the common case for
// personal chats where chat_id == user_id anyway).
func (b *Bot) userIDForChat(chatID int64) int64 {
	if b == nil {
		return chatID
	}
	b.followupMu.Lock()
	defer b.followupMu.Unlock()
	if uid, ok := b.chatUsers[chatID]; ok && uid != 0 {
		return uid
	}
	return chatID
}

// triggerFollowup builds a synthetic user turn with the task's
// outcome and submits it to the dispatcher inbox so the
// conversation continues with the fresh data — independent of the
// chat's verbosity preference. The verbosity preference governs
// the *push notification* about the task (see NotifyTaskCompleted),
// not whether the originally-scheduled question gets answered;
// once the dispatcher set await_completion=true, the answer is
// owed regardless.
func (b *Bot) triggerFollowup(task *persistence.Task, success bool, message string) {
	if b == nil || task == nil {
		return
	}
	fu, ok := b.claimFollowup(task.ID)
	if !ok {
		return
	}

	// 2026-05-16 → 2026-05-21: the leaf-transfer workaround
	// (findActiveDescendant + pendingFollowups requeue) was the
	// pre-fix path that papered over strict-adaptive parents
	// completing before their children. The executor now pauses
	// the parent on WAITING_FOR_CHILDREN until every descendant
	// terminates (see commit 770df1d), so by the time
	// NotifyTaskCompleted lands here the parent's status truly
	// reflects the whole subtree. findActiveDescendant is kept
	// for now as a defense-in-depth probe — if it ever finds an
	// active descendant, the executor's contract has regressed
	// and an operator should see a loud log instead of silently
	// re-running the lead pick. (Hard removal lands in a later
	// commit once we have production telemetry confirming the
	// probe never fires.)
	if b.taskRepo != nil {
		if leaf := b.findActiveDescendant(task.ID); leaf != "" {
			b.logger.Warn().
				Str("parent_task_id", task.ID).
				Str("leaf_task_id", leaf).
				Int64("chat_id", fu.chatID).
				Msg("auto-resume: parent task COMPLETED but found active descendant — executor contract regression? defense-in-depth: transferring followup to leaf")
			b.followupMu.Lock()
			b.pendingFollowups[leaf] = followupContext{
				chatID:    fu.chatID,
				projectID: fu.projectID,
				createdAt: fu.createdAt,
			}
			b.followupMu.Unlock()
			return
		}
	}

	chatID := fu.chatID
	r := b.Receiver()
	if r == nil {
		b.logger.Warn().Str("task_id", task.ID).Int64("chat_id", chatID).
			Msg("auto-resume: receiver not configured — dropping followup")
		return
	}

	outcome := taskOutcome{task: task, success: success, message: message, fu: fu}

	// Coalesce by chat_turn_id when present: the same dispatcher
	// turn may have spawned several tasks; if they terminate while
	// the chat lock is held by a sibling reply, they should be
	// delivered as ONE synthetic turn instead of N. Tasks with no
	// chat_turn_id (legacy / API-initiated) keep the per-task
	// delivery shape.
	if task.ChatTurnID != nil && *task.ChatTurnID != "" {
		b.enqueueTurnOutcome(*task.ChatTurnID, outcome)
		return
	}

	syntheticText := b.composeSyntheticTurn([]taskOutcome{outcome})

	// Route the synthetic auto-resume turn through the receiver.
	// SessionStore reads existing conversation history; the receiver
	// appends the synthetic user turn before dispatching — so we do
	// NOT AddMessage here (would double it in the dispatcher's
	// history). UserID==0 marks the turn as server-internal so the
	// SessionStore skips its allowlist check.
	b.logger.Info().
		Str("task_id", task.ID).
		Int64("chat_id", chatID).
		Msg("auto-resume: routing follow-up through ConversationChannel receiver")
	go b.handleReceiverTurn(r, &Message{
		ChatID: chatID,
		UserID: 0,
		Text:   syntheticText,
	})
}

// enqueueTurnOutcome appends an outcome to the bucket for the given
// chat_turn_id. The first outcome (when no deliverer is in flight)
// also spawns the goroutine that will drain the bucket once the
// chat lock is free; subsequent outcomes just pile up. The
// deliverer drains AFTER acquiring the receiver lock so completions
// that arrive while it's waiting are included in the same batch.
func (b *Bot) enqueueTurnOutcome(turnID string, outcome taskOutcome) {
	if b == nil || turnID == "" {
		return
	}
	b.followupMu.Lock()
	if b.chatTurnOutcomes == nil {
		b.chatTurnOutcomes = make(map[string][]taskOutcome)
	}
	if b.chatTurnDelivering == nil {
		b.chatTurnDelivering = make(map[string]bool)
	}
	b.chatTurnOutcomes[turnID] = append(b.chatTurnOutcomes[turnID], outcome)
	queueLen := len(b.chatTurnOutcomes[turnID])
	delivering := b.chatTurnDelivering[turnID]
	if !delivering {
		b.chatTurnDelivering[turnID] = true
	}
	b.followupMu.Unlock()

	b.logger.Info().
		Str("task_id", outcome.task.ID).
		Str("chat_turn_id", turnID).
		Int64("chat_id", outcome.fu.chatID).
		Int("batch_size", queueLen).
		Bool("deliverer_already_running", delivering).
		Msg("auto-resume: enqueued outcome for coalesced delivery")

	if delivering {
		// Another goroutine is already on the hook to deliver this
		// turn's outcomes; it'll pick up this entry on drain.
		return
	}
	go b.deliverCoalescedTurn(turnID, outcome.fu.chatID)
}

// deliverCoalescedTurn waits for the receiver lock for the chat,
// then drains every outcome queued for the chat turn and routes
// them as one synthetic turn. Splitting the lock acquisition from
// the drain is the whole point: tasks that terminate while we're
// waiting on the lock will still be included in the batch we
// finally deliver.
func (b *Bot) deliverCoalescedTurn(turnID string, chatID int64) {
	if b == nil {
		return
	}
	r := b.Receiver()
	if r == nil {
		b.logger.Warn().Str("chat_turn_id", turnID).Int64("chat_id", chatID).
			Msg("auto-resume: receiver not configured — dropping coalesced batch")
		// Best-effort: still clear delivering + outcomes so a future
		// completion gets a fresh chance instead of being orphaned.
		b.followupMu.Lock()
		delete(b.chatTurnOutcomes, turnID)
		delete(b.chatTurnDelivering, turnID)
		b.followupMu.Unlock()
		return
	}

	lock := b.receiverLock(chatID)
	lock.Lock()
	defer lock.Unlock()

	// Drain-Receive loop (2026-05-29 fix for the
	// TestTriggerFollowup_CoalescesSameTurn race). Pre-fix the
	// deliverer cleared `delivering=false` during drain, then
	// called Receive. Outcomes that arrived while Receive was
	// running found delivering=false + spawned their own
	// deliverer — producing 2 Receive calls for what should be
	// ONE coalesced turn. Now: delivering stays true through the
	// entire deliverer's lifetime; new outcomes during Receive
	// are absorbed into the NEXT batch. delivering is cleared
	// atomically with the "bucket is empty" check so the next
	// triggerFollowup spawns a fresh deliverer.
	for {
		b.followupMu.Lock()
		outcomes := b.chatTurnOutcomes[turnID]
		delete(b.chatTurnOutcomes, turnID)
		if len(outcomes) == 0 {
			delete(b.chatTurnDelivering, turnID)
			b.followupMu.Unlock()
			return
		}
		b.followupMu.Unlock()

		syntheticText := b.composeSyntheticTurn(outcomes)

		b.logger.Info().
			Str("chat_turn_id", turnID).
			Int64("chat_id", chatID).
			Int("outcome_count", len(outcomes)).
			Msg("auto-resume: routing coalesced follow-up through ConversationChannel receiver")

		ctx, cancel := context.WithTimeout(context.Background(), b.effectiveDispatchTimeout())
		cm := MessageToChannelMessage(&Message{
			ChatID: chatID,
			UserID: 0,
			Text:   syntheticText,
		})
		start := time.Now()
		err := r.Receive(ctx, cm)
		cancel()
		if err != nil {
			b.logger.Warn().
				Err(err).
				Str("chat_turn_id", turnID).
				Int64("chat_id", chatID).
				Dur("duration", time.Since(start)).
				Msg("telegram receiver-path: coalesced Receive returned error")
			// On error, still clear delivering so the next
			// outcome gets a fresh chance. Don't loop again —
			// the upstream receiver said no, and the operator
			// sees the warn log.
			b.followupMu.Lock()
			delete(b.chatTurnDelivering, turnID)
			b.followupMu.Unlock()
			return
		}
		b.logger.Info().
			Str("chat_turn_id", turnID).
			Int64("chat_id", chatID).
			Dur("duration", time.Since(start)).
			Msg("telegram receiver-path: coalesced Receive completed")
		// Loop back: any outcomes that arrived during Receive
		// get drained + delivered as a second batch under the
		// same deliverer.
	}
}

// composeSyntheticTurn renders the synthetic user turn fed back to
// the dispatcher when one or more tasks terminate. Single-outcome
// batches keep the original wording; coalesced batches add a short
// preamble so the dispatcher LLM understands several tasks
// terminated together and answers once. Per-task body (status,
// error class, artifact list, last-status humanise) is unchanged.
func (b *Bot) composeSyntheticTurn(outcomes []taskOutcome) string {
	var sb strings.Builder
	if len(outcomes) > 1 {
		fmt.Fprintf(&sb, "[%d tasks from this turn terminated.]\n\n", len(outcomes))
	}
	for i, o := range outcomes {
		if i > 0 {
			sb.WriteString("\n---\n\n")
		}
		b.appendOutcomeBlock(&sb, o)
	}
	sb.WriteString("\nPlease continue answering my earlier question with this fresh data. If any task failed, explain what went wrong and offer next steps.")
	return sb.String()
}

// appendOutcomeBlock writes one task's outcome (status header,
// optional error, optional artifact list, last-status humanise)
// onto sb. Extracted from triggerFollowup so the single-task and
// coalesced paths share the body — keeping any future format
// change in one place.
func (b *Bot) appendOutcomeBlock(sb *strings.Builder, o taskOutcome) {
	task := o.task
	taskRef := idfmt.Short(task.ID)
	if o.success {
		fmt.Fprintf(sb, "[Task %s completed successfully.]\n", taskRef)
	} else {
		fmt.Fprintf(sb, "[Task %s did NOT complete successfully. Status: %s.]\n", taskRef, task.Status)
	}
	if !o.success && task.LastError != nil && *task.LastError != "" {
		errMsg := *task.LastError
		if len(errMsg) > 1500 {
			errMsg = errMsg[:1500] + "…"
		}
		fmt.Fprintf(sb, "Error: %s\n", errMsg)
	}
	if task.LastErrorClass != nil && *task.LastErrorClass != "" {
		fmt.Fprintf(sb, "Failure class: %s\n", *task.LastErrorClass)
	}
	if o.message != "" {
		humanized := humanizeTaskMessage(o.message)
		if len(humanized) > 800 {
			humanized = humanized[:800] + "…"
		}
		if humanized != "" {
			fmt.Fprintf(sb, "Last status: %s\n", humanized)
		}
	}
	if o.success && b.artifactRepo != nil {
		b.appendArtifactListing(sb, task)
	}
}

// appendArtifactListing renders the "Produced N artifact(s):" block
// for the synthetic followup turn. Filters to OUTPUT class (same
// rationale as before — transient classes confuse send_artifact).
//
// Falls back to descendant artifacts when the task's own row count
// is zero AND the task spawned children — the strict-adaptive
// route-parent case (2026-05-21 incident: T-6da5 / T-b129 / T-3d1e
// all completed with zero own-artifacts because the route step
// just delegates; the real output lived on the child task and the
// dispatcher LLM had nothing to read, so it told the user "no
// research data was produced" despite the child producing a
// perfectly good deliverable).
//
// When the rows come from descendants, each line is annotated with
// "from T-XXXX" so the LLM passes the right task_id to read_artifact.
func (b *Bot) appendArtifactListing(sb *strings.Builder, task *persistence.Task) {
	if b == nil || b.artifactRepo == nil || task == nil {
		return
	}
	listCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ownOutputs := b.listOutputArtifacts(listCtx, task.ID)
	if len(ownOutputs) > 0 {
		// Own artifacts present — use them verbatim. Per-row task_id
		// annotation isn't needed because every row belongs to
		// this task.
		fmt.Fprintf(sb, "\nProduced %d artifact(s):\n", len(ownOutputs))
		for _, a := range ownOutputs {
			sb.WriteString(formatArtifactLine(a, "") + "\n")
		}
		sb.WriteString("\nUse read_artifact(task_id, artifact_name) to fetch any of these into your context, or send_artifact to deliver the full file to the user.\n")
		return
	}

	// Own list is empty. If this task has descendants (route
	// parent / fan-out parent), aggregate THEIR OUTPUT artifacts
	// so the dispatcher LLM can read them.
	if b.taskRepo == nil {
		return
	}
	descArtifacts, descAttribution := b.collectDescendantOutputArtifacts(listCtx, task.ID)
	if len(descArtifacts) == 0 {
		return
	}
	fmt.Fprintf(sb, "\nProduced %d artifact(s) on descendant tasks:\n", len(descArtifacts))
	for _, a := range descArtifacts {
		owner := descAttribution[a.ID]
		sb.WriteString(formatArtifactLine(a, owner) + "\n")
	}
	sb.WriteString("\nEach row's task id is annotated — pass it to read_artifact(task_id, artifact_name) or send_artifact to fetch the file. This task is a route/fan-out parent; its real output lives on the child shown per row.\n")
}

// listOutputArtifacts returns OUTPUT-class artifacts for a single
// task. Errors are swallowed back to an empty slice — losing the
// listing degrades the dispatcher's reply but does not block the
// chat reply path.
func (b *Bot) listOutputArtifacts(ctx context.Context, taskID string) []*persistence.Artifact {
	arts, err := b.artifactRepo.List(ctx, persistence.ArtifactFilter{TaskID: &taskID, PageSize: 25})
	if err != nil || len(arts) == 0 {
		return nil
	}
	out := arts[:0:0]
	for _, a := range arts {
		if a.ArtifactClass == persistence.ArtifactClassOutput {
			out = append(out, a)
		}
	}
	return out
}

// collectDescendantOutputArtifacts walks the parent's descendants
// (BFS, bounded) and gathers each descendant's OUTPUT artifacts.
// Returns the merged list plus an attribution map (artifact ID →
// task ID) so the caller can annotate the rendered line with the
// owning task. Bounded by both the BFS step count and a per-list
// cap so a pathological tree can't blow up the synthetic turn.
func (b *Bot) collectDescendantOutputArtifacts(ctx context.Context, parentID string) ([]*persistence.Artifact, map[string]string) {
	const (
		maxWalk      = 32 // matches findActiveDescendant
		maxArtifacts = 50 // soft cap on synthetic-turn size
	)
	attribution := map[string]string{}
	var collected []*persistence.Artifact
	queue := []string{parentID}
	visited := map[string]struct{}{parentID: {}}
	for len(queue) > 0 && len(visited) < maxWalk {
		head := queue[0]
		queue = queue[1:]
		children, err := b.taskRepo.GetChildren(ctx, head)
		if err != nil {
			continue
		}
		for _, child := range children {
			if child == nil {
				continue
			}
			if _, seen := visited[child.ID]; seen {
				continue
			}
			visited[child.ID] = struct{}{}
			for _, a := range b.listOutputArtifacts(ctx, child.ID) {
				collected = append(collected, a)
				attribution[a.ID] = child.ID
				if len(collected) >= maxArtifacts {
					return collected, attribution
				}
			}
			queue = append(queue, child.ID)
		}
	}
	return collected, attribution
}

// formatArtifactLine renders one row of the synthetic turn's
// "Produced N artifact(s):" block. ownerTaskID is the empty string
// when every row belongs to the same task (no per-row task_id
// needed); when non-empty, the FULL task id is appended so the
// LLM can hand it directly to read_artifact / send_artifact. The
// 2026-05-21 watchlist incident proved the short form (T-XXXX)
// fails the tool's lookup ("failed to load task: not found"), so
// we ship the long form here even though it's noisier.
func formatArtifactLine(a *persistence.Artifact, ownerTaskID string) string {
	line := fmt.Sprintf("  - %s (%s", a.Name, a.ArtifactClass)
	if a.SizeBytes != nil {
		line += fmt.Sprintf(", %d bytes", *a.SizeBytes)
	}
	line += ")"
	if ownerTaskID != "" {
		line += fmt.Sprintf(" — task_id=%s", ownerTaskID)
	}
	return line
}

// NotifyTaskCompleted sends a completion message to all chats watching this task.
// Implements executor.CompletionNotifier.
func (b *Bot) NotifyTaskCompleted(_ context.Context, task *persistence.Task, success bool, message string) {
	// Use a detached context with a generous timeout so the notification
	// succeeds even when the execution context is already cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 2026-05-16 fix: triggerFollowup must fire regardless of
	// watcher state. The pre-fix early-returns below (no watcher
	// repo / GetWatchers err / empty chatIDs) skipped it, which
	// broke the adaptive-workflow leaf path entirely: when a parent
	// transfers its followup to a leaf, the leaf has no watchers
	// of its own, so the leaf's NotifyTaskCompleted hit the empty-
	// chatIDs branch and silently dropped the followup. Operator
	// symptom: the dispatcher's auto-resume reply never landed —
	// the chat saw the parent's "task completed" notification but
	// never the actual answer derived from the leaf's artifacts.
	defer b.triggerFollowup(task, success, message)

	// 2026-05-16 fix: when this task spawned active descendant
	// tasks (ROUTE delegation from the adaptive workflow), it is
	// a routing-housekeeping terminal. The real summary + artifacts
	// will land on the leaf. Transfer the parent's watchers to the
	// leaf so the leaf's completion lands the user-facing fanout on
	// the same chats, and suppress the parent's intermediate
	// "task completed" notification (both DM and forum thread) so
	// the operator sees ONE clean completion, not two.
	leafID := ""
	if b.taskRepo != nil {
		leafID = b.findActiveDescendant(task.ID)
	}

	// Phase 29 — forum topic fan-out. Only fires when this is a
	// leaf completion (no active descendants below it); a routing
	// parent's intermediate event is just noise in the shared
	// thread. The leaf's NotifyTaskCompleted will hit this path
	// with the real artifacts to consolidate. forumDelivered is
	// consumed by the per-watcher artifact fanout below to skip
	// duplicate uploads when the forum thread already shipped them.
	forumDelivered := false
	if leafID == "" && b.forumEnabled() {
		forumDelivered = b.notifyForumThread(ctx, task, success, message)
	}

	if b.watcherRepo == nil {
		b.logger.Warn().Str("task_id", task.ID).Msg("notify: no watcher repo configured")
		return
	}

	chatIDs, err := b.watcherRepo.GetWatchers(ctx, task.ID)
	if err != nil {
		b.logger.Error().Err(err).Str("task_id", task.ID).Msg("notify: failed to get watchers")
		return
	}

	if leafID != "" {
		// Transfer watcher rows from the routing parent to the
		// leaf. Best-effort: a per-chat error is logged but doesn't
		// block the rest of the transfer (one missed watcher is
		// less bad than zero).
		for _, cid := range chatIDs {
			if werr := b.watcherRepo.Watch(ctx, leafID, cid); werr != nil {
				b.logger.Warn().Err(werr).
					Str("from_task", task.ID).
					Str("to_task", leafID).
					Int64("chat_id", cid).
					Msg("notify: failed to transfer watcher to leaf")
			}
		}
		if rerr := b.watcherRepo.RemoveWatchers(ctx, task.ID); rerr != nil {
			b.logger.Warn().Err(rerr).
				Str("task_id", task.ID).
				Msg("notify: failed to remove parent watchers after transfer")
		}
		b.logger.Info().
			Str("task_id", task.ID).
			Str("leaf_task_id", leafID).
			Int("watchers", len(chatIDs)).
			Msg("notify: transferred watchers to leaf and suppressed parent intermediate notification")
		return
	}

	if len(chatIDs) == 0 {
		b.logger.Debug().Str("task_id", task.ID).Msg("notify: no watchers registered")
		return
	}

	emoji := "✅"
	status := "completed"
	if !success {
		emoji = "❌"
		status = "failed"
	}

	// Per-chat verbosity preference governs what reaches the user
	// for THIS chat. Three modes:
	//   - silent: no chat notification at all. The dispatcher's
	//     wait_for_task tool still receives terminal status via
	//     the task repo (DB-side), so an in-flight conversation
	//     waiting on data still resumes — only the push to chat
	//     is suppressed.
	//   - short: one-line "✅ Task <id> completed" / "❌ Task <id>
	//     failed: <reason>". No artifact uploads, no humanized
	//     output prose.
	//   - full: legacy rich notification with humanizeTaskMessage,
	//     artifact uploads to watchers, the works.
	// Watchers are removed regardless of mode so the watcher row
	// doesn't leak across modes; "silent" is "process internally,
	// don't ping the user", not "ignore the task".
	humanized := message
	if humanized != "" {
		humanized = humanizeTaskMessage(humanized)
	}

	for _, chatID := range chatIDs {
		mode := b.getVerbosity(chatID)
		switch mode {
		case "silent":
			b.logger.Debug().Str("task_id", task.ID).Int64("chat_id", chatID).
				Msg("notify: silent mode — skipping push, dispatcher wait_for_task still receives terminal status via DB")
			continue
		case "short":
			line := fmt.Sprintf("%s Task %s %s", emoji, idfmt.Short(task.ID), strings.ToUpper(status))
			if !success && humanized != "" {
				short := humanized
				if len(short) > 200 {
					short = short[:200] + "…"
				}
				line += ": " + short
			}
			// Phase 28: send via sendMessageGetID so we capture the
			// message_id and remember the chat→task mapping. Lets
			// the operator reply to this notification and have the
			// reply route to task_messages for this task.
			if mid, err := b.sendMessageGetID(ctx, chatID, line); err != nil {
				b.logger.Error().Err(err).Str("task_id", task.ID).Int64("chat_id", chatID).Msg("notify: short send failed")
			} else if b.notifTracker != nil {
				b.notifTracker.remember(chatID, mid, task.ID, task.ProjectID)
			}
		default: // "full" or unset
			text := fmt.Sprintf("%s Task %s (%s)\n\nProject: %s\nStatus: %s",
				emoji, status, task.ID, task.ProjectID, strings.ToUpper(status))
			body := humanized
			if body != "" {
				if success {
					if len(body) > 500 {
						body = body[:500] + "…"
					}
					text += "\n\n" + body
				} else {
					if len(body) > 300 {
						body = body[:300] + "…"
					}
					text += "\nError: " + body
				}
			}
			// 2026-05-17 — surface deliverable filenames inline with
			// the completion notification. Without this an operator
			// reading on a phone (no shell access) sees the writer's
			// 2-line summary but no way to read the actual file. The
			// per-watcher artifact uploads further down still fire
			// for chats on "full" verbosity; the link block makes
			// the SAME files discoverable via the artifact UI when
			// the operator prefers a browser over Telegram's
			// document viewer.
			if success {
				text += b.renderDeliverableLinks(ctx, task)
			}
			// Phase 28: track the notification id so replies to it
			// route to this task's conversation thread.
			if mid, err := b.sendMessageGetID(ctx, chatID, text); err != nil {
				b.logger.Error().Err(err).Str("task_id", task.ID).Int64("chat_id", chatID).Msg("notify: full send failed")
			} else if b.notifTracker != nil {
				b.notifTracker.remember(chatID, mid, task.ID, task.ProjectID)
			}
			// Artifacts only fire on full mode — short / silent
			// users don't want the file-upload spam either. And
			// when the forum thread already received the artifact
			// drop (T3+T4: consolidated delivery), skip the DM
			// duplicate so the operator gets one canonical copy
			// in the thread.
			if success && b.artifactRepo != nil && !forumDelivered {
				b.sendArtifactsToWatchers(ctx, task.ID, []int64{chatID})
			}
		}
	}

	b.logger.Info().Str("task_id", task.ID).Int("watchers", len(chatIDs)).Bool("success", success).Msg("notify: task completion notifications dispatched")

	if err := b.watcherRepo.RemoveWatchers(ctx, task.ID); err != nil {
		b.logger.Error().Err(err).Str("task_id", task.ID).Msg("notify: failed to remove watchers")
	}

	// triggerFollowup runs from the deferred call at the top of the
	// function. Verbosity governs the noise floor of unsolicited
	// pings; the dispatcher's await_completion contract survives
	// that filter and the defer ensures it fires on every exit
	// path including the no-watchers / leaf-transfer cases.
}

// NotifyBudgetBreach posts a soft- or hard-cap alert for the given
// project to every operator with project access in telegram.allowed_users
// (wildcard or explicit match). Implements budget.Notifier.
//
// Dedup: one alert per (projectID, period, level, period-key) for the
// bot process lifetime. period-key is the local-TZ day or month so
// "daily soft breach at 2026-04-20" fires once on 2026-04-20 and
// doesn't re-fire on the same day. A process restart clears the
// cache and re-alerts — intentional, since the operator wants to
// see state after a restart.
func (b *Bot) NotifyBudgetBreach(ctx context.Context, projectID, level, period string, d budget.Decision) {
	if b == nil || projectID == "" || level == "" || period == "" {
		return
	}
	now := time.Now()
	// Look up the project so we can respect its configured timezone
	// for period-key computation. Fall back to UTC if unavailable.
	loc := time.UTC
	if b.registry != nil {
		if proj := b.registry.GetProject(projectID); proj != nil && proj.Budget.Timezone != "" {
			if z, err := time.LoadLocation(proj.Budget.Timezone); err == nil {
				loc = z
			}
		}
	}
	nowLocal := now.In(loc)
	var periodKey string
	switch period {
	case "daily":
		periodKey = nowLocal.Format("2006-01-02")
	case "monthly":
		periodKey = nowLocal.Format("2006-01")
	default:
		periodKey = "unknown"
	}
	dedupKey := projectID + "|" + period + "|" + level + "|" + periodKey

	b.budgetAlertMu.Lock()
	if b.budgetAlertsSent == nil {
		b.budgetAlertsSent = make(map[string]struct{})
	}
	if _, seen := b.budgetAlertsSent[dedupKey]; seen {
		b.budgetAlertMu.Unlock()
		return
	}
	b.budgetAlertsSent[dedupKey] = struct{}{}
	b.budgetAlertMu.Unlock()

	// Pick the dollar figure appropriate to the period.
	spent := d.DailyUSD
	if period == "monthly" {
		spent = d.MonthlyUSD
	}

	emoji := "⚠️"
	headline := "soft cap breached"
	tail := "autonomy continues; tasks still accepted."
	if level == "hard" {
		emoji = "🚫"
		headline = "hard cap hit — new tasks blocked"
		tail = "bump the cap in the project YAML or wait for the period to roll over."
	}

	text := fmt.Sprintf("%s %s (%s)\n\nProject: %s\nSpent this %s: $%.2f\n%s\n\n%s",
		emoji, headline, period, projectID, period, spent, d.Reason, tail)

	recipients := b.budgetAlertRecipients(projectID)
	if len(recipients) == 0 {
		b.logger.Debug().Str("project", projectID).Str("level", level).Str("period", period).
			Msg("budget alert: no eligible telegram recipients — skipping send")
		return
	}
	b.logger.Info().Str("project", projectID).Str("level", level).Str("period", period).
		Int("recipients", len(recipients)).Msg("budget alert: sending telegram notification")

	// Detached context — caller's context may be the already-cancelled
	// task / HTTP context. Generous timeout because the alert path is
	// best-effort, not on the critical task path.
	sendCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = ctx // intentionally unused; see comment above
	for _, chatID := range recipients {
		if err := b.sendMessage(sendCtx, chatID, text); err != nil {
			b.logger.Error().Err(err).Int64("chat_id", chatID).
				Str("project", projectID).Msg("budget alert: send failed")
		}
	}
}

// NotifyFill enqueues a trading_fills row into the per-order
// debouncer. Multiple fills on the same order arriving within
// fillDebounceWindow collapse into one Telegram message keyed by
// the project's NotifyFillsChatID. Best-effort and async: the
// caller's context is intentionally not threaded into the timer
// callback so the handler can return 204 immediately.
//
// No-op when the project has no NotifyFillsChatID configured or
// when the bot has no registry wired (the tests path).
func (b *Bot) NotifyFill(ctx context.Context, fill *persistence.TradingFill) {
	if b == nil || fill == nil || fill.ProjectID == "" || fill.OrderID == "" {
		return
	}
	if b.registry == nil {
		return
	}
	project := b.registry.GetProject(fill.ProjectID)
	if project == nil || project.Trading.NotifyFillsChatID == 0 {
		return
	}
	chatID := project.Trading.NotifyFillsChatID
	key := fill.ProjectID + "|" + fill.OrderID

	b.fillNotifyMu.Lock()
	if b.fillNotifyBuf == nil {
		b.fillNotifyBuf = map[string][]*persistence.TradingFill{}
	}
	if b.fillNotifyTimers == nil {
		b.fillNotifyTimers = map[string]*time.Timer{}
	}
	b.fillNotifyBuf[key] = append(b.fillNotifyBuf[key], fill)
	if t, ok := b.fillNotifyTimers[key]; ok {
		t.Stop()
	}
	b.fillNotifyTimers[key] = time.AfterFunc(fillDebounceWindow, func() {
		b.flushFillNotification(key, chatID)
	})
	b.fillNotifyMu.Unlock()
}

// flushFillNotification drains the buffered fills for `key`,
// formats one operator-friendly message, and sends to chatID.
// Called from the timer goroutine started by NotifyFill.
func (b *Bot) flushFillNotification(key string, chatID int64) {
	b.fillNotifyMu.Lock()
	fills := b.fillNotifyBuf[key]
	delete(b.fillNotifyBuf, key)
	delete(b.fillNotifyTimers, key)
	b.fillNotifyMu.Unlock()
	if len(fills) == 0 {
		return
	}

	// Aggregate: total qty + weighted average price + symbol +
	// project come from the buffered fills. All fills in the
	// buffer share (project_id, order_id) by construction so
	// symbol and project are identical across rows.
	var totalQty, weightedPrice float64
	for _, f := range fills {
		totalQty += f.Qty
		weightedPrice += f.Qty * f.Price
	}
	avgPrice := 0.0
	if totalQty > 0 {
		avgPrice = weightedPrice / totalQty
	}
	first := fills[0]
	var text string
	if len(fills) == 1 {
		text = fmt.Sprintf("✅ fill — %s\n\nProject: %s\nOrder: %s\nQty: %g @ $%.4f\nNotional: $%.2f",
			first.Symbol, first.ProjectID, first.OrderID,
			first.Qty, first.Price, first.Qty*first.Price,
		)
	} else {
		text = fmt.Sprintf("✅ %d fills aggregated — %s\n\nProject: %s\nOrder: %s\nTotal qty: %g @ avg $%.4f\nTotal notional: $%.2f",
			len(fills), first.Symbol, first.ProjectID, first.OrderID,
			totalQty, avgPrice, weightedPrice,
		)
	}
	sendCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.sendMessage(sendCtx, chatID, text); err != nil {
		b.logger.Error().Err(err).
			Int64("chat_id", chatID).
			Str("project_id", first.ProjectID).
			Str("order_id", first.OrderID).
			Int("fills", len(fills)).
			Msg("fill notification: send failed")
		return
	}
	b.logger.Info().
		Int64("chat_id", chatID).
		Str("project_id", first.ProjectID).
		Str("order_id", first.OrderID).
		Str("symbol", first.Symbol).
		Float64("total_qty", totalQty).
		Int("fill_count", len(fills)).
		Msg("fill notification sent")
}

// NotifyEffectiveCostDrift posts a $/success drift alert. Implements
// budget.EffectiveCostNotifier. Distinct from NotifyBudgetBreach
// because the signal is different — total spend may be healthy when
// this fires; what's degrading is quality-of-spend.
//
// Dedup is handled by the monitor's own cooldown table (12h
// default), not here — the bot trusts the caller. Recipients are
// every operator with Telegram access (wildcard or any project),
// since model-level alerts aren't project-scoped.
func (b *Bot) NotifyEffectiveCostDrift(ctx context.Context, alert budget.EffectiveCostAlert) {
	if b == nil || alert.Role == "" || alert.Model == "" {
		return
	}
	text := fmt.Sprintf(
		"📈 effective-cost drift\n\nRole: %s\nModel: %s\n\n24h: $%.4f / success (over %d successes, $%.2f spend)\n7d baseline: $%.4f / success\nRatio: %.2fx\n\nThe model is producing usable output at %.1f× its usual cost. Worth investigating: model regressed, prompt drifted, or task mix shifted toward harder problems. Adjust the role's model, lower the iteration budget, or accept the new normal.",
		alert.Role, alert.Model,
		alert.Current24hUSDPerSuccess, alert.Successes24h, alert.Spend24hUSD,
		alert.Baseline7dUSDPerSuccess,
		alert.Ratio, alert.Ratio,
	)

	// Recipients: every operator with bot access. Effective-cost
	// alerts are about model behavior, not project-scoped — every
	// allowed user sees them, regardless of project list.
	var recipients []int64
	for chatID, ua := range b.config.AllowedUsers {
		if ua.Allowed {
			recipients = append(recipients, chatID)
		}
	}
	if len(recipients) == 0 {
		b.logger.Debug().Str("role", alert.Role).Str("model", alert.Model).
			Msg("effective-cost alert: no eligible telegram recipients — skipping send")
		return
	}
	b.logger.Info().Str("role", alert.Role).Str("model", alert.Model).
		Float64("ratio", alert.Ratio).
		Int("recipients", len(recipients)).
		Msg("effective-cost alert: sending telegram notification")

	sendCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = ctx
	for _, chatID := range recipients {
		if err := b.sendMessage(sendCtx, chatID, text); err != nil {
			b.logger.Error().Err(err).Int64("chat_id", chatID).
				Str("role", alert.Role).Str("model", alert.Model).
				Msg("effective-cost alert: send failed")
		}
	}
}

// budgetAlertRecipients returns the chat IDs of operators allowed to
// see this project. Honors UserAccess.Wildcard ("*" → any project)
// and explicit project lists. Anyone with Allowed=false is skipped.
func (b *Bot) budgetAlertRecipients(projectID string) []int64 {
	var out []int64
	for chatID, ua := range b.config.AllowedUsers {
		if !ua.Allowed {
			continue
		}
		if ua.Wildcard() {
			out = append(out, chatID)
			continue
		}
		for _, p := range ua.Projects {
			if p == projectID {
				out = append(out, chatID)
				break
			}
		}
	}
	return out
}

// renderDeliverableLinks builds the "Produced files:" download
// block appended to the task-complete notification. Reads OUTPUT
// artifacts from the artifact repo (same filter as
// sendArtifactsToWatchers minus the response.md suffix
// exclusion — the artifact link is informational and the
// operator may prefer the raw response when debugging) and
// resolves each to a project-scoped /ui/projects/<id>/artifacts/raw
// URL when WebUIBaseURL is configured.
//
// When the daemon has no WebUIBaseURL OR no artifact repo, the
// block degrades to a name-only "produced files: deliverable.md,
// summary.txt" notice with a "operator must have shell access"
// trailer so operators don't see broken half-links.
func (b *Bot) renderDeliverableLinks(ctx context.Context, task *persistence.Task) string {
	if b == nil || task == nil || b.artifactRepo == nil {
		return ""
	}
	filter := persistence.ArtifactFilter{TaskID: &task.ID, PageSize: 25}
	arts, err := b.artifactRepo.List(ctx, filter)
	if err != nil || len(arts) == 0 {
		return ""
	}
	var names []string
	for _, a := range arts {
		if a == nil {
			continue
		}
		if a.ArtifactClass != persistence.ArtifactClassOutput {
			continue
		}
		// Skip the raw-response dumps the same way
		// sendArtifactsToWatchers does — they're per-step debug
		// transcripts, not the operator deliverable.
		if strings.HasSuffix(a.Name, "-response.md") {
			continue
		}
		names = append(names, a.Name)
	}
	if len(names) == 0 {
		return ""
	}
	links := conversation.BuildDeliverableLinks(b.config.WebUIBaseURL, task.ProjectID, names)
	return conversation.RenderDeliverableLinks(links)
}

// sendArtifactsToWatchers sends OUTPUT artifacts for a completed task to all watching chats.
func (b *Bot) sendArtifactsToWatchers(ctx context.Context, taskID string, chatIDs []int64) {
	filter := persistence.ArtifactFilter{TaskID: &taskID, PageSize: 20}
	artifacts, err := b.artifactRepo.List(ctx, filter)
	if err != nil || len(artifacts) == 0 {
		return
	}

	// Send only OUTPUT artifacts, excluding raw agent response dumps (*-response.md).
	sent := 0
	for _, a := range artifacts {
		if a.ArtifactClass != persistence.ArtifactClassOutput {
			continue
		}
		if strings.HasSuffix(a.Name, "-response.md") {
			continue
		}
		for _, chatID := range chatIDs {
			if err := b.SendDocument(ctx, chatID, a.StoragePath, a.Name); err != nil {
				b.logger.Warn().Err(err).Str("artifact", a.Name).Int64("chat_id", chatID).Msg("notify: failed to send artifact")
			}
		}
		sent++
	}
	if sent > 0 {
		b.logger.Info().Str("task_id", taskID).Int("artifacts", sent).Msg("notify: sent output artifacts to watchers")
	}
}

// NewBot creates a new Telegram bot backed by the dispatcher agent.
//
// chatClient is the LLM client used by the dispatcher. All tool dependencies
// (task/execution/artifact repos, registry) are injected via BotOption functions
// and wired into the dispatcher before the bot is returned.
func NewBot(config BotConfig, chatClient chat.Provider, opts ...BotOption) (*Bot, error) {
	if config.Token == "" {
		return nil, ErrEmptyToken
	}

	b := &Bot{
		config:           config,
		llmClient:        chatClient,
		conversations:    make(map[int64]*chat.Conversation),
		activeProjects:   make(map[int64]string),
		receiverLocks:    make(map[int64]*sync.Mutex),
		verbosity:        make(map[int64]string),
		rateLimits:       make(map[int64]*rateLimitEntry),
		pendingFollowups: make(map[string]followupContext),
		chatUsers:        make(map[int64]int64),
		baseURL:          fmt.Sprintf("https://api.telegram.org/bot%s", config.Token),
		logger:           zerolog.Nop(),
		stopChan:         make(chan struct{}),
		notifTracker:     newTaskNotifTracker(),
		forumCreateInFly: make(map[string]chan struct{}),
	}

	for _, opt := range opts {
		opt(b)
	}

	if b.httpClient == nil {
		b.httpClient = &http.Client{
			Timeout: telegramHTTPTimeout,
		}
	}

	// The dispatcher is constructed by service.Container.initDispatcher
	// (see internal/service/container_dispatcher.go) and wired into
	// this bot as a ConversationChannel receiver via SetReceiver —
	// the same dispatcher can serve multiple channels (Telegram,
	// GitHub, future inbound integrations). Direct telegram.NewBot
	// callers (test fixtures) that need dispatcher-bound chat must
	// SetReceiver themselves.
	return b, nil
}

// Start begins polling for updates.
func (b *Bot) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return errors.New("bot is already running")
	}
	b.running = true
	b.mu.Unlock()

	// Restore conversations from disk if persistence is configured
	if b.config.SessionPath != "" {
		loaded, err := chat.LoadConversations(b.config.SessionPath, b.config.MaxHistory)
		if err != nil {
			b.logger.Warn().Err(err).Str("path", b.config.SessionPath).Msg("failed to load sessions (starting fresh)")
		} else if len(loaded) > 0 {
			b.mu.Lock()
			for chatID, conv := range loaded {
				b.tuneConversation(conv)
				b.conversations[chatID] = conv
			}
			b.mu.Unlock()
			b.logger.Info().Int("count", len(loaded)).Msg("restored conversations from disk")
		}
	}

	// Register bot commands in the Telegram menu.
	b.registerBotMenu(ctx)

	b.wg.Add(1)
	go b.pollLoop(ctx)

	// Periodic session save every 5 minutes during long conversations.
	if b.config.SessionPath != "" {
		b.wg.Add(1)
		go b.periodicSaveLoop(ctx)
	}

	b.logger.Info().
		Int("allowed_users", len(b.config.AllowedUsers)).
		Int("rate_limit", b.config.RateLimit).
		Int("max_history", b.config.MaxHistory).
		Msg("telegram polling started")

	return nil
}

// Stop stops the bot.
func (b *Bot) Stop() error {
	b.mu.Lock()
	if !b.running {
		b.mu.Unlock()
		return nil
	}
	b.running = false
	b.mu.Unlock()

	close(b.stopChan)
	b.wg.Wait()

	// Persist conversations to disk
	b.saveConversations()

	return nil
}

// saveConversations writes current conversations to disk if persistence is configured.
func (b *Bot) saveConversations() {
	if b.config.SessionPath == "" {
		return
	}
	b.mu.RLock()
	convs := make(map[int64]*chat.Conversation, len(b.conversations))
	for k, v := range b.conversations {
		convs[k] = v
	}
	b.mu.RUnlock()

	if err := chat.SaveConversations(b.config.SessionPath, convs); err != nil {
		b.logger.Error().Err(err).Str("path", b.config.SessionPath).Msg("failed to save sessions")
	} else {
		b.logger.Info().Int("count", len(convs)).Str("path", b.config.SessionPath).Msg("conversations saved to disk")
	}
}

// periodicSaveLoop saves conversations to disk every 5 minutes.
func (b *Bot) periodicSaveLoop(ctx context.Context) {
	defer b.wg.Done()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-b.stopChan:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.saveConversations()
		}
	}
}

// pollLoop continuously polls for updates.
//
// Cluster-aware: when WithLeaderGate is set, the loop only
// calls getUpdates while the gate reports IsLeader=true.
// Non-leader replicas idle (sleep + recheck on a short
// interval) so two daemons can't both consume the same
// Telegram update and double-reply the user.
//
// Restart-aware: when WithPollerStateRepository is set, the
// initial offset is loaded from Postgres (the
// telegram_poller_state table) and persisted after each
// batch. Without persistence, a restart resets the offset to
// 0 and Telegram replays whatever is still in the bot's
// update queue.
func (b *Bot) pollLoop(ctx context.Context) {
	defer b.wg.Done()

	offset := b.loadInitialOffset(ctx)
	wasLeader := false
	for {
		select {
		case <-b.stopChan:
			return
		case <-ctx.Done():
			return
		default:
		}

		if b.leaderGate != nil && !b.leaderGate.IsLeader() {
			// Non-leader: idle. Log once on the transition so
			// operators can grep "non-leader" / "became leader"
			// to follow failover. 2s sleep is short enough to
			// take over quickly when a deposed leader's lease
			// expires, but long enough that idle replicas don't
			// burn CPU.
			if wasLeader {
				b.logger.Info().Msg("telegram poller: lost leader lease, idling")
				wasLeader = false
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if b.leaderGate != nil && !wasLeader {
			// Just acquired the lease. Re-read the persisted
			// offset so a takeover doesn't start from the
			// in-memory zero and replay queued updates.
			fresh := b.loadInitialOffset(ctx)
			if fresh > offset {
				offset = fresh
			}
			b.logger.Info().Int64("offset", offset).Msg("telegram poller: acquired leader lease")
			wasLeader = true
		}

		// Leader epoch fence (review B1). The IsLeader() idle gate
		// above can report a STALE true: a TTL-expired-but-paused
		// leader resuming after a GC/scheduler stall still carries a
		// cached leader bit. Re-read the lock epoch here, immediately
		// before getUpdates — the dangerous consume — so a successor's
		// epoch bump fences this stale leader out instead of letting
		// it double-consume updates. nil / IsLeader-only gates are
		// pre-fence and proceed unchanged.
		if proceed, reason := leaderelection.DangerousWriteAllowed(ctx, b.leaderGate); !proceed {
			b.logger.Warn().Str("reason", reason).Msg("telegram poller: leader epoch fence — skipping getUpdates")
			leaderelection.LeaderFenceRejected("telegram_poller")
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		updates, err := b.getUpdates(ctx, offset)
		if err != nil {
			b.logger.Warn().Str("error", sanitizeTelegramError(err)).Msg("telegram getUpdates failed")
			time.Sleep(5 * time.Second)
			continue
		}

		for _, upd := range updates {
			offset = upd.UpdateID + 1
			if err := b.HandleUpdate(ctx, &upd); err != nil {
				b.logger.Warn().
					Err(err).
					Int64("update_id", upd.UpdateID).
					Msg("telegram update handling failed")
				continue
			}
		}
		// Persist after the batch so the next poll (or a
		// successor leader) resumes from the new watermark.
		// Best-effort: a transient DB write failure logs and
		// the in-memory offset continues to advance.
		if len(updates) > 0 {
			b.persistOffset(ctx, offset)
		}

		// Small delay between polls
		time.Sleep(1 * time.Second)
	}
}

// loadInitialOffset returns the offset to start polling from.
// Reads the persisted watermark when WithPollerStateRepository
// is wired; falls back to 0 (legacy behaviour) otherwise.
func (b *Bot) loadInitialOffset(ctx context.Context) int64 {
	if b.pollerStateRepo == nil || b.pollerBotID == "" {
		return 0
	}
	state, err := b.pollerStateRepo.Get(ctx, b.pollerBotID)
	if err != nil {
		// ErrNotFound is the common path on first boot —
		// only warn on other errors (DB down, schema drift).
		if !errors.Is(err, persistence.ErrNotFound) {
			b.logger.Warn().Err(err).Msg("telegram poller: load offset failed; starting at 0")
		}
		return 0
	}
	return state.Offset
}

// persistOffset writes the watermark through to the
// telegram_poller_state table. Best-effort: write failures
// are logged but don't block the polling loop — the in-memory
// offset is the authoritative cursor while the daemon runs.
func (b *Bot) persistOffset(ctx context.Context, offset int64) {
	if b.pollerStateRepo == nil || b.pollerBotID == "" {
		return
	}
	err := b.pollerStateRepo.Set(ctx, &persistence.TelegramPollerState{
		BotID:  b.pollerBotID,
		Offset: offset,
	})
	if err != nil {
		b.logger.Warn().Err(err).Int64("offset", offset).Msg("telegram poller: persist offset failed")
	}
}

// getUpdates fetches new updates from Telegram API.
func (b *Bot) getUpdates(ctx context.Context, offset int64) ([]Update, error) {
	url := fmt.Sprintf("%s/getUpdates?timeout=30&offset=%d", b.baseURL, offset)
	start := time.Now()
	b.logger.Debug().
		Int64("offset", offset).
		Dur("poll_timeout", telegramLongPollTimeout).
		Msg("telegram getUpdates started")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		b.logger.Warn().
			Str("error", sanitizeTelegramError(err)).
			Int64("offset", offset).
			Dur("duration", time.Since(start)).
			Msg("telegram getUpdates request failed")
		return nil, fmt.Errorf("failed to get updates: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.logger.Warn().
			Err(err).
			Int64("offset", offset).
			Int("status_code", resp.StatusCode).
			Dur("duration", time.Since(start)).
			Msg("telegram getUpdates response read failed")
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	b.logger.Debug().
		Int64("offset", offset).
		Int("status_code", resp.StatusCode).
		Int("response_bytes", len(body)).
		Dur("duration", time.Since(start)).
		Msg("telegram getUpdates response received")

	var result GetUpdatesResponse
	if err := json.Unmarshal(body, &result); err != nil {
		b.logger.Warn().
			Err(err).
			Str("response_body", truncateTelegramLogString(string(body), 512)).
			Msg("telegram getUpdates response parse failed")
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if !result.OK {
		b.logger.Warn().
			Str("response_body", truncateTelegramLogString(string(body), 512)).
			Msg("telegram getUpdates returned not ok")
		return nil, errors.New("getUpdates request failed")
	}

	b.logger.Debug().
		Int64("offset", offset).
		Int("update_count", len(result.Result)).
		Dur("duration", time.Since(start)).
		Msg("telegram getUpdates finished")

	return result.Result, nil
}

// sendChatAction sends a "typing" indicator to the chat.
func (b *Bot) sendChatAction(ctx context.Context, chatID int64, action string) {
	body, _ := json.Marshal(map[string]any{"chat_id": chatID, "action": action})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/sendChatAction", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

// startTypingLoop sends "typing" every 4 seconds until the returned cancel func is called.
func (b *Bot) startTypingLoop(ctx context.Context, chatID int64) context.CancelFunc {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		b.sendChatAction(ctx, chatID, "typing")
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.sendChatAction(ctx, chatID, "typing")
			}
		}
	}()
	return cancel
}

var _ = (*Bot).startTypingLoop

// getFile retrieves the file path from Telegram for a given file_id.
func (b *Bot) getFile(ctx context.Context, fileID string) (string, error) {
	url := fmt.Sprintf("%s/getFile?file_id=%s", b.baseURL, fileID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("getFile request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to parse getFile response: %w", err)
	}
	if !result.OK || result.Result.FilePath == "" {
		return "", fmt.Errorf("getFile failed for file_id %s", fileID)
	}
	return result.Result.FilePath, nil
}

// maxTelegramDownloadBytes caps incoming file downloads. Telegram bots are
// limited to 20 MB by the Bot API itself, but we enforce a local ceiling in
// case a malicious path is substituted or the limit is ever raised. It is a
// var (not const) so regression tests can lower the cap without streaming
// the full 100 MiB through a test HTTP server.
var maxTelegramDownloadBytes int64 = 100 * 1024 * 1024

// downloadFile downloads a file from Telegram to the specified local path.
func (b *Bot) downloadFile(ctx context.Context, telegramPath, destPath string) error {
	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.config.Token, telegramPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("file download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("file download returned status %d", resp.StatusCode)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer func() { _ = out.Close() }()

	// Cap the copy at the limit; detect oversize by peeking one more byte
	// from the raw body afterwards. io.LimitReader prevents writing past
	// the limit into the destination file in the first place — the old
	// CopyN(+1) approach wrote one extra byte on every download and only
	// rejected after the fact.
	if _, err := io.Copy(out, io.LimitReader(resp.Body, maxTelegramDownloadBytes)); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	var probe [1]byte
	if n, _ := resp.Body.Read(probe[:]); n > 0 {
		_ = out.Close()
		_ = os.Remove(destPath)
		return fmt.Errorf("telegram file exceeds %d byte limit", maxTelegramDownloadBytes)
	}
	return nil
}

// sendDocument sends a file to a Telegram chat.
//
// The multipart body streams through io.Pipe rather than buffering the
// whole file in memory. Agent artifacts (code archives, data dumps)
// can run to hundreds of MiB; with multiple watchers and concurrent
// task completions the prior bytes.Buffer approach was an OOM vector.
// The producer goroutine closes its end of the pipe on completion or
// error, and propagates any write/copy error via pw.CloseWithError so
// http.Client.Do's body read surfaces it on this side.
func (b *Bot) sendDocument(ctx context.Context, chatID int64, filePath, caption string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer func() { _ = file.Close() }()
	return b.sendDocumentFromReader(ctx, chatID, file, filepath.Base(filePath), caption)
}

// sendDocumentFromReader uploads an in-memory or backend-streamed
// artifact to Telegram. Factored out of sendDocument so the
// FileSender's two surfaces (path-based for legacy callers, reader-
// based for backend-aware artifact reads) share the multipart-pipe
// implementation.
//
// The caller owns body's lifecycle — sendDocumentFromReader streams
// from it but does not Close it. fileName lands in the multipart
// "filename" field; pick the artifact's recorded Name, not its
// storage key.
func (b *Bot) sendDocumentFromReader(ctx context.Context, chatID int64, body io.Reader, fileName, caption string) error {
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	go func() {
		defer func() { _ = pw.Close() }()
		defer func() { _ = writer.Close() }()

		if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
			_ = pw.CloseWithError(fmt.Errorf("multipart chat_id: %w", err))
			return
		}
		if caption != "" {
			if err := writer.WriteField("caption", caption); err != nil {
				_ = pw.CloseWithError(fmt.Errorf("multipart caption: %w", err))
				return
			}
		}
		part, err := writer.CreateFormFile("document", fileName)
		if err != nil {
			_ = pw.CloseWithError(fmt.Errorf("multipart create form file: %w", err))
			return
		}
		if _, err := io.Copy(part, body); err != nil {
			_ = pw.CloseWithError(fmt.Errorf("multipart copy file: %w", err))
			return
		}
	}()

	url := fmt.Sprintf("%s/sendDocument", b.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		_ = pr.Close()
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sendDocument failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return fmt.Errorf("sendDocument returned %d: %s", resp.StatusCode, string(errBody))
	}
	return nil
}

// SendDocument sends a file to a chat (implements FileSender interface).
func (b *Bot) SendDocument(ctx context.Context, chatID int64, path, caption string) error {
	return b.sendDocument(ctx, chatID, path, caption)
}

// SendDocumentReader sends an artifact to a chat from a streaming
// reader instead of a filesystem path. Used by the dispatcher's
// send_artifact tool when the artifact lives in an S3 backend and
// has no on-disk representation. fileName is the user-visible name
// the Telegram client shows (use the artifact's recorded Name, not
// its storage key).
func (b *Bot) SendDocumentReader(ctx context.Context, chatID int64, body io.Reader, fileName, caption string) error {
	return b.sendDocumentFromReader(ctx, chatID, body, fileName, caption)
}

// DownloadTelegramFile downloads a file by file_id to the given directory.
// Returns the local file path.
func (b *Bot) DownloadTelegramFile(ctx context.Context, fileID, fileName, destDir string) (string, error) {
	safeName, err := safepath.CleanFileName(fileName)
	if err != nil {
		return "", fmt.Errorf("invalid telegram file name: %w", err)
	}
	telegramPath, err := b.getFile(ctx, fileID)
	if err != nil {
		return "", err
	}
	_ = os.MkdirAll(destDir, 0o755)
	destPath, err := safepath.JoinUnder(destDir, safeName)
	if err != nil {
		return "", fmt.Errorf("invalid telegram destination path: %w", err)
	}
	if err := b.downloadFile(ctx, telegramPath, destPath); err != nil {
		return "", err
	}
	b.logger.Info().Str("file", safeName).Str("dest", destPath).Msg("downloaded telegram file")
	return destPath, nil
}

// HandleUpdate processes an incoming update.
func (b *Bot) HandleUpdate(ctx context.Context, upd *Update) error {
	// CallbackQuery dispatch — 2026.6.0 inline-keyboard surface.
	// One Update carries either a Message OR a CallbackQuery; the
	// callback branch returns BEFORE the message-handling logic
	// to avoid a "click registered as text" misfire.
	if upd.CallbackQuery != nil {
		return b.handleCallbackQuery(ctx, upd.CallbackQuery)
	}
	if upd.Message == nil {
		b.logger.Debug().Int64("update_id", upd.UpdateID).Msg("telegram update ignored without message")
		return nil
	}

	msg := &Message{
		ID:       upd.Message.ID,
		ChatID:   upd.Message.Chat.ID,
		UserID:   upd.Message.From.ID,
		Username: upd.Message.From.Username,
		Text:     upd.Message.Text,
	}
	if upd.Message.ReplyToMessage != nil {
		msg.ReplyToMessageID = upd.Message.ReplyToMessage.ID
	}
	if upd.Message.MessageThreadID != 0 {
		msg.MessageThreadID = upd.Message.MessageThreadID
	}

	// Voice / audio attachments take precedence over document /
	// photo because the channel routes them through STT (slice 3 of
	// the voice MVP). If STT isn't wired, the audio still surfaces
	// as a file attachment via the legacy path below.
	if fid, name, hint := detectVoiceAttachment(upd.Message.Voice, upd.Message.Audio); fid != "" {
		msg.FileID = fid
		msg.FileName = name
		msg.IsVoice = true
		msg.VoiceHint = voiceHint{MimeType: hint.MimeType, SampleRateHz: hint.SampleRateHz}
	} else if upd.Message.Document != nil {
		msg.FileID = upd.Message.Document.FileID
		msg.FileName = upd.Message.Document.FileName
	} else if len(upd.Message.Photo) > 0 {
		// Use the largest photo resolution
		best := upd.Message.Photo[len(upd.Message.Photo)-1]
		msg.FileID = best.FileID
		msg.FileName = "photo.jpg"
	}
	if msg.Text == "" && upd.Message.Caption != "" {
		msg.Text = upd.Message.Caption
	}
	if msg.Text == "" && msg.FileID != "" && !msg.IsVoice {
		msg.Text = "I've attached a file."
	}

	return b.HandleMessage(ctx, msg)
}

// ActiveChatCount returns the number of distinct chat sessions the
// bot is tracking — i.e. operators with an active conversation.
// Powers the landing page's "active chats" tile so operators see the
// shape of in-flight bot work alongside in-flight tasks. nil-safe so
// the dashboard handler can call it without first checking that the
// bot exists in deployments without telegram.
func (b *Bot) ActiveChatCount() int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.conversations)
}

// SetSecretsDetector wires the redaction backstop. Optional —
// without it the bot's outbound text is passed through verbatim
// (the pre-feature behaviour). Wired in container.go from the
// shared *secrets.MultiDetector that the executor / artifacts /
// memory layers also use, so all sinks share one corpus.
func (b *Bot) SetSecretsDetector(d secrets.Detector) {
	if b == nil {
		return
	}
	b.secretsDetector = d
}

// sendMessage sends a message to a chat.
func (b *Bot) sendMessage(ctx context.Context, chatID int64, text string) error {
	return b.sendMessageWithMarkup(ctx, chatID, text, nil)
}

// sendMessageWithMarkup is sendMessage's inline-keyboard sibling.
// Added 2026.6.0 for the SaaS-readiness Telegram surface. Pass
// markup=nil for a plain message — that path delegates to
// sendMessage's existing behaviour. When markup is non-nil it
// rides on the same secrets-redact / HTTP / logging pipeline so
// callers don't get a surprise difference in observability.
func (b *Bot) sendMessageWithMarkup(ctx context.Context, chatID int64, text string, markup *InlineKeyboardMarkup) error {
	if text == "" {
		return ErrEmptyMessage
	}

	// Redact-mode backstop: scan every outbound message and
	// replace any findings with [REDACTED:<type>] before the bytes
	// hit the wire. Catches anything that slipped past the
	// upstream checkpoints (result.json, container logs, audit,
	// memory) so a leaked secret can't exfiltrate via chat reply.
	// No-op when no detector is wired or when the message is
	// clean.
	if b.secretsDetector != nil {
		if findings := b.secretsDetector.Scan([]byte(text)); len(findings) > 0 {
			text = string(secrets.Redact([]byte(text), findings))
			b.logger.Warn().
				Int64("chat_id", chatID).
				Int("findings", len(findings)).
				Msg("telegram sendMessage: redacted secret(s) before transmit")
		}
	}

	reqBody := SendMessageRequest{
		ChatID:      chatID,
		Text:        text,
		ReplyMarkup: markup,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/sendMessage", b.baseURL)
	start := time.Now()
	b.logger.Debug().
		Int64("chat_id", chatID).
		Int("text_len", len(text)).
		Msg("telegram sendMessage started")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		b.logger.Warn().
			Str("error", sanitizeTelegramError(err)).
			Int64("chat_id", chatID).
			Dur("duration", time.Since(start)).
			Msg("telegram sendMessage request failed")
		return fmt.Errorf("failed to send message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		b.logger.Warn().
			Err(err).
			Int64("chat_id", chatID).
			Int("status_code", resp.StatusCode).
			Dur("duration", time.Since(start)).
			Msg("telegram sendMessage response read failed")
		return fmt.Errorf("failed to read response: %w", err)
	}

	b.logger.Debug().
		Int64("chat_id", chatID).
		Int("status_code", resp.StatusCode).
		Int("response_bytes", len(respBody)).
		Dur("duration", time.Since(start)).
		Msg("telegram sendMessage response received")

	var result SendMessageResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		b.logger.Warn().
			Err(err).
			Int64("chat_id", chatID).
			Str("response_body", truncateTelegramLogString(string(respBody), 512)).
			Msg("telegram sendMessage response parse failed")
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if !result.OK {
		b.logger.Warn().
			Int64("chat_id", chatID).
			Str("description", result.Description).
			Msg("telegram sendMessage returned not ok")
		return fmt.Errorf("sendMessage failed: %s", result.Description)
	}

	b.logger.Info().
		Int64("chat_id", chatID).
		Int64("message_id", result.Result.MessageID).
		Dur("duration", time.Since(start)).
		Msg("telegram message sent")

	return nil
}

// sendMessageGetID sends a message and returns the Telegram message ID.
func (b *Bot) sendMessageGetID(ctx context.Context, chatID int64, text string) (int64, error) {
	if text == "" {
		return 0, ErrEmptyMessage
	}
	body, _ := json.Marshal(SendMessageRequest{ChatID: chatID, Text: text})
	url := fmt.Sprintf("%s/sendMessage", b.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	var result SendMessageResponse
	if err := json.Unmarshal(respBody, &result); err != nil || !result.OK {
		return 0, fmt.Errorf("sendMessage failed: %s", result.Description)
	}
	return result.Result.MessageID, nil
}

// editMessageText edits a previously sent message.
func (b *Bot) editMessageText(ctx context.Context, chatID, messageID int64, text string) error {
	if text == "" {
		return nil
	}
	// Telegram rejects edits with identical text — truncate to 4096 chars.
	if len(text) > 4096 {
		text = text[:4093] + "..."
	}
	body, _ := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	})
	url := fmt.Sprintf("%s/editMessageText", b.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// getVerbosity returns the per-chat notification verbosity.
// Empty preference falls back to "short" — the focused, one-line
// completion notification. Operators who want the rich
// artifact-uploading "full" mode opt in via /verbose full.
//
// Historical note: the default was "full" before 2026-05-17.
// Customers complained that the full mode forwarded transient
// artifacts (handover.json, debug logs) alongside the actual
// answer — see also the OUTPUT-class filter in triggerFollowup.
func (b *Bot) getVerbosity(chatID int64) string {
	if b == nil {
		return "short"
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	v := b.verbosity[chatID]
	if v == "" {
		return "short"
	}
	return v
}

// setVerbosity records the chat's notification preference.
// Validation lives in the slash-command handler; the storage layer
// trusts the caller.
func (b *Bot) setVerbosity(chatID int64, mode string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.verbosity[chatID] = mode
}

// getConversation returns or creates a conversation for the given chat ID.
// If SessionTTL is configured and the conversation has been idle longer than
// the TTL, it is silently reset before being returned.
func (b *Bot) getConversation(chatID int64) *chat.Conversation {
	b.mu.Lock()
	defer b.mu.Unlock()

	if conv, ok := b.conversations[chatID]; ok {
		if b.config.SessionTTL > 0 && time.Since(conv.LastUsed()) > b.config.SessionTTL {
			b.logger.Info().Int64("chat_id", chatID).
				Dur("ttl", b.config.SessionTTL).
				Msg("session expired — starting fresh")
			delete(b.conversations, chatID)
		} else {
			return conv
		}
	}

	maxHistory := b.config.MaxHistory
	if maxHistory <= 0 {
		maxHistory = 20
	}

	conv := chat.NewConversation(fmt.Sprintf("telegram-%d", chatID), maxHistory)
	b.tuneConversation(conv)
	b.conversations[chatID] = conv
	return conv
}

// resetConversation clears the conversation for the given chat ID.
// Also drops the persisted row so a replica failover doesn't replay
// the just-cleared conversation. The DB delete fires after the
// in-memory clear and is best-effort (logged but doesn't block the
// caller — the in-memory state is the authoritative truth for the
// current process).
func (b *Bot) resetConversation(chatID int64) {
	b.mu.Lock()
	delete(b.conversations, chatID)
	persister := b.sessionPersister
	b.mu.Unlock()
	if persister != nil {
		_ = persister.Delete(context.Background(), strconv.FormatInt(chatID, 10))
	}
}

// hasInMemorySession reports whether the bot's cache already has a
// conversation for chatID. SessionStore.Load uses it to decide
// whether to hydrate from the DB on a cache miss — without this
// check we'd hit the DB on every inbound message instead of only
// the post-restart / post-failover first one.
func (b *Bot) hasInMemorySession(chatID int64) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.conversations[chatID]
	return ok
}

// hydrateSession populates the bot's in-memory cache from a
// persisted DB row. Called by SessionStore.Load when
// hasInMemorySession returned false and the persister surfaced a
// session. Subsequent getConversation / getActiveProject calls
// then see the rehydrated state transparently.
func (b *Bot) hydrateSession(chatID int64, messages []chat.Message, activeProject string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.conversations[chatID]; exists {
		// Concurrent inbound message already populated the cache —
		// don't clobber its (potentially newer) state.
		return
	}
	maxHistory := b.config.MaxHistory
	if maxHistory <= 0 {
		maxHistory = 20
	}
	conv := chat.NewConversation(fmt.Sprintf("telegram-%d", chatID), maxHistory)
	b.tuneConversation(conv)
	for _, m := range messages {
		conv.AddMessage(m)
	}
	b.conversations[chatID] = conv
	if activeProject != "" {
		b.activeProjects[chatID] = activeProject
	}
}

// sessionPersisterRef returns the wired persister (nil-safe). Read
// under the bot's RW mutex so a race with WithSessionPersister at
// construction can't return a torn pointer.
func (b *Bot) sessionPersisterRef() *sessionstore.Persister {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.sessionPersister
}

// getActiveProject returns the active project for a chat.
func (b *Bot) getActiveProject(chatID int64) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.activeProjects[chatID]
}

// setActiveProject sets the active project for a chat.
func (b *Bot) setActiveProject(chatID int64, projectID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.activeProjects[chatID] = projectID
}

// getProjectList returns every registered project, unfiltered. Callers
// that hand the list to a specific user's dispatcher turn should use
// getProjectListForUser instead so per-user scoping holds.
func (b *Bot) getProjectList() []*registry.Project {
	if b.registry == nil {
		return nil
	}
	return b.registry.ListProjects()
}

// getProjectListForUser returns the subset of projects the given user
// is permitted to see. Used when building Request.Projects for the
// dispatcher so tools like list_projects / switch_project can't hand
// the model a project the operator isn't cleared for.
func (b *Bot) getProjectListForUser(userID int64) []*registry.Project {
	all := b.getProjectList()
	if len(b.config.AllowedUsers) == 0 {
		// No allowlist configured → dev mode, everyone sees everything.
		return all
	}
	ua, ok := b.config.AllowedUsers[userID]
	if !ok || !ua.Allowed {
		return nil
	}
	if ua.Wildcard() {
		return all
	}
	filtered := make([]*registry.Project, 0, len(ua.Projects))
	for _, p := range all {
		if ua.CanAccessProject(p.ID) {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

// resolveLeadSystemPrompt builds the lead system prompt for the active
// project, scoped to what the given user may see so the "other projects"
// section in the prompt doesn't suggest forbidden ones.
func (b *Bot) resolveLeadSystemPrompt(userID int64, projectID string) string {
	if b.registry == nil || projectID == "" {
		return ""
	}
	leadPrompt, _ := dispatcher.ResolveLeadPrompt(b.registry, projectID)
	if leadPrompt == "" {
		return ""
	}
	project := b.registry.GetProject(projectID)
	if project == nil {
		return ""
	}
	swarm := b.registry.GetSwarm(project.SwarmID)
	return dispatcher.BuildLeadSystemPrompt(project, swarm, leadPrompt, b.getProjectListForUser(userID))
}

// handleReceiverTurn drives one inbound message through the wired
// conversation.Receiver. Runs on its own goroutine so the poll
// loop keeps draining updates while the LLM call is in flight.
// Multi-user bots get parallel dispatch across chats (dispatcher.Agent
// is safe for concurrent use), while turns for the same chat are
// serialised so post-turn history replacement cannot reorder or
// overwrite adjacent messages.
//
// Known follow-ups: metrics emission
// (MessageLatency / MessagesSent / ToolCallsTotal) and the typing
// indicator are not re-attached on this path yet; both land as
// separate slices once the receiver migration soaks.
//
// Errors are logged and swallowed; they don't propagate back to the
// poll loop. The receiver itself routes the reply through
// Channel.Send / StreamingSend and persists the post-turn state via
// SessionStore.Append.
func (b *Bot) handleReceiverTurn(r conversation.Receiver, msg *Message) {
	lock := b.receiverLock(msg.ChatID)
	lock.Lock()
	defer lock.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), b.effectiveDispatchTimeout())
	defer cancel()

	b.logger.Info().
		Int64("chat_id", msg.ChatID).
		Int64("user_id", msg.UserID).
		Int("text_len", len(msg.Text)).
		Bool("is_voice", msg.IsVoice).
		Msg("telegram receiver-path: dispatching to receiver")

	start := time.Now()
	cm := MessageToChannelMessage(msg)
	err := r.Receive(ctx, cm)
	dur := time.Since(start)
	if err != nil {
		b.logger.Warn().
			Err(err).
			Int64("chat_id", msg.ChatID).
			Int64("user_id", msg.UserID).
			Dur("duration", dur).
			Msg("telegram receiver-path: Receive returned error")
		return
	}
	b.logger.Info().
		Int64("chat_id", msg.ChatID).
		Dur("duration", dur).
		Msg("telegram receiver-path: Receive completed")
}

func (b *Bot) receiverLock(chatID int64) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.receiverLocks == nil {
		b.receiverLocks = make(map[int64]*sync.Mutex)
	}
	lock := b.receiverLocks[chatID]
	if lock == nil {
		lock = &sync.Mutex{}
		b.receiverLocks[chatID] = lock
	}
	return lock
}

func truncateTelegramLogString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "...(truncated)"
}

func sanitizeTelegramError(err error) string {
	if err == nil {
		return ""
	}

	msg := err.Error()
	for _, raw := range strings.Fields(msg) {
		trimmed := strings.Trim(raw, "\"")
		if parsed, parseErr := url.Parse(trimmed); parseErr == nil && parsed.Host == "api.telegram.org" {
			parsed.Path = sanitizeTelegramPath(parsed.Path)
			msg = strings.ReplaceAll(msg, trimmed, parsed.String())
		}
	}
	return msg
}

func sanitizeTelegramPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, "bot") {
			parts[i] = "bot<redacted>"
		}
	}
	return strings.Join(parts, "/")
}
