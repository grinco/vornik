package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// KindApplier seam (2026-07-19-instinct-lift-measurement-design.md §4.5):
// some proposal kinds mutate application state directly rather than
// rewriting a deployed config file. A registered KindApplier bypasses the
// file-based apply path (buildOps/validate/reload/mirror) entirely — the
// engine just dispatches to it and records the ledger transition.
type KindApplier interface {
	Kind() string
	// Apply performs the mutation and returns the JSON snapshot Rollback
	// restores from (persisted in pre_apply_snapshot by the engine).
	Apply(ctx context.Context, p *persistence.ControlPlaneProposal) (snapshot string, err error)
	// Rollback restores pre-apply state from p.PreApplySnapshot. It MUST
	// fail safe when current state no longer matches the snapshot's
	// assumptions (never blindly overwrite newer operator intent).
	Rollback(ctx context.Context, p *persistence.ControlPlaneProposal) error
}

// instinctRetireStore is the narrow slice of persistence.InstinctRepository
// InstinctRetireApplier needs.
type instinctRetireStore interface {
	Get(ctx context.Context, id string) (*persistence.Instinct, error)
	Retire(ctx context.Context, id string) error
	UnretireTo(ctx context.Context, id, priorStatus string) error
}

// retireSnapshot is the JSON pre-apply snapshot for an instinct_retire
// proposal: the instinct's status immediately before Retire flipped it.
type retireSnapshot struct {
	InstinctID  string `json:"instinct_id"`
	PriorStatus string `json:"prior_status"`
}

// InstinctRetireApplier is the KindApplier for
// persistence.ProposalKindInstinctRetire — it retires an instinct on Apply
// and restores its prior status on Rollback.
type InstinctRetireApplier struct{ Instincts instinctRetireStore }

// Kind implements KindApplier.
func (a *InstinctRetireApplier) Kind() string { return persistence.ProposalKindInstinctRetire }

// Apply implements KindApplier.
func (a *InstinctRetireApplier) Apply(ctx context.Context, p *persistence.ControlPlaneProposal) (string, error) {
	var ev struct {
		InstinctID string `json:"instinct_id"`
	}
	if err := json.Unmarshal([]byte(p.Evidence), &ev); err != nil || ev.InstinctID == "" {
		return "", fmt.Errorf("instinct_retire: proposal evidence missing instinct_id")
	}
	inst, err := a.Instincts.Get(ctx, ev.InstinctID)
	if err != nil {
		return "", fmt.Errorf("instinct_retire: %w", err)
	}
	if inst.Status == persistence.InstinctStatusRetired {
		return "", fmt.Errorf("instinct_retire: instinct %s already retired", inst.ID)
	}
	snap, _ := json.Marshal(retireSnapshot{InstinctID: inst.ID, PriorStatus: inst.Status})
	if err := a.Instincts.Retire(ctx, inst.ID); err != nil {
		return "", fmt.Errorf("instinct_retire: %w", err)
	}
	return string(snap), nil
}

// Rollback implements KindApplier.
func (a *InstinctRetireApplier) Rollback(ctx context.Context, p *persistence.ControlPlaneProposal) error {
	var snap retireSnapshot
	if err := json.Unmarshal([]byte(p.PreApplySnapshot), &snap); err != nil || snap.InstinctID == "" {
		return fmt.Errorf("instinct_retire rollback: bad pre_apply_snapshot")
	}
	if err := a.Instincts.UnretireTo(ctx, snap.InstinctID, snap.PriorStatus); err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return fmt.Errorf("instinct_retire rollback refused (fail-safe): instinct %s is no longer retired — it was re-scored/promoted after the apply; not overwriting newer state: %w", snap.InstinctID, err)
		}
		return err
	}
	return nil
}

// kindApplier is a nil-map-safe lookup into e.KindAppliers.
func (e *ApplyEngine) kindApplier(kind string) KindApplier {
	if e.KindAppliers == nil {
		return nil
	}
	return e.KindAppliers[kind]
}
