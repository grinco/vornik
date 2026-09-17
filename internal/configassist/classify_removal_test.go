package configassist

import "testing"

// Regression: audit 2026-09-15 CA-01 — "Changes That Remove Guardrails Can Be
// Class A". Classification read the NEW SCALAR and ignored what the runtime
// does with a deleted key or a sentinel zero. Three distinct shapes, one
// rule: a change is classified by its effect on the running daemon, not by
// the ordering of two strings.
func TestClassify_GuardrailRemovalIsNotTuning(t *testing.T) {
	cases := []struct {
		name string
		ch   Change
		want string
		why  string
	}{
		{
			name: "deleting requireApproval",
			ch:   Change{File: "projects/p.yaml", Key: "autonomy.requireApproval", Before: "true", After: ""},
			want: ClassE,
			// registry decodes the absent key to the zero value false, the
			// same runtime state as an explicit false — which the table
			// already calls E.
			why: "deletion decodes to the false default: approval is gone",
		},
		{
			name: "explicit false still E",
			ch:   Change{File: "projects/p.yaml", Key: "autonomy.requireApproval", Before: "true", After: "false"},
			want: ClassE,
			why:  "the already-covered explicit case must not regress",
		},
		{
			name: "zeroing maxTasksPerHour",
			ch:   Change{File: "projects/p.yaml", Key: "autonomy.maxTasksPerHour", Before: "4", After: "0"},
			want: ClassD,
			// Manager.checkRateLimit: `MaxTasksPerHour <= 0` returns true,
			// "no limit". Numerically smaller, semantically unbounded.
			why: "zero is the no-limit sentinel, so this RAISES throughput",
		},
		{
			name: "negative maxTasksPerHour",
			ch:   Change{File: "projects/p.yaml", Key: "autonomy.maxTasksPerHour", Before: "4", After: "-1"},
			want: ClassD,
			why:  "any value <= 0 hits the same no-limit branch",
		},
		{
			name: "deleting maxTasksPerHour",
			ch:   Change{File: "projects/p.yaml", Key: "autonomy.maxTasksPerHour", Before: "4", After: ""},
			want: ClassD,
			why:  "the absent key decodes to 0, which is no limit",
		},
		{
			name: "deleting maxConcurrentTasks",
			ch:   Change{File: "projects/p.yaml", Key: "maxConcurrentTasks", Before: "2", After: ""},
			want: ClassD,
			why:  "same sentinel family as maxTasksPerHour",
		},
		{
			name: "lowering maxTasksPerHour to a positive value is still tuning",
			ch:   Change{File: "projects/p.yaml", Key: "autonomy.maxTasksPerHour", Before: "9", After: "4"},
			want: ClassA,
			why:  "a genuinely smaller cap must stay class A",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyOne(tc.ch)
			if got.Class != tc.want {
				t.Fatalf("%s: got class %s (%q), want %s — %s", tc.name, got.Class, got.Reason, tc.want, tc.why)
			}
		})
	}
}

// Regression: audit 2026-09-15 CA-01, second half — adding a feed to a
// NON-EMPTY list is a topology change, but only the whole-key transition
// ("" → something) was recognised, so the per-item keys an insertion
// actually produces fell through to A.
func TestClassify_FeedInsertionIsTopology(t *testing.T) {
	for _, key := range []string{
		"autonomy.feeds.newsroom.url",
		"autonomy.feeds.newsroom.slug",
		"autonomy.feeds.2.url",
	} {
		got := classifyOne(Change{File: "projects/p.yaml", Key: key, Before: "", After: "https://example.test/feed"})
		if got.Class != ClassC {
			t.Fatalf("%s: declaring a new feed is topology C, got %s (%q)", key, got.Class, got.Reason)
		}
	}
	// A cadence edit on an EXISTING feed keeps its spend/tuning reading.
	if got := classifyOne(Change{File: "projects/p.yaml", Key: "autonomy.feeds.newsroom.cadence", Before: "2h", After: "1h"}); got.Class != ClassD {
		t.Fatalf("shortening a feed cadence stays D (more ticks), got %s", got.Class)
	}
}

// Regression: audit 2026-09-15 CA-01, third half — `workflowId`/`projectId`
// bind a project to what it RUNS. They sat in the class-A tuning list while
// the sibling `defaultWorkflowId` was correctly C.
func TestClassify_WorkflowAndProjectBindingIsTopology(t *testing.T) {
	for _, key := range []string{"workflowId", "projectId", "defaultWorkflowId"} {
		got := classifyOne(Change{File: "projects/p.yaml", Key: key, Before: "old", After: "new"})
		if got.Class != ClassC {
			t.Fatalf("%s rebinds what the project runs: want C, got %s (%q)", key, got.Class, got.Reason)
		}
	}
}
