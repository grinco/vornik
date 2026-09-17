package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunOperatorIdentityLinkSuite pins the cross-channel link repository's
// contract on BOTH backends.
//
// It exists because the two drivers were found to disagree TWICE in one
// night, on a repository whose SQLite half had been a no-op stub for months:
// first on linked_at (Postgres stamps its own clock, SQLite honoured the
// caller's), then on ListForOperator's ordering (linked_at ASC vs
// channel_speaker_id). Each was a one-line fix and neither would have been
// found by either driver's own tests, because each was internally consistent.
//
// The ordering matters beyond tidiness: ListForOperator is what
// `vornikctl operator show-links` previews and what the merge path walks, so
// a preview whose order depends on the backend is a different answer to the
// same question.
func RunOperatorIdentityLinkSuite(t *testing.T, repo persistence.OperatorIdentityLinkRepository) {
	t.Run("miss_is_not_found", func(t *testing.T) {
		AssertMissRepo(t, "OperatorIdentityLinkRepository.Get", repo.Get)
	})
	t.Run("repository_stamps_linked_at", func(t *testing.T) { linkStampsLinkedAt(t, repo) })
	t.Run("repoint_keeps_the_original_linked_at", func(t *testing.T) { linkRepointKeepsLinkedAt(t, repo) })
	t.Run("list_is_ordered_by_linked_at", func(t *testing.T) { linkListOrdering(t, repo) })
	t.Run("deletes_are_idempotent", func(t *testing.T) { linkDeletes(t, repo) })
}

// linkStampsLinkedAt — the column records when the REPOSITORY observed the
// link, not what a caller claimed. A repository recording when something
// happened must not take the answer from the thing it is recording, and the
// two drivers disagreed about exactly this.
func linkStampsLinkedAt(t *testing.T, repo persistence.OperatorIdentityLinkRepository) {
	ctx := context.Background()
	speaker := uniqueID("telegram")
	before := time.Now().UTC().Add(-time.Minute)

	wantOK(t, "Upsert", repo.Upsert(ctx, &persistence.OperatorIdentityLink{
		ChannelSpeakerID: speaker, OperatorID: "account:" + uniqueID("u"),
		LinkedBy: "link-code", LinkedAt: time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC),
	}))
	got, err := repo.Get(ctx, speaker)
	wantOK(t, "Get", err)
	if got.LinkedAt.Before(before) {
		t.Errorf("linked_at = %v, want the repository's own clock — the caller's 1999 was honoured", got.LinkedAt)
	}
	if got.LinkedBy != "link-code" {
		t.Errorf("linked_by = %q, want link-code", got.LinkedBy)
	}
}

// linkRepointKeepsLinkedAt — the column records when the operator FIRST
// confirmed the relationship, not when a merge last rewrote the row.
func linkRepointKeepsLinkedAt(t *testing.T, repo persistence.OperatorIdentityLinkRepository) {
	ctx := context.Background()
	speaker := uniqueID("telegram")
	wantOK(t, "Upsert", repo.Upsert(ctx, &persistence.OperatorIdentityLink{
		ChannelSpeakerID: speaker, OperatorID: "account:first", LinkedBy: "link-code",
	}))
	first, err := repo.Get(ctx, speaker)
	wantOK(t, "Get", err)

	// The equality check below can only tell "kept the original" from
	// "overwrote with now" if the column can REPRESENT the gap between the
	// two writes. On a whole-second column with both writes in the same
	// second, an overwriting driver produces an equal value and this test
	// reports green against the behaviour it exists to outlaw — the same
	// vacuous shape a reviewer found in linkListOrdering, in the test next
	// door (review-20260915-ba13 F1, applied here by self-audit).
	//
	// So prove discriminability first, with a probe row written after a gap.
	time.Sleep(2 * time.Millisecond)
	probe := uniqueID("probe")
	wantOK(t, "Upsert(probe)", repo.Upsert(ctx, &persistence.OperatorIdentityLink{
		ChannelSpeakerID: probe, OperatorID: "account:probe", LinkedBy: "link-code",
	}))
	probeRow, err := repo.Get(ctx, probe)
	wantOK(t, "Get(probe)", err)
	if probeRow.LinkedAt.Equal(first.LinkedAt) {
		t.Fatalf("linked_at is identical for two writes 2ms apart (%v): this column's precision "+
			"cannot distinguish kept-the-original from overwrote-with-now, so the assertion "+
			"below proves nothing", first.LinkedAt)
	}
	wantOK(t, "cleanup probe", repo.Delete(ctx, probe))

	wantOK(t, "repoint", repo.Upsert(ctx, &persistence.OperatorIdentityLink{
		ChannelSpeakerID: speaker, OperatorID: "account:second", LinkedBy: "cli",
	}))
	after, err := repo.Get(ctx, speaker)
	wantOK(t, "Get after repoint", err)

	if after.OperatorID != "account:second" || after.LinkedBy != "cli" {
		t.Errorf("repoint did not move the row: %+v", after)
	}
	if !after.LinkedAt.Equal(first.LinkedAt) {
		t.Errorf("linked_at = %v after repoint, want the original %v", after.LinkedAt, first.LinkedAt)
	}
}

