package ui

import (
	"bytes"
	"strings"
	"testing"
)

// TestPageFooterVersionLink verifies the daemon version renders in the footer as
// a changelog link when WithVersion is set, and that an unset version renders
// nothing (graceful). The version reaches the template via the server-bound
// "appVersion" FuncMap entry — no page data threading.
func TestPageFooterVersionLink(t *testing.T) {
	s := NewServer(WithVersion("2026.7.4-52-gebe09761"))
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "pageFooter", nil); err != nil {
		t.Fatalf("render pageFooter: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Vornik 2026.7.4-52-gebe09761") {
		t.Errorf("footer must show the version, got: %q", out)
	}
	if !strings.Contains(out, `href="https://docs.vornik.io/changelog"`) {
		t.Errorf("footer must link to the changelog, got: %q", out)
	}
	if !strings.Contains(out, `rel="noopener"`) {
		t.Errorf("external link must carry rel=noopener, got: %q", out)
	}

	// Lazy provider (the daemon's path): WithVersionFunc is read at render time,
	// so a version set AFTER server construction still shows. Regression for the
	// construction-order bug where initHTTPServer runs before SetVersion.
	var ver string
	sLazy := NewServer(WithVersionFunc(func() string { return ver }))
	ver = "2026.7.4-54-gcf5738cc" // set AFTER NewServer, mimicking SetVersion timing
	var bufLazy bytes.Buffer
	if err := sLazy.templates.ExecuteTemplate(&bufLazy, "pageFooter", nil); err != nil {
		t.Fatalf("render pageFooter (lazy): %v", err)
	}
	if !strings.Contains(bufLazy.String(), "Vornik 2026.7.4-54-gcf5738cc") {
		t.Errorf("lazy version provider must be read at render time, got: %q", bufLazy.String())
	}

	// Unset version → footer renders nothing (no empty <footer>, no dangling link).
	s2 := NewServer()
	var buf2 bytes.Buffer
	if err := s2.templates.ExecuteTemplate(&buf2, "pageFooter", nil); err != nil {
		t.Fatalf("render pageFooter (empty version): %v", err)
	}
	if strings.TrimSpace(buf2.String()) != "" {
		t.Errorf("empty version must render nothing, got: %q", buf2.String())
	}
}
