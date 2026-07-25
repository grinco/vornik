package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/telemetryclient"
)

var telemetryCmd = &cobra.Command{
	Use:   "telemetry",
	Short: "Inspect anonymous usage telemetry",
}

var telemetrySampleCmd = &cobra.Command{
	Use:   "sample",
	Short: "Show telemetry state and sample payloads without sending",
	RunE:  runTelemetrySample,
}

var telemetryEmitInstallSource string

var telemetryEmitInstallCmd = &cobra.Command{
	Use:    "emit-install",
	Short:  "Emit the successful-install lifecycle event",
	Hidden: true,
	RunE:   runTelemetryEmitInstall,
}

func init() {
	telemetryEmitInstallCmd.Flags().StringVar(&telemetryEmitInstallSource, "source",
		telemetryclient.SourceQuickstart, "closed install source enum")
	telemetryCmd.AddCommand(telemetrySampleCmd, telemetryEmitInstallCmd)
	rootCmd.AddCommand(telemetryCmd)
}

func runTelemetrySample(cmd *cobra.Command, _ []string) error {
	configPath := telemetryConfigPath()
	enabled, source, err := config.ResolveTelemetryFile(configPath, os.Getenv("VORNIK_TELEMETRY"))
	if err != nil {
		// Inspection fails closed but still renders exactly what the client
		// would build if enabled.
		enabled = false
		source = "config-error"
	}
	install := telemetryclient.InstallEvent(Version, telemetryclient.SourceQuickstart)
	project := telemetryclient.ProjectEvent(Version, telemetryclient.SourceCLITemplate,
		"personal-assistant", true, false)
	installURL, installBody, buildErr := renderTelemetrySample(install)
	if buildErr != nil {
		return buildErr
	}
	projectURL, projectBody, buildErr := renderTelemetrySample(project)
	if buildErr != nil {
		return buildErr
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Anonymous telemetry: %v (source: %s)\nPrivacy: docs/public/reference/telemetry-and-privacy.md\n\nInstall URL:\n%s\nInstall body:\n%s\n\nProject URL:\n%s\nProject body:\n%s\n",
		enabled, source, installURL, installBody, projectURL, projectBody)
	return nil
}

func runTelemetryEmitInstall(_ *cobra.Command, _ []string) error {
	enabled, _, err := config.ResolveTelemetryFile(
		telemetryConfigPath(), os.Getenv("VORNIK_TELEMETRY"))
	if err != nil {
		return nil // fail closed and never fail installation
	}
	client := lifecycleTelemetryClient(enabled)
	_ = client.Emit(context.Background(),
		telemetryclient.InstallEvent(Version, telemetryEmitInstallSource))
	return nil
}

func renderTelemetrySample(event telemetryclient.Event) (string, string, error) {
	req, err := telemetryclient.BuildRequest(telemetryclient.DefaultEndpoint, event)
	if err != nil {
		return "", "", err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return "", "", err
	}
	return req.URL.String(), string(data), nil
}

func telemetryConfigPath() string {
	if path := os.Getenv("VORNIK_CONFIG"); path != "" {
		return path
	}
	configsDir := resolveConfigsDir("")
	if configsDir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configsDir), "config.yaml")
}
