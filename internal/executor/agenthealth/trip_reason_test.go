package agenthealth

import (
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

type reasonSink struct {
	trips []string
}

func (s *reasonSink) SetStateGauge(string, chat.CircuitState) {}
func (s *reasonSink) IncTrips(_ string, reason string)        { s.trips = append(s.trips, reason) }

// Two rules can open a circuit now, and they mean different things: a rate
// breach describes a share of recent traffic, a consecutive run describes a
// model that has stopped answering at all. An operator reading
// vornik_agent_model_health_trips_total must be able to tell them apart —
// otherwise the trip that finally catches a slow model looks identical to the
// ones that were already firing (design §4).
func TestRegistry_TripCarriesItsReason(t *testing.T) {
	now := time.Now()
	sink := &reasonSink{}
	r := NewRegistry(Config{
		Enabled: true,
		Health:  chat.HealthConfig{Window: time.Minute, MinSamples: 3, FailureRate: 0.5, OpenCooldown: 30 * time.Second},
		Now:     func() time.Time { return now },
	})
	r.SetMetrics(sink)

	// Three failures 50s apart: too slow for the window, so only the
	// consecutive rule can see them.
	infra := errors.New("PROVIDER_ERROR: upstream provider returned an error")
	for i := 0; i < 3; i++ {
		now = now.Add(50 * time.Second)
		r.Record("slow-model", false, infra)
	}

	if len(sink.trips) != 1 {
		t.Fatalf("trips = %v, want exactly one", sink.trips)
	}
	if sink.trips[0] != chat.TripReasonConsecutive {
		t.Errorf("trip reason = %q, want %q", sink.trips[0], chat.TripReasonConsecutive)
	}
}
