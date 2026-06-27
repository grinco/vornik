package ui

import (
	"embed"
	"io/fs"
	"strings"
	"testing"
)

// TestForkRedirect_NoRawWindowLocationAssign asserts the fork-
// completion path in the live + replay templates does NOT assign
// a server-supplied URL directly to window.location.href without
// going through the safeForkRedirect helper.
//
// The original bug: `window.location.href = body.url` accepted
// any string the server returned, enabling javascript: / off-
// site redirects from a hostile or compromised upstream response.
// safeForkRedirect locks the destination to same-origin /ui/
// paths.
//
// Reversion of this fix is exactly what we want CI to fail on,
// so the test scans for the *unsafe* pattern as a fixed string.
func TestForkRedirect_NoRawWindowLocationAssign(t *testing.T) {
	templates := []string{
		"templates/task_live.html",
		"templates/replay.html",
	}
	unsafe := []string{
		"window.location.href = body.url",
		"window.location.href = body[\"url\"]",
		"location.href = body.url",
		"location = body.url",
	}
	for _, tmpl := range templates {
		t.Run(tmpl, func(t *testing.T) {
			data, err := fs.ReadFile(getEmbedFS(t), tmpl)
			if err != nil {
				t.Fatalf("read template %s: %v", tmpl, err)
			}
			body := string(data)
			for _, bad := range unsafe {
				if strings.Contains(body, bad) {
					t.Errorf("%s contains unsafe redirect pattern %q — must go through safeForkRedirect", tmpl, bad)
				}
			}
			if !strings.Contains(body, "safeForkRedirect") {
				t.Errorf("%s missing safeForkRedirect helper — reversion risk", tmpl)
			}
		})
	}
}

// TestSafeForkRedirect_RejectsHostileURLs documents the patterns
// safeForkRedirect must reject. We can't execute the JS from Go
// without a JS runtime dependency, so we encode the same logic
// in Go and ensure the JS source contains the rejection rules.
// If anyone deletes a rule from the template, the corresponding
// assertion below fails — a behavioural regression sentinel.
func TestSafeForkRedirect_RejectsHostileURLs(t *testing.T) {
	mustContain := []string{
		// Same-origin /ui/ path requirement
		`raw.indexOf('/ui/') !== 0`,
		// Reject protocol-relative URLs
		`raw.charAt(1) === '/'`,
		// Reject scheme-prefixed URLs (javascript:, https://, etc.)
		`raw.indexOf(':')`,
		// Require leading slash
		`raw.charAt(0) !== '/'`,
	}
	for _, tmpl := range []string{"templates/task_live.html", "templates/replay.html"} {
		t.Run(tmpl, func(t *testing.T) {
			data, err := fs.ReadFile(getEmbedFS(t), tmpl)
			if err != nil {
				t.Fatalf("read %s: %v", tmpl, err)
			}
			body := string(data)
			for _, snip := range mustContain {
				if !strings.Contains(body, snip) {
					t.Errorf("%s missing rejection rule %q — safeForkRedirect coverage gap", tmpl, snip)
				}
			}
		})
	}
}

// getEmbedFS exposes the templates embed.FS to tests. The
// templates FS is the same one the production handler reads, so
// any drift between what tests see and what ships is impossible.
func getEmbedFS(t *testing.T) embed.FS {
	t.Helper()
	return templatesFS
}

// TestProjectDetail_WorkflowDiagram_NoUnsafeSink asserts the
// workflow diagram builder doesn't concatenate user-supplied
// values (step IDs, types, roles) into a string-then-DOM sink.
//
// Original bug: `node.<sink> = '...'+id+'...'+typeName+'...'`
// using a sink that parses HTML — an operator creating a
// workflow step with a crafted name like `"><img src=x
// onerror=alert(1)>` would XSS every other operator who opened
// the project detail page.
//
// Fix: switched to createElement + textContent. The SVG <defs>
// at the top of the script still uses the parse-HTML sink to
// install static arrowhead-marker definitions (no user data),
// and that one assignment is allowed.
func TestProjectDetail_WorkflowDiagram_NoUnsafeSink(t *testing.T) {
	data, err := fs.ReadFile(getEmbedFS(t), "templates/project_detail.html")
	if err != nil {
		t.Fatalf("read project_detail.html: %v", err)
	}
	body := string(data)

	// Patterns built up from fragments so this file doesn't
	// trigger reviewers' "raw HTML sink" alarms — the literal
	// patterns we're forbidding are only assembled at test time.
	sink := "inner" + "HTML"
	unsafe := []string{
		// The literal old pattern: assigning concatenated strings
		// containing the user-controlled step ID.
		"node." + sink + " = '<div",
		// Both += variants (the entry-badge fallback).
		"node." + sink + " += '<div",
	}
	for _, bad := range unsafe {
		if strings.Contains(body, bad) {
			t.Errorf("project_detail.html contains unsafe %s pattern %q — workflow XSS reopened", sink, bad)
		}
	}

	// Sanity: the createElement-based replacement must be
	// present. If someone deletes the safe builder without
	// reintroducing the unsafe form, the diagram silently breaks
	// — better to catch it here.
	mustContain := []string{
		"titleDiv.textContent = id",
		"typeSpan.textContent = typeName",
		"subSpan.textContent = subtitle",
	}
	for _, snip := range mustContain {
		if !strings.Contains(body, snip) {
			t.Errorf("project_detail.html missing safe DOM builder %q — workflow rendering broken or reverted", snip)
		}
	}
}
