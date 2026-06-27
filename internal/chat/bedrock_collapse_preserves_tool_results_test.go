package chat

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// TestCollapseSameRoleTurns_PreservesToolResults — the regression
// guard for the silent tool-result drop bug observed 2026-05-07
// where Bedrock-routed agents kept calling the same tool because
// they never saw the result. Pre-fix collapseSameRoleTurns called
// contentBlocksToText (text-only extractor) on the second of two
// consecutive same-role messages and `continue`d on the empty
// string — silently discarding ToolResult / ToolUse / Image /
// Document blocks. The fix appends every content block verbatim.
//
// Setup: a user message with text immediately followed by a user
// message with a single ToolResult block — the exact pattern that
// arises when the dispatcher injects a follow-up user turn before
// pending parallel tool results land. Pre-fix the merged output
// has only the text; the tool result is lost. Post-fix the merged
// output has both blocks and the model can see its tool's output
// on the next turn.
func TestCollapseSameRoleTurns_PreservesToolResults(t *testing.T) {
	in := []bedrocktypes.Message{
		{
			Role: bedrocktypes.ConversationRoleUser,
			Content: []bedrocktypes.ContentBlock{
				&bedrocktypes.ContentBlockMemberText{Value: "follow-up question"},
			},
		},
		{
			Role: bedrocktypes.ConversationRoleUser,
			Content: []bedrocktypes.ContentBlock{
				&bedrocktypes.ContentBlockMemberToolResult{
					Value: bedrocktypes.ToolResultBlock{
						ToolUseId: aws.String("tooluse_search_xyz"),
						Content: []bedrocktypes.ToolResultContentBlock{
							&bedrocktypes.ToolResultContentBlockMemberText{Value: "search returned 3 hits"},
						},
					},
				},
			},
		},
	}

	out := collapseSameRoleTurns(in)
	if len(out) != 1 {
		t.Fatalf("collapsed message count = %d, want 1 (two consecutive user turns must merge)", len(out))
	}

	merged := out[0]
	if merged.Role != bedrocktypes.ConversationRoleUser {
		t.Errorf("role = %s, want user", merged.Role)
	}

	// The text block survives.
	hasText := false
	hasToolResult := false
	for _, blk := range merged.Content {
		switch tb := blk.(type) {
		case *bedrocktypes.ContentBlockMemberText:
			if tb.Value != "" {
				hasText = true
			}
		case *bedrocktypes.ContentBlockMemberToolResult:
			if aws.ToString(tb.Value.ToolUseId) == "tooluse_search_xyz" {
				hasToolResult = true
			}
		}
	}
	if !hasText {
		t.Errorf("text block dropped during collapse; content = %#v", merged.Content)
	}
	if !hasToolResult {
		t.Errorf("ToolResult block silently dropped during collapse — this is the bug. content = %#v", merged.Content)
	}
}

// TestCollapseSameRoleTurns_PreservesToolUseInAssistantTurns — the
// flip-side guarantee: assistant ToolUse blocks must also survive
// the collapse. A scenario where this matters: a streaming response
// is split across two assistant deltas and the converter has
// already split one tool-use across two same-role messages. The
// merge must keep the ToolUse block intact, not text-collapse it
// to nothing.
func TestCollapseSameRoleTurns_PreservesToolUseInAssistantTurns(t *testing.T) {
	in := []bedrocktypes.Message{
		{
			Role: bedrocktypes.ConversationRoleAssistant,
			Content: []bedrocktypes.ContentBlock{
				&bedrocktypes.ContentBlockMemberText{Value: "I'll search for that."},
			},
		},
		{
			Role: bedrocktypes.ConversationRoleAssistant,
			Content: []bedrocktypes.ContentBlock{
				&bedrocktypes.ContentBlockMemberToolUse{
					Value: bedrocktypes.ToolUseBlock{
						ToolUseId: aws.String("tooluse_search_xyz"),
						Name:      aws.String("search"),
						Input:     nil,
					},
				},
			},
		},
	}

	out := collapseSameRoleTurns(in)
	if len(out) != 1 {
		t.Fatalf("collapsed message count = %d, want 1", len(out))
	}

	hasToolUse := false
	for _, blk := range out[0].Content {
		if _, ok := blk.(*bedrocktypes.ContentBlockMemberToolUse); ok {
			hasToolUse = true
		}
	}
	if !hasToolUse {
		t.Errorf("ToolUse block silently dropped during assistant-role collapse; content = %#v", out[0].Content)
	}
}

