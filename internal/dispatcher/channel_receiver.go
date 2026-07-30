package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/mediakind"
	"vornik.io/vornik/internal/registry"
)

// ChannelReceiver translates an inbound conversation.ChannelMessage
// into a dispatcher invocation and routes the reply back through the
// originating Channel. Implements conversation.Receiver.
//
// Slice 3 of the ConversationChannel rollout: with this in place,
// future channel implementations (GitHub App, Slack, email) become
// pure translation layers — they call Receiver.Receive and never
// touch the dispatcher / persistence / scheduler directly.
//
// See https://docs.vornik.io
type ChannelReceiver struct {
	// Channel is the originating Channel — every reply this receiver
	// produces is sent back via Channel.Send or, when the Channel
	// also satisfies conversation.StreamingChannel, via
	// StreamingSend.
	Channel conversation.Channel

	// Agent runs the LLM tool-calling loop. *dispatcher.Agent
	// satisfies the Doer interface; tests supply stubs.
	Agent Doer

	// Sessions resolves per-channel conversation state (history,
	// active project, allowed-projects scope, lead system prompt) and
	// persists the post-turn updates. One implementation per channel
	// implementation; nil disables session persistence entirely
	// (each turn runs against an empty history — useful for tests).
	Sessions SessionStore

	// ResultPostprocessor optionally rewrites final Result.Text before
	// it reaches the user. Channels use it to attach channel-specific
	// affordances that don't belong in the dispatcher's reply itself —
	// Telegram's output-guard footer is the motivating case. Nil leaves
	// Result.Text untouched (the GitHub channel's default). Streaming
	// flows emit raw deltas while the model is running, then append any
	// missing postprocessed suffix before closing the stream.
	ResultPostprocessor func(Result) string

	// Disclosure enforces EU AI Act Art 50(1): the human must be told they
	// are interacting with an AI system, at the latest at the first
	// interaction of a session.
	//
	// This lives on the receiver rather than in each channel because every
	// channel funnels through Receive — one chokepoint means a channel
	// added later cannot forget to disclose. Nil disables the behaviour and
	// exists only so pre-existing construction sites keep compiling; a
	// production wiring that leaves it nil is non-conforming.
	//
	// Design: https://docs.vornik.io
	Disclosure *aidisclosure.Service

	// DisclosureMetrics observes serve/failure counts. Nil is fine.
	DisclosureMetrics DisclosureObserver

	// Media governs whether this receiver may attach media to the
	// dispatcher's own turn, and how much. Nil disables inline media
	// entirely — every media attachment then hands over to a specialist,
	// which is the pre-existing behaviour and the safe default.
	//
	// see LLD § https://docs.vornik.io §4.3
	Media *MediaSight
}

// DisclosureObserver receives Art 50 disclosure outcomes. Implemented by the
// metrics layer; nil-safe at every call site.
//
// A `stage="send"` failure is a conformity alarm, not merely a transport
// error — turns are being refused. A `stage="write"` failure is the quiet
// one: nothing user-visible breaks, and only the Art 99 evidence trail
// degrades, which is why it must be observable rather than logged and lost.
type DisclosureObserver interface {
	DisclosureServed(channel, cadence string)
	DisclosureFailed(channel, stage string)
}

// Doer is the narrow dispatcher contract the ChannelReceiver
// depends on. *dispatcher.Agent satisfies it; tests supply stubs to
// exercise the receiver without standing up a full Agent. The two
// methods mirror Agent.Process / Agent.ProcessStreaming verbatim so
// the receiver can swap between them based on the Channel's
// streaming capability.
type Doer interface {
	Process(ctx context.Context, req Request) Result
	ProcessStreaming(ctx context.Context, req Request, onText chat.StreamCallback) Result
}

