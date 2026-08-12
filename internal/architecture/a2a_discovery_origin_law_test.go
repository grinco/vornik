package architecture

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A2A agent cards must advertise the daemon's CANONICAL public origin.
//
// Before 6eacf5fb the card's BaseURLProvider was built from
// `c.Config.Telegram.WebUIBaseURL` — a setting that exists to render onboarding
// links for a chat channel. It can legitimately be empty, or point somewhere
// entirely different from the daemon's public origin. Machines discovering an
// agent then read a base URL that does not address the API they must submit
// tasks to, so discovery succeeds and every subsequent call goes nowhere.
//
// The value was also captured ONCE at init, so a config reload could not correct
// it without a daemon restart.
//
// WHY A SOURCE-LEVEL LAW RATHER THAN A BEHAVIOUR TEST. The provider itself is
// unit-tested (TestA2ABaseURLProvider_UsesLiveCanonicalOrigin), but the defect
// was in the WIRING — which provider the handler literal receives inside
// initHTTPServer. Reverting exactly that line leaves the whole suite green,
// because container_http_edition_test.go documents why this package tests
// initHTTPServer's products rather than calling it: an end-to-end container
// needs a live database and significant scaffolding.
//
// So this pins the line the bug was on. It is deliberately narrow: it does not
// care how the origin is computed, only that A2A discovery is not sourced from a
// chat channel's link setting.

func TestA2ADiscoveryDoesNotUseTelegramBaseURL(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "internal", "service", "container_http.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// The A2A handler literal, from `a2a.Handler{` to its closing brace at the
	// same indentation. Scoped so an unrelated Telegram reference elsewhere in
	// the file cannot trip the law, and so a Telegram reference moving INTO the
	// literal cannot escape it.
	block := a2aHandlerLiteral(t, string(src))

	if strings.Contains(block, "Telegram.") {
		t.Errorf("the A2A handler is configured from a Telegram setting:\n%s\n\n"+
			"Agent cards are consumed by machines that then POST tasks to the "+
			"advertised base URL. telegram.web_ui_base_url is an onboarding-link "+
			"setting: it may be empty or point elsewhere, and a card carrying it "+
			"sends clients to an address that does not serve the API.", block)
	}
	if !strings.Contains(block, "a2aBaseURLProvider()") {
		t.Errorf("the A2A handler no longer takes its base URL from "+
			"c.a2aBaseURLProvider():\n%s\n\nThat provider reads the canonical public "+
			"origin live, so a config reload fixes future discovery documents "+
			"without a daemon restart. A value captured once at init cannot.", block)
	}
}

// a2aHandlerLiteral returns the source of the `&a2a.Handler{...}` composite
// literal, or fails the test when the shape it pins no longer exists — because a
// law that silently matches nothing is not a law.
func a2aHandlerLiteral(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "&a2a.Handler{")
	if start < 0 {
		t.Fatal("no &a2a.Handler{ literal in container_http.go — if the A2A handler " +
			"moved, move this law with it rather than deleting it")
	}
	rest := src[start:]
	// Close on the first line that is exactly the literal's closing brace.
	end := regexp.MustCompile(`(?m)^\t\t\}`).FindStringIndex(rest)
	if end == nil {
		t.Fatal("could not find the end of the &a2a.Handler literal")
	}
	return rest[:end[1]]
}
