package cli

// Skill source resolver. Turns the operator-typed source argument
// into a fetched SWARM-SKILL.md byte stream — handling the four
// input shapes the CLI accepts:
//
//   - local file path           → os.ReadFile
//   - https:// URL              → HTTP GET
//   - git+https:// or .git URL  → git clone, find swarm-skill.md
//   - <handle>/<skill>[@<ver>]  → registry-index lookup, then git
//
// The resolver returns a SkillSourceResult that captures every
// piece of provenance the install path needs: the bytes
// themselves, the canonical source URL (recorded in the ledger),
// and the revision string (git tag/sha, or "https" / "file" for
// non-git sources).

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const maxResolvedSkillSourceBytes = 1 << 20

// SkillSourceResult bundles the resolver's outputs so each
// install / update path consumes one struct instead of a tuple
// the next refactor inevitably gets out of order.
type SkillSourceResult struct {
	// Bytes is the SWARM-SKILL.md content. Pass straight to
	// ParseSwarmSkill + ValidateSwarmSkillMarkdown.
	Bytes []byte
	// SourceURL is what gets recorded in the ledger so
	// `vornikctl skill update` knows where to re-fetch from.
	SourceURL string
	// Revision is the recorded git tag or commit sha (or the
	// literal strings "https"/"file"/"local" for non-git
	// sources). This is the human-facing ref the operator asked
	// for; ResolvedSHA below is the pin used for drift detection.
	Revision string
	// ResolvedSHA is the actual git commit the clone landed on
	// (rev-parse HEAD), ALWAYS resolved for git sources even when
	// the operator asked for a branch/tag — so a re-install can
	// detect that a mutable ref now points at different code
	// (supply-chain drift). Empty for non-git sources (drift there
	// falls back to the content checksum).
	ResolvedSHA string
	// SourceFilename is the basename of the file the bytes came
	// from. Used by the install path when the source repo
	// publishes several skills.
	SourceFilename string
}

// SkillSourceForm classifies the source string so callers can
// branch on the resolved form without reparsing.
type SkillSourceForm int

const (
	SourceFormLocal  SkillSourceForm = iota // file:// or a path that os.Stat finds
	SourceFormHTTPS                         // https:// URL
	SourceFormGit                           // git+https:// or .git suffix
	SourceFormHandle                        // <handle>/<skill>[@<version>]
)

// ClassifySkillSource picks the form a source string represents.
// The order of checks matters — local-file paths can look like
// handles ("foo/bar.md") if the operator forgets the leading
// "./", so we stat first.
func ClassifySkillSource(source string) (SkillSourceForm, error) {
	if source == "" {
		return 0, fmt.Errorf("source is empty")
	}
	switch {
	case strings.HasPrefix(source, "file://"):
		return SourceFormLocal, nil
	case strings.HasPrefix(source, "git+https://"), strings.HasPrefix(source, "git+ssh://"):
		return SourceFormGit, nil
	case strings.HasSuffix(source, ".git"):
		return SourceFormGit, nil
	case strings.HasPrefix(source, "https://"), strings.HasPrefix(source, "http://"):
		return SourceFormHTTPS, nil
	}
	// Disambiguate path vs handle: if the string looks like a
	// path (absolute, dot-prefix, or os.Stat hit) treat it as
	// local. Otherwise it's a handle.
	if filepath.IsAbs(source) || strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") {
		return SourceFormLocal, nil
	}
	if _, err := os.Stat(source); err == nil {
		return SourceFormLocal, nil
	}
	if strings.Count(source, "/") == 1 {
		return SourceFormHandle, nil
	}
	return 0, fmt.Errorf("could not classify source %q (try file://, https://, git+https://, or <handle>/<skill>)", source)
}

// ResolveSkillSource is the entry point: dispatch to the
// per-form resolver. The handle path needs an index client
// passed in so this file stays free of the index-fetch logic
// (kept in skill_index.go).
type SkillIndexResolver interface {
	ResolveHandle(handle, skill, version string) (gitURL, revision string, err error)
}

// ResolveSkillSource is the dispatcher.
func ResolveSkillSource(source string, idx SkillIndexResolver) (*SkillSourceResult, error) {
	form, err := ClassifySkillSource(source)
	if err != nil {
		return nil, err
	}
	switch form {
	case SourceFormLocal:
		return resolveLocalSource(source)
	case SourceFormHTTPS:
		return resolveHTTPSSource(source)
	case SourceFormGit:
		return resolveGitSource(source, "")
	case SourceFormHandle:
		if idx == nil {
			return nil, fmt.Errorf("handle source %q requires a registry index (set --registry or VORNIK_SKILL_REGISTRY_URL)", source)
		}
		handle, skill, version, err := parseSkillHandle(source)
		if err != nil {
			return nil, err
		}
		gitURL, rev, err := idx.ResolveHandle(handle, skill, version)
		if err != nil {
			return nil, err
		}
		return resolveGitSource(gitURL, rev)
	}
	return nil, fmt.Errorf("unsupported source form %d", form)
}

