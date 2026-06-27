package verifier

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// artifactClass helpers to avoid importing persistence constants in tests.
const artifactClassOutput = persistence.ArtifactClassOutput

// makeResumeDir creates a temp dir and writes a RESUME.md file inside it.
// Returns the dir path and a cleanup func.
func makeResumeDir(t *testing.T, resumeContent string) string {
	t.Helper()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "RESUME.md"), []byte(resumeContent), 0600)
	require.NoError(t, err)
	return dir
}

// TestCVClaimsGrounded_ResumeFile_FlagsFabricatedEmployer — grounding check
// reads the résumé from a workspace file and flags an employer name that
// doesn't appear in it.
func TestCVClaimsGrounded_ResumeFile_FlagsFabricatedEmployer(t *testing.T) {
	resume := "# Résumé\n## Experience\nGlobex Linux GmbH — Senior Engineer 2020–2026\n"
	projectDir := makeResumeDir(t, resume)

	// CV claims "Globex Corp" which is NOT in the resume.
	cvText := "I worked at Globex Corp as a Senior Engineer for five years.\n"
	withBodyReader(t, func(_ *persistence.Artifact) ([]byte, error) {
		return []byte(cvText), nil
	})

	cfg := Config{
		Name: "cv_check",
		Type: "cv_claims_grounded",
		Params: map[string]any{
			"resume_file":      "RESUME.md",
			"artifact_pattern": "cv-*.md",
		},
	}
	in := Input{
		ProjectDir: projectDir,
		Artifacts: []*persistence.Artifact{
			{Name: "cv-draft.md", ArtifactClass: artifactClassOutput},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "fabricated employer must trigger a violation")
	assert.Equal(t, "cv_check", v.VerifierName)
	assert.Contains(t, v.Detail, "Globex Corp")
}

// TestCVClaimsGrounded_ResumeFile_PassesWhenEmployerGrounded — when the CV
// names an employer that IS in the résumé file, no violation is raised.
func TestCVClaimsGrounded_ResumeFile_PassesWhenEmployerGrounded(t *testing.T) {
	resume := "# Résumé\n## Experience\nGlobex Linux GmbH — Senior Engineer 2020–2026\n"
	projectDir := makeResumeDir(t, resume)

	// Use a period after the company name so the org-name regex can't bleed
	// into the following capital "Led" on the next sentence.
	cvText := "Platform lead at Globex Linux GmbH. Led the infrastructure team.\n"
	withBodyReader(t, func(_ *persistence.Artifact) ([]byte, error) {
		return []byte(cvText), nil
	})

	cfg := Config{
		Type: "cv_claims_grounded",
		Params: map[string]any{
			"resume_file":      "RESUME.md",
			"artifact_pattern": "cv-*.md",
		},
	}
	in := Input{
		ProjectDir: projectDir,
		Artifacts: []*persistence.Artifact{
			{Name: "cv-draft.md", ArtifactClass: artifactClassOutput},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "grounded employer must not trigger a violation")
}

// TestCVClaimsGrounded_InlineResumeTakesPrecedence — inline params["resume"]
// wins over resume_file when both are set (backward-compat precedence).
func TestCVClaimsGrounded_InlineResumeTakesPrecedence(t *testing.T) {
	// resume_file points at a file with "Globex Linux GmbH".
	// params["resume"] (inline) says "Acme Corp" — only that is used.
	// CV says "Globex Linux GmbH" which is not in the inline text →
	// violation should fire (inline takes over, file is ignored).
	resume := "# Résumé\n## Experience\nGlobex Linux GmbH — Senior Engineer\n"
	projectDir := makeResumeDir(t, resume)

	cvText := "I led the team at Globex Linux GmbH for three years.\n"
	withBodyReader(t, func(_ *persistence.Artifact) ([]byte, error) {
		return []byte(cvText), nil
	})

	cfg := Config{
		Type: "cv_claims_grounded",
		Params: map[string]any{
			// Inline resume: only mentions Acme Corp, not Globex.
			"resume":           "Acme Corp is my current employer.",
			"resume_file":      "RESUME.md",
			"artifact_pattern": "cv-*.md",
		},
	}
	in := Input{
		ProjectDir: projectDir,
		Artifacts: []*persistence.Artifact{
			{Name: "cv-draft.md", ArtifactClass: artifactClassOutput},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	// Globex Linux GmbH is in the file but NOT in the inline text, so the
	// verifier must flag it (inline took precedence over the file).
	require.NotNil(t, v, "inline resume takes precedence; Globex not in inline text must flag")
	assert.Contains(t, v.Detail, "Globex Linux GmbH")
}

// TestCVClaimsGrounded_ResumeFileEmptyProjectDir_Abstains — when
// resume_file is set but ProjectDir is empty, the verifier must abstain
// (return nil) rather than error or false-positive.
func TestCVClaimsGrounded_ResumeFileEmptyProjectDir_Abstains(t *testing.T) {
	cvText := "I worked at Globex Corp.\n"
	withBodyReader(t, func(_ *persistence.Artifact) ([]byte, error) {
		return []byte(cvText), nil
	})

	cfg := Config{
		Type: "cv_claims_grounded",
		Params: map[string]any{
			"resume_file":      "RESUME.md",
			"artifact_pattern": "cv-*.md",
		},
	}
	in := Input{
		ProjectDir: "", // deliberately empty
		Artifacts: []*persistence.Artifact{
			{Name: "cv-draft.md", ArtifactClass: artifactClassOutput},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "empty ProjectDir with resume_file must abstain")
}

// TestCVClaimsGrounded_PathTraversalRejected_Abstains — a resume_file of
// "../../etc/passwd" must be rejected by the path-traversal guard and the
// verifier must abstain (return nil), not read the file or error.
func TestCVClaimsGrounded_PathTraversalRejected_Abstains(t *testing.T) {
	projectDir := t.TempDir()

	cvText := "I worked at Globex Corp.\n"
	withBodyReader(t, func(_ *persistence.Artifact) ([]byte, error) {
		return []byte(cvText), nil
	})

	cfg := Config{
		Type: "cv_claims_grounded",
		Params: map[string]any{
			"resume_file":      "../../etc/passwd",
			"artifact_pattern": "cv-*.md",
		},
	}
	in := Input{
		ProjectDir: projectDir,
		Artifacts: []*persistence.Artifact{
			{Name: "cv-draft.md", ArtifactClass: artifactClassOutput},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "path-traversal resume_file must be rejected; verifier must abstain")
}

// TestCVClaimsGrounded_MissingResumeFile_Abstains — when resume_file is
// set to a non-existent path inside ProjectDir, the verifier must abstain
// (nil) without failing the step.
func TestCVClaimsGrounded_MissingResumeFile_Abstains(t *testing.T) {
	projectDir := t.TempDir() // no RESUME.md written here

	cvText := "I worked at Globex Corp.\n"
	withBodyReader(t, func(_ *persistence.Artifact) ([]byte, error) {
		return []byte(cvText), nil
	})

	cfg := Config{
		Type: "cv_claims_grounded",
		Params: map[string]any{
			"resume_file":      "RESUME.md",
			"artifact_pattern": "cv-*.md",
		},
	}
	in := Input{
		ProjectDir: projectDir,
		Artifacts: []*persistence.Artifact{
			{Name: "cv-draft.md", ArtifactClass: artifactClassOutput},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "missing resume_file must abstain, not fail the step")
}

// TestResolveResumeFromFile_Unit — unit coverage for the safe-join helper.
func TestResolveResumeFromFile_Unit(t *testing.T) {
	dir := t.TempDir()
	content := "Canonical résumé content\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "resume.md"), []byte(content), 0600))

	t.Run("happy path", func(t *testing.T) {
		got := resolveResumeFromFile(map[string]any{"resume_file": "resume.md"}, dir)
		assert.Equal(t, content, got)
	})

	t.Run("empty resume_file", func(t *testing.T) {
		got := resolveResumeFromFile(map[string]any{"resume_file": ""}, dir)
		assert.Equal(t, "", got)
	})

	t.Run("empty projectDir", func(t *testing.T) {
		got := resolveResumeFromFile(map[string]any{"resume_file": "resume.md"}, "")
		assert.Equal(t, "", got)
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		got := resolveResumeFromFile(map[string]any{"resume_file": "../../etc/hosts"}, dir)
		assert.Equal(t, "", got)
	})

	t.Run("missing file returns empty", func(t *testing.T) {
		got := resolveResumeFromFile(map[string]any{"resume_file": "nonexistent.md"}, dir)
		assert.Equal(t, "", got)
	})

	t.Run("nil params returns empty", func(t *testing.T) {
		got := resolveResumeFromFile(nil, dir)
		assert.Equal(t, "", got)
	})
}
