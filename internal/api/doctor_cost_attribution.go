package api

// Doctor check that surfaces the cost-attribution source mix.
// Pairs with the vornik_api_cost_attribution_total counter
// shipped in a5bea19 — the counter alone is dashboard fodder;
// this check makes the operator backlog visible on every
// `vornikctl doctor` run so the per-project-API-key migration
// doesn't drift silently.

import (
	"fmt"
	"sort"
	"strings"

	dto "github.com/prometheus/client_model/go"
)

// costAttributionWarnFraction is the trustworthy-row share
// below which the check WARNs. 0.90 means "at least 90% of
// recent cost rows MUST come from DB-backed keys". Operator
// freshly enabling DB keys may sit below this until their
// clients migrate — the check stays WARNING (not ERROR) so
// it's actionable backlog, not boot-blocking.
const costAttributionWarnFraction = 0.90

// costAttributionMinTotal is the floor below which the check
// stays OK regardless of distribution. A daemon with three
// total external API calls shouldn't WARN — the sample is too
// small to draw conclusions. The threshold matches the typical
// "fresh deployment / test" range.
const costAttributionMinTotal = 10

// checkCostAttribution reports the per-source distribution of
// the vornik_api_cost_attribution_total counter:
//
//	OK      — metrics unwired, total below threshold, or
//	          key-bound fraction ≥ 0.90.
//	WARNING — key-bound fraction < 0.90, with the headline
//	          metric (key-bound %, header count, fallback
//	          count, anonymous count) in the message + the
//	          remediation steps as Items.
func (h *DoctorHandlers) checkCostAttribution() DoctorCheck {
	name := "cost_attribution_source_mix"
	if h.apiMetrics == nil || h.apiMetrics.CostAttributionTotal == nil {
		return DoctorCheck{Name: name, Status: "OK", Message: "API metrics not wired, skipping"}
	}
	counts := readCostAttributionCounts(h.apiMetrics)
	total := counts.total()
	if total < costAttributionMinTotal {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("%d cost-attribution sample(s) since boot — below the %d-row floor; check is informational only.", total, costAttributionMinTotal),
		}
	}
	keyBoundFrac := float64(counts.keyBound) / float64(total)
	if keyBoundFrac >= costAttributionWarnFraction {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("%.0f%% of %d cost rows came from DB-backed API keys (key-bound: %d, header: %d, fallback: %d, anonymous: %d).", keyBoundFrac*100, total, counts.keyBound, counts.header, counts.fallback, counts.anonymous),
		}
	}
	items := []string{
		"Create per-project API keys via /ui/projects/{id}/keys or `vornikctl key create --project <id>`.",
		"Update client configurations to use the new sk-vornik-… prefixed keys.",
		"Header-supplied attribution is unauditable: a buggy client can route under any project; switch to DB-backed keys to make it tamper-resistant.",
	}
	if counts.fallback > 0 {
		items = append(items, fmt.Sprintf("%d row(s) hit the daemon-wide ExternalAPIBillingProjectID fallback — every static-key caller pools onto one project.", counts.fallback))
	}
	if counts.anonymous > 0 {
		items = append(items, fmt.Sprintf("%d row(s) attributed to the _external sentinel — no project derivable. Inspect the journal for 'external API call attributed to _external' warns.", counts.anonymous))
	}
	sort.Strings(items)
	return DoctorCheck{
		Name:   name,
		Status: "WARNING",
		Message: fmt.Sprintf(
			"only %.0f%% of %d cost rows came from a DB-backed key (key-bound: %d, header: %d, fallback: %d, anonymous: %d). Drive the key-bound fraction to ≥ %.0f%% to retire the legacy attribution paths.",
			keyBoundFrac*100, total, counts.keyBound, counts.header, counts.fallback, counts.anonymous, costAttributionWarnFraction*100,
		),
		Items: items,
	}
}

// attributionCounts is the snapshot of the per-source counter
// read at doctor-check time. Each field maps to one
// AttributionSource label.
type attributionCounts struct {
	keyBound  uint64
	header    uint64
	fallback  uint64
	anonymous uint64
}

func (c attributionCounts) total() uint64 {
	return c.keyBound + c.header + c.fallback + c.anonymous
}

// readCostAttributionCounts harvests the per-source counter via
// Prometheus's per-metric Write() — the same path
// `testutil.ToFloat64` uses, lifted into production so the
// doctor handler doesn't have to depend on the test util.
func readCostAttributionCounts(m *APIMetrics) attributionCounts {
	out := attributionCounts{}
	if m == nil || m.CostAttributionTotal == nil {
		return out
	}
	collect := func(source AttributionSource, dst *uint64) {
		c, err := m.CostAttributionTotal.GetMetricWithLabelValues(string(source))
		if err != nil {
			return
		}
		var pb dto.Metric
		if err := c.Write(&pb); err != nil {
			return
		}
		if pb.Counter != nil && pb.Counter.Value != nil {
			*dst = uint64(*pb.Counter.Value)
		}
	}
	collect(AttributionFromDBKey, &out.keyBound)
	collect(AttributionFromHeader, &out.header)
	collect(AttributionFromFallback, &out.fallback)
	collect(AttributionAnonymous, &out.anonymous)
	return out
}

// Ensure the strings import isn't dropped by a future trimming
// pass — sort.Strings + this is the only string-shaped op.
var _ = strings.Join
