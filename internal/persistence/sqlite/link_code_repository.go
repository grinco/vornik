package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// LinkCodeRepository implements persistence.LinkCodeRepository on SQLite —
// the self-service channel-link codes of oidc-identity-permissions-design
// §5.2. The `link_codes` table shipped with the identity core and had no
// producer or consumer until Phase 4.
//
// The Postgres repository stays the semantic source of truth; the shared
// suite (repotest.RunLinkCodeSuite) runs against both, so a divergence in
// the single-use rule is a test failure rather than a Community-only
// identity-takeover primitive.
type LinkCodeRepository struct {
	db *sql.DB
}

// NewLinkCodeRepository constructs the SQLite link-code repository.
func NewLinkCodeRepository(db *sql.DB) *LinkCodeRepository {
	return &LinkCodeRepository{db: db}
}

// CreateLinkCode implements persistence.LinkCodeRepository.
func (r *LinkCodeRepository) CreateLinkCode(ctx context.Context, lc *persistence.LinkCode) error {
	if lc == nil || lc.CodeHash == "" || lc.UserID == "" {
		return fmt.Errorf("sqlite: link code requires a hash and a user")
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO link_codes (code_hash, user_id, created_at, expires_at)
		 VALUES (?, ?, ?, ?)`,
		lc.CodeHash, lc.UserID, sqliteTime(lc.CreatedAt), sqliteTime(lc.ExpiresAt))
	if err != nil {
		return fmt.Errorf("sqlite: create link code: %w", err)
	}
	return nil
}

// GetLinkCode implements persistence.LinkCodeRepository.
func (r *LinkCodeRepository) GetLinkCode(ctx context.Context, codeHash string) (*persistence.LinkCode, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT code_hash, user_id, created_at, expires_at, used_at, used_by_channel, used_by_external_id
		   FROM link_codes WHERE code_hash = ?`, codeHash)
	lc, err := scanLinkCode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get link code: %w", err)
	}
	return lc, nil
}

// ConsumeLinkCode implements persistence.LinkCodeRepository.
//
// The claim is a single conditional UPDATE, not a read-then-write: the
// `used_at IS NULL AND expires_at > now` predicate lives in the statement so
// two racing redemptions cannot both observe the code as unused. A zero
// rows-affected result means absent, already used, or expired — all three
// answer ErrNotFound, deliberately, so the caller cannot build the oracle
// §5.2 forbids.
func (r *LinkCodeRepository) ConsumeLinkCode(ctx context.Context, codeHash, channel, externalID string) (*persistence.LinkCode, error) {
	now := time.Now().UTC()
	res, err := r.db.ExecContext(ctx,
		`UPDATE link_codes
		    SET used_at = ?, used_by_channel = ?, used_by_external_id = ?
		  WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?`,
		sqliteTime(now), channel, externalID, codeHash, sqliteTime(now))
	if err != nil {
		return nil, fmt.Errorf("sqlite: consume link code: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("sqlite: consume link code: %w", err)
	}
	if n == 0 {
		return nil, persistence.ErrNotFound
	}
	return r.GetLinkCode(ctx, codeHash)
}

// OutstandingLinkCodes implements persistence.LinkCodeRepository.
func (r *LinkCodeRepository) OutstandingLinkCodes(ctx context.Context, userID string) ([]*persistence.LinkCode, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT code_hash, user_id, created_at, expires_at, used_at, used_by_channel, used_by_external_id
		   FROM link_codes
		  WHERE user_id = ? AND used_at IS NULL AND expires_at > ?
		  ORDER BY created_at`, userID, sqliteTime(time.Now().UTC()))
	if err != nil {
		return nil, fmt.Errorf("sqlite: outstanding link codes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.LinkCode
	for rows.Next() {
		lc, err := scanLinkCode(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: outstanding link codes: %w", err)
		}
		out = append(out, lc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: outstanding link codes: %w", err)
	}
	return out, nil
}

// scanLinkCode reads one row. The inline scanner interface is the shape both
// *sql.Row and *sql.Rows satisfy, matching the convention already used by
// scanCanary and the skill repository rather than introducing a second one.
func scanLinkCode(sc interface {
	Scan(dest ...interface{}) error
}) (*persistence.LinkCode, error) {
	var (
		lc        persistence.LinkCode
		created   sqlTime
		expires   sqlTime
		usedAt    sqlNullTime
		usedChan  sql.NullString
		usedExtID sql.NullString
	)
	if err := sc.Scan(&lc.CodeHash, &lc.UserID, &created, &expires, &usedAt, &usedChan, &usedExtID); err != nil {
		return nil, err
	}
	lc.CreatedAt = created.Time
	lc.ExpiresAt = expires.Time
	if usedAt.Valid {
		t := usedAt.Time
		lc.UsedAt = &t
	}
	lc.UsedByChannel = usedChan.String
	lc.UsedByExternalID = usedExtID.String
	return &lc, nil
}
