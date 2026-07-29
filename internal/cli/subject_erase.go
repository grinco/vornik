package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/erasure"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/storage"
)

// `vornikctl subject erase` — GDPR Art 17, increment 5 slice 5b.
//
// Preview first, then confirm. Erasure is irreversible and "run it and find out"
// is not an acceptable interface for it, which is the same discipline
// `vornikctl erase artifact` already applies to a single artifact.

var subjectEraseCmd = &cobra.Command{
	Use:   "erase <request-id>",
	Short: "Carry out an Article 17 erasure for a verified request",
	Long: `Erase the personal data held about a subject, for a verified erasure request.

Shows the plan and asks for confirmation before destroying anything.

THE GROUND DECIDES WHAT HAPPENS TO SHARED RECORDS. A record that also concerns
another person cannot be half-deleted, so the Art 17(1) limb recorded at intake
selects the treatment:

  Art 17(1)(a)  no longer necessary          → shared records REDACTED
  Art 17(1)(b)  consent withdrawn            → shared records REDACTED
  Art 17(1)(c)  Art 21 objection             → shared records REDACTED
  Art 17(1)(d)  unlawfully processed         → shared records DELETED IN FULL
  Art 17(1)(e)  legal obligation to erase    → shared records DELETED IN FULL
  Art 17(1)(f)  child information services   → shared records REDACTED

Under (d) and (e) the controller has no discretion to keep any part, so another
person's context is lost too. Under the others, redaction honours this subject
while preserving theirs.

WHAT THIS SLICE CANNOT YET DO. Redaction of shared records needs an LLM pass over
the memory pipeline and is not implemented. Those records are reported as
DEFERRED and are NOT erased — the request is deliberately left un-actioned so it
still shows as live, because marking it complete while data remains would put a
false completion in the accountability ledger.

The report names what was retained under an exemption and why. A subject told
"erased" while records remain has been misled.`,
	Args: cobra.ExactArgs(1),
	RunE: runSubjectErase,
}

// erasureGroundHelp lists the valid --ground values for an error message.
func erasureGroundHelp() string {
	grounds := []datasubject.ErasureGround{
		datasubject.GroundNoLongerNecessary,
		datasubject.GroundConsentWithdrawn,
		datasubject.GroundObjection,
		datasubject.GroundUnlawfulProcessing,
		datasubject.GroundLegalObligation,
		datasubject.GroundChildServices,
	}
	var b strings.Builder
	for _, g := range grounds {
		note := ""
		if g.RemovesRetentionDiscretion() {
			note = "  [shared records deleted in full]"
		}
		fmt.Fprintf(&b, "  %-34s %s — %s%s\n", g, g.Article(), g.Label(), note)
	}
	return b.String()
}

// artifactRowStore adapts the artifact repository to erasure.ArtifactStore.
//
// The Art 17 path must remove the uploaded file and its row, not only the data
// derived from them: the upload is the most direct copy of the subject's data and
// its filename is often personal data by itself.
type artifactRowStore struct {
	repo *postgres.ArtifactRepository
}

func (a artifactRowStore) ArtifactStoragePath(ctx context.Context, id string) (string, error) {
	art, err := a.repo.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if art == nil {
		return "", nil
	}
	return art.StoragePath, nil
}

func (a artifactRowStore) DeleteArtifactRow(ctx context.Context, id string) error {
	return a.repo.Delete(ctx, id)
}

// cascadeEraser adapts the erasure service to datasubject.ArtifactEraser.
type cascadeEraser struct {
	svc *erasure.Service
}

func (c cascadeEraser) EraseArtifact(ctx context.Context, artifactID string) (int, error) {
	res, err := c.svc.EraseIncludingArtifact(ctx, artifactID)
	if err != nil {
		return 0, err
	}
	return res.ChunksDeleted, nil
}

