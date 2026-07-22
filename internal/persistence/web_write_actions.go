package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// WebWriteAction is one row of the web_write_actions table (migration 132) — a
// supervised, human-approved single-page web form submission. It mirrors the
// LLD §Components.3 schema for the "what the site sends equals what the
// operator approved" primitive.
//
// The JSON-shaped columns are carried as raw []byte (JSONB) so the repo stays
// agnostic to the payload/selector/field-table encodings owned by the
// dispatcher + scraper tasks.
type WebWriteAction struct {
	SubmissionID string // primary key
	ProjectID    string
	TaskID       string
	AgentRunID   string
	TargetURL    string
	TargetHost   string

	PayloadJSON      []byte // proposed field name→value set (JSONB)
	SelectorBindings []byte // field→selector bindings (JSONB)
	FieldTableJSON   []byte // full enumerated field table incl. provenance (JSONB)
	VolatileFields   []byte // JSON array of volatile field names (JSONB)

	ScreenshotRef   string // preview screenshot artifact ref
	ConfirmationRef string // post-submit confirmation artifact ref

	Status            string // pending|approved|submitting|submitted|rejected|expired|failed|unknown
	ApprovalTokenHash string // hash of the whole-row approved set, set on Approve
	InsecureBypass    bool   // true when written under web.writes=insecure (allowlist bypassed)
	Approver          string // operator identity that approved/rejected

	CreatedAt       time.Time
	DecidedAt       sql.NullTime // approve/reject decision time
	SubmittedAt     sql.NullTime // terminal (submitted/failed/unknown) time
	TokenConsumedAt sql.NullTime // set atomically on the approved→submitting CAS
}

// ErrNoTransition is returned by the guarded CAS transitions (Approve,
// Finalize, Resolve, Reject) when zero rows matched the explicit source-state
// guard — i.e. the row was not in a state from which the transition is legal
// (or the submission_id does not exist). CASToSubmitting reports this as a
// (false, nil) loser instead of an error, since a lost double-submit race is
// an expected outcome there, not a fault.
var ErrNoTransition = errors.New("persistence: web_write_action not in a valid source state for transition")

// WebWriteRepo persists supervised web-write actions and their status
// transitions. Every mutating method is a single guarded compare-and-set: the
// legal source status is pinned in the WHERE clause so a transition can never
// fire from the wrong state, and concurrent callers cannot both win.
type WebWriteRepo interface {
	// Create inserts a new action row (defaults SubmissionID + CreatedAt +
	// Status='pending' when left zero).
	Create(ctx context.Context, a *WebWriteAction) error
	// Get loads a single action by submission_id.
	Get(ctx context.Context, submissionID string) (*WebWriteAction, error)
	// Approve is the pending→approved CAS: stamps the token hash, approver and
	// decided_at. Errors with ErrNoTransition if the row is not pending.
	Approve(ctx context.Context, submissionID, tokenHash, approver string) error
	// CASToSubmitting is the C3 double-submit guard: the approved→submitting
	// transition (also stamping token_consumed_at). Returns true iff exactly
	// one row changed — the single winner. A loser (0 rows) returns
	// (false, nil).
	CASToSubmitting(ctx context.Context, submissionID string) (bool, error)
	// Finalize is the submitting→(submitted|failed|unknown) CAS, stamping
	// submitted_at. Errors with ErrNoTransition if the row is not submitting.
	Finalize(ctx context.Context, submissionID, status string) error
	// Resolve is the operator-recovery (unknown|submitting)→(submitted|failed)
	// CAS, stamping submitted_at. Errors with ErrNoTransition if the row is
	// neither unknown nor submitting.
	Resolve(ctx context.Context, submissionID, status string) error
	// Reject is the (pending|approved)→rejected CAS, stamping approver and
	// decided_at. Errors with ErrNoTransition if the row is terminal already.
	Reject(ctx context.Context, submissionID, approver string) error
}

// webWriteRepo is the SQL-backed WebWriteRepo. It works against any *sql.DB
// (including the daemon's metrics-wrapped pool, which satisfies *sql.DB via the
// standard interface) using parameterized queries only.
type webWriteRepo struct {
	db *sql.DB
}

// NewWebWriteRepo constructs a WebWriteRepo over db.
func NewWebWriteRepo(db *sql.DB) WebWriteRepo {
	return &webWriteRepo{db: db}
}

