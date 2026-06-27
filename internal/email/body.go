package email

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"
)

// parseContentType is a thin wrapper around mime.ParseMediaType that
// never returns an error — malformed headers fall back to
// "text/plain" with no params. The channel's body extractor depends
// on always having a value to switch on; surfacing parse errors
// here would just add error-handling noise at every call site.
func parseContentType(header string) (mediaType string, params map[string]string) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "text/plain", map[string]string{}
	}
	mt, p, err := mime.ParseMediaType(header)
	if err != nil {
		return "text/plain", map[string]string{}
	}
	if p == nil {
		p = map[string]string{}
	}
	return mt, p
}

// decodeTransfer decodes the body bytes per Content-Transfer-Encoding.
// Slice 1 supports the three real-world encodings: 7bit / 8bit
// (passthrough), quoted-printable, and base64. Anything else (rare —
// `binary`, `x-uuencode`) falls through to passthrough rather than
// erroring; surface the raw bytes to the user and let downstream
// sanitisation handle the rest.
func decodeTransfer(enc string, body []byte) string {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "", "7bit", "8bit", "binary":
		return string(body)
	case "quoted-printable":
		dec, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err != nil {
			return string(body)
		}
		return string(dec)
	case "base64":
		// Tolerate whitespace / line wrapping by stripping before
		// decode — RFC 2045 requires it but some senders include
		// CRLFs that fail strict decoding.
		clean := stripBase64Whitespace(body)
		dec, err := base64.StdEncoding.DecodeString(string(clean))
		if err != nil {
			return string(body)
		}
		return string(dec)
	}
	return string(body)
}

// decodePart runs decodeTransfer and then transcodes the result from
// the supplied charset into UTF-8. Slice 3: the slice-1/2 path assumed
// every body part was UTF-8 once transfer-decoded, which silently
// mangled mail from any non-Western correspondent (ISO-8859-*,
// Windows-125*, GB2312, Shift_JIS, …). Now we honour the
// Content-Type charset param.
//
// Lookup is via golang.org/x/text/encoding/htmlindex, which already
// covers the full IANA + WHATWG-aliased charset table — no per-name
// switch needed. Unknown / empty charsets passthrough (the body is
// assumed UTF-8, matching the slice-1 behaviour), and any transcode
// error falls back to the pre-transcode string so we never lose the
// body entirely. UTF-8-in / UTF-8-out also short-circuits — no point
// roundtripping bytes that are already valid.
func decodePart(enc, charset string, body []byte) string {
	decoded := decodeTransfer(enc, body)
	return transcodeToUTF8(decoded, charset)
}

// transcodeToUTF8 converts s from the named charset into UTF-8 and
// returns the result. Empty/UTF-8/ASCII charsets passthrough; unknown
// labels passthrough with no error (defensive — a typo in
// Content-Type shouldn't drop the message body); transcode errors
// passthrough with the pre-transcode bytes (same logic: the body is
// always preferable to nothing).
func transcodeToUTF8(s, charset string) string {
	c := strings.ToLower(strings.TrimSpace(charset))
	if c == "" || c == "utf-8" || c == "utf8" || c == "us-ascii" || c == "ascii" {
		return s
	}
	// htmlindex.Get accepts every label the WHATWG Encoding spec
	// enumerates plus a few common aliases (iso-8859-1, latin1, …).
	enc, err := htmlindex.Get(c)
	if err != nil || enc == encoding.Nop {
		return s
	}
	// Short-circuit when the bytes happen to be valid UTF-8 already.
	// Common for ISO-8859-1 / Windows-1252 mail that only used the
	// 7-bit ASCII range — no point allocating a transformer.
	if utf8.ValidString(s) && isASCII(s) {
		return s
	}
	out, _, err := transform.String(enc.NewDecoder(), s)
	if err != nil {
		return s
	}
	return out
}

