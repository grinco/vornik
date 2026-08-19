package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunChatMemoryWriteConfirmationSuite exercises the shared-scope confirmation state store and
// its append-only audit companion (chat memory-write design §5.3, slice 3 part 2) against
// whichever backend the caller wires. Run from BOTH sqlite and postgres so a dialect
// divergence (TIMESTAMPTZ vs RFC3339 TEXT, ON CONFLICT DO UPDATE, NULL acknowledged_at) can't
// reach production — `go test ./...` is sqlite-only.
//
// The safety properties under test are the ones the design leans on:
//   - the (channel, session_id) primary key makes a second Propose REPLACE the first and
//     clear its acknowledgement, so an acknowledgement can only discharge the most recent
//     proposal;
//   - Acknowledge stamps ONLY when operator_id matches (Bob cannot discharge Alice's proposal);
//   - the audit row is append-only and stores a fingerprint, never content.
func RunChatMemoryWriteConfirmationSuite(
	t *testing.T,
	confirms persistence.ChatMemoryWriteConfirmationRepository,
	audit persistence.ChatMemoryWriteAuditRepository,
) {
	t.Helper()
	t.Run("Get_miss_obeys_the_contract", func(t *testing.T) {
		AssertMiss(t, "ChatMemoryWriteConfirmationRepository.Get", func() (*persistence.ChatMemoryWriteConfirmation, error) {
			return confirms.Get(context.Background(), uniqueID("absent-chan"), uniqueID("absent-session"))
		})
	})
	runConfirmProposeGet(t, confirms)
	runConfirmAcknowledge(t, confirms)
	runConfirmReplace(t, confirms)
	runConfirmDeleteAndSweep(t, confirms)
	runConfirmScoping(t, confirms)
	runConfirmAudit(t, audit)
}

// timesCloseToSecond compares timestamps to the second: Postgres TIMESTAMPTZ truncates to
// microseconds while SQLite keeps RFC3339Nano, so a sub-second exact compare would spuriously
// diverge across backends. Second granularity is well within what any of these rows needs.
func timesCloseToSecond(a, b time.Time) bool {
	return a.UTC().Truncate(time.Second).Equal(b.UTC().Truncate(time.Second))
}

func runConfirmProposeGet(t *testing.T, confirms persistence.ChatMemoryWriteConfirmationRepository) {
	ctx := context.Background()

	t.Run("Get returns ErrNotFound for an unknown conversation", func(t *testing.T) {
		got, err := confirms.Get(ctx, "slack", "never-proposed")
		if !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("Get: want ErrNotFound, got %v", err)
		}
		if got != nil {
			t.Errorf("an un-proposed conversation must return nil, got %+v", got)
		}
	})

	t.Run("Propose then Get round-trips, acknowledged_at NULL", func(t *testing.T) {
		proposed := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		rec := &persistence.ChatMemoryWriteConfirmation{
			Channel:            "slack",
			SessionID:          "rt-1",
			ContentFingerprint: "fp-abc",
			Scope:              "shared",
			OperatorID:         "slack:UALICE",
			ProposedAt:         proposed,
			ExpiresAt:          proposed.Add(15 * time.Minute),
		}
		if err := confirms.Propose(ctx, rec); err != nil {
			t.Fatalf("Propose: %v", err)
		}
		got, err := confirms.Get(ctx, "slack", "rt-1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got == nil {
			t.Fatal("Propose then Get returned nil")
		}
		if got.ContentFingerprint != "fp-abc" || got.Scope != "shared" || got.OperatorID != "slack:UALICE" {
			t.Errorf("round-trip mismatch: %+v", got)
		}
		if !timesCloseToSecond(got.ProposedAt, proposed) || !timesCloseToSecond(got.ExpiresAt, proposed.Add(15*time.Minute)) {
			t.Errorf("timestamps did not round-trip: proposed=%v expires=%v", got.ProposedAt, got.ExpiresAt)
		}
		if got.Acknowledged() {
			t.Error("a freshly proposed row must have acknowledged_at NULL")
		}
	})
}

