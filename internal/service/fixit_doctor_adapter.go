package service

// Fix-It Doctor repair chat (task 3.2) — wires internal/fixitdoctor's
// converse Service behind api.FixItDoctor. Mirrors
// project_wizard_adapter.go's shape and the same "keep the api
// package free of an import on the internal implementation package"
// reasoning: the two packages don't share envelope types, so the
// adapter does the field-by-field translation in one place.
//
// Grounding completeness note: task 3.1 deliberately left
// IntegrationProbeProvider / ReloadStatusProvider unwired ("task 3.4
// wires the real one from ui.Server's per-kind+project probe cache" —
// see assembler.go's doc comments); task 3.4 (this file, below) wires
// both against *Container.uiServer via the fixit_ui_bridge_adapter.go
// adapters, so red_integration / failed_reload sessions now ground on
// live state. Both degrade gracefully (fail-closed / "no result known",
// never a panic) on a node with no UI server (c.uiServer nil — see
// Container.uiServer's doc comment) — the SAME degradation task 3.1
// documented, just now reached only in that narrower case rather than
// unconditionally. failed_task and degraded_feature are functional today
// via c.repos.Tasks/Executions/StepOutcomes/Instincts and
// featuredoctor's own registry.

import (
	"context"
	"strings"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/fixitdoctor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/projectdoctor"
)

// fixItDoctorAdapter satisfies api.FixItDoctor by wrapping a
// *fixitdoctor.Service.
type fixItDoctorAdapter struct {
	svc *fixitdoctor.Service
}

func newFixItDoctorAdapter(svc *fixitdoctor.Service) api.FixItDoctor {
	if svc == nil {
		return nil
	}
	return &fixItDoctorAdapter{svc: svc}
}

// buildFixItDoctorOrNil constructs the repair-chat service from the
// container's existing dependencies. Returns nil (handler 503s) when
// the chat router or the fixit sessions repo is missing — mirrors
// buildProjectWizardOrNil's gating.
//
// projectDoctor is the task 3.3 action-dispatcher's set_secret seam.
// It's a parameter (rather than read off c) because it isn't built
// until later in initHTTPServer — see container_http.go's call site,
// which was moved past the projectdoctor.New(...) construction for
// exactly this reason.
func buildFixItDoctorOrNil(c *Container, projectDoctor *projectdoctor.Doctor) api.FixItDoctor {
	if c == nil || c.repos == nil || c.repos.FixItSessions == nil || c.ChatClient == nil {
		return nil
	}
	svc := &fixitdoctor.Service{
		Sessions: c.repos.FixItSessions,
		Assembler: &fixitdoctor.Assembler{
			Tasks:        c.repos.Tasks,
			Executions:   c.repos.Executions,
			StepOutcomes: c.repos.StepOutcomes,
			Learned:      c.repos.Instincts,
			// Task 3.4: wired against *Container.uiServer, read lazily
			// (see fixit_ui_bridge_adapter.go) — uiServer doesn't exist
			// yet at this call site (see the file-level doc comment
			// above), only by the time an actual request invokes these.
			IntegrationProbes: fixitIntegrationProbeProvider{c: c},
			ReloadStatus:      fixitReloadStatusProvider{c: c},
		},
		Chat:              c.ChatClient,
		Model:             resolveFixItDoctorModel(c.Config),
		Edition:           c.Edition(),
		LLMUsage:          c.repos.LLMUsage,
		BudgetRepo:        c.repos.LLMUsage,
		Projects:          c.Registry,
		Metrics:           c.fixItDoctorMetrics,
		MaxActiveSessions: 5,
		MaxTurns:          20,
	}
	// Task 3.3: wire the deny-by-default action dispatcher's pipelines
	// (see fixit_dispatch_adapter.go). Nil-safe throughout — a missing
	// piece just leaves that one ActionKind's pipeline nil.
	wireFixItDispatcher(svc, c, projectDoctor)
	return newFixItDoctorAdapter(svc)
}

// resolveFixItDoctorModel picks the LLM model for repair-chat turns.
// chat.fixit_model when set (mirrors chat.wizard_model's precedent),
// else "" (the chat provider's own default).
func resolveFixItDoctorModel(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Chat.FixItModel)
}

func (a *fixItDoctorAdapter) Converse(ctx context.Context, sessionID, operatorID, failureKind, failureRefID, projectID, userMessage string) (*api.FixItResult, error) {
	ref := fixitdoctor.FailureRef{
		Kind:      fixitdoctor.FailureKind(failureKind),
		ID:        failureRefID,
		ProjectID: projectID,
	}
	res, err := a.svc.Converse(ctx, sessionID, operatorID, ref, userMessage)
	if err != nil {
		return nil, err
	}
	return toAPIFixItResult(res), nil
}

// Apply satisfies api.FixItDoctor's task-3.3 addition — one deny-by-
// default dispatch call against sessionID's last proposed actions. See
// fixitdoctor.Service.Dispatch (dispatch.go) for the actual routing;
// this adapter only translates the result to the api-boundary DTO.
func (a *fixItDoctorAdapter) Apply(ctx context.Context, sessionID, operatorID string, actionIndex int, secretValue string) (*api.FixItApplyResult, error) {
	res, err := a.svc.Dispatch(ctx, sessionID, operatorID, actionIndex, secretValue)
	if err != nil {
		return nil, err
	}
	return &api.FixItApplyResult{
		Kind:       string(res.Kind),
		Result:     res.Result,
		Detail:     res.Detail,
		Diff:       res.Diff,
		RollbackID: res.RollbackID,
	}, nil
}

// Rollback satisfies api.FixItDoctor's Rollback addition — see
// fixitdoctor.Service.RollbackConfigApply.
func (a *fixItDoctorAdapter) Rollback(ctx context.Context, sessionID, operatorID, proposalID string) (*api.FixItApplyResult, error) {
	res, err := a.svc.RollbackConfigApply(ctx, sessionID, operatorID, proposalID)
	if err != nil {
		return nil, err
	}
	return &api.FixItApplyResult{
		Kind:       string(res.Kind),
		Result:     res.Result,
		Detail:     res.Detail,
		RollbackID: res.RollbackID,
	}, nil
}

func (a *fixItDoctorAdapter) SessionScope(ctx context.Context, sessionID, operatorID string) (string, bool, error) {
	row, err := a.svc.Sessions.Get(ctx, sessionID)
	if err != nil {
		if err == persistence.ErrNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	if row == nil || row.OperatorID != operatorID {
		return "", false, nil
	}
	return row.ProjectID, true, nil
}

func toAPIFixItResult(res *fixitdoctor.Result) *api.FixItResult {
	if res == nil {
		return nil
	}
	out := &api.FixItResult{SessionID: res.SessionID}
	if res.Envelope != nil {
		env := &api.FixItEnvelope{
			Message:  res.Envelope.Message,
			Resolved: res.Envelope.Resolved,
		}
		for _, a := range res.Envelope.Actions {
			env.Actions = append(env.Actions, api.FixItProposedAction{
				Kind:   string(a.Kind),
				Label:  a.Label,
				Params: a.Params,
			})
		}
		out.Envelope = env
	}
	if res.StatusPoll != nil {
		out.StatusPoll = &api.FixItStatusPoll{
			Summary: res.StatusPoll.Summary,
			Healthy: res.StatusPoll.Healthy,
		}
	}
	return out
}
