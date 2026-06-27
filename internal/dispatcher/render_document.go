package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/safepath"
)

// renderDocumentArgs is the parsed shape of the render_document tool
// arguments. See the tool descriptor for the documented fields.
type renderDocumentArgs struct {
	Content string   `json:"content"`
	Name    string   `json:"name"`
	Formats []string `json:"formats"`
}

// renderDocument writes markdown content + converted forms (HTML / PDF)
// and delivers each file directly to the chat via FileSender. No
// LLM, no agent container, no task — the conversion is a one-shot
// shell call to pandoc / weasyprint. The dispatcher's prompt
// instructs the LLM to prefer this tool over create_task whenever
// the user supplies the content themselves and just wants the
// formats rendered.
//
// Why this exists: pre-2026-05-18 the bot's CV renders went through
// create_task → adaptive workflow → research agent → writer agent.
// Two LLMs scoping work whose deterministic transform is one
// pandoc invocation. The agents routinely hallucinated their way
// through "writer didn't produce file" loops; the user got nothing.
// render_document is the deterministic escape hatch.
//
// Formats vocabulary:
//   - "md"   — writes content verbatim to <name>.md and delivers it
//   - "html" — pandoc <name>.md -o <name>.html
//   - "pdf"  — pandoc <name>.md --pdf-engine=weasyprint -o <name>.pdf
//
// On exec failure (pandoc not installed, conversion crash) the tool
// reports the failure plainly; the dispatcher's prompt forbids
// inline-rendering as a fallback.
// renderRequestedFormats renders the requested non-md formats from the source
// markdown, returning the produced file paths (md source first, always) and a
// per-format failure list. Extracted from renderDocument to keep it under the
// complexity ratchet.
func renderRequestedFormats(ctx context.Context, tmpDir, mdPath, safeName, content string, wantSet map[string]bool) (produced, failures []string) {
	produced = []string{mdPath} // md is the source — always available.
	if wantSet["html"] {
		htmlPath := filepath.Join(tmpDir, safeName+".html")
		if err := renderMarkdownToHTML(ctx, mdPath, htmlPath, safeName, content); err != nil {
			failures = append(failures, fmt.Sprintf("html: %v", err))
		} else {
			produced = append(produced, htmlPath)
		}
	}
	if wantSet["pdf"] {
		pdfPath := filepath.Join(tmpDir, safeName+".pdf")
		if err := renderMarkdownToPDF(ctx, mdPath, pdfPath); err != nil {
			failures = append(failures, fmt.Sprintf("pdf: %v", err))
		} else {
			produced = append(produced, pdfPath)
		}
	}
	return produced, failures
}

// deliverRenderedFiles streams each produced file to the operator via the
// FileSender, returning the basenames delivered and per-file send/open errors.
// Extracted from renderDocument to keep it under the complexity ratchet.
func deliverRenderedFiles(ctx context.Context, fs FileSender, produced []string) (delivered, sendErrs []string) {
	for _, p := range produced {
		base := filepath.Base(p)
		f, oerr := os.Open(p)
		if oerr != nil {
			sendErrs = append(sendErrs, fmt.Sprintf("%s: %v", base, oerr))
			continue
		}
		err := fs.SendArtifactFile(ctx, base, f, "Rendered "+base)
		_ = f.Close()
		if err != nil {
			sendErrs = append(sendErrs, fmt.Sprintf("%s: %v", base, err))
			continue
		}
		delivered = append(delivered, base)
	}
	return delivered, sendErrs
}

func (te *ToolExecutor) renderDocument(ctx context.Context, argsJSON string, fs FileSender) ToolResult {
	var args renderDocumentArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	if strings.TrimSpace(args.Content) == "" {
		return ToolResult{Content: "render_document: content is required (the markdown source to render)."}
	}
	if strings.TrimSpace(args.Name) == "" {
		return ToolResult{Content: "render_document: name is required (the base filename, no extension)."}
	}
	if len(args.Formats) == 0 {
		args.Formats = []string{"md", "html", "pdf"}
	}
	safeName, err := safepath.CleanFileName(args.Name)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("render_document: invalid name: %v", err)}
	}
	// Strip any extension the caller accidentally included.
	safeName = strings.TrimSuffix(safeName, filepath.Ext(safeName))
	if safeName == "" {
		return ToolResult{Content: "render_document: name is required (the base filename, no extension)."}
	}
	if fs == nil {
		return ToolResult{Content: "render_document: file sending is not configured."}
	}

	tmpDir, err := os.MkdirTemp("", "vornik-render-*")
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("render_document: tmpdir: %v", err)}
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	mdPath := filepath.Join(tmpDir, safeName+".md")
	if err := os.WriteFile(mdPath, []byte(args.Content), 0o600); err != nil {
		return ToolResult{Content: fmt.Sprintf("render_document: write md: %v", err)}
	}

	wantSet := map[string]bool{}
	for _, f := range args.Formats {
		wantSet[strings.ToLower(strings.TrimSpace(f))] = true
	}

	produced, failures := renderRequestedFormats(ctx, tmpDir, mdPath, safeName, args.Content, wantSet)

	// If the caller asked for md ONLY, produced is [mdPath] and we
	// deliver it. If md wasn't requested but other formats were, drop
	// the md from delivery.
	if !wantSet["md"] {
		produced = produced[1:]
	}

	if len(produced) == 0 {
		if len(failures) > 0 {
			return ToolResult{Content: fmt.Sprintf("render_document: every requested format failed: %s", strings.Join(failures, "; "))}
		}
		return ToolResult{Content: "render_document: no formats requested."}
	}

	delivered, sendErrs := deliverRenderedFiles(ctx, fs, produced)

	switch {
	case len(delivered) > 0 && len(sendErrs) == 0 && len(failures) == 0:
		return ToolResult{Content: fmt.Sprintf("Delivered: %s", strings.Join(delivered, ", ")), Provenance: outputguard.ProvenanceFirstParty}
	case len(delivered) > 0 && len(sendErrs) == 0:
		return ToolResult{Content: fmt.Sprintf("Delivered: %s. Some formats failed to render: %s", strings.Join(delivered, ", "), strings.Join(failures, "; ")), Provenance: outputguard.ProvenanceFirstParty}
	case len(delivered) > 0:
		return ToolResult{Content: fmt.Sprintf("Delivered: %s. Delivery errors: %s", strings.Join(delivered, ", "), strings.Join(sendErrs, "; ")), Provenance: outputguard.ProvenanceFirstParty}
	default:
		return ToolResult{Content: fmt.Sprintf("render_document: nothing delivered. render failures: %s; send errors: %s", strings.Join(failures, "; "), strings.Join(sendErrs, "; ")), Provenance: outputguard.ProvenanceFirstParty}
	}
}