// isASCII reports whether s contains only 7-bit ASCII bytes. Used by
// transcodeToUTF8 to skip a no-op transformation on Western mail
// whose body never escapes the ASCII range.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// stripBase64Whitespace removes whitespace (space / tab / CR / LF)
// from a base64-encoded payload so encoding/base64's strict
// decoder accepts it. Mirrors RFC 2045's whitespace tolerance
// without resorting to mime.WordDecoder gymnastics.
func stripBase64Whitespace(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for _, b := range in {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		}
		out = append(out, b)
	}
	return out
}

// extractTextFromMultipart walks a multipart body and returns the
// best plaintext rendering. Strategy:
//
//   - Iterate every part once.
//   - If a text/plain part exists, return its decoded body.
//   - Otherwise, if a text/html part exists, return stripHTMLTags
//     of its decoded body.
//   - Otherwise, return the empty string — the channel records the
//     message with no body and logs a TODO for slice-2 attachment
//     handling.
//
// Nested multiparts (multipart/alternative inside multipart/mixed)
// recurse once — the common shape for HTML+text alternative with
// attachments is exactly two layers deep, and stopping at two
// avoids stack-blowing on pathological inputs.
func extractTextFromMultipart(boundary string, body []byte) string {
	return extractTextFromMultipartDepth(boundary, body, 0)
}

func extractTextFromMultipartDepth(boundary string, body []byte, depth int) string {
	const maxDepth = 3
	if depth > maxDepth {
		return ""
	}
	r := multipart.NewReader(bytes.NewReader(body), boundary)
	var htmlFallback string
	for {
		part, err := r.NextPart()
		if err != nil {
			break
		}
		partType, partParams := parseContentType(part.Header.Get("Content-Type"))
		partEnc := part.Header.Get("Content-Transfer-Encoding")
		buf, _ := io.ReadAll(part)
		_ = part.Close()

		if strings.HasPrefix(partType, "multipart/") {
			b := partParams["boundary"]
			if b == "" {
				continue
			}
			nested := extractTextFromMultipartDepth(b, buf, depth+1)
			if nested != "" {
				return nested
			}
			continue
		}
		if partType == "text/plain" {
			return decodePart(partEnc, partParams["charset"], buf)
		}
		if partType == "text/html" && htmlFallback == "" {
			htmlFallback = stripHTMLTags(decodePart(partEnc, partParams["charset"], buf))
		}
	}
	return htmlFallback
}

// stripHTMLTags renders HTML to plain text via golang.org/x/net/html.
// Slice 2 rewrite — replaces the slice-1 naive angle-bracket strip
// with a real DOM walk that:
//
//   - Skips <script>, <style>, <head>, <noscript> bodies entirely
//     (no leaked CSS / JS in the rendered text).
//   - Inserts "\n" at block-level boundaries (<p>, <div>, <br>,
//     <li>, <h1>-<h6>, <tr>, <hr>) so multi-paragraph mail renders
//     readably.
//   - Preserves text inside inline elements (<a>, <b>, <i>, <em>,
//     <strong>, <span>) by treating them as no-ops on the layout.
//   - Decodes HTML entities (&amp; → &, &nbsp; →  ) via the
//     x/net/html tokenizer's built-in entity table.
//
// On a parse error (malformed input the tokenizer can't continue
// past) we fall back to the slice-1 naive strip so a hostile or
// truncated body still produces *something* rather than an empty
// string — matches the per-message "log and continue" posture the
// rest of the channel uses.
func stripHTMLTags(in string) string {
	if in == "" {
		return ""
	}
	doc, err := html.Parse(strings.NewReader(in))
	if err != nil {
		return naiveStripHTMLTags(in)
	}
	var b strings.Builder
	walkHTMLNode(doc, &b)
	return collapseWhitespace(b.String())
}

