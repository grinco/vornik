package agentloop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func init() {
	Handlers["git_status"] = gitStatus
	Handlers["git_diff"] = gitDiff
	Handlers["git_log"] = gitLog
	Handlers["git_show"] = gitShow
}

// gitOutputCap is the 30 000 the bash `%.30000s` used. D4 (design §3.1):
// bash counted characters, this counts bytes on a rune boundary, and the
// trailer's total is bytes.
const gitOutputCap = 30000

// runGit runs git in repo and returns stdout and stderr separately.
func runGit(repo string, argv ...string) (stdout, stderr string, err error) {
	cmd := exec.Command("git", argv...)
	cmd.Dir = repo
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

// runGitMerged is `$( (cd repo && git …) 2>&1 || true )`: stdout and stderr
// interleaved into one stream, trailing newlines stripped by the capture.
func runGitMerged(repo string, argv ...string) string {
	cmd := exec.Command("git", argv...)
	cmd.Dir = repo
	var both bytes.Buffer
	cmd.Stdout, cmd.Stderr = &both, &both
	_ = cmd.Run()
	return strings.TrimRight(both.String(), "\n")
}

func capGitOutput(s string) string {
	if len(s) > gitOutputCap {
		return byteSliceOnRune(s, gitOutputCap) + fmt.Sprintf("\n\n[... truncated at 30KB, total %d bytes]", len(s))
	}
	return s
}

func repoPath(env Env, a args) (string, string) {
	// A revision is one argv element, but git still interprets a leading dash
	// as an option (audit 2026-09-05: --output wrote outside the workspace).
	if strings.HasPrefix(a.str("revision", ""), "-") {
		return "", "ERROR: revision must not start with '-'"
	}
	resolved, err := resolvePath(env.Workspace, a.str("path", "project"))
	if err != nil {
		return "", err.Error()
	}
	return resolved, ""
}

func gitStatus(env Env, raw json.RawMessage) string {
	a := decodeArgs(raw)
	repo, refusal := repoPath(env, a)
	if refusal != "" {
		return refusal
	}
	if !isDir(repo) {
		return "ERROR: not a git repository: " + repo
	}
	if _, _, err := runGit(repo, "rev-parse", "--git-dir"); err != nil {
		return "ERROR: not a git repository: " + repo
	}
	branch, _, _ := runGit(repo, "rev-parse", "--abbrev-ref", "HEAD")
	branch = strings.TrimSpace(branch)
	ahead, behind := 0, 0
	if ab, _, err := runGit(repo, "rev-list", "--left-right", "--count", "HEAD...@{u}"); err == nil {
		parts := strings.Fields(ab)
		if len(parts) == 2 {
			ahead, _ = strconv.Atoi(parts[0])
			behind, _ = strconv.Atoi(parts[1])
		}
	}
	porcelain, _, _ := runGit(repo, "status", "--porcelain=v1")
	files := []pyObject{}
	for _, line := range strings.Split(porcelain, "\n") {
		if len(line) < 3 {
			continue
		}
		files = append(files, pyObject{{"path", line[3:]}, {"status", line[:2]}})
	}
	return pyJSON(pyObject{{"branch", branch}, {"ahead", ahead}, {"behind", behind}, {"files", files}})
}

func gitDiff(env Env, raw json.RawMessage) string {
	a := decodeArgs(raw)
	repo, refusal := repoPath(env, a)
	if refusal != "" {
		return refusal
	}
	argv := []string{"diff"}
	if rev := a.str("revision", ""); rev != "" && rev != "null" {
		argv = append(argv, rev)
	} else if a.boolFlag("staged") {
		argv = append(argv, "--cached")
	}
	if paths := a.strList("paths"); len(paths) > 0 {
		argv = append(append(argv, "--"), paths...)
	}
	return capGitOutput(runGitMerged(repo, argv...))
}

func gitLog(env Env, raw json.RawMessage) string {
	a := decodeArgs(raw)
	repo, refusal := repoPath(env, a)
	if refusal != "" {
		return refusal
	}
	n := a.intOr("max", 20)
	if n < 1 {
		n = 1
	}
	if n > 200 {
		n = 200
	}
	argv := []string{"log", fmt.Sprintf("-%d", n), "--pretty=format:%H%x1f%h%x1f%an%x1f%aI%x1f%s"}
	if rev := a.str("revision", ""); rev != "" && rev != "null" {
		argv = append(argv, rev)
	}
	if paths := a.strList("paths"); len(paths) > 0 {
		argv = append(append(argv, "--"), paths...)
	}
	stdout, stderr, err := runGit(repo, argv...)
	if err != nil {
		return "ERROR: " + strings.TrimSpace(stderr)
	}
	commits := []pyObject{}
	for _, line := range strings.Split(stdout, "\n") {
		parts := strings.Split(line, "\x1f")
		if len(parts) != 5 {
			continue
		}
		commits = append(commits, pyObject{{"sha", parts[0]}, {"short_sha", parts[1]}, {"author", parts[2]}, {"date", parts[3]}, {"subject", parts[4]}})
	}
	return pyJSON(commits)
}

func gitShow(env Env, raw json.RawMessage) string {
	a := decodeArgs(raw)
	repo, refusal := repoPath(env, a)
	if refusal != "" {
		return refusal
	}
	argv := []string{"show", a.str("revision", "HEAD")}
	if paths := a.strList("paths"); len(paths) > 0 {
		argv = append(append(argv, "--"), paths...)
	}
	return capGitOutput(runGitMerged(repo, argv...))
}
