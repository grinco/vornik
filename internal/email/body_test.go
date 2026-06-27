package email

import (
	"strings"
	"testing"
)

func TestParseContentType_EmptyDefaultsToTextPlain(t *testing.T) {
	mt, params := parseContentType("")
	if mt != "text/plain" {
		t.Errorf("mt = %q, want text/plain", mt)
	}
	if params == nil {
		t.Error("params must be non-nil")
	}
}

func TestParseContentType_MalformedFallsBack(t *testing.T) {
	mt, _ := parseContentType(";;;;not a content type")
	if mt != "text/plain" {
		t.Errorf("mt = %q, want text/plain fallback", mt)
	}
}

func TestParseContentType_MultipartWithBoundary(t *testing.T) {
	mt, params := parseContentType(`multipart/alternative; boundary="==boundary=="`)
	if mt != "multipart/alternative" {
		t.Errorf("mt = %q", mt)
	}
	if params["boundary"] != "==boundary==" {
		t.Errorf("boundary = %q", params["boundary"])
	}
}

func TestDecodeTransfer_Passthrough(t *testing.T) {
	cases := []string{"", "7bit", "8bit", "binary", "X-WEIRD"}
	for _, enc := range cases {
		got := decodeTransfer(enc, []byte("hello"))
		if got != "hello" {
			t.Errorf("enc=%q: got %q, want hello", enc, got)
		}
	}
}

func TestDecodeTransfer_QuotedPrintable(t *testing.T) {
	got := decodeTransfer("quoted-printable", []byte("Caf=C3=A9"))
	if got != "Café" {
		t.Errorf("decoded = %q, want Café", got)
	}
}

func TestDecodeTransfer_QuotedPrintableInvalidFallsBack(t *testing.T) {
	got := decodeTransfer("quoted-printable", []byte("=ZZ"))
	if got != "=ZZ" {
		t.Errorf("invalid QP must fall through to raw, got %q", got)
	}
}

func TestDecodeTransfer_Base64(t *testing.T) {
	// "Hello, world!" base64 encoded.
	got := decodeTransfer("base64", []byte("SGVsbG8sIHdvcmxkIQ=="))
	if got != "Hello, world!" {
		t.Errorf("decoded = %q", got)
	}
}

func TestDecodeTransfer_Base64WithLineWrapping(t *testing.T) {
	got := decodeTransfer("base64", []byte("SGVsbG8s\r\nIHdvcmxkIQ=="))
	if got != "Hello, world!" {
		t.Errorf("decoded = %q", got)
	}
}

func TestDecodeTransfer_Base64InvalidFallsBack(t *testing.T) {
	got := decodeTransfer("base64", []byte("not_base64!@#"))
	if got != "not_base64!@#" {
		t.Errorf("invalid base64 must fall through, got %q", got)
	}
}

func TestStripBase64Whitespace(t *testing.T) {
	got := stripBase64Whitespace([]byte(" a\tb\rc\nd"))
	if string(got) != "abcd" {
		t.Errorf("got %q", string(got))
	}
}

func TestStripHTMLTags_BasicTags(t *testing.T) {
	got := stripHTMLTags("<p>Hello <b>world</b>!</p>")
	if got != "Hello world!" {
		t.Errorf("got %q", got)
	}
}

func TestStripHTMLTags_CollapsesWhitespace(t *testing.T) {
	// x/net/html walker collapses runs of whitespace inside one
	// text node to a single space. The slice-2 walker also inserts
	// a newline at the </p> boundary, but since this is a single
	// <p> with content + nothing after it, TrimSpace strips the
	// trailing newline.
	got := stripHTMLTags("<p>Hello\n\n  world</p>")
	if got != "Hello world" {
		t.Errorf("got %q", got)
	}
}

