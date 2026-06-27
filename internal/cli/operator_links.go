package cli

// `vornikctl operator link / unlink / show-links` —
// cross-channel identity-link CLI. Backs the daemon's
// /api/v1/operators/{id}/links endpoints.
//
//   vornikctl operator link <canonical> <channel-speaker-id>
//   vornikctl operator unlink <channel-speaker-id>
//   vornikctl operator show-links <operator-id>
//
// The canonical id is whichever speaker the operator picks as
// the "primary" — typically the one with more accumulated
// profile content. Subsequent dispatcher reads + writes
// involving the linked speaker resolve to the canonical row,
// so an operator sees one profile across Telegram + webchat
// + Slack + future channels.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var (
	operatorLinkLinkedBy  string
	operatorLinkJSON      bool
	operatorShowLinksJSON bool

	operatorLinkCmd = &cobra.Command{
		Use:   "link <canonical-id> <channel-speaker-id>",
		Short: "Link <channel-speaker-id> to a canonical operator id (consolidate profiles)",
		Long: `Add an identity-link row so reads / writes against the channel
speaker id resolve to the canonical operator id. After link, the
dispatcher's per-turn profile lookup sees one row for both ids.

Example:
  vornikctl operator link webchat:abc-hash telegram:42

Reads for telegram:42 now return the webchat:abc-hash profile.`,
		Args: cobra.ExactArgs(2),
		RunE: runOperatorLink,
	}

	operatorUnlinkCmd = &cobra.Command{
		Use:   "unlink <channel-speaker-id>",
		Short: "Drop one identity-link row",
		Long: `Remove the link from <channel-speaker-id> to its canonical operator id.
The canonical profile + other links stay intact.

For full revocation use 'vornikctl operator forget <canonical-id>'
which drops every link to that operator plus the profile itself.`,
		Args: cobra.ExactArgs(1),
		RunE: runOperatorUnlink,
	}

	operatorShowLinksCmd = &cobra.Command{
		Use:   "show-links <operator-id>",
		Short: "List every speaker id that resolves to <operator-id>",
		Args:  cobra.ExactArgs(1),
		RunE:  runOperatorShowLinks,
	}
)

func init() {
	operatorLinkCmd.Flags().StringVar(&operatorLinkLinkedBy, "linked-by", "cli", "Who authorised the link (recorded; one of self / cli / auto)")
	operatorLinkCmd.Flags().BoolVar(&operatorLinkJSON, "json", false, "Output JSON instead of human-readable")
	operatorShowLinksCmd.Flags().BoolVar(&operatorShowLinksJSON, "json", false, "Output JSON instead of table")
	operatorCmd.AddCommand(operatorLinkCmd)
	operatorCmd.AddCommand(operatorUnlinkCmd)
	operatorCmd.AddCommand(operatorShowLinksCmd)
}

// operatorLinkEntry mirrors api.OperatorLinkJSON; local shape so
// the CLI doesn't import the api package.
type operatorLinkEntry struct {
	ChannelSpeakerID string `json:"channel_speaker_id"`
	OperatorID       string `json:"operator_id"`
	LinkedAt         string `json:"linked_at"`
	LinkedBy         string `json:"linked_by"`
}

type operatorLinksResponse struct {
	Links []operatorLinkEntry `json:"links"`
}

func runOperatorLink(cmd *cobra.Command, args []string) error {
	canonical := strings.TrimSpace(args[0])
	speaker := strings.TrimSpace(args[1])
	if canonical == "" || speaker == "" {
		return fmt.Errorf("operator link: both ids must be non-empty")
	}
	if canonical == speaker {
		return fmt.Errorf("operator link: canonical and channel-speaker ids must differ (a self-link is meaningless)")
	}
	body := map[string]string{
		"channel_speaker_id": speaker,
		"linked_by":          strings.TrimSpace(operatorLinkLinkedBy),
	}
	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/operators/"+url.PathEscape(canonical)+"/links", body)
	if err != nil {
		return fmt.Errorf("operator link: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return ParseAPIError(resp)
	}
	var row operatorLinkEntry
	if err := json.NewDecoder(resp.Body).Decode(&row); err != nil {
		return fmt.Errorf("operator link: decode response: %w", err)
	}
	if operatorLinkJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(row)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Linked %s → %s (linked_by=%s).\n",
		row.ChannelSpeakerID, row.OperatorID, row.LinkedBy)
	return nil
}

func runOperatorUnlink(cmd *cobra.Command, args []string) error {
	speaker := strings.TrimSpace(args[0])
	if speaker == "" {
		return fmt.Errorf("operator unlink: channel-speaker-id required")
	}
	// We need the canonical id for the URL — look it up first.
	client := ClientFromEnv()
	// The API expects /operators/{canonical}/links/{speaker}; we
	// don't know the canonical id, so do a GET via the speaker's
	// own /operators/{speaker}/links endpoint won't work either.
	// Instead, the API allows DELETE on /operators/<canonical>/links/<speaker>
	// with the canonical being authoritative. For unlink, we
	// route through a special discovery: GET /operators/<speaker>
	// returns 404 if it's not a canonical id (just a link target);
	// in that case we look up its canonical via the link table.
	//
	// Simpler: the daemon accepts the speaker id as the canonical
	// path segment so we don't need a discovery round-trip — the
	// link row is keyed on the channel_speaker_id (PK), so the
	// URL only needs the speaker.
	resp, err := client.Do(http.MethodDelete,
		"/api/v1/operators/_/links/"+url.PathEscape(speaker), nil)
	if err != nil {
		return fmt.Errorf("operator unlink: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return ParseAPIError(resp)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Unlinked %s.\n", speaker)
	return nil
}

func runOperatorShowLinks(cmd *cobra.Command, args []string) error {
	id := strings.TrimSpace(args[0])
	if id == "" {
		return fmt.Errorf("operator show-links: operator id required")
	}
	client := ClientFromEnv()
	resp, err := client.Get("/api/v1/operators/" + url.PathEscape(id) + "/links")
	if err != nil {
		return fmt.Errorf("operator show-links: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	var out operatorLinksResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("operator show-links: decode response: %w", err)
	}
	if operatorShowLinksJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if len(out.Links) == 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No links pointing at %s.\n", id)
		return nil
	}
	sort.Slice(out.Links, func(i, j int) bool {
		return out.Links[i].LinkedAt < out.Links[j].LinkedAt
	})
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CHANNEL SPEAKER ID\tOPERATOR ID\tLINKED AT\tLINKED BY")
	for _, l := range out.Links {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", l.ChannelSpeakerID, l.OperatorID, l.LinkedAt, l.LinkedBy)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nTotal: %d link(s).\n", len(out.Links))
	return nil
}
