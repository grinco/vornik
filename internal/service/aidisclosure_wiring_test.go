package service

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryChannelReceiverWiresTheAIDisclosure is a source-level guard.
//
// EU AI Act Art 50(1) is enforced at dispatcher.ChannelReceiver, which is
// exactly one type — but it is CONSTRUCTED at five sites, and a sixth will
// appear the next time someone adds a channel. Nothing in the type system
// forces the Disclosure field to be set: the zero value is nil, nil is
// tolerated so tests keep compiling, and a nil there means the channel
// silently talks to humans without disclosing.
//
// A unit test per channel would not catch the case that actually matters —
// the channel nobody has written yet. So this scans the tree instead: every
// `&dispatcher.ChannelReceiver{` literal outside _test.go must name
// Disclosure. Cheap, and it fails on the PR that introduces the gap rather
// than in an enforcement action.
//
// If you are here because this test failed on a new channel: add
// `Disclosure: c.AIDisclosure` to the literal. If the channel genuinely
// cannot interact with a natural person (an agent-to-agent surface such as
// A2A — see the design's §9.4 reasoning), add it to the allowlist below WITH
// the statutory reason, not just a path.
func TestEveryChannelReceiverWiresTheAIDisclosure(t *testing.T) {
	offenders := receiverLiteralsMissingField(t, "Disclosure:")
	if len(offenders) > 0 {
		t.Errorf(
			"these dispatcher.ChannelReceiver construction sites do not set Disclosure, "+
				"so those channels would converse with humans without the EU AI Act "+
				"Art 50(1) notice: %v", offenders)
	}
}

// TestEveryChannelReceiverWiresTheDisclosureMetrics is the observability half
// of the same guard.
//
// Serving the disclosure and being able to PROVE it are separate obligations:
// Art 50 requires the notice, Art 99 makes the breach enforceable, and an
// operator who cannot show a serve rate cannot demonstrate compliance — nor
// notice it silently stopping. The disclosure is served deep inside the
// receiver and nothing user-visible changes when the counters never move, so a
// nil observer is indistinguishable from a conforming deployment with no
// traffic. That is precisely the failure this scan exists to prevent.
//
// If you are here because this failed on a new channel: add
// `DisclosureMetrics: c.disclosureObserver()` to the literal.
func TestEveryChannelReceiverWiresTheDisclosureMetrics(t *testing.T) {
	offenders := receiverLiteralsMissingField(t, "DisclosureMetrics:")
	if len(offenders) > 0 {
		t.Errorf(
			"these dispatcher.ChannelReceiver construction sites do not set "+
				"DisclosureMetrics, so Art 50 serve/failure counts on those channels "+
				"are unobserved and conformity cannot be evidenced: %v", offenders)
	}
}

// receiverLiteralsMissingField scans every non-test .go file for
// `&dispatcher.ChannelReceiver{` literals that do not name the given field.
func receiverLiteralsMissingField(t *testing.T, field string) []string {
	t.Helper()
	// Paths whose ChannelReceiver literal is exempt, and why. Empty today:
	// all five shipped channels are human-facing.
	exempt := map[string]string{}

	root := repoRootForDisclosureScan(t)
	literal := regexp.MustCompile(`&dispatcher\.ChannelReceiver\{`)

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable tree entries aren't this test's business
		}
		if info.IsDir() {
			name := info.Name()
			// Skip dot-directories (.git, and the generated .vornik-export /
			// .vornik-public-clone mirrors — those are build outputs, and
			// scanning them would report the same five sites twice more).
			if name != "." && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path) //nolint:gosec // walking our own repo
		if readErr != nil {
			return nil
		}
		text := string(src)
		for _, loc := range literal.FindAllStringIndex(text, -1) {
			body, ok := compositeLiteralBody(text[loc[1]:])
			if !ok {
				continue
			}
			if strings.Contains(body, field) {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if _, allowed := exempt[rel]; allowed {
				continue
			}
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return offenders
}

// compositeLiteralBody returns the text up to the brace that closes the
// literal, tracking nesting so a nested struct doesn't end it early.
func compositeLiteralBody(s string) (string, bool) {
	depth := 1
	for i, r := range s {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i], true
			}
		}
	}
	return "", false
}

func repoRootForDisclosureScan(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 10 {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate repo root (no go.mod found walking up)")
	return ""
}
