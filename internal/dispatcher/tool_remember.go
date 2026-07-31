package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

// ChatMemoryWriter persists a chat-deposited fact to project memory as a
// chat_memory chunk, and can undo a partial write. Implemented by
// *memory.Pipeline (IngestChatMemory / DeleteChatMemory). Nil leaves the
// personal write path reporting "not built", so a deployment without the
// pipeline wired behaves as slice 3 did.
type ChatMemoryWriter interface {
	IngestChatMemory(ctx context.Context, projectID, channel, sessionID, content string) (memory.ChatMemoryIngestResult, error)
	DeleteChatMemory(ctx context.Context, projectID, artifactID string) error
}

// DataSubjectLinker is the narrow slice of the GDPR data-subject
// repository the chat-memory write needs to link a chunk to the operator
// who deposited it, so an Art 17 erasure for that operator finds it.
// Implemented by *postgres.DataSubjectRepository. Nil skips the link (the
// write still happens) — but production must wire it; the §6.1 coverage
// otherwise regresses.
type DataSubjectLinker interface {
	FindSubjectByIdentifier(ctx context.Context, kind, value string) (string, error)
	CreateSubject(ctx context.Context, s datasubject.Subject) error
	AddIdentifier(ctx context.Context, subjectID string, id datasubject.Identifier) error
	AddLink(ctx context.Context, subjectID string, l datasubject.Link) error
}

// chatMemoryCompensationDeletes counts D4.1a compensation deletes — a
// chat chunk written but then dropped because its data_subject_link
// could not be recorded. This fallback is rare by design; a non-zero
// count flags a link-layer problem (the same failure class the
// reranker/distiller unbilled-call-site incidents taught us to make
// visible rather than swallow), so it is a named, attributable metric.
var chatMemoryCompensationDeletes = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "vornik",
	Subsystem: "chat_memory",
	Name:      "compensation_delete_total",
	Help:      "Chat-memory writes rolled back because the data-subject link failed after the chunk insert (design D4.1a).",
})

// SLICE 1 of the chat memory-write design
// (https://docs.vornik.io), after three review
// rounds. This slice is the HARD GATE and nothing else — there is deliberately NO write
// path yet, because the gate has to exist before anything can write.
//
// WHY THE GATE IS THE DESIGN. Review round 1 rejected the original proposal for offering
// downranking as the injection mitigation: "The difference between 'never writes' and
// 'writes with low weight' is not a security boundary — it's a ranking problem with an
// adversarial optimizer on the input side." A chat turn is untrusted input, and once it is
// in project memory it enters the lead's prompt and every future task, outliving the
// conversation that carried it. §5.1's channel-level opt-in is the boundary that replaced
// the ranking story.
//
// WHAT THE GATE DOES NOT BUY, per review 2: authorization containment, not blast-radius
// containment. A misleading `remember` inside a granted channel still writes something that
// persists until per-data-subject Art 17 deletion. That is accepted — the write was
// authorized. The gate bounds WHO may write, never WHAT a permitted writer may say.

// MemoryWriteGate answers whether a given channel session has been granted the capability
// to write durable project memory.
//
// Deny-by-default at both levels: a nil gate means the capability is absent from this
// deployment, and a gate that returns false means this channel was never granted it. The
// two are indistinguishable to the caller on purpose (see rememberUnavailable).
type MemoryWriteGate interface {
	CanWriteMemory(ctx context.Context, channel, sessionID string) bool
}

// SetMemoryWriteGate wires the capability check.
//
// Writes through to the tool executor as well: NewAgent builds a.toolExecutor once, so a
// setter that assigned only the agent field would leave the executor holding nil — the bug
// that made the chat bug-report tool permanently dark (fixed 1234aea37, found in
// production rather than in tests).
func (a *Agent) SetMemoryWriteGate(g MemoryWriteGate) {
	if a == nil {
		return
	}
	a.memoryWrite = g
	if a.toolExecutor != nil {
		a.toolExecutor.memoryWrite = g
	}
}

