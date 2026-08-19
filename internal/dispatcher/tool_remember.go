package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memory/ned"
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

// SetChatMemoryNED wires the shared-scope pre-commit named-entity resolution
// gate (slice 4, §6). Nil leaves shared writes reporting "not built" — the
// gate is the containment for third-party data in a shared deposit, so a
// shared write cannot persist without it.
//
// Writes through to the tool executor for the same reason SetMemoryWriteGate
// does: NewAgent builds a.toolExecutor once, so assigning only the agent field
// would leave the executor holding nil and the shared write path permanently
// dark (the 1234aea37 class).
func (a *Agent) SetChatMemoryNED(g *ned.Gate) {
	if a == nil {
		return
	}
	a.sharedNED = g
	if a.toolExecutor != nil {
		a.toolExecutor.sharedNED = g
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

	// PERSONAL scope persists end-to-end (slice 5): a chat_memory chunk in the
	// active project's memory, linked to the operator's own data subject. It
	// NEVER runs the NED gate. SHARED scope runs the slice-3 confirmation
	// machine and, once acknowledged, the slice-4 pre-commit NED gate before it
	// can persist (rememberShared).
	if scope != memoryScopeShared {
		return te.rememberPersonal(ctx, channel, sessionID, activeProject, in.Content)
	}
	return te.rememberShared(ctx, channel, sessionID, activeProject, in.Content)
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
//   - AUTHORIZED: authorizeSharedWrite grants → rememberSharedGranted runs the pre-commit NED
//     gate (slice 4) and, on a proceed verdict, persists the chat_memory chunk, links any
//     resolved third-party subjects, writes the append-only audit row and deletes the pending
//     row (one-shot). When the write path is not fully wired it falls back to the slice-3
//     terminal (audit + delete + "not built"). A NED block/error writes ZERO rows.
//
// The acknowledgement itself is stamped ELSEWHERE — ChannelReceiver.Receive, from a
// human-originated inbound turn — never from any argument the model passes here. That is why a
// model calling remember repeatedly in one turn only ever cycles PROPOSED → PROPOSED and can
// never reach AUTHORIZED (the parser.go:186 mistake, §5.3.2).
func (te *ToolExecutor) rememberShared(ctx context.Context, channel, sessionID, activeProject, content string) ToolResult {
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
	if errors.Is(err, persistence.ErrNotFound) {
		// No confirmation has been proposed for this conversation yet. That
		// is the FIRST-proposal path — authorizeSharedWrite takes a nil
		// record and proposes one. Reporting it as a failure would break
		// every shared write that had not already been proposed.
		rec = nil
	} else if err != nil {
		return ToolResult{Content: "I couldn't check the confirmation state for this " +
			"conversation, so nothing was saved. Tell the user there was a temporary problem " +
			"and to try again."}
	}

	switch decision := authorizeSharedWrite(rec, content, operatorID, now); {
	case decision.permits():
		return te.rememberSharedGranted(ctx, rec, channel, sessionID, activeProject, content, now)

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

// rememberSharedGranted is reached once authorizeSharedWrite has granted (the
// speaker's acknowledgement is on record). It is where slice 4 lives.
//
// When the shared WRITE path is not fully wired — no NED gate, no pipeline, no
// audit sink, or no active project — it falls back UNCHANGED to the slice-3
// terminal (rememberSharedAuthorized: audit + one-shot delete + "not built"),
// so a sqlite dev daemon or a partial deployment behaves exactly as slice 3
// did and the existing slice-3 tests still hold.
//
// When it IS fully wired it runs the pre-commit NED gate on the RAW content
// BEFORE any DB write (design D6.1, C1): a block/error verdict returns a
// refusal with ZERO rows written and leaves the pending confirmation in place;
// a proceed verdict carries the token the write path requires.
func (te *ToolExecutor) rememberSharedGranted(
	ctx context.Context,
	rec *persistence.ChatMemoryWriteConfirmation,
	channel, sessionID, activeProject, content string,
	now time.Time,
) ToolResult {
	if te.sharedNED == nil || te.chatMemory == nil || te.memoryAudit == nil || activeProject == "" {
		// Not fully wired — preserve the slice-3 terminal exactly.
		return te.rememberSharedAuthorized(ctx, rec, now)
	}

	// NED runs on the raw content BEFORE anything is written. A block or error
	// therefore leaves zero rows (the C1 orphan case is designed out) and the
	// pending confirmation stays so the user can rephrase and try again.
	decision := te.sharedNED.Screen(ctx, activeProject, content)
	switch decision.Verdict {
	case ned.VerdictBlock:
		return ToolResult{Content: sharedNEDBlockRefusal(decision.BlockedPersons)}
	case ned.VerdictError:
		te.logger.Warn().Err(decision.Err).Str("project", activeProject).
			Msg("remember: NED gate errored on a shared write — failing closed")
		return ToolResult{Content: sharedNEDErrorRefusal()}
	}

	// Proceed: persist, link resolved third parties, audit, then consume the
	// pending row. The token proves this went through the gate (I3).
	return te.rememberSharedWrite(ctx, rec, channel, sessionID, activeProject, content,
		now, decision.Authorization(), decision.MatchedEntityIDs)
}

// rememberSharedWrite persists an authorized, NED-cleared shared deposit.
//
// The `auth ned.SharedWriteAuthorization` parameter is the type-level
// guardrail (design I3): the token can only be minted by ned.Gate on a proceed
// verdict, so this function CANNOT be called — cannot even be compiled into a
// caller — without a shared write having gone through the NED gate first.
// Personal scope uses rememberPersonal, which takes no token and never runs
// NED. The defensive Granted() check is belt-and-braces; the real guarantee is
// that no other package can construct a granted token.
//
// Ordering: IngestChatMemory (create artifact → RunStandardGates → IngestText →
// backfill) → link resolved third-party subjects (D4.1) → audit → one-shot
// delete. A link failure compensates by deleting the just-written chunk (D4.1a)
// so no chunk carrying a resolved third party is left without a
// data_subject_links row (a silent Art-17 gap).
func (te *ToolExecutor) rememberSharedWrite(
	ctx context.Context,
	rec *persistence.ChatMemoryWriteConfirmation,
	channel, sessionID, activeProject, content string,
	now time.Time,
	auth ned.SharedWriteAuthorization,
	matchedEntityIDs []string,
) ToolResult {
	if !auth.Granted() {
		// Unreachable in practice (the caller passes a proceed-minted token);
		// present so the guardrail also holds at runtime, not only at compile time.
		return ToolResult{Content: "I couldn't authorize that shared write. Nothing has been saved."}
	}

	res, err := te.chatMemory.IngestChatMemory(ctx, activeProject, channel, sessionID, content)
	if err != nil {
		te.logger.Warn().Err(err).Str("project", activeProject).Str("channel", channel).
			Msg("remember: shared IngestChatMemory failed")
		return ToolResult{Content: "I hit a temporary problem saving that to shared memory, so nothing " +
			"was kept and your confirmation still stands. Tell the user there was a problem and to try again."}
	}
	if res.Stats.Admitted == 0 {
		return ToolResult{Content: "I couldn't keep that: the memory gates filtered it (it may be a " +
			"duplicate of something already in shared memory, too short, or contain a secret). Nothing was kept."}
	}

	// Link every resolved third party to every chunk (D4.1). A resolved person
	// carried into a shared chunk with no link is a silent Art-17 gap, so a link
	// failure compensates the whole write away (D4.1a).
	if len(matchedEntityIDs) > 0 {
		if linkErr := te.linkKGEntitiesToChunks(ctx, matchedEntityIDs, activeProject, res.ChunkIDs); linkErr != nil {
			if delErr := te.chatMemory.DeleteChatMemory(ctx, activeProject, res.ArtifactID); delErr != nil {
				te.logger.Error().Err(delErr).Str("project", activeProject).Str("artifact", res.ArtifactID).
					Msg("remember: COMPENSATION delete failed after shared link error — an unlinked chat_memory chunk may persist")
			}
			chatMemoryCompensationDeletes.Inc()
			te.logger.Warn().Err(linkErr).Str("project", activeProject).
				Msg("remember: shared data-subject link failed; compensated by deleting the chunk")
			return ToolResult{Content: "I couldn't record who this shared memory concerns, so I did NOT keep " +
				"it (a memory that can't be tied to the people in it can't be erased on request). Tell the user " +
				"there was a temporary problem and to try again."}
		}
	}

	// Accountability BEFORE consuming the pending row (§5.3.3). If the audit
	// cannot be written we cannot leave a persisted-but-unaccounted shared
	// chunk, so compensate it away and keep the pending row for a retry.
	ackAt := now
	if rec.AcknowledgedAt != nil {
		ackAt = *rec.AcknowledgedAt
	}
	if auditErr := te.memoryAudit.Record(ctx, &persistence.ChatMemoryWriteAudit{
		Channel:            rec.Channel,
		SessionID:          rec.SessionID,
		ContentFingerprint: rec.ContentFingerprint,
		Scope:              rec.Scope,
		OperatorID:         rec.OperatorID,
		ProposedAt:         rec.ProposedAt,
		AcknowledgedAt:     ackAt,
		GrantedAt:          now,
	}); auditErr != nil {
		if delErr := te.chatMemory.DeleteChatMemory(ctx, activeProject, res.ArtifactID); delErr != nil {
			te.logger.Error().Err(delErr).Str("project", activeProject).Str("artifact", res.ArtifactID).
				Msg("remember: COMPENSATION delete failed after audit error — an unaudited chat_memory chunk may persist")
		}
		chatMemoryCompensationDeletes.Inc()
		return ToolResult{Content: "The user's confirmation is valid, but I couldn't write the required " +
			"audit record, so I did NOT keep the memory. Tell the user there was a temporary problem and to try again."}
	}

	// One-shot: remove the pending row so a second remember() for the same
	// content finds nothing and cannot re-authorize.
	if err := te.memoryConfirms.Delete(ctx, rec.Channel, rec.SessionID); err != nil {
		te.logger.Warn().Err(err).Str("channel", rec.Channel).
			Msg("remember: shared write persisted+audited but pending-row delete failed")
		return ToolResult{Content: "The shared memory was saved and recorded, but I couldn't clear the " +
			"pending confirmation. Do not retry; tell the user it is handled."}
	}

	return ToolResult{Content: "Saved to SHARED project memory, readable by everyone in this project — " +
		"kept for about 90 days as low-confidence context, and anyone it names who is known to the project " +
		"is linked so an erasure request for them would find it. Tell the user it was saved."}
}

// linkKGEntitiesToChunks records a data-subject link for every resolved
// third-party entity (matchID = a knowledge_entities row id) against every
// just-written chunk, via the first production binder BindKGExtraction (D4.1).
// It reuses an existing subject when the kg_entity identifier is already known
// and otherwise creates one, at the SourceKGExtraction confidence ceiling.
//
// Returns an error (which the caller compensates on) when the linker is not
// wired but there ARE third parties to link — a shared chunk naming a resolved
// person MUST be linked, so "no linker" is a failure here, not a skip.
func (te *ToolExecutor) linkKGEntitiesToChunks(ctx context.Context, matchIDs []string, projectID string, chunkIDs []string) error {
	if len(matchIDs) == 0 {
		return nil
	}
	if te.dataSubjects == nil {
		return fmt.Errorf("no data-subject linker wired, but the deposit names %d resolved subject(s)", len(matchIDs))
	}
	seen := map[string]bool{}
	for _, matchID := range matchIDs {
		if matchID == "" || seen[matchID] {
			continue
		}
		seen[matchID] = true
		if err := te.linkOneKGEntity(ctx, matchID, projectID, chunkIDs); err != nil {
			return err
		}
	}
	return nil
}

// linkOneKGEntity resolves-or-creates the subject for a single matched entity
// and links it to every chunk. Split out of linkKGEntitiesToChunks so each
// half stays readable (and under the cognitive-complexity gate).
func (te *ToolExecutor) linkOneKGEntity(ctx context.Context, matchID, projectID string, chunkIDs []string) error {
	subjectID, err := te.dataSubjects.FindSubjectByIdentifier(ctx, datasubject.KindKGEntity, matchID)
	if err != nil {
		return fmt.Errorf("find subject for %s: %w", matchID, err)
	}
	if subjectID == "" {
		// New subject: create it and record the kg_entity identifier (at the
		// ceiling, enforced by the binder) so a later deposit reuses it.
		subjectID = persistence.GenerateID("subject")
		// Through the shared helper: KG resolution (increment 4) recognises a
		// placeholder BY THIS NAME when it folds one into an identified person.
		// If the two spellings diverged, every subject minted here would present
		// as an un-adoptable conflict and one person would keep two subject rows.
		if err := te.dataSubjects.CreateSubject(ctx, datasubject.Subject{
			ID: subjectID, DisplayName: datasubject.PlaceholderSubjectName(matchID),
		}); err != nil {
			return fmt.Errorf("create subject for %s: %w", matchID, err)
		}
		idOnly, err := datasubject.BindKGExtraction(matchID, "", projectID, datasubject.ConfidencePossible)
		if err != nil {
			return fmt.Errorf("bind identifier for %s: %w", matchID, err)
		}
		for _, id := range idOnly.Identifiers {
			if err := te.dataSubjects.AddIdentifier(ctx, subjectID, id); err != nil {
				return fmt.Errorf("add identifier for %s: %w", matchID, err)
			}
		}
	}
	for _, cid := range chunkIDs {
		b, err := datasubject.BindKGExtraction(matchID, cid, projectID, datasubject.ConfidencePossible)
		if err != nil {
			return fmt.Errorf("bind link for %s/%s: %w", matchID, cid, err)
		}
		for _, l := range b.Links {
			if err := te.dataSubjects.AddLink(ctx, subjectID, l); err != nil {
				return fmt.Errorf("add link for %s/%s: %w", matchID, cid, err)
			}
		}
	}
	return nil
}

// sharedNEDBlockRefusal is the §6.2.1 refusal for a `new`/`ambiguous` verdict:
// it NAMES the person(s) detected (hiding the name forces the user to guess
// what tripped the gate) and offers the three concrete paths.
func sharedNEDBlockRefusal(persons []string) string {
	who := "someone"
	if named := namedList(persons); named != "" {
		who = named
	}
	return "I can't save that to shared memory: it names " + who + ", and I can't link them to a known " +
		"person in this project, so an erasure request for them later would not find this. You can: rephrase " +
		"it without naming them, ask me to save it to your personal profile instead, or route it through a " +
		"task so it gets reviewed before it lands. Nothing has been saved. Relay this to the user."
}

// sharedNEDErrorRefusal is the fail-closed refusal (D6.3) — distinct from a
// block: NED could not run, so we could not verify who the note names.
func sharedNEDErrorRefusal() string {
	return "I couldn't verify who that shared note might name (the check failed), so I did NOT save it — " +
		"on shared memory I fail safe rather than risk storing something about a person we can't later erase. " +
		"Tell the user to try again in a moment, or to save it to their personal profile instead. Nothing has been saved."
}

// namedList renders a person list for the refusal: "Alice", "Alice and Bob",
// or "Alice, Bob and Carol". Empty/blank names are dropped.
func namedList(persons []string) string {
	cleaned := make([]string, 0, len(persons))
	for _, p := range persons {
		if p = strings.TrimSpace(p); p != "" {
			cleaned = append(cleaned, p)
		}
	}
	switch len(cleaned) {
	case 0:
		return ""
	case 1:
		return cleaned[0]
	case 2:
		return cleaned[0] + " and " + cleaned[1]
	default:
		return strings.Join(cleaned[:len(cleaned)-1], ", ") + " and " + cleaned[len(cleaned)-1]
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
