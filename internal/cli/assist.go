package cli

// `vornikctl assist <project> "<intent>"` — the configuration assistant's
// operator CLI entrypoint (2026-09-13 design §6.3 entrypoint 1). One call = one intent
// = one reviewable proposal (or a named refusal). It never applies
// anything itself: review with `vornikctl control-plane show <id>` and
// decide with `control-plane approve` / `apply`.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

const assistAPITimeout = 10 * time.Minute

var (
	assistCmd = &cobra.Command{
		Use:   "assist <project> <intent...>",
		Short: "Ask the configuration assistant for a reviewable config change",
		Long: `Turns a natural-language request about a project's autonomy settings,
swarm or workflows into a proposal in the control-plane ledger. The
assistant edits a private, secret-sanitized copy of the config tree,
validates the result with the daemon's own loaders, classifies its blast
radius (A tuning … E authority), has a separate judge model check it, and
files a proposal for you to approve. Class C/D/E never auto-apply; class E
needs a human and shows the effective permission diff.

Needs an explicit operator capability (admin.allowed_keys key or admin
session) and the config-assistant feature enabled.`,
		Args: cobra.MinimumNArgs(2),
		RunE: runAssist,
	}
	assistJSONOut        bool
	assistIdempotencyKey string
)

func init() {
	assistCmd.Flags().BoolVar(&assistJSONOut, "json", false, "print the raw result")
	assistCmd.Flags().StringVar(&assistIdempotencyKey, "idempotency-key", "", "replay-safe key: a retry with the same key returns the same proposal")
	rootCmd.AddCommand(assistCmd)
}

type assistResultWire struct {
	RequestID string `json:"request_id"`
	Proposal  *struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Status      string `json:"status"`
		BlastRadius string `json:"blast_radius"`
	} `json:"proposal"`
	Refusal *struct {
		Code     string   `json:"Code"`
		Message  string   `json:"Message"`
		Findings []string `json:"Findings"`
	} `json:"refusal"`
	Class   string `json:"class"`
	Verdict *struct {
		Judged   bool   `json:"judged"`
		Decision string `json:"decision"`
		Summary  string `json:"summary"`
		Reason   string `json:"reason"`
	} `json:"verdict"`
	AutoApplied bool     `json:"auto_applied"`
	Diff        string   `json:"diff"`
	Summary     string   `json:"summary"`
	HasEvidence bool     `json:"has_measurement"`
	Ignored     []string `json:"ignored_deletions"`
	Duplicate   bool     `json:"duplicate"`
	Permissions []struct {
		Subject string   `json:"subject"`
		Gains   []string `json:"gains"`
		Loses   []string `json:"loses"`
	} `json:"permission_diff"`
}

//nolint:gocognit,funlen // Output handling mirrors the API envelope so each optional section stays in order.
func runAssist(_ *cobra.Command, args []string) error {
	project := args[0]
	intent := strings.TrimSpace(strings.Join(args[1:], " "))
	client := assistClientFromEnv()
	resp, err := client.Do(http.MethodPost, "/api/v1/operator/assist", map[string]any{
		"projectId": project, "intent": intent, "idempotencyKey": assistIdempotencyKey,
	})
	if err != nil {
		return fmt.Errorf("assist: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return ParseAPIError(resp)
	}
	var out assistResultWire
	raw, rerr := decodeAndKeepRaw(resp, &out)
	if rerr != nil {
		return rerr
	}
	if assistJSONOut {
		fmt.Println(string(raw))
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintf(tw, "request\t%s\n", out.RequestID); err != nil {
		return err
	}
	if out.Refusal != nil {
		if _, err := fmt.Fprintf(tw, "refused\t%s\n\t%s\n", out.Refusal.Code, out.Refusal.Message); err != nil {
			return err
		}
		for _, f := range out.Refusal.Findings {
			if _, err := fmt.Fprintf(tw, "\t- %s\n", f); err != nil {
				return err
			}
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		return &silentExit{}
	}
	if out.Proposal != nil {
		if _, err := fmt.Fprintf(tw, "proposal\t%s\t%s\n", out.Proposal.ID, out.Proposal.Status); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(tw, "title\t%s\n", out.Proposal.Title); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(tw, "class\t%s\n", out.Class); err != nil {
		return err
	}
	if out.Verdict != nil {
		if out.Verdict.Judged {
			if _, err := fmt.Fprintf(tw, "judge\t%s\t%s\n", out.Verdict.Decision, out.Verdict.Summary); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(tw, "judge\tNOT JUDGED\t%s\n", out.Verdict.Reason); err != nil {
				return err
			}
		}
	}
	if !out.HasEvidence {
		if _, err := fmt.Fprintln(tw, "evidence\tnone — a requested change, not an evidence-driven one"); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(tw, "auto-applied\t%v\n", out.AutoApplied); err != nil {
		return err
	}
	if out.Duplicate {
		if _, err := fmt.Fprintln(tw, "note\tidempotent replay: this is the proposal an earlier call filed"); err != nil {
			return err
		}
	}
	for _, d := range out.Ignored {
		if _, err := fmt.Fprintf(tw, "ignored deletion\t%s\n", d); err != nil {
			return err
		}
	}
	for _, p := range out.Permissions {
		if _, err := fmt.Fprintf(tw, "permission diff\t%s gains %s; loses %s\n", p.Subject, strings.Join(p.Gains, ","), strings.Join(p.Loses, ",")); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if out.Summary != "" {
		fmt.Println("\n" + out.Summary)
	}
	if out.Diff != "" {
		fmt.Println("\n" + out.Diff)
	}
	if out.Proposal != nil && !out.AutoApplied {
		fmt.Printf("\nreview: vornikctl control-plane show %s   approve: vornikctl control-plane approve %s\n", out.Proposal.ID, out.Proposal.ID)
	}
	return nil
}

func assistClientFromEnv() *Client {
	client := ClientFromEnv()
	if client.httpClient != nil && client.httpClient.Timeout == DefaultAPITimeout {
		client.httpClient.Timeout = assistAPITimeout
	}
	return client
}

// decodeAndKeepRaw decodes JSON into v and returns the raw bytes.
func decodeAndKeepRaw(resp *http.Response, v any) ([]byte, error) {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return raw, nil
}

// silentExit makes a refusal exit non-zero without the "error: " prefix
// (main.go suppresses it for an empty Error()).
type silentExit struct{}

func (silentExit) Error() string { return "" }
