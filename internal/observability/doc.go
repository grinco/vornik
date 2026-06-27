// Package observability provides metrics and tracing infrastructure for vornik.
//
// This package implements Phase 1 observability scaffolding with Prometheus metrics
// and OpenTelemetry tracing support.
//
// ## Components
//
//   - Config: Configuration for metrics and tracing endpoints
//   - Metrics: Prometheus registry and metrics server
//   - Tracing: OpenTelemetry tracer provider setup
//
// ## Usage
//
//	cfg := observability.Config{
//	    MetricsAddr:     ":9090",
//	    TracingEnabled:  true,
//	    TracingEndpoint: "localhost:4317",
//	}
//
//	obs, err := observability.New(cfg, logger)
//	if err != nil {
//	    // handle error
//	}
//	defer obs.Shutdown(context.Background())
//
//	// Start metrics server (non-blocking)
//	go obs.StartMetricsServer(context.Background())
package observability
