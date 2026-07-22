package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/persistence"
)

// `vornikctl web-write resolve <submission_id> --submitted|--failed` — the
// operator-recovery command for supervised web-write actions (LLD
// §Components.6 / N3). When a submission is stuck in `submitting` (the daemon
// crashed or lost the scraper reply mid-submit) or `unknown` (the outcome could
// not be determined), the operator verifies what actually happened on the site
// and records the terminal outcome here.
//
// It NEVER re-submits — the one-time approval token was already consumed at the
// approved→submitting CAS. This only transitions the persisted row to its true
// terminal state (`submitted` or `failed`) so downstream state/audit is
// accurate. persistence.WebWriteRepo.Resolve CASes on
// `status IN ('unknown','submitting')`, so a row in any other state is refused
// with a friendly message rather than silently no-op'd.

var (
	webWriteResolveSubmitted bool
	webWriteResolveFailed    bool
)

var webWriteCmd = &cobra.Command{
	Use:   "web-write",
	Short: "Operate supervised web-write actions (web_submit)",
	Long: `Operator surface for the supervised web-write primitive (web_submit).

Web writes are previewed, approved in the authenticated /inbox, and submitted
with outgoing-request interception. When a submission gets stuck mid-flight,
'resolve' records the verified terminal outcome.`,
}

var webWriteResolveCmd = &cobra.Command{
	Use:   "resolve <submission_id>",
	Short: "Record the terminal outcome of a stuck web-write submission",
	Long: `Transition a stuck web-write submission to its verified terminal state.

Only submissions still in the 'submitting' or 'unknown' state are resolvable —
these are the rows where the daemon could not confirm the outcome itself. The
operator inspects the target site, confirms whether the form actually went
through, and records it:

  vornikctl web-write resolve <submission_id> --submitted   # it did go through
  vornikctl web-write resolve <submission_id> --failed      # it did not

Exactly one of --submitted / --failed is required. This command NEVER
re-submits the form (the approval token was already consumed); it only records
the outcome and writes an audit row.`,
	Args: cobra.ExactArgs(1),
	RunE: runWebWriteResolve,
}

func init() {
	webWriteResolveCmd.Flags().BoolVar(&webWriteResolveSubmitted, "submitted", false,
		"Record the submission as successfully submitted")
	webWriteResolveCmd.Flags().BoolVar(&webWriteResolveFailed, "failed", false,
		"Record the submission as failed")

	webWriteCmd.AddCommand(webWriteResolveCmd)
	rootCmd.AddCommand(webWriteCmd)
}

func runWebWriteResolve(cmd *cobra.Command, args []string) error {
	submissionID := args[0]
	status, err := webWriteResolveStatus(webWriteResolveSubmitted, webWriteResolveFailed)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := openVornikDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	repo := persistence.NewWebWriteRepo(db)
	if err := resolveWebWrite(ctx, repo, submissionID, status); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "submission %s resolved as %s\n", submissionID, status)
	return nil
}

// webWriteResolveStatus maps the mutually-exclusive --submitted/--failed flags
// to the terminal status. Exactly one must be set; neither or both is a usage
// error (the two states are opposites, so silently picking one would risk
// recording the wrong outcome for a side-effecting action).
func webWriteResolveStatus(submitted, failed bool) (string, error) {
	switch {
	case submitted && failed:
		return "", errors.New("pass exactly one of --submitted or --failed, not both")
	case submitted:
		return "submitted", nil
	case failed:
		return "failed", nil
	default:
		return "", errors.New("one of --submitted or --failed is required")
	}
}

// resolveWebWrite performs the operator-recovery transition, translating the
// repo's ErrNoTransition (0 rows matched the `status IN ('unknown','submitting')`
// guard) into a friendly, actionable message. Split out from the cobra RunE so
// it's unit-testable against a fake WebWriteRepo.
func resolveWebWrite(ctx context.Context, repo persistence.WebWriteRepo, submissionID, status string) error {
	if err := repo.Resolve(ctx, submissionID, status); err != nil {
		if errors.Is(err, persistence.ErrNoTransition) {
			return fmt.Errorf("submission %s is not in a resolvable state (must be submitting/unknown); "+
				"nothing was changed", submissionID)
		}
		return fmt.Errorf("resolve submission %s: %w", submissionID, err)
	}
	return nil
}
