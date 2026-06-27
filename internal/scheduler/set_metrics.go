package scheduler

// SetMetrics updates the Prometheus metrics on a running Scheduler.
// Used when observability is initialised after the scheduler is created,
// allowing the caller to wire a new registry without re-creating the scheduler.
func (s *Scheduler) SetMetrics(metrics *Metrics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = metrics
}
