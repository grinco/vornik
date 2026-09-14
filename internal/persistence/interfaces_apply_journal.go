package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// config_apply_journal — the durable record of a multi-file config apply
// (https://docs.vornik.io).
//
// apply.go writes each file atomically, but a bundle of N files plus the
// ledger row is not one transaction: a crash after the second rename and
// before MarkApplied leaves two files new, one old, and a ledger that says
// APPROVED. The journal row is committed BEFORE the first mutation (§1.1),
// carries every pre-image and read-set hash, and lets a restart either
// finish a proven-complete generation or restore every pre-image — never
// guess (§5).

// Journal states (§3). PREPARED/APPLYING/VERIFYING/REVERTING are in flight;
// APPLIED/PENDING_RESTART/REVERTED/FAILED are terminal (terminal_at set);
// DRIFT is OPEN but parked: it waits for an operator and is never pruned.
const (
	JournalStatePrepared       = "PREPARED"
	JournalStateApplying       = "APPLYING"
	JournalStateVerifying      = "VERIFYING"
	JournalStateApplied        = "APPLIED"
	JournalStatePendingRestart = "PENDING_RESTART"
	JournalStateReverting      = "REVERTING"
	JournalStateReverted       = "REVERTED"
	JournalStateDrift          = "DRIFT"
	JournalStateFailed         = "FAILED"
)

// JournalReadSetAbsent is the read_set value recording that a dependency was
// absent when the proposal was grounded and must still be absent at apply.
const JournalReadSetAbsent = "ABSENT"

// JournalSchemaVersion is the classifier/schema version stamped on every row
// this code writes (schema_version column). Bump when the JSON column
// shapes change so a reconciler can refuse a row it does not understand.
const JournalSchemaVersion = 1

// JournalMaxOps caps the file ops one journal row may carry — the same bound
// as the apply engine's scaffoldMaxOps (§2: "Both stores cap the row at
// ProposalMaxContentBytes per file and scaffoldMaxOps files").
const JournalMaxOps = 12

// ErrJournalTooLarge is returned by Prepare when a row exceeds JournalMaxOps
// ops or a pre-image exceeds ProposalMaxContentBytes.
const ErrJournalTooLarge RepositoryError = "config apply journal row too large"

// JournalOp is one file operation in the journal's op digest (§2 `ops`).
type JournalOp struct {
	Op            string `json:"op"`   // create | replace
	Path          string `json:"path"` // relative to deployment_root
	ContentSHA256 string `json:"content_sha256"`
}

// JournalPreImage is what one target looked like when PREPARED was committed
// (§2 `pre_images`). Existed=false records an expected absence — the trusted
// deletion boundary (§6) turns on it.
type JournalPreImage struct {
	Existed bool   `json:"existed"`
	SHA256  string `json:"sha256,omitempty"`
	Content string `json:"content,omitempty"`
}

// ConfigApplyJournal is one row of config_apply_journal (§2). The JSONB/TEXT
// columns are typed here and marshalled by the repositories.
type ConfigApplyJournal struct {
	ID         string
	ProposalID string
	// RequestID is the assistant request that produced the proposal, when any.
	RequestID string
	State     string
	// OpDigest is sha256 over the canonical ops list.
	OpDigest      string
	SchemaVersion int
	// DeploymentRoot is the realpath of the config dir the ops resolve under.
	DeploymentRoot   string
	AffectedProjects []string
	// WriterEpoch is the config-writer leader-lock epoch at PREPARE (§1.2).
	WriterEpoch int64
	Ops         []JournalOp
	PreImages   map[string]JournalPreImage
	// ReadSet maps every read dependency to its sha256 or JournalReadSetAbsent.
	ReadSet map[string]string
	// Progress lists the paths whose rename committed, in write order.
	Progress         []string
	GenerationBefore string
	GenerationAfter  string
	// Failure is the reason on FAILED/DRIFT (and the REVERTING trigger).
	Failure    string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	TerminalAt *time.Time
}

// JournalPatch carries the optional columns a Transition may set alongside
// state. Empty strings leave the column untouched; Terminal stamps
// terminal_at = now.
type JournalPatch struct {
	GenerationAfter string
	Failure         string
	Terminal        bool
}

// JournalLedgerCommit is the input to MarkAppliedWithLedger: the journal's
// terminal transition and the proposal-ledger write it must land with.
type JournalLedgerCommit struct {
	JournalID string
	// To is the journal's terminal state: JournalStateApplied, or
	// JournalStatePendingRestart when every changed key is restart-only (§3).
	To              string
	GenerationAfter string
	ProposalID      string
	AppliedBy       string
	// Snapshot is the ledger's pre_apply_snapshot, DERIVED from this row's
	// pre-images so the two cannot disagree (§2).
	Snapshot string
}