// Create inserts a pending action. SubmissionID and CreatedAt default when
// zero; Status defaults to 'pending'.
func (r *webWriteRepo) Create(ctx context.Context, a *WebWriteAction) error {
	if a.SubmissionID == "" {
		a.SubmissionID = GenerateID("webwrite")
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	if a.Status == "" {
		a.Status = "pending"
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO web_write_actions (
			submission_id, project_id, task_id, agent_run_id, target_url, target_host,
			payload_json, selector_bindings, field_table_json, volatile_fields,
			screenshot_ref, confirmation_ref, status, approval_token_hash,
			insecure_bypass, approver, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		a.SubmissionID, a.ProjectID, nullString(a.TaskID), nullString(a.AgentRunID),
		a.TargetURL, a.TargetHost,
		a.PayloadJSON, a.SelectorBindings, a.FieldTableJSON, a.VolatileFields,
		nullString(a.ScreenshotRef), nullString(a.ConfirmationRef), a.Status,
		nullString(a.ApprovalTokenHash), a.InsecureBypass, nullString(a.Approver),
		a.CreatedAt,
	)
	return err
}

// Get loads a single action. Returns sql.ErrNoRows when absent.
func (r *webWriteRepo) Get(ctx context.Context, submissionID string) (*WebWriteAction, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT submission_id, project_id, task_id, agent_run_id, target_url, target_host,
		       payload_json, selector_bindings, field_table_json, volatile_fields,
		       screenshot_ref, confirmation_ref, status, approval_token_hash,
		       insecure_bypass, approver, created_at, decided_at, submitted_at, token_consumed_at
		FROM web_write_actions
		WHERE submission_id = $1`, submissionID)

	var (
		a               WebWriteAction
		taskID          sql.NullString
		agentRunID      sql.NullString
		screenshotRef   sql.NullString
		confirmationRef sql.NullString
		tokenHash       sql.NullString
		approver        sql.NullString
	)
	if err := row.Scan(
		&a.SubmissionID, &a.ProjectID, &taskID, &agentRunID, &a.TargetURL, &a.TargetHost,
		&a.PayloadJSON, &a.SelectorBindings, &a.FieldTableJSON, &a.VolatileFields,
		&screenshotRef, &confirmationRef, &a.Status, &tokenHash,
		&a.InsecureBypass, &approver, &a.CreatedAt, &a.DecidedAt, &a.SubmittedAt, &a.TokenConsumedAt,
	); err != nil {
		return nil, err
	}
	a.TaskID = taskID.String
	a.AgentRunID = agentRunID.String
	a.ScreenshotRef = screenshotRef.String
	a.ConfirmationRef = confirmationRef.String
	a.ApprovalTokenHash = tokenHash.String
	a.Approver = approver.String
	return &a, nil
}

// Approve performs the pending→approved CAS.
func (r *webWriteRepo) Approve(ctx context.Context, submissionID, tokenHash, approver string) error {
	res, err := r.db.ExecContext(ctx,
		"UPDATE web_write_actions SET status='approved', approval_token_hash=$2, approver=$3, decided_at=NOW() WHERE submission_id=$1 AND status='pending'",
		submissionID, tokenHash, approver)
	return casErr(res, err)
}

// CASToSubmitting performs the approved→submitting single-winner CAS. This is
// the C3 double-submit guard: exactly one concurrent caller can win.
func (r *webWriteRepo) CASToSubmitting(ctx context.Context, submissionID string) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		"UPDATE web_write_actions SET status='submitting', token_consumed_at=NOW() WHERE submission_id=$1 AND status='approved'",
		submissionID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Finalize performs the submitting→(submitted|failed|unknown) CAS.
func (r *webWriteRepo) Finalize(ctx context.Context, submissionID, status string) error {
	switch status {
	case "submitted", "failed", "unknown":
	default:
		return fmt.Errorf("persistence: Finalize invalid target status %q (want submitted|failed|unknown)", status)
	}
	res, err := r.db.ExecContext(ctx,
		"UPDATE web_write_actions SET status=$2, submitted_at=NOW() WHERE submission_id=$1 AND status='submitting'",
		submissionID, status)
	return casErr(res, err)
}

// Resolve performs the operator-recovery (unknown|submitting)→(submitted|failed) CAS.
func (r *webWriteRepo) Resolve(ctx context.Context, submissionID, status string) error {
	switch status {
	case "submitted", "failed":
	default:
		return fmt.Errorf("persistence: Resolve invalid target status %q (want submitted|failed)", status)
	}
	res, err := r.db.ExecContext(ctx,
		"UPDATE web_write_actions SET status=$2, submitted_at=NOW() WHERE submission_id=$1 AND status IN ('unknown','submitting')",
		submissionID, status)
	return casErr(res, err)
}

// Reject performs the (pending|approved)→rejected CAS.
func (r *webWriteRepo) Reject(ctx context.Context, submissionID, approver string) error {
	res, err := r.db.ExecContext(ctx,
		"UPDATE web_write_actions SET status='rejected', approver=$2, decided_at=NOW() WHERE submission_id=$1 AND status IN ('pending','approved')",
		submissionID, approver)
	return casErr(res, err)
}

// casErr maps a guarded UPDATE result to an error: propagates the driver error,
// else returns ErrNoTransition when the source-state guard matched no row.
func casErr(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNoTransition
	}
	return nil
}

// nullString renders "" as SQL NULL so nullable text columns stay NULL rather
// than empty-string.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