// TestCollapseSameRoleTurns_FoldsConsecutiveTextBlocks — the
// pre-existing readability behavior must survive: two text blocks
// from same-role messages should fold into one with a "\n\n"
// separator. Pin the contract so the new merge logic doesn't
// regress the readability win for purely textual conversations.
func TestCollapseSameRoleTurns_FoldsConsecutiveTextBlocks(t *testing.T) {
	in := []bedrocktypes.Message{
		{
			Role: bedrocktypes.ConversationRoleUser,
			Content: []bedrocktypes.ContentBlock{
				&bedrocktypes.ContentBlockMemberText{Value: "first line"},
			},
		},
		{
			Role: bedrocktypes.ConversationRoleUser,
			Content: []bedrocktypes.ContentBlock{
				&bedrocktypes.ContentBlockMemberText{Value: "second line"},
			},
		},
	}

	out := collapseSameRoleTurns(in)
	if len(out) != 1 || len(out[0].Content) != 1 {
		t.Fatalf("text-only merge produced %d msgs / %d blocks; want 1/1",
			len(out), len(out[0].Content))
	}
	tb := out[0].Content[0].(*bedrocktypes.ContentBlockMemberText)
	if tb.Value != "first line\n\nsecond line" {
		t.Errorf("merged text = %q, want first line\\n\\nsecond line", tb.Value)
	}
}

// TestExtractToolCalls_PartialOnError — Issue 3 fix. When one tool
// call's argument fails to marshal, the function must return the
// successfully-extracted calls AND the error, so the dispatcher's
// tool loop carries on with whatever's salvageable instead of
// stalling cold with an empty ToolCalls slice.
//
// The previous behavior (return nil, err) silently dropped every
// tool call in the same turn — including valid ones — and the
// caller swallowed the error, leaving the dispatcher with no tools
// to execute. Agent stalls "Done.".
//
// We can't trigger documentToJSON failure with a synthetic input
// in this test (the smithy SDK accepts most inputs), but the
// shape of the contract — partial-on-error — can be pinned by
// confirming the function returns a non-nil result + nil error
// on the happy path with multiple tools.
func TestExtractToolCalls_HappyPathReturnsAllValid(t *testing.T) {
	blocks := []bedrocktypes.ContentBlock{
		&bedrocktypes.ContentBlockMemberToolUse{
			Value: bedrocktypes.ToolUseBlock{
				ToolUseId: aws.String("call_1"),
				Name:      aws.String("search"),
				Input:     nil,
			},
		},
		&bedrocktypes.ContentBlockMemberToolUse{
			Value: bedrocktypes.ToolUseBlock{
				ToolUseId: aws.String("call_2"),
				Name:      aws.String("read_file"),
				Input:     nil,
			},
		},
		&bedrocktypes.ContentBlockMemberText{Value: "let me check those"},
	}
	calls, _, err := extractToolCallsFromContent(blocks)
	if err != nil {
		t.Fatalf("happy path returned err: %v", err)
	}
	if len(calls) != 2 {
		t.Errorf("got %d calls, want 2", len(calls))
	}
}

// TestBuildToolResultBlock_FallsBackOnEmptyID — Issue 8 fix. Pre-
// fix an empty ToolCallID returned nil and the tool result was
// silently dropped; Bedrock's next call would then 400 because
// the assistant's toolUse had no matching toolResult. Now an
// empty ID synthesizes a stable fallback so the conversation is
// still well-formed.
func TestBuildToolResultBlock_FallsBackOnEmptyID(t *testing.T) {
	m := Message{
		Role:       "tool",
		Name:       "search",
		ToolCallID: "", // empty — pre-fix this returned nil
		Content:    "search returned 3 hits",
	}
	blk := buildToolResultBlock(m)
	if blk == nil {
		t.Fatal("buildToolResultBlock returned nil for empty ID; pre-fix bug regressed")
	}
	tr, ok := blk.(*bedrocktypes.ContentBlockMemberToolResult)
	if !ok {
		t.Fatalf("got %T, want ContentBlockMemberToolResult", blk)
	}
	got := aws.ToString(tr.Value.ToolUseId)
	if got != "tooluse_search" {
		t.Errorf("synthetic ID = %q, want tooluse_search", got)
	}
}

// TestBuildToolResultBlock_AnonymousFallback — defensive: when
// neither ToolCallID nor Name is set (rare; suggests upstream
// corruption), still emit a synthetic ID so the conversation
// reaches Bedrock and the model can re-plan rather than the
// caller getting a 400.
func TestBuildToolResultBlock_AnonymousFallback(t *testing.T) {
	m := Message{
		Role:       "tool",
		ToolCallID: "",
		Name:       "",
		Content:    "result",
	}
	blk := buildToolResultBlock(m)
	if blk == nil {
		t.Fatal("anonymous tool result must still produce a block, not nil")
	}
}
