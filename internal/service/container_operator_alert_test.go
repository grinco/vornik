package service

import (
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
)

// TestOperatorAlertNotifier_NilUnlessConfigured guards the backwards-compatible
// default: with no operator recipient configured the fallback sink is nil, so
// combinedSteeringNotifier behaves exactly as before. A configured recipient
// produces a live notifier.
func TestOperatorAlertNotifier_NilUnlessConfigured(t *testing.T) {
	t.Run("unconfigured → nil", func(t *testing.T) {
		c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
		if c.operatorAlertNotifier() != nil {
			t.Fatal("expected nil operator-alert notifier when no recipient is configured")
		}
	})

	t.Run("channel without session → nil", func(t *testing.T) {
		c := &Container{Logger: zerolog.Nop(), Config: &config.Config{
			SteeringOperatorAlert: config.SteeringOperatorAlertConfig{Channel: "telegram"},
		}}
		if c.operatorAlertNotifier() != nil {
			t.Fatal("expected nil when a channel is set but no session")
		}
	})

	t.Run("channel + session → live notifier", func(t *testing.T) {
		c := &Container{Logger: zerolog.Nop(), Config: &config.Config{
			SteeringNotificationsEnabled: true,
			SteeringOperatorAlert:        config.SteeringOperatorAlertConfig{Channel: "telegram", Session: "555"},
		}}
		if c.operatorAlertNotifier() == nil {
			t.Fatal("expected a live operator-alert notifier when channel+session are configured")
		}
	})
}
