package executor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taskkeys"
)

// TestUnownedTaskKey_E2E_RealMinterRealStore is the guard the 2026-09-13
// identity plan (§5, review R2 gate) requires BEFORE any api_keys migration
// or resolver change lands: a REAL task run, through the real executor loop
// (MockRuntime stands in for the container only), mints its per-task key
// with the PRODUCTION minter over a REAL store, the container receives that
// key, the key authenticates through the production auth backend as a
// project-bound credential, and it carries NO owner. It is then revoked at
// teardown.
//
// This is the test that catches the whole class the plan names: a
// non-null owner default in a later migration, a resolver that decides to
// refuse unowned keys as a "safety default", an ORM change nobody read.
// Every agent run in the deployment goes through this path.
func TestUnownedTaskKey_E2E_RealMinterRealStore(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Connect(ctx, sqlite.DefaultConfig())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Migrate(ctx))
	keys := sqlite.NewAPIKeyRepository(db.DB)

	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{`{"status":"COMPLETED","message":"done"}`}

	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	ar := &stubArtifactRepo{}
	e := NewWithOptions(rt, er, ar, tr, nil, WithAPIKeyMinter(taskkeys.New(keys)))
	e.config.RetryDelay = 0
	judgeDone := make(chan struct{})
	e.judgeRunner = &stubJudgeRunner{done: judgeDone}
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1", HallucinationJudge: registry.ProjectHallucinationJudge{Enabled: true}},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {ID: "wf1", Entrypoint: "work",
				Steps:     map[string]registry.WorkflowStep{"work": {Type: "agent", Role: "worker", OnSuccess: "done"}},
				Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}}},
		},
	})

	const taskID = "t-unowned-key"
	tr.AddTask(&persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusLeased, Attempt: 1, MaxAttempts: 1,
		Payload: []byte(`{"context":{"prompt":"do the thing"}}`), CreatedAt: time.Now()})
	require.NoError(t, e.Execute(taskID))
	select {
	case <-judgeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("workflow did not complete")
	}
	require.Equal(t, 1, rt.StartCalls())

	// 1. The container received a freshly minted per-task key.
	rt.mu.Lock()
	minted := rt.lastConfig.EnvVars["VORNIK_API_KEY"]
	rt.mu.Unlock()
	require.NotEmpty(t, minted, "container must receive VORNIK_API_KEY")
	claimed, _, perr := apikey.Parse(minted)
	require.NoError(t, perr)
	assert.True(t, apikey.MatchesProject(claimed, "p1"), "minted key must be project-scoped to p1")

	// 2. The row the minter wrote carries NO owner. (Asserted on the row
	// itself: if a future migration adds an owner column with a non-null
	// default, or the minter starts stamping one, this is where it shows.)
	rows, err := keys.ListByProject(ctx, "p1")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, persistence.TaskKeyNamePrefix+taskID, row.Name)
	assert.Empty(t, row.ClientKind, "per-task keys must not carry a client_kind (companion path)")
	assert.Empty(t, row.OwnerUserID, "an executor-minted key must be UNOWNED")

	// 3. Teardown revoked it: the production auth backend now refuses the
	// key, and it refused NOTHING about ownership on the way — a fresh
	// mint of the same shape authenticates as an unowned project credential.
	backend := auth.NewDBKeysBackend(keys, nil)
	_, aerr := backend.Authenticate(ctx, auth.Credential{BearerToken: minted})
	assert.ErrorIs(t, aerr, auth.ErrNoCredential, "revoked per-task key must no longer authenticate")

	fresh, err := taskkeys.New(keys).MintTaskKey(ctx, "p1", "t-fresh")
	require.NoError(t, err)
	id, aerr := backend.Authenticate(ctx, auth.Credential{BearerToken: fresh})
	require.NoError(t, aerr)
	assert.Equal(t, "p1", id.BoundProjectID)
	assert.Equal(t, []string{"p1"}, id.Projects)
	freshRow, _ := id.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey)
	require.NotNil(t, freshRow)
	assert.Empty(t, freshRow.OwnerUserID, "a freshly minted task key resolves as UNOWNED — never refused, never promoted")
}