// SessionStore is the per-channel state surface. Each channel
// implementation provides its own — Telegram already has one
// (Bot.conversations); GitHub App / Slack /
// email build minimal ones over their inbound handler's in-memory
// maps. The interface is kept narrow so a new channel needs only
// two methods to participate.
type SessionStore interface {
	// Load returns the current session state for the given inbound
	// message. Implementations create empty state for new sessions
	// and resolve the speaker against any per-channel allowlist
	// (returning conversation.ErrSpeakerUnknown when denied). The
	// receiver surfaces ErrSpeakerUnknown without invoking the
	// dispatcher.
	Load(ctx context.Context, msg conversation.ChannelMessage) (Session, error)

	// Append records the dispatcher's reply turn into the session's
	// persistent state. Implementations also update the active
	// project from result.NewProject when non-empty and trim history
	// per the session's max-history policy.
	Append(ctx context.Context, msg conversation.ChannelMessage, result Result) error
}

// Session is the channel-resolved snapshot the receiver hands to
// the dispatcher. Fields mirror the relevant subset of
// dispatcher.Request — the receiver fills in Messages with the
// user's new turn before dispatching.
type Session struct {
	// History is the conversation so far (without the inbound turn).
	// The receiver appends the new user message before invoking the
	// dispatcher.
	History []chat.Message

	// ActiveProject is the project_id currently /project'd into for
	// this session. Empty when no project has been selected.
	ActiveProject string

	// AvailableProjects is the registry snapshot used to populate
	// the dispatcher's system prompt. Nil acceptable; the dispatcher
	// renders a minimal prompt without project context.
	AvailableProjects []*registry.Project

	// AllowedProjects is the speaker's project-access scope (same
	// shape as Request.AllowedProjects). Nil = no restriction;
	// non-empty = exact-match whitelist with "*" wildcard.
	AllowedProjects []string

	// LeadSystemPrompt overrides the default dispatcher system
	// prompt with the project-specific lead persona when non-empty.
	LeadSystemPrompt string

	// ChatID is the legacy dispatcher.Request.ChatID — only used by
	// tools that send files back to the user. Implementations pass
	// the platform's numeric chat identifier when one exists
	// (Telegram chat_id); leave 0 for channels that lack one
	// (GitHub: comments are atomic, not file-bearing).
	ChatID int64

	// FileSender backs the dispatcher's send_artifact tool. Nil
	// disables the tool on this channel (GitHub App at least until
	// the design open question on attachments is resolved).
	FileSender FileSender

	// ContextTier signals the session's context-budget headroom —
	// PEAK / GOOD / DEGRADING / POOR — derived per channel from the
	// session's conversation token count against the channel's
	// configured budget. The dispatcher reads it on every turn:
	// DEGRADING-or-worse forces deferred tool loading regardless
	// of the catalog-size threshold. Zero-value (TierPeak) means
	// "no degradation signal" — defaults apply.
	ContextTier chat.ContextTier

	// ContextHeadroomPct is the remaining-budget percentage that
	// produced ContextTier — [0, 100]. Threaded to the dispatcher so
	// it can observe the histogram and the UI / API surface can
	// render the exact % alongside the discrete band. Zero ("no
	// signal") means the channel didn't compute a budget — same
	// effect as ContextTier=TierPeak on the metrics side.
	ContextHeadroomPct float64
}

