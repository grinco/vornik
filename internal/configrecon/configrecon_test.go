package configrecon

import (
	"bytes"
	"testing"
)

// swarmBare is a minimal swarm-file blob carrying a BARE agent image on one
// role, plus surrounding fields the normalizer must leave byte-identical.
const swarmBare = `---
swarmId: basic-swarm
displayName: Dev swarm
roles:
    - name: "lead"
      model: "zai.glm-5"
      runtime:
        image: "vornik-agent:latest"
        network: egress
    - name: "coder"
      model: "kimi-k2.5"
---
Body text that must not change: image: vornik-agent:latest is prose here.
`

// swarmQualified is swarmBare with the image already qualified.
const swarmQualified = `---
swarmId: basic-swarm
displayName: Dev swarm
roles:
    - name: "lead"
      model: "zai.glm-5"
      runtime:
        image: "ghcr.io/grinco/vornik-agent:latest"
        network: egress
    - name: "coder"
      model: "kimi-k2.5"
---
Body text that must not change: image: vornik-agent:latest is prose here.
`

// agentImageNormalizer returns the shipped agent-image-qualify normalizer from
// the live registry (test helper).
func agentImageNormalizer(t *testing.T) Normalizer {
	t.Helper()
	const name = "agent-image-qualify"
	for _, n := range registered {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("normalizer %q not registered", name)
	return Normalizer{}
}

// TestAgentImageQualify_RewritesBareLine asserts a bare image: line in a
// swarms/*.md blob is qualified, and ONLY the image line changes.
func TestAgentImageQualify_RewritesBareLine(t *testing.T) {
	n := agentImageNormalizer(t)
	out, changed, note := n.Normalize([]byte(swarmBare))
	if !changed {
		t.Fatalf("expected changed=true for a bare image line")
	}
	if note == "" {
		t.Errorf("expected a non-empty note when a rewrite fired")
	}
	if !bytes.Contains(out, []byte(`image: "ghcr.io/grinco/vornik-agent:latest"`)) {
		t.Errorf("output does not contain the qualified image line:\n%s", out)
	}
	if bytes.Contains(out, []byte(`image: "vornik-agent:latest"`)) {
		t.Errorf("output still contains the bare image line:\n%s", out)
	}
	// Byte-identity of everything except the single image line: swapping the
	// bare form for the qualified form in the input must reproduce the output
	// exactly (proves no other byte moved).
	want := []byte(swarmQualified)
	if !bytes.Equal(out, want) {
		t.Errorf("only the image line should change.\n got: %q\nwant: %q", out, want)
	}
}

// TestAgentImageQualify_LeavesQualifiedUnchanged asserts an already-qualified
// image is a no-op (changed=false), the idempotency floor.
func TestAgentImageQualify_LeavesQualifiedUnchanged(t *testing.T) {
	n := agentImageNormalizer(t)
	out, changed, _ := n.Normalize([]byte(swarmQualified))
	if changed {
		t.Errorf("already-qualified image must not change")
	}
	if !bytes.Equal(out, []byte(swarmQualified)) {
		t.Errorf("output must be byte-identical to input when unchanged")
	}
}

// TestAgentImageQualify_Idempotent asserts Normalize(Normalize(x)) == Normalize(x).
func TestAgentImageQualify_Idempotent(t *testing.T) {
	n := agentImageNormalizer(t)
	once, _, _ := n.Normalize([]byte(swarmBare))
	twice, changed, _ := n.Normalize(once)
	if changed {
		t.Errorf("second pass over normalized content must not change")
	}
	if !bytes.Equal(once, twice) {
		t.Errorf("normalizer is not idempotent")
	}
}

// TestAgentImageQualify_AppliesTo covers path matching on the
// config-root-relative path, including the host-local safety case (A5).
func TestAgentImageQualify_AppliesTo(t *testing.T) {
	n := agentImageNormalizer(t)
	cases := map[string]bool{
		"swarms/basic-swarm.md":             true,
		"configs/swarms/basic-swarm.md":     true,
		"swarms/trading-swarm.md":           true,
		"roles/role-library.md":             false, // not a swarm file
		"config.yaml":                       false,
		"swarms/nested/x.md":                false, // not directly under swarms/
		"projects/acme/swarms/x.md":         false, // host-local — MUST NOT match (A5)
		"configs/projects/acme/swarms/x.md": false, // host-local under configs/ prefix too
	}
	for rel, want := range cases {
		if got := n.AppliesTo(rel); got != want {
			t.Errorf("AppliesTo(%q) = %v, want %v", rel, got, want)
		}
	}
}

// TestAgentImageQualify_MalformedPassThrough asserts the no-error contract
// (A1/A2): content with no image line is returned verbatim, changed=false.
func TestAgentImageQualify_MalformedPassThrough(t *testing.T) {
	n := agentImageNormalizer(t)
	blob := []byte("this: is\nnot: a swarm\n# base_image: vornik-agent:latest (a comment, not the key)\n")
	out, changed, _ := n.Normalize(blob)
	if changed {
		t.Errorf("no real image key present → must not change")
	}
	if !bytes.Equal(out, blob) {
		t.Errorf("malformed/unrelated content must pass through verbatim")
	}
}

// TestNormalizeImageLine_ValueForms exercises the line-level value-core parser
// across quote styles and the complex-line pass-through (a bare value with a
// trailing inline comment is NOT rewritten — the normalizer never mangles a line
// it doesn't fully understand). This targets normalizeImageLine directly, since
// full-file Normalize only considers lines inside the YAML frontmatter fence.
func TestNormalizeImageLine_ValueForms(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		changed bool
	}{
		{"double-quoted-bare", `        image: "vornik-agent:latest"`, `        image: "ghcr.io/grinco/vornik-agent:latest"`, true},
		{"single-quoted-bare", `        image: 'vornik-agent:latest'`, `        image: 'ghcr.io/grinco/vornik-agent:latest'`, true},
		{"unquoted-bare", `        image: vornik-agent:latest`, `        image: ghcr.io/grinco/vornik-agent:latest`, true},
		{"unquoted-digest", `        image: vornik-agent@sha256:abc`, `        image: ghcr.io/grinco/vornik-agent@sha256:abc`, true},
		{"already-qualified", `        image: "ghcr.io/grinco/vornik-agent:latest"`, `        image: "ghcr.io/grinco/vornik-agent:latest"`, false},
		{"trailing-inline-comment-passthrough", `        image: vornik-agent:latest # bare + comment`, `        image: vornik-agent:latest # bare + comment`, false},
		{"non-agent-image", `        image: "docker.io/library/golang:1.25"`, `        image: "docker.io/library/golang:1.25"`, false},
		{"empty-value", `        image:`, `        image:`, false},
		{"not-image-key", `        base_image: vornik-agent:latest`, `        base_image: vornik-agent:latest`, false},
		{"preserves-crlf", "        image: \"vornik-agent:latest\"\r\n", "        image: \"ghcr.io/grinco/vornik-agent:latest\"\r\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := normalizeImageLine(tc.in)
			if changed != tc.changed {
				t.Errorf("changed = %v, want %v", changed, tc.changed)
			}
			if out != tc.want {
				t.Errorf("out = %q, want %q", out, tc.want)
			}
		})
	}
}

