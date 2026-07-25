package api

import (
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Regression: T-8f69 (task_20260725222814_dff3a694415c8f69, 2026-07-25).
//
// A companion-architectural-review delegation carried its design doc as an
// inputArtifact but did NOT set skip_auto_extract. REST auto-extract therefore
// ran at upload time and stamped context.inputExtractions, which makes
// executor.extractTaskInputArtifacts SKIP STAGING the raw file ("agent uses
// document_* tools instead"). The reviewer role's allowedTools is builtins-only
// — no document_* — so the fallback that branch assumes returned
// 403 FORBIDDEN 17 times. The agent found an empty artifacts/in/, fell back to
// memory_search (76 calls), and wrote a review from 6 partial RAG chunks —
// exactly what the workflow prompt forbids. 107 tool calls, $1.43, 7 containers.
//
// Same class as B-10 (task_20260528134611) and the 2026-06-28 / 2026-07-03
// "document tools 403 + no staged file" reviews. Client-side guidance had told
// callers to set the flag only for companion-rag-ingest, so every other
// artifact-bearing workflow reproduced it. The flag is now derived from the
// workflow definition server-side and cannot be forgotten by a caller.
func TestResolveSkipAutoExtract(t *testing.T) {
	artifactWF := &registry.Workflow{RequireInputArtifacts: true}
	plainWF := &registry.Workflow{}

	tests := []struct {
		name      string
		requested bool
		wf        *registry.Workflow
		want      bool
		why       string
	}{
		{
			name:      "artifact-ingesting workflow forces skip even when caller omits the flag",
			requested: false,
			wf:        artifactWF,
			want:      true,
			why: "the T-8f69 case: require_input_artifacts means the workflow reads the " +
				"STAGED file, so auto-extract must not suppress staging",
		},
		{
			name:      "artifact-ingesting workflow keeps skip when caller asks for it",
			requested: true,
			wf:        artifactWF,
			want:      true,
			why:       "explicit request agrees with the derived value",
		},
		{
			name:      "ordinary workflow preserves the caller's opt-in",
			requested: true,
			wf:        plainWF,
			want:      true,
			why:       "callers may still stage raw files for a workflow that doesn't declare the flag",
		},
		{
			name:      "ordinary workflow keeps auto-extract by default",
			requested: false,
			wf:        plainWF,
			want:      false,
			why:       "preserves the Telegram/email 'just index it' upload shape",
		},
		{
			name:      "unknown workflow keeps auto-extract by default",
			requested: false,
			wf:        nil,
			want:      false,
			why:       "ad-hoc/unknown workflow IDs must not change behaviour (nil-registry parity)",
		},
		{
			name:      "unknown workflow preserves the caller's opt-in",
			requested: true,
			wf:        nil,
			want:      true,
			why:       "a nil lookup must never downgrade an explicit skip_auto_extract=true",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveSkipAutoExtract(tc.requested, tc.wf)
			if got != tc.want {
				t.Fatalf("resolveSkipAutoExtract(%v, %+v) = %v, want %v — %s",
					tc.requested, tc.wf, got, tc.want, tc.why)
			}
		})
	}
}
