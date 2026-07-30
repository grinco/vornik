package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/storage"
)

var (
	subjectName          string
	subjectIDKind        string
	subjectIDValue       string
	subjectRequestKind   string
	subjectVerifiedBy    string
	subjectVerifiedHow   string
	subjectRefuseWhy     string
	subjectOutPath       string
	subjectEraseApply    bool
	subjectRewriteModel  string
	subjectLinkTable     string
	subjectLinkRow       string
	subjectLinkProject   string
	subjectLinkShared    bool
	subjectErasureGround string
	subjectEraseYes      bool
)

var subjectCmd = &cobra.Command{
	Use:   "subject",
	Short: "GDPR data-subject rights (access, and the request ledger)",
	Long: `Exercise and record GDPR data-subject rights.

A data subject is a person this deployment holds data about. The subject axis
records WHICH person a row of personal data concerns, which is what makes
Articles 15, 16, 17 and 20 answerable per-person rather than by hand-searching
free text.

Identity verification is a hard gate, not a formality: producing an access
export for an unverified requester discloses that person's data to whoever
asked, so the request mechanism is itself an attack surface. Nothing can be
exported until 'subject verify' has recorded who established identity and how.`,
}

var subjectCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Register a data subject",
	Args:  cobra.NoArgs,
	RunE:  runSubjectCreate,
}

var subjectIdentifyCmd = &cobra.Command{
	Use:   "identify <subject-id>",
	Short: "Record an identifier for a subject (operator-asserted)",
	Long: `Record something that identifies this subject — an email address, a
channel handle, a user id.

Recorded with source 'operator_asserted': you are asserting the link, so it is
trusted but attributable rather than derived. Automatic binding from
authenticated sessions and email envelopes carries its own provenance.`,
	Args: cobra.ExactArgs(1),
	RunE: runSubjectIdentify,
}

var subjectLinkCmd = &cobra.Command{
	Use:   "link <subject-id>",
	Short: "Record that a subject appears in a specific row",
	Long: `Link a subject to a row of personal data.

--table must name one of the tables the export and erasure executors know how
to act on; anything else is refused rather than stored, so a link can never
point somewhere nothing can read.

--shared marks a row that ALSO concerns other people. Its content is then
withheld from an access export under Article 15(4). If you are unsure, pass
--shared: the unknown case is treated as shared, because guessing the other way
discloses one person's data to another.`,
	Args: cobra.ExactArgs(1),
	RunE: runSubjectLink,
}

var subjectRequestCmd = &cobra.Command{
	Use:   "request <subject-id>",
	Short: "Open a rights request and start the Article 12(3) clock",
	Args:  cobra.ExactArgs(1),
	RunE:  runSubjectRequest,
}

var subjectVerifyCmd = &cobra.Command{
	Use:   "verify <request-id>",
	Short: "Record that the requester's identity is established",
	Long: `Clear the identity gate for a request.

Both --by and --how are required. An unattributable verification is
indistinguishable from none, and the question after an incident is who let a
request through and on what evidence.

If you cannot identify the requester, do NOT verify — refuse the request with
'subject refuse', citing Article 12(6). That is the lawful answer, and it is a
response the subject can challenge rather than silence.`,
	Args: cobra.ExactArgs(1),
	RunE: runSubjectVerify,
}

var subjectRefuseCmd = &cobra.Command{
	Use:   "refuse <request-id>",
	Short: "Refuse a request, with a stated ground",
	Args:  cobra.ExactArgs(1),
	RunE:  runSubjectRefuse,
}

var subjectExportCmd = &cobra.Command{
	Use:   "export <request-id>",
	Short: "Produce the Article 15 / 20 report for a verified request",
	Long: `Assemble the report for a verified access or portability request.

Records that also concern other people are LISTED but their content is withheld
under Article 15(4) — handing over a record naming three people because one of
them asked would trade one obligation for two breaches.

The report states the limits of its own search. It covers everything the
deployment could identify as being about the subject; it is not a guarantee that
no further data exists, and the subject is entitled to know that.`,
	Args: cobra.ExactArgs(1),
	RunE: runSubjectExport,
}

var subjectRequestsCmd = &cobra.Command{
	Use:   "requests",
	Short: "List live requests with their Article 12(3) deadlines",
	Args:  cobra.NoArgs,
	RunE:  runSubjectRequests,
}

