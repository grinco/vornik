// Package misscontract declares, once, what every single-entity repository
// lookup returns when the row it asks for is absent.
//
// It exists because there was no convention. An audit on 2026-08-19 found
// 28 methods returning persistence.ErrNotFound on a miss and 7 returning
// (nil, nil) on the sqlite backend, 29 and 8 on postgres — and
// KnowledgeEntityRepository.Get disagreeing with itself across the two. An
// author writing a test double for such a method was guessing, and guessing
// strict is the direction that silently certifies: fakeExtractedDocRepo.Get
// returned an error where production returned (nil, nil), so every test took
// the err != nil branch, none reached the dereference below it, and the
// resulting nil-pointer panic crash-looped the daemon 28 times in ten
// minutes on document-ingest's first run (7f0b5337, 4e7b4910).
//
// The package deliberately does not import testing: cmd/lint-lld-contracts
// links it to enforce the contract statically. The assertion helper that
// wraps it for tests lives in internal/persistence/repotest.
package misscontract

import (
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// MissBehavior is what a single-entity lookup produces for an absent row.
type MissBehavior int

const (
	// MissErrNotFound means the lookup returns (nil, persistence.ErrNotFound)
	// for an absent row. This is the convention; every registered key
	// declares it.
	MissErrNotFound MissBehavior = iota

	// MissNilNil means the lookup returns (nil, nil) and absence is ordinary.
	//
	// No key declares this today — the backends were normalised to
	// ErrNotFound so that a double author cannot guess wrong. It stays in
	// the vocabulary so that a future deliberate exception is expressible
	// and therefore visible, rather than reappearing by accident.
	MissNilNil
)

func (b MissBehavior) String() string {
	switch b {
	case MissErrNotFound:
		return "MissErrNotFound"
	case MissNilNil:
		return "MissNilNil"
	default:
		return fmt.Sprintf("MissBehavior(%d)", int(b))
	}
}

// Contract maps "<Interface>.<Method>" to the behaviour an absent row
// produces. Every single-entity lookup on a persistence interface is either
// registered here or named in Excluded; cmd/lint-lld-contracts fails the
// build when a lookup is in neither, so the table cannot rot as interfaces
// grow.
var Contract = map[string]MissBehavior{
	"A2APushConfigRepository.Get":                  MissErrNotFound, // *A2APushConfig
	"APIKeyRepository.LookupActiveByHash":          MissErrNotFound, // *APIKey
	"ArtifactRepository.Get":                       MissErrNotFound, // *Artifact
	"ArtifactRepository.GetByHash":                 MissErrNotFound, // *Artifact
	"ChannelSessionRepository.Load":                MissErrNotFound, // *ChannelSession
	"ChatAuditRepository.GetByID":                  MissErrNotFound, // *ChatAuditEntry
	"ChatMemoryWriteConfirmationRepository.Get":    MissErrNotFound, // *ChatMemoryWriteConfirmation
	"CorpusEpochRepository.GetEpoch":               MissErrNotFound, // *CorpusEpoch
	"CostTuningCanaryRepository.GetByProposalID":   MissErrNotFound, // *CostTuningCanary
	"CrossProjectCallRepository.GetByCalleeTaskID": MissErrNotFound, // *CrossProjectCall
	"CrossProjectCallRepository.Get":               MissErrNotFound, // *CrossProjectCall
	"DaemonLeaderLockRepository.Get":               MissErrNotFound, // *DaemonLeaderLock
	// (nil, nil): a PR with no review state is the ORDINARY first-delivery
	// case, and callers read it as "never reviewed, nothing in flight" — the
	// fail-toward-more-review direction. ErrNotFound here would make every
	// first push look like a failure at the one moment it is most normal.
	"ForgePRReviewStateRepository.Get":               MissNilNil,      // *ForgePRReviewState
	"ExecutionQualityScoreRepository.GetByExecution": MissErrNotFound, // *ExecutionQualityScore
	"ExecutionRepository.GetByTaskID":                MissErrNotFound, // *Execution
	"ExecutionRepository.Get":                        MissErrNotFound, // *Execution
	"ExecutionToolGrantRepository.Current":           MissErrNotFound, // *ExecutionToolGrant
	"ExtractedDocumentRepository.GetByArtifact":      MissErrNotFound, // *ExtractedDocument
	"ExtractedDocumentRepository.Get":                MissErrNotFound, // *ExtractedDocument
	"FixItSessionRepository.Get":                     MissErrNotFound, // *FixItSession
	"HealingTriggerOverrideRepository.Get":           MissErrNotFound, // *HealingTriggerOverride
	"IdentityRepository.GetGroupByName":              MissErrNotFound, // *Group
	"InstallationOnboardingSessionRepository.Get":    MissErrNotFound, // *InstallationOnboardingSession
	"InstinctRepository.Get":                         MissErrNotFound, // *Instinct
	"KnowledgeEdgeRepository.Get":                    MissErrNotFound, // *KnowledgeEdge
	"KnowledgeEntityRepository.GetByCanonical":       MissErrNotFound, // *KnowledgeEntity
	"KnowledgeEntityRepository.Get":                  MissErrNotFound, // *KnowledgeEntity
	"MCPOAuthTokenRepository.Get":                    MissErrNotFound, // *MCPOAuthToken
	"MemoryQuarantineRepository.Get":                 MissErrNotFound, // *MemoryQuarantineItem
	"OperatorIdentityLinkRepository.Get":             MissErrNotFound, // *OperatorIdentityLink
	"OperatorProfileRepository.Get":                  MissErrNotFound, // *OperatorProfile
	"ProjectSpawnRepository.GetBySpawnedProject":     MissErrNotFound, // *ProjectSpawn
	"ProjectWizardSessionRepository.Get":             MissErrNotFound, // *ProjectWizardSession
	"ProposalRepository.GetByID":                     MissErrNotFound, // *ControlPlaneProposal
	"ReminderRepository.Get":                         MissErrNotFound, // *Reminder
	"SkillRepository.GetByID":                        MissErrNotFound, // *Skill
	"SkillRepository.Get":                            MissErrNotFound, // *Skill
	"TaskJudgeVerdictRepository.GetByTask":           MissErrNotFound, // *TaskJudgeVerdict
	"TaskMessageRepository.GetOpenCheckpoint":        MissErrNotFound, // *TaskMessage
	"TaskPostMortemRepository.Get":                   MissErrNotFound, // *TaskPostMortem
	"TaskRepository.GetByIdempotencyKey":             MissErrNotFound, // *Task
	"TaskRepository.Get":                             MissErrNotFound, // *Task
	"TaskScratchpadRepository.Get":                   MissErrNotFound, // *TaskScratchpad
	"TelegramPollerStateRepository.Get":              MissErrNotFound, // *TelegramPollerState
	"TelegramThreadRepository.GetByTask":             MissErrNotFound, // *TelegramTaskThread
	"TelegramThreadRepository.GetByThread":           MissErrNotFound, // *TelegramTaskThread
	"UISessionRepository.GetActiveByTokenHash":       MissErrNotFound, // *UISession
	"WebWriteRepo.Get":                               MissErrNotFound, // *WebWriteAction
	"WorkflowHealingCandidateRepository.Get":         MissErrNotFound, // *HealingCandidate
	"WorkflowHealingTrialRepository.Get":             MissErrNotFound, // *HealingTrial
	"WorkflowHealingTriggerRepository.Get":           MissErrNotFound, // *HealingTrigger
	"WorkflowProposalRepository.Get":                 MissErrNotFound, // *WorkflowProposal
}

// Excluded names methods that match a lookup's shape — (*T, error) — but are
// not keyed lookups, with the reason. Absence from Contract is therefore a
// stated decision rather than an oversight.
var Excluded = map[string]string{
	"DBTX.QueryContext":                            "not a repository lookup; returns *sql.Rows",
	"ChunkGraphExtractionRepository.Stats":         "aggregate over a set; always produces a value",
	"MemoryRetrievalAuditRepository.FeedbackStats": "aggregate over a set; always produces a value",
	"SkillRepository.Upsert":                       "a write that returns the stored row, not a lookup",
	"TaskRepository.LeaseTask":                     "a queue poll: an empty queue is the common case, not a miss",
}

// Behavior returns the declared behaviour for key.
func Behavior(key string) (MissBehavior, bool) {
	b, ok := Contract[key]
	return b, ok
}

// Check reports whether the (value, error) pair a lookup returned for an
// absent row satisfies the contract registered for key. It returns nil when
// the pair conforms. An unregistered key is itself an error, so that a typo
// in a caller's key cannot pass vacuously.
func Check(key string, valueIsNil bool, err error) error {
	want, ok := Behavior(key)
	if !ok {
		if reason, excluded := Excluded[key]; excluded {
			return fmt.Errorf("%s is excluded from the miss contract (%s)", key, reason)
		}
		return fmt.Errorf("%s is not registered in the miss contract", key)
	}
	if cerr := CheckBehavior(want, valueIsNil, err); cerr != nil {
		return fmt.Errorf("%s: %w", key, cerr)
	}
	return nil
}

// CheckBehavior reports whether a (value, error) pair satisfies want.
func CheckBehavior(want MissBehavior, valueIsNil bool, err error) error {
	if !valueIsNil {
		return fmt.Errorf("a miss returned a non-nil value; %s requires a nil value", want)
	}
	switch want {
	case MissErrNotFound:
		if err == nil {
			return errors.New("a miss returned a nil error; the contract is persistence.ErrNotFound")
		}
		if !errors.Is(err, persistence.ErrNotFound) {
			return fmt.Errorf("a miss returned %v; the contract is persistence.ErrNotFound", err)
		}
		return nil
	case MissNilNil:
		if err != nil {
			return fmt.Errorf("a miss returned %v; the contract is a nil error", err)
		}
		return nil
	default:
		return fmt.Errorf("unknown miss behaviour %s", want)
	}
}