// parseSkillHandle splits "<handle>/<skill>" or
// "<handle>/<skill>@<version>" into the three components.
func parseSkillHandle(source string) (handle, skill, version string, err error) {
	rest := source
	if at := strings.LastIndex(rest, "@"); at > 0 && at < len(rest)-1 {
		version = rest[at+1:]
		rest = rest[:at]
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("handle %q must be <handle>/<skill>[@<version>]", source)
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), strings.TrimSpace(version), nil
}

func resolveLocalSource(source string) (*SkillSourceResult, error) {
	path := strings.TrimPrefix(source, "file://")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxResolvedSkillSourceBytes {
		return nil, fmt.Errorf("read %s: body exceeds %d bytes", path, maxResolvedSkillSourceBytes)
	}
	abs, _ := filepath.Abs(path)
	return &SkillSourceResult{
		Bytes:          data,
		SourceURL:      "file://" + abs,
		Revision:       "local",
		SourceFilename: filepath.Base(path),
	}, nil
}

func resolveHTTPSSource(source string) (*SkillSourceResult, error) {
	parsed, err := url.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", source, err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "vornikctl-skill/1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", source, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch %s: HTTP %d", source, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResolvedSkillSourceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) > maxResolvedSkillSourceBytes {
		return nil, fmt.Errorf("fetch %s: body exceeds %d bytes", source, maxResolvedSkillSourceBytes)
	}
	return &SkillSourceResult{
		Bytes:          body,
		SourceURL:      source,
		Revision:       "https",
		SourceFilename: filepath.Base(parsed.Path),
	}, nil
}

// resolveGitSource clones the repo to a temp dir, optionally
// checks out the requested revision, then locates a single
// `.swarm-skill.md` file (or one with `SKILL.md` at the root)
// to install. Multi-skill repos with a `vornik-skills.yaml`
// manifest are deferred — operators with a multi-skill repo
// install each one explicitly by file URL until manifest
// support lands.
func resolveGitSource(source, rev string) (*SkillSourceResult, error) {
	gitURL := strings.TrimPrefix(source, "git+")
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("git not found on PATH; install git or fetch the file via https://")
	}
	tmp, err := os.MkdirTemp("", "vornikctl-skill-clone-*")
	if err != nil {
		return nil, fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	args := []string{"clone", "--depth", "1"}
	if rev != "" {
		args = append(args, "--branch", rev)
	}
	args = append(args, gitURL, tmp)
	cmd := exec.Command("git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git clone %s: %w\n%s", gitURL, err, string(out))
	}

	path, err := findSkillFileInRepo(tmp)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cloned %s: %w", path, err)
	}
	if len(data) > maxResolvedSkillSourceBytes {
		return nil, fmt.Errorf("read cloned %s: body exceeds %d bytes", path, maxResolvedSkillSourceBytes)
	}
	// Always resolve the actual commit the clone landed on — even when a
	// branch/tag was requested — so re-installs can detect that a mutable
	// ref now points at different code (supply-chain drift, Option A).
	resolvedSHA := ""
	if out, err := exec.Command("git", "-C", tmp, "rev-parse", "HEAD").Output(); err == nil {
		resolvedSHA = strings.TrimSpace(string(out))
	}
	resolvedRev := rev
	if resolvedRev == "" {
		resolvedRev = resolvedSHA // no ref asked for → show the sha
	}
	if resolvedRev == "" {
		resolvedRev = "git"
	}
	return &SkillSourceResult{
		Bytes:          data,
		SourceURL:      gitURL,
		Revision:       resolvedRev,
		ResolvedSHA:    resolvedSHA,
		SourceFilename: filepath.Base(path),
	}, nil
}

// findSkillFileInRepo picks the SWARM-SKILL.md file from a
// freshly-cloned repo. Preference order: `SKILL.md` at the
// root → `*.swarm-skill.md` at the root (single match only;
// multi-match errors). Subdirectory search isn't done — we
// only support flat repos in this slice to keep the resolver
// honest. Multi-skill repos with `vornik-skills.yaml` are a
// follow-up.
func findSkillFileInRepo(dir string) (string, error) {
	if path := filepath.Join(dir, "SKILL.md"); statIsFile(path) {
		return path, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read repo: %w", err)
	}
	var matches []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(strings.ToLower(name), ".swarm-skill.md") {
			matches = append(matches, filepath.Join(dir, name))
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no SKILL.md or *.swarm-skill.md found at repo root; multi-skill repos with vornik-skills.yaml are not yet supported")
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("multiple skill files at repo root (%d); pick one and install via its file URL until manifest support lands", len(matches))
	}
}

func statIsFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}
