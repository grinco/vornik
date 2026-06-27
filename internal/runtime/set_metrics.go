package runtime

// SetMetrics updates the Prometheus metrics on a running Manager.
// Used when observability is initialised after the manager is created,
// allowing the caller to wire a new registry without re-creating the manager.
func (m *Manager) SetMetrics(metrics *Metrics) {
	m.metrics = metrics
}
