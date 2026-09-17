package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/pricing/upstream"
)

var (
	pricingSyncPricingPath  string
	pricingSyncSnapshotPath string
	pricingSyncCommit       string
)

const (
	pricingUpstreamRepo = "BerriAI/litellm"
	pricingUpstreamFile = "model_prices_and_context_window.json"
)

var pricingCmd = &cobra.Command{
	Use:   "pricing",
	Short: "Inspect and maintain the model pricing table",
}

var pricingSyncUpstreamCmd = &cobra.Command{
	Use:   "sync-upstream",
	Short: "Re-derive the pinned LiteLLM price snapshot and print what changed",
	Long: `Re-derive the pinned upstream price snapshot that ` + "`doctor pricing_drift`" + ` compares against.

This FETCHES from the network and rewrites a file in the source tree. It
applies nothing to pricing.yaml: upstream is not automatically right — 9 of the
50 ids that match disagree by more than 10%, and the largest disagreement is an
operator-verified vendor rate that upstream gets wrong by 4.6x.

The daemon never fetches. The snapshot is embedded at build time precisely so an
upstream edit cannot change what this deployment bills, or throttle a budget,
between one boot and the next. Refreshing is a deliberate act that produces a
diff a human reviews and commits.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		table, err := pricing.Load(pricingSyncPricingPath)
		if err != nil {
			return fmt.Errorf("load %s: %w", pricingSyncPricingPath, err)
		}
		ourIDs := table.IDs()

		commit := pricingSyncCommit
		var committedAt string
		if commit == "" {
			if commit, committedAt, err = resolveUpstreamHead(cmd.Context()); err != nil {
				return err
			}
		}

		raw, err := fetchUpstream(cmd.Context(), commit)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)

		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return fmt.Errorf("decode upstream table: %w", err)
		}

		snap, err := upstream.Derive(decoded, ourIDs, upstream.Provenance{
			Repo:        pricingUpstreamRepo,
			Path:        pricingUpstreamFile,
			Commit:      commit,
			CommittedAt: committedAt,
			SHA256:      hex.EncodeToString(sum[:]),
			EntryCount:  len(decoded),
			DerivedAt:   time.Now().UTC().Format("2006-01-02"),
		})
		if err != nil {
			return err
		}

		out, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			return fmt.Errorf("encode snapshot: %w", err)
		}
		out = append(out, '\n')
		if err := os.WriteFile(pricingSyncSnapshotPath, out, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", pricingSyncSnapshotPath, err)
		}

		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"wrote %s\n  upstream %s@%s (%d entries, sha256 %s)\n  our ids %d, matched upstream %d (%.0f%%)\n",
			pricingSyncSnapshotPath, pricingUpstreamRepo, commit[:min(12, len(commit))],
			len(decoded), hex.EncodeToString(sum[:])[:12],
			snap.Total(), snap.Matched(), 100*float64(snap.Matched())/float64(max(1, snap.Total())))

		rep := snap.Compare(upstream.RatesFrom(table), upstream.DriftThreshold)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", formatDriftReport(rep, snap))
		_, _ = fmt.Fprintln(cmd.OutOrStdout(),
			"\nNothing was applied. Review the diff, then commit the snapshot.")
		return nil
	},
}

func resolveUpstreamHead(ctx context.Context) (commit, committedAt string, err error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/commits?path=%s&per_page=1",
		pricingUpstreamRepo, pricingUpstreamFile)
	body, err := httpGet(ctx, url)
	if err != nil {
		return "", "", fmt.Errorf("resolve upstream HEAD: %w", err)
	}
	var commits []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &commits); err != nil {
		return "", "", fmt.Errorf("decode commit list: %w", err)
	}
	if len(commits) == 0 {
		return "", "", fmt.Errorf("no commits found for %s", pricingUpstreamFile)
	}
	return commits[0].SHA, commits[0].Commit.Committer.Date, nil
}

func fetchUpstream(ctx context.Context, commit string) ([]byte, error) {
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s",
		pricingUpstreamRepo, commit, pricingUpstreamFile)
	body, err := httpGet(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("fetch upstream table: %w", err)
	}
	return body, nil
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// formatDriftReport renders a comparison for a human. Shared with the doctor
// check's item list so the two cannot describe the same finding differently.
func formatDriftReport(rep upstream.Report, snap upstream.Snapshot) string {
	s := fmt.Sprintf("compared %d of %d priced ids against the pinned snapshot "+
		"(%d have no upstream entry and are NOT verified here)\n",
		rep.Compared, snap.Total(), rep.Unmatched)
	if len(rep.Drifted) == 0 {
		s += "  no price disagrees by more than 10%\n"
	}
	for _, d := range rep.Drifted {
		s += fmt.Sprintf("  DRIFT %-38s ours in/out %.4f/%.4f  upstream %.4f/%.4f  (%.0f%%)\n",
			d.ID, d.OurInput, d.OurOutput, d.UpstreamInput, d.UpstreamOutput, d.Ratio*100)
	}
	if n := len(rep.CacheReadAvailable); n > 0 {
		s += fmt.Sprintf("  %d id(s) have an upstream cache_read rate this table does not carry\n", n)
		for _, e := range rep.CacheReadAvailable {
			s += fmt.Sprintf("    + %-38s cache_read %.4f  context %d\n", e.ID, e.CacheRead, e.MaxInputTokens)
		}
	}
	return s
}

func init() {
	pricingSyncUpstreamCmd.Flags().StringVar(&pricingSyncPricingPath, "pricing", "configs/pricing.yaml",
		"the pricing table whose ids define the snapshot")
	pricingSyncUpstreamCmd.Flags().StringVar(&pricingSyncSnapshotPath, "out",
		"internal/pricing/upstream/snapshot.json", "where to write the derived snapshot")
	pricingSyncUpstreamCmd.Flags().StringVar(&pricingSyncCommit, "commit", "",
		"pin this upstream commit instead of resolving the file's current HEAD")
	pricingCmd.AddCommand(pricingSyncUpstreamCmd)
	rootCmd.AddCommand(pricingCmd)
}
