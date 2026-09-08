package forge

import (
	"context"
	"errors"
	"time"
)

// CI-outcome ingestion, provider-neutral
// (https://docs.vornik.io §7).
//
// The names avoid vendor nouns because the shape is not GitHub's: GitLab
// pipelines and Gitea Actions are the same three questions — what did the run
// conclude, what did it upload, and give me one of those uploads.

// ErrCIUnsupported is what a provider without CI ingestion returns.
//
// Callers treat it as "no CI outcome" rather than as a failure: a GitLab
// project should not stop reviewing pull requests because the CI reader is not
// built yet. It is a sentinel rather than a bool so the reason survives to the
// log.
var ErrCIUnsupported = errors.New("forge: CI outcome ingestion not supported by this provider")

// CIRef identifies the CI run a ci_run.completed ForgeJob is about.
//
// A REFERENCE, never content: the run's artifact excerpt is deliberately
// absent (design §5.1). A job travels inside a task payload, which is persisted
// on the task row and rendered into prompts by machinery this type does not
// own, so untrusted bytes must not enter it. The excerpt is read later through
// forge.fetch_ci, which is the one place that wraps it.
type CIRef struct {
	RunID        int64  `json:"run_id"`
	HeadSHA      string `json:"head_sha"`
	Conclusion   string `json:"conclusion"`
	WorkflowName string `json:"workflow_name,omitempty"`
	WorkflowPath string `json:"workflow_path,omitempty"`
}

// CIRun is one completed CI run.
type CIRun struct {
	RunID        int64
	HeadSHA      string
	Number       int // pull request, or 0 — see ForgeCIOutcome.Number
	WorkflowName string
	WorkflowPath string
	RunAttempt   int
	Conclusion   string
	StartedAt    time.Time
	CompletedAt  time.Time
	Jobs         []CIJob
}

// CIJob is one job within a run.
type CIJob struct {
	Name       string
	Status     string
	Conclusion string
}

// CIArtifact is one file the run uploaded.
type CIArtifact struct {
	ID        int64
	Name      string
	SizeBytes int64
	Expired   bool
}

// CIReader is the optional half of a ForgeProvider: the ability to read what
// CI concluded.
//
// SEPARATE FROM ForgeProvider on purpose. Making these three methods part of
// the main interface would force every provider — including ones that will
// never grow CI support — to carry three stubs, and a stub that returns nil is
// how "not supported" becomes indistinguishable from "nothing ran". A caller
// type-asserts, and an assertion that fails is an honest, explicit absence.
type CIReader interface {
	// FetchCIRun returns the run's conclusion and its per-job breakdown.
	FetchCIRun(ctx context.Context, repo string, runID int64) (CIRun, error)

	// ListCIArtifacts names what the run uploaded. An expired artifact is
	// returned with Expired set rather than omitted, so a caller can say "the
	// plan existed but GitHub has already deleted it" instead of "no plan".
	ListCIArtifacts(ctx context.Context, repo string, runID int64) ([]CIArtifact, error)

	// FetchCIArtifact downloads one artifact.
	//
	// It MUST refuse anything larger than maxBytes BEFORE reading the body —
	// the size is known from the listing and from Content-Length, and a reader
	// that streams first and checks later has already paid the cost the cap
	// exists to avoid. Returns ErrCIArtifactTooLarge in that case.
	FetchCIArtifact(ctx context.Context, repo string, artifactID int64, maxBytes int64) ([]byte, error)

	// VerifyCIAccess reports whether the credentials can read CI outcomes
	// (GitHub: the App installation's `actions: read`). Called at boot for
	// every project with CI ingestion configured, so a missing permission
	// surfaces as a warning an operator can act on rather than as a 403 hours
	// later against the first completed run.
	VerifyCIAccess(ctx context.Context) error
}

// ErrCIArtifactTooLarge is returned when an artifact exceeds the caller's cap.
//
// Distinct from a fetch failure because the operator response differs: a
// too-large artifact means "raise the cap or upload less", while a failure
// means "something is wrong". Reported, never silently treated as absent.
var ErrCIArtifactTooLarge = errors.New("forge: CI artifact exceeds the configured size ceiling")