// blockLevelTags is the set of HTML elements whose closing should
// insert a newline in the text rendering. Conservative list —
// matches the elements browsers render with `display: block` by
// default plus <br> (which gets a newline at *open*, not close).
var blockLevelTags = map[string]struct{}{
	"address":    {},
	"article":    {},
	"aside":      {},
	"blockquote": {},
	"div":        {},
	"dl":         {},
	"dt":         {},
	"dd":         {},
	"fieldset":   {},
	"figcaption": {},
	"figure":     {},
	"footer":     {},
	"form":       {},
	"h1":         {},
	"h2":         {},
	"h3":         {},
	"h4":         {},
	"h5":         {},
	"h6":         {},
	"header":     {},
	"hr":         {},
	"li":         {},
	"main":       {},
	"nav":        {},
	"ol":         {},
	"p":          {},
	"pre":        {},
	"section":    {},
	"table":      {},
	"tbody":      {},
	"thead":      {},
	"tfoot":      {},
	"tr":         {},
	"ul":         {},
}

// skipTagBodies enumerates the elements whose children we drop on
// the floor (script / style produce text content that's noise; head
// holds metadata; noscript is a fallback for JS-disabled clients
// that mirrors the visible body for the same delivery, so dropping
// it avoids double-rendering).
var skipTagBodies = map[string]struct{}{
	"script":   {},
	"style":    {},
	"head":     {},
	"noscript": {},
	"template": {},
}

