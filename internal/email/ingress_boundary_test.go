package email

// ingress_boundary_test.go — high-value tests for the email ingress
// channel as an untrusted-input boundary. Focus areas not already
// pinned by body_test.go / signature_test.go / imap_test.go /
// attachments_test.go:
//
//   - RFC 2047 encoded-word decoding: adjacent-word concatenation
//     semantics, mixed plain+encoded, charset+transfer variants.
//   - charset transcoding via htmlindex for non-Western scripts
//     (Shift_JIS / GB2312 / Windows-1251) that the existing suite
//     doesn't exercise (it only covers ISO-8859-1 / Windows-1252).
//   - multipart walk: text/plain preference when ordered after
//     text/html at the same level, and through a nested alternative.
//   - HeaderAuthVerifier policy edges (softfail strict-vs-relaxed,
//     multi-method header parse with junk segments).
//   - From-address extraction through display names + encoded words,
//     case-folding, and the unparseable-fallback path.
//
// Every assertion was confirmed against current behavior before being
// written; no production code is modified by this file.

import (
	"context"
	"testing"
)

// --- RFC 2047 encoded-word decoding (untrusted Subject header) ---

// TestDecodeHeader_AdjacentEncodedWordsConcatenateNoSpace locks in the
// stdlib WordDecoder semantics the channel relies on: two adjacent
// encoded-words separated only by linear whitespace are joined with NO
// separating space (RFC 2047 §6.2). A regression here would silently
// merge or split words in routed subjects.
func TestDecodeHeader_AdjacentEncodedWordsConcatenateNoSpace(t *testing.T) {
	got := decodeHeader("=?UTF-8?B?SGVsbG8=?= =?UTF-8?B?V29ybGQ=?=")
	if got != "HelloWorld" {
		t.Errorf("decodeHeader concatenation = %q, want %q", got, "HelloWorld")
	}
}

// TestDecodeHeader_MixedPlainAndEncodedWord covers the common real-world
// "Re: <encoded>" subject shape where a plaintext prefix precedes an
// encoded-word. The plaintext run (including its trailing space) must
// survive verbatim while the encoded-word decodes.
func TestDecodeHeader_MixedPlainAndEncodedWord(t *testing.T) {
	got := decodeHeader("Re: =?UTF-8?B?SGVsbG8=?=")
	if got != "Re: Hello" {
		t.Errorf("decodeHeader mixed = %q, want %q", got, "Re: Hello")
	}
}

// TestDecodeHeader_QEncodingISO88591 exercises the Q-encoding + non-UTF-8
// charset path (=E9 => é under ISO-8859-1). Existing tests only cover
// B-encoded UTF-8; this pins the quoted-printable-style word decode that
// most Western MUAs emit for accented subjects.
func TestDecodeHeader_QEncodingISO88591(t *testing.T) {
	got := decodeHeader("=?ISO-8859-1?Q?Caf=E9?=")
	if got != "Café" {
		t.Errorf("decodeHeader Q/ISO-8859-1 = %q, want %q", got, "Café")
	}
}

// TestDecodeHeader_UnsupportedEncodingFallsBackToRaw confirms the
// defensive contract: a syntactically-shaped but invalid encoded-word
// (bad encoding letter "X") must NOT error or drop the value — it falls
// back to the raw header text so a malformed subject still routes.
func TestDecodeHeader_UnsupportedEncodingFallsBackToRaw(t *testing.T) {
	raw := "=?bogus?X?zz?="
	got := decodeHeader(raw)
	if got != raw {
		t.Errorf("decodeHeader malformed = %q, want raw %q", got, raw)
	}
}

// --- charset transcoding (non-Western scripts) ---

// TestTranscodeToUTF8_ShiftJIS verifies CJK transcoding through
// htmlindex. The two Shift_JIS double-byte sequences below decode to
// 日本 (Japan). The existing suite only covers single-byte Western
// charsets, so this is the first multi-byte-charset assertion.
func TestTranscodeToUTF8_ShiftJIS(t *testing.T) {
	// 0x93 0xfa 0x96 0x7b == "日本" in Shift_JIS.
	in := string([]byte{0x93, 0xfa, 0x96, 0x7b})
	got := transcodeToUTF8(in, "shift_jis")
	if got != "日本" {
		t.Errorf("transcode Shift_JIS = %q, want %q", got, "日本")
	}
}