// Receive implements conversation.Receiver. Runs the dispatcher with
// the channel's session state, routes the assistant reply back
// through Channel.Send (or StreamingSend when supported), and
// persists the updated session.
func (r *ChannelReceiver) Receive(ctx context.Context, msg conversation.ChannelMessage) error {
	if r.Agent == nil {
		return errors.New("dispatcher: ChannelReceiver.Agent is nil")
	}
	if r.Channel == nil {
		return errors.New("dispatcher: ChannelReceiver.Channel is nil")
	}

	// ChannelSpecific is untrusted upstream metadata that rides back to
	// the channel's Send; bound + strip it once here at ingest so every
	// channel gets the same treatment (security LLD review batch 3).
	msg.ChannelSpecific = conversation.SanitizeChannelSpecific(msg.ChannelSpecific)

	sess, err := r.loadSession(ctx, msg)
	if err != nil {
		return err
	}

	// EU AI Act Art 50(1)+(5). Served BEFORE the dispatcher runs, so the
	// human sees it before any AI-generated content rather than merely in
	// the same exchange — and so a dispatcher failure cannot leave a turn
	// where the AI spoke and the disclosure did not.
	if err := r.serveDisclosure(ctx, msg); err != nil {
		return err
	}

	// Media on this turn: classify each attachment, decide whether this
	// model can perceive it, and append a directive generated from the
	// branch actually taken. Pixels ride on THIS turn only — see the
	// stripInlineMedia call before Append below.
	// see LLD § https://docs.vornik.io §4.3
	outcomes := r.buildSightOutcomes(ctx, msg)
	r.observeOutcomes(outcomes)
	turnText := enrichUserContent(msg) + mediaNotes(outcomes)
	userTurn := chat.Message{Role: "user", Content: turnText}
	if blocks := mediaBlocks(turnText, outcomes); blocks != nil {
		userTurn.Blocks = blocks
	}
	// Copy rather than append in place: sess.History's backing array is
	// owned by the session store, and appending into spare capacity would
	// mutate what the store still holds.
	history := make([]chat.Message, 0, len(sess.History)+1)
	history = append(history, sess.History...)
	history = append(history, userTurn)
	// OperatorID is "<source>:<speaker>" when both are present —
	// the stable per-operator key the dispatcher's profile lookup
	// uses. Empty when SpeakerID is missing (synthetic / system
	// turns) so the dispatcher skips the lookup rather than
	// building a malformed key like "telegram:" that would
	// collide across users.
	operatorID := ""
	if msg.Source != "" && msg.SpeakerID != "" {
		operatorID = msg.Source + ":" + msg.SpeakerID
	}
	// Channels that know their own platform identity hand it over so the
	// agent can tell the lead which address in the thread is its own.
	// Type-assert rather than widening Channel — same idiom as
	// StreamingChannel in dispatch().
	channelIdentity := ""
	if sic, ok := r.Channel.(conversation.SelfIdentifyingChannel); ok {
		channelIdentity = sic.SelfIdentity()
	}
	req := Request{
		ChatID:               sess.ChatID,
		ChannelIdentity:      channelIdentity,
		Messages:             history,
		Project:              sess.ActiveProject,
		Projects:             sess.AvailableProjects,
		LeadSystemPrompt:     sess.LeadSystemPrompt,
		AllowedProjects:      sess.AllowedProjects,
		FileSender:           sess.FileSender,
		ContextTier:          sess.ContextTier,
		ContextHeadroomPct:   sess.ContextHeadroomPct,
		OriginatingChannel:   r.Channel.Name(),
		OriginatingSessionID: msg.SessionID,
		OperatorID:           operatorID,
	}

	result := r.dispatch(ctx, msg.SessionID, req)
	if result.Err != nil {
		return result.Err
	}

	if r.Sessions != nil {
		// Never persist pixels. The session store writes result.Messages
		// back as the new history, so an image left in place would be
		// replayed on every subsequent turn — a permanent per-image token
		// charge (the chat layer bills image blocks at a flat figure
		// regardless of size) and a cache-prefix invalidation each turn.
		// see LLD § https://docs.vornik.io §4.3(1)
		result.Messages = stripInlineMedia(result.Messages)
		if err := r.Sessions.Append(ctx, msg, result); err != nil {
			return fmt.Errorf("dispatcher: session append: %w", err)
		}
	}
	return nil
}

// loadSession resolves the per-channel session state. Nil
// SessionStore yields an empty Session (useful in tests). Speaker-
// rejection (ErrSpeakerUnknown) is propagated without invoking the
// dispatcher so unauthorised callers don't burn LLM budget.
func (r *ChannelReceiver) loadSession(ctx context.Context, msg conversation.ChannelMessage) (Session, error) {
	if r.Sessions == nil {
		return Session{}, nil
	}
	sess, err := r.Sessions.Load(ctx, msg)
	if err != nil {
		if errors.Is(err, conversation.ErrSpeakerUnknown) {
			return Session{}, err
		}
		return Session{}, fmt.Errorf("dispatcher: session load: %w", err)
	}
	return sess, nil
}