func runConfirmAcknowledge(t *testing.T, confirms persistence.ChatMemoryWriteConfirmationRepository) {
	ctx := context.Background()

	t.Run("Acknowledge stamps only on operator match", func(t *testing.T) {
		proposed := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		if err := confirms.Propose(ctx, &persistence.ChatMemoryWriteConfirmation{
			Channel: "slack", SessionID: "ack-1", ContentFingerprint: "fp-1", Scope: "shared",
			OperatorID: "slack:UALICE", ProposedAt: proposed, ExpiresAt: proposed.Add(15 * time.Minute),
		}); err != nil {
			t.Fatalf("Propose: %v", err)
		}

		// Bob cannot discharge Alice's proposal.
		at := proposed.Add(2 * time.Minute)
		stamped, err := confirms.Acknowledge(ctx, "slack", "ack-1", "slack:UBOB", at)
		if err != nil {
			t.Fatalf("Acknowledge(bob): %v", err)
		}
		if stamped {
			t.Error("a different operator must not be able to acknowledge")
		}
		if got, _ := confirms.Get(ctx, "slack", "ack-1"); got == nil || got.Acknowledged() {
			t.Error("row must still be unacknowledged after a wrong-operator attempt")
		}

		// Alice can.
		stamped, err = confirms.Acknowledge(ctx, "slack", "ack-1", "slack:UALICE", at)
		if err != nil {
			t.Fatalf("Acknowledge(alice): %v", err)
		}
		if !stamped {
			t.Error("the proposing operator must be able to acknowledge")
		}
		got, err := confirms.Get(ctx, "slack", "ack-1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got == nil || !got.Acknowledged() {
			t.Fatal("row must be acknowledged after the right operator acknowledges")
		}
		if !timesCloseToSecond(*got.AcknowledgedAt, at) {
			t.Errorf("acknowledged_at = %v, want ~%v", *got.AcknowledgedAt, at)
		}
	})

	t.Run("Acknowledge on an absent conversation stamps nothing", func(t *testing.T) {
		stamped, err := confirms.Acknowledge(ctx, "slack", "ack-absent", "slack:UALICE", time.Now())
		if err != nil {
			t.Fatalf("Acknowledge: %v", err)
		}
		if stamped {
			t.Error("acknowledging a conversation with no pending row must report false")
		}
	})
}

// The PK is the safety property: a second Propose REPLACES the first AND clears any
// acknowledgement, so a superseded proposal is unacknowledgeable rather than dormant.
func runConfirmReplace(t *testing.T, confirms persistence.ChatMemoryWriteConfirmationRepository) {
	ctx := context.Background()

	t.Run("Propose replaces the pending row and clears the acknowledgement", func(t *testing.T) {
		proposed := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		if err := confirms.Propose(ctx, &persistence.ChatMemoryWriteConfirmation{
			Channel: "slack", SessionID: "replace-1", ContentFingerprint: "fp-old", Scope: "shared",
			OperatorID: "slack:UALICE", ProposedAt: proposed, ExpiresAt: proposed.Add(15 * time.Minute),
		}); err != nil {
			t.Fatalf("first Propose: %v", err)
		}
		if _, err := confirms.Acknowledge(ctx, "slack", "replace-1", "slack:UALICE", proposed.Add(time.Minute)); err != nil {
			t.Fatalf("Acknowledge: %v", err)
		}

		// Supersede it.
		if err := confirms.Propose(ctx, &persistence.ChatMemoryWriteConfirmation{
			Channel: "slack", SessionID: "replace-1", ContentFingerprint: "fp-new", Scope: "shared",
			OperatorID: "slack:UALICE", ProposedAt: proposed.Add(5 * time.Minute), ExpiresAt: proposed.Add(20 * time.Minute),
		}); err != nil {
			t.Fatalf("second Propose: %v", err)
		}
		got, err := confirms.Get(ctx, "slack", "replace-1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ContentFingerprint != "fp-new" {
			t.Errorf("second proposal did not replace the first: fingerprint=%q", got.ContentFingerprint)
		}
		if got.Acknowledged() {
			t.Error("a superseding proposal must clear the previous acknowledgement")
		}
	})
}

