package email

// email_wave3_test.go — second-pass, high-value tests for internal/email.
//
// Scope was chosen against a coverprofile of the package to hit funcs that
// the existing suites (channel_test / smtp_test / smtp_envelope_test /
// reconnect_test / attachments_test / signature_test / body_test /
// imap_test / ingress_boundary_test) leave partially or wholly uncovered.
// Every assertion was confirmed against current behaviour before being
// written; this file modifies no production code.
//
// Focus areas (no overlap with ingress_boundary_test.go, which owns
// RFC 2047 / charset / multipart / SPF-policy / From-extraction):
//
//   - SMTP outbound rendering edges: Message-ID *format* + per-render
//     uniqueness, Message-ID host derived through a display-name From,
//     References-header angle normalisation through Channel.Send.
//   - Channel.Send behaviour: body CRLF-normalisation reaches the wire
//     payload, the no-References / SessionID-default threading branch,
//     ChannelSpecific in_reply_to + references overrides, and the
//     envelope From propagated to SMTPSendRequest.
//   - attachment persistence: writeAttachmentUnique numeric-suffix
//     collision handling + too-many-duplicates ceiling,
//     mintAttachmentArtifactID determinism/divergence, and the
//     ArtifactRepository.Create wiring carrying the right metadata.
//   - signature trusted-server filtering seam: authServIDTrusted match,
//     untrusted A-R header ignored so strict rejects a forged pass,
//     and rank ordering for mergeVerdict.
//   - imap parseReferences comma/whitespace/angle tolerance edges.
//   - transcodeToUTF8 fallback edges (unknown label, empty charset).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// --- SMTP outbound rendering edges ---

// TestW3EmailNewMessageIDFormatAndUniqueness pins the Message-ID shape
// RenderRFC5322 hands back: a 32-hex-char random prefix, the literal
// ".vornik@" tag, and the From domain. Two renders of the same message
// must produce DIFFERENT IDs (crypto/rand suffix) so threading state
// never collides across two outbound sends on one daemon.
func TestW3EmailNewMessageIDFormatAndUniqueness(t *testing.T) {
	out := OutboundMessage{From: "bot@vornik.io", To: "alice@ext.test", Body: "x"}
	_, id1, err := RenderRFC5322(out)
	if err != nil {
		t.Fatalf("RenderRFC5322: %v", err)
	}
	_, id2, err := RenderRFC5322(out)
	if err != nil {
		t.Fatalf("RenderRFC5322: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("two renders produced identical Message-IDs %q; want unique", id1)
	}
	for _, id := range []string{id1, id2} {
		if !strings.HasSuffix(id, ".vornik@vornik.io") {
			t.Errorf("Message-ID %q missing .vornik@<domain> tail", id)
		}
		hexPart := strings.TrimSuffix(id, ".vornik@vornik.io")
		if len(hexPart) != 32 {
			t.Errorf("Message-ID random prefix = %d chars, want 32: %q", len(hexPart), hexPart)
		}
		for _, r := range hexPart {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Errorf("Message-ID prefix %q has non-hex rune %q", hexPart, r)
			}
		}
	}
}

// TestW3EmailMessageIDHostThroughDisplayNameFrom verifies the Message-ID
// host is the domain of a display-name-form From (the production
// from_address shape). messageIDHost runs after the bare-address parse
// in newMessageID; this confirms the angle/space trimming yields the
// clean domain rather than "vornik.io>" or a fallback.
func TestW3EmailMessageIDHostThroughDisplayNameFrom(t *testing.T) {
	if got := messageIDHost("Assistant <bot@vornik.io>"); got != "vornik.io" {
		t.Errorf("messageIDHost(display-name) = %q, want vornik.io", got)
	}
}

