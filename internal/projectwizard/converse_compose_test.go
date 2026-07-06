package projectwizard

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/templates"
)

// composeFakeTemplateSource implements both TemplateSource (the
// scalar seam Wizard.Templates is typed to) and MultiMaterialiser
// (the seam Compose actually consumes) — mirroring the production
// catalogTemplateSource adapter, which implements both. lastSlug
// records the slug MaterialiseMulti was asked to render so tests can
// assert the anchor-selection decision (empty/unknown template →
// custom-base).
type composeFakeTemplateSource struct {
	known    map[string]bool
	files    map[string]string
	matErr   error
	lastSlug string
}

func (f *composeFakeTemplateSource) Lookup(slug string) (TemplateSpec, bool) {
	if f.known[slug] {
		return TemplateSpec{Slug: slug}, true
	}
	return TemplateSpec{}, false
}

func (f *composeFakeTemplateSource) Materialise(_ string, _ map[string]string) (map[string]string, error) {
	return f.files, f.matErr
}

func (f *composeFakeTemplateSource) MaterialiseMulti(slug string, _ map[string][]string, _ templates.OptionsResolver) (map[string]string, error) {
	f.lastSlug = slug
	if f.matErr != nil {
		return nil, f.matErr
	}
	return f.files, nil
}

const envelopeComposeSuccess = `{"message":"Looks good.","ready_to_commit":true,"composition":{"template":"custom-base","params":{"projectId":["pricing-watch"]},"addons":[{"type":"secret_requirement","name":"SLACK_TOKEN","label":"Slack token"}]}}`

// TestConverse_ComposeSuccessAllowsCommit: a composition that composes
// cleanly (custom-base + a secret_requirement addon) keeps
// ready_to_commit=true and persists the composition JSON onto the
// session row for Commit to consume later.
func TestConverse_ComposeSuccessAllowsCommit(t *testing.T) {
	w, store, _ := newWizardForTest(chatReply{content: envelopeComposeSuccess})
	w.Templates = &composeFakeTemplateSource{known: map[string]bool{"custom-base": true}, files: baseFiles()}
	w.KnownMCP = func(context.Context) map[string]bool { return map[string]bool{} }

	res, err := w.Converse(context.Background(), "", "op_1", "build me a pricing watch")
	if err != nil {
		t.Fatalf("converse: %v", err)
	}
	if !res.Envelope.ReadyToCommit {
		t.Errorf("expected ready_to_commit to survive a successful compose, got false; message=%q", res.Envelope.Message)
	}

	stored, gerr := store.Get(context.Background(), res.SessionID)
	if gerr != nil {
		t.Fatalf("get session: %v", gerr)
	}
	if len(stored.Composition) == 0 {
		t.Fatal("expected composition JSON persisted on the session")
	}
	if !containsStr(string(stored.Composition), "custom-base") {
		t.Errorf("persisted composition missing template slug: %s", stored.Composition)
	}
}

const envelopeComposeUnknownMCP = `{"message":"Looks good.","ready_to_commit":true,"composition":{"template":"custom-base","params":{"projectId":["pricing-watch"]},"addons":[{"type":"mcp_server","name":"unknown-slack"}]}}`

// TestConverse_ComposeErrorBlocksAndFeedsBack: an addon that fails
// Compose (unknown MCP server, empty KnownMCP) forces
// ready_to_commit=false and folds the ComposeError text into the
// operator-visible message so the LLM can self-correct next turn.
func TestConverse_ComposeErrorBlocksAndFeedsBack(t *testing.T) {
	w, _, _ := newWizardForTest(chatReply{content: envelopeComposeUnknownMCP})
	w.Templates = &composeFakeTemplateSource{known: map[string]bool{"custom-base": true}, files: baseFiles()}
	w.KnownMCP = func(context.Context) map[string]bool { return map[string]bool{} }

	res, err := w.Converse(context.Background(), "", "op_1", "build me a pricing watch")
	if err != nil {
		t.Fatalf("converse: %v", err)
	}
	if res.Envelope.ReadyToCommit {
		t.Error("expected ready_to_commit forced false on compose error")
	}
	if !containsStr(res.Envelope.Message, "composition") || !containsStr(res.Envelope.Message, "unknown-slack") {
		t.Errorf("expected compose error text fed back into message, got %q", res.Envelope.Message)
	}
}

// TestConverse_ComposeStructuralErrorSinglePrefix guards the
// double-prefix regression: ComposeError.Error() self-prefixes
// "composition: " on structural (AddonIndex<0) failures, and the
// Converse gate also wraps the feedback in "(composition: ...)". The
// wrapper must not stack the two into "(composition: composition:
// ...)" — this text is fed back to the LLM to self-correct, so the
// prefix must appear exactly once. A materialise failure is the
// cheapest structural error to provoke.
func TestConverse_ComposeStructuralErrorSinglePrefix(t *testing.T) {
	w, _, _ := newWizardForTest(chatReply{content: envelopeComposeEmptyTemplate})
	w.Templates = &composeFakeTemplateSource{
		known:  map[string]bool{"custom-base": true},
		matErr: errString("boom"),
	}
	w.KnownMCP = func(context.Context) map[string]bool { return map[string]bool{} }

	res, err := w.Converse(context.Background(), "", "op_1", "build me something")
	if err != nil {
		t.Fatalf("converse: %v", err)
	}
	if res.Envelope.ReadyToCommit {
		t.Error("expected ready_to_commit forced false on structural compose error")
	}
	if containsStr(res.Envelope.Message, "composition: composition:") {
		t.Errorf("compose feedback double-prefixed the composition tag: %q", res.Envelope.Message)
	}
	// And it must still carry the prefix exactly once + the underlying cause.
	if !containsStr(res.Envelope.Message, "composition:") || !containsStr(res.Envelope.Message, "boom") {
		t.Errorf("expected single composition-prefixed feedback with the cause, got %q", res.Envelope.Message)
	}
}

