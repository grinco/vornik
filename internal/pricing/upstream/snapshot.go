// Package upstream compares vornik's hand-maintained pricing.yaml against a
// PINNED snapshot derived from LiteLLM's published price table.
//
// It exists because `pricing_coverage` can only answer "is there an entry?".
// A present-but-stale rate is invisible to every other check, and every rate
// drives cost metrics, budget enforcement and spend attribution. See
// https://docs.vornik.io,
// amendment 2026-09-17.
//
// Nothing here fetches at runtime. The snapshot is embedded, so an upstream
// edit cannot change what this daemon bills between one boot and the next;
// refreshing it is a deliberate act that produces a reviewable diff.
package upstream

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"

	"vornik.io/vornik/internal/pricing"
)

//go:embed snapshot.json
var embedded []byte

// Provenance records where the snapshot came from, precisely enough that the
// derivation can be reproduced and checked against the source it claims.
type Provenance struct {
	Repo        string `json:"repo"`
	Path        string `json:"path"`
	Commit      string `json:"commit"`
	CommittedAt string `json:"committedAt"`
	// SHA256 of the UPSTREAM file as fetched, not of the derived snapshot:
	// it is what makes "this was derived from that" checkable.
	SHA256     string `json:"sha256"`
	EntryCount int    `json:"upstreamEntryCount"`
	DerivedAt  string `json:"derivedAt"`
}

// Entry is one upstream price, converted to vornik's units (USD per 1M
// tokens). A nil *Entry means the id is ABSENT upstream — recorded explicitly
// so a report can state the denominator it actually examined.
type Entry struct {
	Input          float64  `json:"input"`
	Output         float64  `json:"output"`
	CacheRead      *float64 `json:"cacheRead,omitempty"`
	MaxInputTokens int      `json:"maxInputTokens,omitempty"`
}

// Snapshot is the vendored artifact: our ids, each with its upstream match or
// an explicit null.
type Snapshot struct {
	Provenance Provenance        `json:"provenance"`
	Models     map[string]*Entry `json:"models"`
}

// Rate is one of our own prices, in USD per 1M tokens.
type Rate struct {
	Input  float64
	Output float64
	// CacheRead is non-nil when pricing.yaml already carries the tier, so the
	// enrichment report does not suggest a field we already have.
	CacheRead *float64
}

// Total is how many of our ids the snapshot covers.
func (s Snapshot) Total() int { return len(s.Models) }

// Matched is how many of them upstream actually prices — the only ids a drift
// comparison can say anything about.
func (s Snapshot) Matched() int {
	n := 0
	for _, e := range s.Models {
		if e != nil {
			n++
		}
	}
	return n
}

// Drift is one disagreement, carrying BOTH numbers: upstream is not
// automatically right, and a report that hides our value invites replacing a
// verified rate with a worse one.
type Drift struct {
	ID             string
	OurInput       float64
	UpstreamInput  float64
	OurOutput      float64
	UpstreamOutput float64
	// Ratio is the larger of the two relative differences, for ordering.
	Ratio float64
}

// Enrichment is a field upstream has and we do not.
type Enrichment struct {
	ID             string
	CacheRead      float64
	MaxInputTokens int
}

// Report is the result of a comparison. Compared is the denominator: the
// number of ids that had an upstream price at all.
type Report struct {
	Compared           int
	Unmatched          int
	Drifted            []Drift
	CacheReadAvailable []Enrichment
}

// Compare reports how our table disagrees with the pinned snapshot. It decides
// nothing and writes nothing (C6): both numbers are returned so the operator
// picks. `threshold` is a relative difference, e.g. 0.10 for 10%.
func (s Snapshot) Compare(ours map[string]Rate, threshold float64) Report {
	var rep Report
	for id, rate := range ours {
		entry, pinned := s.Models[id]
		if !pinned || entry == nil {
			// Either the snapshot predates this id, or upstream has never
			// heard of it. Both are "cannot compare", never "agrees".
			rep.Unmatched++
			continue
		}
		rep.Compared++
		di := relDiff(rate.Input, entry.Input)
		do := relDiff(rate.Output, entry.Output)
		if worst := max(di, do); worst > threshold {
			rep.Drifted = append(rep.Drifted, Drift{
				ID: id, Ratio: worst,
				OurInput: rate.Input, UpstreamInput: entry.Input,
				OurOutput: rate.Output, UpstreamOutput: entry.Output,
			})
		}
		if entry.CacheRead != nil && rate.CacheRead == nil {
			rep.CacheReadAvailable = append(rep.CacheReadAvailable, Enrichment{
				ID: id, CacheRead: *entry.CacheRead, MaxInputTokens: entry.MaxInputTokens,
			})
		}
	}
	// Worst disagreement first; ties by id so the report is stable across runs
	// rather than following map order.
	sort.Slice(rep.Drifted, func(i, j int) bool {
		if rep.Drifted[i].Ratio != rep.Drifted[j].Ratio {
			return rep.Drifted[i].Ratio > rep.Drifted[j].Ratio
		}
		return rep.Drifted[i].ID < rep.Drifted[j].ID
	})
	sort.Slice(rep.CacheReadAvailable, func(i, j int) bool {
		return rep.CacheReadAvailable[i].ID < rep.CacheReadAvailable[j].ID
	})
	return rep
}