func init() {
	subjectCreateCmd.Flags().StringVar(&subjectName, "name", "", "operator-facing label for the subject (required)")
	subjectIdentifyCmd.Flags().StringVar(&subjectIDKind, "kind", "", "identifier kind: email | channel | user_id | operator_id (required)")
	subjectIdentifyCmd.Flags().StringVar(&subjectIDValue, "value", "", "identifier value (required)")
	subjectRequestCmd.Flags().StringVar(&subjectRequestKind, "kind", "access", "access | portability | erasure | rectification | restriction | objection")
	subjectVerifyCmd.Flags().StringVar(&subjectVerifiedBy, "by", "", "who established identity (required)")
	subjectVerifyCmd.Flags().StringVar(&subjectVerifiedHow, "how", "", "how identity was established (required)")
	subjectRefuseCmd.Flags().StringVar(&subjectRefuseWhy, "reason", "", "the ground for refusal (required)")
	subjectExportCmd.Flags().StringVar(&subjectOutPath, "out", "", "write the report here instead of stdout")
	subjectRequestCmd.Flags().StringVar(&subjectErasureGround, "ground", "",
		"Art 17(1) ground, required for --kind erasure (see 'subject erase --help')")
	subjectEraseCmd.Flags().StringVar(&subjectOutPath, "out", "", "write the erasure report here instead of stdout")
	subjectEraseCmd.Flags().BoolVar(&subjectEraseApply, "apply", false,
		"skip per-record review of proposed redactions (AUDITED: every record committed "+
			"this way is stamped review_bypassed in the request record)")
	subjectEraseCmd.Flags().StringVar(&subjectRewriteModel, "rewrite-model", "",
		"model used to rewrite shared records (defaults to the chat section's model)")
	subjectEraseCmd.Flags().BoolVar(&subjectEraseYes, "yes", false,
		"skip the confirmation prompt (erasure is irreversible)")
	subjectLinkCmd.Flags().StringVar(&subjectLinkTable, "table", "", "table the row lives in (required)")
	subjectLinkCmd.Flags().StringVar(&subjectLinkRow, "row", "", "row id (required)")
	subjectLinkCmd.Flags().StringVar(&subjectLinkProject, "project", "", "project id, for project-scoped tables")
	subjectLinkCmd.Flags().BoolVar(&subjectLinkShared, "shared", false,
		"the row also concerns other people (content withheld under Art 15(4))")

	subjectCmd.AddCommand(subjectCreateCmd, subjectIdentifyCmd, subjectLinkCmd, subjectRequestCmd,
		subjectVerifyCmd, subjectRefuseCmd, subjectExportCmd, subjectEraseCmd, subjectRequestsCmd)
	rootCmd.AddCommand(subjectCmd)
}

// openSubjectRepo wires the repository, or explains why it cannot.
func openSubjectRepo(ctx context.Context) (*postgres.DataSubjectRepository, func(), error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	db, err := requirePostgresDB(backend, "subject")
	if err != nil {
		_ = backend.Close()
		return nil, nil, err
	}
	return postgres.NewDataSubjectRepository(db), func() { _ = backend.Close() }, nil
}

