package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/storage"
)

// `vornikctl subject resolve-kg` — GDPR increment 4, the knowledge-graph binder,
// RESOLVED ON DEMAND (controller decision 2026-07-29).
//
// The rejected alternative was to materialise every KG-derived PERSON entity as
// a data-subject row, giving each named third party a standing subject. That
// would build a permanent register of identified people out of LLM extraction
// output — increasing the personal data held in order to make deletion
// possible. So resolution happens here, when a request has actually named
// someone, and never as a background sweep.
//
// PREVIEW, THEN NAME WHAT IS REAL. The binder's confidence ceiling is
// `possible`: it has false positives (an entity carrying a person's name that
// is not that person) and false negatives (a nickname the extractor never
// linked). Running it without an --entity shows candidates and writes nothing;
// only an entity id a human named is bound. On the erasure path an auto-linked
// false positive deletes a third party's data — a breach committed while
// honouring a right.

var (
	subjectKGNames    []string
	subjectKGProjects []string
	subjectKGEntities []string
	subjectKGAdopt    bool
)

var subjectResolveKGCmd = &cobra.Command{
	Use:   "resolve-kg <subject-id>",
	Short: "Find and bind the knowledge-graph entities that denote this subject",
	Long: `Resolve a subject against the knowledge graph, so records that merely NAME
them are reachable by an access export or an erasure.

Run with no --entity to PREVIEW. Nothing is written: you get every PERSON entity
whose canonical name or alias contains one of this subject's names, with how
many records mentions it, and who (if anyone) already holds it.

Re-run with --entity <id> for each entity that really is this person. Links are
written at 'possible' confidence — this binder is the widest net available and
the least reliable, and every report says so.

WHY YOU PICK, AND NOT THE DAEMON. A name search cannot tell one Jane Doe from
another, and an entity carrying a person's name may not be a person at all.
Linking a wrong entity to an erasure request destroys someone else's data while
honouring this subject's rights, so the judgement is yours and nothing is bound
without it.

--name adds a name to search for. Use it for a nickname or a maiden name the
extractor never linked to the canonical entity: that false negative is the known
limit of this binder, and you are the backstop for it.

COLLISIONS. An entity may already be bound to the placeholder subject the chat
path mints (display name 'kg:<entity-id>') when a shared note named this person.
--adopt moves that placeholder's links onto this subject and removes it, so one
person does not keep two subject rows. An entity bound to another IDENTIFIED
subject is refused outright and --adopt does not override it: two named people
claiming one entity is a question about people, and merging them would disclose
one person's data to another.`,
	Args: cobra.ExactArgs(1),
	RunE: runSubjectResolveKG,
}

func init() {
	subjectResolveKGCmd.Flags().StringArrayVar(&subjectKGNames, "name", nil,
		"extra name to search for (repeatable) — a nickname or former name the extractor never linked")
	subjectResolveKGCmd.Flags().StringArrayVar(&subjectKGProjects, "project", nil,
		"limit the search to these projects (repeatable); default is every project with a graph")
	subjectResolveKGCmd.Flags().StringArrayVar(&subjectKGEntities, "entity", nil,
		"bind this entity id to the subject (repeatable); without it the command only previews")
	subjectResolveKGCmd.Flags().BoolVar(&subjectKGAdopt, "adopt", false,
		"fold a placeholder 'kg:<entity-id>' subject into this one, moving its links")
	subjectCmd.AddCommand(subjectResolveKGCmd)
}

// openKGResolver wires the resolver over the subject repository and the graph.
func openKGResolver(ctx context.Context) (*datasubject.KGResolver, func(), error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	db, err := requirePostgresDB(backend, "subject resolve-kg")
	if err != nil {
		_ = backend.Close()
		return nil, nil, err
	}
	resolver := &datasubject.KGResolver{
		Store: postgres.NewDataSubjectRepository(db),
		Index: postgres.NewDataSubjectKGIndex(db),
	}
	return resolver, func() { _ = backend.Close() }, nil
}

func runSubjectResolveKG(_ *cobra.Command, args []string) error {
	subjectID := args[0]
	// Generous: a resolve over every project's graph is a handful of indexed
	// queries per name, but an operator with many projects should not have a
	// timeout decide how much of their obligation gets discharged.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	resolver, closeFn, err := openKGResolver(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	if len(subjectKGEntities) == 0 {
		return previewKGCandidates(ctx, resolver, subjectID)
	}
	return bindKGEntities(ctx, resolver, subjectID)
}