// relDiff is relative to the UPSTREAM value. A zero upstream price with a
// non-zero one of ours is a total disagreement, not a division by zero.
func relDiff(ours, upstream float64) float64 {
	if upstream == 0 {
		if ours == 0 {
			return 0
		}
		return 1
	}
	d := (ours - upstream) / upstream
	if d < 0 {
		d = -d
	}
	return d
}

// upstreamEntry is the subset of LiteLLM's schema this derivation reads.
type upstreamEntry struct {
	InputCostPerToken  *float64 `json:"input_cost_per_token"`
	OutputCostPerToken *float64 `json:"output_cost_per_token"`
	CacheReadCost      *float64 `json:"cache_read_input_token_cost"`
	MaxInputTokens     int      `json:"max_input_tokens"`
}

// perMillion converts upstream's per-token costs to vornik's units. Done once,
// here, rather than in every consumer.
const perMillion = 1e6

// Derive builds the snapshot for `ourIDs` from a decoded upstream table.
// Matching is EXACT — the same rule billing uses. An advisory fold would have
// to choose which upstream entry an ambiguous id meant, and
// `moonshotai/kimi-k2.6` matches three at three different prices.
func Derive(up map[string]json.RawMessage, ourIDs []string, prov Provenance) (Snapshot, error) {
	snap := Snapshot{Provenance: prov, Models: make(map[string]*Entry, len(ourIDs))}
	for _, id := range ourIDs {
		raw, ok := up[id]
		if !ok {
			snap.Models[id] = nil
			continue
		}
		var ue upstreamEntry
		if err := json.Unmarshal(raw, &ue); err != nil {
			return Snapshot{}, fmt.Errorf("decode upstream entry %q: %w", id, err)
		}
		if ue.InputCostPerToken == nil {
			// Present upstream but unpriced (embedding-only or metadata rows).
			// Not a match: there is nothing to compare against.
			snap.Models[id] = nil
			continue
		}
		e := &Entry{Input: *ue.InputCostPerToken * perMillion, MaxInputTokens: ue.MaxInputTokens}
		if ue.OutputCostPerToken != nil {
			e.Output = *ue.OutputCostPerToken * perMillion
		}
		if ue.CacheReadCost != nil {
			cr := *ue.CacheReadCost * perMillion
			e.CacheRead = &cr
		}
		snap.Models[id] = e
	}
	return snap, nil
}

// Load decodes the embedded snapshot.
func Load() (Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(embedded, &s); err != nil {
		return Snapshot{}, fmt.Errorf("decode embedded pricing snapshot: %w", err)
	}
	return s, nil
}

// DriftThreshold is the relative disagreement worth reporting. It lives here,
// not in each caller: the doctor check and the sync verb must not be able to
// report different findings about the same table. 10% separates the nine real
// disagreements measured 2026-09-17 (33%-85%, several exactly 2x) from
// rounding in a hand-entered table.
const DriftThreshold = 0.10

// RatesFrom projects a pricing table into the comparison's units, so the
// doctor check and the sync verb compare the same thing by construction.
func RatesFrom(table *pricing.Table) map[string]Rate {
	ids := table.IDs()
	out := make(map[string]Rate, len(ids))
	for _, id := range ids {
		entry, ok := table.Lookup(id)
		if !ok {
			continue
		}
		r := Rate{Input: entry.InputUSDPerMillion, Output: entry.OutputUSDPerMillion}
		if entry.CacheReadPerMillion > 0 {
			cr := entry.CacheReadPerMillion
			r.CacheRead = &cr
		}
		out[id] = r
	}
	return out
}