// SetMemoryWriteConfirmations wires the shared-scope confirmation store and its append-only
// audit companion (chat memory-write design §5.3, slice 3 part 2).
//
// Writes through to the tool executor as well, for the exact reason SetMemoryWriteGate does:
// NewAgent builds a.toolExecutor once, so a setter that assigned only the agent field would
// leave the executor holding nil and the shared-scope path permanently reporting "not built"
// even after wiring — the class of bug that shipped the chat bug-report tool dark (1234aea37).
func (a *Agent) SetMemoryWriteConfirmations(
	confirms persistence.ChatMemoryWriteConfirmationRepository,
	audit persistence.ChatMemoryWriteAuditRepository,
) {
	if a == nil {
		return
	}
	a.memoryConfirms = confirms
	a.memoryAudit = audit
	if a.toolExecutor != nil {
		a.toolExecutor.memoryConfirms = confirms
		a.toolExecutor.memoryAudit = audit
	}
}

// SetChatMemoryWriter wires the personal-scope memory-write path (slice 5): the
// pipeline that persists a chat_memory chunk and the data-subject repository that
// links it to the operator for Art 17. Either may be nil — a nil writer leaves the
// personal path reporting "not built", a nil linker skips the (required-in-prod)
// data-subject link.
//
// Writes through to the tool executor for the same reason SetMemoryWriteGate does:
// NewAgent builds a.toolExecutor once, so assigning only the agent field would leave
// the executor holding nil and the write path permanently dark (the 1234aea37 class).
func (a *Agent) SetChatMemoryWriter(w ChatMemoryWriter, linker DataSubjectLinker) {
	if a == nil {
		return
	}
	a.chatMemory = w
	a.dataSubjects = linker
	if a.toolExecutor != nil {
		a.toolExecutor.chatMemory = w
		a.toolExecutor.dataSubjects = linker
	}
}

// rememberUnavailable is the single refusal text.
//
// IDENTICAL whether the capability is absent or merely ungranted for this channel (review
// 2 minor). Two different messages would form a capability-existence oracle: a caller could
// learn which deployments have the feature configured but withheld. It also matches the
// shape `get_channel_thread` already uses, so the surface reads consistently.
const rememberUnavailable = "Saving to memory is not available on this deployment."

type rememberArgs struct {
	Content string `json:"content"`
	// Scope is the model's REQUEST for where the fact goes. Not authoritative — see
	// resolveMemoryScope: anything unrecognised, including silence, is personal.
	Scope string `json:"scope"`
}

// remember is the chat-facing memory-write tool.
//
// SLICE 1 BEHAVIOUR: gate, validate, then say plainly that the write path is not built.
// It deliberately does NOT accept-and-discard — a tool that silently swallows the content
// is exactly the "it said it would remember and nothing happened" report that started this
// work, and shipping that shape again while the write path is pending would recreate the
// original bug with extra steps.
func (te *ToolExecutor) remember(ctx context.Context, args, activeProject string) ToolResult {
	channel, sessionID := originatingChannelFromContext(ctx)

	// No originating channel (API, A2A, an autonomy tick) means there is no channel whose
	// grant could be checked. Refuse rather than fall through to a default.
	if te.memoryWrite == nil || channel == "" || sessionID == "" {
		return ToolResult{Content: rememberUnavailable}
	}
	if !te.memoryWrite.CanWriteMemory(ctx, channel, sessionID) {
		return ToolResult{Content: rememberUnavailable}
	}

	var in rememberArgs
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			return ToolResult{Content: fmt.Sprintf(
				"remember: could not parse arguments (%v). Pass the `content` to remember.", err)}
		}
	}
	if strings.TrimSpace(in.Content) == "" {
		return ToolResult{Content: "remember needs `content`: the fact to keep."}
	}

	scope := resolveMemoryScope(in.Scope)

	// PERSONAL scope now persists end-to-end (slice 5): a chat_memory chunk in the
	// active project's memory, linked to the operator's own data subject. SHARED
	// scope still runs the slice-3 confirmation machine and waits for slice 4's NED
	// gate before it can persist (rememberShared).
	if scope != memoryScopeShared {
		return te.rememberPersonal(ctx, channel, sessionID, activeProject, in.Content)
	}
	return te.rememberShared(ctx, channel, sessionID, in.Content)
}