// swarmBodyImageOnly: frontmatter image ALREADY qualified, but the markdown
// BODY contains a lone `image: vornik-agent:latest` prose line. The body line
// must NOT be rewritten (review I1 — a config-tree guard never edits operator
// content at the git-committed mirror seam).
const swarmBodyImageOnly = `---
swarmId: basic-swarm
roles:
    - name: "lead"
      runtime:
        image: "ghcr.io/grinco/vornik-agent:latest"
---
The role prompt may literally discuss config, e.g.
image: vornik-agent:latest
is what a broken swarm file looks like — do not touch this prose.
`

// swarmFrontAndBodyImage: BARE image in frontmatter AND a lone image line in the
// body. Only the frontmatter line may be qualified.
const swarmFrontAndBodyImage = `---
swarmId: basic-swarm
roles:
    - name: "lead"
      runtime:
        image: "vornik-agent:latest"
---
Example of the defect we guard against:
image: vornik-agent:latest
`

// swarmFrontAndBodyImageWant is swarmFrontAndBodyImage with ONLY the frontmatter
// image qualified; the body line is byte-identical.
const swarmFrontAndBodyImageWant = `---
swarmId: basic-swarm
roles:
    - name: "lead"
      runtime:
        image: "ghcr.io/grinco/vornik-agent:latest"
---
Example of the defect we guard against:
image: vornik-agent:latest
`