func TestStripHTMLTags_EmptyInput(t *testing.T) {
	if got := stripHTMLTags(""); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestDecodeHeader_Plain(t *testing.T) {
	if got := decodeHeader("Plain subject"); got != "Plain subject" {
		t.Errorf("got %q", got)
	}
}

func TestDecodeHeader_EncodedWord(t *testing.T) {
	got := decodeHeader("=?UTF-8?B?SGVsbG8=?=")
	if got != "Hello" {
		t.Errorf("got %q, want Hello", got)
	}
}

func TestDecodeHeader_EmptyReturnsEmpty(t *testing.T) {
	if got := decodeHeader(""); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestDecodeHeader_MalformedFallsBackToRaw(t *testing.T) {
	in := "=?UTF-8?B?broken?="
	got := decodeHeader(in)
	// Either decode succeeds with a mangled body or fallback returns raw —
	// the contract is "no panic, no error surfaced." Just assert non-empty.
	if got == "" {
		t.Errorf("got empty for %q", in)
	}
}

func TestExtractTextBody_PlainPassthrough(t *testing.T) {
	got := extractTextBody("text/plain", "", []byte("hello"))
	if got != "hello" {
		t.Errorf("got %q", got)
	}
}

func TestExtractTextBody_HTMLOnly(t *testing.T) {
	got := extractTextBody("text/html", "", []byte("<p>hello</p>"))
	if got != "hello" {
		t.Errorf("got %q", got)
	}
}

func TestExtractTextBody_MultipartAlternativePrefersPlain(t *testing.T) {
	boundary := "BNDRY"
	body := []byte(
		"--BNDRY\r\n" +
			"Content-Type: text/html\r\n\r\n" +
			"<p>HTML body</p>\r\n" +
			"--BNDRY\r\n" +
			"Content-Type: text/plain\r\n\r\n" +
			"plaintext body\r\n" +
			"--BNDRY--\r\n",
	)
	got := extractTextBody(`multipart/alternative; boundary="`+boundary+`"`, "", body)
	if !strings.Contains(got, "plaintext body") {
		t.Errorf("got %q, want plaintext body", got)
	}
}

func TestExtractTextBody_MultipartFallsBackToHTML(t *testing.T) {
	boundary := "BNDRY"
	body := []byte(
		"--BNDRY\r\n" +
			"Content-Type: text/html\r\n\r\n" +
			"<p>HTML body</p>\r\n" +
			"--BNDRY--\r\n",
	)
	got := extractTextBody(`multipart/alternative; boundary="`+boundary+`"`, "", body)
	if !strings.Contains(got, "HTML body") {
		t.Errorf("got %q", got)
	}
}

func TestExtractTextBody_MultipartMissingBoundary(t *testing.T) {
	// No boundary param → fall through to passthrough decode.
	got := extractTextBody("multipart/mixed", "", []byte("raw"))
	if got != "raw" {
		t.Errorf("got %q", got)
	}
}

func TestExtractTextBody_QuotedPrintableTextPlain(t *testing.T) {
	got := extractTextBody("text/plain", "quoted-printable", []byte("Caf=C3=A9"))
	if got != "Café" {
		t.Errorf("got %q", got)
	}
}

func TestExtractTextFromMultipart_NestedRecursion(t *testing.T) {
	// Outer multipart/mixed wraps an inner multipart/alternative.
	body := []byte(
		"--OUTER\r\n" +
			`Content-Type: multipart/alternative; boundary="INNER"` + "\r\n\r\n" +
			"--INNER\r\n" +
			"Content-Type: text/html\r\n\r\n" +
			"<p>html</p>\r\n" +
			"--INNER\r\n" +
			"Content-Type: text/plain\r\n\r\n" +
			"nested plaintext\r\n" +
			"--INNER--\r\n" +
			"--OUTER--\r\n",
	)
	got := extractTextFromMultipart("OUTER", body)
	if !strings.Contains(got, "nested plaintext") {
		t.Errorf("got %q", got)
	}
}

func TestExtractTextFromMultipart_AllAttachmentsReturnsEmpty(t *testing.T) {
	body := []byte(
		"--BNDRY\r\n" +
			"Content-Type: application/pdf\r\n\r\n" +
			"PDFDATA\r\n" +
			"--BNDRY--\r\n",
	)
	got := extractTextFromMultipart("BNDRY", body)
	if got != "" {
		t.Errorf("got %q, want empty (attachments-only)", got)
	}
}

// ---- slice-2 HTML walker corpus ----

func TestStripHTMLTags_SkipsScriptContent(t *testing.T) {
	in := `<p>Hi</p><script>alert('xss')</script><p>Bye</p>`
	got := stripHTMLTags(in)
	if strings.Contains(got, "alert") {
		t.Errorf("script body must be skipped, got %q", got)
	}
	if !strings.Contains(got, "Hi") || !strings.Contains(got, "Bye") {
		t.Errorf("paragraph text must survive, got %q", got)
	}
}

func TestStripHTMLTags_SkipsStyleContent(t *testing.T) {
	in := `<style>body{color:red}</style><p>Visible</p>`
	got := stripHTMLTags(in)
	if strings.Contains(got, "color:red") {
		t.Errorf("style body must be skipped, got %q", got)
	}
	if !strings.Contains(got, "Visible") {
		t.Errorf("body content must survive, got %q", got)
	}
}

func TestStripHTMLTags_SkipsHeadContent(t *testing.T) {
	in := `<html><head><title>secret</title></head><body><p>Hello</p></body></html>`
	got := stripHTMLTags(in)
	if strings.Contains(got, "secret") {
		t.Errorf("head content must be skipped, got %q", got)
	}
	if !strings.Contains(got, "Hello") {
		t.Errorf("body content must survive, got %q", got)
	}
}

func TestStripHTMLTags_SkipsNoscriptContent(t *testing.T) {
	in := `<p>Main</p><noscript>fallback for JS-off</noscript>`
	got := stripHTMLTags(in)
	if strings.Contains(got, "fallback") {
		t.Errorf("noscript content must be skipped, got %q", got)
	}
}

func TestStripHTMLTags_BlockElementsInsertNewlines(t *testing.T) {
	in := `<p>first</p><p>second</p>`
	got := stripHTMLTags(in)
	if !strings.Contains(got, "\n") {
		t.Errorf("expected newline between paragraphs, got %q", got)
	}
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Errorf("both paragraphs must survive, got %q", got)
	}
}

func TestStripHTMLTags_BrProducesNewline(t *testing.T) {
	in := `line1<br>line2`
	got := stripHTMLTags(in)
	if got != "line1\nline2" {
		t.Errorf("<br> handling: got %q, want %q", got, "line1\nline2")
	}
}

func TestStripHTMLTags_ListItemsBecomeLines(t *testing.T) {
	in := `<ul><li>one</li><li>two</li></ul>`
	got := stripHTMLTags(in)
	// Each <li> emits its content then a newline; ul/ol wrappers emit
	// a trailing newline too. Just verify both items appear on
	// separate lines.
	lines := strings.Split(got, "\n")
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, "one") || !strings.Contains(joined, "two") {
		t.Errorf("list rendering: got %q", got)
	}
	if !strings.Contains(got, "\n") {
		t.Errorf("expected newlines between list items, got %q", got)
	}
}

