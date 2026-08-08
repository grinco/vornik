package service

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// TestRegisterSubsystems_LogsRealEdition is the regression test for an
// operator-reported inconsistency (2026-08-07): a daemon whose banner read
// `enterprise edition` logged `"edition":"community"` on every EE capability
// registration line.
//
// The capabilities themselves were gated correctly — registration is driven by
// c.providers, never by c.Edition() — so nothing was actually disabled. But
// registerSubsystems() runs inside NewContainer, while SetEdition() was called
// by Run() AFTERWARDS, so Edition() fell back to the community default at the
// moment those lines were emitted. An operator diagnosing "are my EE features
// on?" from the logs was told community by four consecutive lines.
//
// instinct, trading, blackbox and clustering are all Enterprise capabilities
// (internal/editions matrix + featuredoctor registry), so "community" was
// wrong for every one of them.
func TestRegisterSubsystems_LogsRealEdition(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{Logger: zerolog.New(&buf)}
	c.applyOptions([]ContainerOption{
		WithProviders(ProviderSet{Clustering: stubClustering{}}),
		WithEdition("enterprise"),
	})

	c.registerSubsystems()

	var sawCapabilityLine bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		capName, ok := rec["capability"].(string)
		if !ok {
			continue
		}
		sawCapabilityLine = true
		if got := rec["edition"]; got != "enterprise" {
			t.Errorf("capability %q logged edition=%v, want \"enterprise\" — "+
				"an operator reading the logs is told EE features are community", capName, got)
		}
	}
	if !sawCapabilityLine {
		t.Fatal("no capability registration line was logged; the test asserts nothing")
	}
}

// TestWithEdition_AppliedBeforeSubsystemRegistration pins the ordering itself:
// the edition must be stamped by applyOptions (which runs early in
// NewContainer), NOT by a setter call after construction — otherwise anything
// NewContainer does with Edition() reads the default.
func TestWithEdition_AppliedBeforeSubsystemRegistration(t *testing.T) {
	c := &Container{}
	c.applyOptions([]ContainerOption{WithEdition("enterprise")})
	if got := c.Edition(); got != "enterprise" {
		t.Errorf("Edition() after applyOptions = %q, want %q", got, "enterprise")
	}
}

// TestWithEdition_NormalizesUnknownToCommunity: the option routes through
// SetEdition, so it inherits version.NormalizeEdition's fail-safe contract —
// an exact "enterprise" is honoured and EVERYTHING else, including a
// differently-cased variant, degrades to community.
//
// The case sensitivity is deliberate, not an oversight: the value arrives from
// an ldflag, and defaulting an unrecognised string to community means a
// mis-stamped build loses EE features loudly rather than claiming an
// entitlement it may not have.
func TestWithEdition_NormalizesUnknownToCommunity(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"enterprise", "enterprise"},
		{"ENTERPRISE", "community"},
		{"banana", "community"},
		{"", "community"},
	} {
		c := &Container{}
		c.applyOptions([]ContainerOption{WithEdition(tc.in)})
		if got := c.Edition(); got != tc.want {
			t.Errorf("WithEdition(%q) → Edition() = %q, want %q", tc.in, got, tc.want)
		}
	}
}