// TestAgentImageQualify_BodyImageNotRewritten (review I1): a lone image line in
// the markdown body is left untouched when the frontmatter is already clean.
func TestAgentImageQualify_BodyImageNotRewritten(t *testing.T) {
	n := agentImageNormalizer(t)
	out, changed, note := n.Normalize([]byte(swarmBodyImageOnly))
	if changed {
		t.Errorf("a body-only image line must not be rewritten")
	}
	if note != "" {
		t.Errorf("no note expected when nothing in frontmatter changed, got %q", note)
	}
	if !bytes.Equal(out, []byte(swarmBodyImageOnly)) {
		t.Errorf("body content must be byte-identical:\n%s", out)
	}
}

// TestAgentImageQualify_OnlyFrontmatterImageRewritten (review I1): with a bare
// image in BOTH frontmatter and body, only the frontmatter line is qualified.
func TestAgentImageQualify_OnlyFrontmatterImageRewritten(t *testing.T) {
	n := agentImageNormalizer(t)
	out, changed, _ := n.Normalize([]byte(swarmFrontAndBodyImage))
	if !changed {
		t.Fatalf("the frontmatter image must be qualified")
	}
	if !bytes.Equal(out, []byte(swarmFrontAndBodyImageWant)) {
		t.Errorf("only the frontmatter image may change.\n got: %q\nwant: %q", out, swarmFrontAndBodyImageWant)
	}
}

// TestAgentImageQualify_NoFrontmatterFence: a file with no leading `---` fence
// has no frontmatter, so even a bare image line is left untouched.
func TestAgentImageQualify_NoFrontmatterFence(t *testing.T) {
	n := agentImageNormalizer(t)
	blob := []byte("# just markdown\nimage: vornik-agent:latest\nmore prose\n")
	out, changed, _ := n.Normalize(blob)
	if changed {
		t.Errorf("no frontmatter fence → nothing to normalise")
	}
	if !bytes.Equal(out, blob) {
		t.Errorf("fence-less file must pass through verbatim:\n%s", out)
	}
}

// TestApplyMirrorNormalizers_RunsOnlyMirrorSafe registers a throwaway
// ReconcilerOnly normalizer that WOULD fire and asserts ApplyMirrorNormalizers
// never runs it at the seam (only Risk==MirrorSafe run).
func TestApplyMirrorNormalizers_RunsOnlyMirrorSafe(t *testing.T) {
	restore := registered
	t.Cleanup(func() { registered = restore })

	reconcilerFired := false
	registered = []Normalizer{
		{
			Name:          "mirror-safe-probe",
			Justification: "test",
			Risk:          MirrorSafe,
			AppliesTo:     func(string) bool { return true },
			Normalize: func(c []byte) ([]byte, bool, string) {
				return append(c, []byte("+safe")...), true, "safe fired"
			},
		},
		{
			Name:          "reconciler-only-probe",
			Justification: "test",
			Risk:          ReconcilerOnly,
			AppliesTo:     func(string) bool { return true },
			Normalize: func(c []byte) ([]byte, bool, string) {
				reconcilerFired = true
				return append(c, []byte("+recon")...), true, "recon fired"
			},
		},
	}

	out, notes := ApplyMirrorNormalizers("swarms/x.md", []byte("base"))
	if reconcilerFired {
		t.Errorf("ReconcilerOnly normalizer must NOT run at the mirror seam")
	}
	if string(out) != "base+safe" {
		t.Errorf("output = %q, want the MirrorSafe transform only", out)
	}
	if len(notes) != 1 || notes[0].Name != "mirror-safe-probe" || !notes[0].Changed {
		t.Errorf("expected exactly one note from the MirrorSafe normalizer, got %+v", notes)
	}
	if notes[0].Message != "safe fired" {
		t.Errorf("note must bind name↔message; got %q", notes[0].Message)
	}
}

