package configassist

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The canary is a NON-CREDENTIAL marker: the point is to prove where bytes
// travel, not to put a real secret in a repository test.
const multilineSecretCanary = "CANARY-MULTILINE-SECRET-MUST-NOT-EGRESS"

// Regression: audit 2026-09-15 CA-03 — "Multiline Secret Material Reaches the
// Assistant Transport". The sanitizer is line-oriented, so for a block
// scalar it replaced the `|` MARKER with a placeholder and left the indented
// secret body sitting in the snapshot, where file_read then handed it to the
// model. The design promise for named_secrets[].value is unconditional
// redaction (§10a), and a YAML representation must not be able to defeat it.
func TestSanitize_BlockScalarSecretsAreRedacted(t *testing.T) {
	cases := map[string]string{
		"literal block":              "projectId: assistant\nnamed_secrets:\n  - name: PEM\n    value: |\n      " + multilineSecretCanary + "\n      second-line-of-the-key\n",
		"folded block":               "projectId: assistant\nnamed_secrets:\n  - name: PEM\n    value: >\n      " + multilineSecretCanary + "\n      second-line-of-the-key\n",
		"strip chomp":                "projectId: assistant\nnamed_secrets:\n  - name: PEM\n    value: |-\n      " + multilineSecretCanary + "\n",
		"keep chomp":                 "projectId: assistant\nnamed_secrets:\n  - name: PEM\n    value: |+\n      " + multilineSecretCanary + "\n",
		"explicit indent":            "projectId: assistant\nnamed_secrets:\n  - name: PEM\n    value: |2\n      " + multilineSecretCanary + "\n",
		"secret-bearing key by name": "projectId: assistant\nchat:\n  api_key: |\n    " + multilineSecretCanary + "\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			root := writeTree(t, map[string]string{"projects/assistant.yaml": content})
			s, err := Build(root, nil, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			got := string(s.Files["projects/assistant.yaml"])
			if strings.Contains(got, multilineSecretCanary) {
				t.Fatalf("multiline secret body survived sanitization:\n%s", got)
			}
			if strings.Contains(got, "second-line-of-the-key") {
				t.Fatalf("continuation line of a secret block survived:\n%s", got)
			}
			if s.Placeholders() == 0 {
				t.Fatal("the redaction must register a placeholder so re-materialisation restores the real value")
			}
		})
	}
}

// The redacted block must still re-materialise to the ORIGINAL bytes, or a
// round-trip through the assistant would silently rewrite a real secret.
func TestSanitize_BlockScalarRematerialisesExactly(t *testing.T) {
	original := "projectId: assistant\nnamed_secrets:\n  - name: PEM\n    value: |\n      " + multilineSecretCanary + "\n      second-line-of-the-key\nautonomy:\n  goal: be useful\n"
	root := writeTree(t, map[string]string{"projects/assistant.yaml": original})
	s, err := Build(root, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	back, err := s.Rematerialize("projects/assistant.yaml", s.Files["projects/assistant.yaml"])
	if err != nil {
		t.Fatal(err)
	}
	if back != original {
		t.Fatalf("re-materialisation is not byte-exact:\nwant:\n%q\ngot:\n%q", original, back)
	}
}

// Regression: audit 2026-09-15 CA-05 — "The Stale-Base Record Is Captured Too
// Late". Build saves the bytes the model reasons over, but BaseHashes used to
// re-read the DEPLOYED file when the proposal was filed — after the assistant
// and the judge had finished. An operator editing that file in the interval
// got a proposal whose full-file replacement derived from the OLD content but
// whose hash authenticated the NEW file, so the apply engine's stale-base
// check passed and silently overwrote their change.
func TestBaseHashes_AuthenticateTheSnAPSHOTNotTheLaterFile(t *testing.T) {
	root := writeTree(t, map[string]string{
		"projects/assistant.yaml": "projectId: assistant\nautonomy:\n  maxTasksPerHour: 4\n",
	})
	s, err := Build(root, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !strings.Contains(string(s.Files["projects/assistant.yaml"]), "maxTasksPerHour: 4") {
		t.Fatal("fixture: the snapshot should hold the original value")
	}
	// The operator edits the deployed file while the model is thinking.
	concurrent := "projectId: assistant\nautonomy:\n  maxTasksPerHour: 99\n"
	if err := os.WriteFile(filepath.Join(root, "projects", "assistant.yaml"), []byte(concurrent), 0o600); err != nil {
		t.Fatal(err)
	}
	hashes, err := s.BaseHashes([]string{"projects/assistant.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if hashes["projects/assistant.yaml"] == hashOf(concurrent) {
		t.Fatal("the base hash authenticates the CONCURRENTLY EDITED file: applying would silently overwrite it")
	}
	if hashes["projects/assistant.yaml"] != hashOf("projectId: assistant\nautonomy:\n  maxTasksPerHour: 4\n") {
		t.Fatalf("the base hash must be the snapshot's own pre-image, got %s", hashes["projects/assistant.yaml"])
	}
}

// hashOf mirrors the sha256 hex the snapshot records for a file's bytes.
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
