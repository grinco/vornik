package configassist

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// received returns every message body this provider was handed, joined — the
// TRANSPORT boundary. Asserting on the snapshot helper alone is what let CA-03
// reopen: the helper was fixed for one YAML shape and the bytes still reached
// the model through file_read for two others.
func (s *scriptedProvider) received() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, conv := range s.messages {
		for _, m := range conv {
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// Regression: re-audit 2026-09-15 CA-03 (REOPENED) — "anchored and quoted-key
// YAML secrets still reach the assistant".
//
// The first fix taught a LINE-ORIENTED sanitiser about block scalars. YAML's
// representation space is larger than any line regex: `value: &pem |` puts an
// anchor between the key and the block header so only the anchor line was
// replaced, and `"value": |` does not match secretLineRe's unquoted-key
// pattern at all. Both left the indented secret body in the snapshot, and
// file_read handed it to the model.
//
// THE SEAM, and the reason this test is at the provider and not the helper:
// redaction is only true if the bytes never reach the transport. A sanitiser
// test asserts what the sanitiser did; this asserts what the model received.
//
// The canary is a NON-CREDENTIAL marker.
func TestPropose_NoSecretRepresentationReachesTheProvider(t *testing.T) {
	const canary = "CANARY-REAUDIT-SECRET-MUST-NOT-EGRESS"
	cases := map[string]string{
		"plain block":           "projectId: assistant\nnamed_secrets:\n  - name: PEM\n    value: |\n      " + canary + "\nautonomy:\n  goal: be useful\n",
		"anchored block":        "projectId: assistant\nnamed_secrets:\n  - name: PEM\n    value: &pem |\n      " + canary + "\nautonomy:\n  goal: be useful\n",
		"quoted key":            "projectId: assistant\nnamed_secrets:\n  - name: PEM\n    \"value\": |\n      " + canary + "\nautonomy:\n  goal: be useful\n",
		"single-quoted key":     "projectId: assistant\nnamed_secrets:\n  - name: PEM\n    'value': |\n      " + canary + "\nautonomy:\n  goal: be useful\n",
		"anchored scalar":       "projectId: assistant\nnamed_secrets:\n  - name: PEM\n    value: &pem " + canary + "\nautonomy:\n  goal: be useful\n",
		"quoted key + scalar":   "projectId: assistant\nnamed_secrets:\n  - name: PEM\n    \"value\": " + canary + "\nautonomy:\n  goal: be useful\n",
		"tagged block":          "projectId: assistant\nnamed_secrets:\n  - name: PEM\n    value: !!str |\n      " + canary + "\nautonomy:\n  goal: be useful\n",
		"secret-bearing quoted": "projectId: assistant\nchat:\n  \"api_key\": |\n    " + canary + "\nautonomy:\n  goal: be useful\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if err := os.WriteFile(filepath.Join(f.root, "projects", "assistant.yaml"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			// The model reads the file, which is the egress path.
			f.assistant.script = []*chat.ChatResponse{
				toolResp("glm-5.2:cloud", `PLAN: ["projects/assistant.yaml"]`,
					call("c1", "file_read", map[string]any{"path": "projects/assistant.yaml"})),
				textResp("glm-5.2:cloud", "read it"),
			}
			res, err := f.engine.Propose(context.Background(), operatorReq("look at the project"))
			if err != nil {
				t.Fatal(err)
			}
			if got := f.assistant.received(); strings.Contains(got, canary) {
				t.Fatalf("%s: the secret body reached the assistant transport", name)
			}
			if got := f.judge.received(); strings.Contains(got, canary) {
				t.Fatalf("%s: the secret body reached the judge transport", name)
			}
			_ = res
		})
	}
}

// The post-condition that makes the above hold for representations nobody has
// thought of yet: after sanitising, every secret-bearing node the YAML parser
// can see must carry a placeholder. A file that still has a live secret value
// is REFUSED rather than shipped to a model.
func TestSanitize_RefusesAFileItCouldNotFullyRedact(t *testing.T) {
	const canary = "CANARY-REAUDIT-POSTCONDITION"
	root := writeTree(t, map[string]string{
		"projects/assistant.yaml": "projectId: assistant\nnamed_secrets:\n  - name: PEM\n    value: &pem |\n      " + canary + "\n",
	})
	s, err := Build(root, nil, Limits{})
	if err != nil {
		// Refusing to build is an acceptable outcome; leaking is not.
		if strings.Contains(err.Error(), canary) {
			t.Fatal("the refusal itself leaked the secret")
		}
		return
	}
	defer s.Close()
	if strings.Contains(string(s.Files["projects/assistant.yaml"]), canary) {
		t.Fatal("a secret survived sanitisation and the build did not refuse")
	}
}