// walkHTMLNode recurses through an html.Node tree, emitting text
// content to b. Block-level closing tags push a "\n"; inline tags
// pass through. CDATA / comments are ignored.
//
// Whitespace inside text nodes — including newlines and tabs — is
// treated as plain whitespace and gets collapsed to a single space
// by collapseWhitespace. Per the HTML spec a literal newline inside
// the markup is rendered as a space; only <br> and block-level
// boundaries introduce real linebreaks.
func walkHTMLNode(n *html.Node, b *strings.Builder) {
	switch n.Type {
	case html.TextNode:
		// Replace embedded newlines / tabs with spaces so
		// collapseWhitespace doesn't promote them to block-level
		// separators. The walker introduces real newlines only
		// at <br> and at block-element close.
		b.WriteString(textNodeWhitespaceToSpaces(n.Data))
		return
	case html.ElementNode:
		if _, skip := skipTagBodies[n.Data]; skip {
			return
		}
		if n.Data == "br" {
			b.WriteByte('\n')
			return
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkHTMLNode(c, b)
	}
	if n.Type == html.ElementNode {
		if _, block := blockLevelTags[n.Data]; block {
			b.WriteByte('\n')
		}
	}
}

// textNodeWhitespaceToSpaces flattens \n / \r / \t inside an HTML
// text node to spaces so the downstream collapse pass treats them
// as inline whitespace, not block-level separators.
func textNodeWhitespaceToSpaces(in string) string {
	if !strings.ContainsAny(in, "\n\r\t") {
		return in
	}
	var b strings.Builder
	b.Grow(len(in))
	for _, r := range in {
		switch r {
		case '\n', '\r', '\t':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// collapseWhitespace tightens runs of consecutive whitespace into a
// single space, then collapses runs of newlines into a single
// newline. Preserves the block-level newline separators
// walkHTMLNode inserted so paragraphs stay visible. Trims leading
// and trailing whitespace.
func collapseWhitespace(in string) string {
	var b strings.Builder
	b.Grow(len(in))
	var (
		prevSpace   bool
		prevNewline bool
	)
	for _, r := range in {
		switch r {
		case '\n':
			if !prevNewline {
				b.WriteByte('\n')
				prevNewline = true
				prevSpace = true
			}
		case ' ', '\t', '\r':
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		default:
			b.WriteRune(r)
			prevSpace = false
			prevNewline = false
		}
	}
	return strings.TrimSpace(b.String())
}

// naiveStripHTMLTags is the slice-1 fallback retained for the
// parser-failure path. Removes everything between angle brackets and
// collapses consecutive whitespace. Will mangle pathological HTML
// but produces *something* readable when the x/net/html parser
// refuses to continue.
func naiveStripHTMLTags(in string) string {
	var b strings.Builder
	inTag := false
	prevSpace := false
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case c == '<':
			inTag = true
		case c == '>' && inTag:
			inTag = false
		case inTag:
			continue
		case c == '\r':
			continue
		case c == '\n' || c == '\t' || c == ' ':
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		default:
			b.WriteByte(c)
			prevSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}

// MessageBody is the structured return value of parseMessageBody.
// Holds the decoded text rendering plus the collected attachments
// in document order (top-down through the multipart tree). The
// channel layer consumes this to populate the
// conversation.ChannelMessage envelope and to feed
// PersistAttachments.
type MessageBody struct {
	// Text is the plaintext rendering — text/plain wins when present;
	// HTML is rendered via stripHTMLTags otherwise. May be empty on
	// attachment-only messages.
	Text string

	// Attachments lists non-text parts in document order. Inline
	// images (Content-Disposition: inline) are skipped — they're
	// part of the HTML body, not user-uploaded artifacts.
	Attachments []ParsedAttachment
}

// parseMessageBody walks a (possibly multipart) RFC 5322 body and
// extracts both the text rendering and the attachment list. Slice 2
// expansion of the slice-1 extractTextBody: same text-resolution
// rules but with the attachment-collection pass running in parallel.
//
// Strategy:
//   - text/* parts: feed the first into Text (preferring text/plain
//     when both text/plain and text/html exist).
//   - non-text parts with disposition != "inline": collect into
//     Attachments.
//   - inline parts (Content-Disposition: inline): skip entirely.
//     These are typically cid:-referenced images embedded in the
//     HTML body; the LLM doesn't need them and persisting them
//     would just clutter the artifact store.
//   - nested multipart/* parts: recurse, with the text resolution
//     short-circuiting on the first non-empty plaintext result.
//
// Maximum recursion depth is bounded by the same constant the
// slice-1 extractor used (maxDepth = 3) so a pathologically deep
// multipart tree doesn't blow the stack.
func parseMessageBody(contentType, transferEnc string, body []byte) MessageBody {
	mt, params := parseContentType(contentType)
	if strings.HasPrefix(mt, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			// No boundary = malformed multipart envelope; treat the
			// payload as opaque text so we don't lose the content.
			return MessageBody{Text: decodeTransfer(transferEnc, body)}
		}
		return collectMessageBody(boundary, body, 0)
	}
	decoded := decodePart(transferEnc, params["charset"], body)
	if strings.HasPrefix(mt, "text/html") {
		return MessageBody{Text: stripHTMLTags(decoded)}
	}
	// Non-multipart non-text top-level body: rare but treat as text
	// (no attachment to extract; the channel just gets an opaque
	// body).
	return MessageBody{Text: decoded}
}

// collectMessageBody is the recursive walker parseMessageBody
// delegates to. Mirrors extractTextFromMultipartDepth's shape but
// also accumulates attachments and inline-disposition skips.
func collectMessageBody(boundary string, body []byte, depth int) MessageBody {
	const maxDepth = 3
	if depth > maxDepth {
		return MessageBody{}
	}
	r := multipart.NewReader(bytes.NewReader(body), boundary)
	out := MessageBody{}
	var (
		plainText    string
		htmlText     string
		plainTextSet bool
	)
	for {
		part, err := r.NextPart()
		if err != nil {
			break
		}
		partType, partParams := parseContentType(part.Header.Get("Content-Type"))
		partEnc := part.Header.Get("Content-Transfer-Encoding")
		disposition, dispParams := parseContentDisposition(part.Header.Get("Content-Disposition"))
		buf, _ := io.ReadAll(part)
		_ = part.Close()

		if strings.HasPrefix(partType, "multipart/") {
			b := partParams["boundary"]
			if b == "" {
				continue
			}
			nested := collectMessageBody(b, buf, depth+1)
			if nested.Text != "" && !plainTextSet {
				plainText = nested.Text
				plainTextSet = true
			}
			out.Attachments = append(out.Attachments, nested.Attachments...)
			continue
		}

		// Inline parts are part of the rendered body; skip them
		// regardless of media type.
		if strings.EqualFold(disposition, "inline") {
			continue
		}

		// Attachment branch: explicit "attachment" disposition OR a
		// non-text type without an inline disposition (defensive —
		// some MUAs omit Content-Disposition entirely on embedded
		// files).
		if strings.EqualFold(disposition, "attachment") || (!strings.HasPrefix(partType, "text/") && disposition == "") {
			decoded := []byte(decodeTransfer(partEnc, buf))
			filename := dispParams["filename"]
			if filename == "" {
				filename = partParams["name"]
			}
			filename = decodeHeader(filename)
			if strings.TrimSpace(filename) == "" {
				// Synthesise a deterministic placeholder so downstream
				// code (UI labels, artifact rows) never sees an empty
				// name. The numeric suffix mirrors the index inside
				// PersistAttachments's fallback so two anonymous
				// attachments in the same message don't collide.
				filename = synthesiseAnonymousFilename(len(out.Attachments), partType)
			}
			out.Attachments = append(out.Attachments, ParsedAttachment{
				Filename:    filename,
				ContentType: partType,
				Content:     decoded,
				SizeBytes:   int64(len(decoded)),
			})
			continue
		}

		// Text branches: prefer text/plain over text/html for the
		// rendered body.
		if partType == "text/plain" && !plainTextSet {
			plainText = decodePart(partEnc, partParams["charset"], buf)
			plainTextSet = true
			continue
		}
		if partType == "text/html" && htmlText == "" {
			htmlText = stripHTMLTags(decodePart(partEnc, partParams["charset"], buf))
			continue
		}
	}
	if plainTextSet {
		out.Text = plainText
	} else {
		out.Text = htmlText
	}
	return out
}

// synthesiseAnonymousFilename builds a stable filename for an
// attachment that didn't supply a Content-Disposition filename or
// Content-Type name. Uses the index inside the current multipart
// level + the MIME subtype as an extension hint when one's
// available.
func synthesiseAnonymousFilename(index int, mimeType string) string {
	ext := ""
	if slash := strings.LastIndex(mimeType, "/"); slash > 0 && slash < len(mimeType)-1 {
		subtype := mimeType[slash+1:]
		// Drop trailing parameter-ish chars some weird inputs leak.
		if cut := strings.IndexAny(subtype, "; \t"); cut > 0 {
			subtype = subtype[:cut]
		}
		if subtype != "" {
			ext = "." + subtype
		}
	}
	if ext == "" {
		ext = ".bin"
	}
	return fmt.Sprintf("attachment-%d%s", index, ext)
}

// parseContentDisposition is the disposition-header equivalent of
// parseContentType. Defensive on missing / malformed values.
func parseContentDisposition(header string) (disposition string, params map[string]string) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", map[string]string{}
	}
	disp, p, err := mime.ParseMediaType(header)
	if err != nil {
		return "", map[string]string{}
	}
	if p == nil {
		p = map[string]string{}
	}
	return disp, p
}

// decodeHeader runs an RFC 2047 word-decode pass over a header
// value so encoded subjects like "=?UTF-8?B?SGVsbG8=?=" render as
// "Hello". Defensive on errors — a malformed encoded-word falls
// back to the raw value.
func decodeHeader(in string) string {
	in = strings.TrimSpace(in)
	if in == "" {
		return ""
	}
	dec := new(mime.WordDecoder)
	out, err := dec.DecodeHeader(in)
	if err != nil {
		return in
	}
	return out
}
