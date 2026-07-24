package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExampleParallelResearchWorkflow locks in the shipped configs/workflows/
// parallel-research.md example: it parses and passes Validate, exercising the
// full parallel contract end-to-end on a real file (mirrors
// `vornikctl workflow validate`).
func TestExampleParallelResearchWorkflow(t *testing.T) {
	root := repoRootFromRegistryTest(t)
	path := filepath.Join(root, "configs", "workflows", "parallel-research.md")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	wf, err := ParseWorkflowMarkdown(content, path)
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown: %v", err)
	}
	if err := wf.Validate(path); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	fan, ok := wf.Steps["research_fanout"]
	if !ok || fan.Type != "parallel" {
		t.Fatalf("research_fanout missing or not parallel: %+v", fan)
	}
	if fan.Join != "synthesize" || fan.JoinPolicy != "quorum:2" {
		t.Errorf("join/join_policy = %q/%q, want synthesize/quorum:2", fan.Join, fan.JoinPolicy)
	}
	if len(fan.Branches) != 3 {
		t.Errorf("branches = %d, want 3", len(fan.Branches))
	}
	if syn := wf.Steps["synthesize"]; !syn.StageChildArtifacts {
		t.Errorf("synthesize should declare stage_child_artifacts")
	}
}

// validParallelWorkflow builds a minimal, structurally-valid workflow with a
// single `parallel` fan-out step joining an agent synthesize step. Each case
// in the matrix below mutates it so the ONLY thing under test is the parallel
// contract.
func validParallelWorkflow() *Workflow {
	return &Workflow{
		ID:         "par-test",
		Entrypoint: "fanout",
		Steps: map[string]WorkflowStep{
			"fanout": {
				Type: "parallel",
				Join: "synthesize",
				Branches: []WorkflowBranch{
					{ID: "market", Role: "researcher", Prompt: "research market"},
					{ID: "tech", Role: "researcher", Prompt: "assess tech"},
					{ID: "legal", Role: "researcher", Prompt: "check legal"},
				},
				JoinPolicy: "all",
			},
			"synthesize": {
				Type:      "agent",
				Role:      "analyst",
				Prompt:    "synthesize the legs",
				OnSuccess: "complete",
			},
		},
		Terminals: map[string]WorkflowTerminal{
			"complete": {Status: "COMPLETED"},
		},
	}
}

