package mediakind

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		fileName string
		mime     string
		want     Kind
	}{
		// MIME is authoritative when it carries a signal.
		{"jpeg by mime", "whatever", "image/jpeg", KindImage},
		{"png by mime", "x", "image/png", KindImage},
		{"audio by mime", "x", "audio/ogg", KindAudio},
		{"video by mime", "x", "video/mp4", KindVideo},
		{"pdf by mime", "x", "application/pdf", KindDocument},
		{"epub by mime", "x", "application/epub+zip", KindDocument},
		{"plain text by mime", "x", "text/plain", KindDocument},

		// Extension fallback when MIME is absent or generic.
		{"jpg by ext", "photo.jpg", "", KindImage},
		{"jpeg by ext", "photo.JPEG", "", KindImage},
		{"heic by ext", "photo.heic", "", KindImage},
		{"opus by ext", "voice.opus", "", KindAudio},
		{"mkv by ext", "clip.mkv", "", KindVideo},
		{"epub by ext", "book.epub", "", KindDocument},
		{"md by ext", "notes.md", "", KindDocument},
		{"octet-stream falls back to ext", "photo.png", "application/octet-stream", KindImage},
		{"binary octet-stream falls back to ext", "clip.mp4", "binary/octet-stream", KindVideo},

		// T-1df3's exact input.
		{"T-1df3 photo", "photo.jpg", "image/jpeg", KindImage},

		// Disagreement resolves to MIME — a channel that declares a
		// real type is more trustworthy than a filename a user chose.
		{"mime wins over ext", "photo.jpg", "audio/mpeg", KindAudio},

		// Unknown stays unknown: staging treats it conservatively.
		{"no signal at all", "mystery", "", KindUnknown},
		{"unknown ext", "thing.xyz", "", KindUnknown},
		{"unknown mime, unknown ext", "thing.xyz", "application/x-wat", KindUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.fileName, tc.mime); got != tc.want {
				t.Errorf("Classify(%q, %q) = %v, want %v", tc.fileName, tc.mime, got, tc.want)
			}
		})
	}
}

// Extraction is a sufficient stand-in only for documents. This is the
// predicate D1 got wrong: an image's OCR+EXIF extraction replaced the
// pixels (T-1df3), and D4's trailer told the lead the work was done.
func TestExtractionSufficient(t *testing.T) {
	sufficient := map[Kind]bool{
		KindDocument: true,
		KindImage:    false,
		KindAudio:    false,
		KindVideo:    false,
		KindUnknown:  false,
	}
	for k, want := range sufficient {
		if got := ExtractionSufficient(k); got != want {
			t.Errorf("ExtractionSufficient(%v) = %v, want %v", k, got, want)
		}
	}
}

func TestCapabilities_DeclaredWins(t *testing.T) {
	declared := map[string][]Modality{
		// An operator declaring a model blind must win over the
		// pattern list, which would otherwise call this sighted.
		"claude-opus-4-7": {ModalityText},
		"custom-eyes":     {ModalityText, ModalityVision},
	}
	if Capabilities("claude-opus-4-7", declared).Can(ModalityVision) {
		t.Error("explicit [text] declaration must override the vision pattern match")
	}
	if !Capabilities("custom-eyes", declared).Can(ModalityVision) {
		t.Error("explicit [text, vision] declaration must grant vision to an unpatterned model")
	}
	if !Capabilities("custom-eyes", declared).Can(ModalityText) {
		t.Error("declared text modality lost")
	}
}

func TestCapabilities_PatternFallback(t *testing.T) {
	sighted := []string{
		"claude-opus-4-7",
		"gpt-4o",
		"gpt-5.4",
		"gemini-2.5-pro",
		"llava:13b",
		"qwen2.5-vl-7b",
		"pixtral-large",
		"some-vision-model",
		// The vision role's configured pair. Both MUST resolve
		// sighted or §4.5's requiredModalities validation fails the
		// deployment's own swarm config at load.
		"google.gemma-3-27b-it",
		"gemma4:31b",
	}
	for _, m := range sighted {
		if !Capabilities(m, nil).Can(ModalityVision) {
			t.Errorf("Capabilities(%q) should report vision", m)
		}
	}

	blind := []string{
		"glm-5.2",        // the dispatcher model — unproven, must default blind
		"zai.glm-5",      // its fallback
		"gpt-oss:20b",    // narrator/utility
		"minimax-m2.7",   // publisher
		"gemma-2-27b-it", // gemma 2 predates multimodal gemma
		"",
	}
	for _, m := range blind {
		if Capabilities(m, nil).Can(ModalityVision) {
			t.Errorf("Capabilities(%q) should NOT report vision", m)
		}
	}
}

