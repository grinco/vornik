package dispatcher

import (
	"context"
	"strings"
)

// resolveInputFileSource translates an `input_files` entry from
// create_task into a real host path before it reaches the artifact
// snapshotter and the executor's container-staging guard. The LLM
// can pass two things in input_files today:
//
//  1. A host file path (Telegram /tmp upload, prior-task artifact
//     storage path) — pass through unchanged.
//  2. An artifact ID — typically surfaced via the email-channel
//     attachment-plumbing path ("artifact_id=email-att-…" in the
//     enriched user message). Resolve to the artifact's StoragePath
//     so the snapshotter reads the real bytes.
//
// Heuristic: a value with no path separator AND a successful
// artifactRepo.Get lookup is treated as an ID. Unknown IDs, repo
// errors, and nil-repo all fall through verbatim so container
// staging still rejects bad inputs with its existing diagnostics
// — we don't silently rewrite operator-supplied strings on
// transient failures.
//
// Origin: 2026-05-21 regression. The attachment-plumbing commit
// (0ef1aca) made the LLM see `artifact_id=email-att-2bd029f351c8e72b`
// in the enriched email and pass it through to create_task's
// input_files. The host-path-only assumption rejected the literal
// ID as "outside allowed roots" and the EPUB never reached the
// agent.
func (te *ToolExecutor) resolveInputFileSource(ctx context.Context, src string) string {
	if strings.ContainsAny(src, "/\\") {
		return src
	}
	if te == nil || te.artifactRepo == nil {
		return src
	}
	art, err := te.artifactRepo.Get(ctx, src)
	if err != nil || art == nil || art.StoragePath == "" {
		return src
	}
	return art.StoragePath
}
