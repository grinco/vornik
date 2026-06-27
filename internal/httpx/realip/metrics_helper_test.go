package realip

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// newTestRegistry returns a fresh, isolated registry so each metrics test
// avoids the promauto.DefaultRegisterer double-registration panic.
func newTestRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()
	return prometheus.NewRegistry()
}
