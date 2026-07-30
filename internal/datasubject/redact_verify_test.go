package datasubject

import (
	"errors"
	"strings"
	"testing"
)

// Increment 5, slice 5c — the verification floor (design §3, §3.1).
//
// Redaction is an LLM rewrite, so it is non-deterministic. The CHECK is not: after
// the model returns, we assert mechanically that no known identifier of the subject
// survives in the output. If one does, the redaction FAILED — nothing is written and
// the chunk stays deferred.
//
// Same shape as Export.LeaksForeignContent for Art 15(4): re-assert the property on
// the finished artefact instead of trusting the code that produced it.
//
// The failure direction is deliberate. A false POSITIVE (we think an identifier
// survived when it did not) defers the chunk to a human — annoying, safe. A false
// NEGATIVE writes text that still identifies the subject and reports success —
// which is the outcome this whole file exists to prevent. Every ambiguous case
// therefore resolves toward "deferred".
//
// Design: https://docs.vornik.io §3

// THE HEADLINE REGRESSION. A rewrite that still contains the subject's email or
// name must be rejected outright.
func TestVerifyRedaction_RejectsASurvivingIdentifier(t *testing.T) {
	ids := []string{"jane@example.com", "Jane Doe"}
	for _, tc := range []struct{ name, output string }{
		{"email survives verbatim", "Called jane@example.com about the results."},
		{"name survives verbatim", "Called Jane Doe about the results."},
		{"email survives mid-sentence", "cc: jane@example.com, and others"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyRedaction(ids, tc.output)
			if err == nil {
				t.Fatal("a surviving identifier must fail verification")
			}
			var verr *RedactionVerificationError
			if !errors.As(err, &verr) {
				t.Fatalf("want *RedactionVerificationError, got %T", err)
			}
			if len(verr.Surviving) == 0 {
				t.Error("the error must name which identifiers survived, so the reason is recordable")
			}
		})
	}
}

// A genuinely redacted rewrite passes, INCLUDING one that keeps another person's
// data. Preserving the other subject is the point of redacting rather than deleting.
func TestVerifyRedaction_AcceptsARewriteThatKeepsOtherSubjects(t *testing.T) {
	ids := []string{"jane@example.com", "Jane Doe"}
	output := "Called the client about the scan results; Peter Novak (peter@example.com) " +
		"joined the call and agreed to follow up."
	if err := VerifyRedaction(ids, output); err != nil {
		t.Fatalf("a clean rewrite preserving another subject must pass: %v", err)
	}
}

// --- §3.1 normalisation: each of these evades a naive strings.Contains ---

func TestVerifyRedaction_NormalisationDefeatsEvasion(t *testing.T) {
	ids := []string{"jane@example.com", "Jane Doe"}
	for _, tc := range []struct{ name, output string }{
		{"case variation", "contact JANE@EXAMPLE.COM"},
		{"mixed case name", "spoke to jANe dOE today"},
		{"whitespace runs", "spoke to Jane    Doe today"},
		{"newline inside the name", "spoke to Jane\nDoe today"},
		{"tab inside the name", "spoke to Jane\tDoe today"},
		// U+200B zero-width space: invisible to a reader, defeats substring matching.
		{"zero-width space injected", "contact jane\u200b@example.com"},
		{"zero-width non-joiner", "contact ja\u200cne@example.com"},
		{"BOM injected", "contact jane@example\ufeff.com"},
		// NFKC: full-width forms render as ordinary letters.
		{"full-width characters", "contact ｊａｎｅ@example.com"},
		// Punctuation-insensitive email forms (§3.1).
		{"at-word obfuscation", "contact jane at example dot com"},
		{"bracketed at", "contact jane[at]example.com"},
		{"parenthesised at", "contact jane(at)example.com"},
		{"bracketed dot", "contact jane@example[dot]com"},
		// Encoded copies, which ingested content really does carry.
		{"percent-encoded", "contact jane%40example.com"},
		{"base64-encoded copy", "raw: amFuZUBleGFtcGxlLmNvbQ=="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifyRedaction(ids, tc.output); err == nil {
				t.Errorf("%q must be caught — it identifies the subject to a reader "+
					"while evading a naive substring check", tc.output)
			}
		})
	}
}