// TestTranscodeToUTF8_GB2312 verifies Simplified-Chinese transcoding
// (the bytes below are 你好 in GB2312). GB2312 is a WHATWG-aliased label
// htmlindex resolves to GBK; the channel must not pass these bytes
// through raw to the LLM.
func TestTranscodeToUTF8_GB2312(t *testing.T) {
	// 0xc4 0xe3 0xba 0xc3 == "你好" in GB2312/GBK.
	in := string([]byte{0xc4, 0xe3, 0xba, 0xc3})
	got := transcodeToUTF8(in, "gb2312")
	if got != "你好" {
		t.Errorf("transcode GB2312 = %q, want %q", got, "你好")
	}
}

// TestTranscodeToUTF8_Windows1251Cyrillic pins the Windows-125x family
// beyond -1252 (the only one currently tested). The byte run below is
// "Привет" in Windows-1251 (Cyrillic).
func TestTranscodeToUTF8_Windows1251Cyrillic(t *testing.T) {
	// "Привет" in Windows-1251.
	in := string([]byte{0xcf, 0xf0, 0xe8, 0xe2, 0xe5, 0xf2})
	got := transcodeToUTF8(in, "windows-1251")
	if got != "Привет" {
		t.Errorf("transcode Windows-1251 = %q, want %q", got, "Привет")
	}
}

// --- multipart walk: text/plain preference ordering ---

// TestExtractTextFromMultipart_PlainPreferredEvenWhenHTMLFirst guards the
// alternative-resolution rule: when a single multipart level holds both
// text/html (first) and text/plain (second), the plaintext part wins.
// The walker stashes the HTML as a fallback but returns plain the moment
// it sees it — order-independence is the contract.
func TestExtractTextFromMultipart_PlainPreferredEvenWhenHTMLFirst(t *testing.T) {
	body := "--B\r\n" +
		"Content-Type: text/html\r\n\r\n<b>H</b>\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain\r\n\r\nP\r\n" +
		"--B--\r\n"
	got := extractTextFromMultipart("B", []byte(body))
	if got != "P" {
		t.Errorf("multipart text preference = %q, want %q", got, "P")
	}
}

// TestExtractTextFromMultipart_NestedAlternativeTranscodedQP combines
// three boundary behaviors in one realistic envelope: a nested
// multipart/alternative inside multipart/mixed, quoted-printable
// transfer encoding, and an ISO-8859-1 charset param — all of which must
// compose so "Caf=E9" surfaces as "Café".
func TestExtractTextFromMultipart_NestedAlternativeTranscodedQP(t *testing.T) {
	body := "--M\r\n" +
		"Content-Type: multipart/alternative; boundary=A\r\n\r\n" +
		"--A\r\n" +
		"Content-Type: text/html\r\n\r\n<p>ignored</p>\r\n" +
		"--A\r\n" +
		"Content-Type: text/plain; charset=ISO-8859-1\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\nCaf=E9\r\n" +
		"--A--\r\n" +
		"--M--\r\n"
	got := extractTextFromMultipart("M", []byte(body))
	if got != "Café" {
		t.Errorf("nested transcoded plain = %q, want %q", got, "Café")
	}
}

// TestParseMessageBody_MultipartMissingBoundaryTreatsBodyAsOpaque pins
// the malformed-envelope path: a Content-Type of multipart/* with NO
// boundary param can't be walked, so the raw transfer-decoded payload is
// surfaced as opaque text rather than dropping the message. No
// attachments are extracted.
func TestParseMessageBody_MultipartMissingBoundaryTreatsBodyAsOpaque(t *testing.T) {
	mb := parseMessageBody("multipart/mixed", "", []byte("raw opaque payload"))
	if mb.Text != "raw opaque payload" {
		t.Errorf("opaque body = %q, want %q", mb.Text, "raw opaque payload")
	}
	if len(mb.Attachments) != 0 {
		t.Errorf("attachments = %d, want 0", len(mb.Attachments))
	}
}

