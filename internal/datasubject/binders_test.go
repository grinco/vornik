package datasubject

import (
	"strings"
	"testing"
)

func TestBindAuthenticatedIdentity(t *testing.T) {
	b, err := BindAuthenticatedIdentity("user-1", "telegram", "12345")
	if err != nil {
		t.Fatalf("BindAuthenticatedIdentity: %v", err)
	}
	if len(b.Identifiers) != 2 {
		t.Fatalf("want user_id + channel identifiers, got %+v", b.Identifiers)
	}
	// Authentication is the only basis entitled to claim certainty; keeping the
	// entitlement narrow is what stops `certain` becoming meaningless.
	for _, id := range b.Identifiers {
		if id.Confidence != ConfidenceCertain {
			t.Errorf("%s should be certain, got %q", id.Kind, id.Confidence)
		}
	}
	if b.Identifiers[1].Value != "telegram:12345" {
		t.Errorf("channel identifier = %q, want telegram:12345", b.Identifiers[1].Value)
	}
	if len(b.Links) != 1 || b.Links[0].Exclusivity != ExclusiveRow {
		t.Errorf("the identity row is about one person: %+v", b.Links)
	}
}

// A bare channel name identifies nobody, so it must not become an identifier.
func TestBindAuthenticatedIdentity_PartialChannelIsNotAnIdentifier(t *testing.T) {
	b, err := BindAuthenticatedIdentity("user-1", "telegram", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range b.Identifiers {
		if id.Kind == KindChannel {
			t.Errorf("a channel with no external id must not be an identifier: %+v", id)
		}
	}
}

func TestBindAuthenticatedIdentity_RequiresUserID(t *testing.T) {
	if _, err := BindAuthenticatedIdentity("  ", "telegram", "1"); err == nil {
		t.Fatal("an empty user id must be refused")
	}
}

func TestBindOperatorLink(t *testing.T) {
	b, err := BindOperatorLink("op-1", "telegram:999")
	if err != nil {
		t.Fatalf("BindOperatorLink: %v", err)
	}
	if len(b.Links) != 1 || b.Links[0].Table != TableOperatorProfile {
		t.Fatalf("want an operator_profile link, got %+v", b.Links)
	}
	// A profile is about one person by construction, so it is one of the few
	// rows erasure may delete without the shared-row decision.
	if b.Links[0].Exclusivity != ExclusiveRow {
		t.Errorf("operator_profile should be exclusive, got %q", b.Links[0].Exclusivity)
	}
}

// The address is a fact off the wire; that it denotes a particular human is an
// inference. A shared mailbox or a role address makes it a poor proxy, so this
// binder must not claim certainty.
func TestBindEmailEnvelope_IsProbableNotCertain(t *testing.T) {
	b, err := BindEmailEnvelope("Jane Doe <Jane.Doe@Example.COM>", "msg-1", "assistant")
	if err != nil {
		t.Fatalf("BindEmailEnvelope: %v", err)
	}
	if len(b.Identifiers) != 1 || b.Identifiers[0].Kind != KindEmail {
		t.Fatalf("want one email identifier, got %+v", b.Identifiers)
	}
	if b.Identifiers[0].Confidence != ConfidenceProbable {
		t.Errorf("an envelope address must be probable, not %q", b.Identifiers[0].Confidence)
	}
	if b.Identifiers[0].Value != "jane.doe@example.com" {
		t.Errorf("address not normalised: %q", b.Identifiers[0].Value)
	}
	// An email concerns its recipients and anyone discussed in it, not only the
	// sender — claiming exclusivity would authorise deleting their data on the
	// sender's request.
	if len(b.Links) != 1 || b.Links[0].Exclusivity != SharedRow {
		t.Errorf("an email message must be linked as shared: %+v", b.Links)
	}
	if b.Links[0].ProjectID != "assistant" {
		t.Errorf("project scope lost: %+v", b.Links[0])
	}
}

func TestBindEmailEnvelope_NoRowStillYieldsTheIdentifier(t *testing.T) {
	b, err := BindEmailEnvelope("someone@example.com", "", "")
	if err != nil {
		t.Fatalf("BindEmailEnvelope: %v", err)
	}
	if len(b.Identifiers) != 1 || len(b.Links) != 0 {
		t.Errorf("want an identifier and no links, got %+v / %+v", b.Identifiers, b.Links)
	}
}

// Normalisation must fold case and display names, and must NOT canonicalise
// further. On this axis a false MERGE is worse than a false split: a split
// loses coverage, a merge discloses one person's data to another.
func TestNormaliseEmail(t *testing.T) {
	for in, want := range map[string]string{
		"Jane Doe <Jane.Doe@Example.COM>": "jane.doe@example.com",
		"  UPPER@EXAMPLE.ORG  ":           "upper@example.org",
		"plain@example.net":               "plain@example.net",
	} {
		got, err := NormaliseEmail(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormaliseEmail(%q) = %q, want %q", in, got, want)
		}
	}

	// Gmail-style +tags and dots must survive: treating a+x@ as a@ would merge
	// two identifiers on a provider-specific convention.
	for _, in := range []string{"a+tag@example.com", "a.b@example.com"} {
		got, err := NormaliseEmail(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != in {
			t.Errorf("NormaliseEmail must not canonicalise %q into %q — that merges people", in, got)
		}
	}

	for _, bad := range []string{"", "   ", "not-an-address", "@example.com", "user@"} {
		if _, err := NormaliseEmail(bad); err == nil {
			t.Errorf("NormaliseEmail(%q) should error", bad)
		}
	}
}

func TestBinding_Validate(t *testing.T) {
	if err := (Binding{}).Validate(); err == nil {
		t.Error("an empty binding must be refused")
	}
	// An identifier may not claim more confidence than its source allows —
	// the same rule links obey, since both feed the same reports.
	bad := Binding{Identifiers: []Identifier{{
		Kind: KindEmail, Value: "a@b.c",
		Source: SourceKGExtraction, Confidence: ConfidenceCertain,
	}}}
	err := bad.Validate()
	if err == nil {
		t.Fatal("an over-confident identifier must be refused")
	}
	if !strings.Contains(err.Error(), "maximum") {
		t.Errorf("error should name the ceiling: %v", err)
	}
	// A malformed identifier is refused rather than persisted half-formed.
	if err := (Binding{Identifiers: []Identifier{{Kind: "", Value: "x",
		Source: SourceAuthenticated, Confidence: ConfidenceCertain}}}).Validate(); err == nil {
		t.Error("an identifier with no kind must be refused")
	}
	// A link naming an uncovered table is refused through Link.Validate.
	if err := (Binding{Links: []Link{{Table: "tool_audit_log", RowID: "r",
		Source: SourceAuthenticated, Confidence: ConfidenceCertain,
		Exclusivity: ExclusiveRow}}}).Validate(); err == nil {
		t.Error("a link to an uncovered table must be refused")
	}
}

// Every binder's output must pass Validate, or a binder could persist evidence
// the rest of the system considers malformed.
func TestAllBindersProduceValidBindings(t *testing.T) {
	bindings := []func() (Binding, error){
		func() (Binding, error) { return BindAuthenticatedIdentity("u", "telegram", "1") },
		func() (Binding, error) { return BindOperatorLink("op", "telegram:1") },
		func() (Binding, error) { return BindEmailEnvelope("a@b.co", "m1", "p") },
	}
	for i, fn := range bindings {
		b, err := fn()
		if err != nil {
			t.Errorf("binder %d returned an error: %v", i, err)
			continue
		}
		if err := b.Validate(); err != nil {
			t.Errorf("binder %d produced an invalid binding: %v", i, err)
		}
	}
}

// TestBindKGExtraction covers the first production binder (chat memory-write
// design D4.1): a resolved PERSON entity becomes a kg_entity identifier plus a
// post-insert link to the chat_memory chunk it was found in.
func TestBindKGExtraction(t *testing.T) {
	t.Run("identifier and link at the source ceiling", func(t *testing.T) {
		b, err := BindKGExtraction("ent-42", "chunk-7", "proj-x", ConfidencePossible)
		if err != nil {
			t.Fatalf("BindKGExtraction: %v", err)
		}
		if len(b.Identifiers) != 1 || b.Identifiers[0].Kind != KindKGEntity || b.Identifiers[0].Value != "ent-42" {
			t.Fatalf("want one kg_entity identifier for ent-42; got %+v", b.Identifiers)
		}
		if b.Identifiers[0].Source != SourceKGExtraction || b.Identifiers[0].Confidence != ConfidencePossible {
			t.Errorf("identifier source/confidence = %s/%s, want kg_extraction/possible", b.Identifiers[0].Source, b.Identifiers[0].Confidence)
		}
		if len(b.Links) != 1 {
			t.Fatalf("want one link; got %+v", b.Links)
		}
		l := b.Links[0]
		if l.Table != TableProjectMemoryChunks || l.RowID != "chunk-7" || l.ProjectID != "proj-x" {
			t.Errorf("link target wrong: %+v", l)
		}
		if l.Confidence != ConfidencePossible || l.Exclusivity != SharedRow {
			t.Errorf("link confidence/exclusivity = %s/%s, want possible/shared", l.Confidence, l.Exclusivity)
		}
	})

	t.Run("clamps a too-high confidence to the ceiling", func(t *testing.T) {
		// A caller must not be able to smuggle `certain` past AddIdentifier
		// (which does not re-validate) — the binder clamps to the ceiling.
		for _, in := range []Confidence{ConfidenceCertain, ConfidenceProbable, Confidence("")} {
			b, err := BindKGExtraction("ent-1", "chunk-1", "p", in)
			if err != nil {
				t.Fatalf("BindKGExtraction(%q): %v", in, err)
			}
			if b.Identifiers[0].Confidence != ConfidencePossible || b.Links[0].Confidence != ConfidencePossible {
				t.Errorf("confidence %q was not clamped to possible: id=%s link=%s",
					in, b.Identifiers[0].Confidence, b.Links[0].Confidence)
			}
		}
	})

	t.Run("identifier only when no chunk id is known yet", func(t *testing.T) {
		b, err := BindKGExtraction("ent-9", "", "", ConfidencePossible)
		if err != nil {
			t.Fatalf("BindKGExtraction: %v", err)
		}
		if len(b.Links) != 0 {
			t.Errorf("no link should be produced without a chunk id; got %+v", b.Links)
		}
		if len(b.Identifiers) != 1 {
			t.Errorf("the identifier must still be produced; got %+v", b.Identifiers)
		}
	})

	t.Run("empty match id is rejected", func(t *testing.T) {
		if _, err := BindKGExtraction("  ", "chunk-1", "p", ConfidencePossible); err == nil {
			t.Error("an empty match id must be rejected")
		}
	})

	// Guards the I2 assumption: the ceiling this binder clamps to IS the
	// SourceKGExtraction default. If that default is ever raised, this fails
	// and forces a re-think rather than silently over-claiming.
	t.Run("ConfidencePossible is the SourceKGExtraction default", func(t *testing.T) {
		got, err := DefaultConfidence(SourceKGExtraction)
		if err != nil || got != ConfidencePossible {
			t.Fatalf("DefaultConfidence(kg_extraction) = %q,%v; want possible", got, err)
		}
	})
}
