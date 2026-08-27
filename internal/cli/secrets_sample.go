package cli

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"vornik.io/vornik/internal/secrets"
)

// Rule scoping and safe sampling for `vornikctl secrets scan-history`.
//
// WHY THIS EXISTS. --apply used to rewrite EVERY finding the detector produced.
// Measured on this deployment that is 24 strong, typed credential findings
// against 7,270 heuristic ones — `entropy` and `generic_kv` — so the operator's
// only choices were to leave real credentials at rest or irreversibly rewrite
// thousands of hashes, ids and base64 blobs that in an audit table ARE the
// record. That is why the historical purge sat open for a week.
//
// Design of record:
// https://docs.vornik.io
//
// The masking here is a security boundary, not formatting. Read maskedContext's
// comment before changing it.

// ruleSelection decides which findings get written back. Display and counting
// always cover everything; only the redaction set narrows.
type ruleSelection struct {
	spec string
	all  bool
	// names is the explicit set when the spec is neither "strong" nor "all".
	names map[string]bool
	// strong selects every non-heuristic type, including ones added later. A
	// name-set snapshot would silently exclude a pattern shipped after this
	// run was scripted, which is the wrong direction for a credential scanner.
	strong bool
}

const (
	ruleSpecStrong = "strong"
	ruleSpecAll    = "all"
)

// parseRuleSelection resolves the --rules value against the in-force corpus.
//
// An unrecognised name is a hard error naming the valid set. The alternative —
// an empty selection — would redact nothing and report success, which is the
// silent-control failure this codebase has spent two designs retiring.
func parseRuleSelection(spec string, corpus []secrets.Pattern) (ruleSelection, error) {
	switch strings.TrimSpace(spec) {
	case "", ruleSpecStrong:
		return ruleSelection{spec: ruleSpecStrong, strong: true}, nil
	case ruleSpecAll:
		return ruleSelection{spec: ruleSpecAll, all: true}, nil
	}

	valid := validRuleNames(corpus)
	sel := ruleSelection{spec: spec, names: map[string]bool{}}
	for _, raw := range strings.Split(spec, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if !valid[name] {
			return ruleSelection{}, fmt.Errorf(
				"unknown rule %q; valid values are %q, %q, or a comma-separated list of: %s",
				name, ruleSpecStrong, ruleSpecAll, strings.Join(sortedRuleNames(valid), ", "))
		}
		sel.names[name] = true
	}
	if len(sel.names) == 0 {
		return ruleSelection{}, fmt.Errorf("--rules %q selects no rules", spec)
	}
	return sel, nil
}

// validRuleNames is the corpus plus entropy, which is a detector MODE rather
// than a Pattern and so is absent from EffectivePatterns — but is nonetheless
// the single largest finding type, and the one an operator most needs to name.
func validRuleNames(corpus []secrets.Pattern) map[string]bool {
	valid := map[string]bool{secrets.FindingTypeEntropy: true}
	for _, p := range corpus {
		valid[p.Name] = true
	}
	return valid
}

func sortedRuleNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// selects reports whether a finding type is in the write-back set.
func (s ruleSelection) selects(findingType string) bool {
	switch {
	case s.all:
		return true
	case s.strong:
		return !secrets.IsHeuristicType(findingType)
	default:
		return s.names[findingType]
	}
}

// selectFindings keeps only what will actually be redacted. Order is preserved
// because secrets.Redact requires findings in offset order.
func selectFindings(findings []secrets.Finding, sel ruleSelection) []secrets.Finding {
	out := make([]secrets.Finding, 0, len(findings))
	for _, f := range findings {
		if sel.selects(f.Type) {
			out = append(out, f)
		}
	}
	return out
}

// newRunSalt returns 32 random bytes that live only for this process.
//
// The identity token printed beside a sample is HMAC'd under this, NOT a bare
// sha256 of the matched value. A bare hash prefix is not reconstructable but it
// IS confirmable: anyone holding a candidate credential can hash it and check
// the output, so every archived sample would become a lookup table answering
// "was THIS secret in the audit log?" — long after the rows were purged.
//
// A per-run salt keeps the only correlation an operator actually needs (are
// these matches the same value, within this run) and makes the output inert the
// moment the process exits.
func newRunSalt() []byte {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		// crypto/rand failing is not survivable for a security control: a
		// predictable salt silently restores the oracle the salt exists to
		// close. Panic rather than degrade.
		panic(fmt.Sprintf("secrets scan-history: cannot generate run salt: %v", err))
	}
	return salt
}

