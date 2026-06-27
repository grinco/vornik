package playbook

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// Covers the repo-error return, the role filter, and the limit cap +
// default — the LearnedRemediations branches not exercised by the
// existing class/project-scope test.
func TestLearnedRemediations_ErrorRoleAndLimit(t *testing.T) {
	ctx := context.Background()

	t.Run("repo error propagates", func(t *testing.T) {
		repo := &fakeLister{err: errors.New("db down")}
		_, err := LearnedRemediations(ctx, repo, "Timeout", "p", "", 0)
		require.Error(t, err)
	})

	t.Run("role filter drops non-matching roles", func(t *testing.T) {
		repo := &fakeLister{rowsByStatus: map[string][]*persistence.Instinct{
			persistence.InstinctStatusActive: {
				{ID: "a", Action: "x", Confidence: 0.8, Trigger: mustTrigger(t, "scout", "Timeout")},
				{ID: "b", Action: "y", Confidence: 0.9, Trigger: mustTrigger(t, "writer", "Timeout")},
			},
		}}
		got, err := LearnedRemediations(ctx, repo, "Timeout", "p", "scout", 0)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "scout", got[0].Role)
	})

	t.Run("nil instinct rows are skipped", func(t *testing.T) {
		repo := &fakeLister{rowsByStatus: map[string][]*persistence.Instinct{
			persistence.InstinctStatusActive: {
				nil, // a nil row must be skipped, not panic
				{ID: "a", Action: "x", Confidence: 0.8, Trigger: mustTrigger(t, "scout", "Timeout")},
			},
		}}
		got, err := LearnedRemediations(ctx, repo, "Timeout", "p", "", 0)
		require.NoError(t, err)
		require.Len(t, got, 1)
	})

	t.Run("limit caps the result", func(t *testing.T) {
		rows := make([]*persistence.Instinct, 0, 5)
		for _, id := range []string{"a", "b", "c", "d", "e"} {
			rows = append(rows, &persistence.Instinct{ID: id, Action: "x", Confidence: 0.8, Trigger: mustTrigger(t, "scout", "Timeout")})
		}
		repo := &fakeLister{rowsByStatus: map[string][]*persistence.Instinct{persistence.InstinctStatusActive: rows}}
		got, err := LearnedRemediations(ctx, repo, "Timeout", "p", "", 2)
		require.NoError(t, err)
		assert.Len(t, got, 2, "explicit limit=2 should cap the result")
	})
}