const envelopeComposeEmptyTemplate = `{"message":"Building from scratch.","ready_to_commit":false,"composition":{"template":"","params":{"projectId":["pricing-watch"]},"addons":[]}}`

// TestConverse_EmptyTemplateAnchorsCustomBase: an empty composition
// template anchors on "custom-base" rather than failing or passing
// the empty string through to MaterialiseMulti.
func TestConverse_EmptyTemplateAnchorsCustomBase(t *testing.T) {
	w, _, _ := newWizardForTest(chatReply{content: envelopeComposeEmptyTemplate})
	fake := &composeFakeTemplateSource{known: map[string]bool{"custom-base": true}, files: baseFiles()}
	w.Templates = fake
	w.KnownMCP = func(context.Context) map[string]bool { return map[string]bool{} }

	if _, err := w.Converse(context.Background(), "", "op_1", "build me something"); err != nil {
		t.Fatalf("converse: %v", err)
	}
	if fake.lastSlug != "custom-base" {
		t.Errorf("expected compose to anchor on custom-base for an empty template, got slug %q", fake.lastSlug)
	}
}

// TestConverse_ProposalTurnClearsStaleComposition guards the
// whole-branch review's Important #1 finding: session.Composition
// must track the LATEST turn only, not stick around from an earlier
// one. Turn 1 composes cleanly and persists a composition; turn 2
// emits a proposal-only ready envelope (no composition at all). If
// the stale turn-1 composition survived, Commit would silently
// commit the build the operator never approved instead of the
// proposal they're now looking at.
func TestConverse_ProposalTurnClearsStaleComposition(t *testing.T) {
	w, store, _ := newWizardForTest(
		chatReply{content: envelopeComposeSuccess},
		chatReply{content: envelopeProposeReady},
	)
	w.Templates = &composeFakeTemplateSource{known: map[string]bool{"custom-base": true}, files: baseFiles()}
	w.KnownMCP = func(context.Context) map[string]bool { return map[string]bool{} }

	res1, err := w.Converse(context.Background(), "", "op_1", "build me a pricing watch")
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	stored, gerr := store.Get(context.Background(), res1.SessionID)
	if gerr != nil {
		t.Fatalf("get session after turn 1: %v", gerr)
	}
	if len(stored.Composition) == 0 {
		t.Fatal("expected composition persisted after turn 1")
	}

	res2, err := w.Converse(context.Background(), res1.SessionID, "op_1", "actually let's do this instead")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if res2.Envelope.Composition != nil {
		t.Fatalf("test fixture bug: turn 2 envelope unexpectedly carries a composition: %+v", res2.Envelope.Composition)
	}
	if !res2.Envelope.ReadyToCommit {
		t.Errorf("expected the legacy proposal path to report ready_to_commit=true, got message=%q", res2.Envelope.Message)
	}

	stored2, gerr := store.Get(context.Background(), res1.SessionID)
	if gerr != nil {
		t.Fatalf("get session after turn 2: %v", gerr)
	}
	if len(stored2.Composition) != 0 {
		t.Errorf("expected stale composition cleared after a proposal-only turn, got %s", stored2.Composition)
	}
}

// TestConverse_NoCompositionUsesLegacyPath: an envelope carrying only
// proposal.raw (no composition) still runs the existing
// validateProposal path unchanged — compose must never be invoked.
func TestConverse_NoCompositionUsesLegacyPath(t *testing.T) {
	w, _, _ := newWizardForTest(chatReply{content: envelopeProposeReady})
	fake := &composeFakeTemplateSource{known: map[string]bool{"custom-base": true}, files: baseFiles()}
	w.Templates = fake

	res, err := w.Converse(context.Background(), "", "op_1", "I want a news feed")
	if err != nil {
		t.Fatalf("converse: %v", err)
	}
	if res.Envelope.Proposal == nil || res.Envelope.Proposal.Raw["projectId"] != "news" {
		t.Errorf("expected legacy proposal path to run, got %+v", res.Envelope.Proposal)
	}
	if !res.Envelope.ReadyToCommit {
		t.Error("expected ready_to_commit=true via the legacy validateProposal path (no template anchor, permissive validator)")
	}
	if fake.lastSlug != "" {
		t.Errorf("compose must not run when the envelope has no composition; MaterialiseMulti was called with slug %q", fake.lastSlug)
	}
}