// identityToken is the display-only correlation handle for a matched value.
// crypto/hmac, not a bare hash.Hash over salt||match.
func identityToken(salt []byte, match string) string {
	mac := hmac.New(sha256.New, salt)
	_, _ = mac.Write([]byte(match))
	return hex.EncodeToString(mac.Sum(nil))[:8]
}

// maskedContext renders the text around one finding with EVERY overlapping
// finding replaced by its typed marker.
//
// SECURITY BOUNDARY — read before editing. The masking is driven by the
// detector's Start/End offsets over the ORIGINAL text, never by re-scanning the
// window substring. Re-scanning would miss a credential whose span begins
// before the window or ends after it and would emit the visible half verbatim.
// Adjacency matters too: two findings meeting at one offset must both mask, so
// the overlap test is half-open (start < winEnd && end > winStart) rather than
// inclusive on either side.
//
// The loop strides to f.End after writing a placeholder, which lands exactly
// past the span just masked — so no byte of a masked finding is ever emitted,
// whatever the topology. Code review 2026-08-27 read this as skipping an
// EARLIER finding's bytes; it does not, and
// TestMaskedContextHandlesEveryOverlapTopology pins adjacent, nested and
// partially-overlapping spans in both slice orders.
func maskedContext(text string, all []secrets.Finding, target secrets.Finding) string {
	winStart := target.Start - sampleContextWindow
	if winStart < 0 {
		winStart = 0
	}
	winEnd := target.End + sampleContextWindow
	if winEnd > len(text) {
		winEnd = len(text)
	}
	// Snap to rune boundaries. The window is a byte offset arithmetic, so on
	// non-ASCII text it lands mid-rune roughly two times in three and emits
	// replacement garbage — into the --json output as well, where it is
	// invalid UTF-8 rather than merely ugly. Widening (start) and narrowing
	// (end) both move AWAY from the match, so neither can uncover a byte the
	// masking would otherwise hide.
	for winStart > 0 && !utf8.RuneStart(text[winStart]) {
		winStart--
	}
	for winEnd < len(text) && !utf8.RuneStart(text[winEnd]) {
		winEnd--
	}

	var b strings.Builder
	if winStart > 0 {
		b.WriteString("…")
	}
	for i := winStart; i < winEnd; {
		if f, ok := findingAt(all, i, winStart, winEnd); ok {
			b.WriteString("<REDACTED:" + f.Type + ">")
			// Skip to the end of the finding, clamped to the window.
			i = f.End
			if i > winEnd {
				break
			}
			continue
		}
		b.WriteByte(text[i])
		i++
	}
	if winEnd < len(text) {
		b.WriteString("…")
	}
	return b.String()
}

// findingAt returns the first finding in slice order covering offset i,
// considering only findings that overlap the window at all. The i >= f.Start
// bound is load-bearing: with i > f.Start the first byte of every match is
// emitted verbatim before masking begins (found by mutation-testing,
// 2026-08-27; TestMaskedContextIsByteExact catches it).
func findingAt(all []secrets.Finding, i, winStart, winEnd int) (secrets.Finding, bool) {
	for _, f := range all {
		if f.Start >= winEnd || f.End <= winStart {
			continue // does not overlap the window
		}
		if i >= f.Start && i < f.End {
			return f, true
		}
	}
	return secrets.Finding{}, false
}

// sampleLine renders one masked example. The matched bytes never appear: the
// value is identified by a per-run token and its length, and the context is
// masked by offset.
func sampleLine(salt []byte, rowID, tool, text string, all []secrets.Finding, target secrets.Finding) string {
	return fmt.Sprintf("  %-12s row=%s tool=%s len=%d tok=%s\n    %s",
		target.Type, rowID, tool, len(target.Match), identityToken(salt, target.Match),
		maskedContext(text, all, target))
}

// sampleContextWindow is how much surrounding text a sample carries. Enough to
// recognise `"test_case_ids": [...]` around a match; short enough that a sample
// listing stays readable.
const sampleContextWindow = 48
