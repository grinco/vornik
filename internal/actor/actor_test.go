package actor

import "testing"

func TestRoundTrip(t *testing.T) {
	cases := []Actor{
		APIKey("key_123"),
		User("usr_456"),
		Autonomy,
		CrossProjectCall,
		Counterfactual,
		Anonymous("install-a"),
	}
	for _, want := range cases {
		got, err := Parse(want.String())
		if err != nil {
			t.Fatalf("Parse(%q): %v", want.String(), err)
		}
		if got != want {
			t.Errorf("round trip %q = %+v, want %+v", want.String(), got, want)
		}
	}
}

// A system actor may name a sub-source, so the id half can itself contain a
// colon. Splitting on every colon (or demanding exactly one) would reject the
// signature-verified webhook actor the design specifies in rule 7.
func TestParse_SplitsOnFirstColonOnly(t *testing.T) {
	got, err := Parse("system:webhook:github")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != KindSystem || got.ID != "webhook:github" {
		t.Errorf("got %+v, want system / webhook:github", got)
	}
	if got.String() != "system:webhook:github" {
		t.Errorf("String() = %q, round trip broken", got.String())
	}
}

func TestParse_Rejects(t *testing.T) {
	bad := map[string]string{
		"":              "empty",
		"   ":           "blank",
		"noseparator":   "no kind separator",
		"api_key:":      "empty id",
		"wizard:merlin": "unknown kind must not silently bucket somewhere",
		":123":          "empty kind",
		"API_KEY:abc":   "kind is case-sensitive; a near-miss is a bug, not an alias",
	}
	for in, why := range bad {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) succeeded, want error (%s)", in, why)
		}
	}
}

// An empty id must not produce a malformed "api_key:" row. Callers write NULL
// for the zero actor instead.
func TestConstructors_EmptyIDYieldsZero(t *testing.T) {
	for name, got := range map[string]Actor{
		"APIKey":    APIKey(""),
		"User":      User("   "),
		"System":    System(""),
		"Anonymous": Anonymous(""),
	} {
		if !got.IsZero() {
			t.Errorf("%s(empty) = %+v, want zero", name, got)
		}
		if got.String() != "" {
			t.Errorf("%s(empty).String() = %q, want empty so the caller writes NULL", name, got.String())
		}
	}
}

// The asymmetry in §3.2: a key is a fact that can later resolve to a person;
// the absence of one is not a fact about anyone. Anonymous and system work must
// never become promotable, or the leaderboard starts inventing attribution.
func TestPromotable(t *testing.T) {
	promotable := []Actor{APIKey("k"), User("u")}
	for _, a := range promotable {
		if !a.Promotable() {
			t.Errorf("%s must be promotable to a person", a)
		}
	}
	never := []Actor{Autonomy, CrossProjectCall, Counterfactual, KGExtraction, Anonymous("install")}
	for _, a := range never {
		if a.Promotable() {
			t.Errorf("%s must NEVER be promotable — §3.2 depends on it", a)
		}
	}
}

// system: is an actor, not a null. Both must be distinguishable, because
// collapsing them is what makes the dashboard flatter human usage.
func TestSystemIsNotZero(t *testing.T) {
	if Autonomy.IsZero() {
		t.Error("a system actor must not read as unattributed")
	}
	if !Autonomy.IsSystem() {
		t.Error("Autonomy must report as system so the UI can mark it distinct")
	}
	if (Actor{}).IsSystem() {
		t.Error("the zero actor is 'we failed to record', not system work")
	}
}

// Every well-known system actor must parse — a typo in one of these would not
// fail anywhere, it would silently mint a new leaderboard row.
func TestWellKnownSystemActorsAreValid(t *testing.T) {
	for _, a := range []Actor{
		Autonomy, CrossProjectCall, Counterfactual,
		KGExtraction, MemoryTitler, TaskNarrator, MemoryNarrative,
	} {
		parsed, err := Parse(a.String())
		if err != nil {
			t.Errorf("well-known actor %q does not parse: %v", a, err)
			continue
		}
		if parsed != a {
			t.Errorf("well-known actor %q round-trips to %+v", a, parsed)
		}
		if !a.IsSystem() {
			t.Errorf("%q must be a system actor", a)
		}
	}
}
