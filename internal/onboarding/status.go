// Package onboarding provides first-run installation detection and
// setup-guide session state for the Vornik daemon.
package onboarding

import (
	"context"
	"strings"

	"vornik.io/vornik/internal/config"
)

// CommittedSessionChecker is the minimal repository contract the
// detector needs to decide whether the install is already onboarded.
type CommittedSessionChecker interface {
	HasCommitted(ctx context.Context) (bool, error)
}

// Status is the setup-guide decision payload shared by the UI and API.
// FreshInstall means the guide should auto-open for an admin-capable
// entry point.
type Status struct {
	FreshInstall bool     `json:"fresh_install"`
	Onboarded    bool     `json:"onboarded"`
	Source       string   `json:"source"`
	Reasons      []string `json:"reasons,omitempty"`
}

// Detector resolves the installation onboarding state from durable
// setup rows plus a conservative config heuristic.
type Detector struct {
	Sessions CommittedSessionChecker
	Config   *config.Config
}

// Detect reports whether the guide should auto-open. Durable committed
// onboarding state wins. If there is no committed row yet, the detector
// falls back to a conservative heuristic that treats an install as
// fresh when there is no wired chat backend or when memory is enabled
// without an embedding model.
func (d Detector) Detect(ctx context.Context) Status {
	if d.Sessions != nil {
		if committed, err := d.Sessions.HasCommitted(ctx); err == nil {
			if committed {
				return Status{
					FreshInstall: false,
					Onboarded:    true,
					Source:       "durable",
				}
			}
		}
	}

	status := Status{Source: "heuristic"}
	if d.Config == nil {
		status.FreshInstall = true
		status.Reasons = append(status.Reasons, "config unavailable")
		return status
	}

	chatConfigured := strings.TrimSpace(d.Config.Chat.Endpoint) != "" && strings.TrimSpace(d.Config.Chat.Model) != ""
	dispatcherPinned := strings.TrimSpace(d.Config.Telegram.DispatcherProjectID) != ""

	if !chatConfigured {
		status.FreshInstall = true
		status.Reasons = append(status.Reasons, "chat backend not wired")
	}
	if d.Config.Memory.Enabled {
		if strings.TrimSpace(d.Config.Memory.EmbeddingModel) == "" {
			status.FreshInstall = true
			status.Reasons = append(status.Reasons, "memory enabled without embedding model")
		}
		if strings.TrimSpace(d.Config.Memory.EmbeddingEndpoint) == "" {
			status.FreshInstall = true
			status.Reasons = append(status.Reasons, "memory enabled without embedding endpoint")
		}
	}
	if !dispatcherPinned {
		status.FreshInstall = true
		status.Reasons = append(status.Reasons, "dispatcher project not pinned")
	}
	if !status.FreshInstall {
		status.Onboarded = true
	}
	return status
}
