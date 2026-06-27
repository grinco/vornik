package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
)

// gitPushToOrigin pushes sha to refs/heads/branch on the local clone's `origin`
// remote (already pointing at the correct GitHub URL — the daemon cloned it).
//
// The installation token is handed to git via GIT_CONFIG_* environment
// variables that set an `http.extraheader` Authorization header, NOT via the
// command line and NOT embedded in the remote URL — so the token never appears
// in process argv (visible to `ps`) nor in on-disk git config. GitHub accepts
// `Authorization: Basic base64("x-access-token:<token>")` for App tokens.
//
// The push is NON-FORCE: an already-up-to-date ref is a no-op success
// (idempotent re-run), and a divergent ref is rejected by git rather than
// force-overwritten — exactly the ForgeProvider.PushBranch contract.
func gitPushToOrigin(ctx context.Context, gitDir, branch, sha, token string) error {
	if strings.TrimSpace(gitDir) == "" {
		return fmt.Errorf("forge/github: push: empty gitDir")
	}
	if strings.TrimSpace(sha) == "" || strings.TrimSpace(branch) == "" {
		return fmt.Errorf("forge/github: push: empty branch or sha")
	}
	refspec := fmt.Sprintf("%s:refs/heads/%s", sha, branch)

	authHeader := "Authorization: Basic " +
		base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))

	cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "push", "origin", refspec)
	// GIT_CONFIG_COUNT/KEY/VALUE inject config without touching argv or disk.
	cmd.Env = append(cmd.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0="+authHeader,
		"GIT_TERMINAL_PROMPT=0", // never block on an interactive credential prompt
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// git's stderr names the failure (non-fast-forward, auth, etc.); surface
		// it but keep the token (only in env) out of the message.
		return fmt.Errorf("forge/github: git push %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	return nil
}
