package email

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

// IMAPClient is the narrow inbound-transport seam the email
// channel depends on. Production wiring supplies an
// emersion/go-imap-backed adapter (see imap_emersion.go); tests
// inject an in-memory fake (see imap_fake_test.go). The interface
// intentionally exposes only the four operations the channel
// needs — full IMAP capability isn't part of the abstraction.
//
// All methods take a context and may return when ctx is cancelled.
// Connect must be called before FetchUnseen / MarkSeen; Close
// tears down the connection and is safe to call multiple times.
type IMAPClient interface {
	// Connect dials the IMAP server, authenticates with the given
	// credentials, and selects the configured mailbox. Returns a
	// non-nil error on any failure; the channel surfaces that as a
	// boot-time fatal so a misconfigured server is loud.
	Connect(ctx context.Context, cfg IMAPDialConfig) error

	// FetchUnseen returns every message in the selected mailbox
	// whose \Seen flag is not set. Implementations should return
	// the RFC 5322 wire bytes (headers + body) so the channel can
	// run its own parser — the abstraction does NOT do parsing.
	// An empty slice is a valid response when no new mail has
	// arrived; callers don't treat it as an error.
	FetchUnseen(ctx context.Context) ([]RawMessage, error)

	// MarkSeen sets the \Seen flag on the message with the given
	// IMAP UID so the next FetchUnseen cycle doesn't re-deliver
	// it. Returns an error on transport failures; the channel
	// logs but doesn't propagate (a re-poll will re-attempt).
	MarkSeen(ctx context.Context, uid string) error

	// Close tears down the IMAP connection. Idempotent.
	Close() error
}

// IMAPDialConfig is the per-Connect parameter bundle. Holding the
// per-call surface in one struct keeps the IMAPClient signature
// from drifting when a future field (e.g. OAuth2 token instead of
// password) lands.
type IMAPDialConfig struct {
	// Host is the IMAP server hostname (e.g. imap.gmail.com).
	Host string

	// Port is the IMAP server port. Zero means "use TLS on 993";
	// the production adapter picks the default.
	Port int

	// Username is the IMAP login username.
	Username string

	// Password is the IMAP login password (or app-specific
	// password for providers like Gmail that gate IMAP behind
	// per-app credentials).
	Password string

	// Mailbox is the IMAP folder to SELECT after login. Empty
	// defaults to "INBOX" at the channel layer; this struct just
	// passes the operator-supplied value through.
	Mailbox string
}

// RawMessage is the IMAPClient's per-message return value. The
// adapter populates UID with the IMAP UID (so MarkSeen can target
// the same message later) and Body with the wire-format RFC 5322
// bytes. Parsing happens at the channel layer, not in the
// adapter, so the same parser runs for live IMAP and for the test
// fake.
type RawMessage struct {
	// UID is the IMAP UID of this message in the selected
	// mailbox. Opaque string at the IMAPClient layer — adapters
	// may encode their own provider-private form here as long as
	// MarkSeen accepts the same value back.
	UID string

	// Body is the full RFC 5322 wire-format payload (headers +
	// blank line + body). The channel passes this to ParseRFC5322
	// to extract Subject / From / Body / threading headers.
	Body []byte
}

// ParsedMessage is the headers-plus-body shape ParseRFC5322
// produces. Holds only the fields the email channel needs;
// further per-feature parsing (attachments, HTML/text alternative
// resolution, DKIM-Signature header dissection) is the consumer's
// responsibility.
type ParsedMessage struct {
	// MessageID is the angle-bracket-stripped Message-ID: header
	// value. Empty when the upstream didn't supply one (rare —
	// every well-formed mail has one).
	MessageID string

	// From is the bare address from the From: header
	// (display-name stripped). Lowercased for allowlist
	// comparisons — the channel's allowlist is case-insensitive.
	From string

	// Subject is the decoded Subject: header. Empty when absent.
	Subject string

	// Body is the message body, plaintext. The slice-1 parser
	// takes the text/plain part of multipart/alternative messages
	// when present; HTML-only messages run through a naive
	// strip-tags fallback.
	Body string

	// Attachments are the non-text MIME parts extracted by the
	// slice-2 body walker. Empty when the message had no
	// attachments OR when the channel's attachment-persistence
	// wiring isn't configured. Inline images (Content-Disposition:
	// inline) are NOT in this list — they're part of the rendered
	// HTML body, not user-uploaded artifacts.
	Attachments []ParsedAttachment

	// InReplyTo is the angle-bracket-stripped In-Reply-To header.
	// Empty for top-level messages.
	InReplyTo string

	// References is the parsed References: header — a
	// space-separated list of Message-IDs, oldest-first.
	References []string

	// Date is the parsed Date: header. Zero when absent or
	// unparseable; the channel falls back to the wall clock.
	Date time.Time

	// AuthResults captures every "Authentication-Results:" header on
	// the message (multi-hop relays each stamp one) so the slice-3
	// HeaderAuthVerifier can inspect upstream MTA verdicts without
	// re-doing the DKIM/SPF crypto in-process. Empty when the
	// upstream relay didn't stamp the header.
	AuthResults []string
	// ReceivedSPF captures every "Received-SPF:" header on the
	// message. Same posture as AuthResults — used by the verifier to
	// honour upstream SPF outcomes when the relay didn't fold them
	// into Authentication-Results. Empty is the common case for
	// providers that prefer Authentication-Results exclusively.
	ReceivedSPF []string
}

