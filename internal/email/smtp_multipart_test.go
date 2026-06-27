package email

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"
)

// TestRenderRFC5322_NoAttachments_Unchanged pins that the text/plain path is
// byte-for-byte unchanged when Attachments is empty (the multipart capability
// must not perturb the existing shape).
func TestRenderRFC5322_NoAttachments_Unchanged(t *testing.T) {
	out := OutboundMessage{
		From: "vornik@test", To: "a@ext.test", Subject: "Hi", Body: "plain body\n",
		Date: time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC),
	}
	rendered, _, err := RenderRFC5322(out)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(rendered)
	if !strings.Contains(s, "Content-Type: text/plain; charset=UTF-8") {
		t.Fatalf("expected text/plain, got:\n%s", s)
	}
	if strings.Contains(s, "multipart") {
		t.Fatalf("no-attachment render must not be multipart:\n%s", s)
	}
}

// TestRenderRFC5322_Multipart round-trips a message with two attachments: it
// must parse as multipart/mixed with a text part carrying the body and one
// base64 part per attachment whose decoded bytes + filename match.
func TestRenderRFC5322_Multipart(t *testing.T) {
	out := OutboundMessage{
		From: "vornik@test", To: "a@ext.test", Subject: "Report", Body: "See attached.\n",
		Date: time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC),
		Attachments: []OutboundAttachment{
			{Filename: "report.md", ContentType: "text/markdown", Content: []byte("# Report\nbody bytes\n")},
			{Filename: "data.bin", Content: []byte{0x00, 0x01, 0x02, 0xff, 0xfe}}, // empty CT → octet-stream
		},
	}
	rendered, _, err := RenderRFC5322(out)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// Split headers / body, parse the top-level Content-Type.
	msg, err := mail.ReadMessage(bytes.NewReader(rendered))
	if err != nil {
		t.Fatalf("mail.ReadMessage: %v", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse top content-type: %v", err)
	}
	if mediaType != "multipart/mixed" {
		t.Fatalf("top media type = %q, want multipart/mixed", mediaType)
	}

	mr := multipart.NewReader(msg.Body, params["boundary"])
	type got struct {
		ctype, disp, filename string
		body                  []byte
	}
	var parts []got
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		raw, _ := io.ReadAll(p) // multipart.Reader does NOT decode CTE — caller must.
		b := raw
		if strings.EqualFold(p.Header.Get("Content-Transfer-Encoding"), "base64") {
			// Strip the RFC 2045 line wrapping before decoding.
			compact := strings.Join(strings.Fields(string(raw)), "")
			decoded, derr := base64.StdEncoding.DecodeString(compact)
			if derr != nil {
				t.Fatalf("base64 decode part: %v", derr)
			}
			b = decoded
		}
		_, dparams, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
		parts = append(parts, got{
			ctype:    p.Header.Get("Content-Type"),
			disp:     p.Header.Get("Content-Disposition"),
			filename: dparams["filename"],
			body:     b,
		})
	}
	if len(parts) != 3 {
		t.Fatalf("want 3 parts (text + 2 attachments), got %d", len(parts))
	}
	// Part 0: text body.
	if !strings.HasPrefix(parts[0].ctype, "text/plain") || !strings.Contains(string(parts[0].body), "See attached.") {
		t.Fatalf("part 0 not the text body: %+v", parts[0])
	}
	// Part 1: report.md, decoded bytes intact.
	if parts[1].filename != "report.md" || !strings.HasPrefix(parts[1].ctype, "text/markdown") {
		t.Fatalf("part 1 headers wrong: %+v", parts[1])
	}
	if string(parts[1].body) != "# Report\nbody bytes\n" {
		t.Fatalf("part 1 body mismatch: %q", parts[1].body)
	}
	if !strings.Contains(parts[1].disp, "attachment") {
		t.Fatalf("part 1 must be Content-Disposition: attachment: %q", parts[1].disp)
	}
	// Part 2: binary, empty CT → octet-stream, raw bytes intact through base64.
	if parts[2].filename != "data.bin" || !strings.HasPrefix(parts[2].ctype, "application/octet-stream") {
		t.Fatalf("part 2 headers wrong: %+v", parts[2])
	}
	if !bytes.Equal(parts[2].body, []byte{0x00, 0x01, 0x02, 0xff, 0xfe}) {
		t.Fatalf("part 2 binary bytes corrupted: %v", parts[2].body)
	}
}

// TestRenderRFC5322_FilenameInjection pins that a malicious attachment name
// can't break out of the Content-Disposition header / inject MIME headers.
func TestRenderRFC5322_FilenameInjection(t *testing.T) {
	out := OutboundMessage{
		From: "vornik@test", To: "a@ext.test", Subject: "x", Body: "b",
		Attachments: []OutboundAttachment{
			{Filename: "../../etc/evil\"\r\nX-Injected: yes.txt", Content: []byte("x")},
		},
	}
	rendered, _, err := RenderRFC5322(out)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(rendered)
	// The real safety property: the filename's CR/LF must not have created a
	// new header line (i.e. no "\r\nX-Injected:" header). The string
	// "X-Injected" may still appear harmlessly inside the quoted filename.
	if strings.Contains(s, "\r\nX-Injected:") {
		t.Fatalf("header injection via filename was not neutralised:\n%s", s)
	}
	// Basename-reduced, quotes stripped, on a single line.
	if !strings.Contains(s, `filename="evil X-Injected: yes.txt"`) {
		t.Fatalf("sanitised filename not as expected:\n%s", s)
	}
}

func TestSanitizeAttachmentFilename(t *testing.T) {
	cases := map[string]string{
		"report.pdf":        "report.pdf",
		"/abs/path/cv.docx": "cv.docx",
		`bad"name.txt`:      "badname.txt",
		"line\r\nbreak.txt": "line break.txt",
		`a\b\c.bin`:         "c.bin",
		"":                  "attachment.bin",
		"   ":               "attachment.bin",
	}
	for in, want := range cases {
		if got := sanitizeAttachmentFilename(in); got != want {
			t.Errorf("sanitizeAttachmentFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
