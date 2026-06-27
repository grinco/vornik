package executor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// planChangeSummary is the structured result of walking the commits a plan
// produced. It drives two outputs: a set of .patch files persisted as
// artifacts (one per commit) and a human-readable summary that replaces the
// last agent's raw JSON as the task's final message.
type planChangeSummary struct {
	FromSHA   string   // pre-plan HEAD (full sha)
	ToSHA     string   // post-plan HEAD (full sha)
	Patches   []string // absolute host paths to .patch files
	Commits   []string // one "short  subject" line per commit, newest first
	Summary   string   // human-readable markdown, suitable for UI + Telegram
	OutputDir string   // temp directory holding Patches and CHANGES.md
}

// generatePlanChanges runs `git format-patch` and `git log` against the
// worktree to build a mailbox-format patch set plus a summary of the work
// done during the plan. The caller is responsible for persisting the
// returned files through the artifact store and for removing outputDir
// when done. Returns a nil summary if there are no commits in range.
func generatePlanChanges(ctx context.Context, worktreeDir, fromSHA, toSHA string) (*planChangeSummary, error) {
	if worktreeDir == "" || fromSHA == "" || toSHA == "" || fromSHA == toSHA {
		return nil, nil
	}

	outputDir, err := os.MkdirTemp("", "vornik-plan-patches-*")
	if err != nil {
		return nil, fmt.Errorf("create patch output dir: %w", err)
	}

	// `format-patch` writes one .patch per commit in reverse-chronological
	// index order (0001, 0002, …). Output goes to our temp dir so the
	// worktree stays clean. --zero-commit and --no-signature keep the
	// patches reproducible across runs; callers who want From/signature
	// headers can reverse this later.
	out, err := exec.CommandContext(ctx,
		"git", "-C", worktreeDir,
		"format-patch",
		fromSHA+".."+toSHA,
		"-o", outputDir,
	).CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(outputDir)
		return nil, fmt.Errorf("git format-patch failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// git format-patch prints one line per patch file on stdout. Parse
	// that rather than re-scanning the directory — avoids picking up
	// any leftover files and preserves the order.
	var patches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// format-patch prints paths relative to cwd or absolute; normalise.
		if !filepath.IsAbs(line) {
			line = filepath.Join(worktreeDir, line)
		}
		patches = append(patches, line)
	}

	// Build the prose summary separately so the UI message is readable
	// without the user opening patch files.
	commits, summary, err := buildChangeSummary(ctx, worktreeDir, fromSHA, toSHA)
	if err != nil {
		_ = os.RemoveAll(outputDir)
		return nil, fmt.Errorf("build change summary: %w", err)
	}

	// Persist the summary next to the patches so it's part of the
	// artifact set. CHANGES.md is a convention the UI can render and
	// Telegram can forward as a document.
	summaryPath := filepath.Join(outputDir, "CHANGES.md")
	if err := os.WriteFile(summaryPath, []byte(summary), 0o644); err != nil {
		_ = os.RemoveAll(outputDir)
		return nil, fmt.Errorf("write summary: %w", err)
	}

	return &planChangeSummary{
		FromSHA:   fromSHA,
		ToSHA:     toSHA,
		Patches:   patches,
		Commits:   commits,
		Summary:   summary,
		OutputDir: outputDir,
	}, nil
}

// buildChangeSummary formats `git log <from>..<to>` into a markdown block
// plus a slice of "<short>  <subject>" lines suitable for log fields.
//
// The summary lives in whitespace-preserving UI panels and in Telegram
// messages, so it uses simple bullet prose rather than aggressive markdown
// that would render as literal syntax.
func buildChangeSummary(ctx context.Context, worktreeDir, fromSHA, toSHA string) ([]string, string, error) {
	// %h = short sha, %s = subject, %b = body. %x00 emits a literal NUL
	// byte in the *output*, which lets us split cleanly on a byte that
	// can't appear in commit messages. We do not embed NULs in the
	// format argument itself — exec.Command rejects NUL bytes in args.
	const formatSpec = "%h%n%s%n%b%x00"

	out, err := exec.CommandContext(ctx,
		"git", "-C", worktreeDir,
		"log",
		fromSHA+".."+toSHA,
		"--no-merges",
		"--reverse",
		"--format="+formatSpec,
	).Output()
	if err != nil {
		return nil, "", fmt.Errorf("git log: %w", err)
	}

	raw := strings.TrimRight(string(out), "\x00\n")
	if raw == "" {
		// No commits in range — empty summary signals "nothing committed".
		return nil, "", nil
	}

	var commitLines []string
	var b strings.Builder
	fmt.Fprintf(&b, "Changes from %s to %s\n\n", short(fromSHA), short(toSHA))

	for _, record := range strings.Split(raw, "\x00") {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		lines := strings.SplitN(record, "\n", 3)
		if len(lines) < 2 {
			continue
		}
		shortHash := strings.TrimSpace(lines[0])
		subject := strings.TrimSpace(lines[1])
		body := ""
		if len(lines) == 3 {
			body = strings.TrimSpace(lines[2])
		}

		commitLines = append(commitLines, fmt.Sprintf("%s  %s", shortHash, subject))

		fmt.Fprintf(&b, "• %s (%s)\n", subject, shortHash)
		if body != "" {
			// Indent the body by two spaces so it visually hangs under
			// the bullet in plain-text renders.
			for _, bl := range strings.Split(body, "\n") {
				fmt.Fprintf(&b, "  %s\n", bl)
			}
		}
		b.WriteString("\n")
	}

	return commitLines, strings.TrimRight(b.String(), "\n") + "\n", nil
}

// short is defined in plan_step.go.