// previewKGCandidates prints the proposals and writes nothing.
func previewKGCandidates(ctx context.Context, resolver *datasubject.KGResolver, subjectID string) error {
	cands, err := resolver.Candidates(ctx, subjectID, subjectKGNames, subjectKGProjects)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		fmt.Printf("No PERSON entity matches this subject's names.\n\n")
		fmt.Printf("That is a real answer, not an error: the graph may hold nothing about them, or\n")
		fmt.Printf("they may appear only under a name it never linked. Try --name with a nickname,\n")
		fmt.Printf("a former name, or a shorter form. Nothing was written.\n")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ENTITY\tPROJECT\tNAME\tMATCHED ON\tRECORDS\tSTATE")
	for _, c := range cands {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
			c.Entity.ID, c.Entity.ProjectID, truncateName(c.Entity.CanonicalName),
			c.MatchedOn, c.MentionCount, describeKGState(c))
	}
	_ = w.Flush()

	fmt.Printf("\nNOTHING WAS WRITTEN. These are candidates at 'possible' confidence — the widest\n")
	fmt.Printf("net this deployment has and the least reliable one. A name search cannot tell one\n")
	fmt.Printf("person from another of the same name, and an entity carrying a person's name may\n")
	fmt.Printf("not be a person at all.\n\n")
	fmt.Printf("Bind the ones that really are this subject:\n\n")
	fmt.Printf("  vornikctl subject resolve-kg %s", subjectID)
	for _, n := range subjectKGNames {
		fmt.Printf(" --name %q", n)
	}
	for _, c := range cands {
		if c.State != datasubject.KGConflict {
			fmt.Printf(" --entity %s", c.Entity.ID)
		}
	}
	fmt.Printf("\n")
	if anyPlaceholder(cands) {
		fmt.Printf("\nAdd --adopt to fold the placeholder subject(s) above into this one.\n")
	}
	if anyConflict(cands) {
		fmt.Printf("\nEntities held by another identified subject are omitted from that line and\n")
		fmt.Printf("cannot be bound here. Reconcile the two subjects by hand first.\n")
	}
	return nil
}

// bindKGEntities binds each named entity, reporting each outcome. One failure
// does not abandon the rest: a partly-discharged request the operator can see
// is better than an all-or-nothing that stops at the first refusal.
func bindKGEntities(ctx context.Context, resolver *datasubject.KGResolver, subjectID string) error {
	var failures []string
	for _, entityID := range subjectKGEntities {
		res, err := resolver.Bind(ctx, datasubject.KGBindRequest{
			SubjectID: subjectID,
			EntityID:  entityID,
			Names:     subjectKGNames,
			Projects:  subjectKGProjects,
			Adopt:     subjectKGAdopt,
		})
		if err != nil {
			fmt.Printf("REFUSED %s: %v\n", entityID, err)
			failures = append(failures, entityID)
			continue
		}
		fmt.Printf("bound %s: %d record(s) linked", entityID, res.LinksAdded)
		if res.AdoptedFrom != "" {
			fmt.Printf(", %d moved from placeholder %s (removed)", res.LinksMoved, res.AdoptedFrom)
		}
		fmt.Printf("\n")
		if res.MentionsTruncated {
			fmt.Printf("  NOTE: %s has more records than one run links. Re-run this command to\n"+
				"  pick up the rest — the coverage above is partial until it reports fewer.\n", entityID)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d entities were not bound: %s",
			len(failures), len(subjectKGEntities), strings.Join(failures, ", "))
	}
	fmt.Printf("\nLinks are recorded at 'possible' confidence and marked as shared rows: an access\n")
	fmt.Printf("export LISTS them and withholds their content under Art 15(4), and an erasure\n")
	fmt.Printf("treats them under the shared-record rule for the request's Art 17(1) ground.\n")
	return nil
}

// describeKGState renders a candidate's binding state for the preview.
func describeKGState(c datasubject.KGCandidate) string {
	switch c.State {
	case datasubject.KGBoundHere:
		return "already bound here"
	case datasubject.KGBoundToPlaceholder:
		return fmt.Sprintf("placeholder %s — needs --adopt", c.BoundSubjectID)
	case datasubject.KGConflict:
		return fmt.Sprintf("HELD BY %s (%q) — resolve by hand", c.BoundSubjectID, c.BoundSubjectName)
	default:
		return "free"
	}
}

func anyPlaceholder(cands []datasubject.KGCandidate) bool {
	for _, c := range cands {
		if c.State == datasubject.KGBoundToPlaceholder {
			return true
		}
	}
	return false
}

func anyConflict(cands []datasubject.KGCandidate) bool {
	for _, c := range cands {
		if c.State == datasubject.KGConflict {
			return true
		}
	}
	return false
}

// truncateName keeps the preview table readable without hiding which person a
// row is about — the first 48 characters of a name are always the deciding part.
func truncateName(s string) string {
	const limit = 48
	if len(s) <= limit {
		return s
	}
	return s[:limit-1] + "…"
}
