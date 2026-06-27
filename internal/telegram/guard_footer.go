package telegram

import (
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/outputguard"
)

// GuardFooterPostprocessor returns a dispatcher.ChannelReceiver
// ResultPostprocessor closure that appends Telegram's one-line
// guard-finding footer to the assistant reply text. Exported so
// the service container can wire it into the slice-2 Telegram
// ChannelReceiver without taking a dependency on this package's
// internal renderer.
func GuardFooterPostprocessor() func(dispatcher.Result) string {
	return func(r dispatcher.Result) string {
		footer := renderGuardFooter(r.GuardWarnings)
		if footer == "" {
			return r.Text
		}
		if r.Text == "" {
			return footer
		}
		return r.Text + "\n\n" + footer
	}
}

// renderGuardFooter produces a one-line operator-facing summary
// of output-guard findings, appended to the assistant reply so
// the user knows the dispatcher caught adversarial content in a
// tool result this turn.
//
// Examples:
//
//	"⚠ Output guard flagged 1 finding on web_fetch: injection_instruction (auto-redacted)"
//	"⚠ Output guard flagged 2 findings on web_fetch, file_read: encoded_payload, injection_instruction"
//
// Empty input returns empty string — happy-path tool calls leave
// no footer so the chat stays clean.
//
// INFO-ONLY FILTER. Telegram's chat surface is operator-facing and
// should only show actionable findings (Warn+). Pure-Info warnings
// (e.g. encoded_payload triggered by a long URL query string in a
// scraper request) stay in the audit log + Result.GuardWarnings for
// the UI, but they don't reach the chat footer here. A warning that
// carries any Warn+ kind alongside an Info kind is still shown — we
// only drop the warnings whose MaxSeverity == SeverityInfo (which
// implies every kind in the warning was Info-rank).
//
// Telegram's sendMessage strips parse_mode (see CLAUDE.md feedback
// memory), so the prefix uses plain unicode rather than markdown.
func renderGuardFooter(warnings []dispatcher.GuardWarning) string {
	if len(warnings) == 0 {
		return ""
	}
	// Drop pure-Info warnings. MaxSeverity == SeverityInfo means
	// every finding in this warning fired at Info rank, so the
	// whole warning is noise for the chat surface. Warnings that
	// mix Info kinds with Warn+ kinds keep their Warn+ MaxSeverity
	// and survive this filter — operators still see the Info kind
	// listed alongside the actionable one.
	filtered := make([]dispatcher.GuardWarning, 0, len(warnings))
	for _, w := range warnings {
		if w.MaxSeverity == outputguard.SeverityInfo {
			continue
		}
		filtered = append(filtered, w)
	}
	if len(filtered) == 0 {
		return ""
	}
	tools := make([]string, 0, len(filtered))
	kindsSet := map[string]struct{}{}
	redacted := false
	maxSev := outputguard.Severity("")
	rank := func(s outputguard.Severity) int {
		switch s {
		case outputguard.SeverityHigh:
			return 3
		case outputguard.SeverityWarn:
			return 2
		case outputguard.SeverityInfo:
			return 1
		}
		return 0
	}
	for _, w := range filtered {
		tools = append(tools, w.Tool)
		if w.Redacted {
			redacted = true
		}
		if rank(w.MaxSeverity) > rank(maxSev) {
			maxSev = w.MaxSeverity
		}
		for _, k := range w.Kinds {
			kindsSet[k] = struct{}{}
		}
	}
	sort.Strings(tools)
	tools = dedupePreserveOrder(tools)
	kinds := make([]string, 0, len(kindsSet))
	for k := range kindsSet {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	findingsWord := "finding"
	if len(filtered) > 1 {
		findingsWord = "findings"
	}
	tail := ""
	if redacted {
		tail = " (auto-redacted)"
	}
	return fmt.Sprintf("⚠ Output guard flagged %d %s on %s: %s%s",
		len(filtered), findingsWord,
		strings.Join(tools, ", "),
		strings.Join(kinds, ", "),
		tail,
	)
}

// dedupePreserveOrder removes adjacent duplicates from a sorted
// slice. Plain set-then-sort would work too, but doing it on the
// already-sorted slice is one pass and zero allocations beyond
// the input.
func dedupePreserveOrder(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}
