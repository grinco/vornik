package memory

import (
	"strings"
)

// buildEmbedContext returns the contextualisation prefix prepended to a
// chunk's content before it goes to the embedding endpoint. Disambiguates
// two chunks that share vocabulary but belong to different sources or
// sections — e.g. a "deploy script" chunk in `research/old.md` vs
// `decisions/new.md`. Stored content stays raw; only the embed input
// is prefixed, so search/display/dedup are unaffected.
//
// Format is two short labelled lines so the embedding model treats them
// as routing hints, not as content the user asked about.
//
// Empty prefix when neither source nor section is informative (e.g. an
// unnamed artifact with no markdown headings). Returning "" keeps the
// embed input identical to the raw content for those cases.
func buildEmbedContext(sourceName, content string) string {
	source := strings.TrimSpace(sourceName)
	section := firstHeading(content)
	if source == "" && section == "" {
		return ""
	}
	var b strings.Builder
	if source != "" {
		b.WriteString("Source: ")
		b.WriteString(source)
		b.WriteByte('\n')
	}
	if section != "" {
		b.WriteString("Section: ")
		b.WriteString(section)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}

// firstHeading scans the first 20 lines of content for a markdown H1 or
// H2 and returns its text. Mirrors extractContentTitle's heading logic
// from repository.go but standalone here so the embedder doesn't import
// repository internals.
func firstHeading(content string) string {
	const maxLines = 20
	scanned := 0
	for len(content) > 0 && scanned < maxLines {
		nl := strings.IndexByte(content, '\n')
		var line string
		if nl < 0 {
			line = content
			content = ""
		} else {
			line = content[:nl]
			content = content[nl+1:]
		}
		scanned++
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			if t := strings.TrimSpace(strings.TrimPrefix(line, "## ")); t != "" {
				return t
			}
		}
		if strings.HasPrefix(line, "# ") {
			if t := strings.TrimSpace(strings.TrimPrefix(line, "# ")); t != "" {
				return t
			}
		}
	}
	return ""
}

// applyEmbedContext returns the embed-ready input string for one chunk.
// Equivalent to buildEmbedContext(...) + content. Pulled out so call
// sites read clearly and so tests can assert on the composed form.
func applyEmbedContext(sourceName, content string) string {
	prefix := buildEmbedContext(sourceName, content)
	if prefix == "" {
		return content
	}
	return prefix + content
}