func TestValidate_ParallelStepMatrix(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(w *Workflow)
		wantError bool
		errSubstr string
	}{
		{
			name:      "valid parallel + agent join",
			mutate:    func(*Workflow) {},
			wantError: false,
		},
		{
			name: "valid: empty join_policy defaults to all",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.JoinPolicy = ""
				w.Steps["fanout"] = s
			},
			wantError: false,
		},
		{
			name: "valid: quorum within range",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.JoinPolicy = "quorum:2"
				w.Steps["fanout"] = s
			},
			wantError: false,
		},
		{
			name: "valid: best_effort",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.JoinPolicy = "best_effort"
				w.Steps["fanout"] = s
			},
			wantError: false,
		},
		{
			name: "invalid: no branches",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.Branches = nil
				w.Steps["fanout"] = s
			},
			wantError: true,
			errSubstr: "at least one branch",
		},
		{
			name: "invalid: missing join",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.Join = ""
				w.Steps["fanout"] = s
			},
			wantError: true,
			errSubstr: "join is required",
		},
		{
			name: "invalid: self-join (join == the parallel step itself)",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.Join = "fanout"
				w.Steps["fanout"] = s
			},
			wantError: true,
			errSubstr: "must not be the parallel step itself",
		},
		{
			name: "invalid: join target not found",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.Join = "nope"
				w.Steps["fanout"] = s
			},
			wantError: true,
			errSubstr: "not found in steps",
		},
		{
			name: "invalid: quorum:0",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.JoinPolicy = "quorum:0"
				w.Steps["fanout"] = s
			},
			wantError: true,
			errSubstr: "quorum threshold must be ≥1",
		},
		{
			name: "invalid: quorum exceeds branch count",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.JoinPolicy = "quorum:4" // only 3 branches
				w.Steps["fanout"] = s
			},
			wantError: true,
			errSubstr: "exceeds branch count",
		},
		{
			name: "invalid: malformed join_policy",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.JoinPolicy = "sometimes"
				w.Steps["fanout"] = s
			},
			wantError: true,
			errSubstr: "not one of",
		},
		{
			name: "invalid: duplicate branch ids",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.Branches = []WorkflowBranch{
					{ID: "dup", Role: "r", Prompt: "p"},
					{ID: "dup", Role: "r", Prompt: "p"},
				}
				w.Steps["fanout"] = s
			},
			wantError: true,
			errSubstr: "duplicate branch id",
		},
		{
			name: "invalid: branch missing role",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.Branches = []WorkflowBranch{{ID: "x", Prompt: "p"}}
				w.Steps["fanout"] = s
			},
			wantError: true,
			errSubstr: "role is required",
		},
		{
			name: "invalid: branch missing prompt",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.Branches = []WorkflowBranch{{ID: "x", Role: "r"}}
				w.Steps["fanout"] = s
			},
			wantError: true,
			errSubstr: "prompt is required",
		},
		{
			name: "invalid: branch missing id",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.Branches = []WorkflowBranch{{Role: "r", Prompt: "p"}}
				w.Steps["fanout"] = s
			},
			wantError: true,
			errSubstr: "branch id is required",
		},
		{
			name: "invalid: a second parallel step (non-entrypoint) is rejected",
			mutate: func(w *Workflow) {
				// A second parallel step can only be non-entrypoint, so the
				// entrypoint rule rejects it — eliminating the old
				// join-target-is-parallel vector (LLD v0.6 §1, C2).
				w.Steps["tail"] = WorkflowStep{Type: "agent", Role: "analyst", Prompt: "x", OnSuccess: "complete"}
				w.Steps["synthesize"] = WorkflowStep{
					Type:     "parallel",
					Join:     "tail",
					Branches: []WorkflowBranch{{ID: "a", Role: "r", Prompt: "p"}},
				}
			},
			wantError: true,
			errSubstr: "must be the workflow entrypoint",
		},
		{
			name: "invalid: parallel sets on_success",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.OnSuccess = "synthesize"
				w.Steps["fanout"] = s
			},
			wantError: true,
			errSubstr: "must not set on_success",
		},
		{
			name: "invalid: parallel sets on_fail",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.OnFail = "synthesize"
				w.Steps["fanout"] = s
			},
			wantError: true,
			errSubstr: "must not set on_success",
		},
		{
			name: "invalid: parallel sets gates",
			mutate: func(w *Workflow) {
				s := w.Steps["fanout"]
				s.Gates = []WorkflowGate{{Condition: "x == 1", Target: "synthesize"}}
				w.Steps["fanout"] = s
			},
			wantError: true,
			errSubstr: "must not set on_success",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := validParallelWorkflow()
			tt.mutate(w)
			err := w.Validate(tt.name + ".md")
			if tt.wantError && err == nil {
				t.Fatalf("expected validation error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if tt.wantError && tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.errSubstr)
			}
		})
	}
}

// TestValidate_ParallelJoinReachability proves the reachability walker follows
// a parallel step's join edge: a join consumer reachable ONLY via the join
// edge is valid, and a genuinely orphaned step is still rejected.
func TestValidate_ParallelJoinReachability(t *testing.T) {
	t.Run("join reachable only via parallel edge is valid", func(t *testing.T) {
		// synthesize is not the target of any on_success/gate — its ONLY
		// inbound edge is fanout.join. Without join-following it would be
		// flagged unreachable.
		w := validParallelWorkflow()
		if err := w.Validate("reach.md"); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})

	t.Run("orphaned step is rejected as unreachable", func(t *testing.T) {
		w := validParallelWorkflow()
		w.Steps["orphan"] = WorkflowStep{Type: "agent", Role: "r", Prompt: "p", OnSuccess: "complete"}
		err := w.Validate("orphan.md")
		if err == nil || !strings.Contains(err.Error(), "not reachable") {
			t.Fatalf("expected unreachable error, got %v", err)
		}
	})
}