// rememberPersonal persists a personal-scope chat deposit (chat memory-write design
// slice 5): the fact is the operator's OWN data, so it is written as a chat_memory
// chunk in the active project and linked to the operator's data subject for Art 17.
//
// Personal scope runs NO named-entity resolution — it does not extract third parties
// (that gate is shared-scope-only, slice 4). The residual §6.1 boundary (a chat note
// may name someone the write does not index) is covered by the Art 15/17 disclosure,
// not by a gate here.
//
// If the post-insert data-subject link fails, the chunk is COMPENSATED away rather
// than left unlinked — an unlinked chunk is a silent Art-17 gap (design D4.1a).
func (te *ToolExecutor) rememberPersonal(ctx context.Context, channel, sessionID, activeProject, content string) ToolResult {
	// Write path not wired (no pipeline, or no active project to write into):
	// report plainly rather than implying a save, exactly as slice 3 did.
	if te.chatMemory == nil || activeProject == "" {
		return rememberSaveNotBuilt(memoryScopePersonal)
	}
	// The chunk is the operator's own data; without a resolvable operator identity
	// there is nobody to attribute it to for Art 17, and a personal note with no
	// owner is meaningless. Refuse (the §5.6.5 "identity cannot be resolved" rule).
	operatorID, _ := operatorIDFromContext(ctx)
	if operatorID == "" {
		return ToolResult{Content: "I can't save that to your personal memory: I can't tell who " +
			"is speaking, and a personal note has to belong to someone. Nothing has been saved."}
	}

	res, err := te.chatMemory.IngestChatMemory(ctx, activeProject, channel, sessionID, content)
	if err != nil {
		te.logger.Warn().Err(err).Str("project", activeProject).Str("channel", channel).
			Msg("remember: IngestChatMemory failed")
		return ToolResult{Content: "I hit a temporary problem saving that, so nothing was kept. " +
			"Tell the user there was a problem and to try again."}
	}
	if res.Stats.Admitted == 0 {
		// Gated out: duplicate, too short, or a secret-dump. Report truthfully
		// rather than implying a save.
		return ToolResult{Content: "I couldn't keep that: the memory gates filtered it (it may be a " +
			"duplicate of something already in memory, too short, or contain a secret). Nothing was kept."}
	}

	// GDPR link (design D4.1, operator-self variant). Best case the linker is wired;
	// if it is and the link fails, compensate.
	if te.dataSubjects != nil {
		if linkErr := te.linkOperatorToChunks(ctx, operatorID, activeProject, res.ChunkIDs); linkErr != nil {
			if delErr := te.chatMemory.DeleteChatMemory(ctx, activeProject, res.ArtifactID); delErr != nil {
				te.logger.Error().Err(delErr).Str("project", activeProject).Str("artifact", res.ArtifactID).
					Msg("remember: COMPENSATION delete failed after link error — an unlinked chat_memory chunk may persist")
			}
			chatMemoryCompensationDeletes.Inc()
			te.logger.Warn().Err(linkErr).Str("project", activeProject).
				Msg("remember: data-subject link failed; compensated by deleting the chunk")
			return ToolResult{Content: "I couldn't record who this memory belongs to, so I did NOT keep " +
				"it (a memory that can't be tied to a person can't be erased on request). Tell the user " +
				"there was a temporary problem and to try again."}
		}
	}

	return ToolResult{Content: "Saved to this project's memory as a personal chat note — it's kept for " +
		"about 90 days and only surfaces as low-confidence context. Tell the user it was saved."}
}

// linkOperatorToChunks resolves (or creates) the operator's data subject and links
// every chunk of the just-written deposit to it, so an Art 17 erasure for that
// operator finds the chat_memory chunk (design D4.1). The identity is the operator
// id, and the link is recorded as an operator assertion at PROBABLE confidence —
// downgraded from the source ceiling because the operator id is only as strong as
// the channel that produced it (§5.6.5), and UNKNOWN exclusivity because personal
// scope runs no NED, so whether the note also concerns others is undetermined
// (treated as shared by the erasure planner — the safe direction).
func (te *ToolExecutor) linkOperatorToChunks(ctx context.Context, operatorID, projectID string, chunkIDs []string) error {
	subjectID, err := te.dataSubjects.FindSubjectByIdentifier(ctx, datasubject.KindOperatorID, operatorID)
	if err != nil {
		return fmt.Errorf("find subject: %w", err)
	}
	if subjectID == "" {
		subjectID = persistence.GenerateID("subject")
		if err := te.dataSubjects.CreateSubject(ctx, datasubject.Subject{ID: subjectID, DisplayName: operatorID}); err != nil {
			return fmt.Errorf("create subject: %w", err)
		}
		if err := te.dataSubjects.AddIdentifier(ctx, subjectID, datasubject.Identifier{
			Kind:       datasubject.KindOperatorID,
			Value:      operatorID,
			Source:     datasubject.SourceOperatorAsserted,
			Confidence: datasubject.ConfidenceProbable,
		}); err != nil {
			return fmt.Errorf("add identifier: %w", err)
		}
	}
	for _, cid := range chunkIDs {
		if err := te.dataSubjects.AddLink(ctx, subjectID, datasubject.Link{
			Table:       datasubject.TableProjectMemoryChunks,
			RowID:       cid,
			ProjectID:   projectID,
			Source:      datasubject.SourceOperatorAsserted,
			Confidence:  datasubject.ConfidenceProbable,
			Exclusivity: datasubject.UnknownExclusivity,
		}); err != nil {
			return fmt.Errorf("add link for chunk %s: %w", cid, err)
		}
	}
	return nil
}

