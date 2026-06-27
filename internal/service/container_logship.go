package service

// Log-forwarding (logship) wiring. The forwarding stack itself
// (internal/enterprise/logship) is an Enterprise capability stripped from the
// CE source; the CE container drives it only through the
// contracts.LogForwarder seam built by ProviderSet.LogForwarderFactory. When
// the factory is nil (Community) or the config disables forwarding, the root
// logger is left untouched (zero overhead) and no goroutines/sinks are created.
// See https://docs.vornik.io and
// editions-phase2c-gated-feature-relocation-design.md (Phase A).

import (
	"context"
	"io"
	"os"

	"github.com/prometheus/client_golang/prometheus"
)

// logshipMetricsAttacher is the optional interface an EE LogForwarder may
// implement so the CE container can register the logship Prometheus series
// once the observability registry exists (the forwarder is built at boot,
// before the registry). It is deliberately NOT part of contracts.LogForwarder
// — the core seam stays narrow; metrics wiring is driven opaquely via this
// type assertion. A Community build has no forwarder, so this is never hit.
type logshipMetricsAttacher interface {
	AttachMetrics(reg *prometheus.Registry)
}

// initLogship builds and starts the log-forwarding stack when the edition
// provides a factory AND config enables it. It is called from NewContainer
// AFTER the audit repos exist but BEFORE the scheduler/dispatcher capture
// them, so the audit decorators reach those consumers. Metrics are attached
// later (in NewContainerWithObservability) via attachLogshipMetrics once the
// Prometheus registry exists.
//
// When forwarding is disabled this is a no-op and the root logger is left
// exactly as initLogger built it (zero overhead).
//
// Gating: the Logship capability is an EE feature. When providers.Logship is
// false (Community edition or any build that omits the flag) the function
// returns immediately. The factory is the structural gate: a Community build
// wires no LogForwarderFactory, so even with the flag set there is nothing to
// construct. The factory returns (nil, nil) when config disables forwarding.
func (c *Container) initLogship() error {
	if !c.providers.Logship {
		c.Logger.Info().Str("capability", "logship").Str("edition", c.Edition()).Msg("EE capability omitted by edition")
		return nil
	}
	if c.providers.LogForwarderFactory == nil {
		// EE flag set but no factory wired (e.g. a misassembled build) — treat
		// as Community: no forwarding, no error.
		return nil
	}
	fwd, err := c.providers.LogForwarderFactory(c.Config.Logging.Forward, os.Getenv)
	if err != nil {
		return err
	}
	if fwd == nil {
		// Forwarding disabled in config — nothing built (zero overhead).
		return nil
	}
	fwd.Start()
	c.logshipForwarder = fwd

	// App-log tap: re-point the root logger at MultiWriter(stdout, tap) so
	// every component's JSON line is also offered to the forwarder. Done here
	// rather than in initLogger so the disabled path stays a plain
	// zerolog.New(os.Stdout) with no wrapper.
	c.Logger = c.Logger.Output(io.MultiWriter(os.Stdout, fwd.AppWriter(os.Stdout)))

	// Audit taps: decorate the admin + tool audit repos so a successful write
	// also ships an audit Event.
	c.decorateAuditRepos()

	c.Logger.Info().Msg("log forwarding enabled")
	return nil
}

// decorateAuditRepos delegates to the forwarder's idempotent repo decoration
// so every successful audit write also ships an Event. It must be re-applied
// after any c.repos rebuild (e.g. the observability metrics-wrapping rebuild)
// so the fresh handles are decorated too. A no-op when forwarding is off
// (forwarder nil) or the repos aren't built. The forwarder guards against
// double-wrapping.
func (c *Container) decorateAuditRepos() {
	if c.logshipForwarder == nil || c.repos == nil {
		return
	}
	c.logshipForwarder.DecorateAuditRepos(c.repos)
}

// attachLogshipMetrics registers the logship Prometheus series and attaches
// them to the running forwarder. Called from NewContainerWithObservability
// after the registry exists. No-op when forwarding is disabled or there is no
// registry, or when the forwarder does not expose metrics attachment.
func (c *Container) attachLogshipMetrics() {
	if c.logshipForwarder == nil {
		return
	}
	reg := c.observabilityRegistry()
	if reg == nil {
		return
	}
	if a, ok := c.logshipForwarder.(logshipMetricsAttacher); ok {
		a.AttachMetrics(reg)
	}
}

// drainLogship flushes pending events and stops the forwarder on shutdown.
// Bounded by ctx. No-op when forwarding is disabled.
func (c *Container) drainLogship(ctx context.Context) {
	if c.logshipForwarder == nil {
		return
	}
	if err := c.logshipForwarder.Drain(ctx); err != nil {
		c.Logger.Warn().Err(err).Msg("logship drain did not complete within shutdown budget")
	}
}
