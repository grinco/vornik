// Package cli provides command-line interface commands for vornik.
package cli

import (
	"github.com/spf13/cobra"
	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/version"
)

// Build information (set at build time or via SetVersion)
var (
	Version   = version.Default
	BuildDate = version.UnknownBuildDate
)

// SetVersion sets the version information for the CLI.
func SetVersion(v string) {
	Version = v
}

// edition is the build edition reported by `vornikctl version`. Set via
// SetEdition from cmd/vornikctl/main.go (ldflags-injected); defaults to
// community when unstamped.
var edition = version.DefaultEdition

// SetEdition records the build edition for the version command.
func SetEdition(e string) { edition = version.NormalizeEdition(e) }

// ObservabilityConfig holds the parsed observability CLI flags.
var ObservabilityConfig observability.Config

// rootCmd is the shared Cobra root for the vornikctl operator CLI. The
// daemon binary (cmd/vornik/main.go) does NOT go through this package;
// it calls service.Run directly. Use reflects that: help text rendered
// from this tree is for operators invoking vornikctl, not the daemon.
var rootCmd = &cobra.Command{
	Use:   "vornikctl",
	Short: "vornik operator CLI",
	Long:  "vornikctl inspects and controls a running vornik daemon.",
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

// RootCmd exposes the assembled command tree for tooling (e.g. the docs
// generator that renders the CLI reference). It is not for runtime use —
// callers should go through Execute.
func RootCmd() *cobra.Command {
	return rootCmd
}

func init() {
	// Observability flags
	rootCmd.PersistentFlags().StringVar(&ObservabilityConfig.MetricsAddr, "metrics-addr", ":9090", "address for Prometheus metrics server")
	rootCmd.PersistentFlags().BoolVar(&ObservabilityConfig.TracingEnabled, "tracing-enabled", false, "enable OpenTelemetry tracing")
	rootCmd.PersistentFlags().StringVar(&ObservabilityConfig.TracingEndpoint, "tracing-endpoint", "localhost:4317", "OTLP gRPC endpoint for trace export")

	// Add subcommands
	rootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Println(version.BuildLine("vornikctl version", Version, BuildDate, edition))
	},
}