func mkBranches(n int) []WorkflowBranch {
	out := make([]WorkflowBranch, n)
	for i := 0; i < n; i++ {
		out[i] = WorkflowBranch{ID: string(rune('a'+i%26)) + string(rune('0'+i/26)), Role: "r", Prompt: "p"}
	}
	return out
}

// TestValidate_ParallelFanOutBranchCount rejects a single parallel entrypoint
// whose static branch count exceeds the fan-out limit, and accepts one at the
// limit.
func TestValidate_ParallelFanOutBranchCount(t *testing.T) {
	over := &Workflow{
		ID:         "over",
		Entrypoint: "p1",
		Steps: map[string]WorkflowStep{
			"p1":   {Type: "parallel", Join: "tail", Branches: mkBranches(parallelCumulativeFanOutLimit + 1)},
			"tail": {Type: "agent", Role: "r", Prompt: "x", OnSuccess: "complete"},
		},
		Terminals: map[string]WorkflowTerminal{"complete": {Status: "COMPLETED"}},
	}
	if err := over.Validate("over.md"); err == nil || !strings.Contains(err.Error(), "exceeding the static limit") {
		t.Fatalf("expected fan-out branch-count breach, got %v", err)
	}

	atLimit := &Workflow{
		ID:         "ok",
		Entrypoint: "p1",
		Steps: map[string]WorkflowStep{
			"p1":   {Type: "parallel", Join: "tail", Branches: mkBranches(parallelCumulativeFanOutLimit)},
			"tail": {Type: "agent", Role: "r", Prompt: "x", OnSuccess: "complete"},
		},
		Terminals: map[string]WorkflowTerminal{"complete": {Status: "COMPLETED"}},
	}
	if err := atLimit.Validate("ok.md"); err != nil {
		t.Fatalf("expected at-limit fan-out to pass, got %v", err)
	}
}

// TestValidate_ParallelMustBeEntrypoint locks in the v0.6 §1 rule: a parallel
// step is valid only as the workflow entrypoint. Two parallel steps (only one
// can be the entrypoint) are therefore always rejected (C2).
func TestValidate_ParallelMustBeEntrypoint(t *testing.T) {
	t.Run("parallel at entrypoint is accepted", func(t *testing.T) {
		if err := validParallelWorkflow().Validate("ep.md"); err != nil {
			t.Fatalf("expected accepted, got %v", err)
		}
	})
	t.Run("parallel not at entrypoint is rejected", func(t *testing.T) {
		// Entrypoint is an agent step; the parallel step sits downstream.
		w := &Workflow{
			ID:         "notep",
			Entrypoint: "intake",
			Steps: map[string]WorkflowStep{
				"intake":     {Type: "agent", Role: "r", Prompt: "x", OnSuccess: "fanout"},
				"fanout":     {Type: "parallel", Join: "synthesize", Branches: []WorkflowBranch{{ID: "a", Role: "r", Prompt: "p"}}},
				"synthesize": {Type: "agent", Role: "a", Prompt: "s", OnSuccess: "complete"},
			},
			Terminals: map[string]WorkflowTerminal{"complete": {Status: "COMPLETED"}},
		}
		err := w.Validate("notep.md")
		if err == nil || !strings.Contains(err.Error(), "must be the workflow entrypoint") {
			t.Fatalf("expected parallel-not-entrypoint rejection, got %v", err)
		}
	})
	t.Run("two parallel steps are rejected", func(t *testing.T) {
		w := &Workflow{
			ID:         "twopar",
			Entrypoint: "p1",
			Steps: map[string]WorkflowStep{
				"p1":   {Type: "parallel", Join: "mid", Branches: []WorkflowBranch{{ID: "a", Role: "r", Prompt: "p"}}},
				"mid":  {Type: "agent", Role: "r", Prompt: "x", OnSuccess: "p2"},
				"p2":   {Type: "parallel", Join: "tail", Branches: []WorkflowBranch{{ID: "b", Role: "r", Prompt: "p"}}},
				"tail": {Type: "agent", Role: "r", Prompt: "x", OnSuccess: "complete"},
			},
			Terminals: map[string]WorkflowTerminal{"complete": {Status: "COMPLETED"}},
		}
		if err := w.Validate("twopar.md"); err == nil || !strings.Contains(err.Error(), "must be the workflow entrypoint") {
			t.Fatalf("expected two-parallel rejection, got %v", err)
		}
	})
}

