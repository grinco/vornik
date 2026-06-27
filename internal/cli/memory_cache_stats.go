package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var (
	memoryCacheStatsJSON bool
)

var memoryCacheStatsCmd = &cobra.Command{
	Use:   "cache-stats",
	Short: "Show LLM cache effectiveness (embedding + response)",
	Long: `Report row counts, lifetime hits, $ saved, and on-disk size for the
embedding cache (Phase D — keyed on content_hash+model) and the
response cache (Phase E — keyed on model+purpose+prompt). Both
caches must be enabled in the daemon config to populate; either
or both may render as "disabled" on a fresh deployment.

Example:
  vornikctl memory cache-stats
  vornikctl memory cache-stats --json`,
	RunE: runMemoryCacheStats,
}

func init() {
	memoryCacheStatsCmd.Flags().BoolVar(&memoryCacheStatsJSON, "json", false, "JSON output instead of the short-form table")
	memoryCmd.AddCommand(memoryCacheStatsCmd)
}

// cacheStatsResponse mirrors the daemon's /api/v1/memory/cache-stats
// payload. Kept in this file because no other CLI command consumes
// the shape.
type cacheStatsResponse struct {
	EmbeddingCache struct {
		Enabled        bool  `json:"enabled"`
		RowCount       int64 `json:"row_count"`
		ApproxBytes    int64 `json:"approx_bytes"`
		DistinctModels int   `json:"distinct_models"`
	} `json:"embedding_cache"`
	ResponseCache struct {
		Enabled          bool    `json:"enabled"`
		RowCount         int64   `json:"row_count"`
		ApproxBytes      int64   `json:"approx_bytes"`
		DistinctPurposes int     `json:"distinct_purposes"`
		TotalHits        int64   `json:"total_hits"`
		TotalSavingsUSD  float64 `json:"total_savings_usd"`
	} `json:"response_cache"`
}

func runMemoryCacheStats(cmd *cobra.Command, args []string) error {
	raw, err := fetchJSON("/api/v1/memory/cache-stats")
	if err != nil {
		return err
	}
	if memoryCacheStatsJSON {
		return prettyPrintJSON(raw)
	}
	var parsed cacheStatsResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CACHE\tSTATUS\tROWS\tHITS\t$ SAVED\tSIZE\tNOTE")

	// Embedding cache row. No savings figure today (Phase D doesn't
	// thread pricing through the embedder); shown as "—" so the
	// column lines up visually.
	embedStatus, embedNote := cacheStatusLabel(parsed.EmbeddingCache.Enabled, parsed.EmbeddingCache.RowCount)
	embedSize := humanBytes(parsed.EmbeddingCache.ApproxBytes)
	_, _ = fmt.Fprintf(tw, "embedding\t%s\t%d\t%s\t%s\t%s\t%s\n",
		embedStatus,
		parsed.EmbeddingCache.RowCount,
		"—",
		"—",
		embedSize,
		distinctModelsNote(embedNote, parsed.EmbeddingCache.DistinctModels))

	// Response cache row. Savings + hits are the headline columns.
	respStatus, respNote := cacheStatusLabel(parsed.ResponseCache.Enabled, parsed.ResponseCache.RowCount)
	respSize := humanBytes(parsed.ResponseCache.ApproxBytes)
	savings := "—"
	if parsed.ResponseCache.TotalSavingsUSD > 0 {
		savings = fmt.Sprintf("$%.2f", parsed.ResponseCache.TotalSavingsUSD)
	} else if parsed.ResponseCache.Enabled {
		savings = "$0.00"
	}
	hitsCol := "—"
	if parsed.ResponseCache.Enabled {
		hitsCol = fmt.Sprintf("%d", parsed.ResponseCache.TotalHits)
	}
	_, _ = fmt.Fprintf(tw, "response\t%s\t%d\t%s\t%s\t%s\t%s\n",
		respStatus,
		parsed.ResponseCache.RowCount,
		hitsCol,
		savings,
		respSize,
		distinctPurposesNote(respNote, parsed.ResponseCache.DistinctPurposes))

	if err := tw.Flush(); err != nil {
		return err
	}
	return nil
}

// cacheStatusLabel renders the "Enabled + populated" / "Enabled but
// empty" / "Disabled" status the column shows. Returned note carries
// any context-dependent message the per-row distinctNote helpers
// might extend.
func cacheStatusLabel(enabled bool, rowCount int64) (status, note string) {
	if !enabled {
		return "disabled", ""
	}
	if rowCount == 0 {
		return "enabled", "no rows yet"
	}
	return "enabled", ""
}

func distinctModelsNote(prefix string, n int) string {
	if n > 1 {
		extra := fmt.Sprintf("%d models — embedder may have swapped", n)
		if prefix == "" {
			return extra
		}
		return prefix + "; " + extra
	}
	return prefix
}

func distinctPurposesNote(prefix string, n int) string {
	if n > 0 {
		extra := fmt.Sprintf("%d purposes", n)
		if prefix == "" {
			return extra
		}
		return prefix + "; " + extra
	}
	return prefix
}

// humanBytes formats bytes as B / KB / MB / GB matching the
// /ui/spend tile's fmtBytes shape. Kept inline to avoid pulling in
// the UI package or duplicating the standard "byte humanize" helper.
func humanBytes(n int64) string {
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
	)
	switch {
	case n < kb:
		return fmt.Sprintf("%d B", n)
	case n < mb:
		return fmt.Sprintf("%.1f KB", float64(n)/kb)
	case n < gb:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	}
}
