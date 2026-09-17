package service

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestChatEntrypoint_IsActuallyHung is a source contract, in the style of this
// package's other reachability lints.
//
// The config assistant's chat entrypoint has the exact shape that has produced
// five "built but not hung" defects in this codebase: a complete, gated,
// tested surface that nothing calls. Its engine half — EntrypointChat, the
// class ceiling in GateEntrypointCeiling, their tests — sat in the tree
// through the whole first release with no caller, and the plan's WP8 read as
// unwritten code when it was unwired code.
//
// A behavioural test cannot see this. The channels' own tests drive
// tryConfigAssistant and handleConfigCommand directly, so they pass whether or
// not the container ever constructs an adapter — which is precisely how the
// previous four got through.
func TestChatEntrypoint_IsActuallyHung(t *testing.T) {
	src := readServiceSource(t, "container_http.go")

	if !strings.Contains(src, "configassist.NewChatAdapter") {
		t.Fatal("nothing builds the chat adapter; both entrypoints are dormant in every deployment " +
			"no matter what their own tests say")
	}
	for _, want := range []string{"SetConfigAssistant"} {
		if !strings.Contains(src, want) {
			t.Errorf("the adapter is built and never handed to a channel (%s missing)", want)
		}
	}
	// BOTH channels, and each held to the SAME standard. The first version
	// checked Telegram with a specific call site and Slack with the bare
	// substring "c.SlackChannels", which appears in this file for unrelated
	// wiring — so deleting the Slack loop entirely would have passed
	// (review-20260915-8717 F2). The weaker assertion was on the half the
	// adapter's own comment calls drift-prone.
	if !regexp.MustCompile(`range c\.SlackChannels\s*\{[^}]*SetConfigAssistant\(`).MatchString(src) {
		t.Error("the Slack channels never receive the adapter (no loop over c.SlackChannels " +
			"that calls SetConfigAssistant)")
	}
	if !strings.Contains(src, "c.TelegramBot.SetConfigAssistant") {
		t.Error("the Telegram bot never receives the adapter")
	}

	// The GATING RELATIONSHIP, not two substrings that happen to co-occur.
	// Asserting only that both appear somewhere in the file passes against a
	// refactor that builds the adapter unconditionally and gates only the
	// handing-out — which advertises the entrypoint and then refuses every
	// call, the shape the container comment calls worse than not advertising
	// it at all (F2).
	gated := regexp.MustCompile(
		`if c\.Config\.ConfigAssistant\.ChatEntrypoint \{[^}]*configassist\.NewChatAdapter\(`)
	if !gated.MatchString(src) {
		t.Error("the adapter is not constructed INSIDE the ChatEntrypoint gate; a deployment " +
			"that has not opened the entrypoint would advertise a surface refusing every request")
	}
}

// TestChatAdapter_IsTheOnlyConstructionOfAChatRequest keeps the entrypoint
// value out of the channels' hands. A channel that could name its own
// entrypoint could name an operator one, and the class ceiling is a property
// OF the entrypoint — so choosing it is choosing your own authority.
func TestChatAdapter_IsTheOnlyConstructionOfAChatRequest(t *testing.T) {
	entrypointLiteral := regexp.MustCompile(`EntrypointChat|EntrypointAgent|EntrypointREST|EntrypointCLI|EntrypointConsole`)
	for _, dir := range []string{"../slack", "../telegram"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") {
				continue
			}
			body, err := os.ReadFile(dir + "/" + name)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if entrypointLiteral.Match(body) {
				t.Errorf("%s/%s names a configassist entrypoint constant; the channels must hand "+
					"the adapter a request and let it fix the entrypoint, because the entrypoint "+
					"decides the class ceiling", dir, name)
			}
		}
	}
}

func readServiceSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", name, err)
	}
	return string(b)
}

// TestLateWiring_UsesSettersNotOptions catches the class of defect that has now
// appeared THREE times in one feature, each time in a new place.
//
// api.NewServer is called at a fixed point in container init. Anything the
// container learns AFTER that — the assistant engine is read back off the built
// server — cannot be wired with a ServerOption, because the option list has
// already been consumed. Appending to it there compiles, reads correctly, and
// does nothing.
//
// That is precisely how the Telegram link-code redeemer ended up wired to nil
// and answering "not valid" to every code the UI issued, and I wrote the same
// bug again for the healing comparison window while building it. No behavioural
// test sees it: the feature's own tests drive the handler directly and pass
// against a server that was never wired.
func TestLateWiring_UsesSettersNotOptions(t *testing.T) {
	src := readServiceSource(t, "container_http.go")

	newServer := strings.Index(src, "api.NewServer(apiOpts...)")
	if newServer < 0 {
		t.Fatal("api.NewServer(apiOpts...) not found; this contract no longer knows where the " +
			"option list stops being read")
	}
	// Everything after that point may not append to apiOpts.
	tail := src[newServer:]
	if i := strings.Index(tail, "apiOpts = append("); i >= 0 {
		line := tail[i:]
		if nl := strings.IndexByte(line, '\n'); nl > 0 {
			line = line[:nl]
		}
		t.Errorf("apiOpts is appended to AFTER api.NewServer has consumed it, so the option is "+
			"dead wiring:\n\t%s\nUse a setter on the built server instead — nothing is served "+
			"until init returns", strings.TrimSpace(line))
	}
}
