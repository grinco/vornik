package ui

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Customer report, 2026-09-18: a company evaluating Vornik saw no new keys on
// the adoption board despite a large audience, and the last-accessed date never
// moved.
//
// Reproduced on the reference deployment. The board's key rows were built ONLY
// from three activity ledgers — memory_retrieval_audit, memory_ingest_audit and
// task_llm_usage — so a credential used for REST, the UI, chat or A2A wrote to
// none of them and was absent however heavily it was used. `slava/companion`
// had authenticated that same day and showed 0/0/0 across all three. The
// recency column came from the same ledgers, so it stalled for the identical
// reason: one cause, both symptoms.
//
// Absence reads as "nobody is using it". §5 of the adoption design exists to
// stop a thin sample being presented as a confident ranking; this is that rule
// applied to the ROW SET rather than the column — a board that silently omits
// credentials it has no ledger rows for reports "not used" and means "not
// measured".

func keyRepoWith(keys ...*persistence.APIKey) *adminKeysRepoStub {
	return &adminKeysRepoStub{byProject: map[string][]*persistence.APIKey{"p1": keys}}
}

func findKeyRow(rows []keyRow, id string) *keyRow {
	for i := range rows {
		if rows[i].KeyID == id {
			return &rows[i]
		}
	}
	return nil
}

// A credential that has authenticated but produced no ledger activity must
// still appear, carrying its authentication as evidence of life.
func TestAdoptionKeys_SeedsCredentialWithNoLedgerActivity(t *testing.T) {
	used := time.Date(2026, 9, 18, 9, 27, 0, 0, time.UTC)
	s := NewServer(WithAPIKeyRepository(keyRepoWith(&persistence.APIKey{
		ID: "akey_quiet", Name: "vadim/laptop", ProjectID: "p1", LastUsedAt: &used,
	})))

	st := &adoptionStats{}
	s.resolveEphemeralKeys(context.Background(), st, []string{"p1"})

	row := findKeyRow(st.Keys, "akey_quiet")
	if row == nil {
		t.Fatalf("a key that authenticated is absent from the board; rows=%+v", st.Keys)
	}
	if row.Label != "vadim/laptop" {
		t.Errorf("label = %q, want the operator-facing name", row.Label)
	}
	if row.Ephemeral {
		t.Errorf("a real credential was folded into the ephemeral bucket")
	}
	if row.LastSeen == nil || !row.LastSeen.Equal(used) {
		t.Errorf("LastSeen = %v, want the key's last_used_at %v", row.LastSeen, used)
	}
}

// The recency signal must come from authentication too, not only from ledger
// activity — that is the half of the report about the date not incrementing.
func TestAdoptionKeys_LastSeenFallsBackToAuthentication(t *testing.T) {
	used := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	s := NewServer(WithAPIKeyRepository(keyRepoWith(&persistence.APIKey{
		ID: "akey_active", Name: "slava/companion", ProjectID: "p1", LastUsedAt: &used,
	})))

	st := &adoptionStats{Keys: []keyRow{{KeyID: "akey_active", RAGQueries: 4}}}
	s.resolveEphemeralKeys(context.Background(), st, []string{"p1"})

	row := findKeyRow(st.Keys, "akey_active")
	if row == nil {
		t.Fatal("an active credential vanished")
	}
	if row.LastSeen == nil || !row.LastSeen.Equal(used) {
		t.Errorf("LastSeen = %v, want %v on a row that already had activity", row.LastSeen, used)
	}
	if row.RAGQueries != 4 {
		t.Errorf("existing activity was lost: RAGQueries = %d", row.RAGQueries)
	}
}

// Seeding must not flood the board with the system's own per-task credentials —
// this deployment holds 623 of them against 14 real keys. They are folded, and
// seeding them individually would bury every human row.
func TestAdoptionKeys_DoesNotSeedEphemeralAgentKeys(t *testing.T) {
	used := time.Now()
	s := NewServer(WithAPIKeyRepository(keyRepoWith(
		&persistence.APIKey{ID: "key_t1", Name: persistence.TaskKeyNamePrefix + "abc", ProjectID: "p1", LastUsedAt: &used},
		&persistence.APIKey{ID: "key_w1", Name: warmAgentKeyNamePrefix + "def", ProjectID: "p1", LastUsedAt: &used},
	)))

	st := &adoptionStats{}
	s.resolveEphemeralKeys(context.Background(), st, []string{"p1"})

	if len(st.Keys) != 0 {
		t.Errorf("ephemeral agent credentials were seeded as rows: %+v", st.Keys)
	}
}

// A revoked credential is gone; showing it would pad the board with history.
func TestAdoptionKeys_DoesNotSeedRevokedKeys(t *testing.T) {
	used := time.Now()
	revoked := time.Now()
	s := NewServer(WithAPIKeyRepository(keyRepoWith(&persistence.APIKey{
		ID: "akey_dead", Name: "old/laptop", ProjectID: "p1", LastUsedAt: &used, RevokedAt: &revoked,
	})))

	st := &adoptionStats{}
	s.resolveEphemeralKeys(context.Background(), st, []string{"p1"})

	if findKeyRow(st.Keys, "akey_dead") != nil {
		t.Errorf("a revoked credential was seeded: %+v", st.Keys)
	}
}

