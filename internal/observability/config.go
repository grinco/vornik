package observability

// Config holds observability configuration for metrics and tracing.
type Config struct {
	// MetricsAddr is the address for the Prometheus metrics server.
	// Default is ":9090".
	MetricsAddr string

	// TracingEnabled controls whether OpenTelemetry tracing is enabled.
	// Default is false.
	TracingEnabled bool

	// TracingEndpoint is the OTLP gRPC endpoint for trace export.
	// Default is "localhost:4317".
	TracingEndpoint string
}
