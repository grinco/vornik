package service

// container_clustering_accessors.go — exported Container accessors that the
// relocated EE clustering provider (internal/enterprise/clustering) needs to
// build its subsystems without touching internal/service unexported state.
//
// These mirror the exported accessors the trading subsystem already uses
// (Repos, InitWorkerElector, RegisterExtraElector, CollectorsCtx). The
// clustering subsystems were moved out of package service in Phase 2c; the
// registration logic that used to read c.capabilities() / c.daemonHolderID()
// / c.observabilityRegistry() / c.operatorAlertNotifier() inline now lives in
// the EE provider and reaches those values through these wrappers.

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"vornik.io/vornik/internal/config"
)

// ClusterOperatorAlerter is the narrow push-alert contract the cluster monitor
// uses for operator alerts. Satisfied by *steering.OperatorAlertNotifier.
// Returned as an interface (nil when no recipient is configured) so the EE
// clustering package needn't import internal/steering.
type ClusterOperatorAlerter interface {
	NotifyOperator(ctx context.Context, subject, body string)
}

// Capabilities is the exported wrapper for capabilities(): the resolved node
// capabilities (ServeUI/ServeAPI/ServeWebhooks/RunWorkers/RelayMode) the EE
// clustering provider gates its subsystems on.
func (c *Container) Capabilities() config.NodeCapabilities { return c.capabilities() }

// DaemonHolderID is the exported wrapper for daemonHolderID(): this daemon
// instance's stable holder string (hostname:pid:nonce), used as the cluster
// node instance_id by the heartbeat subsystems.
func (c *Container) DaemonHolderID() string { return c.daemonHolderID() }

// ObservabilityRegistry is the exported wrapper for observabilityRegistry():
// the Prometheus registry the cluster monitor registers its gauges on. May be
// nil (no metrics) — callers nil-guard.
func (c *Container) ObservabilityRegistry() *prometheus.Registry { return c.observabilityRegistry() }

// OperatorAlerter is the exported wrapper for operatorAlertNotifier(): the
// operator-alert sink the cluster monitor edge-alerts through. Returns a nil
// interface (not a typed-nil) when no operator recipient is configured, so the
// EE provider's nil check behaves correctly.
func (c *Container) OperatorAlerter() ClusterOperatorAlerter {
	n := c.operatorAlertNotifier()
	if n == nil {
		return nil
	}
	return n
}