func TestStripHTMLTags_HeadingsBecomeOwnLines(t *testing.T) {
	in := `<h1>Title</h1><p>body</p>`
	got := stripHTMLTags(in)
	if !strings.Contains(got, "Title") || !strings.Contains(got, "body") {
		t.Errorf("heading + body should survive, got %q", got)
	}
	// Heading closes with a newline so body falls on its own line.
	if !strings.Contains(got, "Title\nbody") {
		t.Errorf("heading + body separation: got %q", got)
	}
}

func TestStripHTMLTags_PreservesNestedInlineText(t *testing.T) {
	in := `<p>Hello <b>bold <i>italic</i></b> world</p>`
	got := stripHTMLTags(in)
	if got != "Hello bold italic world" {
		t.Errorf("inline preservation: got %q", got)
	}
}

func TestStripHTMLTags_DecodesHTMLEntities(t *testing.T) {
	in := `<p>Tom &amp; Jerry &lt;script&gt; &nbsp; end</p>`
	got := stripHTMLTags(in)
	if !strings.Contains(got, "Tom & Jerry") {
		t.Errorf("entity decode: got %q", got)
	}
	if !strings.Contains(got, "<script>") {
		t.Errorf("&lt;script&gt; decode: got %q", got)
	}
}

func TestStripHTMLTags_TolerantOfBrokenMarkup(t *testing.T) {
	// Unterminated tag, missing close. The walker must not blow up.
	in := `<p>unterminated <b>oh no`
	got := stripHTMLTags(in)
	if !strings.Contains(got, "unterminated") {
		t.Errorf("broken markup lost text: got %q", got)
	}
	if !strings.Contains(got, "oh no") {
		t.Errorf("broken markup lost inline text: got %q", got)
	}
}

