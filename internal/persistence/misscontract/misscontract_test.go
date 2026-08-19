package misscontract_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/misscontract"
)

// The convention this package exists to pin. A single-entity lookup that
// finds no row returns (nil, persistence.ErrNotFound) — never (nil, nil),
// which is what let a stricter-than-production test double certify the
// nil-dereference that crash-looped the daemon on 2026-08-19 (7f0b5337).

func TestCheckBehavior_ErrNotFound_accepts_the_declared_pair(t *testing.T) {
	if err := misscontract.CheckBehavior(misscontract.MissErrNotFound, true, persistence.ErrNotFound); err != nil {
		t.Fatalf("declared pair rejected: %v", err)
	}
}

func TestCheckBehavior_ErrNotFound_accepts_a_wrapped_error(t *testing.T) {
	wrapped := fmt.Errorf("Get: %w", persistence.ErrNotFound)
	if err := misscontract.CheckBehavior(misscontract.MissErrNotFound, true, wrapped); err != nil {
		t.Fatalf("wrapped ErrNotFound rejected: %v", err)
	}
}

// Regression: 2026-08-19. This is the exact shape that shipped the crash —
// production returned (nil, nil) for an absent row while every double
// returned an error, so no test ever reached the dereference.
func TestCheckBehavior_ErrNotFound_rejects_a_permissive_miss(t *testing.T) {
	err := misscontract.CheckBehavior(misscontract.MissErrNotFound, true, nil)
	if err == nil {
		t.Fatal("a (nil, nil) miss was accepted against MissErrNotFound")
	}
	if !strings.Contains(err.Error(), "nil error") {
		t.Errorf("divergence not named in %q", err)
	}
}

func TestCheckBehavior_ErrNotFound_rejects_a_non_nil_value(t *testing.T) {
	if err := misscontract.CheckBehavior(misscontract.MissErrNotFound, false, persistence.ErrNotFound); err == nil {
		t.Fatal("a non-nil value alongside ErrNotFound was accepted")
	}
}

func TestCheckBehavior_ErrNotFound_rejects_an_unrelated_error(t *testing.T) {
	if err := misscontract.CheckBehavior(misscontract.MissErrNotFound, true, errors.New("connection refused")); err == nil {
		t.Fatal("an unrelated error was accepted as a conforming miss")
	}
}

func TestCheckBehavior_NilNil_accepts_the_declared_pair(t *testing.T) {
	if err := misscontract.CheckBehavior(misscontract.MissNilNil, true, nil); err != nil {
		t.Fatalf("declared pair rejected: %v", err)
	}
}

func TestCheckBehavior_NilNil_rejects_ErrNotFound(t *testing.T) {
	if err := misscontract.CheckBehavior(misscontract.MissNilNil, true, persistence.ErrNotFound); err == nil {
		t.Fatal("ErrNotFound was accepted against MissNilNil")
	}
}

// A typo in a contract key must fail loudly rather than vacuously pass —
// otherwise the guard reports conformance for a method nobody checked.
func TestCheck_unregistered_key_is_an_error(t *testing.T) {
	err := misscontract.Check("NoSuchRepository.Get", true, persistence.ErrNotFound)
	if err == nil {
		t.Fatal("an unregistered key was accepted")
	}
	if !strings.Contains(err.Error(), "NoSuchRepository.Get") {
		t.Errorf("key not named in %q", err)
	}
}

func TestCheck_resolves_a_registered_key(t *testing.T) {
	const key = "ExtractedDocumentRepository.Get"
	if _, ok := misscontract.Behavior(key); !ok {
		t.Fatalf("%s is not registered", key)
	}
	if err := misscontract.Check(key, true, persistence.ErrNotFound); err != nil {
		t.Fatalf("registered key rejected its own contract: %v", err)
	}
}

// Every method the audit named as permissive must be registered, so that
// wiring AssertMiss into the suites cannot silently skip one.
func TestContract_registers_every_previously_permissive_lookup(t *testing.T) {
	permissive := []string{
		"ChatMemoryWriteConfirmationRepository.Get",
		"ExecutionToolGrantRepository.Current",
		"MCPOAuthTokenRepository.Get",
		"TaskMessageRepository.GetOpenCheckpoint",
		"TaskScratchpadRepository.Get",
		"ExtractedDocumentRepository.Get",
		"ExtractedDocumentRepository.GetByArtifact",
		"KnowledgeEdgeRepository.Get",
		"KnowledgeEntityRepository.Get",
		"KnowledgeEntityRepository.GetByCanonical",
	}
	for _, key := range permissive {
		b, ok := misscontract.Behavior(key)
		if !ok {
			t.Errorf("%s: not registered", key)
			continue
		}
		if b != misscontract.MissErrNotFound {
			t.Errorf("%s: declared %v, want MissErrNotFound after normalisation", key, b)
		}
	}
}

// The excluded set is documented rather than merely absent: a reader must be
// able to tell "deliberately not a keyed lookup" from "nobody got to it yet".
func TestExcluded_carries_a_reason_for_every_entry(t *testing.T) {
	if len(misscontract.Excluded) == 0 {
		t.Fatal("no exclusions declared")
	}
	for key, reason := range misscontract.Excluded {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s: excluded with no reason", key)
		}
		if _, ok := misscontract.Behavior(key); ok {
			t.Errorf("%s: both excluded and registered", key)
		}
	}
	if _, ok := misscontract.Excluded["TaskRepository.LeaseTask"]; !ok {
		t.Error("LeaseTask must be excluded — an empty queue is not a miss")
	}
}

func TestMissBehavior_String(t *testing.T) {
	cases := map[misscontract.MissBehavior]string{
		misscontract.MissErrNotFound: "MissErrNotFound",
		misscontract.MissNilNil:      "MissNilNil",
		misscontract.MissBehavior(9): "MissBehavior(9)",
	}
	for b, want := range cases {
		if got := b.String(); got != want {
			t.Errorf("MissBehavior(%d).String() = %q, want %q", int(b), got, want)
		}
	}
}

// An excluded key must fail differently from an unknown one: "this is
// deliberately not a keyed lookup" is a different mistake from a typo, and
// the message has to say which.
func TestCheck_excludedKeySaysSo(t *testing.T) {
	err := misscontract.Check("TaskRepository.LeaseTask", true, persistence.ErrNotFound)
	if err == nil {
		t.Fatal("an excluded key was accepted")
	}
	if !strings.Contains(err.Error(), "excluded") {
		t.Errorf("message does not say the key is excluded: %v", err)
	}
	if !strings.Contains(err.Error(), "queue poll") {
		t.Errorf("message does not carry the exclusion's reason: %v", err)
	}
}

func TestCheckBehavior_rejectsAnUnknownBehaviour(t *testing.T) {
	if err := misscontract.CheckBehavior(misscontract.MissBehavior(42), true, nil); err == nil {
		t.Fatal("an unknown behaviour was accepted")
	}
}
