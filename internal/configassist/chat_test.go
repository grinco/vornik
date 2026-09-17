package configassist

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestRenderChatResult_NeverGoesQuiet — every path says something. A chat that
// goes silent is indistinguishable from a daemon that died, and the person is
// left holding a request whose fate they cannot determine.
func TestRenderChatResult_NeverGoesQuiet(t *testing.T) {
	for name, res := range map[string]*Result{
		"nil result":                nil,
		"neither filed nor refused": {},
		"refusal":                   {Refusal: &Refusal{Code: RefuseEntrypointCeiling, Message: "class D is not available through the chat entrypoint"}},
		"filed":                     {Proposal: &persistence.ControlPlaneProposal{ID: "cap_1"}, Class: ClassA, HasEvidence: true},
	} {
		if got := strings.TrimSpace(renderChatResult(res)); got == "" {
			t.Errorf("%s rendered an empty message", name)
		}
	}
}

// TestRenderChatResult_RefusalIsTheEnginesWords — the class ceiling's refusal
// carries the by-hand remedy §6.3 specifies. Re-wording it here would drift
// from the console's, and the console's is the one the design fixes.
func TestRenderChatResult_RefusalIsTheEnginesWords(t *testing.T) {
	const msg = "class D is not available through the chat entrypoint (it carries classes A and B2 only); " +
		"make this change through the operator REST/CLI or console entrypoint, or by hand"
	got := renderChatResult(&Result{Refusal: &Refusal{Code: RefuseEntrypointCeiling, Message: msg}})
	if got != msg {
		t.Errorf("the refusal was re-worded:\n got: %s\nwant: %s", got, msg)
	}
}

// TestRenderChatResult_SaysNothingWasApplied — the chat entrypoint never
// auto-applies (§6.3, MayAutoApply). "Filed proposal cap_1" on its own reads
// like the change happened; a person will close the gap themselves if the
// message does not.
func TestRenderChatResult_SaysNothingWasApplied(t *testing.T) {
	got := renderChatResult(&Result{
		Proposal: &persistence.ControlPlaneProposal{ID: "cap_1"}, Class: ClassA, HasEvidence: true,
	})
	if !strings.Contains(got, "cap_1") || !strings.Contains(got, "class "+ClassA) {
		t.Errorf("the reply does not identify what was filed: %s", got)
	}
	if !strings.Contains(got, "Nothing has been applied") {
		t.Errorf("the reply lets a person believe the change is live: %s", got)
	}
}

// TestRenderChatResult_UnmeasuredSaysSo — §4.1's honesty rules hold in chat
// too: a proposal with no measurement behind it must not borrow the authority
// of one that has.
func TestRenderChatResult_UnmeasuredSaysSo(t *testing.T) {
	unmeasured := renderChatResult(&Result{
		Proposal: &persistence.ControlPlaneProposal{ID: "cap_2"}, Class: ClassA, HasEvidence: false,
	})
	if !strings.Contains(unmeasured, "No measurement stands behind this") {
		t.Errorf("an unmeasured proposal did not say so: %s", unmeasured)
	}
	measured := renderChatResult(&Result{
		Proposal: &persistence.ControlPlaneProposal{ID: "cap_3"}, Class: ClassA, HasEvidence: true,
	})
	if strings.Contains(measured, "No measurement stands behind this") {
		t.Errorf("a measured proposal was reported as unmeasured: %s", measured)
	}
}

// TestNewChatAdapter_NilEngineIsNilAdapter — the container wires this
// unconditionally and the channels decide the entrypoint exists by testing the
// interface for nil. A non-nil adapter wrapping no engine would make every
// deployment advertise an entrypoint that cannot serve.
func TestNewChatAdapter_NilEngineIsNilAdapter(t *testing.T) {
	if NewChatAdapter(nil) != nil {
		t.Fatal("a nil engine produced a live adapter; every deployment would advertise the entrypoint")
	}
}
