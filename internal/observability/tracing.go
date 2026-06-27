package observability

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// Tracing holds the OpenTelemetry tracer provider.
type Tracing struct {
	cfg      Config
	logger   zerolog.Logger
	provider *sdktrace.TracerProvider
}

// NewTracing creates a new Tracing instance with OpenTelemetry configuration.
// If tracing is disabled in config, returns nil without error.
func NewTracing(cfg Config, logger zerolog.Logger) (*Tracing, error) {
	if !cfg.TracingEnabled {
		logger.Info().Msg("tracing disabled, skipping tracer initialization")
		return nil, nil
	}

	// Create OTLP gRPC exporter
	exporter, err := otlptracegrpc.New(context.Background(),
		otlptracegrpc.WithEndpoint(cfg.TracingEndpoint),
		otlptracegrpc.WithInsecure(), // For local development; use TLS in production
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	// Create resource with service name
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("vornik"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Create tracer provider
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	// Set global tracer provider
	otel.SetTracerProvider(provider)

	// Set global propagator for trace context propagation
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	logger.Info().Str("endpoint", cfg.TracingEndpoint).Msg("tracing initialized")

	return &Tracing{
		cfg:      cfg,
		logger:   logger,
		provider: provider,
	}, nil
}

// Shutdown gracefully shuts down the tracer provider.
func (t *Tracing) Shutdown(ctx context.Context) error {
	if t.provider == nil {
		return nil
	}

	t.logger.Info().Msg("shutting down tracer provider")
	return t.provider.Shutdown(ctx)
}
