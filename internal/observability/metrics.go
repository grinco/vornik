package observability

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
)

// Metrics holds the Prometheus registry and metrics server.
type Metrics struct {
	cfg    Config
	logger zerolog.Logger
	regs   *prometheus.Registry
	server *http.Server
}

// NewMetrics creates a new Metrics instance with a Prometheus registry.
func NewMetrics(cfg Config, logger zerolog.Logger) *Metrics {
	registry := prometheus.NewRegistry()
	registryUp := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "vornik",
		Subsystem: "observability",
		Name:      "up",
		Help:      "Whether the dedicated observability registry is active.",
	})
	registryUp.Set(1)
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		registryUp,
	)

	return &Metrics{
		cfg:    cfg,
		logger: logger,
		regs:   registry,
	}
}

// Registry returns the Prometheus registry for registering custom metrics.
func (m *Metrics) Registry() *prometheus.Registry {
	return m.regs
}

// StartServer starts the Prometheus metrics HTTP server.
// This should be run in a goroutine as it blocks until the context is cancelled.
func (m *Metrics) StartServer(ctx context.Context) error {
	mux := http.NewServeMux()

	// Expose the Prometheus metrics endpoint
	mux.Handle("/metrics", promhttp.HandlerFor(m.regs, promhttp.HandlerOpts{}))

	m.server = &http.Server{
		Addr:              m.cfg.MetricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	m.logger.Info().Str("address", m.cfg.MetricsAddr).Msg("starting metrics server")

	if err := m.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("metrics server error: %w", err)
	}

	return nil
}

// Shutdown gracefully stops the metrics server.
func (m *Metrics) Shutdown(ctx context.Context) error {
	if m.server == nil {
		return nil
	}

	m.logger.Info().Msg("stopping metrics server")
	return m.server.Shutdown(ctx)
}

// MustRegister registers the provided Collectors with the registry.
// It panics if any Collector registration fails.
func (m *Metrics) MustRegister(collectors ...prometheus.Collector) {
	m.regs.MustRegister(collectors...)
}