// dispatch runs the dispatcher and routes the reply back through
// the Channel. Branches on whether the Channel implements
// StreamingChannel; falls back to a one-shot Send on any streaming
// error so a flaky upstream never silently swallows the reply.
func (r *ChannelReceiver) dispatch(ctx context.Context, sessionID string, req Request) Result {
	sc, streaming := r.Channel.(conversation.StreamingChannel)
	if !streaming {
		result := r.Agent.Process(ctx, req)
		r.sendReply(ctx, sessionID, r.postprocess(result))
		return result
	}

	stream, err := sc.StreamingSend(ctx, sessionID)
	if err != nil {
		// StreamingSend itself failed — bypass streaming entirely.
		result := r.Agent.Process(ctx, req)
		r.sendReply(ctx, sessionID, r.postprocess(result))
		return result
	}

	var (
		streamErr    error
		streamedText string
	)
	onText := chat.StreamCallback(func(accumulated string) {
		if streamErr != nil {
			return
		}
		// chat.StreamCallback emits accumulated text; the Stream
		// interface accepts deltas. Slice by byte position: the
		// callback aggregates by string concatenation on the
		// dispatcher side, so byte-slicing matches what the dispatcher
		// considered "new". UTF-8 boundary anomalies are unlikely
		// because upstream flushes at token boundaries; if one
		// occurs the next delta self-corrects (the rune is
		// re-emitted whole in the next accumulated slice).
		if len(accumulated) <= len(streamedText) {
			return
		}
		delta := accumulated[len(streamedText):]
		if err := stream.Append(delta); err != nil {
			streamErr = err
			return
		}
		streamedText = accumulated
	})

	result := r.Agent.ProcessStreaming(ctx, req, onText)
	finalText := r.postprocess(result)
	if streamErr == nil && finalText != "" && finalText != streamedText {
		switch {
		case streamedText == "":
			if err := stream.Append(finalText); err != nil {
				streamErr = err
			} else {
				streamedText = finalText
			}
		case strings.HasPrefix(finalText, streamedText):
			if err := stream.Append(finalText[len(streamedText):]); err != nil {
				streamErr = err
			} else {
				streamedText = finalText
			}
		case result.Text != "" && strings.HasPrefix(finalText, result.Text) && strings.HasSuffix(streamedText, result.Text):
			// Tool-use streams include progress markers before the final
			// model reply. That means the transport-visible stream is not
			// byte-identical to Result.Text, but it already contains the
			// final answer. Append only the channel postprocessor suffix
			// (for example Telegram's guard footer) instead of treating
			// the expected status-prefix difference as a failed stream and
			// sending a duplicate fallback message.
			suffix := finalText[len(result.Text):]
			if suffix != "" {
				if err := stream.Append(suffix); err != nil {
					streamErr = err
				} else {
					streamedText += suffix
				}
			}
		case result.Text != "" && !strings.Contains(streamedText, result.Text):
			// A provider may stream only tool-status text and then return a
			// final answer without text deltas. Keep the status paragraph
			// and append the final reply rather than replacing it with a
			// one-shot fallback that would duplicate the already-edited
			// Telegram message.
			sep := ""
			if !strings.HasSuffix(streamedText, "\n") {
				sep = "\n\n"
			}
			if err := stream.Append(sep + finalText); err != nil {
				streamErr = err
			} else {
				streamedText += sep + finalText
			}
		}
	}

	_, closeErr := stream.Close()
	if (streamErr != nil || closeErr != nil) && streamedText == "" {
		// Stream finalisation failed — fall back to a one-shot Send
		// only when nothing was delivered. If Telegram already has a
		// partially edited message, sending a second full reply is the
		// duplicate-message failure mode this path is meant to avoid.
		r.sendReply(ctx, sessionID, finalText)
	}
	return result
}

// postprocess applies the optional ResultPostprocessor to the
// final reply text. When unset, returns result.Text verbatim so
// channels with no transformation needs (GitHub today) keep the
// existing behaviour.
func (r *ChannelReceiver) postprocess(result Result) string {
	text := result.Text
	if r.ResultPostprocessor != nil {
		text = r.ResultPostprocessor(result)
	}
	// Art 50(1) footer for per-message channels, applied LAST so a
	// configured postprocessor cannot strip the legally required notice.
	return r.disclosureFooter(text)
}