// Fail-closed: an unknown model is assumed blind so the system hands
// over rather than sending pixels a model will ignore.
func TestCapabilities_FailsClosed(t *testing.T) {
	s := Capabilities("some-model-nobody-has-heard-of", nil)
	if s.Can(ModalityVision) {
		t.Error("unknown model must not be granted vision")
	}
	if s.Can(ModalityAudio) {
		t.Error("unknown model must not be granted audio")
	}
	if !s.Can(ModalityText) {
		t.Error("every model can read text")
	}
}

func TestCapabilities_CaseAndSpaceInsensitive(t *testing.T) {
	declared := map[string][]Modality{"Custom-Eyes": {ModalityText, ModalityVision}}
	if !Capabilities("  custom-eyes  ", declared).Can(ModalityVision) {
		t.Error("declaration lookup must be case-insensitive and space-trimmed")
	}
	if !Capabilities("GPT-4O", nil).Can(ModalityVision) {
		t.Error("pattern match must be case-insensitive")
	}
}

func TestParseModalities(t *testing.T) {
	got, err := ParseModalities([]string{"text", "VISION", " audio "})
	if err != nil {
		t.Fatalf("ParseModalities: %v", err)
	}
	set := NewSet(got...)
	for _, m := range []Modality{ModalityText, ModalityVision, ModalityAudio} {
		if !set.Can(m) {
			t.Errorf("modality %v missing from parsed set", m)
		}
	}
	if _, err := ParseModalities([]string{"telepathy"}); err == nil {
		t.Error("unknown modality must be a config error, not silently dropped")
	}
}

func TestSetString(t *testing.T) {
	if got := NewSet(ModalityText, ModalityVision).String(); got != "text,vision" {
		t.Errorf("Set.String() = %q, want %q", got, "text,vision")
	}
	if got := NewSet().String(); got != "" {
		t.Errorf("empty Set.String() = %q, want empty", got)
	}
}

func TestKindString(t *testing.T) {
	for k, want := range map[Kind]string{
		KindImage:    "image",
		KindAudio:    "audio",
		KindVideo:    "video",
		KindDocument: "document",
		KindUnknown:  "unknown",
	} {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(k), got, want)
		}
	}
}

func TestModalityString(t *testing.T) {
	for m, want := range map[Modality]string{
		ModalityText:   "text",
		ModalityVision: "vision",
		ModalityAudio:  "audio",
	} {
		if got := m.String(); got != want {
			t.Errorf("Modality(%d).String() = %q, want %q", int(m), got, want)
		}
	}
}

// MIME types arrive with parameters on some channels
// ("text/plain; charset=utf-8"); the parameter must not defeat the
// prefix match and send a text document down the extension path.
func TestClassify_StripsMIMEParameters(t *testing.T) {
	if got := Classify("notes", "text/plain; charset=utf-8"); got != KindDocument {
		t.Errorf("parameterised text/plain classified as %v, want document", got)
	}
	if got := Classify("photo", "image/jpeg;  quality=80"); got != KindImage {
		t.Errorf("parameterised image/jpeg classified as %v, want image", got)
	}
}

// A concrete but unrecognised MIME type still gets an extension pass —
// the type may be novel while the extension is ordinary.
func TestClassify_UnknownMIMEFallsBackToExtension(t *testing.T) {
	if got := Classify("clip.mp4", "application/x-newfangled"); got != KindVideo {
		t.Errorf("unknown MIME with .mp4 classified as %v, want video", got)
	}
}

func TestClassify_DocumentMIMEWithoutExtension(t *testing.T) {
	if got := Classify("attachment", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"); got != KindDocument {
		t.Errorf("docx MIME classified as %v, want document", got)
	}
}