func runSubjectCreate(_ *cobra.Command, _ []string) error {
	if strings.TrimSpace(subjectName) == "" {
		return fmt.Errorf("--name is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, closeFn, err := openSubjectRepo(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	s := datasubject.Subject{ID: newSubjectID(), DisplayName: subjectName}
	if err := repo.CreateSubject(ctx, s); err != nil {
		return err
	}
	fmt.Printf("data subject registered: %s (%s)\n", s.ID, s.DisplayName)
	fmt.Printf("next: vornikctl subject identify %s --kind email --value someone@example.com\n", s.ID)
	return nil
}

func runSubjectIdentify(_ *cobra.Command, args []string) error {
	if strings.TrimSpace(subjectIDKind) == "" || strings.TrimSpace(subjectIDValue) == "" {
		return fmt.Errorf("--kind and --value are required")
	}
	value := subjectIDValue
	if subjectIDKind == datasubject.KindEmail {
		norm, err := datasubject.NormaliseEmail(value)
		if err != nil {
			return err
		}
		value = norm
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, closeFn, err := openSubjectRepo(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	// An identifier already claimed by a different subject is a merge, and a
	// wrong merge discloses one person's data to another — refuse and let a
	// human decide.
	if existing, err := repo.FindSubjectByIdentifier(ctx, subjectIDKind, value); err != nil {
		return err
	} else if existing != "" && existing != args[0] {
		return fmt.Errorf("identifier %s=%s already belongs to subject %s; "+
			"merging two subjects would disclose one person's data to another — resolve this deliberately",
			subjectIDKind, value, existing)
	}

	id := datasubject.Identifier{
		Kind: subjectIDKind, Value: value,
		Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceCertain,
	}
	if err := repo.AddIdentifier(ctx, args[0], id); err != nil {
		return err
	}
	fmt.Printf("identifier recorded: %s=%s (source=operator_asserted)\n", id.Kind, id.Value)
	return nil
}

func runSubjectLink(_ *cobra.Command, args []string) error {
	if strings.TrimSpace(subjectLinkTable) == "" || strings.TrimSpace(subjectLinkRow) == "" {
		return fmt.Errorf("--table and --row are required")
	}
	excl := datasubject.ExclusiveRow
	if subjectLinkShared {
		excl = datasubject.SharedRow
	}
	link := datasubject.Link{
		Table: datasubject.LinkableTable(subjectLinkTable), RowID: subjectLinkRow,
		ProjectID:  subjectLinkProject,
		Source:     datasubject.SourceOperatorAsserted,
		Confidence: datasubject.ConfidenceCertain,
		// An operator asserting a link may still be unsure whether the row
		// concerns others; --shared is how they say so, and Validate refuses a
		// table outside the closed set before anything is stored.
		Exclusivity: excl,
	}
	if err := link.Validate(); err != nil {
		return fmt.Errorf("%w\n\nlinkable tables: %s", err, strings.Join(datasubject.LinkableTables(), ", "))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, closeFn, err := openSubjectRepo(ctx)
	if err != nil {
		return err
	}
	defer closeFn()
	if err := repo.AddLink(ctx, args[0], link); err != nil {
		return err
	}
	fmt.Printf("linked %s row %s to subject %s (%s)\n", link.Table, link.RowID, args[0], link.Exclusivity)
	return nil
}

func runSubjectRequest(_ *cobra.Command, args []string) error {
	kind := datasubject.RequestKind(subjectRequestKind)
	switch kind {
	case datasubject.RequestAccess, datasubject.RequestPortability, datasubject.RequestErasure,
		datasubject.RequestRectification, datasubject.RequestRestriction, datasubject.RequestObjection:
	default:
		return fmt.Errorf("unknown request kind %q", subjectRequestKind)
	}
	// The Art 17 ground is captured HERE, at intake, because it is a fact about
	// what the subject asked for — and because it decides whether a record that
	// also concerns somebody else is redacted or destroyed. Choosing it later, at
	// execution time, would turn a statutory limb into an operator preference.
	ground := datasubject.ErasureGround(subjectErasureGround)
	if kind == datasubject.RequestErasure {
		if err := ground.Validate(); err != nil {
			return fmt.Errorf("%w\n\nPass --ground with one of:\n%s", err, erasureGroundHelp())
		}
	} else if strings.TrimSpace(subjectErasureGround) != "" {
		return fmt.Errorf("--ground applies only to an erasure request, not %q", kind)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, closeFn, err := openSubjectRepo(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	req := datasubject.Request{
		ID: newRequestID(), SubjectID: args[0], Kind: kind,
		State: datasubject.StateOpen, OpenedAt: time.Now().UTC(),
		ErasureGround: ground,
	}
	if err := repo.CreateRequest(ctx, req); err != nil {
		return err
	}
	fmt.Printf("request opened: %s (%s) for subject %s\n", req.ID, req.Kind, req.SubjectID)
	fmt.Printf("Article 12(3) response due: %s\n", req.Deadline().Format(time.RFC3339))
	fmt.Printf("next: establish identity, then\n  vornikctl subject verify %s --by <you> --how <evidence>\n", req.ID)
	return nil
}

func runSubjectVerify(_ *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, closeFn, err := openSubjectRepo(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	req, err := repo.GetRequest(ctx, args[0])
	if err != nil {
		return err
	}
	if err := req.Verify(subjectVerifiedBy, subjectVerifiedHow, time.Now().UTC()); err != nil {
		return err
	}
	if err := repo.SaveRequest(ctx, req); err != nil {
		return err
	}
	fmt.Printf("request %s verified by %q (%s)\n", req.ID, req.VerifiedBy, req.VerifiedHow)
	return nil
}

func runSubjectRefuse(_ *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, closeFn, err := openSubjectRepo(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	req, err := repo.GetRequest(ctx, args[0])
	if err != nil {
		return err
	}
	if err := req.Refuse(subjectRefuseWhy); err != nil {
		return err
	}
	if err := repo.SaveRequest(ctx, req); err != nil {
		return err
	}
	fmt.Printf("request %s refused: %s\n", req.ID, req.RefusedReason)
	fmt.Println("Tell the subject this ground — Article 12(4) requires it, and an unexplained refusal reads as obstruction.")
	return nil
}

func runSubjectExport(_ *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	repo, closeFn, err := openSubjectRepo(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	req, err := repo.GetRequest(ctx, args[0])
	if err != nil {
		return err
	}
	links, err := repo.ListLinks(ctx, req.SubjectID)
	if err != nil {
		return err
	}
	items, err := repo.CollectItems(ctx, req.SubjectID, links)
	if err != nil {
		return err
	}
	methods := map[string]bool{}
	for _, l := range links {
		methods[string(l.Source)] = true
	}
	methodList := make([]string, 0, len(methods))
	for m := range methods {
		methodList = append(methodList, m)
	}

	exp, err := datasubject.BuildExport(req, items, methodList)
	if err != nil {
		return err
	}
	ids, err := repo.ListIdentifiers(ctx, req.SubjectID)
	if err != nil {
		return err
	}
	exp.Identifiers = ids

	// Belt: assert the Art 15(4) property on the finished artefact before it
	// leaves the process. The leak is silent and the report looks plausible
	// either way, so it is checked rather than assumed.
	if exp.LeaksForeignContent(items) {
		return fmt.Errorf("refusing to emit the report: it would disclose content from a record " +
			"that also concerns another person (Art 15(4)) — this is a bug, please report it")
	}

	blob, err := json.MarshalIndent(exp, "", "  ")
	if err != nil {
		return err
	}
	sum := sha256.Sum256(blob)
	hash := "sha256:" + hex.EncodeToString(sum[:])

	if subjectOutPath != "" {
		if err := os.WriteFile(subjectOutPath, blob, 0o600); err != nil {
			return err
		}
		fmt.Printf("report written to %s\n", subjectOutPath)
	} else {
		fmt.Println(string(blob))
	}

	if err := req.Action(hash, time.Now().UTC()); err != nil {
		return err
	}
	if err := repo.SaveRequest(ctx, req); err != nil {
		return err
	}
	fmt.Printf("\nrequest %s actioned; report hash %s\n", req.ID, hash)
	fmt.Printf("%d item(s); %d withheld under Art 15(4)\n", len(exp.Items), countWithheld(exp))
	return nil
}

func countWithheld(e *datasubject.Export) int {
	n := 0
	for _, it := range e.Items {
		if it.Withheld != "" {
			n++
		}
	}
	return n
}

func runSubjectRequests(_ *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, closeFn, err := openSubjectRepo(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	reqs, err := repo.ListLiveRequests(ctx)
	if err != nil {
		return err
	}
	if len(reqs) == 0 {
		fmt.Println("no live rights requests.")
		return nil
	}
	now := time.Now().UTC()
	fmt.Printf("%-28s %-14s %-10s %-22s %s\n", "REQUEST", "KIND", "STATE", "DUE (Art 12(3))", "STATUS")
	for _, r := range reqs {
		status := "in time"
		switch {
		case r.Overdue(now):
			status = "OVERDUE — the response deadline has passed"
		case r.NeedsAttention(now):
			status = "DUE SOON — act now"
		}
		fmt.Printf("%-28s %-14s %-10s %-22s %s\n",
			r.ID, r.Kind, r.State, r.Deadline().Format(time.RFC3339), status)
	}
	return nil
}

func newSubjectID() string { return "ds_" + time.Now().UTC().Format("20060102150405.000000") }
func newRequestID() string { return "dsr_" + time.Now().UTC().Format("20060102150405.000000") }
