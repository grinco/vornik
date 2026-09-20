package configassist

import (
	"context"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// SlotStore hands out class-E admission slots. The implementation is a table
// whose PRIMARY KEY (credential_id, day, slot) IS the cap: Reserve returns
// persistence.ErrDuplicateKey when the slot is already held, identically on
// both drivers.
type SlotStore interface {
	Reserve(ctx context.Context, credentialID, day string, slot int) error
}

// reserveClassESlot admits one class-E proposal, or refuses.
//
// WHY A RESERVATION AND NOT A WIDER TRANSACTION (design §13.9a). The count-then-
// insert alternative has to make "no one else is filing right now" true for the
// duration, which means a predicate lock over rows that do not exist yet —
// SERIALIZABLE plus a retry loop on Postgres, and a different argument on
// SQLite. A safety property whose proof differs per driver is the shape that has
// cost this repository a JSONB byte-exactness bug and a LIKE pattern that
// matched nothing on one driver for weeks. A UNIQUE constraint needs no
// isolation-level reasoning at all.
//
// THE RETRY IS A LINEAR PROBE, NOT A RE-COUNT. hint is the exhaustive same-day
// count §13.9 already computes, used only to skip slots that are obviously
// taken; if it is stale the INSERT collides and the next slot is tried. A
// re-count after each collision would cost a query to produce a hint that can be
// stale again by the time it is used. At most cap-hint+1 attempts, then the
// loud refusal §13.9 already defines — never queued, never retried on a timer.
//
// NO RELEASE PATH. A request that dies after reserving consumes its slot, so
// the failure mode is under-admission by at most the number of crashed
// requests — which under a crash loop is the whole day. That is the control
// firing rather than failing: a credential producing reservations faster than
// proposals is indistinguishable from the leaked-credential burst the cap
// exists to bound.
func (e *Engine) reserveClassESlot(ctx context.Context, credentialID string, dailyCap, hint int, now time.Time) *Refusal {
	if e == nil || e.Slots == nil {
		// The deployment predates the table. The pre-existing exhaustive count
		// still runs beside this, so admission is no weaker than it was — this
		// is the upgrade path, not a bypass.
		return nil
	}
	if dailyCap <= 0 {
		return nil
	}
	if credentialID == "" {
		// An unattributed caller cannot be counted, so it must not be admitted
		// under a per-credential quota either.
		return &Refusal{
			Code:    RefuseClassECap,
			Message: "a class-E proposal needs an actor credential to charge its daily slot to; refused",
		}
	}

	day := now.UTC().Format("2006-01-02")
	start := hint + 1
	if start < 1 {
		start = 1
	}
	for slot := start; slot <= dailyCap; slot++ {
		err := e.Slots.Reserve(ctx, credentialID, day, slot)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, persistence.ErrDuplicateKey):
			continue // taken; probe the next one
		default:
			// Cannot tell is not permission: the cap bounds a leaked
			// credential, so an unreadable store refuses.
			return &Refusal{
				Code:    RefuseClassECap,
				Message: "cannot reserve a class-E slot: " + err.Error(),
			}
		}
	}
	return &Refusal{
		Code: RefuseClassECap,
		Message: fmt.Sprintf(
			"class-E proposals are capped at %d per credential per day and every slot for %s is taken; "+
				"refused, not queued (a burst of class-E proposals is what a leaked credential looks like)",
			dailyCap, day),
	}
}
