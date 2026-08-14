package registry

import (
	"reflect"
	"strings"
	"testing"

	"vornik.io/vornik/internal/promptblock"
)

// Suppression is the operator knob LLD 09 §13.3.1 promises: an operator who
// finds an ADVISORY guidance block redundant for their swarm may switch it off.
// It is deliberately a toggle over daemon-authored blocks and never a text
// field — free prompt text from config would be an arbitrary-system-prompt
// write through the trusted directive channel.
//
// The two unhonourable cases resolve differently on purpose (operator ruling
// 2026-08-13):
//
//   - Naming an INVARIANT block fails the swarm file. It is always an operator
//     error, and quietly ignoring it would leave a deployment believing a
//     warning is off when the rule behind it still runs.
//   - Naming an UNKNOWN block only warns. Block names are a config contract
//     across releases, and a name this binary no longer declares must not take
//     an existing deployment's swarms offline on upgrade.

func swarmWithSuppression(names ...string) *Swarm {
	return &Swarm{
		ID: "test-swarm",
		Roles: []SwarmRole{{
			Name:    "worker",
			Runtime: SwarmRoleRuntime{Image: "localhost/vornik-agent:latest"},
		}},
		SuppressedGuidanceBlocks: names,
	}
}

func TestSwarmValidate_SuppressingAnAdvisoryBlockIsAllowed(t *testing.T) {
	sw := swarmWithSuppression(promptblock.ToolBudget, promptblock.CanonicalContext)
	if err := sw.Validate("test-swarm.md"); err != nil {
		t.Fatalf("suppressing advisory blocks must validate: %v", err)
	}
	if warns := sw.UnknownSuppressedGuidanceBlocks(); len(warns) != 0 {
		t.Errorf("no warnings expected for known advisory blocks, got %v", warns)
	}
}

func TestSwarmValidate_SuppressingAnInvariantBlockFailsTheFile(t *testing.T) {
	sw := swarmWithSuppression(promptblock.ReportingIntegrity)
	err := sw.Validate("test-swarm.md")
	if err == nil {
		t.Fatal("suppressing an invariant block must fail validation, got nil")
	}
	var vErr SwarmValidationError
	if !asSwarmValidationError(err, &vErr) {
		t.Fatalf("want SwarmValidationError, got %T: %v", err, err)
	}
	if vErr.Field != "suppressedGuidanceBlocks" {
		t.Errorf("error field = %q, want suppressedGuidanceBlocks", vErr.Field)
	}
	// The message has to tell the operator WHY, or they will read the refusal
	// as a bug in the knob rather than as the rule still applying.
	for _, want := range []string{promptblock.ReportingIntegrity, "invariant"} {
		if !strings.Contains(vErr.Message, want) {
			t.Errorf("message %q does not mention %q", vErr.Message, want)
		}
	}
}

func TestSwarmValidate_UnknownBlockWarnsButLoads(t *testing.T) {
	sw := swarmWithSuppression("tool-budgett", promptblock.ToolBudget)
	if err := sw.Validate("test-swarm.md"); err != nil {
		t.Fatalf("an unknown block name must not fail the file: %v", err)
	}
	warns := sw.UnknownSuppressedGuidanceBlocks()
	if !reflect.DeepEqual(warns, []string{"tool-budgett"}) {
		t.Errorf("UnknownSuppressedGuidanceBlocks = %v, want [tool-budgett]", warns)
	}
}

// A blank entry is a YAML artefact (a trailing "- " in a list), not a request.
// It must neither warn nor fail.
func TestSwarmValidate_BlankEntriesAreIgnored(t *testing.T) {
	sw := swarmWithSuppression("", "   ", promptblock.ToolBudget)
	if err := sw.Validate("test-swarm.md"); err != nil {
		t.Fatalf("blank suppression entries must be ignored: %v", err)
	}
	if warns := sw.UnknownSuppressedGuidanceBlocks(); len(warns) != 0 {
		t.Errorf("blank entries must not warn, got %v", warns)
	}
}

func TestSwarmValidate_NoSuppressionListIsTheDefault(t *testing.T) {
	sw := swarmWithSuppression()
	if err := sw.Validate("test-swarm.md"); err != nil {
		t.Fatalf("a swarm with no suppression list must validate: %v", err)
	}
	if sw.SuppressesGuidanceBlock(promptblock.ToolBudget) {
		t.Error("no list must suppress nothing")
	}
}

func TestSwarm_SuppressesGuidanceBlock(t *testing.T) {
	sw := swarmWithSuppression(promptblock.ToolBudget)
	if !sw.SuppressesGuidanceBlock(promptblock.ToolBudget) {
		t.Error("a listed advisory block must report as suppressed")
	}
	if sw.SuppressesGuidanceBlock(promptblock.CanonicalContext) {
		t.Error("an unlisted block must not report as suppressed")
	}
	// Defence in depth: validation rejects this config, but if an invariant
	// name ever reaches a live Swarm (a hand-edited file loaded by an older
	// binary, a struct built in code), the answer is still no.
	rogue := &Swarm{ID: "x", SuppressedGuidanceBlocks: []string{promptblock.ReportingIntegrity}}
	if rogue.SuppressesGuidanceBlock(promptblock.ReportingIntegrity) {
		t.Error("an invariant block must never report as suppressed, whatever the config says")
	}
}

// The knob is a toggle over names, never operator-authored text. This pins the
// field's TYPE: a string field here would be an arbitrary-system-prompt write
// through the trusted directive channel (LLD 09 §13.7).
func TestSuppressionFieldIsAListOfNamesNotText(t *testing.T) {
	f, ok := reflect.TypeOf(Swarm{}).FieldByName("SuppressedGuidanceBlocks")
	if !ok {
		t.Fatal("Swarm has no SuppressedGuidanceBlocks field")
	}
	if f.Type.Kind() != reflect.Slice || f.Type.Elem().Kind() != reflect.String {
		t.Fatalf("SuppressedGuidanceBlocks is %s; it must be a list of block NAMES", f.Type)
	}
	if got := f.Tag.Get("yaml"); got != "suppressedGuidanceBlocks" {
		t.Errorf("yaml tag = %q, want suppressedGuidanceBlocks", got)
	}
}

// asSwarmValidationError is a local errors.As, kept here so the test reads the
// same whether Validate returns the value or a wrapped pointer.
func asSwarmValidationError(err error, out *SwarmValidationError) bool {
	if v, ok := err.(SwarmValidationError); ok {
		*out = v
		return true
	}
	return false
}
