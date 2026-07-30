package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/erasure"
	"vornik.io/vornik/internal/persistence/postgres"
)

// Wiring for slice 5c — Art 17 redaction of records that also concern other people.
//
// Everything here is assembled per invocation rather than held on a long-lived
// container: an erasure is a rare, operator-initiated, irreversible act, and the cost
// of building a client for it is irrelevant next to the cost of getting it wrong.
//
// see LLD § https://docs.vornik.io §5, §8

// identifierSource adapts the repository to datasubject.IdentifierSource.
//
// The repository returns rich Identifier records; verification needs only their
// values. Kept as an adapter rather than changing the repository signature, because
// the request record wants the full rows and only the check wants the strings.
type identifierSource struct {
	repo *postgres.DataSubjectRepository
}

func (s identifierSource) Identifiers(ctx context.Context, subjectID string) ([]string, error) {
	rows, err := s.repo.ListIdentifiers(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if v := strings.TrimSpace(r.Value); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		// Fail loudly rather than returning an empty set. VerifyRedaction refuses an
		// empty identifier list anyway, but the error here names the actual problem
		// instead of surfacing as an unexplained deferral on every shared record.
		return nil, fmt.Errorf("subject %s has no recorded identifiers, so no rewrite could "+
			"be verified; add the identifiers to the request before erasing", subjectID)
	}
	return out, nil
}

// newRedactDeps assembles the redaction capability, or explains why it cannot.
//
// Returns (nil, nil) when redaction is simply not configured — the executor then
// DEFERS every shared record with a recorded reason, which is the honest outcome and
// exactly the 5b behaviour. An error is returned only when redaction was configured
// but is broken, because silently degrading a misconfigured erasure to "deferred"
// would look like a policy decision rather than a fault.
func newRedactDeps(cfg *config.Config, d *erasureDeps) (*datasubject.RedactDeps, error) {
	provider, model := newRewriteProvider(cfg)
	if provider == nil {
		return nil, nil
	}
	rw, err := erasure.NewRewriter(provider, model)
	if err != nil {
		return nil, err
	}
	deps := &datasubject.RedactDeps{
		Redactor:           postgres.NewChunkRedactorRepository(d.DB),
		Rewriter:           rw,
		Identifiers:        identifierSource{repo: d.Repo},
		ApplyWithoutReview: subjectEraseApply,
	}
	if !subjectEraseApply {
		deps.Approve = reviewRedactionOnTerminal
	}
	return deps, nil
}

// newRewriteProvider builds the LLM client used for rewrites.
//
// Only the plain OpenAI-compatible HTTP client is supported here, deliberately. The
// daemon's router, CLI-subscription and queued providers carry per-project routing,
// breakers and concurrency limits that exist for agent traffic; a one-shot operator
// command wants none of that, and reproducing it would put erasure behaviour at the
// mercy of unrelated routing config. An operator using a router setup pins an
// endpoint-capable model with --rewrite-model, or runs with redaction unconfigured
// and handles shared records manually.
func newRewriteProvider(cfg *config.Config) (*chat.Client, string) {
	if cfg == nil {
		return nil, ""
	}
	endpoint := strings.TrimSpace(cfg.Chat.Endpoint)
	model := strings.TrimSpace(subjectRewriteModel)
	if model == "" {
		model = strings.TrimSpace(cfg.Chat.Model)
	}
	if endpoint == "" || model == "" {
		// Not configured. Not an error — see newRedactDeps.
		return nil, ""
	}
	return chat.NewClient(endpoint, cfg.Chat.APIKey, model,
		chat.WithLogger(zerolog.Nop()),
	), model
}

// reviewRedactionOnTerminal is the §8 gate: a human reads what a generative model
// proposes to do to a record concerning a third party, BEFORE it happens.
//
// Default-on permanently, not just for a first release. The diff is shown in full
// rather than summarised, because the failure this gate exists to catch is
// OVER-redaction — a rewrite that also removed the other person's data would verify
// perfectly clean and pass every mechanical check.
func reviewRedactionOnTerminal(p datasubject.RedactionProposal) (bool, error) {
	fmt.Printf("\n────────────────────────────────────────────────────────────\n")
	fmt.Printf("REVIEW REDACTION  %s/%s\n", p.Table, p.RowID)
	fmt.Printf("model: %s\n", p.Model)
	fmt.Printf("removing: %s\n", strings.Join(p.Identifiers, ", "))
	fmt.Printf("\n--- BEFORE ---\n%s\n", p.Before)
	fmt.Printf("\n--- AFTER ----\n%s\n", p.After)
	fmt.Printf("\nCheck that OTHER people's data survived — that is what this review is for.\n")
	fmt.Printf("Apply this rewrite? [y/N/q]: ")

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		// Could not read a decision. Treat as "not approved": the caller records a
		// deferral and nothing is written.
		return false, fmt.Errorf("read review decision: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	case "q", "quit":
		return false, fmt.Errorf("review abandoned by the operator")
	default:
		return false, nil
	}
}