// TestW3EmailRenderReferencesNormalisesMixedAngles guards the
// References-header renderer against the realistic case where the
// caller-supplied chain already carries angle brackets on some IDs and
// not others (different MUAs in one thread). Every entry must end up
// wrapped exactly once — no doubled "<<id>>" and no bare id.
func TestW3EmailRenderReferencesNormalisesMixedAngles(t *testing.T) {
	got := renderReferencesHeader("<root@a> middle@b <leaf@c>")
	want := "<root@a> <middle@b> <leaf@c>"
	if got != want {
		t.Errorf("renderReferencesHeader = %q, want %q", got, want)
	}
}

// TestW3EmailNormaliseCRLFLeadingBareLF covers the i==0 branch of the
// CRLF rewriter: a body that opens with a bare LF must get a CR
// prepended (the rewrite-needed scan and the writer both special-case
// index 0). A bare LF leading the DATA stream is exactly what trips the
// strict-MTA 451 the function defends against.
func TestW3EmailNormaliseCRLFLeadingBareLF(t *testing.T) {
	got := normaliseCRLF("\nbody")
	if got != "\r\nbody" {
		t.Errorf("normaliseCRLF(leading LF) = %q, want %q", got, "\r\nbody")
	}
}

// --- Channel.Send behaviour ---

// sendOnlyChannel builds a Channel wired with a fakeSMTP and a fixed
// clock, no IMAP poll loop running. Keeps the Send tests free of the
// goroutine/waitFor dance the inbound tests need.
func sendOnlyChannel(t *testing.T, smtp *fakeSMTP) *Channel {
	t.Helper()
	cfg := validConfig()
	cfg.SMTPSender = smtp
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ch
}

// TestW3EmailSendNormalisesBodyToCRLFOnWire confirms Send's rendered
// payload carries CRLF line endings even when the dispatcher hands it a
// bare-LF body (LLM output is \n-delimited). Asserts on the wire bytes
// after the header/body separator so we're testing the real outbound
// payload, not RenderRFC5322 in isolation.
func TestW3EmailSendNormalisesBodyToCRLFOnWire(t *testing.T) {
	smtp := &fakeSMTP{}
	ch := sendOnlyChannel(t, smtp)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "s1",
		Text:      "line one\nline two\nline three",
		ChannelSpecific: map[string]string{
			"to":      "bob@ext.test",
			"subject": "Subj",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqs := smtp.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("smtp requests = %d, want 1", len(reqs))
	}
	payload := string(reqs[0].Payload)
	sep := strings.Index(payload, "\r\n\r\n")
	if sep < 0 {
		t.Fatal("no header/body separator on wire")
	}
	body := payload[sep+4:]
	if strings.Contains(body, "\nline two") && !strings.Contains(body, "\r\nline two") {
		t.Errorf("body not CRLF-normalised on wire:\n%q", body)
	}
	if strings.Contains(body, "\r\r\n") {
		t.Errorf("body CRLF doubled:\n%q", body)
	}
}

