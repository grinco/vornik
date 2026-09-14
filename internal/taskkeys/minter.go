// Package taskkeys mints and revokes the per-task and warm-pool API keys
// the executor injects into agent containers. Extracted from
// internal/service on 2026-09-13 so the identity work's FIRST test — a real
// task run mints its scoped key, the key works, and carries NO owner (plan
// §5, review R2 gate: "an actual executor task still uses an unowned scoped
// key") — can drive the production minter from the executor's end-to-end
// harness instead of a look-alike.
package taskkeys

import (
	"context"
	"time"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// Minter implements executor.APIKeyMinter over the api_keys repository.
// One struct per daemon; the executor calls it on every container start.
type Minter struct{ repo persistence.APIKeyRepository }

// New constructs the minter.
func New(repo persistence.APIKeyRepository) *Minter { return &Minter{repo: repo} }

// MintTaskKey generates a fresh project-scoped key, persists only the
// hash (never the raw key), and returns the raw key to the executor for
// injection into the container's VORNIK_API_KEY env var.
//
// Key invariants:
//   - client_kind is intentionally EMPTY — a non-empty client_kind would
//     route the key through the companion-path allowlist in middleware.go
//     (~line 382), blocking every internal agent call. Agent task keys
//     are identified by Name alone: "agent:task_<taskID>".
//   - expires_at is now+48h as belt-and-braces; the primary lifecycle is
//     the RevokeTaskKey call at step teardown.
func (m *Minter) MintTaskKey(ctx context.Context, projectID, taskID string) (string, error) {
	raw, err := apikey.Generate(projectID)
	if err != nil {
		return "", err
	}
	exp := time.Now().UTC().Add(48 * time.Hour)
	err = m.repo.Create(ctx, &persistence.APIKey{
		ID:        persistence.GenerateID("key"),
		ProjectID: projectID,
		Name:      persistence.TaskKeyNamePrefix + taskID,
		KeyHash:   apikey.Hash(raw),
		KeyPrefix: apikey.DisplayPrefix(raw),
		ExpiresAt: &exp,
		CreatedBy: "executor",
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

// RevokeTaskKey revokes the key named "agent:task_<taskID>". Uses
// RevokeByName so the caller doesn't need the key ID. Idempotent —
// zero-row UPDATE is not an error.
func (m *Minter) RevokeTaskKey(ctx context.Context, taskID string) error {
	return m.repo.RevokeByName(ctx, persistence.TaskKeyNamePrefix+taskID)
}

// WarmAgentKeyNamePrefix is the reserved APIKey.Name prefix for
// project-scoped warm-pool agent credentials (Finding B1(b)). The
// (project, role) tuple is appended so each pool gets a stable,
// distinguishable key. Deliberately NOT persistence.TaskKeyNamePrefix:
// warm keys are project-scoped, not task-scoped, so they must not match
// persistence.TaskIDFromKeyName (which would otherwise make the audit
// handlers and CallMCPTool treat a warm key as bound to a bogus task ID).
const WarmAgentKeyNamePrefix = "agent:warm_"

// MintProjectScopedKey generates a fresh PROJECT-scoped key for a
// warm-pool container (Finding B1(b)) and persists only the hash. Unlike
// MintTaskKey the key is not bound to any task — warm containers are
// baked once and reused across tasks, so a project-scoped credential
// (pools are keyed per project/role) is the tightest scope achievable
// without restarting the container per task. expires_at is now+48h;
// warm keys are reused for the pool's lifetime and a stale key simply
// expires (a new one is minted on the next cold start).
//
// Invariants match MintTaskKey: empty client_kind (a non-empty value
// routes through the companion-path allowlist in middleware.go and
// blocks every internal agent call).
func (m *Minter) MintProjectScopedKey(ctx context.Context, projectID, role string) (string, error) {
	raw, err := apikey.Generate(projectID)
	if err != nil {
		return "", err
	}
	exp := time.Now().UTC().Add(48 * time.Hour)
	err = m.repo.Create(ctx, &persistence.APIKey{
		ID:        persistence.GenerateID("key"),
		ProjectID: projectID,
		Name:      WarmAgentKeyNamePrefix + projectID + ":" + role,
		KeyHash:   apikey.Hash(raw),
		KeyPrefix: apikey.DisplayPrefix(raw),
		ExpiresAt: &exp,
		CreatedBy: "executor-warm",
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}
