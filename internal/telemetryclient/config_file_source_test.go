package telemetryclient

import "testing"

// TestProjectEvent_ConfigFileSourceSurvivesNormalisation — regression for "a
// project created by writing a config file is invisible to project_created
// telemetry".
//
// normaliseSource folds anything unrecognised to SourceCLIBasic, so emitting a
// new source without extending the closed enum would have MISLABELLED the event
// as a wizard creation rather than fixed the gap — a wrong number is worse than
// the silence it replaces, because it looks like data.
func TestProjectEvent_ConfigFileSourceSurvivesNormalisation(t *testing.T) {
	ev := ProjectEvent("1.2.3", SourceConfigFile, "", false, true)
	if ev.source != SourceConfigFile {
		t.Fatalf("source = %q, want %q — the event would be counted as a wizard creation",
			ev.source, SourceConfigFile)
	}
}

// And the fold still protects against a genuinely unknown value.
func TestProjectEvent_UnknownSourceStillFolds(t *testing.T) {
	ev := ProjectEvent("1.2.3", "something-new", "", false, false)
	if ev.source != SourceCLIBasic {
		t.Fatalf("source = %q, want the fold to %q", ev.source, SourceCLIBasic)
	}
}

// The file-drop path has no template by construction, and ProjectEvent forces
// "custom" when builtIn is false — so the emit site does not have to remember.
func TestProjectEvent_ConfigFileHasNoTemplate(t *testing.T) {
	ev := ProjectEvent("1.2.3", SourceConfigFile, "", false, false)
	if ev.properties == nil || ev.properties.Template != "custom" {
		t.Fatalf("template = %+v, want custom", ev.properties)
	}
}