// The bound stated in §3.2, pinned as a test so the claim in the subject-facing
// report stays honest. These are NOT caught, and the report must not imply they are.
func TestVerifyRedaction_DoesNotCatchInferentialIdentification(t *testing.T) {
	ids := []string{"jane@example.com", "Jane Doe"}
	for _, tc := range []struct{ name, output string }{
		{"pronoun reference", "Called her about her scan results; she sounded relieved."},
		{"role reference", "The patient was relieved by the oncology result."},
		{"quasi-identifiers", "Female, 34, oncology, admitted in January."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifyRedaction(ids, tc.output); err != nil {
				t.Errorf("§3.2 says inferential identification is out of scope; "+
					"claiming to catch %q would make the report's guarantee false", tc.output)
			}
		})
	}
}

// Unicode confusables are explicitly out of scope (§3.1). Pinned so the gap is a
// recorded decision rather than an assumed capability — the cost of the miss is a
// human-reviewed rewrite, not a silent leak, because --review is default-on.
func TestVerifyRedaction_ConfusablesAreKnownNotToBeCaught(t *testing.T) {
	// Cyrillic 'а' (U+0430) in place of Latin 'a'.
	cyrillic := "contact jаne@example.com"
	if err := VerifyRedaction([]string{"jane@example.com"}, cyrillic); err == nil {
		t.Log("documented gap: NFKC folds compatibility forms but not confusables")
	}
}

// --- fail-closed inputs ---

// An empty rewrite is not a redaction. Nor is one that merely deleted everything —
// that destroys the other subject's data, which is what redaction exists to avoid.
func TestVerifyRedaction_RejectsEmptyOutput(t *testing.T) {
	for _, tc := range []struct{ name, output string }{
		{"empty", ""},
		{"whitespace only", "   \n\t "},
		{"zero-width only", "\u200b\u200c\ufeff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifyRedaction([]string{"jane@example.com"}, tc.output); err == nil {
				t.Error("an empty rewrite must not be accepted as a successful redaction")
			}
		})
	}
}

// No identifiers means nothing can be verified, so there is no basis to claim the
// subject's data is gone. Fail closed rather than reporting a vacuous success —
// this is the same reasoning as PlanErasure refusing a missing ground.
func TestVerifyRedaction_RefusesWhenThereAreNoIdentifiers(t *testing.T) {
	for _, ids := range [][]string{nil, {}, {""}, {"   "}} {
		if err := VerifyRedaction(ids, "some rewritten text"); err == nil {
			t.Errorf("with identifiers %q there is nothing to verify against; "+
				"reporting success would be a vacuous guarantee", ids)
		}
	}
}

// A blank identifier among real ones must not be treated as "contained in
// everything" — that would defer every chunk forever.
func TestVerifyRedaction_IgnoresBlankIdentifiersAmongRealOnes(t *testing.T) {
	ids := []string{"jane@example.com", "", "   "}
	if err := VerifyRedaction(ids, "Called the client; Peter joined."); err != nil {
		t.Fatalf("blank identifiers must be skipped, not matched against everything: %v", err)
	}
}

// Very short identifiers match too eagerly once punctuation is stripped, so they are
// compared verbatim-only rather than through the alphanumeric form. Without this a
// two-letter identifier defers every chunk in the project.
func TestVerifyRedaction_ShortIdentifiersDoNotMatchEverything(t *testing.T) {
	if err := VerifyRedaction([]string{"jo"}, "The report concerns nobody in particular."); err != nil {
		t.Errorf("a 2-char identifier must not match unrelated text: %v", err)
	}
	// But it is still caught when it genuinely appears as a word.
	if err := VerifyRedaction([]string{"jo"}, "Spoke to Jo about it."); err == nil {
		t.Error("a short identifier appearing as a word must still be caught")
	}
}

// The error must not paste the subject's identifier into a log line. It is the
// subject's personal data, and an erasure path that leaks it into logs has created
// a new copy of what it was asked to delete.
func TestRedactionVerificationError_DoesNotLeakTheIdentifier(t *testing.T) {
	err := VerifyRedaction([]string{"jane@example.com"}, "contact jane@example.com")
	if err == nil {
		t.Fatal("expected a verification failure")
	}
	msg := err.Error()
	if strings.Contains(msg, "jane@example.com") {
		t.Errorf("the error message must not contain the raw identifier — an erasure "+
			"path that logs it has made a fresh copy of the data: %q", msg)
	}
	if !strings.Contains(msg, "1") {
		t.Errorf("the message should still say how many identifiers survived: %q", msg)
	}
	// The structured field carries the real values for the request record.
	var verr *RedactionVerificationError
	if errors.As(err, &verr) && len(verr.Surviving) == 1 && verr.Surviving[0] != "jane@example.com" {
		t.Error("the structured field should carry the actual identifier for the record")
	}
}
