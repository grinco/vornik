package conversation

import (
	"sort"
	"unicode"
	"unicode/utf8"
)

// ChannelSpecific sanitisation bounds. The map is platform-supplied
// metadata (Telegram MessageThreadID, Slack team_id, GitHub
// installation_id, …) that rides an inbound ChannelMessage and is read
// back by the originating channel's Send to rebuild the reply wire shape.
// It is UNTRUSTED upstream input: unbounded entries are a memory-growth
// vector, and control bytes are a log-injection / wire-shape-corruption
// risk when a value is interpolated into a header, URL, or log line.
const (
	maxChannelSpecificEntries = 32
	maxChannelSpecificKeyLen  = 128
	maxChannelSpecificValLen  = 4096
)

// SanitizeChannelSpecific returns a bounded, control-character-free copy
// of an inbound message's ChannelSpecific map. It keeps at most
// maxChannelSpecificEntries entries (deterministically, by sorted key so
// truncation is stable), truncates keys/values to their length caps
// (rune-safe), and strips Unicode control characters from both. A nil or
// empty input returns nil; an input that sanitises to nothing returns nil.
//
// The dispatcher applies this once at ingest (ChannelReceiver.Receive) so
// every channel benefits without each implementation re-deriving the
// rules. (Security LLD review batch 3 — "treat ChannelSpecific as
// untrusted; sanitize at use".)
func SanitizeChannelSpecific(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if len(out) >= maxChannelSpecificEntries {
			break
		}
		ck := stripControl(truncateRunes(k, maxChannelSpecificKeyLen))
		if ck == "" {
			// A key that is empty or all-control after cleaning carries
			// no usable routing information; drop it.
			continue
		}
		out[ck] = stripControl(truncateRunes(m[k], maxChannelSpecificValLen))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// truncateRunes caps s to at most n runes without splitting a rune.
func truncateRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
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

// stripControl removes Unicode control characters (C0/C1, DEL) while
// preserving all printable runes, including legitimate multibyte UTF-8.
func stripControl(s string) string {
	if s == "" {
		return s
	}
	hasControl := false
	for _, r := range s {
		if unicode.IsControl(r) {
			hasControl = true
			break
		}
	}
	if !hasControl {
		return s
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
