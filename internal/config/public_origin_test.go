package config

import (
	"strings"
	"testing"
)

// One public origin, two historical keys (callback-and-webhook-surfaces design §6).
// server.public_base_url is canonical — the more generic, server-level statement (operator
// decision 2026-08-04) — with auth.external_base_url as the fallback so no deployment has to
// edit config.

func TestPublicOrigin_Precedence(t *testing.T) {
	c := &Config{}
	if got := c.PublicOrigin(); got != "" {
		t.Errorf("neither key set: got %q, want empty", got)
	}

	c.Auth.ExternalBaseURL = "https://fallback.example.com"
	if got := c.PublicOrigin(); got != "https://fallback.example.com" {
		t.Errorf("fallback: got %q", got)
	}

	c.Server.PublicBaseURL = "https://canonical.example.com"
	if got := c.PublicOrigin(); got != "https://canonical.example.com" {
		t.Errorf("server.public_base_url must win: got %q", got)
	}
}

// TestPublicOrigin_StripsTrailingSlash — every consumer concatenates a path onto this, so a
// trailing slash would produce "//auth/mcp/callback" and a redirect_uri that byte-mismatches
// what the vendor has registered.
func TestPublicOrigin_StripsTrailingSlash(t *testing.T) {
	c := &Config{}
	c.Server.PublicBaseURL = "https://x.example.com/"
	if got := c.PublicOrigin(); got != "https://x.example.com" {
		t.Errorf("got %q", got)
	}
	c.Server.PublicBaseURL = "  https://y.example.com  "
	if got := c.PublicOrigin(); got != "https://y.example.com" {
		t.Errorf("whitespace must be trimmed: got %q", got)
	}
}

// TestValidate_DisagreeingOriginsFailTheBoot — the failure this prevents is SILENT misrouting: a
// vendor redirects a browser to the wrong host, nothing errors, and the authorization code is
// stranded. A refused start naming both keys is strictly easier to diagnose, and the check is a
// string compare before any listener is bound.
func TestValidate_DisagreeingOriginsFailTheBoot(t *testing.T) {
	c := minimalValidConfig()
	c.Server.PublicBaseURL = "https://one.example.com"
	c.Auth.ExternalBaseURL = "https://two.example.com"

	err := c.Validate()
	if err == nil {
		t.Fatal("expected the boot to be refused")
	}
	// Both keys must be named, or the operator has to guess which to edit.
	for _, want := range []string{"server.public_base_url", "auth.external_base_url", "one.example.com", "two.example.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q: %v", want, err)
		}
	}
}

func TestValidate_AgreeingOriginsAreFine(t *testing.T) {
	c := minimalValidConfig()
	c.Server.PublicBaseURL = "https://same.example.com"
	// A trailing slash on one side is not a disagreement — that would refuse a
	// boot over a cosmetic difference.
	c.Auth.ExternalBaseURL = "https://same.example.com/"
	if err := c.Validate(); err != nil {
		t.Errorf("agreeing origins must validate: %v", err)
	}
}

func TestValidate_OneKeyOnlyIsFine(t *testing.T) {
	for _, tc := range []struct{ name, server, auth string }{
		{"server only", "https://x.example.com", ""},
		{"auth only", "", "https://x.example.com"},
		{"neither", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := minimalValidConfig()
			c.Server.PublicBaseURL = tc.server
			c.Auth.ExternalBaseURL = tc.auth
			if err := c.Validate(); err != nil {
				t.Errorf("must validate: %v", err)
			}
		})
	}
}
