package podman

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The shipped role prompts make a PROMISE about the agent image. Examples:
//
//	configs/swarms/assistant-swarm.md:      "The agent image ships pandoc + weasyprint"
//	configs/project-templates/.../swarm.md.tmpl: "pdf via pandoc --pdf-engine=weasyprint"
//
// Nothing enforced that promise. An agent asked for a PDF runs
// `pandoc --pdf-engine=weasyprint` in run_shell, and if the toolchain were
// missing it would fail inside a container, mid-task, as a step failure with
// no operator-visible signal that the IMAGE was the problem — the same class
// of silent failure as the 2026-08-05 lost-deliverable incident.
//
// A base-image bump, an apt package rename, or a well-meaning
// "slim the image" edit is all it takes. These tests make the promise
// enforceable.

// pdfToolchainPackages are the apt packages the PDF path depends on.
//   - pandoc: the converter every shipped prompt names.
//   - weasyprint: the headless PDF engine (chosen over a LaTeX toolchain:
//     ~150MB instead of ~1.5GB).
//   - fonts-dejavu: without a font package weasyprint still exits 0 but
//     renders tofu boxes, so a "successful" PDF is unreadable. It is part of
//     the toolchain, not a nicety.
var pdfToolchainPackages = []string{"pandoc", "weasyprint", "fonts-dejavu"}

// TestAgentImageInstallsPDFToolchain pins the PDF toolchain into the
// distributed agent image.
func TestAgentImageInstallsPDFToolchain(t *testing.T) {
	cf := readCompose(t, filepath.Join("..", "..", "images", "vornik-agent", "Containerfile"))

	// Only consider apt-get install lines, so a mention in a comment cannot
	// satisfy the assertion — the comment block explaining WHY these exist
	// sits a few lines below the install and would otherwise mask a removal.
	installed := aptInstalledPackages(cf)
	for _, pkg := range pdfToolchainPackages {
		if !installed[pkg] {
			t.Errorf("agent Containerfile no longer apt-installs %q — the shipped role prompts "+
				"promise the image ships pandoc + weasyprint, and an agent asked for a PDF will "+
				"fail inside the container with no signal that the image is at fault", pkg)
		}
	}
}

// TestShippedPromptsPromiseMatchesImage — the prompts and the image must not
// drift apart in EITHER direction. If a prompt tells an agent to use a PDF
// engine, the image has to carry it.
func TestShippedPromptsPromiseMatchesImage(t *testing.T) {
	cf := readCompose(t, filepath.Join("..", "..", "images", "vornik-agent", "Containerfile"))
	installed := aptInstalledPackages(cf)

	// Engines a prompt might name → the apt package that provides them.
	engines := map[string]string{"weasyprint": "weasyprint", "pandoc": "pandoc"}

	roots := []string{
		filepath.Join("..", "..", "configs", "swarms"),
		filepath.Join("..", "..", "configs", "project-templates"),
		filepath.Join("..", "..", "configs", "role-library"),
	}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || (!strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".tmpl")) {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			body := string(b)
			for engine, pkg := range engines {
				if strings.Contains(body, engine) && !installed[pkg] {
					t.Errorf("%s instructs an agent to use %q but the agent image does not install %q",
						path, engine, pkg)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}

// aptInstalledPackages collects package names from the Containerfile's
// `apt-get install` invocations. Deliberately ignores comments so a removal
// cannot hide behind the prose that documents it.
func aptInstalledPackages(containerfile string) map[string]bool {
	out := map[string]bool{}
	lines := strings.Split(containerfile, "\n")
	inInstall := false
	flag := regexp.MustCompile(`^-`)
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "apt-get install") {
			inInstall = true
			// Trim everything up to and including "install".
			if i := strings.Index(line, "install"); i >= 0 {
				line = line[i+len("install"):]
			}
		} else if !inInstall {
			continue
		}
		continued := strings.HasSuffix(line, "\\")
		line = strings.TrimSuffix(line, "\\")
		for _, tok := range strings.Fields(line) {
			// Stop at a shell chain — the next command isn't apt any more.
			if tok == "&&" || tok == "||" || tok == ";" {
				continued = false
				break
			}
			if flag.MatchString(tok) {
				continue
			}
			out[tok] = true
		}
		if !continued {
			inInstall = false
		}
	}
	return out
}

// TestAgentImageRendersRealPDF is the substantive check: the binaries being
// present does not prove they WORK. weasyprint needs its cairo/pango stack and
// at least one font, and it can exit 0 while producing tofu.
//
// Opt-in, because it needs podman plus a built image:
//
//	VORNIK_IMAGE_SMOKE=1 go test ./deployments/podman/ -run RendersRealPDF
//
// Run it after any change to the agent Containerfile's base image or package
// set. The Czech text is deliberate — the 2026-08-05 report that motivated
// this was Czech, and diacritics are exactly what a missing font eats.
func TestAgentImageRendersRealPDF(t *testing.T) {
	if os.Getenv("VORNIK_IMAGE_SMOKE") == "" {
		t.Skip("set VORNIK_IMAGE_SMOKE=1 to run the agent-image PDF smoke test (needs podman + a built image)")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not on PATH")
	}
	image := os.Getenv("VORNIK_AGENT_IMAGE")
	if image == "" {
		image = "ghcr.io/grinco/vornik-agent:latest"
	}

	const script = `set -e
cd /tmp
printf '%s' '<html><body><h1>Ahoj</h1><p>příliš žluťoučký kůň</p></body></html>' > t.html
weasyprint t.html t.pdf
head -c 5 t.pdf
`
	out, err := exec.Command("podman", "run", "--rm", "--entrypoint", "sh", image, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("weasyprint render failed in %s: %v\n%s", image, err, out)
	}
	if !strings.Contains(string(out), "%PDF-") {
		t.Errorf("render produced no PDF magic bytes in %s; got:\n%s", image, out)
	}

	// pandoc's markdown→PDF path is the one the shipped prompts actually name,
	// so exercise it too rather than trusting weasyprint alone.
	const viaPandoc = `set -e
cd /tmp
printf '%s' '# Ahoj

příliš žluťoučký kůň' > t.md
pandoc t.md --pdf-engine=weasyprint -o p.pdf
head -c 5 p.pdf
`
	out, err = exec.Command("podman", "run", "--rm", "--entrypoint", "sh", image, "-c", viaPandoc).CombinedOutput()
	if err != nil {
		t.Fatalf("pandoc --pdf-engine=weasyprint failed in %s: %v\n%s", image, err, out)
	}
	if !strings.Contains(string(out), "%PDF-") {
		t.Errorf("pandoc PDF path produced no PDF magic bytes in %s; got:\n%s", image, out)
	}
}
