package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/ratings"
)

// GET /api/v1/skills/{id}/rating-rollup — did this skill make things worse?
// (LLD 2026-09-07-execution-ratings-rollup-design.md, phase 2.)
//
// Read-only and advisory. Nothing here feeds behaviour: the rollup informs an
// operator deciding whether to retire a skill, and never gates a run.

// defaultRollupWindowHours is one week — long enough that a low-traffic project
// clears the per-arm floor, short enough that a verdict is about the skill as it
// behaves now rather than as it behaved a quarter ago.
const defaultRollupWindowHours = 168

// RatingArmResponse is one arm, rendered with its coverage.
//
// Coverage is not optional here. The design refuses an aggregate without its
// counterfactual anywhere, API included: a lift figure whose arms were observed
// at different rates measures who was watching, and a consumer that only got
// the number could not tell.
type RatingArmResponse struct {
	Coverage   float64 `json:"coverage"`
	RatedN     int     `json:"rated_n"`
	UpN        int     `json:"up_n"`
	ContestedN int     `json:"contested_n"`
}

// RatingRollupContextResponse is one (project, workflow) context.
type RatingRollupContextResponse struct {
	ProjectID  string            `json:"project_id"`
	WorkflowID string            `json:"workflow_id"`
	Verdict    string            `json:"verdict"`
	Lift       float64           `json:"lift"`
	Treatment  RatingArmResponse `json:"treatment"`
	Baseline   RatingArmResponse `json:"baseline"`
}

// SkillRatingRollupResponse is the endpoint's body.
type SkillRatingRollupResponse struct {
	SkillID     string                        `json:"skill_id"`
	WindowHours int                           `json:"window_hours"`
	Summary     string                        `json:"summary"`
	Contexts    []RatingRollupContextResponse `json:"contexts"`
}

// SkillRatingRollup serves the rollup for one skill.
func (s *Server) SkillRatingRollup(w http.ResponseWriter, r *http.Request, skillID string) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if s.ratingRollupRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "RATINGS_DISABLED",
			"execution ratings not wired on this deployment")
		return
	}
	if skillID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "skill id is required")
		return
	}

	hours := defaultRollupWindowHours
	if raw := strings.TrimSpace(r.URL.Query().Get("window_hours")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			// Refused rather than silently defaulted: a rollup over a window
			// the caller did not ask for is a different measurement wearing
			// the same name, and they would quote it as the one they asked for.
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
				"window_hours must be a positive integer")
			return
		}
		hours = n
	}

	res, err := ratings.SkillRollup(r.Context(), s.ratingRollupRepo, skillID,
		time.Duration(hours)*time.Hour, ratings.DefaultConfig())
	if err != nil {
		s.logger.Error().Err(err).Str("skillId", skillID).Msg("rating rollup failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to compute rollup")
		return
	}

	out := SkillRatingRollupResponse{
		SkillID:     res.SkillID,
		WindowHours: hours,
		Summary:     res.Summary,
		Contexts:    make([]RatingRollupContextResponse, 0, len(res.Contexts)),
	}
	for _, c := range res.Contexts {
		out.Contexts = append(out.Contexts, RatingRollupContextResponse{
			ProjectID:  c.ProjectID,
			WorkflowID: c.WorkflowID,
			Verdict:    c.Result.Verdict,
			Lift:       c.Result.Lift,
			Treatment: RatingArmResponse{
				Coverage: c.Result.TreatmentCoverage, RatedN: c.Result.TreatmentN,
				UpN: c.Result.TreatmentUp, ContestedN: c.Result.TreatmentContested,
			},
			Baseline: RatingArmResponse{
				Coverage: c.Result.BaselineCoverage, RatedN: c.Result.BaselineN,
				UpN: c.Result.BaselineUp, ContestedN: c.Result.BaselineContested,
			},
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// apiV1SkillsHandler routes /api/v1/skills/{id}/... .
//
// Was a direct HandleFunc to SkillSetGlobal, which owned the whole prefix. A
// second verb needs a dispatcher; the shape mirrors apiV1ExecutionsHandler, and
// the fall-through still reaches SkillSetGlobal so the shipped POST is
// unchanged rather than re-implemented here.
func (s *Server) apiV1SkillsHandler(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/skills/")
	if id, ok := strings.CutSuffix(rest, "/rating-rollup"); ok && id != "" && !strings.Contains(id, "/") {
		s.SkillRatingRollup(w, r, id)
		return
	}
	s.SkillSetGlobal(w, r)
}
