package persistence

import "context"

// ClassESlotRepository hands out the class-E daily admission slots the config
// assistant's cap is made of (config-assistant design §13.9a).
//
// THE CONSTRAINT IS THE CAP. Reserve INSERTs (credential_id, day, slot) into a
// table whose PRIMARY KEY is exactly that triple, so the Nth slot is handed out
// by a successful INSERT and a taken slot comes back as ErrDuplicateKey —
// identically on both drivers, with no isolation-level reasoning. The
// alternative it replaced, counting and inserting inside one transaction, needs
// SERIALIZABLE plus a retry loop on Postgres and a different argument on
// SQLite; a safety property whose proof differs per driver is the shape that
// has cost this repository a JSONB byte-exactness bug and a LIKE pattern that
// matched nothing on one driver for weeks.
//
// There is deliberately NO release. An unused reservation is under-admission,
// which costs an operator one refused proposal, against over-admission, which
// is the thing the cap exists to prevent.
type ClassESlotRepository interface {
	// Reserve claims one slot. Returns nil when the slot is now held by this
	// caller, ErrDuplicateKey when someone else holds it, and any other error
	// when the store could not answer — which callers must treat as a refusal,
	// because the cap bounds a leaked credential and "cannot tell" is not
	// permission.
	//
	// day is a UTC calendar day formatted YYYY-MM-DD. It is text rather than a
	// date so both drivers compare the same bytes and no timezone can enter
	// through a driver.
	Reserve(ctx context.Context, credentialID, day string, slot int) error
}
