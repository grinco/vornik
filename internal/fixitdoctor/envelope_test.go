package fixitdoctor

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/version"
)

func TestAllowedActionKinds_CommunityExcludesConfigApply(t *testing.T) {
	kinds := AllowedActionKinds(version.EditionCommunity)
	for _, k := range kinds {
		if k == ActionKindConfigApply {
			t.Fatalf("community edition must not include config_apply, got %v", kinds)
		}
	}
	// Everything else should still be present.
	want := []ActionKind{ActionKindConfigApplyGate, ActionKindRetryTask, ActionKindReprobeIntegration, ActionKindSetSecret, ActionKindLinkOut}
	for _, w := range want {
		found := false
		for _, k := range kinds {
			if k == w {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected community kinds to include %q, got %v", w, kinds)
		}
	}
}

func TestAllowedActionKinds_EnterpriseIncludesConfigApply(t *testing.T) {
	kinds := AllowedActionKinds(version.EditionEnterprise)
	found := false
	for _, k := range kinds {
		if k == ActionKindConfigApply {
			found = true
		}
	}
	if !found {
		t.Fatalf("enterprise edition must include config_apply, got %v", kinds)
	}
}

func TestAllowedActionKinds_UnknownEditionFailsSafeToCommunity(t *testing.T) {
	kinds := AllowedActionKinds("bogus")
	for _, k := range kinds {
		if k == ActionKindConfigApply {
			t.Fatalf("unknown/unstamped edition must fail-safe to community, got %v", kinds)
		}
	}
}

func TestEnvelopeResponseFormat_SchemaEnumMatchesEdition(t *testing.T) {
	ceFormat := EnvelopeResponseFormat(version.EditionCommunity)
	if strings.Contains(string(ceFormat.JSONSchema.Schema), `"config_apply"`) {
		t.Fatalf("community response_format schema must not mention config_apply: %s", ceFormat.JSONSchema.Schema)
	}
	eeFormat := EnvelopeResponseFormat(version.EditionEnterprise)
	if !strings.Contains(string(eeFormat.JSONSchema.Schema), `"config_apply"`) {
		t.Fatalf("enterprise response_format schema must mention config_apply: %s", eeFormat.JSONSchema.Schema)
	}
}
