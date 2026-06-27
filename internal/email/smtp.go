package email

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// SMTPSender is the narrow outbound-transport seam the email
// channel depends on. Production wiring supplies a net/smtp-backed
// adapter (NewNetSMTPSender below); tests inject a fake that
// captures the rendered payload for assertion. Mirrors IMAPClient's
// shape so both seams look the same to readers.
type SMTPSender interface {
	// Send delivers a single pre-rendered RFC 5322 message. The
	// implementation is responsible for the SMTP envelope (MAIL
	// FROM / RCPT TO), the rendered Payload becomes the DATA
	// section verbatim. Returns an error on any transport
	// failure; Channel.Send wraps that with email-channel
	// context.
	Send(ctx context.Context, req SMTPSendRequest) error

	// Close is a hook for adapters that pool connections. Slice 1's
	// stdlib adapter dials per-Send, so it's a no-op there.
	Close() error
}

// SMTPSendRequest is the per-call parameter bundle. From / To
// drive the SMTP envelope; Payload is the rendered RFC 5322 bytes
// (headers + blank line + body) and goes directly into the DATA
// command.
type SMTPSendRequest struct {
	From    string
	To      []string
	Payload []byte
}

// OutboundMessage is the channel-side view of an outbound email
// before RFC 5322 rendering. CC / BCC / custom headers land in a
// later slice when an actual need arises.
type OutboundMessage struct {
	From       string
	To         string
	Subject    string
	Body       string
	InReplyTo  string
	References string
	Date       time.Time
	// Attachments, when non-empty, switches the rendering to
	// multipart/mixed (text body part + one base64 part per
	// attachment). Empty keeps the legacy text/plain shape unchanged.
	Attachments []OutboundAttachment
}

// OutboundAttachment is one file to attach to an outbound email. The
// channel reads the bytes (from the artifact store) before render;
// RenderRFC5322 base64-encodes them into a multipart part.
type OutboundAttachment struct {
	// Filename is the operator-visible name; sanitised at render to
	// strip CR/LF/quote header-injection vectors and reduce to a
	// basename.
	Filename string
	// ContentType is the MIME type; falls back to
	// application/octet-stream when empty.
	ContentType string
	// Content holds the raw (already-decoded) file bytes.
	Content []byte
}

// RenderRFC5322 turns an OutboundMessage into the wire-format
// bytes the SMTP DATA command consumes. Generates a Message-ID
// derived from the channel name + a random hex suffix; the second
// return value is that ID (angle-stripped) so Channel.Send can
// hand it back to the caller for InReplyTo threading.
//
// Threading rules (RFC 5322 §3.6.4):
//
//   - In-Reply-To: gets the immediate parent's Message-ID.
//   - References: gets the full chain (root first, parent last);
//     the channel supplies an already-joined string here.
//
// Headers are emitted in RFC 5322 / RFC 5321 friendly order
// (envelope-relevant headers first, threading headers next, content
// headers last). Body is appended verbatim — the caller is
// responsible for any plaintext normalisation.
func RenderRFC5322(m OutboundMessage) (rendered []byte, messageID string, err error) {
	if strings.TrimSpace(m.From) == "" {
		return nil, "", fmt.Errorf("email: From is required")
	}
	if strings.TrimSpace(m.To) == "" {
		return nil, "", fmt.Errorf("email: To is required")
	}
	date := m.Date
	if date.IsZero() {
		date = time.Now()
	}
	msgID, err := newMessageID(m.From)
	if err != nil {
		return nil, "", err
	}

	var b bytes.Buffer
	writeHeader(&b, "From", m.From)
	writeHeader(&b, "To", m.To)
	writeHeader(&b, "Subject", m.Subject)
	writeHeader(&b, "Date", date.UTC().Format(time.RFC1123Z))
	writeHeader(&b, "Message-ID", "<"+msgID+">")
	if m.InReplyTo != "" {
		writeHeader(&b, "In-Reply-To", "<"+m.InReplyTo+">")
	}
	if m.References != "" {
		writeHeader(&b, "References", renderReferencesHeader(m.References))
	}
	writeHeader(&b, "MIME-Version", "1.0")

	if len(m.Attachments) == 0 {
		writeHeader(&b, "Content-Type", "text/plain; charset=UTF-8")
		writeHeader(&b, "Content-Transfer-Encoding", "8bit")
		b.WriteString("\r\n")
		// Body — CRLF-normalised so we don't emit bare LFs into an SMTP
		// DATA stream, which some MTAs reject.
		b.WriteString(normaliseCRLF(m.Body))
		return b.Bytes(), msgID, nil
	}
	if err := writeMultipartBody(&b, m); err != nil {
		return nil, "", err
	}
	return b.Bytes(), msgID, nil
}