// --- HeaderAuthVerifier policy edges ---

// TestHeaderAuth_SoftFailStrictRejectsRelaxedAdmits exercises the
// softfail verdict (SPF ~all) under both policies in one test: strict
// must reject (softfail is not an explicit pass) while relaxed admits
// (softfail is not an explicit fail). This is the most security-relevant
// gray-zone verdict and isn't covered by the existing neutral cases.
func TestHeaderAuth_SoftFailStrictRejectsRelaxedAdmits(t *testing.T) {
	msg := ParsedMessage{
		From:        "sender@example.com",
		ReceivedSPF: []string{"softfail (domain owner discourages) client-ip=1.2.3.4"},
	}

	strict := HeaderAuthVerifier{Policy: AuthPolicyStrict}
	if err := strict.Verify(context.Background(), msg); err == nil {
		t.Error("strict policy admitted SPF softfail; want rejection")
	}

	relaxed := HeaderAuthVerifier{Policy: AuthPolicyRelaxed}
	if err := relaxed.Verify(context.Background(), msg); err != nil {
		t.Errorf("relaxed policy rejected SPF softfail: %v; want admit", err)
	}
}

// TestParseAuthResultsHeader_SkipsJunkSegmentsKeepsValid feeds a header
// with a malformed segment ("=bad" — equals at index 0, no method name)
// alongside a valid spf=pass. The parser must drop the junk segment
// (eq<=0 guard) and still surface the valid method, and must read the
// leading authserv-id correctly. Guards the tokeniser against
// attacker-injected garbage segments.
func TestParseAuthResultsHeader_SkipsJunkSegmentsKeepsValid(t *testing.T) {
	id, methods := parseAuthResultsHeader("relay.example.com 1; =bad; spf=pass smtp.mailfrom=a@b.com")
	if id != "relay.example.com" {
		t.Errorf("authserv-id = %q, want %q", id, "relay.example.com")
	}
	if len(methods) != 1 {
		t.Fatalf("methods = %+v, want exactly 1 (spf=pass)", methods)
	}
	if methods[0].Method != "spf" || methods[0].Result != authPass {
		t.Errorf("method = %+v, want {spf pass}", methods[0])
	}
}

// --- From-address extraction (untrusted From header) ---

// TestParseRFC5322_FromDisplayNameStrippedAndLowercased verifies the
// allowlist-facing From normalisation: the display name is dropped, the
// bare address is extracted, and the whole thing is lower-cased so the
// case-insensitive allowlist compares cleanly. A "+tag" must be
// preserved (it's part of the address, not noise).
func TestParseRFC5322_FromDisplayNameStrippedAndLowercased(t *testing.T) {
	raw := "From: \"Alice Smith\" <Alice.Smith+News@Example.COM>\r\n" +
		"Subject: hi\r\n\r\nbody\r\n"
	p, err := ParseRFC5322([]byte(raw))
	if err != nil {
		t.Fatalf("ParseRFC5322: %v", err)
	}
	if p.From != "alice.smith+news@example.com" {
		t.Errorf("From = %q, want %q", p.From, "alice.smith+news@example.com")
	}
}

// TestParseRFC5322_FromUnparseableFallsBackLowercased confirms the
// tolerant fallback: a From header that net/mail can't parse as an
// address must NOT fail the whole message — the raw value is lower-cased
// and stored so the allowlist check can reject it cleanly downstream.
func TestParseRFC5322_FromUnparseableFallsBackLowercased(t *testing.T) {
	raw := "From: Not-An-Address-AT-ALL\r\nSubject: x\r\n\r\nbody\r\n"
	p, err := ParseRFC5322([]byte(raw))
	if err != nil {
		t.Fatalf("ParseRFC5322 errored on unparseable From: %v; want tolerated", err)
	}
	if p.From != "not-an-address-at-all" {
		t.Errorf("From = %q, want lowercased raw %q", p.From, "not-an-address-at-all")
	}
}
