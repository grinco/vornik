package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
)

// TestOrderEffectivePrice — what price feeds the volume
// estimate on the soak panel. LMT carries the agent's
// committed price; STP/STP_LMT carry the trigger; bare MKT
// without a fill returns 0 so the volume calc skips it
// rather than guessing.
func TestOrderEffectivePrice(t *testing.T) {
	limit := 250.0
	stop := 240.0
	zero := 0.0

	cases := []struct {
		name string
		o    *persistence.TradingOrder
		want float64
	}{
		{"nil order returns 0", nil, 0},
		{"LMT with limit_price", &persistence.TradingOrder{LimitPrice: &limit}, 250.0},
		{"STP with stop_price only", &persistence.TradingOrder{StopPrice: &stop}, 240.0},
		{"LMT preferred over STP when both present (STP_LMT shape)", &persistence.TradingOrder{LimitPrice: &limit, StopPrice: &stop}, 250.0},
		{"bare MKT (no prices) returns 0", &persistence.TradingOrder{OrderType: "MKT"}, 0},
		{"zero limit_price falls through to stop_price", &persistence.TradingOrder{LimitPrice: &zero, StopPrice: &stop}, 240.0},
		{"zero stop_price returns 0 when no limit", &persistence.TradingOrder{StopPrice: &zero}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, orderEffectivePrice(tc.o))
		})
	}
}
