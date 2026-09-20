package slack

import (
	"testing"
	"time"
)

// The incident this file exists for (2026-09-15): the easeit-companion project
// had no slash_command set, so the daemon answered the default /vornik while
// the T-800 Slack app published /t800. Every `/t800 link <code>` passed
// signature verification, was rejected at the command comparison, and was
// answered 200 — which the access logger does not record. Five link codes were
// minted and expired unused over forty minutes while the daemon held both
// halves of the answer at the moment of failure and discarded them.

func TestUnmatchedCommands_RecordsLiteralAndCount(t *testing.T) {
	r := newUnmatchedCommands(4)
	t0 := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

	r.record("/t800", t0)
	r.record("/t800", t0.Add(3*time.Minute))
	r.record("/holly", t0.Add(5*time.Minute))

	got := r.snapshot()
	if len(got) != 2 {
		t.Fatalf("want 2 distinct literals, got %d: %+v", len(got), got)
	}
	// Sorted by count descending, so the operator reads the one that is
	// actually failing first rather than the alphabetically luckiest.
	if got[0].Command != "/t800" || got[0].Count != 2 {
		t.Fatalf("want /t800 x2 first, got %+v", got[0])
	}
	if !got[0].FirstSeen.Equal(t0) || !got[0].LastSeen.Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("window wrong: first=%v last=%v", got[0].FirstSeen, got[0].LastSeen)
	}
	if got[1].Command != "/holly" || got[1].Count != 1 {
		t.Fatalf("want /holly x1 second, got %+v", got[1])
	}
}

// The command literal is attacker-influenceable — only behind a valid
// signature, so this is hygiene rather than defence, but an unbounded map keyed
// on request content is a shape this repository does not ship.
func TestUnmatchedCommands_IsBounded(t *testing.T) {
	r := newUnmatchedCommands(3)
	now := time.Now()
	for _, cmd := range []string{"/a", "/b", "/c", "/d", "/e"} {
		r.record(cmd, now)
	}

	got := r.snapshot()
	if len(got) != 3 {
		t.Fatalf("cap not honoured: want 3 tracked literals, got %d", len(got))
	}
	if n := r.overflowed(); n != 2 {
		t.Fatalf("want 2 dropped literals reported, got %d", n)
	}
}

// A literal already tracked must keep counting after the cap is reached —
// otherwise a burst of junk freezes the count for the one command an operator
// actually needs to see.
func TestUnmatchedCommands_TrackedLiteralStillCountsAfterCap(t *testing.T) {
	r := newUnmatchedCommands(2)
	now := time.Now()
	r.record("/t800", now)
	r.record("/junk1", now)
	r.record("/junk2", now) // dropped: cap reached
	r.record("/t800", now.Add(time.Minute))

	for _, e := range r.snapshot() {
		if e.Command == "/t800" {
			if e.Count != 2 {
				t.Fatalf("tracked literal stopped counting after the cap: %+v", e)
			}
			return
		}
	}
	t.Fatal("/t800 is no longer tracked")
}

// An empty recorder must be distinguishable from one that was never wired:
// the doctor check reports nothing on empty, and "nothing" must mean "no
// mismatch was seen", not "nobody asked".
func TestUnmatchedCommands_EmptySnapshotIsEmptyNotNil(t *testing.T) {
	r := newUnmatchedCommands(4)
	if got := r.snapshot(); len(got) != 0 {
		t.Fatalf("want empty snapshot, got %+v", got)
	}
	if n := r.overflowed(); n != 0 {
		t.Fatalf("want 0 overflow on an empty recorder, got %d", n)
	}
}

// A rejected slash command must be RECORDED by the webhook path, not merely
// answered. This is the regression test for the incident itself: the daemon
// discarded the one fact that would have diagnosed it in seconds.
func TestHandleSlashCommandWebhook_RecordsTheRejectedLiteral(t *testing.T) {
	c := &Channel{
		cfg:               Config{SlashCommand: "/vornik"},
		unmatchedCommands: newUnmatchedCommands(unmatchedCommandsCap),
		clock:             time.Now,
	}

	c.noteUnmatchedSlashCommand("/t800")

	got := c.UnmatchedSlashCommands()
	if len(got) != 1 || got[0].Command != "/t800" || got[0].Count != 1 {
		t.Fatalf("the rejected literal was not recorded: %+v", got)
	}
}

// A channel with no recorder (constructed before this shipped, or in a test)
// must not panic on the record path.
func TestNoteUnmatchedSlashCommand_NilRecorderIsSafe(t *testing.T) {
	c := &Channel{cfg: Config{SlashCommand: "/vornik"}}
	c.noteUnmatchedSlashCommand("/t800")
	if got := c.UnmatchedSlashCommands(); len(got) != 0 {
		t.Fatalf("want no rows from a channel with no recorder, got %+v", got)
	}
}