// ApplyJournalRepository is the backend-agnostic config_apply_journal store.
// Implemented by internal/persistence/{postgres,sqlite} and verified by
// repotest.RunApplyJournalSuite.
type ApplyJournalRepository interface {
	// Prepare inserts row in PREPARED, defaulting CreatedAt/UpdatedAt to now
	// and SchemaVersion to JournalSchemaVersion when unset. The unique partial
	// index on (proposal_id) WHERE terminal_at IS NULL makes a second open row
	// for the same proposal fail with ErrDuplicateKey; an oversized row is
	// ErrJournalTooLarge. Callers rely on the commit being durable before the
	// first temp file is written (§1.1) — the store's synchronous mode is the
	// other half of that contract (§8).
	Prepare(ctx context.Context, row *ConfigApplyJournal) error

	// Get fetches a row by id. Returns ErrNotFound if absent.
	Get(ctx context.Context, id string) (*ConfigApplyJournal, error)

	// OpenForProposal returns the open (terminal_at IS NULL) row for a
	// proposal, or ErrNotFound when none is open.
	OpenForProposal(ctx context.Context, proposalID string) (*ConfigApplyJournal, error)

	// ListOpen returns every open row, oldest first — the reconciler's input.
	ListOpen(ctx context.Context) ([]*ConfigApplyJournal, error)

	// Transition is a compare-and-set on state: the row must currently be in
	// from, else ErrInvalidTransition (ErrNotFound for an unknown id). patch
	// carries the optional columns of the §3 table; updated_at is always
	// stamped.
	Transition(ctx context.Context, id, from, to string, patch JournalPatch) error

	// AppendProgress appends path to progress after that file's rename and
	// parent-dir fsync (§3 "APPLYING (per file)").
	AppendProgress(ctx context.Context, id, path string) error

	// MarkAppliedWithLedger commits the journal's terminal state (To,
	// generation_after, terminal_at) AND control_plane_proposals → APPLIED in
	// ONE database transaction (§3 rows "VERIFYING → APPLIED" and "VERIFYING →
	// PENDING_RESTART"). The journal must be in VERIFYING
	// (ErrInvalidTransition otherwise) and the proposal APPROVED
	// (ErrProposalNotApproved otherwise, same rule as
	// ProposalRepository.MarkApplied); on either refusal nothing is written.
	MarkAppliedWithLedger(ctx context.Context, c JournalLedgerCommit) error
}

// ValidateConfigApplyJournal applies the §2 size caps and the state
// vocabulary before a row is written. Shared by both stores so the bound
// cannot diverge.
func ValidateConfigApplyJournal(row *ConfigApplyJournal) error {
	if row == nil {
		return fmt.Errorf("config apply journal: nil row")
	}
	if len(row.Ops) > JournalMaxOps {
		return ErrJournalTooLarge
	}
	for _, pre := range row.PreImages {
		if len(pre.Content) > ProposalMaxContentBytes {
			return ErrJournalTooLarge
		}
	}
	if row.ProposalID == "" || row.OpDigest == "" || row.DeploymentRoot == "" {
		return fmt.Errorf("config apply journal: proposal_id, op_digest and deployment_root are required")
	}
	return nil
}

// ConfigApplyJournalJSON is the JSON-column half of a row, encoded once by
// MarshalJournalColumns for both stores' INSERT and decoded by Apply after
// a scan. Every field is a non-empty JSON document ("[]"/"{}" when empty) so
// the NOT NULL columns never see a Go nil.
type ConfigApplyJournalJSON struct {
	AffectedProjects string
	Ops              string
	PreImages        string
	ReadSet          string
	Progress         string
}

// MarshalJournalColumns encodes the typed JSON columns of row.
func MarshalJournalColumns(row *ConfigApplyJournal) (ConfigApplyJournalJSON, error) {
	var out ConfigApplyJournalJSON
	var err error
	if out.AffectedProjects, err = marshalJournalList(row.AffectedProjects); err != nil {
		return out, err
	}
	ops := row.Ops
	if ops == nil {
		ops = []JournalOp{}
	}
	if out.Ops, err = marshalJournalField(ops); err != nil {
		return out, err
	}
	pre := row.PreImages
	if pre == nil {
		pre = map[string]JournalPreImage{}
	}
	if out.PreImages, err = marshalJournalField(pre); err != nil {
		return out, err
	}
	rs := row.ReadSet
	if rs == nil {
		rs = map[string]string{}
	}
	if out.ReadSet, err = marshalJournalField(rs); err != nil {
		return out, err
	}
	out.Progress, err = marshalJournalList(row.Progress)
	return out, err
}

func marshalJournalList(v []string) (string, error) {
	if v == nil {
		v = []string{}
	}
	return marshalJournalField(v)
}

func marshalJournalField(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("config apply journal: encode: %w", err)
	}
	return string(b), nil
}

// Apply decodes the JSON columns into row. An empty column decodes to the
// empty value rather than failing, so a row written by hand (or by a store
// default) still scans.
func (c ConfigApplyJournalJSON) Apply(row *ConfigApplyJournal) error {
	row.AffectedProjects = []string{}
	row.Ops = []JournalOp{}
	row.PreImages = map[string]JournalPreImage{}
	row.ReadSet = map[string]string{}
	row.Progress = []string{}
	for _, f := range []struct {
		raw  string
		into any
	}{
		{c.AffectedProjects, &row.AffectedProjects},
		{c.Ops, &row.Ops},
		{c.PreImages, &row.PreImages},
		{c.ReadSet, &row.ReadSet},
		{c.Progress, &row.Progress},
	} {
		if f.raw == "" {
			continue
		}
		if err := json.Unmarshal([]byte(f.raw), f.into); err != nil {
			return fmt.Errorf("config apply journal %s: decode: %w", row.ID, err)
		}
	}
	return nil
}
