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
			if strings.Contains(body, "Disclosure:") {
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

	if len(offenders) > 0 {
		t.Errorf(
			"these dispatcher.ChannelReceiver construction sites do not set Disclosure, "+
				"so those channels would converse with humans without the EU AI Act "+
				"Art 50(1) notice: %v", offenders)
	}
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
