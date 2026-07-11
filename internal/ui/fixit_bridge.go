package ui

// Task 3.4 (fix-it-doctor-design.md §5.5/§7) — the read seams that back
// the Fix-It Doctor's IntegrationProbeProvider / ReloadStatusProvider /
// IntegrationReprober adapters. This file deliberately exposes only
// PRIMITIVE / already-public types (integrations.ProbeResult, plain
// strings) — never a fixitdoctor type — so this package never imports
// internal/fixitdoctor. internal/service (which already imports both
// packages, see fixit_dispatch_adapter.go) does the actual
// fixitdoctor.IntegrationProbeProvider/ReloadStatusProvider/
// IntegrationReprober implementations, translating these methods'
// return values into that package's shapes. Keeping the translation
// there — not here — matches every existing adapter in this codebase
// (api.configReloaderAdapter, ui.integrationsReloaderAdapter,
// fixitConfigReloaderAdapter, ...): the internal package exposes its
// own state via its own vocabulary, and the seam-specific adapter
// package does the mapping.

import (
	"context"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/integrations"
)

// LatestIntegrationProbe returns the cached probe result + the kind's
// registered doc URL for (kindID, projectID) — the red_integration
// grounding bundle's data source (task 3.1's IntegrationProbeProvider,
// wired for real here). ok is false when kindID isn't a registered
// integration kind, or no probe has ever been cached for (kindID,
// projectID) — the SAME "never probed on catalog load" cache
// storeIntegrationProbe populates (design §5.5: populated on save +
// explicit re-check only).
func (s *Server) LatestIntegrationProbe(kindID, projectID string) (result integrations.ProbeResult, docURL string, ok bool) {
	kind, found := findIntegrationKind(s.integrationsRegistry(), kindID)
	if !found {
		return integrations.ProbeResult{}, "", false
	}
	entry, cached := s.cachedIntegrationProbe(kindID, projectID)
	if !cached {
		return integrations.ProbeResult{}, kind.DocURL, false
	}
	return entry.Result, kind.DocURL, true
}

// ReprobeIntegrationLive re-runs a LIVE probe against (kindID,
// projectID)'s currently-saved candidate config — the exact path
// IntegrationRecheck (POST /integrations/{kind}/recheck) drives, minus
// the HTTP request/response plumbing and the caller-scope check (the
// Fix-It Doctor dispatcher has already scope-checked the session before
// ever reaching this method — see fixItScopeAndAdminGate). This is what
// completes reprobe_integration's live path (task 3.3 left
// IntegrationReprober nil / fail-closed): the cache is updated exactly
// like a manual re-check, so the catalog tile reflects the fresh result
// too, not just the fix-it session.
func (s *Server) ReprobeIntegrationLive(ctx context.Context, kindID, projectID string) (integrations.ProbeResult, error) {
	kind, found := findIntegrationKind(s.integrationsRegistry(), kindID)
	if !found {
		return integrations.ProbeResult{}, fmt.Errorf("ui: unknown integration kind %q", kindID)
	}
	if kind.Prober == nil {
		return integrations.ProbeResult{}, fmt.Errorf("ui: %q is not probeable", kindID)
	}
	cand := integrations.CandidateConfig{Kind: kind.ID, ProjectID: projectID, Values: s.currentIntegrationValues(kind.ID, projectID)}
	result := kind.Prober.Probe(ctx, cand)
	s.integrationsMetrics.RecordProbe(kind.ID, string(result.Outcome))
	s.storeIntegrationProbe(kind.ID, projectID, result)
	return result, nil
}

// LatestReloadError reports the current config-reload validation error,
// if any — the failed_reload grounding bundle's data source (task 3.1's
// ReloadStatusProvider, wired for real here) and the SAME
// reloadStatusReader.Status() the persistent restart banner
// (config_reload.go restartBanner) self-clears against, so the doctor
// and the banner can never disagree about whether an error is still
// live. ok is false when the wired ConfigReloader doesn't expose
// Status() (test fakes / the no-reloader smoke path) or the last reload
// cycle recorded no errors — which self-clears the instant a later
// reload succeeds (watcher.go resets reloadErrors at the start of every
// cycle), exactly mirroring the banner's own self-clear.
func (s *Server) LatestReloadError() (message string, ok bool) {
	sr, isReader := s.configReloader.(reloadStatusReader)
	if !isReader {
		return "", false
	}
	status := sr.Status()
	if !status.HasErrors || len(status.Errors) == 0 {
		return "", false
	}
	return strings.Join(status.Errors, "; "), true
}