// TestValidate_BranchWorkflowRefs covers the cross-workflow branch.workflow
// resolution done at config-set load.
func TestValidate_BranchWorkflowRefs(t *testing.T) {
	w := validParallelWorkflow()
	s := w.Steps["fanout"]
	s.Branches[0].Workflow = "research"
	w.Steps["fanout"] = s

	t.Run("unknown branch workflow rejected", func(t *testing.T) {
		known := map[string]*Workflow{"par-test": w}
		if err := w.validateBranchWorkflowRefs("par-test", known); err == nil ||
			!strings.Contains(err.Error(), "non-existent workflow 'research'") {
			t.Fatalf("expected unknown-workflow error, got %v", err)
		}
	})

	t.Run("known branch workflow accepted", func(t *testing.T) {
		known := map[string]*Workflow{"par-test": w, "research": {ID: "research"}}
		if err := w.validateBranchWorkflowRefs("par-test", known); err != nil {
			t.Fatalf("expected accepted, got %v", err)
		}
	})

	t.Run("empty branch workflow is a no-op", func(t *testing.T) {
		plain := validParallelWorkflow()
		if err := plain.validateBranchWorkflowRefs("par-test", map[string]*Workflow{}); err != nil {
			t.Fatalf("expected no-op, got %v", err)
		}
	})
}

// TestParseJoinPolicy exercises the shared policy parser used by both the
// validator and the executor's wake path.
func TestParseJoinPolicy(t *testing.T) {
	tests := []struct {
		policy   string
		branches int
		wantKind string
		wantN    int
		wantErr  bool
	}{
		{"", 3, "all", 0, false},
		{"all", 3, "all", 0, false},
		{"best_effort", 3, "best_effort", 0, false},
		{"quorum:2", 3, "quorum", 2, false},
		{"quorum:3", 3, "quorum", 3, false},
		{"quorum:0", 3, "", 0, true},
		{"quorum:4", 3, "", 0, true},
		{"quorum:x", 3, "", 0, true},
		{"weird", 3, "", 0, true},
	}
	for _, tt := range tests {
		kind, n, err := ParseJoinPolicy(tt.policy, tt.branches)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseJoinPolicy(%q,%d): expected error", tt.policy, tt.branches)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseJoinPolicy(%q,%d): unexpected error %v", tt.policy, tt.branches, err)
			continue
		}
		if kind != tt.wantKind || n != tt.wantN {
			t.Errorf("ParseJoinPolicy(%q,%d) = (%q,%d), want (%q,%d)", tt.policy, tt.branches, kind, n, tt.wantKind, tt.wantN)
		}
	}
}

// TestValidate_ParallelStageChildOnJoin confirms stage_child_artifacts on the
// join step of a resume_after_children workflow is accepted (the join is the
// legal post-fan-out consumer), and rejected on the parallel step itself.
func TestValidate_ParallelStageChildOnJoin(t *testing.T) {
	build := func() *Workflow {
		w := validParallelWorkflow()
		w.ResumeAfterChildren = true
		return w
	}
	t.Run("flag on join consumer is valid", func(t *testing.T) {
		w := build()
		s := w.Steps["synthesize"]
		s.StageChildArtifacts = true
		w.Steps["synthesize"] = s
		if err := w.Validate("stage-ok.md"); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})
	t.Run("flag on the parallel step itself is rejected", func(t *testing.T) {
		w := build()
		s := w.Steps["fanout"]
		s.StageChildArtifacts = true
		w.Steps["fanout"] = s
		if err := w.Validate("stage-bad.md"); err == nil {
			t.Fatalf("expected rejection of stage_child_artifacts on the fan-out origin")
		}
	})
}