// rememberSaveNotBuilt is the "gate passed, but the write path is not built yet" response.
//
// Kept byte-identical to the slice-2 message for the personal path (and reused for shared when
// the confirmation store is unwired), so the model always tells the user which scope it would
// have used rather than implying a save.
func rememberSaveNotBuilt(scope memoryScope) ToolResult {
	where := "your personal profile (only you)"
	if scope == memoryScopeShared {
		where = "shared project memory (everyone in this project)"
	}
	return ToolResult{Content: fmt.Sprintf(
		"Scope resolved to %s — it would go to %s. The save path is NOT implemented yet, so "+
			"nothing has been kept. Tell the user plainly that you could not save it, and "+
			"which scope you would have used, rather than implying you did.", scope, where)}
}

// rememberShared runs the shared-scope confirmation state machine (design §5.3.2). It NEVER
// writes a memory chunk — that is slice 4-5. Its three outcomes are:
//
//   - PROPOSED: no acknowledged row for this content → upsert a pending confirmation
//     (acknowledged_at NULL, expires in 15m) and return a confirmation request that lists the
//     accepted phrases verbatim. No write.
//   - already-proposed-awaiting: an unacknowledged pending row for the SAME content and speaker
//     is still live → re-list the phrases (§5.3.3), which is exactly when the user typed
//     something that did not match. No write.
//   - AUTHORIZED: authorizeSharedWrite grants → write the append-only audit row, delete the
//     pending row (one-shot), and report that the write is authorized but the persist path
//     (slices 4-5) is not built.
//
// The acknowledgement itself is stamped ELSEWHERE — ChannelReceiver.Receive, from a
// human-originated inbound turn — never from any argument the model passes here. That is why a
// model calling remember repeatedly in one turn only ever cycles PROPOSED → PROPOSED and can
// never reach AUTHORIZED (the parser.go:186 mistake, §5.3.2).
func (te *ToolExecutor) rememberShared(ctx context.Context, channel, sessionID, content string) ToolResult {
	// Shared machinery not wired: report not-built rather than silently proposing into a store
	// that does not exist.
	if te.memoryConfirms == nil {
		return rememberSaveNotBuilt(memoryScopeShared)
	}
	// A shared write is bound to the speaker who proposed it (only they may acknowledge). With
	// no resolvable identity there is nobody who could ever discharge the proposal, so refuse
	// rather than store an undischargeable row (§5.6.5: where identity cannot be resolved,
	// refuse).
	operatorID, _ := operatorIDFromContext(ctx)
	if operatorID == "" {
		return ToolResult{Content: "I can't propose a shared-memory write here: I can't tell " +
			"who is speaking, and a shared write must be confirmed by the person who asked for " +
			"it. Nothing has been saved."}
	}

	now := time.Now()
	fp := sharedWriteFingerprint(content)
	rec, err := te.memoryConfirms.Get(ctx, channel, sessionID)
	if err != nil {
		return ToolResult{Content: "I couldn't check the confirmation state for this " +
			"conversation, so nothing was saved. Tell the user there was a temporary problem " +
			"and to try again."}
	}

	switch decision := authorizeSharedWrite(rec, content, operatorID, now); {
	case decision.permits():
		return te.rememberSharedAuthorized(ctx, rec, now)

	case rec != nil && !rec.Acknowledged() && rec.OperatorID == operatorID &&
		rec.ContentFingerprint == fp && now.Before(rec.ExpiresAt):
		// Same content already proposed by this speaker, still awaiting confirmation. Re-list
		// the phrases so a user who typed the wrong thing can discover the right one.
		return ToolResult{Content: "You already proposed saving this to shared project memory " +
			"and the user has not confirmed yet. Do NOT save it or call this again. Ask the " +
			"user to reply with exactly one of these phrases: " + acceptedSharePhrasesText() + "."}

	default:
		// PROPOSED (including a superseded/expired/changed prior row): upsert and ask.
		if err := te.memoryConfirms.Propose(ctx, &persistence.ChatMemoryWriteConfirmation{
			Channel:            channel,
			SessionID:          sessionID,
			ContentFingerprint: fp,
			Scope:              string(memoryScopeShared),
			OperatorID:         operatorID,
			ProposedAt:         now,
			ExpiresAt:          now.Add(sharedConfirmationTTL),
		}); err != nil {
			return ToolResult{Content: "I couldn't record the confirmation for this shared " +
				"write, so nothing was saved. Tell the user there was a temporary problem."}
		}
		return ToolResult{Content: "This would go to SHARED project memory, readable by " +
			"EVERYONE in this project — not just the person who asked. Nothing has been saved " +
			"yet. To confirm, the user must reply with exactly one of these phrases: " +
			acceptedSharePhrasesText() + ". Relay this to the user and wait for them to type " +
			"it; do not claim the memory was saved."}
	}
}