func runSubjectErase(_ *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	deps, closeFn, err := openErasureDeps(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	req, err := deps.Repo.GetRequest(ctx, args[0])
	if err != nil {
		return err
	}
	plan, err := buildErasurePlan(ctx, deps.Repo, req)
	if err != nil {
		return err
	}

	printErasurePlan(plan)
	if len(plan.Actions) == 0 {
		fmt.Println("\nNothing identifiable about this subject was found — nothing to erase.")
		fmt.Println("The request is left open: record the outcome with 'subject refuse' or close it once")
		fmt.Println("the subject has been told what the search covered.")
		return nil
	}

	if !subjectEraseYes {
		fmt.Print("\nType 'erase' to proceed (this cannot be undone): ")
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(answer) != "erase" {
			fmt.Println("Aborted. Nothing was deleted.")
			return nil
		}
	}

	res, execErr := newSubjectExecutor(deps).Execute(ctx, plan)
	if res != nil {
		printErasureResult(res)
		if err := writeErasureReport(ctx, plan, res, &req, deps.Repo); err != nil {
			return err
		}
	}
	return execErr
}

func printErasurePlan(p *datasubject.ErasurePlan) {
	fmt.Printf("Erasure plan for subject %s (request %s)\n", p.SubjectID, p.RequestID)
	fmt.Printf("Ground: %s — %s\n", p.GroundCite, p.Ground.Label())
	if p.Ground.RemovesRetentionDiscretion() {
		fmt.Println("        this ground leaves NO discretion to retain, so records that also")
		fmt.Println("        concern other people are deleted in full, not redacted.")
	}
	fmt.Printf("\n%d record(s): %d to delete, %d to redact\n",
		len(p.Actions), p.DeleteCount(), p.RedactCount())
	for _, a := range p.Actions {
		fmt.Printf("  %-8s %-24s %s\n", a.Disposition, a.Table, a.RowID)
	}
	if len(p.RetainedCategories) > 0 {
		fmt.Println("\nRetained under an exemption (NOT erased):")
		names := make([]string, 0, len(p.RetainedCategories))
		for t := range p.RetainedCategories {
			names = append(names, t)
		}
		sort.Strings(names)
		for _, t := range names {
			fmt.Printf("  %-24s %s\n", t, p.RetainedCategories[t])
		}
	}
}

func printErasureResult(r *datasubject.ErasureResult) {
	fmt.Printf("\nErased: %d row(s), %d artifact(s), %d derived memory chunk(s).\n",
		r.RowsDeleted, r.ArtifactsErased, r.DerivedChunksDeleted)
	for _, d := range r.Deferred {
		fmt.Printf("  DEFERRED  %s/%s — %s\n", d.Table, d.RowID, d.Reason)
	}
	for _, f := range r.Failed {
		fmt.Printf("  FAILED    %s/%s — %s\n", f.Table, f.RowID, f.Err)
	}
	if !r.Complete() {
		fmt.Println("\nThis erasure is INCOMPLETE. The request is left un-actioned so it still shows")
		fmt.Println("as live in 'subject requests' — marking it complete while data remains would")
		fmt.Println("record a false completion in the accountability ledger.")
	}
}

// writeErasureReport emits the report and, ONLY when the erasure completed,
// actions the request.
func writeErasureReport(
	ctx context.Context, plan *datasubject.ErasurePlan, res *datasubject.ErasureResult,
	req *datasubject.Request, repo *postgres.DataSubjectRepository,
) error {
	report := struct {
		Plan   *datasubject.ErasurePlan   `json:"plan"`
		Result *datasubject.ErasureResult `json:"result"`
	}{plan, res}

	blob, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	sum := sha256.Sum256(blob)
	hash := "sha256:" + hex.EncodeToString(sum[:])

	// Store the report ALWAYS, and before anything else — including for an
	// incomplete erasure, where it is the record of what was deferred and why.
	// --out is a convenience for handing a copy to the subject, never the only
	// place the evidence lives: a report that exists solely because someone
	// remembered a flag is not accountability.
	if err := repo.SaveRequestReport(ctx, req.ID, string(blob), hash); err != nil {
		return fmt.Errorf("store erasure report: %w", err)
	}
	fmt.Printf("\nreport stored against request %s (hash %s)\n", req.ID, hash)

	if subjectOutPath != "" {
		if err := os.WriteFile(subjectOutPath, blob, 0o600); err != nil {
			return err
		}
		fmt.Printf("copy written to %s\n", subjectOutPath)
	}

	if !res.Complete() {
		return nil
	}
	if err := req.Action(hash, time.Now().UTC()); err != nil {
		return err
	}
	if err := repo.SaveRequest(ctx, *req); err != nil {
		return err
	}
	fmt.Printf("request %s actioned\n", req.ID)
	return nil
}

// openErasureDeps wires the repository and resolves the containment root.
// erasureDeps is everything the erasure path needs from the environment.
type erasureDeps struct {
	Repo *postgres.DataSubjectRepository
	DB   *sql.DB
	Root string
}

func openErasureDeps(ctx context.Context) (*erasureDeps, func(), error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	closeFn := func() { _ = backend.Close() }
	db, err := requirePostgresDB(backend, "subject erase")
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	root := cfg.Storage.ArtifactsPath
	if strings.TrimSpace(root) == "" {
		// Without a containment root there is no boundary for the recursive
		// deletes the cascade performs. Refuse rather than guess.
		closeFn()
		return nil, nil, fmt.Errorf(
			"storage.artifacts_path is not configured; refusing to erase files without a containment root")
	}
	return &erasureDeps{
		Repo: postgres.NewDataSubjectRepository(db),
		DB:   db,
		Root: root,
	}, closeFn, nil
}

// newSubjectExecutor composes the row deleter with the full artifact cascade.
//
// Artifacts go through erasure.EraseIncludingArtifact, not a row delete: the
// cascade also removes the extraction rows, the derived memory chunks, the
// extraction storage directories, and the uploaded file itself. A plain row
// delete would orphan every one of those while reporting success.
func newSubjectExecutor(d *erasureDeps) *datasubject.Executor {
	return &datasubject.Executor{
		Rows: d.Repo,
		Artifacts: cascadeEraser{svc: &erasure.Service{
			Docs:         postgres.NewExtractedDocumentRepository(d.DB),
			Chunks:       memory.NewRepository(d.DB),
			Artifacts:    artifactRowStore{repo: postgres.NewArtifactRepository(d.DB)},
			ArtifactRoot: d.Root,
		}},
	}
}

// buildErasurePlan collects the subject's links and decides their treatment.
func buildErasurePlan(
	ctx context.Context, repo *postgres.DataSubjectRepository, req datasubject.Request,
) (*datasubject.ErasurePlan, error) {
	links, err := repo.ListLinks(ctx, req.SubjectID)
	if err != nil {
		return nil, err
	}
	items, err := repo.CollectItems(ctx, req.SubjectID, links)
	if err != nil {
		return nil, err
	}
	methods := map[string]bool{}
	for _, l := range links {
		methods[string(l.Source)] = true
	}
	methodList := make([]string, 0, len(methods))
	for m := range methods {
		methodList = append(methodList, m)
	}
	sort.Strings(methodList)
	return datasubject.PlanErasure(req, items, methodList)
}
