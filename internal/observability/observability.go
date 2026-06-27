package observability

import (
	"context"

	"github.com/rs/zerolog"
)

// Observability aggregates metrics and tracing components.
type Observability struct {
	cfg     Config
	logger  zerolog.Logger
	Metrics *Metrics
	Tracing *Tracing
}

// New creates a new Observability instance with metrics and optional tracing.
func New(cfg Config, logger zerolog.Logger) (*Observability, error) {
	obs := &Observability{
		cfg:    cfg,
		logger: logger,
	}

	// Always create the metrics registry so subsystem metrics can register
	// against it. The dedicated metrics server only starts if MetricsAddr is set,
	// but the registry is also used by /metrics on the main API server.
	obs.Metrics = NewMetrics(cfg, logger)
	if cfg.MetricsAddr != "" {
		logger.Info().Str("address", cfg.MetricsAddr).Msg("dedicated metrics server configured")
	}

	// Initialize tracing (optional, may be nil if disabled)
	tracing, err := NewTracing(cfg, logger)
	if err != nil {
		return nil, err
	}
	obs.Tracing = tracing

	return obs, nil
}

// StartMetricsServer starts the Prometheus metrics server in a goroutine.
// Returns a channel that will receive any error from the server.
func (o *Observability) StartMetricsServer(ctx context.Context) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		if err := o.Metrics.StartServer(ctx); err != nil {
			errCh <- err
		}
	}()
	return errCh
}

// Shutdown gracefully shuts down all observability components.
func (o *Observability) Shutdown(ctx context.Context) error {
	o.logger.Info().Msg("shutting down observability")

	var errs []error

	if o.Metrics != nil {
		if err := o.Metrics.Shutdown(ctx); err != nil {
			o.logger.Error().Err(err).Msg("metrics shutdown error")
			errs = append(errs, err)
		}
	}

	if o.Tracing != nil {
		if err := o.Tracing.Shutdown(ctx); err != nil {
			o.logger.Error().Err(err).Msg("tracing shutdown error")
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errs[0] // Return first error
	}

	return nil
}