// rememberSharedAuthorized handles the AUTHORIZED transition: audit, then one-shot delete, then
// report that the persist path is not built.
//
// The audit row is written BEFORE the pending row is deleted (§5.3.3), and the write is refused
// outright if the audit sink is missing — an authorized shared write without an Art 5(2)
// accountability record is exactly what review round 4 refused to ship. The actual memory-chunk
// write (IngestText, NED, the content class) is slices 4-5 and deliberately NOT done here.
func (te *ToolExecutor) rememberSharedAuthorized(ctx context.Context, rec *persistence.ChatMemoryWriteConfirmation, now time.Time) ToolResult {
	if te.memoryAudit == nil {
		return ToolResult{Content: "The user's confirmation is valid, but the audit trail " +
			"needed to record a shared write is not configured, so I have NOT authorized it. " +
			"Tell the user this needs an operator to finish setup."}
	}
	ackAt := now
	if rec.AcknowledgedAt != nil {
		ackAt = *rec.AcknowledgedAt
	}
	if err := te.memoryAudit.Record(ctx, &persistence.ChatMemoryWriteAudit{
		Channel:            rec.Channel,
		SessionID:          rec.SessionID,
		ContentFingerprint: rec.ContentFingerprint,
		Scope:              rec.Scope,
		OperatorID:         rec.OperatorID,
		ProposedAt:         rec.ProposedAt,
		AcknowledgedAt:     ackAt,
		GrantedAt:          now,
	}); err != nil {
		// Without the accountability record, do NOT proceed to the one-shot delete that would
		// also destroy the pending row — leave it in place for a retry.
		return ToolResult{Content: "The user's confirmation is valid, but I couldn't write the " +
			"required audit record, so nothing was authorized. Tell the user there was a " +
			"temporary problem and to try again."}
	}
	// One-shot: remove the pending row so a second remember() for the same content finds
	// nothing and cannot re-authorize.
	if err := te.memoryConfirms.Delete(ctx, rec.Channel, rec.SessionID); err != nil {
		return ToolResult{Content: "The shared write was authorized and recorded, but I " +
			"couldn't clear the pending confirmation. Do not retry; tell the user it is handled."}
	}
	return ToolResult{Content: "The user has confirmed sharing this to project memory, so the " +
		"write is AUTHORIZED. The step that actually persists the memory is not built yet, so " +
		"nothing has been written. Tell the user their confirmation was accepted but the save " +
		"itself is not yet implemented — do not imply it has been kept."}
}

// WithCallSiteForTest sets the originating channel + session on a context so tests can
// exercise channel-scoped tools without reaching into the unexported context key.
//
// Test-only helper kept beside the production code it serves, rather than duplicated in
// each test file that needs it.
func WithCallSiteForTest(ctx context.Context, channel, sessionID string) context.Context {
	return withOriginatingChannel(ctx, channel, sessionID)
}