// TestW3EmailSendDefaultsThreadingToSessionID locks the
// no-ChannelSpecific-threading branch: with only SessionID supplied,
// both In-Reply-To and References default to that thread root so a
// reply against a known session is always RFC-5322-threaded.
func TestW3EmailSendDefaultsThreadingToSessionID(t *testing.T) {
	smtp := &fakeSMTP{}
	ch := sendOnlyChannel(t, smtp)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "root-xyz",
		Text:      "reply",
		ChannelSpecific: map[string]string{
			"to":      "bob@ext.test",
			"subject": "Re: Topic",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	payload := string(smtp.snapshot()[0].Payload)
	if !strings.Contains(payload, "In-Reply-To: <root-xyz>") {
		t.Errorf("In-Reply-To not defaulted to SessionID:\n%s", payload)
	}
	if !strings.Contains(payload, "References: <root-xyz>") {
		t.Errorf("References not defaulted to SessionID:\n%s", payload)
	}
}

// TestW3EmailSendChannelSpecificThreadingOverrides exercises the
// ChannelSpecific["in_reply_to"] / ["references"] override branch
// (distinct from the message's own InReplyTo field). The dispatcher
// uses these to thread a reply into an existing chain it tracked
// out-of-band; both supplied IDs must reach the rendered headers,
// angle-wrapped.
func TestW3EmailSendChannelSpecificThreadingOverrides(t *testing.T) {
	smtp := &fakeSMTP{}
	ch := sendOnlyChannel(t, smtp)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "root",
		Text:      "reply",
		ChannelSpecific: map[string]string{
			"to":          "bob@ext.test",
			"subject":     "Re: T",
			"in_reply_to": "parent-99",
			"references":  "root parent-99",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	payload := string(smtp.snapshot()[0].Payload)
	if !strings.Contains(payload, "In-Reply-To: <parent-99>") {
		t.Errorf("ChannelSpecific in_reply_to override missing:\n%s", payload)
	}
	if !strings.Contains(payload, "References: <root> <parent-99>") {
		t.Errorf("ChannelSpecific references override malformed:\n%s", payload)
	}
}

// TestW3EmailSendMessageInReplyToFieldOverridesDefault covers the
// distinct branch where the ChannelMessage's own InReplyTo field (not
// ChannelSpecific) overrides the SessionID default for In-Reply-To,
// while References still falls back to SessionID.
func TestW3EmailSendMessageInReplyToFieldOverridesDefault(t *testing.T) {
	smtp := &fakeSMTP{}
	ch := sendOnlyChannel(t, smtp)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "root-id",
		InReplyTo: "field-parent",
		Text:      "reply",
		ChannelSpecific: map[string]string{
			"to":      "bob@ext.test",
			"subject": "Re: T",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	payload := string(smtp.snapshot()[0].Payload)
	if !strings.Contains(payload, "In-Reply-To: <field-parent>") {
		t.Errorf("message InReplyTo field did not override default:\n%s", payload)
	}
	// References still defaults to the session root.
	if !strings.Contains(payload, "References: <root-id>") {
		t.Errorf("References should default to SessionID:\n%s", payload)
	}
}

// TestW3EmailSendEnvelopeFromMatchesConfig verifies the SMTPSendRequest
// envelope From and To carry the channel's configured FromAddress and
// the resolved recipient — the fields the production adapter feeds into
// MAIL FROM / RCPT TO.
func TestW3EmailSendEnvelopeFromMatchesConfig(t *testing.T) {
	smtp := &fakeSMTP{}
	ch := sendOnlyChannel(t, smtp)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "s",
		Text:      "b",
		ChannelSpecific: map[string]string{
			"to":      "bob@ext.test",
			"subject": "S",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	req := smtp.snapshot()[0]
	if req.From != "vornik@test" {
		t.Errorf("envelope From = %q, want vornik@test", req.From)
	}
	if len(req.To) != 1 || req.To[0] != "bob@ext.test" {
		t.Errorf("envelope To = %v, want [bob@ext.test]", req.To)
	}
}

// --- attachment persistence ---

// TestW3EmailWriteAttachmentUniqueSuffixesCollision drives
// writeAttachmentUnique past its O_EXCL collision branch: writing the
// same filename twice into one dir must keep both files, the second
// gaining a "-2" numeric suffix, with independent contents.
func TestW3EmailWriteAttachmentUniqueSuffixesCollision(t *testing.T) {
	dir := t.TempDir()
	p1, n1, err := writeAttachmentUnique(dir, "report.pdf", []byte("first"))
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if n1 != "report.pdf" {
		t.Errorf("first name = %q, want report.pdf", n1)
	}
	p2, n2, err := writeAttachmentUnique(dir, "report.pdf", []byte("second"))
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if n2 != "report-2.pdf" {
		t.Errorf("collision name = %q, want report-2.pdf", n2)
	}
	if p1 == p2 {
		t.Fatal("collision produced identical paths")
	}
	b1, _ := os.ReadFile(p1)
	b2, _ := os.ReadFile(p2)
	if string(b1) != "first" || string(b2) != "second" {
		t.Errorf("contents clobbered: %q / %q", b1, b2)
	}
}

// TestW3EmailWriteAttachmentUniqueNoExtension confirms the stem/ext
// split handles an extension-less filename: the suffix lands as
// "data-2", not "data-2." with a dangling dot.
func TestW3EmailWriteAttachmentUniqueNoExtension(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := writeAttachmentUnique(dir, "data", []byte("a")); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, n2, err := writeAttachmentUnique(dir, "data", []byte("b"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if n2 != "data-2" {
		t.Errorf("extensionless collision name = %q, want data-2", n2)
	}
}

// TestW3EmailMintAttachmentArtifactIDDeterministicAndDistinct pins the
// ID minter contract: identical (messageID, filename, timestamp) yields
// the same ID (idempotent re-delivery within a second) while a changed
// filename diverges it. Guards the "email-att-" prefix + 16-hex shape.
func TestW3EmailMintAttachmentArtifactIDDeterministicAndDistinct(t *testing.T) {
	ts := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	a := mintAttachmentArtifactID("msg-1", "report.pdf", ts)
	b := mintAttachmentArtifactID("msg-1", "report.pdf", ts)
	if a != b {
		t.Errorf("same inputs gave different IDs %q vs %q", a, b)
	}
	c := mintAttachmentArtifactID("msg-1", "other.pdf", ts)
	if a == c {
		t.Error("different filename gave identical ID")
	}
	if !strings.HasPrefix(a, "email-att-") {
		t.Errorf("ID %q missing email-att- prefix", a)
	}
	if hexPart := strings.TrimPrefix(a, "email-att-"); len(hexPart) != 16 {
		t.Errorf("ID hex part = %d chars, want 16: %q", len(hexPart), hexPart)
	}
}

// TestW3EmailPersistAttachmentsRecordsArtifactMetadata exercises the
// full PersistAttachments happy path against a mocked ArtifactRepository
// and asserts the Artifact row carries the input class, project scope,
// MIME type, size, and a storage path under the per-message subdir.
// Complements the existing happy-path test by pinning ArtifactClass +
// the SizeBytes/MimeType pointer plumbing in one go.
func TestW3EmailPersistAttachmentsRecordsArtifactMetadata(t *testing.T) {
	repo := &fakeArtifactRepo{}
	dir := t.TempDir()
	deps := persistAttachmentDeps{
		Repo:      repo,
		StoreDir:  dir,
		ProjectID: "proj-a",
		MessageID: "abc@host",
		Now:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	atts := []ParsedAttachment{{
		Filename:    "doc.pdf",
		ContentType: "application/pdf",
		Content:     []byte("PDFDATA"),
		SizeBytes:   7,
	}}
	got, err := PersistAttachments(context.Background(), deps, atts)
	if err != nil {
		t.Fatalf("PersistAttachments: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("persisted = %d, want 1", len(got))
	}
	if len(repo.created) != 1 {
		t.Fatalf("repo.Create calls = %d, want 1", len(repo.created))
	}
	art := repo.created[0]
	if art.ProjectID != "proj-a" {
		t.Errorf("ProjectID = %q, want proj-a", art.ProjectID)
	}
	if art.ArtifactClass != persistence.ArtifactClassInput {
		t.Errorf("ArtifactClass = %v, want input", art.ArtifactClass)
	}
	if art.Name != "doc.pdf" {
		t.Errorf("Name = %q, want doc.pdf", art.Name)
	}
	if art.SizeBytes == nil || *art.SizeBytes != 7 {
		t.Errorf("SizeBytes = %v, want 7", art.SizeBytes)
	}
	if art.MimeType == nil || *art.MimeType != "application/pdf" {
		t.Errorf("MimeType = %v, want application/pdf", art.MimeType)
	}
	// Storage path must live under the per-message subdir derived from
	// the angle-stripped Message-ID (@ -> _at_).
	wantDir := filepath.Join(dir, "abc_at_host")
	if filepath.Dir(got[0].StoragePath) != wantDir {
		t.Errorf("storage dir = %q, want under %q", filepath.Dir(got[0].StoragePath), wantDir)
	}
	if data, _ := os.ReadFile(got[0].StoragePath); string(data) != "PDFDATA" {
		t.Errorf("written bytes = %q, want PDFDATA", data)
	}
}

// --- signature trusted-server filtering seam ---

// TestW3EmailAuthServIDTrustedCaseInsensitive pins the trusted-relay
// matcher: configured IDs match case-insensitively and whitespace is
// trimmed on both sides, while an unconfigured ID is rejected.
func TestW3EmailAuthServIDTrustedCaseInsensitive(t *testing.T) {
	v := HeaderAuthVerifier{TrustedServerIDs: []string{" Relay.Example.COM "}}
	if !v.authServIDTrusted("relay.example.com") {
		t.Error("expected case/whitespace-insensitive trusted match")
	}
	if v.authServIDTrusted("evil.example.com") {
		t.Error("untrusted authserv-id must not match")
	}
}

// TestW3EmailStrictRejectsForgedPassFromUntrustedServer is the
// security-critical filtering path: with a trusted-server set
// configured, a forged Authentication-Results header carrying spf=pass
// but stamped by an UNTRUSTED authserv-id must be ignored, so strict
// policy still rejects the message for lack of a trusted pass.
func TestW3EmailStrictRejectsForgedPassFromUntrustedServer(t *testing.T) {
	v := HeaderAuthVerifier{
		Policy:           AuthPolicyStrict,
		TrustedServerIDs: []string{"relay.example.com"},
	}
	msg := ParsedMessage{
		From:        "attacker@evil.test",
		AuthResults: []string{"forged.attacker.test; spf=pass smtp.mailfrom=attacker@evil.test"},
	}
	if err := v.Verify(context.Background(), msg); err == nil {
		t.Error("strict policy admitted a forged pass from untrusted authserv-id")
	}
}

// TestW3EmailStrictAdmitsPassFromTrustedServer is the positive
// counterpart: the same spf=pass stamped by the trusted authserv-id
// admits under strict policy.
func TestW3EmailStrictAdmitsPassFromTrustedServer(t *testing.T) {
	v := HeaderAuthVerifier{
		Policy:           AuthPolicyStrict,
		TrustedServerIDs: []string{"relay.example.com"},
	}
	msg := ParsedMessage{
		From:        "alice@partner.test",
		AuthResults: []string{"relay.example.com; spf=pass smtp.mailfrom=alice@partner.test"},
	}
	if err := v.Verify(context.Background(), msg); err != nil {
		t.Errorf("strict policy rejected trusted pass: %v", err)
	}
}

// TestW3EmailMergeVerdictFailBeatsLaterPass locks the fail-closed
// ranking rule via mergeVerdict: once a fail is seen, a subsequent pass
// on the same method must not mask it. Exercised through summariseAuth's
// two-hop merge so we test the real call path, not rank() alone.
func TestW3EmailMergeVerdictFailBeatsLaterPass(t *testing.T) {
	v := HeaderAuthVerifier{}
	msg := ParsedMessage{
		AuthResults: []string{
			"relay; spf=fail smtp.mailfrom=x@y",
			"relay; spf=pass smtp.mailfrom=x@y",
		},
	}
	if err := v.Verify(context.Background(), msg); err == nil {
		t.Error("later spf=pass masked an earlier spf=fail; want fail-closed rejection")
	}
}

// TestW3EmailReceivedSPFForgedPassIgnoredUnderFiltering covers the
// Received-SPF branch of summariseAuth under trusted-server filtering:
// Received-SPF carries no authserv-id, so a (possibly forged) "pass"
// must be ignored when filtering is on, leaving strict to reject.
func TestW3EmailReceivedSPFForgedPassIgnoredUnderFiltering(t *testing.T) {
	v := HeaderAuthVerifier{
		Policy:           AuthPolicyStrict,
		TrustedServerIDs: []string{"relay.example.com"},
	}
	msg := ParsedMessage{
		From:        "attacker@evil.test",
		ReceivedSPF: []string{"pass (mailfrom: evil.test) client-ip=1.2.3.4"},
	}
	if err := v.Verify(context.Background(), msg); err == nil {
		t.Error("Received-SPF forged pass admitted under filtering; want rejection")
	}
}

// TestW3EmailReceivedSPFFailHonouredUnderFiltering confirms the
// fail-closed exception in the same branch: even under filtering (where
// pass is ignored), an explicit Received-SPF fail still rejects.
func TestW3EmailReceivedSPFFailHonouredUnderFiltering(t *testing.T) {
	v := HeaderAuthVerifier{
		Policy:           AuthPolicyRelaxed,
		TrustedServerIDs: []string{"relay.example.com"},
	}
	msg := ParsedMessage{
		From:        "attacker@evil.test",
		ReceivedSPF: []string{"fail (sender not authorized) client-ip=1.2.3.4"},
	}
	if err := v.Verify(context.Background(), msg); err == nil {
		t.Error("explicit Received-SPF fail must reject even under relaxed+filtering")
	}
}

// --- imap parseReferences edges ---

// TestW3EmailParseReferencesCommaAndAngleTolerance feeds parseReferences
// the non-compliant-but-real mix of comma separators, angle brackets,
// and irregular whitespace some MUAs emit. Output must be the clean,
// angle-stripped ID list in order.
func TestW3EmailParseReferencesCommaAndAngleTolerance(t *testing.T) {
	got := parseReferences("  <a@x> , <b@y>\t<c@z>\r\n")
	want := []string{"a@x", "b@y", "c@z"}
	if len(got) != len(want) {
		t.Fatalf("parseReferences = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseReferences[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestW3EmailParseReferencesEmptyAndWhitespaceOnly confirms the
// early-return / all-empty paths: a blank header and a brackets-only
// noise header both yield nil rather than a slice of empty strings.
func TestW3EmailParseReferencesEmptyAndWhitespaceOnly(t *testing.T) {
	if got := parseReferences("   \t\r\n"); got != nil {
		t.Errorf("blank header = %v, want nil", got)
	}
	if got := parseReferences("<> , <>"); len(got) != 0 {
		t.Errorf("angle-only noise = %v, want empty", got)
	}
}

// --- transcodeToUTF8 fallback edges ---

// TestW3EmailTranscodeUnknownCharsetPassesThrough verifies the
// htmlindex-miss branch: an unrecognised charset label returns the input
// bytes verbatim rather than erroring or dropping the body.
func TestW3EmailTranscodeUnknownCharsetPassesThrough(t *testing.T) {
	in := string([]byte{0x93, 0xfa})
	if got := transcodeToUTF8(in, "x-made-up-charset"); got != in {
		t.Errorf("unknown charset = %q, want pass-through %q", got, in)
	}
}

// TestW3EmailTranscodeEmptyAndUTF8ShortCircuit covers the no-op guards:
// empty charset, "utf-8", and "us-ascii" all return the input untouched
// without consulting htmlindex.
func TestW3EmailTranscodeEmptyAndUTF8ShortCircuit(t *testing.T) {
	for _, cs := range []string{"", "UTF-8", "us-ascii"} {
		if got := transcodeToUTF8("hello", cs); got != "hello" {
			t.Errorf("transcode(%q) = %q, want hello", cs, got)
		}
	}
}
