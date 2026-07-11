package service

// Task 3.4 — wires the two providers task 3.1 deliberately left unwired
// (internal/fixitdoctor/assembler.go: IntegrationProbeProvider,
// ReloadStatusProvider) plus task 3.3's IntegrationReprober, all against
// *ui.Server's existing Phase-5 state (internal/ui/fixit_bridge.go).
//
// Ordering (why these adapters hold *Container, not *ui.Server): the
// Fix-It Doctor service is built earlier in initHTTPServer than the UI
// server is (see fixit_doctor_adapter.go's file-level doc comment and
// container_http.go's WithFixItDoctor call site) — uiServer doesn't
// exist yet at construction time. Every method below reads
// c.uiServer.Load() LAZILY, at call time, not at construction: by the
// time a browser request actually reaches a repair-chat turn or an
// Apply, initHTTPServer has long finished and c.uiServer (an
// atomic.Pointer, stored via .Store() immediately after
// ui.NewServer(...) — see Container.uiServer's doc comment) is stable.
// A nil c or a nil c.uiServer.Load() (a node with no UI server, e.g. a
// worker-only profile, or a request racing daemon startup before the
// store) degrades every method to its documented fail-closed / "no
// result known" branch — never a panic.

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/fixitdoctor"
	"vornik.io/vornik/internal/integrations"
)

// fixitIntegrationProbeProvider implements fixitdoctor.IntegrationProbeProvider
// over ui.Server.LatestIntegrationProbe — the red_integration grounding
// bundle's live data source. ref.ID is the integrations.IntegrationKind.ID
// (e.g. "telegram"); ref.ProjectID scopes project-scope kinds ("" for
// daemon scope).
type fixitIntegrationProbeProvider struct {
	c *Container
}

func (p fixitIntegrationProbeProvider) LatestProbe(_ context.Context, ref fixitdoctor.FailureRef) (integrations.ProbeResult, string, bool, error) {
	if p.c == nil {
		return integrations.ProbeResult{}, "", false, nil
	}
	srv := p.c.uiServer.Load()
	if srv == nil {
		return integrations.ProbeResult{}, "", false, nil
	}
	result, docURL, ok := srv.LatestIntegrationProbe(ref.ID, ref.ProjectID)
	return result, docURL, ok, nil
}

// fixitReloadStatusProvider implements fixitdoctor.ReloadStatusProvider
// over ui.Server.LatestReloadError — the failed_reload grounding
// bundle's live data source. OffendingKeyPath/OffendingValue are left
// zero: ui.Server's reload-error state (config.ReloadStatus.Errors,
// watcher.go) is a flat message list with no structured key-path
// tracking today, so there's nothing safe to populate them from — the
// assembler treats a nil OffendingValue as "not known" (see
// TestAssemble_FailedReload_NoOffendingValue), which is exactly the
// right degrade here rather than fabricating a path.
type fixitReloadStatusProvider struct {
	c *Container
}

func (p fixitReloadStatusProvider) LatestReloadError(_ context.Context, _ fixitdoctor.FailureRef) (fixitdoctor.ReloadValidationError, bool, error) {
	if p.c == nil {
		return fixitdoctor.ReloadValidationError{}, false, nil
	}
	srv := p.c.uiServer.Load()
	if srv == nil {
		return fixitdoctor.ReloadValidationError{}, false, nil
	}
	message, ok := srv.LatestReloadError()
	if !ok {
		return fixitdoctor.ReloadValidationError{}, false, nil
	}
	return fixitdoctor.ReloadValidationError{Message: message}, true, nil
}

// fixitIntegrationReprober implements fixitdoctor.IntegrationReprober over
// ui.Server.ReprobeIntegrationLive — completes reprobe_integration's live
// path (task 3.3 left IntegrationReprober nil, so Dispatch failed closed
// with "not configured on this deployment"; that branch is unreachable
// now except on a UI-less node, where it still fires — see the
// fail-closed check below).
type fixitIntegrationReprober struct {
	c *Container
}

func (p fixitIntegrationReprober) Reprobe(ctx context.Context, projectID, integrationID string) (string, bool, error) {
	if p.c == nil {
		return "", false, fmt.Errorf("fixit: reprobe_integration requires a UI server (none wired on this node)")
	}
	srv := p.c.uiServer.Load()
	if srv == nil {
		return "", false, fmt.Errorf("fixit: reprobe_integration requires a UI server (none wired on this node)")
	}
	result, err := srv.ReprobeIntegrationLive(ctx, integrationID, projectID)
	if err != nil {
		return "", false, err
	}
	summary := result.Summary
	if summary == "" {
		summary = fmt.Sprintf("probe outcome: %s", result.Outcome)
	}
	return summary, result.Outcome == integrations.OutcomeOK, nil
}
