package playbook

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// fakeLister is a tiny in-memory LearnedRemediationLister so the overlay
// can be exercised without a DB. It records the filters it was asked
// for so tests can assert the query shape (project/domain/status scoping).
type fakeLister struct {
	// rowsByStatus maps a status literal to the rows returned for it.
	rowsByStatus map[string][]*persistence.Instinct
	err          error
	gotFilters   []persistence.InstinctFilter
}

func (f *fakeLister) List(_ context.Context, filter persistence.InstinctFilter) ([]*persistence.Instinct, error) {
	f.gotFilters = append(f.gotFilters, filter)
	if f.err != nil {
		return nil, f.err
	}
	status := ""
	if filter.Status != nil {
		status = *filter.Status
	}
	return f.rowsByStatus[status], nil
}

func mustTrigger(t *testing.T, role, class string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]string{"role": role, "error_class": class})
	require.NoError(t, err)
	return b
}

func TestLearnedRemediations_NilRepoOrEmptyArgs(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name           string
		repo           LearnedRemediationLister
		class, project string
	}{
		{"nil repo", nil, "Timeout", "proj1"},
		{"empty class", &fakeLister{}, "", "proj1"},
		{"empty project", &fakeLister{}, "Timeout", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LearnedRemediations(ctx, tc.repo, tc.class, tc.project, "", 0)
			assert.NoError(t, err)
			assert.Nil(t, got)
		})
	}
}

func TestLearnedRemediations_FiltersByClassAndProjectScope(t *testing.T) {
	ctx := context.Background()
	repo := &fakeLister{rowsByStatus: map[string][]*persistence.Instinct{
		persistence.InstinctStatusActive: {
			{ID: "i1", Action: "retrying resolved the Timeout failure", Confidence: 0.7, SupportCount: 5, Trigger: mustTrigger(t, "scout", "Timeout")},
			{ID: "i2", Action: "irrelevant other class", Confidence: 0.9, SupportCount: 9, Trigger: mustTrigger(t, "scout", "ParseError")},
		},
	}}

	got, err := LearnedRemediations(ctx, repo, "Timeout", "proj1", "", 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "i1", got[0].InstinctID)
	assert.Equal(t, "Timeout", got[0].ErrorClass)
	assert.Equal(t, 5, got[0].SupportCount)

	// Query must be scoped to the recovery domain + the project.
	require.NotEmpty(t, repo.gotFilters)
	for _, f := range repo.gotFilters {
		require.NotNil(t, f.Domain)
		assert.Equal(t, persistence.InstinctDomainRecovery, *f.Domain)
		require.NotNil(t, f.ProjectID)
		assert.Equal(t, "proj1", *f.ProjectID)
	}
}

func TestLearnedRemediations_RoleFilter(t *testing.T) {
	ctx := context.Background()
	repo := &fakeLister{rowsByStatus: map[string][]*persistence.Instinct{
		persistence.InstinctStatusActive: {
			{ID: "scout-row", Action: "a", Confidence: 0.7, Trigger: mustTrigger(t, "scout", "Timeout")},
			{ID: "lead-row", Action: "b", Confidence: 0.8, Trigger: mustTrigger(t, "lead", "Timeout")},
			{ID: "norole-row", Action: "c", Confidence: 0.6, Trigger: mustTrigger(t, "", "Timeout")},
		},
	}}

	// role="scout" keeps the scout row + the no-role row (empty trigger
	// role matches any role), drops the lead row.
	got, err := LearnedRemediations(ctx, repo, "Timeout", "proj1", "scout", 0)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, r := range got {
		ids[r.InstinctID] = true
	}
	assert.True(t, ids["scout-row"])
	assert.True(t, ids["norole-row"])
	assert.False(t, ids["lead-row"])
}

func TestLearnedRemediations_MergesStatusesSortsAndLimits(t *testing.T) {
	ctx := context.Background()
	repo := &fakeLister{rowsByStatus: map[string][]*persistence.Instinct{
		persistence.InstinctStatusActive: {
			{ID: "a-low", Action: "x", Confidence: 0.61, Trigger: mustTrigger(t, "scout", "Timeout")},
		},
		persistence.InstinctStatusPromoted: {
			{ID: "p-high", Action: "y", Confidence: 0.95, Trigger: mustTrigger(t, "scout", "Timeout")},
			{ID: "p-mid", Action: "z", Confidence: 0.80, Trigger: mustTrigger(t, "scout", "Timeout")},
		},
	}}

	got, err := LearnedRemediations(ctx, repo, "Timeout", "proj1", "", 2)
	require.NoError(t, err)
	require.Len(t, got, 2) // limit honoured
	assert.Equal(t, "p-high", got[0].InstinctID)
	assert.Equal(t, "p-mid", got[1].InstinctID) // highest confidence first
}

func TestLearnedRemediations_MalformedTriggerSkippedNotFatal(t *testing.T) {
	ctx := context.Background()
	repo := &fakeLister{rowsByStatus: map[string][]*persistence.Instinct{
		persistence.InstinctStatusActive: {
			{ID: "bad", Action: "x", Confidence: 0.9, Trigger: json.RawMessage(`{not json`)},
			{ID: "good", Action: "y", Confidence: 0.7, Trigger: mustTrigger(t, "scout", "Timeout")},
		},
	}}
	got, err := LearnedRemediations(ctx, repo, "Timeout", "proj1", "", 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "good", got[0].InstinctID)
}

func TestLearnedRemediations_RepoErrorPropagates(t *testing.T) {
	ctx := context.Background()
	repo := &fakeLister{err: errors.New("boom")}
	got, err := LearnedRemediations(ctx, repo, "Timeout", "proj1", "", 0)
	assert.Error(t, err)
	assert.Nil(t, got)
}
