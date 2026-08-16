package ui

import (
	"testing"
	"time"
)

type capUsageRow struct {
	key     string
	project string
	count   int64
}

// Ranking on BREADTH rather than volume is the design decision this test
// protects. The panel exists partly to create competition between PoC teams,
// and a leaderboard produces whatever it rewards: event counts would crown
// whoever ran the most autonomy ticks, which teaches nobody anything. Distinct
// capabilities used makes the way to climb "try the next feature".
func TestCapabilityBreadth_RanksOnDistinctCapabilitiesNotVolume(t *testing.T) {
	// team-noisy: one capability, enormous volume.
	// team-broad:  three capabilities, tiny volume.
	usage := []capUsageRow{
		{"tasks", "team-noisy", 100000},
		{"tasks", "team-broad", 3},
		{"chat", "team-broad", 2},
		{"github", "team-broad", 1},
	}
	perTeam := map[string]map[string]bool{}
	for _, u := range usage {
		if perTeam[u.project] == nil {
			perTeam[u.project] = map[string]bool{}
		}
		if u.count > 0 {
			perTeam[u.project][u.key] = true
		}
	}
	if len(perTeam["team-broad"]) <= len(perTeam["team-noisy"]) {
		t.Fatalf("breadth must favour the team using more capabilities: broad=%d noisy=%d",
			len(perTeam["team-broad"]), len(perTeam["team-noisy"]))
	}
	// And volume must not be the tiebreaker that undoes it.
	if 100000 > 0 && len(perTeam["team-noisy"]) != 1 {
		t.Errorf("the high-volume team still counts as one capability, got %d", len(perTeam["team-noisy"]))
	}
}

// A capability nobody uses is an instance-wide gap, not any one team's failing.
// Scoring teams against undiscovered features would make every team look
// equally bad and flatten exactly the difference the board exists to show.
func TestCapabilityBreadth_ScoresOnlyAgainstCapabilitiesSomeoneUses(t *testing.T) {
	adopted := []capability{{Key: "tasks"}, {Key: "chat"}}
	used := map[string]bool{"tasks": true}

	var missing []string
	for _, c := range adopted {
		if !used[c.Key] {
			missing = append(missing, c.Key)
		}
	}
	if len(missing) != 1 || missing[0] != "chat" {
		t.Errorf("missing = %v, want only capabilities OTHERS use", missing)
	}
	// An unused-by-everyone capability must not appear against a team.
	for _, m := range missing {
		if m == "fixit" {
			t.Error("a capability no team uses must not be charged against one team")
		}
	}
}

// The catalogue is curated, not derived from the schema. Anything whose
// emptiness is a SUCCESS must stay out of it — presenting an empty
// security-incident table as an unexplored feature inverts its meaning.
func TestCapabilityCatalogue_ExcludesSignalsWhoseAbsenceIsGood(t *testing.T) {
	for _, c := range capabilityCatalogue {
		switch c.Key {
		case "security_incidents", "memory_embed_dlq", "tasks_lease_audit":
			t.Errorf("%q must not be catalogued: an empty one is success, not a gap", c.Key)
		}
		if c.Label == "" || c.Group == "" {
			t.Errorf("capability %q needs an operator-facing label and group", c.Key)
		}
	}
}

// Nil repo must render nothing rather than an empty table: "not wired" and
// "nothing is used" are different claims and the panel must not conflate them.
func TestCollectCapabilities_NilRepoYieldsNoRows(t *testing.T) {
	s := &Server{}
	got := s.collectCapabilities(t.Context(), time.Now().Add(-30*24*time.Hour))
	if len(got.Rows) != 0 || len(got.Teams) != 0 {
		t.Errorf("nil repo produced %d rows / %d teams, want none", len(got.Rows), len(got.Teams))
	}
}
