package memory

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestTier3SubstringSearch_GatesOnPublishedLifecycle is the regression
// for the 2026-06-04 bug sweep: the tier-3 ILIKE fallbacks (reached
// when both pgvector and tsvector are unavailable) omitted the
// `lifecycle_state = 'published'` filter that the hybrid and FTS tiers
// apply. Under a transient extension outage, recall surfaced
// raw/staged/quarantined/shadow chunks — un-vetted content leaking to
// agents.
//
// sqlmock's regex matcher checks the query text, so requiring
// `lifecycle_state = 'published'` in the expected pattern fails pre-fix
// (the clause is absent) and passes post-fix.
func TestTier3SubstringSearch_GatesOnPublishedLifecycle(t *testing.T) {
	chunkCols := []string{
		"id", "project_id", "task_id", "source_name", "content",
		"score", "content_class", "is_alive", "last_checked_at",
	}

	cases := []struct {
		name string
		call func(r *Repository) error
	}{
		{
			name: "substringSearch",
			call: func(r *Repository) error {
				_, err := r.substringSearch(context.Background(), "p1", "needle", 10, "", false)
				return err
			},
		},
		{
			name: "substringSearchTemporal",
			call: func(r *Repository) error {
				_, err := r.substringSearchTemporal(context.Background(), "p1", "needle", 10, time.Time{}, time.Time{}, "", false)
				return err
			},
		},
		{
			name: "substringSearchWithEpochsTemporal",
			call: func(r *Repository) error {
				_, err := r.substringSearchWithEpochsTemporal(context.Background(), "p1", "needle", 10, []string{"e1"}, time.Time{}, time.Time{}, "", false)
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, mock, cleanup := newRepo(t)
			defer cleanup()

			mock.ExpectQuery(`(?s)content ILIKE.*lifecycle_state = 'published'`).
				WillReturnRows(sqlmock.NewRows(chunkCols))

			if err := tc.call(r); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("%s: tier-3 query did not gate on published lifecycle: %v", tc.name, err)
			}
		})
	}
}
