package ui

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// filterRecordingIngestAudit records the filter it was handed, which is the
// whole point: the defect was not a wrong NUMBER, it was a query with no time
// bound behind a label that promised one.
type filterRecordingIngestAudit struct {
	gotFilter    persistence.MemoryIngestAuditFilter
	listCalls    int
	byProjectHit bool
}

func (f *filterRecordingIngestAudit) List(_ context.Context, filter persistence.MemoryIngestAuditFilter) ([]*persistence.MemoryIngestAudit, error) {
	f.listCalls++
	f.gotFilter = filter
	return nil, nil
}

func (f *filterRecordingIngestAudit) ListByProject(context.Context, string, int) ([]*persistence.MemoryIngestAudit, error) {
	// The unbounded call the panel used to make. Reaching it fails the test.
	f.byProjectHit = true
	return nil, nil
}

func (f *filterRecordingIngestAudit) Record(context.Context, *persistence.MemoryIngestAudit) error {
	return nil
}

// TestAddMemoryWrites_RespectsTheWindowItRendersUnder — the adoption panel says
// "over the last N days" and, until 2026-09-04, counted memory writes over ALL
// HISTORY: addMemoryWrites called the unbounded ListByProject while every other
// collector on the panel took the same `since`. The overstatement grew with
// corpus age, on the panel whose own comments stress honest usage reporting.
// Introduced by d01b78f4, found by the 2026-09-03 audit.
func TestAddMemoryWrites_RespectsTheWindowItRendersUnder(t *testing.T) {
	repo := &filterRecordingIngestAudit{}
	s := &Server{memoryIngestAudit: repo}

	since := time.Now().AddDate(0, 0, -adoptionDays)
	st := &adoptionStats{}
	rows := map[string]*keyRow{}
	row := func(id string) *keyRow {
		if r, ok := rows[id]; ok {
			return r
		}
		r := &keyRow{KeyID: id}
		rows[id] = r
		return r
	}

	s.addMemoryWrites(context.Background(), st, []string{"p1"}, since, row)

	require.Equal(t, 1, repo.listCalls, "the filtered List is the collector the panel must use")
	assert.False(t, repo.byProjectHit, "the unbounded ListByProject must not be reached")
	assert.Equal(t, "p1", repo.gotFilter.ProjectID)
	assert.False(t, repo.gotFilter.Since.IsZero(),
		"a zero Since is the bug: it means the query counted all history under a %d-day label", adoptionDays)
	assert.WithinDuration(t, since, repo.gotFilter.Since, time.Second,
		"the window must be the one the template renders")
	assert.Equal(t, adoptionSampleCap, repo.gotFilter.PageSize, "the cap must survive the switch")
}
