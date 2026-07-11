// Package textutil holds small, canonical string helpers that were previously
// hand-rolled in divergent forms across the tree (audit 2026-07-09 F-2/F-3):
// rune-safe truncation (two variants — by rune count and by byte budget) and
// whitespace flattening. Centralised here so there is one tested
// implementation of each, matching the same "extract on recurrence" rule the
// repo already applied for internal/textsim.
package textutil

import (
	"strings"
	"unicode/utf8"
)

// TruncateRunes caps s to at most n runes, never splitting a multi-byte rune.
// n <= 0 returns "". This is the rune-COUNT variant (the caller cares about a
// display/character limit).
func TruncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// TruncateBytes caps s to at most maxLen BYTES, backing off to the nearest
// rune boundary so the result is never a partial rune. This is the byte-BUDGET
// variant (the caller cares about an encoded/storage size limit). maxLen <= 0
// returns "".
func TruncateBytes(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(s) <= maxLen {
		return s
	}
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Flatten collapses all runs of whitespace in s to single spaces and trims the
// ends — the `strings.Join(strings.Fields(s), " ")` idiom, named once.
func Flatten(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
