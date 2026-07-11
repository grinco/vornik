package service

// Task 3.4 — end-to-end proof that reprobe_integration, dispatched
// through the REAL fixitdoctor.Service (wired via wireFixItDispatcher,
// exactly as container_http.go wires it in production), now executes a
// live probe via *ui.Server instead of task 3.3's fail-closed
// "not configured on this deployment" stub. Uses ui.Server's real,
// unmodified "telegram" catalog entry, whose Prober short-circuits to
// OutcomeFail with no network dial when the candidate carries no
// bot_token (see fixit_ui_bridge_adapter_test.go's file header for why
// this is a safe, deterministic real-Prober exercise).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/fixitdoctor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ui"
	"vornik.io/vornik/internal/version"
)

func TestReprobeIntegration_ViaRealDispatcher_ExecutesLiveProbe(t *testing.T) {
	srv := ui.NewServer()
	c := &Container{}
	c.uiServer.Store(srv)

	sessions := newFakeFixItSessions()
	svc := &fixitdoctor.Service{
		Sessions: sessions,
		Metrics:  fixitdoctor.NewMetrics(prometheus.NewRegistry()),
		Edition:  version.EditionEnterprise,
	}
	wireFixItDispatcher(svc, c, nil)
	require.NotNil(t, svc.IntegrationReprober, "precondition: task 3.4 must wire IntegrationReprober")

	env := fixitdoctor.FixItEnvelope{
		Message: "I can re-check the Telegram connection for you.",
		Actions: []fixitdoctor.ProposedAction{{
			Kind:   fixitdoctor.ActionKindReprobeIntegration,
			Label:  "Re-check Telegram",
			Params: map[string]string{"integration_id": "telegram"},
		}},
	}
	envJSON, err := json.Marshal(env)
	require.NoError(t, err)
	session := &persistence.FixItSession{
		ID:           persistence.GenerateID("fix"),
		OperatorID:   "op-1",
		FailureKind:  string(fixitdoctor.FailureKindRedIntegration),
		FailureRefID: "telegram",
		LastEnvelope: envJSON,
	}
	require.NoError(t, sessions.Insert(context.Background(), session))

	result, err := svc.Dispatch(context.Background(), session.ID, "op-1", 0, "")
	require.NoError(t, err)

	// The task 3.3 fail-closed stub's Detail is the literal string
	// "reprobe_integration is not configured on this deployment" — this
	// must NOT be what comes back now that task 3.4 wired a real
	// IntegrationReprober.
	assert.NotContains(t, strings.ToLower(result.Detail), "not configured",
		"reprobe_integration must no longer fail closed now that it's wired")
	assert.Equal(t, fixitdoctor.ActionResultApplied, result.Result,
		"a re-probe always \"applies\" — it ran a live check regardless of outcome color")

	// Prove it was a LIVE probe (telegram's real Prober, not a canned
	// fake): the catalog's cache now carries a fresh, real result.
	cachedResult, _, ok := srv.LatestIntegrationProbe("telegram", "")
	require.True(t, ok, "the live reprobe must have updated ui.Server's cache")
	assert.NotEmpty(t, cachedResult.Summary)
}