// serveDisclosure delivers the Art 50(1) notice for per-session channels.
//
// The failure model here is deliberately asymmetric (design §4):
//
//   - Send fails      → FAIL CLOSED. Abort the turn. Silence beats an
//     undisclosed AI conversation, and if Send is broken
//     the reply would have failed anyway.
//   - store read fails → serve anyway. A duplicate notice is a UX blemish;
//     a skipped one is non-conformity.
//   - store write fails → continue. The human has been disclosed to, so the
//     obligation is met; only the evidence degraded.
func (r *ChannelReceiver) serveDisclosure(ctx context.Context, msg conversation.ChannelMessage) error {
	if r.Disclosure == nil {
		return nil
	}
	channel := r.Channel.Name()
	if aidisclosure.CadenceFor(channel) != aidisclosure.CadencePerSession {
		return nil // per-message channels carry it as a footer instead
	}

	// The identity the obligation is owed to is not always the session. A channel may
	// declare a coarser one — Slack's sessions are per-thread, but Art 50(1) is owed to
	// a PERSON at their first interaction, not to each thread they open. The notice is
	// still delivered to msg.SessionID below, because that is where they are reading.
	sessionID := msg.SessionID
	scope := sessionID
	if scoper, ok := r.Channel.(conversation.DisclosureScoper); ok {
		if declared := strings.TrimSpace(scoper.DisclosureScope(msg)); declared != "" {
			scope = declared
		}
	}

	notice, serve, err := r.Disclosure.Ensure(ctx, channel, scope)
	if err != nil {
		// Fail toward disclosure: serve is true on a read error.
		r.observeDisclosureFailure(channel, "read")
	}
	if !serve {
		return nil
	}

	if _, sendErr := r.Channel.Send(ctx, conversation.ChannelMessage{
		Source:    channel,
		SessionID: sessionID,
		Text:      notice.Text,
	}); sendErr != nil {
		r.observeDisclosureFailure(channel, "send")
		return fmt.Errorf(
			"dispatcher: AI Act Art 50 disclosure could not be delivered on %s/%s; "+
				"refusing to continue the turn undisclosed: %w",
			channel, sessionID, sendErr)
	}
	r.observeDisclosureServed(channel, aidisclosure.CadencePerSession.String())

	if recErr := r.Disclosure.Record(ctx, channel, scope, notice); recErr != nil {
		// Already disclosed — the obligation is met. Only the Art 99
		// evidence trail degraded, so observe it and carry on.
		r.observeDisclosureFailure(channel, "write")
	}
	return nil
}

// disclosureFooter appends the Art 50(1) notice to an outbound on
// per-message channels — email replies and GitHub comments are standalone
// artifacts that get forwarded, quoted, and read in isolation from the
// thread, so a once-per-session banner would not reach the reader.
//
// An empty reply stays empty: sendReply skips it, and a footer with no
// content behind it helps nobody.
func (r *ChannelReceiver) disclosureFooter(text string) string {
	if r.Disclosure == nil || text == "" {
		return text
	}
	channel := r.Channel.Name()
	if aidisclosure.CadenceFor(channel) != aidisclosure.CadencePerMessage {
		return text
	}
	r.observeDisclosureServed(channel, aidisclosure.CadencePerMessage.String())
	// The "---" rule keeps the notice "clear and distinguishable from
	// surrounding content" (Art 50(5)) as plain text, on every channel,
	// without relying on markup a reader's client may not render.
	return text + "\n\n---\n" + r.Disclosure.Notice().Text
}

func (r *ChannelReceiver) observeDisclosureServed(channel, cadence string) {
	if r.DisclosureMetrics != nil {
		r.DisclosureMetrics.DisclosureServed(channel, cadence)
	}
}

func (r *ChannelReceiver) observeDisclosureFailure(channel, stage string) {
	if r.DisclosureMetrics != nil {
		r.DisclosureMetrics.DisclosureFailed(channel, stage)
	}
}