func runConfirmDeleteAndSweep(t *testing.T, confirms persistence.ChatMemoryWriteConfirmationRepository) {
	ctx := context.Background()

	t.Run("Delete removes the pending row", func(t *testing.T) {
		proposed := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		if err := confirms.Propose(ctx, &persistence.ChatMemoryWriteConfirmation{
			Channel: "slack", SessionID: "del-1", ContentFingerprint: "fp", Scope: "shared",
			OperatorID: "slack:UALICE", ProposedAt: proposed, ExpiresAt: proposed.Add(15 * time.Minute),
		}); err != nil {
			t.Fatalf("Propose: %v", err)
		}
		if err := confirms.Delete(ctx, "slack", "del-1"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		got, err := confirms.Get(ctx, "slack", "del-1")
		if !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("Get after Delete: want ErrNotFound, got %v", err)
		}
		if got != nil {
			t.Error("Delete must remove the pending row")
		}
	})

	t.Run("DeleteExpired sweeps past-expiry rows and leaves live ones", func(t *testing.T) {
		base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		if err := confirms.Propose(ctx, &persistence.ChatMemoryWriteConfirmation{
			Channel: "sweep", SessionID: "expired", ContentFingerprint: "fp", Scope: "shared",
			OperatorID: "slack:UALICE", ProposedAt: base.Add(-30 * time.Minute), ExpiresAt: base.Add(-15 * time.Minute),
		}); err != nil {
			t.Fatalf("Propose expired: %v", err)
		}
		if err := confirms.Propose(ctx, &persistence.ChatMemoryWriteConfirmation{
			Channel: "sweep", SessionID: "live", ContentFingerprint: "fp", Scope: "shared",
			OperatorID: "slack:UALICE", ProposedAt: base, ExpiresAt: base.Add(15 * time.Minute),
		}); err != nil {
			t.Fatalf("Propose live: %v", err)
		}

		n, err := confirms.DeleteExpired(ctx, base)
		if err != nil {
			t.Fatalf("DeleteExpired: %v", err)
		}
		if n < 1 {
			t.Errorf("DeleteExpired removed %d rows, want at least the one expired row", n)
		}
		if got, _ := confirms.Get(ctx, "sweep", "expired"); got != nil {
			t.Error("the expired row must be swept")
		}
		if got, _ := confirms.Get(ctx, "sweep", "live"); got == nil {
			t.Error("the live row must survive the sweep")
		}
	})
}

func runConfirmScoping(t *testing.T, confirms persistence.ChatMemoryWriteConfirmationRepository) {
	ctx := context.Background()

	t.Run("confirmation is scoped by (channel, session)", func(t *testing.T) {
		proposed := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		if err := confirms.Propose(ctx, &persistence.ChatMemoryWriteConfirmation{
			Channel: "slack", SessionID: "shared-id", ContentFingerprint: "fp", Scope: "shared",
			OperatorID: "slack:UALICE", ProposedAt: proposed, ExpiresAt: proposed.Add(15 * time.Minute),
		}); err != nil {
			t.Fatalf("Propose: %v", err)
		}
		got, err := confirms.Get(ctx, "telegram", "shared-id")
		if !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("Get on a different channel: want ErrNotFound, got %v", err)
		}
		if got != nil {
			t.Error("the same session id on a different channel must be independent")
		}
	})
}

func runConfirmAudit(t *testing.T, audit persistence.ChatMemoryWriteAuditRepository) {
	ctx := context.Background()

	t.Run("audit is append-only and queryable by fingerprint", func(t *testing.T) {
		proposed := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		acked := proposed.Add(2 * time.Minute)
		granted := proposed.Add(3 * time.Minute)
		rec := &persistence.ChatMemoryWriteAudit{
			Channel: "slack", SessionID: "audit-1", ContentFingerprint: "fp-audit",
			Scope: "shared", OperatorID: "slack:UALICE",
			ProposedAt: proposed, AcknowledgedAt: acked, GrantedAt: granted,
		}
		if err := audit.Record(ctx, rec); err != nil {
			t.Fatalf("Record: %v", err)
		}
		// Append-only: a second Record for the same fingerprint adds a row rather than replacing.
		if err := audit.Record(ctx, &persistence.ChatMemoryWriteAudit{
			Channel: "slack", SessionID: "audit-1", ContentFingerprint: "fp-audit",
			Scope: "shared", OperatorID: "slack:UALICE",
			ProposedAt: proposed, AcknowledgedAt: acked, GrantedAt: granted.Add(time.Minute),
		}); err != nil {
			t.Fatalf("second Record: %v", err)
		}

		rows, err := audit.ListByFingerprint(ctx, "fp-audit")
		if err != nil {
			t.Fatalf("ListByFingerprint: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("append-only audit produced %d rows for the fingerprint, want 2", len(rows))
		}
		first := rows[0]
		if first.Channel != "slack" || first.SessionID != "audit-1" || first.OperatorID != "slack:UALICE" {
			t.Errorf("audit row round-trip mismatch: %+v", first)
		}
		if !timesCloseToSecond(first.ProposedAt, proposed) || !timesCloseToSecond(first.AcknowledgedAt, acked) || !timesCloseToSecond(first.GrantedAt, granted) {
			t.Errorf("audit timestamps did not round-trip: %+v", first)
		}
	})
}