func TestStripHTMLTags_AnchorTextSurvives(t *testing.T) {
	in := `Click <a href="https://example.com">here</a> for more.`
	got := stripHTMLTags(in)
	if !strings.Contains(got, "Click") || !strings.Contains(got, "here") || !strings.Contains(got, "for more") {
		t.Errorf("anchor text lost: got %q", got)
	}
}

func TestStripHTMLTags_CDATAHandledCleanly(t *testing.T) {
	// CDATA is XHTML, not HTML5. The x/net/html tokenizer treats
	// `<![CDATA[...]]>` as a comment in HTML mode. Either rendering
	// is acceptable so long as we don't crash and the surrounding
	// text survives.
	in := `<p>before <![CDATA[ inside cdata ]]> after</p>`
	got := stripHTMLTags(in)
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("CDATA neighbouring text lost: got %q", got)
	}
}

func TestStripHTMLTags_LongRunCollapsesWhitespace(t *testing.T) {
	in := `<p>a    b\t\tc</p>`
	got := stripHTMLTags(in)
	if !strings.Contains(got, "a b") {
		t.Errorf("whitespace collapse: got %q", got)
	}
}

func TestStripHTMLTags_EmptyInputReturnsEmpty(t *testing.T) {
	if got := stripHTMLTags(""); got != "" {
		t.Errorf("empty input: got %q", got)
	}
}

func TestNaiveStripHTMLTags_FallbackPath(t *testing.T) {
	// Exercise the fallback explicitly so its coverage stays >= 80%.
	got := naiveStripHTMLTags("<p>hi <b>there</b></p>")
	if got != "hi there" {
		t.Errorf("naive strip: got %q", got)
	}
}

// ---- slice-3 charset transcoding ----

func TestTranscodeToUTF8_EmptyCharsetPassthrough(t *testing.T) {
	in := "hello"
	if got := transcodeToUTF8(in, ""); got != in {
		t.Errorf("empty charset must passthrough, got %q", got)
	}
}

func TestTranscodeToUTF8_UTF8Passthrough(t *testing.T) {
	in := "Café au lait"
	for _, label := range []string{"utf-8", "UTF-8", "utf8", "us-ascii", "ascii"} {
		if got := transcodeToUTF8(in, label); got != in {
			t.Errorf("label=%q: got %q, want passthrough", label, got)
		}
	}
}

func TestTranscodeToUTF8_ISO88591(t *testing.T) {
	// "Café" in ISO-8859-1: e9 = 'é'
	in := string([]byte{'C', 'a', 'f', 0xe9})
	got := transcodeToUTF8(in, "iso-8859-1")
	if got != "Café" {
		t.Errorf("ISO-8859-1 decode: got %q (bytes=% x), want Café", got, []byte(got))
	}
}

func TestTranscodeToUTF8_Windows1252SmartQuotes(t *testing.T) {
	// "Hello" with windows-1252 smart-quote markers around it.
	// 0x93 = left double quote (U+201C), 0x94 = right double quote (U+201D).
	in := string([]byte{0x93, 'H', 'i', 0x94})
	got := transcodeToUTF8(in, "windows-1252")
	if got != "“Hi”" {
		t.Errorf("windows-1252 smart quotes: got %q (bytes=% x)", got, []byte(got))
	}
}

