package service

import (
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/ui"
)

// dispatcherToolInventoryAdapter bridges dispatcher.Agent.InventoryTools
// to ui.DispatcherToolInventory. Translates between the two
// near-identical row types so the ui package stays free of the
// internal/dispatcher import that would otherwise pull the chat
// client + tool executor into its test binary.
type dispatcherToolInventoryAdapter struct {
	agent *dispatcher.Agent
}

// newDispatcherToolInventory wraps the boot-time dispatcher agent
// into the UI-facing inventory source. Returns nil when no agent
// is wired so the admin page falls through to its "not wired"
// empty state.
func newDispatcherToolInventory(agent *dispatcher.Agent) ui.DispatcherToolInventory {
	if agent == nil {
		return nil
	}
	return &dispatcherToolInventoryAdapter{agent: agent}
}

// DispatcherTools implements ui.DispatcherToolInventory. Pulls a
// fresh snapshot per call (the underlying state can flip after
// boot — e.g. SetEmailSender lands later once email channels are
// built — so per-request is the right cadence).
func (a *dispatcherToolInventoryAdapter) DispatcherTools() []ui.AdminDispatcherToolRow {
	if a == nil || a.agent == nil {
		return nil
	}
	infos := a.agent.InventoryTools()
	out := make([]ui.AdminDispatcherToolRow, 0, len(infos))
	for _, i := range infos {
		out = append(out, ui.AdminDispatcherToolRow{
			Name:           i.Name,
			Description:    i.Description,
			BackingService: i.BackingService,
			Available:      i.Available,
		})
	}
	return out
}