// sendReply routes a final assistant text back through the
// Channel's Send. Empty replies are skipped — channels reject empty
// outbound messages and an empty Send is a noop the caller doesn't
// need to see in the audit log.
//
// Send errors are logged via the channel's name + the session ID
// so an operator can correlate "the bot replied in the chat but
// nothing landed in the inbox" with the underlying transport
// failure (e.g. SMTP 550 alias-not-permitted). Previously the
// error was discarded — symptom: the chat audit row recorded a
// reply, the recipient never got it, and journald had no signal.
func (r *ChannelReceiver) sendReply(ctx context.Context, sessionID, text string) {
	if text == "" {
		return
	}
	if _, err := r.Channel.Send(ctx, conversation.ChannelMessage{
		Source:    r.Channel.Name(),
		SessionID: sessionID,
		Text:      text,
	}); err != nil {
		r.logger().Warn().
			Err(err).
			Str("channel", r.Channel.Name()).
			Str("session_id", sessionID).
			Int("text_bytes", len(text)).
			Msg("dispatcher: outbound Send failed — reply never reached recipient")
	}
}

// logger returns the receiver's logger or a no-op fallback. The
// ChannelReceiver type was originally designed without a logger
// field; this helper hides that legacy so the sendReply error
// path can log without forcing every callsite to supply a logger.
// Wired to the Agent's logger when r.Agent is a *Agent (the
// production case); test stubs fall through to zerolog.Nop().
func (r *ChannelReceiver) logger() *zerolog.Logger {
	if a, ok := r.Agent.(*Agent); ok && a != nil {
		return &a.logger
	}
	nop := zerolog.Nop()
	return &nop
}

