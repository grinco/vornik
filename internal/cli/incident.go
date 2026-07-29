package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/incident"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/storage"
)

var (
	incFacts        string
	incEffects      string
	incRemedial     string
	incAssessedBy   string
	incAuthRisk     string
	incAuthReason   string
	incSubjRisk     string
	incSubjReason   string
	incReference    string
	incExemption    string
	incSubjectsDone bool
	incAwareAt      string
)

var incidentCmd = &cobra.Command{
	Use:   "incident",
	Short: "GDPR Article 33/34 personal-data-breach ledger",
	Long: `Record and discharge personal-data breaches.

Article 33(5) obliges you to DOCUMENT every personal-data breach — the facts,
the effects, and the remedial action — including breaches you correctly decide
not to notify. This ledger is that record; a written procedure is not.

The 72-hour Article 33(1) clock runs from when you BECAME AWARE, not from when
the breach happened. Both are recorded, and only the first drives the deadline.

Nothing here decides for you whether a breach is notifiable. A wrong automated
"no notification needed" would be a documented, confident, unlawful decision —
far worse than being asked. What is automated is the clock, the prompts, and the
refusal to let a decision go unrecorded.`,
}

var incidentOpenCmd = &cobra.Command{
	Use:   "open",
	Short: "Record a newly detected breach and start the 72-hour clock",
	Args:  cobra.NoArgs,
	RunE:  runIncidentOpen,
}

var incidentFactsCmd = &cobra.Command{
	Use:   "facts <incident-id>",
	Short: "Record the Article 33(5) facts, effects, and remedial action",
	Args:  cobra.ExactArgs(1),
	RunE:  runIncidentFacts,
}

var incidentAssessCmd = &cobra.Command{
	Use:   "assess <incident-id>",
	Short: "Record the risk judgement against both statutory thresholds",
	Long: `Record your risk assessment.

TWO DIFFERENT QUESTIONS, and conflating them is the common error:

  --authority-risk  Article 33: is a risk to rights and freedoms LIKELY?
                    If yes, the supervisory authority must be notified.
  --subject-risk    Article 34: is a HIGH risk likely?
                    If yes, the affected people must be told as well.

A breach can be notifiable to the authority and not to the subjects. Both
answers need a reason even when the answer is yes, because Article 33(5)
requires the assessment documented, not just its conclusion.`,
	Args: cobra.ExactArgs(1),
	RunE: runIncidentAssess,
}

var incidentNotifyCmd = &cobra.Command{
	Use:   "notify-authority <incident-id>",
	Short: "Record notification of the supervisory authority",
	Args:  cobra.ExactArgs(1),
	RunE:  runIncidentNotifyAuthority,
}

var incidentNotifySubjectsCmd = &cobra.Command{
	Use:   "notify-subjects <incident-id>",
	Short: "Record Article 34 communication to subjects, or the exemption relied on",
	Long: `Record how the Article 34 obligation was met.

Supply EITHER --done (you communicated with the affected people) OR --exemption
(the Article 34(3) ground you rely on instead). They are alternatives: recording
neither leaves the obligation open while appearing handled.`,
	Args: cobra.ExactArgs(1),
	RunE: runIncidentNotifySubjects,
}

var incidentNotNotifiableCmd = &cobra.Command{
	Use:   "not-notifiable <incident-id>",
	Short: "Record that Article 33 notification is not owed",
	Long: `Conclude that notification is not required.

This is an OUTCOME with a ground, not an absence of one — Article 33(1) makes
notification the default and non-notification the exception. It is only
available when your recorded assessment says no risk is likely.`,
	Args: cobra.ExactArgs(1),
	RunE: runIncidentNotNotifiable,
}

var incidentCloseCmd = &cobra.Command{
	Use:   "close <incident-id>",
	Short: "Close an incident whose obligations are discharged",
	Args:  cobra.ExactArgs(1),
	RunE:  runIncidentClose,
}

var incidentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List undischarged breaches with their Article 33 deadlines",
	Args:  cobra.NoArgs,
	RunE:  runIncidentList,
}