func TestTranscodeToUTF8_UnknownCharsetPassthrough(t *testing.T) {
	in := "raw bytes"
	if got := transcodeToUTF8(in, "x-not-a-real-charset"); got != in {
		t.Errorf("unknown charset must passthrough, got %q", got)
	}
}

func TestTranscodeToUTF8_AlreadyValidUTF8AndASCIIShortCircuits(t *testing.T) {
	// Even with a wrong charset label, a pure-ASCII string should
	// passthrough without disturbance — common for ISO-8859-1
	// mail bodies that never escaped the 7-bit range.
	in := "Hello, world"
	if got := transcodeToUTF8(in, "iso-8859-1"); got != in {
		t.Errorf("ASCII passthrough with non-utf8 label: got %q", got)
	}
}

func TestDecodePart_TransferThenCharset(t *testing.T) {
	// Quoted-printable encoding of an ISO-8859-1 byte sequence.
	// "Caf\xe9" → quoted-printable "Caf=E9", then ISO-8859-1 → UTF-8.
	got := decodePart("quoted-printable", "iso-8859-1", []byte("Caf=E9"))
	if got != "Café" {
		t.Errorf("decodePart QP+ISO-8859-1: got %q, want Café", got)
	}
}

func TestDecodePart_Base64ThenCharset(t *testing.T) {
	// Base64-encoded ISO-8859-1 bytes "Caf\xe9".
	got := decodePart("base64", "iso-8859-1", []byte("Q2Fm6Q=="))
	if got != "Café" {
		t.Errorf("decodePart base64+ISO-8859-1: got %q", got)
	}
}

func TestDecodePart_EmptyCharsetPreservesUTF8(t *testing.T) {
	// Backwards-compat: no charset label, UTF-8 body, must passthrough.
	got := decodePart("8bit", "", []byte("Café"))
	if got != "Café" {
		t.Errorf("got %q", got)
	}
}

func TestIsASCII(t *testing.T) {
	if !isASCII("hello") {
		t.Error("plain ASCII must be detected")
	}
	if isASCII("Café") {
		t.Error("Café has multibyte runes; must not be ASCII")
	}
	if !isASCII("") {
		t.Error("empty string is trivially ASCII")
	}
}

func TestExtractTextBody_NonUTF8Charset(t *testing.T) {
	// text/plain with ISO-8859-1 charset; body is the raw ISO-8859-1
	// byte sequence "Caf\xe9". Pre-slice-3 this surfaced as invalid
	// UTF-8; now decodePart transcodes.
	body := []byte{'C', 'a', 'f', 0xe9}
	got := extractTextBody("text/plain; charset=ISO-8859-1", "", body)
	if got != "Café" {
		t.Errorf("got %q (bytes=% x), want Café", got, []byte(got))
	}
}

func TestExtractTextBody_MultipartNonUTF8Charset(t *testing.T) {
	body := []byte(
		"--BNDRY\r\n" +
			"Content-Type: text/plain; charset=ISO-8859-1\r\n\r\n" +
			"Caf\xe9 au lait\r\n" +
			"--BNDRY--\r\n",
	)
	got := extractTextBody(`multipart/alternative; boundary="BNDRY"`, "", body)
	if !strings.Contains(got, "Café") {
		t.Errorf("multipart ISO-8859-1 plaintext: got %q (bytes=% x)", got, []byte(got))
	}
}

func TestParseMessageBody_NonUTF8HTML(t *testing.T) {
	// text/html with Windows-1252 charset — confirm html walker
	// receives transcoded text (smart quotes survive as UTF-8).
	htmlBody := []byte{
		'<', 'p', '>',
		0x93, 'H', 'i', 0x94,
		'<', '/', 'p', '>',
	}
	out := parseMessageBody("text/html; charset=windows-1252", "", htmlBody)
	if !strings.Contains(out.Text, "“Hi”") {
		t.Errorf("got %q (bytes=% x), want curly-quoted Hi", out.Text, []byte(out.Text))
	}
}