// enrichUserContent folds the envelope context — the subject line and
// attachment metadata — into the user-visible message the dispatcher hands to
// the LLM. Without it the email channel (and any future channel that populates
// msg.Attachments) would persist attachment bytes as artifacts but the LLM
// would never learn the attachments existed, and a subject-borne instruction
// would be dropped entirely — `msg.Text` alone strips the envelope.
//
// Format kept terse so a chatty attachment list doesn't dominate
// the prompt:
//
//	Subject: add these books to rag
//
//	<msg.Text>
//
//	[Attached files]
//	- book.pdf (application/pdf, 2.3 MB) — artifact_id=art_abc123
//	- cover.jpg (image/jpeg, 188 KB) — artifact_id=art_def456
//
// When an attachment lacks an ArtifactID (no repo wired, or a
// channel that doesn't persist), the trailing "— artifact_id=..."
// segment is omitted so the LLM still sees the file's existence
// without a misleading "you can read it" hint.
//
// No subject and no attachments is unchanged: returns msg.Text verbatim so
// channels that carry neither (Telegram, GitHub, Slack today) keep their
// current single-line content shape. The subject comes from
// ChannelSpecific["subject"], which only the email channel populates today.
func enrichUserContent(msg conversation.ChannelMessage) string {
	subject := strings.TrimSpace(msg.ChannelSpecific[conversation.ChannelSpecificSubject])
	sender := strings.TrimSpace(msg.ChannelSpecific[conversation.ChannelSpecificSender])
	if len(msg.Attachments) == 0 && subject == "" && sender == "" {
		return msg.Text
	}
	var b strings.Builder
	// Envelope header block, then a blank line, then the body. Assembled as a
	// list so any combination of present/absent headers spaces correctly.
	//
	//   From:    who wrote this turn — keeps a multi-party thread attributable
	//            in history. SpeakerID feeds the operator-profile lookup but
	//            never reaches the prompt, so without this the lead saw a flat
	//            stream of user messages and could not tell correspondents
	//            apart.
	//   Subject: on email this routinely carries the whole instruction while
	//            the body is empty ("add these books to rag" + attachments).
	//            It used to be sanitised into ChannelSpecific and never read,
	//            leaving the LLM to guess (incident 2026-07-28).
	//
	// Channels supplying neither are byte-for-byte unchanged (fast path above).
	headers := make([]string, 0, 2)
	if sender != "" {
		headers = append(headers, "From: "+sender)
	}
	if subject != "" {
		headers = append(headers, "Subject: "+subject)
	}
	if len(headers) > 0 {
		b.WriteString(strings.Join(headers, "\n"))
		b.WriteString("\n\n")
	}
	b.WriteString(msg.Text)
	// A subject-only message (empty body, no files) needs no trailer — and
	// must not emit a dangling "[Attached files]" header with no entries.
	if len(msg.Attachments) == 0 {
		return strings.TrimRight(b.String(), "\n")
	}
	if msg.Text != "" && !strings.HasSuffix(msg.Text, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n[Attached files]\n")
	for _, a := range msg.Attachments {
		b.WriteString("- ")
		if a.Name != "" {
			b.WriteString(a.Name)
		} else {
			b.WriteString("(unnamed)")
		}
		// Type + size — both optional; render only when present so
		// channels that don't advertise size up front don't emit
		// noisy "(0 bytes)" strings.
		segs := make([]string, 0, 2)
		if a.MimeType != "" {
			segs = append(segs, a.MimeType)
		}
		if a.SizeBytes > 0 {
			segs = append(segs, humanBytes(a.SizeBytes))
		}
		if len(segs) > 0 {
			b.WriteString(" (")
			b.WriteString(strings.Join(segs, ", "))
			b.WriteString(")")
		}
		if a.ArtifactID != "" {
			b.WriteString(" — artifact_id=")
			b.WriteString(a.ArtifactID)
		}
		b.WriteString("\n")
		if a.Extraction != nil {
			writeExtractionTrailer(&b, a)
		}
	}
	return b.String()
}

// writeExtractionTrailer renders an attachment's extraction summary, and
// states whether that extraction can stand in for the original.
//
// The distinction is load-bearing in both directions:
//
//   - DOCUMENTS keep the exact phrase "↳ ingested into project memory".
//     It is a protocol string, not prose: BuildLeadSystemPrompt's INBOUND
//     ATTACHMENTS branch (a) matches on it to tell the lead the work is
//     done, and agent.go and tools.go match on it too. Do not reword it.
//
//   - MEDIA must NOT say that. An image's extraction is OCR plus EXIF, so
//     branch (a) firing on a photo told the dispatcher to report the file
//     as ingested and schedule nothing — suppressing the vision handover
//     entirely (D4, live on the email channel before this change). The
//     media form surfaces the same extraction ids, so the lead can still
//     use the OCR text or transcript, but denies interpretation explicitly.
//
// The denial is phrased negatively ("has NOT been interpreted") on purpose.
// The failure mode is a model concluding it already has the answer, and an
// absent claim is a weaker guard than an explicit denial.
//
// see LLD § https://docs.vornik.io §4.5b
func writeExtractionTrailer(b *strings.Builder, a conversation.Attachment) {
	kind := mediakind.Classify(a.Name, a.MimeType)
	if mediakind.ExtractionSufficient(kind) {
		b.WriteString("    ↳ ingested into project memory (")
		if a.Extraction.Title != "" {
			b.WriteString(a.Extraction.Title)
			if a.Extraction.Author != "" {
				b.WriteString(" by ")
				b.WriteString(a.Extraction.Author)
			}
			b.WriteString("; ")
		}
		fmt.Fprintf(b, "%d sections, %d chunks; extracted_document_id=%s",
			a.Extraction.SectionCount,
			a.Extraction.ChunksIngested,
			a.Extraction.ExtractedDocumentID)
		b.WriteString(")\n")
		return
	}

	// Media: what the extraction actually contains, then what it is not.
	var what string
	switch kind {
	case mediakind.KindImage:
		what = "metadata + OCR text only — the image itself has NOT been interpreted"
	case mediakind.KindAudio:
		what = "transcript only — non-speech audio has NOT been interpreted"
	case mediakind.KindVideo:
		what = "transcript + sampled keyframes only — the video has NOT been interpreted as a whole"
	default:
		what = "partial extraction only — this file has NOT been interpreted"
	}
	fmt.Fprintf(b, "    ↳ %s (%d sections; extracted_document_id=%s)\n",
		what, a.Extraction.SectionCount, a.Extraction.ExtractedDocumentID)
}

// humanBytes renders a byte count as a short human-readable string.
// Kept inline so the receiver doesn't need a dependency on a units
// package. Three sig figs is plenty for an attachment-list prompt;
// the LLM doesn't act on the exact byte count.
func humanBytes(n int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// Compile-time guard: *ChannelReceiver satisfies the conversation
// Receiver contract.
var _ conversation.Receiver = (*ChannelReceiver)(nil)
