package postgres

import (
	"database/sql"

	"vornik.io/vornik/internal/persistence"
)

// scanInstinctArms stitches the query's two rows — one per arm — onto one
// result. An arm with no executions at all produces NO row, which is why the
// zero value is the right starting point: an absent arm is an empty one, and
// EligibleN 0 is what makes coverage report 0 rather than complete.
func scanInstinctArms(rows *sql.Rows, out *persistence.InstinctRatingArms) error {
	for rows.Next() {
		var treated int
		var arm persistence.RatingArm
		if err := rows.Scan(&treated, &arm.EligibleN, &arm.RatedN, &arm.UpN, &arm.ContestedN); err != nil {
			return err
		}
		if treated == 1 {
			out.Treatment = arm
		} else {
			out.Baseline = arm
		}
	}
	return rows.Err()
}