func init() {
	incidentOpenCmd.Flags().StringVar(&incAwareAt, "aware-at", "",
		"RFC3339 time you became aware (default: now) — this starts the 72-hour clock")
	incidentFactsCmd.Flags().StringVar(&incFacts, "facts", "", "what happened, what data, how many people (required)")
	incidentFactsCmd.Flags().StringVar(&incEffects, "effects", "", "likely consequences for those people (required)")
	incidentFactsCmd.Flags().StringVar(&incRemedial, "remedial", "", "measures taken or proposed (required)")
	incidentAssessCmd.Flags().StringVar(&incAssessedBy, "by", "", "who made the assessment (required)")
	incidentAssessCmd.Flags().StringVar(&incAuthRisk, "authority-risk", "", "yes|no — Art 33: is a risk likely (required)")
	incidentAssessCmd.Flags().StringVar(&incAuthReason, "authority-reason", "", "why (required)")
	incidentAssessCmd.Flags().StringVar(&incSubjRisk, "subject-risk", "", "yes|no — Art 34: is a HIGH risk likely (required)")
	incidentAssessCmd.Flags().StringVar(&incSubjReason, "subject-reason", "", "why (required)")
	incidentNotifyCmd.Flags().StringVar(&incReference, "reference", "", "the authority's case reference, if known")
	incidentNotifySubjectsCmd.Flags().BoolVar(&incSubjectsDone, "done", false, "subjects were communicated with")
	incidentNotifySubjectsCmd.Flags().StringVar(&incExemption, "exemption", "", "Art 34(3) ground relied on instead")

	incidentCmd.AddCommand(incidentOpenCmd, incidentFactsCmd, incidentAssessCmd,
		incidentNotifyCmd, incidentNotifySubjectsCmd, incidentNotNotifiableCmd,
		incidentCloseCmd, incidentListCmd)
	rootCmd.AddCommand(incidentCmd)
}

func openIncidentRepo(ctx context.Context) (*postgres.IncidentRepository, func(), error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	db, err := requirePostgresDB(backend, "incident")
	if err != nil {
		_ = backend.Close()
		return nil, nil, err
	}
	return postgres.NewIncidentRepository(db), func() { _ = backend.Close() }, nil
}

// withIncident loads an incident, applies a transition, and saves it. The
// transitions all validate in the domain type, so the CLI never re-implements a
// rule and cannot disagree with the ledger's own state machine.
func withIncident(id string, apply func(*incident.Incident) error) (incident.Incident, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, closeFn, err := openIncidentRepo(ctx)
	if err != nil {
		return incident.Incident{}, err
	}
	defer closeFn()

	inc, err := repo.Get(ctx, id)
	if err != nil {
		return incident.Incident{}, err
	}
	if err := apply(&inc); err != nil {
		return incident.Incident{}, err
	}
	if err := repo.Save(ctx, inc); err != nil {
		return incident.Incident{}, err
	}
	return inc, nil
}