// writeMultipartBody renders the multipart/mixed body for a message that
// carries attachments: the top-level Content-Type header (with a fresh random
// boundary) followed by a text/plain part for the body and one base64 part per
// attachment. Header values (filename, content-type) are sanitised so an
// attachment name can't inject extra MIME headers.
func writeMultipartBody(b *bytes.Buffer, m OutboundMessage) error {
	boundary, err := genBoundary()
	if err != nil {
		return err
	}
	writeHeader(b, "Content-Type", `multipart/mixed; boundary="`+boundary+`"`)
	b.WriteString("\r\n")

	mw := multipart.NewWriter(b)
	if err := mw.SetBoundary(boundary); err != nil {
		return err
	}
	textPart, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=UTF-8"},
		"Content-Transfer-Encoding": {"8bit"},
	})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(textPart, normaliseCRLF(m.Body)); err != nil {
		return err
	}
	for _, a := range m.Attachments {
		ct := stripHeaderCRLF(a.ContentType)
		if ct == "" {
			ct = "application/octet-stream"
		}
		part, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {ct},
			"Content-Transfer-Encoding": {"base64"},
			"Content-Disposition":       {`attachment; filename="` + sanitizeAttachmentFilename(a.Filename) + `"`},
		})
		if err != nil {
			return err
		}
		if err := writeBase64Wrapped(part, a.Content); err != nil {
			return err
		}
	}
	return mw.Close()
}

// genBoundary returns a random MIME multipart boundary that cannot collide
// with realistic attachment content.
func genBoundary() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "vornik-boundary-" + hex.EncodeToString(raw[:]), nil
}

// sanitizeAttachmentFilename reduces a filename to a safe quoted-string value:
// basename only, with CR/LF and double-quotes stripped so it cannot break out
// of the Content-Disposition header or inject additional MIME headers.
func sanitizeAttachmentFilename(name string) string {
	name = stripHeaderCRLF(name)
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ReplaceAll(name, `"`, "")
	// Collapse internal whitespace runs (CR/LF were already turned into
	// spaces) so a multi-line injection attempt reads as one tidy token.
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "attachment.bin"
	}
	return name
}

// stripHeaderCRLF removes CR/LF and collapses surrounding whitespace so a
// value is safe to place in a single MIME header line.
func stripHeaderCRLF(v string) string {
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	return strings.TrimSpace(v)
}

// writeBase64Wrapped writes content as base64 wrapped at the RFC 2045 76-column
// line limit with CRLF endings.
func writeBase64Wrapped(w io.Writer, content []byte) error {
	enc := base64.StdEncoding.EncodeToString(content)
	const lineLen = 76
	for i := 0; i < len(enc); i += lineLen {
		end := i + lineLen
		if end > len(enc) {
			end = len(enc)
		}
		if _, err := io.WriteString(w, enc[i:end]+"\r\n"); err != nil {
			return err
		}
	}
	return nil
}

// renderReferencesHeader prefixes each space-separated entry with
// angle brackets. Input is the joined References: chain the
// caller supplies (raw IDs, no angles); RFC 5322 requires
// msg-id syntax in headers.
func renderReferencesHeader(refs string) string {
	parts := strings.Fields(refs)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Trim(p, "<> \t")
		if p == "" {
			continue
		}
		out = append(out, "<"+p+">")
	}
	return strings.Join(out, " ")
}

// writeHeader emits a single `Key: Value\r\n` line.
func writeHeader(b *bytes.Buffer, key, value string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(sanitizeHeaderValue(value))
	b.WriteString("\r\n")
}

func sanitizeHeaderValue(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.Join(strings.Fields(value), " ")
}