// ParseRFC5322 turns an RFC 5322 wire-format message into a
// ParsedMessage. Defensive on every header — a missing Subject
// becomes "", not an error; a missing Date becomes the zero time;
// only a structurally broken envelope (no header/body separator,
// unparseable From:) escapes as an error.
//
// Slice 1 limitations called out for the reader:
//
//   - Plaintext bodies are returned as-is. Quoted-printable /
//     base64 transfer encodings are decoded.
//   - multipart/alternative is reduced to the text/plain part when
//     present; otherwise the first text/* part wins.
//   - HTML-only messages get a naive strip-tags fallback (slice 1).
//   - Attachments (non-text parts) are dropped with no record on
//     the ParsedMessage — the channel logs a TODO for slice 2.
func ParseRFC5322(raw []byte) (ParsedMessage, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ParsedMessage{}, fmt.Errorf("read message: %w", err)
	}

	out := ParsedMessage{
		MessageID:   stripAngles(strings.TrimSpace(msg.Header.Get("Message-ID"))),
		Subject:     decodeHeader(msg.Header.Get("Subject")),
		InReplyTo:   stripAngles(strings.TrimSpace(msg.Header.Get("In-Reply-To"))),
		References:  parseReferences(msg.Header.Get("References")),
		AuthResults: collectHeader(msg.Header, "Authentication-Results"),
		ReceivedSPF: collectHeader(msg.Header, "Received-SPF"),
	}

	from := strings.TrimSpace(msg.Header.Get("From"))
	if from != "" {
		addr, err := mail.ParseAddress(from)
		if err != nil {
			// Tolerate non-parseable From: by falling back to the raw
			// value — the allowlist check will reject it cleanly if
			// it's not on the list.
			out.From = strings.ToLower(from)
		} else {
			out.From = strings.ToLower(addr.Address)
		}
	}
	if d, err := mail.ParseDate(msg.Header.Get("Date")); err == nil {
		out.Date = d
	}

	body, err := io.ReadAll(msg.Body)
	if err != nil {
		return ParsedMessage{}, fmt.Errorf("read body: %w", err)
	}
	parsed := parseMessageBody(msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), body)
	out.Body = parsed.Text
	out.Attachments = parsed.Attachments
	return out, nil
}

// extractTextBody picks the plaintext rendering of a message body.
// Slice 1: this is intentionally minimal. multipart/alternative
// chooses text/plain when present; multipart/mixed and friends
// also pick the first text/plain part; HTML-only messages fall
// through stripHTMLTags.
//
// The fancy cases — nested multiparts, base64-encoded attachments,
// quoted-printable text bodies — are handled by mime/multipart and
// mime/quotedprintable in stdlib so we don't reinvent them.
//
// Slice 3: charset transcoding now honours the Content-Type charset
// param (ISO-8859-*, Windows-125*, GB2312, Shift_JIS, …) — non-UTF-8
// bodies arrive decoded into UTF-8 instead of surfacing raw to the
// LLM. Attachment extraction lives on parseMessageBody (slice 2);
// this path stays text-only.
func extractTextBody(contentType, transferEnc string, body []byte) string {
	mt, params := parseContentType(contentType)
	if strings.HasPrefix(mt, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return decodePart(transferEnc, params["charset"], body)
		}
		return extractTextFromMultipart(boundary, body)
	}
	decoded := decodePart(transferEnc, params["charset"], body)
	if strings.HasPrefix(mt, "text/html") {
		return stripHTMLTags(decoded)
	}
	return decoded
}

// collectHeader returns every occurrence of the named header. Mail
// servers stamp multiple Authentication-Results / Received-SPF
// headers (one per relay hop) and the slice-3 verifier needs to
// inspect every one — net/mail's Header.Get returns only the first.
// Header names are matched case-insensitively per RFC 5322 §2.2.
func collectHeader(h mail.Header, name string) []string {
	canon := textproto.CanonicalMIMEHeaderKey(name)
	vals := h[canon]
	if len(vals) == 0 {
		return nil
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// stripAngles removes the leading `<` and trailing `>` IMAP wraps
// around Message-IDs. Defensive — both forms appear in the wild
// depending on the upstream's RFC-compliance posture.
func stripAngles(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return s
}

// parseReferences splits a References: header into individual
// Message-IDs, angle-stripped. Whitespace-separated per RFC 5322;
// we tolerate stray commas (some non-compliant clients emit them).
func parseReferences(header string) []string {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil
	}
	fields := strings.FieldsFunc(header, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == ','
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = stripAngles(f)
		if f == "" {
			continue
		}
		out = append(out, f)
	}
	return out
}
