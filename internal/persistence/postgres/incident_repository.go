package postgres

import (
	"context"
	"database/sql"
	"time"

	"vornik.io/vornik/internal/incident"
)

// IncidentRepository persists the Art 33/34 breach ledger.
//
// see LLD § https://docs.vornik.io §4.10
type IncidentRepository struct {
	db *sql.DB
}

// NewIncidentRepository constructs the repository.
func NewIncidentRepository(db *sql.DB) *IncidentRepository {
	return &IncidentRepository{db: db}
}

// Create records a newly detected incident and starts the 72-hour clock.
func (r *IncidentRepository) Create(ctx context.Context, i incident.Incident) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO security_incidents (id, state, occurred_at, became_aware_at)
		 VALUES ($1, $2, $3, $4)`,
		i.ID, string(i.State), nullTimeIfZero(i.OccurredAt), i.BecameAwareAt)
	return mapDBError(err)
}

// Get fetches one incident.
func (r *IncidentRepository) Get(ctx context.Context, id string) (incident.Incident, error) {
	var i incident.Incident
	var state string
	var occurred, notifiedAuth, notifiedSubj, closed sql.NullTime
	err := r.db.QueryRowContext(ctx,
		`SELECT id, state, occurred_at, became_aware_at, facts, effects, remedial,
		        authority_risk, authority_risk_reason, notified_authority_at, authority_reference,
		        subject_risk, subject_risk_reason, notified_subjects_at, subject_exemption,
		        assessed_by, closed_at
		   FROM security_incidents WHERE id = $1`, id).
		Scan(&i.ID, &state, &occurred, &i.BecameAwareAt, &i.Facts, &i.Effects, &i.Remedial,
			&i.AuthorityRisk, &i.AuthorityRiskReason, &notifiedAuth, &i.AuthorityReference,
			&i.SubjectRisk, &i.SubjectRiskReason, &notifiedSubj, &i.SubjectExemption,
			&i.AssessedBy, &closed)
	if err != nil {
		return incident.Incident{}, mapDBError(err)
	}
	i.State = incident.State(state)
	i.OccurredAt, i.NotifiedAuthorityAt = occurred.Time, notifiedAuth.Time
	i.NotifiedSubjectsAt, i.ClosedAt = notifiedSubj.Time, closed.Time
	return i, nil
}

// Save persists a state transition.
func (r *IncidentRepository) Save(ctx context.Context, i incident.Incident) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE security_incidents
		    SET state = $2, facts = $3, effects = $4, remedial = $5,
		        authority_risk = $6, authority_risk_reason = $7,
		        notified_authority_at = $8, authority_reference = $9,
		        subject_risk = $10, subject_risk_reason = $11,
		        notified_subjects_at = $12, subject_exemption = $13,
		        assessed_by = $14, closed_at = $15, updated_at = NOW()
		  WHERE id = $1`,
		i.ID, string(i.State), i.Facts, i.Effects, i.Remedial,
		i.AuthorityRisk, i.AuthorityRiskReason,
		nullTimeIfZero(i.NotifiedAuthorityAt), i.AuthorityReference,
		i.SubjectRisk, i.SubjectRiskReason,
		nullTimeIfZero(i.NotifiedSubjectsAt), i.SubjectExemption,
		i.AssessedBy, nullTimeIfZero(i.ClosedAt))
	return mapDBError(err)
}

// ListLive returns incidents whose Art 33 obligation is undischarged, oldest
// awareness first — the query the 72-hour deadline check runs.
func (r *IncidentRepository) ListLive(ctx context.Context) ([]incident.Incident, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, state, became_aware_at, authority_risk, subject_risk
		   FROM security_incidents
		  WHERE state IN ('detected', 'assessed')
		  ORDER BY became_aware_at`)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []incident.Incident
	for rows.Next() {
		var i incident.Incident
		var state string
		if err := rows.Scan(&i.ID, &state, &i.BecameAwareAt, &i.AuthorityRisk, &i.SubjectRisk); err != nil {
			return nil, err
		}
		i.State = incident.State(state)
		out = append(out, i)
	}
	return out, rows.Err()
}

// CountLiveByDeadline reports how many undischarged incidents are overdue and
// how many are approaching the 72-hour limit. Used by the doctor check, which
// must not need the whole ledger to answer "is anything on fire".
func (r *IncidentRepository) CountLiveByDeadline(ctx context.Context, now time.Time) (overdue, soon int, err error) {
	live, err := r.ListLive(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, i := range live {
		switch {
		case i.Overdue(now):
			overdue++
		case i.NeedsAttention(now):
			soon++
		}
	}
	return overdue, soon, nil
}
