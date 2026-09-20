package postgres

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// ClassESlotRepository is the Postgres half of the class-E admission cap.
// See persistence.ClassESlotRepository for why the constraint, not a count,
// decides.
type ClassESlotRepository struct{ db DBTX }

// NewClassESlotRepository constructs the repository.
func NewClassESlotRepository(db DBTX) *ClassESlotRepository {
	return &ClassESlotRepository{db: db}
}

// Reserve claims one slot, or reports that it is taken.
//
// No ON CONFLICT DO NOTHING: that would turn a taken slot into a successful
// no-op, and the caller could not tell admission from refusal without a second
// query — which is the race this design exists to remove.
func (r *ClassESlotRepository) Reserve(ctx context.Context, credentialID, day string, slot int) error {
	if credentialID == "" || day == "" || slot < 1 {
		return fmt.Errorf("class-E slot reservation needs a credential, a day and a positive slot")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO class_e_slot_reservations (credential_id, day, slot)
		VALUES ($1, $2, $3)`,
		credentialID, day, slot)
	return mapDBError(err)
}

var _ persistence.ClassESlotRepository = (*ClassESlotRepository)(nil)
