package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// ClassESlotRepository is the SQLite half of the class-E admission cap. It is
// the same contract as the Postgres one, deliberately: the whole reason the cap
// is a PRIMARY KEY rather than a transaction is that both drivers then enforce
// it the same way, with no isolation-level argument to get wrong twice.
type ClassESlotRepository struct{ db *sql.DB }

// NewClassESlotRepository constructs the repository.
func NewClassESlotRepository(db *sql.DB) *ClassESlotRepository {
	return &ClassESlotRepository{db: db}
}

// Reserve claims one slot, or reports that it is taken.
func (r *ClassESlotRepository) Reserve(ctx context.Context, credentialID, day string, slot int) error {
	if credentialID == "" || day == "" || slot < 1 {
		return fmt.Errorf("class-E slot reservation needs a credential, a day and a positive slot")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO class_e_slot_reservations (credential_id, day, slot)
		VALUES (?, ?, ?)`,
		credentialID, day, slot)
	// The same PRIMARY KEY collision Postgres reports as 23505. Matched on the
	// driver's message because that is how every other repository in this
	// package detects it; the contract the two must agree on is
	// persistence.ErrDuplicateKey, and they do.
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return persistence.ErrDuplicateKey
	}
	return err
}

var _ persistence.ClassESlotRepository = (*ClassESlotRepository)(nil)
