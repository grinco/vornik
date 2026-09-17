package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// LinkCodeRepository implements persistence.LinkCodeRepository — the
// self-service channel-link codes of oidc-identity-permissions-design §5.2.
// The `link_codes` table shipped with the identity core and had no producer
// or consumer until Phase 4.
//
// This repository is the semantic source of truth; the SQLite port mirrors
// it statement for statement and the shared suite
// (repotest.RunLinkCodeSuite) runs against both.
type LinkCodeRepository struct {
	db DBTX
}

// NewLinkCodeRepository constructs the Postgres link-code repository.
func NewLinkCodeRepository(db DBTX) *LinkCodeRepository {
	return &LinkCodeRepository{db: db}
}

// CreateLinkCode implements persistence.LinkCodeRepository.
func (r *LinkCodeRepository) CreateLinkCode(ctx context.Context, lc *persistence.LinkCode) error {
	if lc == nil || lc.CodeHash == "" || lc.UserID == "" {
		return fmt.Errorf("postgres: link code requires a hash and a user")
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO link_codes (code_hash, user_id, created_at, expires_at)
		 VALUES ($1, $2, $3, $4)`,
		lc.CodeHash, lc.UserID, lc.CreatedAt, lc.ExpiresAt)
	if err != nil {
		return fmt.Errorf("postgres: create link code: %w", err)
	}
	return nil
}

// GetLinkCode implements persistence.LinkCodeRepository.
func (r *LinkCodeRepository) GetLinkCode(ctx context.Context, codeHash string) (*persistence.LinkCode, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT code_hash, user_id, created_at, expires_at, used_at, used_by_channel, used_by_external_id
		   FROM link_codes WHERE code_hash = $1`, codeHash)
	lc, err := scanLinkCode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get link code: %w", err)
	}
	return lc, nil
}

// ConsumeLinkCode implements persistence.LinkCodeRepository.
//
// One conditional UPDATE ... RETURNING, not a read-then-write: the
// `used_at IS NULL AND expires_at > now` predicate lives in the statement, so
// two racing redemptions cannot both observe the code as unused — one code
// binding two channel identities is an identity-takeover primitive, not a
// duplicate write. No rows returned means absent, already used, or expired,
// and all three answer ErrNotFound deliberately, so no caller can build the
// oracle §5.2 forbids.
func (r *LinkCodeRepository) ConsumeLinkCode(ctx context.Context, codeHash, channel, externalID string) (*persistence.LinkCode, error) {
	now := time.Now().UTC()
	row := r.db.QueryRowContext(ctx,
		`UPDATE link_codes
		    SET used_at = $1, used_by_channel = $2, used_by_external_id = $3
		  WHERE code_hash = $4 AND used_at IS NULL AND expires_at > $5
		  RETURNING code_hash, user_id, created_at, expires_at, used_at, used_by_channel, used_by_external_id`,
		now, channel, externalID, codeHash, now)
	lc, err := scanLinkCode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: consume link code: %w", err)
	}
	return lc, nil
}

// OutstandingLinkCodes implements persistence.LinkCodeRepository.
func (r *LinkCodeRepository) OutstandingLinkCodes(ctx context.Context, userID string) ([]*persistence.LinkCode, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT code_hash, user_id, created_at, expires_at, used_at, used_by_channel, used_by_external_id
		   FROM link_codes
		  WHERE user_id = $1 AND used_at IS NULL AND expires_at > $2
		  ORDER BY created_at`, userID, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("postgres: outstanding link codes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.LinkCode
	for rows.Next() {
		lc, err := scanLinkCode(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: outstanding link codes: %w", err)
		}
		out = append(out, lc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: outstanding link codes: %w", err)
	}
	return out, nil
}

func scanLinkCode(sc interface {
	Scan(dest ...interface{}) error
}) (*persistence.LinkCode, error) {
	var (
		lc        persistence.LinkCode
		usedAt    sql.NullTime
		usedChan  sql.NullString
		usedExtID sql.NullString
	)
	if err := sc.Scan(&lc.CodeHash, &lc.UserID, &lc.CreatedAt, &lc.ExpiresAt, &usedAt, &usedChan, &usedExtID); err != nil {
		return nil, err
	}
	if usedAt.Valid {
		t := usedAt.Time.UTC()
		lc.UsedAt = &t
	}
	lc.CreatedAt = lc.CreatedAt.UTC()
	lc.ExpiresAt = lc.ExpiresAt.UTC()
	lc.UsedByChannel = usedChan.String
	lc.UsedByExternalID = usedExtID.String
	return &lc, nil
}