// renderMarkdownToHTML produces a minimal stand-alone HTML file.
// Three-tier fallback:
//  1. Host pandoc (fastest, no container overhead)
//  2. Pandoc inside the vornik-agent container image (works on
//     immutable hosts where pandoc isn't installed system-wide)
//  3. In-process <pre>-wrapped HTML (last-resort, unstyled)
func renderMarkdownToHTML(ctx context.Context, mdPath, htmlPath, title, rawMarkdown string) error {
	if _, err := exec.LookPath("pandoc"); err == nil {
		cmd := exec.CommandContext(ctx, "pandoc",
			mdPath,
			"--standalone",
			"--metadata", "title="+title,
			"-o", htmlPath,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("pandoc: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := runPandocViaPodman(ctx, mdPath, htmlPath,
		[]string{"--standalone", "--metadata", "title=" + title},
	); err == nil {
		return nil
	} else if !isPodmanUnavailable(err) {
		// Container ran but pandoc failed inside — surface that
		// failure verbatim so operators see the real reason.
		return err
	}
	// Last resort: wrap the markdown in a <pre> block. Readable but
	// unstyled. Operators who care about quality should keep
	// vornik-agent:latest pulled or install pandoc on the host.
	body := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>%s</title>
<style>body{font-family:sans-serif;max-width:48em;margin:2em auto;padding:0 1em;line-height:1.5} pre{white-space:pre-wrap;font-family:inherit}</style>
</head>
<body><pre>%s</pre></body>
</html>
`, html.EscapeString(title), html.EscapeString(rawMarkdown))
	return os.WriteFile(htmlPath, []byte(body), 0o600)
}

// renderMarkdownToPDF produces a PDF via pandoc with weasyprint as
// the engine. Tries host pandoc first, then the vornik-agent
// container as fallback. PDF has NO in-process fallback — if
// neither path works the caller surfaces the failure plainly.
func renderMarkdownToPDF(ctx context.Context, mdPath, pdfPath string) error {
	hostPandoc, _ := exec.LookPath("pandoc")
	hostWeasy, _ := exec.LookPath("weasyprint")
	if hostPandoc != "" && hostWeasy != "" {
		cmd := exec.CommandContext(ctx, "pandoc",
			mdPath,
			"--pdf-engine=weasyprint",
			"-o", pdfPath,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("pandoc/weasyprint: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := runPandocViaPodman(ctx, mdPath, pdfPath,
		[]string{"--pdf-engine=weasyprint"},
	); err == nil {
		return nil
	} else {
		return err
	}
}

// runPandocViaPodman invokes pandoc inside the vornik-agent
// container image which ships both pandoc and weasyprint. Bind-
// mounts the tmpdir (where both input and output live), runs as
// the daemon's UID via --userns=keep-id so the output file is
// readable on the host without chown gymnastics.
//
// Returns errPodmanUnavailable when podman isn't on PATH or the
// vornik-agent image isn't present (callers can fall back). Any
// other error means the container ran but pandoc itself failed.
func runPandocViaPodman(ctx context.Context, inPath, outPath string, extraArgs []string) error {
	if _, err := exec.LookPath("podman"); err != nil {
		return errPodmanUnavailable
	}
	tmpDir := filepath.Dir(inPath)
	inName := filepath.Base(inPath)
	outName := filepath.Base(outPath)
	args := []string{
		"run", "--rm",
		"--userns=keep-id",
		"-v", tmpDir + ":/work:Z",
		"-w", "/work",
		"--entrypoint", "pandoc",
		"vornik-agent:latest",
		inName,
	}
	args = append(args, extraArgs...)
	args = append(args, "-o", outName)
	cmd := exec.CommandContext(ctx, "podman", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Distinguish "image missing" (still podman-unavailable for
		// our purposes — caller can choose a different fallback)
		// from real pandoc errors.
		body := strings.TrimSpace(string(out))
		if strings.Contains(body, "Error: short-name") ||
			strings.Contains(body, "no such image") ||
			strings.Contains(body, "image not known") {
			return errPodmanUnavailable
		}
		return fmt.Errorf("pandoc-in-podman: %v (%s)", err, body)
	}
	return nil
}

// errPodmanUnavailable signals "couldn't even invoke pandoc here";
// callers fall back to the next strategy. isPodmanUnavailable lets
// the call sites distinguish from real conversion errors.
var errPodmanUnavailable = fmt.Errorf("podman path unavailable for pandoc fallback")

func isPodmanUnavailable(err error) bool {
	return err == errPodmanUnavailable
}