func runIncidentOpen(_ *cobra.Command, _ []string) error {
	aware := time.Now().UTC()
	if strings.TrimSpace(incAwareAt) != "" {
		parsed, err := time.Parse(time.RFC3339, incAwareAt)
		if err != nil {
			return fmt.Errorf("--aware-at must be RFC3339: %w", err)
		}
		aware = parsed.UTC()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, closeFn, err := openIncidentRepo(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	inc := incident.Incident{
		ID:    "inc_" + time.Now().UTC().Format("20060102150405.000000"),
		State: incident.StateDetected, BecameAwareAt: aware,
	}
	if err := repo.Create(ctx, inc); err != nil {
		return err
	}
	fmt.Printf("incident recorded: %s\n", inc.ID)
	fmt.Printf("Article 33(1) notification deadline: %s (72h from awareness)\n",
		inc.Deadline().Format(time.RFC3339))
	fmt.Printf("next: vornikctl incident facts %s --facts ... --effects ... --remedial ...\n", inc.ID)
	return nil
}

func runIncidentFacts(_ *cobra.Command, args []string) error {
	inc, err := withIncident(args[0], func(i *incident.Incident) error {
		return i.RecordFacts(incFacts, incEffects, incRemedial)
	})
	if err != nil {
		return err
	}
	fmt.Printf("Article 33(5) record written for %s\n", inc.ID)
	fmt.Printf("next: vornikctl incident assess %s --by <you> --authority-risk yes|no --authority-reason ... "+
		"--subject-risk yes|no --subject-reason ...\n", inc.ID)
	return nil
}

func runIncidentAssess(_ *cobra.Command, args []string) error {
	inc, err := withIncident(args[0], func(i *incident.Incident) error {
		return i.Assess(incAssessedBy, incAuthRisk, incAuthReason, incSubjRisk, incSubjReason)
	})
	if err != nil {
		return err
	}
	fmt.Printf("assessment recorded for %s by %s\n", inc.ID, inc.AssessedBy)
	if inc.MustNotifyAuthority() {
		fmt.Printf("  Art 33: notification IS owed — due %s\n", inc.Deadline().Format(time.RFC3339))
		fmt.Printf("    vornikctl incident notify-authority %s --reference <case-ref>\n", inc.ID)
	} else {
		fmt.Printf("  Art 33: notification not owed on your assessment\n")
		fmt.Printf("    vornikctl incident not-notifiable %s\n", inc.ID)
	}
	if inc.MustNotifySubjects() {
		fmt.Printf("  Art 34: the affected people must be told (or an Art 34(3) ground recorded)\n")
		fmt.Printf("    vornikctl incident notify-subjects %s --done | --exemption <ground>\n", inc.ID)
	} else {
		fmt.Printf("  Art 34: no high risk found, so no communication to subjects is owed\n")
	}
	return nil
}

func runIncidentNotifyAuthority(_ *cobra.Command, args []string) error {
	inc, err := withIncident(args[0], func(i *incident.Incident) error {
		return i.NotifyAuthority(time.Now().UTC(), incReference)
	})
	if err != nil {
		return err
	}
	fmt.Printf("authority notification recorded for %s at %s\n",
		inc.ID, inc.NotifiedAuthorityAt.Format(time.RFC3339))
	return nil
}

func runIncidentNotifySubjects(_ *cobra.Command, args []string) error {
	at := time.Time{}
	if incSubjectsDone {
		at = time.Now().UTC()
	}
	inc, err := withIncident(args[0], func(i *incident.Incident) error {
		return i.NotifySubjects(at, incExemption)
	})
	if err != nil {
		return err
	}
	if inc.SubjectExemption != "" {
		fmt.Printf("Article 34(3) exemption recorded for %s: %s\n", inc.ID, inc.SubjectExemption)
	} else {
		fmt.Printf("communication to subjects recorded for %s\n", inc.ID)
	}
	return nil
}

func runIncidentNotNotifiable(_ *cobra.Command, args []string) error {
	inc, err := withIncident(args[0], func(i *incident.Incident) error {
		return i.MarkNotNotifiable()
	})
	if err != nil {
		return err
	}
	fmt.Printf("%s recorded as not notifiable: %s\n", inc.ID, inc.AuthorityRiskReason)
	fmt.Println("The Article 33(5) record stands regardless — that documentation obligation applies to")
	fmt.Println("every breach, including the ones correctly not notified.")
	return nil
}

func runIncidentClose(_ *cobra.Command, args []string) error {
	inc, err := withIncident(args[0], func(i *incident.Incident) error {
		return i.Close(time.Now().UTC())
	})
	if err != nil {
		return err
	}
	fmt.Printf("%s closed at %s\n", inc.ID, inc.ClosedAt.Format(time.RFC3339))
	return nil
}

func runIncidentList(_ *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, closeFn, err := openIncidentRepo(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	live, err := repo.ListLive(ctx)
	if err != nil {
		return err
	}
	if len(live) == 0 {
		fmt.Println("no undischarged personal-data breaches.")
		return nil
	}
	now := time.Now().UTC()
	fmt.Printf("%-30s %-11s %-22s %s\n", "INCIDENT", "STATE", "ART 33 DEADLINE", "STATUS")
	for _, i := range live {
		status := "in time"
		switch {
		case i.Overdue(now):
			status = "OVERDUE — the 72-hour window has closed"
		case i.NeedsAttention(now):
			status = "DUE SOON — assess and decide now"
		}
		fmt.Printf("%-30s %-11s %-22s %s\n",
			i.ID, i.State, i.Deadline().Format(time.RFC3339), status)
	}
	return nil
}