// TestApplyMirrorNormalizers_ChangeOnlyEmission (A4): an AppliesTo-match that
// makes NO change emits no note.
func TestApplyMirrorNormalizers_ChangeOnlyEmission(t *testing.T) {
	restore := registered
	t.Cleanup(func() { registered = restore })
	registered = []Normalizer{
		{
			Name:          "noop-probe",
			Justification: "test",
			Risk:          MirrorSafe,
			AppliesTo:     func(string) bool { return true }, // matches
			Normalize: func(c []byte) ([]byte, bool, string) {
				return c, false, "" // but never changes
			},
		},
	}
	out, notes := ApplyMirrorNormalizers("swarms/x.md", []byte("unchanged"))
	if string(out) != "unchanged" {
		t.Errorf("no-change normalizer must return content untouched")
	}
	if len(notes) != 0 {
		t.Errorf("a matching-but-no-change normalizer must emit no note, got %+v", notes)
	}
}

// TestApplyMirrorNormalizers_SkipsNonMatchingPath asserts a normalizer whose
// AppliesTo is false is never invoked.
func TestApplyMirrorNormalizers_SkipsNonMatchingPath(t *testing.T) {
	restore := registered
	t.Cleanup(func() { registered = restore })
	invoked := false
	registered = []Normalizer{
		{
			Name:          "path-gated-probe",
			Justification: "test",
			Risk:          MirrorSafe,
			AppliesTo:     func(rel string) bool { return rel == "swarms/x.md" },
			Normalize: func(c []byte) ([]byte, bool, string) {
				invoked = true
				return append(c, '!'), true, "fired"
			},
		},
	}
	out, notes := ApplyMirrorNormalizers("config.yaml", []byte("data"))
	if invoked {
		t.Errorf("Normalize must not run when AppliesTo is false")
	}
	if string(out) != "data" || len(notes) != 0 {
		t.Errorf("non-matching path must be a pure pass-through, got out=%q notes=%+v", out, notes)
	}
}

// TestRegistry_JustificationPresent is the registry lint (A7): every registered
// normalizer must carry a non-empty Justification. Presence only — correctness
// is the human code-review gate.
func TestRegistry_JustificationPresent(t *testing.T) {
	for _, n := range registered {
		if n.Name == "" {
			t.Errorf("a registered normalizer has an empty Name")
		}
		if n.Justification == "" {
			t.Errorf("normalizer %q has an empty Justification (required, §3.2)", n.Name)
		}
		if n.AppliesTo == nil || n.Normalize == nil {
			t.Errorf("normalizer %q has a nil AppliesTo/Normalize func", n.Name)
		}
	}
}

// TestAgentImageQualify_RegisteredMirrorSafe pins the shipped normalizer's risk
// class + that it is actually in the default registry.
func TestAgentImageQualify_RegisteredMirrorSafe(t *testing.T) {
	n := agentImageNormalizer(t)
	if n.Risk != MirrorSafe {
		t.Errorf("agent-image-qualify must be MirrorSafe, got %v", n.Risk)
	}
}