// A seeded, never-active key must not outrank a credential doing real work.
func TestAdoptionKeys_SeededRowsRankBelowActiveOnes(t *testing.T) {
	old := time.Now().Add(-240 * time.Hour)
	recent := time.Now()
	s := NewServer(WithAPIKeyRepository(keyRepoWith(
		&persistence.APIKey{ID: "akey_busy", Name: "busy", ProjectID: "p1", LastUsedAt: &old},
		&persistence.APIKey{ID: "akey_quiet", Name: "quiet", ProjectID: "p1", LastUsedAt: &recent},
	)))

	st := &adoptionStats{Keys: []keyRow{{KeyID: "akey_busy", RAGQueries: 10}}}
	s.resolveEphemeralKeys(context.Background(), st, []string{"p1"})

	if len(st.Keys) < 2 {
		t.Fatalf("expected both credentials on the board, got %+v", st.Keys)
	}
	if st.Keys[0].KeyID != "akey_busy" {
		t.Errorf("a zero-activity key outranked an active one: order = %v",
			[]string{st.Keys[0].KeyID, st.Keys[1].KeyID})
	}
}

// The end-to-end assertion: the customer's symptom is what the PAGE shows, so
// the quiet credential and its last-seen date must survive template rendering.
// A Go-side fix that never reaches the HTML would leave the report unresolved.
func TestInsightsAdoptionPage_RendersQuietCredentialAndLastSeen(t *testing.T) {
	s := NewServer()
	used := time.Date(2026, 9, 18, 9, 27, 0, 0, time.UTC)
	data := AdoptionData{
		Title: "Adoption", CurrentPage: "insights",
		Stats: adoptionStats{
			Days: 30,
			Keys: []keyRow{
				{KeyID: "akey_busy", Label: "vadim/migration-restore", RAGQueries: 709, LastSeen: &used},
				{KeyID: "akey_quiet", Label: "slava/companion", NoMeasuredActivity: true, LastSeen: &used},
			},
			KeysWithActivity: 1,
		},
	}

	rec := httptest.NewRecorder()
	s.render(rec, "insights_adoption.html", data)
	body := rec.Body.String()

	if strings.Contains(body, "Internal server error") {
		t.Fatalf("template errored mid-page: %q", body[max(0, len(body)-300):])
	}
	if !strings.Contains(body, "</html>") {
		t.Fatal("page did not render to completion")
	}
	if !strings.Contains(body, "slava/companion") {
		t.Error("the quiet credential is absent from the rendered page")
	}
	if !strings.Contains(body, "authenticated, no measured activity") {
		t.Error("the quiet credential is not labelled as unmeasured — it would read as idle")
	}
	if !strings.Contains(body, "2026-09-18") {
		t.Error("last-seen date did not render")
	}
	if !strings.Contains(body, "Credentials with measured activity") {
		t.Error("the summary tile still claims every row is an active credential")
	}
}

// A board that silently truncates answers "where is my key?" with silence.
// §5's discipline is that a panel always states what it does not cover; that
// applies to the ROW COUNT as much as to the coverage percentage. On a
// large-audience install this is the whole of the customer's "we don't see new
// keys": the keys are there, below the cut, and nothing said so.
func TestAdoptionKeys_ReportsHowManyCredentialsAreNotShown(t *testing.T) {
	var keys []*persistence.APIKey
	used := time.Now()
	for i := 0; i < 26; i++ {
		keys = append(keys, &persistence.APIKey{
			ID:         fmt.Sprintf("akey_%02d", i),
			Name:       fmt.Sprintf("user-%02d", i),
			ProjectID:  "p1",
			LastUsedAt: &used,
		})
	}
	s := NewServer(WithAPIKeyRepository(keyRepoWith(keys...)))

	st := &adoptionStats{}
	s.resolveEphemeralKeys(context.Background(), st, []string{"p1"})

	if len(st.Keys) != 20 {
		t.Fatalf("rendered %d rows, want the 20-row cap", len(st.Keys))
	}
	if st.KeysTotal != 26 {
		t.Errorf("KeysTotal = %d, want 26 credentials before truncation", st.KeysTotal)
	}
	if st.KeysHidden != 6 {
		t.Errorf("KeysHidden = %d, want 6", st.KeysHidden)
	}
}

// And the count has to reach the page, or it explains nothing.
func TestInsightsAdoptionPage_DisclosesHiddenCredentials(t *testing.T) {
	s := NewServer()
	data := AdoptionData{
		Title: "Adoption", CurrentPage: "insights",
		Stats: adoptionStats{
			Days:       30,
			Keys:       []keyRow{{KeyID: "akey_1", Label: "someone"}},
			KeysTotal:  26,
			KeysHidden: 6,
		},
	}

	rec := httptest.NewRecorder()
	s.render(rec, "insights_adoption.html", data)
	body := rec.Body.String()

	if strings.Contains(body, "Internal server error") {
		t.Fatalf("template errored: %q", body[max(0, len(body)-300):])
	}
	if !strings.Contains(body, "6 more") {
		t.Error("the page does not say how many credentials are hidden below the cut")
	}
}