// normaliseCRLF rewrites the body to use CRLF line endings.
// Defensive: bare LFs in an SMTP DATA stream are a common cause
// of "bare LF in DATA, message rejected" 451 responses from
// strict MTAs.
func normaliseCRLF(in string) string {
	// Quick scan: if every newline already has a preceding CR,
	// short-circuit to avoid the allocation. Loop is O(n) over
	// the body, which is fine for the email size we're sending.
	needsRewrite := false
	for i := 0; i < len(in); i++ {
		if in[i] == '\n' {
			if i == 0 || in[i-1] != '\r' {
				needsRewrite = true
				break
			}
		}
	}
	if !needsRewrite {
		return in
	}
	var b strings.Builder
	b.Grow(len(in) + 16)
	for i := 0; i < len(in); i++ {
		if in[i] == '\n' && (i == 0 || in[i-1] != '\r') {
			b.WriteByte('\r')
		}
		b.WriteByte(in[i])
	}
	return b.String()
}

// newMessageID synthesises an RFC 5322 Message-ID for an outbound
// mail. Format: `<hex>@<from-domain>`. The hex is 16 bytes of
// crypto/rand (32 hex chars) so the ID is collision-proof across
// daemon restarts. Falls back to a time-based suffix when
// crypto/rand is unavailable (essentially never, but keep the
// error path explicit).
func newMessageID(from string) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("email: generate Message-ID: %w", err)
	}
	host := messageIDHost(from)
	return hex.EncodeToString(buf[:]) + ".vornik@" + host, nil
}

// messageIDHost picks the right-hand side of the Message-ID. Uses
// the From: address's domain when parseable, else falls back to
// "vornik.local" — RFC 5322 doesn't require the host to be
// resolvable, just syntactically valid.
func messageIDHost(from string) string {
	at := strings.LastIndex(from, "@")
	if at < 0 || at == len(from)-1 {
		return "vornik.local"
	}
	host := strings.Trim(from[at+1:], "<>")
	host = strings.TrimSpace(host)
	if host == "" {
		return "vornik.local"
	}
	return host
}

// NewNetSMTPSender constructs the production SMTPSender, backed by
// net/smtp from stdlib. Slice 1 uses STARTTLS on submission port
// 587 by default; explicit TLS (port 465 implicit-TLS) is a
// slice-2 concern (rare in modern providers — Gmail/Outlook/Mailgun
// all offer 587 STARTTLS).
//
// Each Send opens a fresh connection (dial-per-send). At the email
// channel's expected cadence — operator replies, not bulk delivery —
// the overhead is negligible and the per-call dial keeps the seam
// stateless. Slice 2 can pool when measured volume justifies it.
func NewNetSMTPSender(host string, port int, username, password string) SMTPSender {
	if port == 0 {
		port = 587
	}
	return &netSMTPSender{
		addr: fmt.Sprintf("%s:%d", host, port),
		host: host,
		auth: smtp.PlainAuth("", username, password, host),
	}
}

type netSMTPSender struct {
	addr string
	host string
	auth smtp.Auth
}

func (s *netSMTPSender) Send(ctx context.Context, req SMTPSendRequest) error {
	// net/smtp doesn't take a context — best we can do is short-
	// circuit if the caller's context is already cancelled and
	// rely on the per-connection timeouts inside the stdlib. A
	// real production hardening pass (slice 2) would wrap a Dialer
	// with the context's deadline.
	if err := ctx.Err(); err != nil {
		return err
	}
	// SMTP envelope (MAIL FROM / RCPT TO) must be bare addresses
	// per RFC 5321. Display-name forms like
	// `"Assistant <bot@vornik.io>"` are only valid in the RFC
	// 5322 From: header (the message payload). Gmail rejects a
	// non-bare envelope with `555 5.5.2 Syntax error, cannot
	// decode response.` envelopeAddress strips the display name
	// for the wire envelope; the rendered Payload still carries
	// the full display-name From: header that the recipient sees.
	from := envelopeAddress(req.From)
	to := make([]string, len(req.To))
	for i, addr := range req.To {
		to[i] = envelopeAddress(addr)
	}
	return smtp.SendMail(s.addr, s.auth, from, to, req.Payload)
}

func (s *netSMTPSender) Close() error { return nil }

// envelopeAddress normalises an address string to the bare
// local@domain form RFC 5321 requires in the SMTP envelope. Accepts
// any RFC 5322 address form (`Name <addr>`, `<addr>`, plain
// `addr`) and returns just the address. Falls back to the input
// verbatim when net/mail can't parse it — the operator's SMTP
// server will reject downstream with a real error rather than
// silently rewriting an intended value.
func envelopeAddress(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}
	addr, err := mail.ParseAddress(trimmed)
	if err != nil || addr == nil {
		return raw
	}
	return addr.Address
}