// linkListOrdering — the merge path walks this, and the operator show-links
// CLI prints it. An order that depends on the backend is a different answer to
// the same question.
//
// RESIDUAL, stated because the alternative is letting "proves ASC ordering"
// stand unqualified (review-20260915-6d99 F2). The repository always stamps
// linked_at itself — that is the contract two tests above pin — so through
// the public interface, timestamp order and insertion order ALWAYS coincide:
// there is no way to write a row whose linked_at is older than a row inserted
// before it. A driver sorting by rowid therefore passes this test exactly as
// a driver sorting by linked_at does.
//
// What this pins is that the two rows come back in WRITE order, which is what
// the merge path and the CLI actually need, and which the SQLite driver got
// wrong (it sorted by channel_speaker_id). Decoupling the two orderings would
// need a backdating seam that exists only to be tested, and a seam that lets
// a caller set linked_at is the very thing the stamping contract forbids.
func linkListOrdering(t *testing.T, repo persistence.OperatorIdentityLinkRepository) {
	ctx := context.Background()
	operator := "account:" + uniqueID("u")
	// Insert in an order that does NOT match the ids' lexical order, so a
	// driver sorting by channel_speaker_id returns a different sequence.
	for _, speaker := range []string{"zeta:" + uniqueID("z"), "alpha:" + uniqueID("a")} {
		wantOK(t, "Upsert", repo.Upsert(ctx, &persistence.OperatorIdentityLink{
			ChannelSpeakerID: speaker, OperatorID: operator, LinkedBy: "link-code",
		}))
		time.Sleep(2 * time.Millisecond) // distinct linked_at on both drivers
	}
	// A row for a DIFFERENT operator, to stress the WHERE filter. Without it
	// a driver returning every operator's rows would still see len == 2 and
	// pass (review-20260915-ba13 F5).
	wantOK(t, "Upsert(other operator)", repo.Upsert(ctx, &persistence.OperatorIdentityLink{
		ChannelSpeakerID: "other:" + uniqueID("o"), OperatorID: "account:" + uniqueID("u"),
		LinkedBy: "link-code",
	}))

	rows, err := repo.ListForOperator(ctx, operator)
	wantOK(t, "ListForOperator", err)
	if len(rows) != 2 {
		t.Fatalf("ListForOperator returned %d rows, want 2 — another operator's rows leaked in", len(rows))
	}
	for _, r := range rows {
		if r.OperatorID != operator {
			t.Errorf("ListForOperator returned a row for %q, want only %q", r.OperatorID, operator)
		}
	}

	// The ordering assertion is only meaningful if the two timestamps are
	// DISTINCT. A driver that truncates linked_at to whole seconds would tie
	// them, After() would be false whatever the tie-break, and this test
	// would pass against the very ordering it exists to outlaw — the vacuous
	// pass I suspected of this suite and a reviewer confirmed
	// (review-20260915-ba13 F1). Fail loudly when the setup cannot
	// discriminate, rather than reporting green.
	if rows[0].LinkedAt.Equal(rows[1].LinkedAt) {
		t.Fatalf("linked_at tied at %v: this column's precision cannot distinguish two rows "+
			"written 2ms apart, so the ordering assertion below proves nothing", rows[0].LinkedAt)
	}
	if rows[0].LinkedAt.After(rows[1].LinkedAt) {
		t.Errorf("rows are not ordered by linked_at ASC: %v then %v", rows[0].LinkedAt, rows[1].LinkedAt)
	}
}

// linkDeletes — a missing row is not an error; the interface says so and the
// merge path relies on it.
func linkDeletes(t *testing.T, repo persistence.OperatorIdentityLinkRepository) {
	ctx := context.Background()
	operator := "account:" + uniqueID("u")
	speaker := uniqueID("slack")
	wantOK(t, "Upsert", repo.Upsert(ctx, &persistence.OperatorIdentityLink{
		ChannelSpeakerID: speaker, OperatorID: operator, LinkedBy: "link-code",
	}))
	wantOK(t, "Delete", repo.Delete(ctx, speaker))
	wantOK(t, "Delete(absent)", repo.Delete(ctx, speaker))

	wantOK(t, "Upsert", repo.Upsert(ctx, &persistence.OperatorIdentityLink{
		ChannelSpeakerID: uniqueID("slack"), OperatorID: operator, LinkedBy: "link-code",
	}))
	wantOK(t, "DeleteAllForOperator", repo.DeleteAllForOperator(ctx, operator))
	rows, err := repo.ListForOperator(ctx, operator)
	wantOK(t, "ListForOperator", err)
	if len(rows) != 0 {
		t.Errorf("DeleteAllForOperator left %d rows", len(rows))
	}
	// Idempotent on an absent operator, like Delete above — only Delete's
	// idempotency was pinned (review-20260915-ba13 F5).
	wantOK(t, "DeleteAllForOperator(absent)", repo.DeleteAllForOperator(ctx, "account:"+uniqueID("gone")))
}
